package api

import (
	"net/http"
	"testing"

	"github.com/oboforty/hiveproj/internal/config"
	"github.com/oboforty/hiveproj/internal/database"
	"github.com/oboforty/hiveproj/internal/store"
)

func TestListAccounts(t *testing.T) {
	// Arrange - seed
	e := newTestEnv(t, config.EnvDebug)

	// Act - list accounts
	rec := e.do(http.MethodGet, "/accounts", nil)
	expectStatus(t, rec, http.StatusOK)

	accounts := decode[[]map[string]any](t, rec)
	if len(accounts) == 0 {
		t.Fatalf("expected at least 1 account from seed, got %d", len(accounts))
	}
	if accounts[0]["username"] != "hive" {
		t.Errorf("expected username hive, got %v", accounts[0]["username"])
	}
	if _, ok := accounts[0]["active_reservations"]; ok {
		t.Error("plain listing must not include full details")
	}

	// Act - detailed list of accounts
	e.reserve(10 * database.GB)
	rec = e.do(http.MethodGet, "/accounts?full=1", nil)
	expectStatus(t, rec, http.StatusOK)

	// Assert - includes summary, but not the reservation list
	if raw := decode[[]map[string]any](t, rec); len(raw) == 0 {
		t.Fatal("expected at least 1 account in full listing")
	} else if _, ok := raw[0]["active_reservations"]; ok {
		t.Error("full listing must not include active_reservations")
	}
	accountSummaries := decode[[]store.AccountSummary](t, rec)
	d := accountSummaries[0]

	if d.ReservedStorage != 10*database.GB {
		t.Errorf("reserved: want %d, got %d", 10*database.GB, d.ReservedStorage)
	}
	if d.FreeStorage != 90*database.GB {
		t.Errorf("free: want %d, got %d", 90*database.GB, d.FreeStorage)
	}
	if d.Usage != 0.1 {
		t.Errorf("used_percent: want 10, got %v", d.Usage)
	}
}
