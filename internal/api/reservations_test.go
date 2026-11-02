package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/oboforty/hiveproj/internal/config"
	"github.com/oboforty/hiveproj/internal/database"
	"github.com/oboforty/hiveproj/internal/dedup"
	"github.com/oboforty/hiveproj/internal/store"
)

/*
Reserving logical storage
*/
func TestCreateReservation(t *testing.T) {
	e := newTestEnv(t, config.EnvDebug)

	rec := e.do(http.MethodPost, "/accounts/"+e.account.ID+"/reservations",
		map[string]any{"size": 1024, "debug_log": "hello"})
	expectStatus(t, rec, http.StatusCreated)

	// Assert - basic field checks
	r := decode[store.Reservation](t, rec)
	if r.ID == "" {
		t.Fatal("expected generated reservation id")
	}
	if r.AccountID != e.account.ID {
		t.Errorf("unexpected account ID: %s != %s", r.AccountID, e.account.ID)
	}

	if r.State != store.StateCompleted {
		t.Errorf("expected state %q, got %q", store.StateCompleted, r.State)
	}
	if r.CompletedAt == nil || r.UpdatedAt == nil {
		t.Error("completed_at and updated_at must be set after POST")
	}

	// Assert - JSON field validation
	expectStatus(t, e.do(http.MethodPost, "/accounts/"+e.account.ID+"/reservations", `{"size": 1, "foo": 1}`), http.StatusBadRequest)

	// Assert - 404 for incorrect account ID
	expectStatus(t, e.do(http.MethodGet, "/accounts/nope/reservations", nil), http.StatusNotFound)
}

/*
Reserving logical storage until account's total size exceeds its configured limit
*/
func TestCreateReservationExceedsLimit(t *testing.T) {
	e := newTestEnv(t, config.EnvDebug)
	path := "/accounts/" + e.account.ID + "/reservations"

	// Assert - an account must not be able to reserve more than its limit.
	rec := e.do(http.MethodPost, path, map[string]any{"size": 100*database.GB + 1})
	expectStatus(t, rec, http.StatusConflict)
	body := decode[map[string]any](t, rec)
	if body["error"] != "insufficient storage" {
		t.Errorf("unexpected error message: %v", body["error"])
	}

	// Arrange - reserve 90 out of 100 GB
	e.reserve(60 * database.GB)
	e.reserve(30 * database.GB)

	// Assert - an account must not have more active reservations than its limit.
	rec = e.do(http.MethodPost, path, map[string]any{"size": 10*database.GB + 1})
	expectStatus(t, rec, http.StatusConflict)
	details := decode[map[string]any](t, rec)["details"].(map[string]any)
	if details["free_bytes"] != float64(10*database.GB) {
		t.Errorf("expected free_bytes %d, got %v", 10*database.GB, details["free_bytes"])
	}

	// Assert - nothing was reserved at the 3rd try
	list := decode[[]store.Reservation](t, e.do(http.MethodGet, path, nil))
	if len(list) != 2 {
		t.Errorf("expected 2 valid reservations, got %d", len(list))
	}
	for _, r := range list {
		if r.AccountID != e.account.ID {
			t.Errorf("reservation %s belongs to %s", r.ID, r.AccountID)
		}
	}

	// Assert - available storage is less than MAX in the summary endpoint
	rec = e.do(http.MethodDelete, "/accounts/"+e.account.ID+"?full=1", nil)
	accountSummary := decode[store.AccountSummary](t, rec)
	if accountSummary.MaxStorage == accountSummary.FreeStorage || accountSummary.ReservedStorage == 0 {
		t.Errorf("expected some reserved space but got 0. Free storage: %d / %d", accountSummary.FreeStorage, accountSummary.MaxStorage)
	}

	// Assert - Releasing space makes room again.
	expectStatus(t, e.do(http.MethodDelete, path+"/"+list[0].ID, nil), http.StatusOK)
	e.reserve(10*database.GB + 1)
}

