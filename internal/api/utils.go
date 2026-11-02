package api

import (
	"net/http"
	"strings"
)

const maxBodyBytes = 1 << 20 // 1 MiB

func httpflag(r *http.Request, name string) bool {
	switch strings.ToLower(r.URL.Query().Get(name)) {
	case "1", "true", "yes":
		return true
	}
	return false
}

func (a *API) ok(w http.ResponseWriter, status int, v any) error {
	writeJSON(w, a.log, status, v)
	return nil
}

func (a *API) health(w http.ResponseWriter, r *http.Request) error {
	if err := a.store.DB().PingContext(r.Context()); err != nil {
		return &HTTPError{Status: http.StatusServiceUnavailable, Message: "database unavailable", Err: err}
	}
	return a.ok(w, http.StatusOK, map[string]string{"status": "ok"})
}
