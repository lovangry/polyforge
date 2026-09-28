package server

import (
	"net/http"

	"github.com/silgrid/polyforge/internal/core/owner"
)

// authenticatedWI executes the WI-scoped request preamble shared by every
// {id}-routed handler: authenticate the caller, read the path {id} work
// item id, resolve the WI's project (CORE-404 when absent, hidden objects
// stay hidden), and enforce the api-key project scope. On any failure it
// writes the typed error envelope once and reports ok=false; the caller must
// return immediately. Handlers whose auth order differs (admin-before-load:
// force-takeover, tombstone) keep their own preamble.
func (s *Server) authenticatedWI(w http.ResponseWriter, r *http.Request) (idn *identity, wiID, projectID string, ok bool) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return nil, "", "", false
	}
	wiID = r.PathValue("id")
	var aerr *owner.APIError
	projectID, aerr = s.wiProject(idn, wiID)
	if aerr != nil {
		writeErr(w, aerr)
		return nil, "", "", false
	}
	if err := s.requireScope(idn, projectID); err != nil {
		writeErr(w, err)
		return nil, "", "", false
	}
	return idn, wiID, projectID, true
}