func TestDeleteReservation(t *testing.T) {
	e := newTestEnv(t, config.EnvDebug)
	created := e.reserve(4096)
	url := "/accounts/" + e.account.ID + "/reservations/" + created.ID

	// Assert - soft delete reservation
	rec := e.do(http.MethodDelete, url, nil)
	expectStatus(t, rec, http.StatusOK)
	deleted := decode[store.Reservation](t, rec)
	if deleted.DeletedAt == nil {
		t.Fatal("expected deleted_at to be set")
	}
	if deleted.DeletedAt.Location() != time.UTC {
		t.Errorf("deleted_at must be UTC, got %v", deleted.DeletedAt.Location())
	}
	if deleted.State != store.StateReleased {
		t.Errorf("expected state %q, got %q", store.StateReleased, deleted.State)
	}

	// Assert - soft delete retrieval
	rec = e.do(http.MethodGet, url, nil)
	expectStatus(t, rec, http.StatusOK)
	if decode[store.Reservation](t, rec).DeletedAt == nil {
		t.Error("reservation should still exist with deleted_at set")
	}
	d := decode[store.AccountDetails](t, e.do(http.MethodGet, "/accounts/"+e.account.ID+"?full=1", nil))
	if d.ReservedStorage != 0 {
		t.Errorf("expected 0 reserved after delete, got %d", d.ReservedStorage)
	}

	// Assert - cannot delete 2x
	expectStatus(t, e.do(http.MethodDelete, url, nil), http.StatusNotFound)

	// Assert - available storage is back at the max
	rec = e.do(http.MethodDelete, "/accounts/"+e.account.ID+"?full=1", nil)
	accountSummary := decode[store.AccountSummary](t, rec)
	if accountSummary.MaxStorage != accountSummary.FreeStorage || accountSummary.ReservedStorage != 0 {
		t.Errorf("expected 0 reserved space but got %d. Free storage: %d / %d", accountSummary.ReservedStorage, accountSummary.FreeStorage, accountSummary.MaxStorage)
	}
}

/*
Concurrent requests must not oversubscribe the account.
*/
func TestCreateReservationConcurrentLimit(t *testing.T) {
	e := newTestEnv(t, config.EnvDebug)
	path := "/accounts/" + e.account.ID + "/reservations"

	const workers = 20
	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		created int
	)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := e.do(http.MethodPost, path, map[string]any{"size": 10 * database.GB})
			if rec.Code == http.StatusCreated {
				mu.Lock()
				created++
				mu.Unlock()
			} else if rec.Code != http.StatusConflict {
				t.Errorf("unexpected status %d: %s", rec.Code, rec.Body.String())
			}
		}()
	}
	wg.Wait()
	if created != 10 {
		t.Errorf("expected exactly 10 successful reservations, got %d", created)
	}
}

func TestClearAccountReservations(t *testing.T) {
	e := newTestEnv(t, config.EnvDebug)
	e.reserve(1)
	r := e.reserve(2)
	expectStatus(t, e.do(http.MethodDelete, "/accounts/"+e.account.ID+"/reservations/"+r.ID, nil), http.StatusOK)

	n, err := e.store.ClearAccountReservations(context.Background(), e.account.ID)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Errorf("expected 2 deleted rows, got %d", n)
	}
	all := decode[[]store.Reservation](t, e.do(http.MethodGet, "/reservations?include_deleted=1", nil))
	if len(all) != 0 {
		t.Errorf("expected all reservations to be gone, got %d", len(all))
	}
}

