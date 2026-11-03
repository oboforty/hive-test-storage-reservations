package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-chi/chi/v5/middleware"
	"github.com/google/uuid"

	"github.com/oboforty/hiveproj/internal/config"
	"github.com/oboforty/hiveproj/internal/database"
	"github.com/oboforty/hiveproj/internal/dedup"
	"github.com/oboforty/hiveproj/internal/logging"
	"github.com/oboforty/hiveproj/internal/store"
)

type testEnv struct {
	t       *testing.T
	api     *API
	handler http.Handler
	store   *store.Store
	dedup   *dedup.Store
	db      *sql.DB
	account store.Account // seeded "hive" account
}

const testMaxParallelChunks = 1000

// setChunkLimit changes the seeded account's max_parallel_chunks.
func (e *testEnv) setChunkLimit(n int) {
	e.t.Helper()
	if _, err := e.db.Exec(`UPDATE accounts SET max_parallel_chunks = ? WHERE id = ?`, n, e.account.ID); err != nil {
		e.t.Fatalf("set chunk limit: %v", err)
	}
	e.account.MaxParallelChunks = n
}

func newTestEnv(t *testing.T, env config.Env) *testEnv {
	t.Helper()
	ctx := context.Background()
	log := logging.Discard()

	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(ctx, db, log); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// High chunk limit so tests that create many reservations aren't capped;
	// chunk-limit tests lower it with setChunkLimit.
	if err := database.Seed(ctx, db, log, testMaxParallelChunks); err != nil {
		t.Fatalf("seed: %v", err)
	}

	st := store.New(db)
	acc, err := st.FindAccount(ctx, database.SeedUsername)
	if err != nil {
		t.Fatalf("find seeded account: %v", err)
	}
	dd := dedup.NewStore(db)
	a := New(st, dd, log, config.Config{Env: env, RequestTimeout: 5 * time.Second, DedupTTL: time.Hour})
	return &testEnv{t: t, api: a, handler: a.Router(), store: st, dedup: dd, db: db, account: acc}
}

// do sends a request. Mutating requests (POST/DELETE) get a freshly
// generated dedup key, like a well-behaved client would send.
func (e *testEnv) do(method, url string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	dedupkey := ""
	if method == http.MethodPost || method == http.MethodDelete {
		dedupkey = uuid.NewString()
	}
	return e.doKey(dedupkey, method, url, body)
}

// doKey sends a request with an explicit dedup key ("" = no header).
func (e *testEnv) doKey(dedupkey, method, url string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case nil:
	case string:
		buf.WriteString(b)
	default:
		if err := json.NewEncoder(&buf).Encode(b); err != nil {
			e.t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, url, &buf)
	req.Header.Set("Content-Type", "application/json")
	if dedupkey != "" {
		req.Header.Set(dedup.HeaderKey, dedupkey)
	}
	rec := httptest.NewRecorder()
	e.handler.ServeHTTP(rec, req)
	return rec
}

func expectStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("expected status %d, got %d; body: %s", want, rec.Code, rec.Body.String())
	}
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode response: %v; body: %s", err, rec.Body.String())
	}
	return v
}

func (e *testEnv) reserve(size int64) store.Reservation {
	e.t.Helper()
	rec := e.do(http.MethodPost, fmt.Sprintf("/accounts/%s/reservations", e.account.ID), map[string]any{"size": size})
	expectStatus(e.t, rec, http.StatusCreated)
	return decode[store.Reservation](e.t, rec)
}

// serveWithErrorMiddleware runs h behind the router's error middleware stack.
func serveWithErrorMiddleware(a *API, h http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/boom?x=1", nil)
	rec := httptest.NewRecorder()
	middleware.RequestID(a.ErrorMiddleware(h)).ServeHTTP(rec, req)
	return rec
}
