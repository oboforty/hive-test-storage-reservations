package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oboforty/hiveproj/internal/config"
	"github.com/oboforty/hiveproj/internal/database"
)

/*
Checks API rate limiter (429)
*/
func TestRateLimit(t *testing.T) {
	e := newTestEnv(t, config.EnvProd)
	e.api.cfg.RateLimit = 3
	h := e.api.Router()

	get := func(remoteAddr, xff string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/accounts", nil)
		req.RemoteAddr = remoteAddr
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for range 3 {
		expectStatus(t, get("203.0.113.1:1000", ""), http.StatusOK)
	}

	rec := get("203.0.113.1:1001", "")
	expectStatus(t, rec, http.StatusTooManyRequests)
	if body := decode[map[string]any](t, rec); body["error"] != "rate limit exceeded" {
		t.Errorf("unexpected body: %s", rec.Body.String())
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Error("expected Retry-After header")
	}

	// Assert: spoofed X-Forwarded-For doesn't grant a fresh bucket
	expectStatus(t, get("203.0.113.1:1002", "198.51.100.7"), http.StatusTooManyRequests)
	expectStatus(t, get("203.0.113.2:1000", ""), http.StatusOK)
}

/*
Prod errors don't leak information
*/
func TestProdClientErrorsKeepMessage(t *testing.T) {
	e := newTestEnv(t, config.EnvProd)
	rec := e.do(http.MethodPost, "/accounts/"+e.account.ID+"/reservations",
		map[string]any{"size": 101 * database.GB})
	expectStatus(t, rec, http.StatusConflict)
	body := decode[map[string]any](t, rec)
	if body["error"] != "insufficient storage" {
		t.Errorf("expected client-safe message, got %v", body["error"])
	}
	if _, ok := body["error_chain"]; ok {
		t.Error("prod must not leak error internals")
	}
}
