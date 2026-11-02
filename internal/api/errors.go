package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"

	"github.com/go-chi/chi/v5/middleware"

	"github.com/oboforty/hiveproj/internal/dedup"
	"github.com/oboforty/hiveproj/internal/store"
)

// HTTPError is an error with an explicit HTTP status and a client-safe message.
type HTTPError struct {
	Status  int
	Message string
	Err     error
	Details map[string]any
}

func (e *HTTPError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("%s: %v", e.Message, e.Err)
	}
	return e.Message
}

func (e *HTTPError) Unwrap() error { return e.Err }

func badRequest(msg string, err error) *HTTPError {
	return &HTTPError{Status: http.StatusBadRequest, Message: msg, Err: err}
}

// errorSlot carries a handler error (or recovered panic) from the handler
// back up to ErrorMiddleware.
type errorSlot struct {
	err   error
	stack []byte
}

type errorSlotKey struct{}

// handlerFunc is an http.HandlerFunc that may return an error. Returned
// errors are rendered centrally by ErrorMiddleware.
type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// handle adapts a handlerFunc to http.HandlerFunc.
func handle(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := h(w, r); err != nil {
			reportError(r, err)
		}
	}
}

// reportError hands an error to ErrorMiddleware (always installed by NewRouter).
func reportError(r *http.Request, err error) {
	if slot, ok := r.Context().Value(errorSlotKey{}).(*errorSlot); ok {
		slot.err = err
	}
}

// ErrorMiddleware is the global error handler. It recovers panics and renders
// any error reported by a handler:
//   - ENV=PROD: 5xx responses get a generic message; 4xx keep their safe message.
//   - otherwise: a JSON document with the full error chain, request info and,
//     for panics, the stack trace.
func (a *API) ErrorMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slot := &errorSlot{}
		r = r.WithContext(context.WithValue(r.Context(), errorSlotKey{}, slot))
		ww, ok := w.(middleware.WrapResponseWriter)
		if !ok {
			ww = middleware.NewWrapResponseWriter(w, r.ProtoMajor)
		}

		func() {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler {
						panic(rec)
					}
					slot.err = fmt.Errorf("panic: %v", rec)
					slot.stack = debug.Stack()
				}
			}()
			next.ServeHTTP(ww, r)
		}()

		if slot.err == nil {
			return
		}
		if ww.Status() != 0 {
			// Headers already sent; nothing we can render, just log.
			a.log.ErrorContext(r.Context(), "error after response was written",
				"err", slot.err, "request_id", middleware.GetReqID(r.Context()))
			return
		}
		a.renderError(ww, r, slot.err, slot.stack)
	})
}

// classify maps an error to its HTTP status, client message and extra details.
func classify(err error) (int, string, map[string]any) {
	var (
		httpErr *HTTPError
		insuff  *store.InsufficientStorageError
		tooMany *store.TooManyReservationsError
	)
	switch {
	case errors.As(err, &httpErr):
		return httpErr.Status, httpErr.Message, httpErr.Details
	case errors.As(err, &insuff):
		return http.StatusConflict, "insufficient storage", map[string]any{
			"account_id":      insuff.AccountID,
			"requested_bytes": insuff.Requested,
			"free_bytes":      insuff.Free,
		}
	case errors.As(err, &tooMany):
		return http.StatusConflict, "too many active reservations", map[string]any{
			"account_id":          tooMany.AccountID,
			"max_parallel_chunks": tooMany.Limit,
			"active_reservations": tooMany.Active,
		}
	case errors.Is(err, dedup.ErrMissingKey):
		return http.StatusBadRequest, fmt.Sprintf("header %s is required", dedup.HeaderKey), nil
	case errors.Is(err, dedup.ErrInvalidKey):
		return http.StatusBadRequest, "invalid dedup key", nil
	case errors.Is(err, dedup.ErrInProgress):
		return http.StatusConflict, "a request with this dedup key is still in progress", nil
	case errors.Is(err, dedup.ErrKeyReused):
		return http.StatusUnprocessableEntity, "dedup key was already used for a different request", nil
	case errors.Is(err, store.ErrNotFound):
		return http.StatusNotFound, "not found", nil
	case errors.Is(err, store.ErrInvalidInput):
		return http.StatusBadRequest, "invalid input", nil
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout, "request timed out", nil
	case errors.Is(err, context.Canceled):
		return 499, "request canceled", nil // client closed request (nginx convention)
	default:
		return http.StatusInternalServerError, http.StatusText(http.StatusInternalServerError), nil
	}
}

func (a *API) renderError(w http.ResponseWriter, r *http.Request, err error, stack []byte) {
	status, msg, details := classify(err)
	ctx := r.Context()
	reqID := middleware.GetReqID(ctx)

	logAttrs := []any{"status", status, "err", err.Error(), "request_id", reqID,
		"method", r.Method, "path", r.URL.Path}
	if status >= 500 {
		if stack != nil {
			logAttrs = append(logAttrs, "stack", string(stack))
		}
		a.log.ErrorContext(ctx, "request failed", logAttrs...)
	} else {
		a.log.DebugContext(ctx, "request rejected", logAttrs...)
	}

	var body map[string]any
	if a.cfg.IsProd() {
		if status >= 500 {
			status = http.StatusInternalServerError
			body = map[string]any{"error": http.StatusText(status)}
		} else {
			body = map[string]any{"error": msg}
			if details != nil {
				body["details"] = details
			}
		}
	} else {
		body = map[string]any{
			"error":       msg,
			"status":      status,
			"detail":      err.Error(),
			"error_type":  fmt.Sprintf("%T", err),
			"error_chain": errorChain(err),
			"request_id":  reqID,
			"method":      r.Method,
			"path":        r.URL.Path,
			"query":       r.URL.RawQuery,
			"timestamp":   time.Now().UTC(),
		}
		if details != nil {
			body["details"] = details
		}
		if stack != nil {
			body["stack"] = string(stack)
		}
	}
	writeJSON(w, a.log, status, body)
}

func errorChain(err error) []string {
	var chain []string
	for err != nil {
		chain = append(chain, err.Error())
		switch x := err.(type) {
		case interface{ Unwrap() []error }:
			for _, e := range x.Unwrap() {
				chain = append(chain, errorChain(e)...)
			}
			return chain
		default:
			err = errors.Unwrap(err)
		}
	}
	return chain
}

func writeJSON(w http.ResponseWriter, log *slog.Logger, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		log.Warn("failed to encode response", "err", err)
	}
}
