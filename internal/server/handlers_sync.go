package server

import (
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/silgrid/polyforge/internal/core/capsule"
)

// C group (rows 14–20): the publish/recovery face. Rows 14–17 and 19 are
// Daemon-only callers (fenced by the generation token); rows 18 and 20 are
// admin-triggered deletion protection.

func (s *Server) hPutBlock(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	wiID := r.PathValue("id")
	if projectID, aerr := s.wiProject(idn, wiID); aerr != nil {
		writeErr(w, aerr)
		return
	} else if serr := s.requireScope(idn, projectID); serr != nil {
		writeErr(w, serr)
		return
	}
	shaHex := r.PathValue("sha")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<20))
	if err != nil {
		writeErr(w, ownerAPI("CORE-413", "block too large"))
		return
	}
	if err := s.Caps.PutBlock(wiID, shaHex, body, r.Header.Get("X-PF-Generation-Token")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) hPostManifest(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	wiID := r.PathValue("id")
	if projectID, aerr := s.wiProject(idn, wiID); aerr != nil {
		writeErr(w, aerr)
		return
	} else if serr := s.requireScope(idn, projectID); serr != nil {
		writeErr(w, serr)
		return
	}
	pathSeqV, err := pathSeq(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	var body struct {
		Manifest    capsule.Manifest `json:"manifest"`
		ManifestSha string           `json:"manifest_sha"`
	}
	if err := decodeBody(r, &body); err != nil {
		writeErr(w, err)
		return
	}
	if body.Manifest.Seq != pathSeqV {
		writeErr(w, ownerAPI("SYNC-400", "path seq does not match manifest seq"))
		return
	}
	head, err := s.Caps.PostManifest(wiID, body.Manifest, body.ManifestSha, r.Header.Get("X-PF-Generation-Token"))
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, map[string]any{"head_seq": head}) })
}

func (s *Server) hHead(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	wiID := r.PathValue("id")
	if projectID, aerr := s.wiProject(idn, wiID); aerr != nil {
		writeErr(w, aerr)
		return
	} else if serr := s.requireScope(idn, projectID); serr != nil {
		writeErr(w, serr)
		return
	}
	h, err := s.Caps.Head(wiID)
	if err != nil {
		// 404 for the pre-bootstrap no-row case (row 16).
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) hGetBlock(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	wiID := r.PathValue("id")
	if projectID, aerr := s.wiProject(idn, wiID); aerr != nil {
		writeErr(w, aerr)
		return
	} else if serr := s.requireScope(idn, projectID); serr != nil {
		writeErr(w, serr)
		return
	}
	b, err := s.Caps.GetBlock(wiID, r.PathValue("sha"))
	writeResult(w, err, func() {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(b)
	})
}

func (s *Server) hRevisionsSince(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	wiID := r.PathValue("id")
	if projectID, aerr := s.wiProject(idn, wiID); aerr != nil {
		writeErr(w, aerr)
		return
	} else if serr := s.requireScope(idn, projectID); serr != nil {
		writeErr(w, serr)
		return
	}
	since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil {
		writeErr(w, ownerAPI("SYNC-400", "bad since"))
		return
	}
	// Row 19 amendment (polyforge#14): include=ops serves each revision's
	// manifest audit content inline (same include style as row 3; §5.1c).
	// Legal value here is ONLY "ops"; anything else is CORE-400.
	includeOps := false
	for _, inc := range strings.Split(r.URL.Query().Get("include"), ",") {
		switch strings.TrimSpace(inc) {
		case "ops":
			includeOps = true
		case "":
		default:
			writeErr(w, ownerAPI("CORE-400", "unknown include value "+inc))
			return
		}
	}
	metas, err := s.Caps.RevisionsSince(wiID, since, includeOps)
	writeResult(w, err, func() { writeJSON(w, http.StatusOK, map[string]any{"revisions": metas}) })
}

func (s *Server) hTombstone(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if idn.Role != "admin" {
		writeErr(w, ownerAPI("CORE-403", "forbidden: admin only"))
		return
	}
	wiID := r.PathValue("id")
	if projectID, aerr := s.wiProject(idn, wiID); aerr != nil {
		writeErr(w, aerr)
		return
	} else if serr := s.requireScope(idn, projectID); serr != nil {
		writeErr(w, serr)
		return
	}
	if err := s.Caps.Tombstone(wiID, r.URL.Query().Get("confirm")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) hRevertTombstone(w http.ResponseWriter, r *http.Request) {
	idn, err := s.authenticate(r)
	if err != nil {
		writeErr(w, err)
		return
	}
	if idn.Role != "admin" {
		writeErr(w, ownerAPI("CORE-403", "forbidden: admin only"))
		return
	}
	wiID := r.PathValue("id")
	if projectID, aerr := s.wiProject(idn, wiID); aerr != nil {
		writeErr(w, aerr)
		return
	} else if serr := s.requireScope(idn, projectID); serr != nil {
		writeErr(w, serr)
		return
	}
	if err := s.Caps.RevertTombstone(wiID); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
