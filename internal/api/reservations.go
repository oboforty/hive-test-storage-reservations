package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/oboforty/hiveproj/internal/dedup"
)

// GET /reservations[?include_deleted=1]
func (a *API) listReservations(w http.ResponseWriter, r *http.Request) error {
	res, err := a.store.ListReservations(r.Context(), httpflag(r, "include_deleted"))
	if err != nil {
		return err
	}
	return a.ok(w, http.StatusOK, res)
}

// GET /accounts/{accountID}/reservations[?include_deleted=1]
func (a *API) listAccountReservations(w http.ResponseWriter, r *http.Request) error {
	acc, err := a.store.GetAccount(r.Context(), chi.URLParam(r, "accountID"))
	if err != nil {
		return err
	}
	res, err := a.store.ListAccountReservations(r.Context(), acc.ID, httpflag(r, "include_deleted"))
	if err != nil {
		return err
	}
	return a.ok(w, http.StatusOK, res)
}

// modelReservation is the dedup.model value of reservation requests.
const modelReservation = "storage_reservation"

type createReservationRequest struct {
	Size     *int64  `json:"size"`
	DebugLog *string `json:"debug_log,omitempty"`
}

// POST /accounts/{accountID}/reservations
func (a *API) createReservation(w http.ResponseWriter, r *http.Request) error {
	var req createReservationRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			return badRequest("request body is required", err)
		}
		return badRequest("invalid JSON body", err)
	}
	if req.Size == nil {
		return badRequest(`field "size" is required`, nil)
	}
	if *req.Size <= 0 {
		return badRequest(`field "size" must be a positive number of bytes`, nil)
	}

	var dedupID *string
	if key, ok := dedup.KeyFromContext(r.Context()); ok {
		dedupID = &key
	}
	res, err := a.store.CreateReservation(r.Context(), chi.URLParam(r, "accountID"), *req.Size, req.DebugLog, dedupID)
	if err != nil {
		return err
	}
	dedup.SetModelID(r.Context(), res.ID)
	a.log.InfoContext(r.Context(), "reservation created",
		"reservation_id", res.ID, "account_id", res.AccountID, "size", res.Size)

	w.Header().Set("Location", fmt.Sprintf("/accounts/%s/reservations/%s", res.AccountID, res.ID))
	return a.ok(w, http.StatusCreated, res)
}

// GET /accounts/{accountID}/reservations/{reservationID}
func (a *API) getReservation(w http.ResponseWriter, r *http.Request) error {
	accountID := chi.URLParam(r, "accountID")
	if _, err := a.store.GetAccount(r.Context(), accountID); err != nil {
		return err
	}
	res, err := a.store.GetReservation(r.Context(), accountID, chi.URLParam(r, "reservationID"))
	if err != nil {
		return err
	}
	return a.ok(w, http.StatusOK, res)
}

// DELETE /accounts/{accountID}/reservations/{reservationID}
func (a *API) deleteReservation(w http.ResponseWriter, r *http.Request) error {
	accountID := chi.URLParam(r, "accountID")
	if _, err := a.store.GetAccount(r.Context(), accountID); err != nil {
		return err
	}
	res, err := a.store.SoftDeleteReservation(r.Context(), accountID, chi.URLParam(r, "reservationID"))
	if err != nil {
		return err
	}
	dedup.SetModelID(r.Context(), res.ID)
	a.log.InfoContext(r.Context(), "reservation released", "reservation_id", res.ID, "account_id", accountID)
	return a.ok(w, http.StatusOK, res)
}

// DELETE /accounts/{accountID}/reservations — soft-deletes every active reservation.
func (a *API) deleteAccountReservations(w http.ResponseWriter, r *http.Request) error {
	accountID := chi.URLParam(r, "accountID")
	n, err := a.store.SoftDeleteAccountReservations(r.Context(), accountID)
	if err != nil {
		return err
	}
	a.log.InfoContext(r.Context(), "account reservations released", "account_id", accountID, "count", n)
	return a.ok(w, http.StatusOK, map[string]any{"account_id": accountID, "released": n})
}
