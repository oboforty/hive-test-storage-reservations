package dedup_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oboforty/hiveproj/internal/database"
	"github.com/oboforty/hiveproj/internal/dedup"
	"github.com/oboforty/hiveproj/internal/logging"
)

func newStore(t *testing.T) *dedup.Store {
	t.Helper()
	ctx := context.Background()
	db, err := database.Open(ctx, filepath.Join(t.TempDir(), "dedup.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := database.Migrate(ctx, db, logging.Discard()); err != nil {
		t.Fatal(err)
	}
	return dedup.NewStore(db)
}

// errorRecorder renders middleware errors with a status per error kind.
func errorRecorder(w http.ResponseWriter, _ *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, dedup.ErrMissingKey), errors.Is(err, dedup.ErrInvalidKey):
		status = http.StatusBadRequest
	case errors.Is(err, dedup.ErrInProgress):
		status = http.StatusConflict
	case errors.Is(err, dedup.ErrKeyReused):
		status = http.StatusUnprocessableEntity
	}
	http.Error(w, err.Error(), status)
}

// counting is a generic "create something" handler unrelated to reservations.
type counting struct {
	calls  atomic.Int32
	status int
	block  chan struct{} // if set, the handler waits on it
	inside chan struct{} // if set, closed once the handler started
}

func (h *counting) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	n := h.calls.Add(1)
	if h.inside != nil {
		close(h.inside)
	}
	if h.block != nil {
		<-h.block
	}
	dedup.SetModelID(r.Context(), fmt.Sprintf("widget-%d", n))
	status := h.status
	if status == 0 {
		status = http.StatusCreated
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(fmt.Appendf(nil, `{"call":%d}`, n))
}

func send(h http.Handler, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/widgets", strings.NewReader(body))
	if key != "" {
		req.Header.Set(dedup.HeaderKey, key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestPendingEntryIsRejected(t *testing.T) {
	st := newStore(t)
	inner := &counting{block: make(chan struct{}), inside: make(chan struct{})}
	h := st.Middleware(dedup.Options{TTL: time.Hour, OnError: errorRecorder})(inner)

	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- send(h, "k1", `{}`) }()
	// req is pending:
	<-inner.inside

	if rec := send(h, "k1", `{}`); rec.Code != http.StatusConflict {
		t.Errorf("concurrent duplicate: status %d, want 409", rec.Code)
	}
	close(inner.block)
	if rec := <-done; rec.Code != http.StatusCreated {
		t.Errorf("original request: status %d, want 201", rec.Code)
	}

	// Assert - duplicated request was rejected while pending
	if inner.calls.Load() != 1 {
		t.Errorf("handler ran %d times, not once", inner.calls.Load())
	}
}

func TestPanicReleasesKey(t *testing.T) {
	st := newStore(t)
	h := st.Middleware(dedup.Options{OnError: errorRecorder})(
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))

	func() {
		defer func() { _ = recover() }()
		send(h, "k1", `{}`)
	}()
	if _, err := st.Get(context.Background(), "k1"); err == nil {
		t.Fatal("panicking request must release the key")
	}
}

/*
Not replaying the same dedup_key is OK, if it expired since
*/
func TestExpiredEntryRunsAgain(t *testing.T) {
	st := newStore(t)
	inner := &counting{}
	h := st.Middleware(dedup.Options{TTL: time.Millisecond, OnError: errorRecorder})(inner)

	send(h, "k1", `{}`)
	time.Sleep(5 * time.Millisecond)
	if rec := send(h, "k1", `{}`); rec.Header().Get(dedup.HeaderReplayed) != "" {
		t.Error("expired entry must not be replayed")
	}
	if inner.calls.Load() != 2 {
		t.Errorf("handler ran %d times, want 2", inner.calls.Load())
	}
}