/*
Simulate a duplicate request (same X-Dedup-ID)
First request can reserve space, while subsequent ones are denied
*/
func TestCreateReservationSameDedupIDConcurrent(t *testing.T) {
	e := newTestEnv(t, config.EnvDebug)
	path := "/accounts/" + e.account.ID + "/reservations"

	const rounds, parallel = 10, 3
	for round := range rounds {
		key := uuid.NewString()

		var (
			wg    sync.WaitGroup
			start = make(chan struct{})
			recs  = make([]*httptest.ResponseRecorder, parallel)
		)
		for i := range parallel {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				recs[i] = e.doKey(key, http.MethodPost, path, map[string]any{"size": database.GB})
			}()
		}
		close(start)
		wg.Wait()

		// Assert - categorize & count responses & check that only one mutated the state
		created, replayed, inProgress := 0, 0, 0
		var createdID string
		for _, rec := range recs {
			switch {
			case rec.Code == http.StatusCreated && rec.Header().Get(dedup.HeaderReplayed) == "":
				created++
				createdID = decode[store.Reservation](t, rec).ID
			case rec.Code == http.StatusCreated:
				replayed++
			case rec.Code == http.StatusConflict:
				inProgress++
			default:
				t.Fatalf("round %d: unexpected status %d: %s", round, rec.Code, rec.Body.String())
			}
		}
		t.Logf("round %d: created=%d replayed=%d in_progress=%d", round, created, replayed, inProgress)
		if created != 1 {
			t.Fatalf("round %d: expected exactly 1 request to succeed, got %d (replayed=%d, in progress=%d)",
				round, created, replayed, inProgress)
		}

		// Assert -
		for _, rec := range recs {
			if rec.Code == http.StatusCreated {
				if id := decode[store.Reservation](t, rec).ID; id != createdID {
					t.Errorf("round %d: replay returned reservation %s, want %s", round, id, createdID)
				}
			}
		}

		// Assert - reservation is linked to the dedup entry.
		r, err := e.store.GetReservation(context.Background(), e.account.ID, createdID)
		if err != nil {
			t.Fatal(err)
		}
		if r.DedupID == nil || *r.DedupID != key {
			t.Errorf("round %d: reservation dedup_id = %v, want %s", round, r.DedupID, key)
		}
	}

	// Assert - double check: one reservation /round was stored.
	list := decode[[]store.Reservation](t, e.do(http.MethodGet, path, nil))
	if len(list) != rounds {
		t.Errorf("expected %d reservations, got %d", rounds, len(list))
	}
}

func TestCreateReservationReplay(t *testing.T) {
	// Arrange - request
	e := newTestEnv(t, config.EnvDebug)
	path := "/accounts/" + e.account.ID + "/reservations"
	key := uuid.NewString()
	body := map[string]any{"size": 2048}

	// Assert - 1st request is ok
	first := e.doKey(key, http.MethodPost, path, body)
	expectStatus(t, first, http.StatusCreated)
	if first.Header().Get(dedup.HeaderReplayed) != "" {
		t.Error("first response must not be marked as replayed")
	}

	// Assert - dedup key replayed header should be set in response (for dups)
	second := e.doKey(key, http.MethodPost, path, body)
	expectStatus(t, second, http.StatusCreated)
	if second.Header().Get(dedup.HeaderReplayed) != "true" {
		t.Error("second response must be marked as replayed")
	}
	if first.Body.String() != second.Body.String() {
		t.Errorf("replayed body differs:\nfirst:  %s\nsecond: %s", first.Body.String(), second.Body.String())
	}

	// Assert
	entry, err := e.dedup.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}
	r := decode[store.Reservation](t, first)
	if entry.CompletedAt == nil || entry.ModelID == nil || *entry.ModelID != r.ID {
		t.Errorf("dedup entry not completed/linked: %+v", entry)
	}
	if entry.Model == nil || *entry.Model != modelReservation {
		t.Errorf("dedup model = %v, want %s", entry.Model, modelReservation)
	}

	// Assert - one reservation was made
	if n := len(decode[[]store.Reservation](t, e.do(http.MethodGet, path, nil))); n != 1 {
		t.Errorf("expected 1 reservation, got %d", n)
	}
}

/*
Replaying a request may be okay, but changing the request body with a used dedup key is definitely invalid
A failed request leaves no dedup entry, so the client can retry with the same key.
*/
func TestDedupKeyManipulation(t *testing.T) {
	e := newTestEnv(t, config.EnvDebug)
	path := "/accounts/" + e.account.ID + "/reservations"
	dedupkey := uuid.NewString()

	// Assert - valid request
	expectStatus(t, e.doKey(dedupkey, http.MethodPost, path, map[string]any{"size": 1}), http.StatusCreated)

	// Assert - duplicate the request, but with a different body
	// 422 or 409
	expectStatus(t, e.doKey(dedupkey, http.MethodPost, path, map[string]any{"size": 2}), http.StatusUnprocessableEntity)
	expectStatus(t, e.doKey(dedupkey, http.MethodDelete, path, nil), http.StatusUnprocessableEntity)

	// Arrange - reserve most of the available storage
	e.reserve(90 * database.GB)
	dedupkey2 := uuid.NewString()

	// Act - reserve too much
	body := map[string]any{"size": 20 * database.GB}
	expectStatus(t, e.doKey(dedupkey2, http.MethodPost, path, body), http.StatusConflict)
	if _, err := e.dedup.Get(context.Background(), dedupkey2); err == nil {
		t.Fatal("failed request must not leave a dedup entry behind")
	}

	// Assert - a retry with the same dedup key is OK, since the first one didn't take effect and the request is more modest now
	body = map[string]any{"size": 5 * database.GB}
	rec := e.doKey(dedupkey2, http.MethodPost, path, body)
	expectStatus(t, rec, http.StatusCreated)
	if rec.Header().Get(dedup.HeaderReplayed) != "" {
		t.Error("retry after failure must run the request, not replay")
	}
}

