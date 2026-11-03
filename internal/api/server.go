// Package api implements the HTTP layer of the storage allocation service.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/oboforty/hiveproj/internal/config"
	"github.com/oboforty/hiveproj/internal/dedup"
	"github.com/oboforty/hiveproj/internal/store"
)

// API holds the dependencies shared by all handlers.
type API struct {
	store *store.Store
	dedup *dedup.Store
	log   *slog.Logger
	cfg   config.Config
}

func New(st *store.Store, dd *dedup.Store, log *slog.Logger, cfg config.Config) *API {
	return &API{store: st, dedup: dd, log: log, cfg: cfg}
}

// idempotent returns a dedup middleware for the given model. Errors are
// rendered by ErrorMiddleware like any handler error.
func (a *API) idempotent(model string) func(http.Handler) http.Handler {
	return a.dedup.Middleware(dedup.Options{
		Model:        model,
		TTL:          a.cfg.DedupTTL,
		MaxBodyBytes: maxBodyBytes,
		OnError:      func(_ http.ResponseWriter, r *http.Request, err error) { reportError(r, err) },
		Logger:       a.log,
	})
}

// Router builds the chi router with all middleware and routes.
func (a *API) Router() http.Handler {
	r := chi.NewRouter()

	r.Use(middleware.RequestID)
	r.Use(a.clientIP())
	r.Use(a.requestLogger)
	r.Use(a.ErrorMiddleware)
	r.Use(a.rateLimit())
	r.Use(requestTimeout(a.cfg.RequestTimeout))

	r.NotFound(handle(func(w http.ResponseWriter, r *http.Request) error {
		return &HTTPError{Status: http.StatusNotFound, Message: "route not found"}
	}))
	r.MethodNotAllowed(handle(func(w http.ResponseWriter, r *http.Request) error {
		return &HTTPError{Status: http.StatusMethodNotAllowed, Message: "method not allowed"}
	}))

	r.Get("/health", handle(a.health))

	r.Get("/accounts", handle(a.listAccounts))
	r.Route("/accounts/{accountID}", func(r chi.Router) {
		r.Get("/", handle(a.getAccount))
		r.Get("/reservations", handle(a.listAccountReservations))
		r.Get("/reservations/{reservationID}", handle(a.getReservation))

		// Mutations are idempotent: they require an X-Dedup-ID header.
		r.Group(func(r chi.Router) {
			r.Use(a.idempotent(modelReservation))
			r.Post("/reservations", handle(a.createReservation))
			r.Delete("/reservations", handle(a.deleteAccountReservations))
			r.Delete("/reservations/{reservationID}", handle(a.deleteReservation))
		})
	})
	r.Get("/reservations", handle(a.listReservations))

	return r
}

func requestTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if d <= 0 {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func (a *API) requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ww := middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		start := time.Now()
		next.ServeHTTP(ww, r)

		status := ww.Status()
		if status == 0 {
			status = http.StatusOK
		}
		level := slog.LevelInfo
		if status >= 500 {
			level = slog.LevelError
		} else if status >= 400 {
			level = slog.LevelWarn
		}
		a.log.Log(r.Context(), level, "http request",
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", status,
			"bytes", ww.BytesWritten(),
			"duration_ms", time.Since(start).Milliseconds(),
			"remote", r.RemoteAddr,
			"client_ip", middleware.GetClientIP(r.Context()),
			"request_id", middleware.GetReqID(r.Context()),
		)
	})
}
