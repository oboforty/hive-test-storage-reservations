package dedup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5/middleware"
)

const (
	// HeaderKey is the request header carrying the client's dedup key.
	HeaderKey = "X-Dedup-ID"
	// HeaderReplayed is set to "true" on responses served from the dedup table.
	HeaderReplayed = "X-Dedup-Replayed"

	maxKeyLen = 255
)

// Options configures a Middleware.
type Options struct {
	// Model is stored in dedup.model (e.g. "storage_reservation").
	Model string
	// TTL is how long an entry is kept (and replayed). 0 = forever.
	TTL time.Duration
	// Optional makes the key optional; requests without it pass through
	// without deduplication. By default the key is required.
	Optional bool
	// MaxBodyBytes limits the request body read for hashing. Default 1 MiB.
	MaxBodyBytes int64
	// OnError renders errors (ErrMissingKey, ErrInProgress, ErrKeyReused,
	// storage errors). Defaults to a plain-text response.
	OnError func(w http.ResponseWriter, r *http.Request, err error)
	// Logger receives warnings about bookkeeping failures. Optional.
	Logger *slog.Logger
}

// Middleware makes the wrapped handler idempotent per dedup key:
//
//   - no entry for the key: the request runs; a 2xx response is stored and the
//     entry marked completed, any other outcome (non-2xx, panic) releases the
//     key so the client may retry with it;
//   - entry completed: the stored status code and body are replayed;
//   - entry pending (completed_at IS NULL): ErrInProgress;
//   - entry created by a different request (method/URI/body): ErrKeyReused.
func (s *Store) Middleware(opts Options) func(http.Handler) http.Handler {
	if opts.MaxBodyBytes <= 0 {
		opts.MaxBodyBytes = 1 << 20
	}
	if opts.OnError == nil {
		opts.OnError = func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, err.Error(), http.StatusConflict)
		}
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			key := r.Header.Get(HeaderKey)
			if key == "" {
				if opts.Optional {
					next.ServeHTTP(w, r)
					return
				}
				opts.OnError(w, r, ErrMissingKey)
				return
			}
			if len(key) > maxKeyLen {
				opts.OnError(w, r, fmt.Errorf("%w: longer than %d characters", ErrInvalidKey, maxKeyLen))
				return
			}

			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, opts.MaxBodyBytes))
			if err != nil {
				opts.OnError(w, r, fmt.Errorf("read request body: %w", err))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
			hash := requestHash(key, r.Method, r.URL.RequestURI(), body)

			existing, err := s.Claim(r.Context(), key, hash, opts.Model, r.Method, opts.TTL)
			if err != nil {
				opts.OnError(w, r, err)
				return
			}
			if existing != nil {
				switch {
				case existing.RequestHash != hash:
					opts.OnError(w, r, ErrKeyReused)
				case existing.Pending():
					opts.OnError(w, r, ErrInProgress)
				default:
					replay(w, *existing)
				}
				return
			}

			// We own the key: run the handler and record its outcome.
			state := &requestState{key: key}
			ctx := context.WithValue(r.Context(), stateKey{}, state)
			ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
			var captured bytes.Buffer
			ww.Tee(&captured)

			// Bookkeeping must survive client disconnects / request timeouts.
			bgCtx := context.WithoutCancel(r.Context())
			completed := false
			defer func() {
				if completed {
					return
				}
				if err := s.Release(bgCtx, key); err != nil {
					opts.Logger.ErrorContext(bgCtx, "dedup: release failed", "key", key, "err", err)
				}
			}()

			next.ServeHTTP(ww, r.WithContext(ctx))

			status := ww.Status()
			if status < 200 || status > 299 {
				return // deferred release
			}
			if err := s.Complete(bgCtx, key, status, captured.Bytes(), state.modelID); err != nil {
				// The response is already sent; keep the entry pending rather
				// than releasing it, so the operation cannot run twice. It is
				// freed when it expires.
				opts.Logger.ErrorContext(bgCtx, "dedup: complete failed", "key", key, "err", err)
			}
			completed = true
		})
	}
}

func replay(w http.ResponseWriter, e Entry) {
	status := http.StatusOK
	if e.ResponseStatusCode != nil {
		status = *e.ResponseStatusCode
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set(HeaderReplayed, "true")
	w.Header().Set(HeaderKey, e.Key)
	if e.CompletedAt != nil {
		w.Header().Set("X-Dedup-Completed-At", e.CompletedAt.Format(time.RFC3339Nano))
	}
	w.WriteHeader(status)
	if e.ResponseBody != nil {
		_, _ = io.WriteString(w, *e.ResponseBody)
	}
}

// requestHash fingerprints the request. The key is part of the hash, so the
// UNIQUE constraint on dedup.request_hash never collides across keys.
func requestHash(key, method, uri string, body []byte) string {
	h := sha256.New()
	for _, part := range []string{key, method, uri, strconv.Itoa(len(body))} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

type stateKey struct{}

type requestState struct {
	key     string
	modelID string
}

// KeyFromContext returns the dedup key of the current request, if the
// request is being processed under a dedup claim.
func KeyFromContext(ctx context.Context) (string, bool) {
	st, ok := ctx.Value(stateKey{}).(*requestState)
	if !ok {
		return "", false
	}
	return st.key, true
}

// SetModelID records the id of the entity the request created or changed;
// it is stored in dedup.model_id when the request completes.
func SetModelID(ctx context.Context, id string) {
	if st, ok := ctx.Value(stateKey{}).(*requestState); ok {
		st.modelID = id
	}
}