/*
max_parallel_chunks: limit on the number of active reservations, regardless of storage limit
*/
func TestCreateReservationExceedsChunkLimit(t *testing.T) {
	e := newTestEnv(t, config.EnvDebug)
	e.setChunkLimit(3)
	path := "/accounts/" + e.account.ID + "/reservations"

	var first store.Reservation
	for i := range 3 {
		r := e.reserve(1)
		if i == 0 {
			first = r
		}
	}

	rec := e.do(http.MethodPost, path, map[string]any{"size": 1})
	expectStatus(t, rec, http.StatusConflict)
	body := decode[map[string]any](t, rec)
	if body["error"] != "too many active reservations" {
		t.Errorf("unexpected error: %v", body["error"])
	}
	details, _ := body["details"].(map[string]any)
	if details["max_parallel_chunks"] != float64(3) || details["active_reservations"] != float64(3) {
		t.Errorf("unexpected details: %v", details)
	}
	if n := len(decode[[]store.Reservation](t, e.do(http.MethodGet, path, nil))); n != 3 {
		t.Errorf("expected 3 active reservations, got %d", n)
	}

	// Assert - released reservations don't count toward the limit.
	expectStatus(t, e.do(http.MethodDelete, path+"/"+first.ID, nil), http.StatusOK)
	e.reserve(1)
	expectStatus(t, e.do(http.MethodPost, path, map[string]any{"size": 1}), http.StatusConflict)
}

func TestCreateReservationChunkLimitConcurrent(t *testing.T) {
	const limit = 3
	e := newTestEnv(t, config.EnvDebug)
	e.setChunkLimit(limit)
	path := "/accounts/" + e.account.ID + "/reservations"

	// fires n concurrent reservation requests and returns the responses
	burst := func(n int) []*httptest.ResponseRecorder {
		recs := make([]*httptest.ResponseRecorder, n)
		var wg sync.WaitGroup
		start := make(chan struct{})
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				recs[i] = e.do(http.MethodPost, path, map[string]any{"size": 1})
			}()
		}
		close(start)
		wg.Wait()
		return recs
	}

	// checks every response is 201 or a well-formed chunk-limit 409
	split := func(recs []*httptest.ResponseRecorder) []store.Reservation {
		t.Helper()
		var created []store.Reservation
		for _, rec := range recs {
			switch rec.Code {
			case http.StatusCreated:
				created = append(created, decode[store.Reservation](t, rec))
			case http.StatusConflict:
				body := decode[map[string]any](t, rec)
				if body["error"] != "too many active reservations" {
					t.Errorf("unexpected error: %v", body["error"])
				}
				details, _ := body["details"].(map[string]any)
				if details["max_parallel_chunks"] != float64(limit) || details["active_reservations"] != float64(limit) {
					t.Errorf("unexpected details: %v", details)
				}
			default:
				t.Errorf("unexpected status %d: %s", rec.Code, rec.Body.String())
			}
		}
		return created
	}

	const workers = 12
	created := split(burst(workers))
	if len(created) != limit {
		t.Fatalf("expected exactly %d reservations, got %d", limit, len(created))
	}
	if n := len(decode[[]store.Reservation](t, e.do(http.MethodGet, path, nil))); n != limit {
		t.Errorf("expected %d active reservations, got %d", limit, n)
	}

	// Assert - released reservations don't count toward the limit:
	// after releasing one, exactly one of the concurrent requests gets the slot.
	expectStatus(t, e.do(http.MethodDelete, path+"/"+created[0].ID, nil), http.StatusOK)
	if got := len(split(burst(workers))); got != 1 {
		t.Errorf("expected exactly 1 reservation after release, got %d", got)
	}
	if n := len(decode[[]store.Reservation](t, e.do(http.MethodGet, path, nil))); n != limit {
		t.Errorf("expected %d active reservations, got %d", limit, n)
	}
}
