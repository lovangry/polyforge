package server

import (
	"io"
	"net/http"
	"strconv"

	"github.com/silgrid/polyforge/internal/core/capsule"
)

// C group (rows 14–20): the publish/recovery face. Rows 14–17 and 19 are
// Daemon-only callers (fenced by the generation token); rows 18 and 20 are
// admin-triggered deletion protection.

func (s *Server) hPutBlock(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticate(r); err != nil {
		writeErr(w, err)
		return
	}
	wiID := r.PathValue("id")
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
	if _, err := s.authenticate(r); err != nil {
		writeErr(w, err)
		return
	}
	wiID := r.PathValue("id")
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
	if _, err := s.authenticate(r); err != nil {
		writeErr(w, err)
		return
	}
	h, err := s.Caps.Head(r.PathValue("id"))
	if err != nil {
		// 404 for the pre-bootstrap no-row case (row 16).
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, h)
}

func (s *Server) hGetBlock(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticate(r); err != nil {
		writeErr(w, err)
		return
	}
	b, err := s.Caps.GetBlock(r.PathValue("id"), r.PathValue("sha"))
	writeResult(w, err, func() {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(b)
	})
}

func (s *Server) hRevisionsSince(w http.ResponseWriter, r *http.Request) {
	if _, err := s.authenticate(r); err != nil {
		writeErr(w, err)
		return
	}
	since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil {
		writeErr(w, ownerAPI("SYNC-400", "bad since"))
		return
	}
	metas, err := s.Caps.RevisionsSince(r.PathValue("id"), since)
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
	if err := s.Caps.Tombstone(r.PathValue("id"), r.URL.Query().Get("confirm")); err != nil {
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
	if err := s.Caps.RevertTombstone(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
