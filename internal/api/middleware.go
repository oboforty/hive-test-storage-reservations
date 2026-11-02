package api

import (
	"net/http"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-chi/httprate"
)

// clientIP resolves the client IP into the request context (read it with
// middleware.GetClientIP). Forwarding headers are only trusted when the
// direct peer is one of cfg.TrustedProxies; otherwise the TCP peer address is
// used, so clients cannot spoof their IP (e.g. to dodge the rate limit).
func (a *API) clientIP() func(http.Handler) http.Handler {
	if len(a.cfg.TrustedProxies) == 0 {
		return middleware.ClientIPFromRemoteAddr
	}
	return middleware.ClientIPFromXFF(a.cfg.TrustedProxies...)
}

// rateLimit allows cfg.RateLimit requests per second per client IP
// (IPv6 clients are bucketed by /64). Excess requests get a 429 rendered by
// ErrorMiddleware, plus Retry-After and X-RateLimit-* headers.
// RateLimit <= 0 disables limiting.
func (a *API) rateLimit() func(http.Handler) http.Handler {
	if a.cfg.RateLimit <= 0 {
		return func(next http.Handler) http.Handler { return next }
	}
	return httprate.LimitBy(a.cfg.RateLimit, time.Second,
		func(r *http.Request) (string, error) {
			return httprate.CanonicalizeIP(middleware.GetClientIP(r.Context())), nil
		},
		httprate.WithLimitHandler(func(_ http.ResponseWriter, r *http.Request) {
			reportError(r, &HTTPError{
				Status:  http.StatusTooManyRequests,
				Message: "rate limit exceeded",
				Details: map[string]any{"limit_per_second": a.cfg.RateLimit},
			})
		}),
	)
}
