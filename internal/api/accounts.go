package api

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/oboforty/hiveproj/internal/store"
)

// GET /accounts[?full=1] — full adds storage usage, but not the reservation
// list (use GET /accounts/{id}?full=1 for that).
func (a *API) listAccounts(w http.ResponseWriter, r *http.Request) error {
	accounts, err := a.store.ListAccounts(r.Context())
	if err != nil {
		return err
	}
	if !httpflag(r, "full") {
		return a.ok(w, http.StatusOK, accounts)
	}

	out := make([]store.AccountSummary, 0, len(accounts))
	for _, acc := range accounts {
		d, err := a.store.AccountSummary(r.Context(), acc)
		if err != nil {
			return err
		}
		out = append(out, d)
	}
	return a.ok(w, http.StatusOK, out)
}

// GET /accounts/{accountID}[?full=1]
func (a *API) getAccount(w http.ResponseWriter, r *http.Request) error {
	acc, err := a.store.GetAccount(r.Context(), chi.URLParam(r, "accountID"))
	if err != nil {
		return err
	}
	if !httpflag(r, "full") {
		return a.ok(w, http.StatusOK, acc)
	}
	d, err := a.store.AccountDetails(r.Context(), acc)
	if err != nil {
		return err
	}
	return a.ok(w, http.StatusOK, d)
}
