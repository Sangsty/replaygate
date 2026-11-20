package server

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
)

const (
	defaultRunList = 50
	maxRunList     = 100
)

func (a *api) listRuns(w http.ResponseWriter, r *http.Request) {
	limit := defaultRunList
	if s := r.URL.Query().Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxRunList {
			writeError(w, http.StatusBadRequest, fmt.Sprintf("limit must be between 1 and %d", maxRunList))
			return
		}
		limit = n
	}
	runs, err := a.store.ListRuns(r.Context(), chi.URLParam(r, "pid"), limit)
	if err != nil {
		a.storeError(w, "list runs", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"runs": runs})
}

// getRunRequest returns the baseline and candidate responses of one request,
// which the dashboard shows side by side.
func (a *api) getRunRequest(w http.ResponseWriter, r *http.Request) {
	rid := chi.URLParam(r, "rid")
	reqID, err := strconv.ParseInt(chi.URLParam(r, "reqid"), 10, 64)
	if !uuidRe.MatchString(rid) || err != nil || reqID < 1 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	detail, err := a.store.RunRequestDetail(r.Context(), chi.URLParam(r, "pid"), rid, reqID)
	if err != nil {
		a.storeError(w, "load request detail", err)
		return
	}
	writeJSON(w, http.StatusOK, detail)
}
