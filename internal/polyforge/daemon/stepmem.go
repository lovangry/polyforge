package daemon

// stepmem.go: the step agent's memory read face. A step agent holds no
// aihub credential path: inside a step (PF_WI_ID set — the same marker
// the step-origin guard recognises) `pf recall` and `pf memory show`
// come here, and the daemon forwards them to aihub with its own machine
// credential. The work item — and with it the project every search is
// scoped to — comes from the caller's UDS peer credential
// (runtime.StepProcessOrigin), never from the request: a step reads its
// own work item's project and nothing else. Every route is a GET, so the
// step-origin guard lets it through as a read; a caller that is no step
// process of this daemon is refused (humans read aihub directly).
//
//	GET /step/context              the caller's work item, project, capsule dir
//	GET /step/memories/search      ?q&type&tags&limit&match[&project]
//	GET /step/memories/{id}        {meta, content}
//
// `pf remember` inside a step needs no route: it writes the capsule's
// memory outbox, uploaded by the daemon after the step (memoutbox.go).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/polyforge/runtime"
)

// stepSearchExcerptRunes is the excerpt the step search asks for (the
// CLI's own recall excerpt).
const stepSearchExcerptRunes = 120

// memContentReader is the aihub per-id body read (the bare markdown
// answer, which the generic JSON Do cannot carry). *aihubclient.Client
// satisfies it.
type memContentReader interface {
	GetMemoryContent(id string) ([]byte, error)
}

// stepJob is the work item a step caller belongs to.
type stepJob struct {
	WorkItemID string `json:"work_item_id"`
	Slug       string `json:"slug,omitempty"`
	Project    string `json:"project"`
	CapsuleDir string `json:"capsule_dir"`
}

// stepCaller resolves the request's step process to its live job, or
// writes the refusal and answers false.
func (d *Daemon) stepCaller(w http.ResponseWriter, r *http.Request) (stepJob, bool) {
	origin := d.stepOriginOf
	if origin == nil {
		origin = d.peerStepOrigin
	}
	wi, ok := origin(r)
	if !ok {
		writeErr(w, api.ErrDaemonForbidden, r.URL.Path+" serves step agents only (the caller is no step process of this daemon) — outside a step, `pf recall` / `pf memory show` read aihub directly")
		return stepJob{}, false
	}
	ji, live := d.rt.Info(d.canonWI(wi))
	if !live || ji.Project == "" {
		writeErr(w, api.ErrDaemonNotFound, "work item "+wi+" of the calling step is not running on this daemon")
		return stepJob{}, false
	}
	return stepJob{WorkItemID: ji.WorkItem, Slug: ji.Slug, Project: ji.Project, CapsuleDir: ji.CapsuleDir}, true
}

// peerStepOrigin is the production origin: the kernel's peer credential
// walked up to a step process of this daemon.
func (d *Daemon) peerStepOrigin(r *http.Request) (string, bool) {
	cred, ok := requestPeerCred(r.Context())
	if !ok {
		return "", false
	}
	return runtime.StepProcessOrigin(cred.pid, d.cfg.MachineID)
}

func (d *Daemon) stepContextRoute(w http.ResponseWriter, r *http.Request) {
	if j, ok := d.stepCaller(w, r); ok {
		writeJSON(w, http.StatusOK, j)
	}
}

// stepSearchRoute forwards one memory search, scoped to the caller's
// project. A `project` the caller names must be that project.
func (d *Daemon) stepSearchRoute(w http.ResponseWriter, r *http.Request) {
	j, ok := d.stepCaller(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	if p := q.Get("project"); p != "" && p != j.Project {
		writeErr(w, api.ErrDaemonForbidden, fmt.Sprintf("a step of %s reads project %q only (asked for %q)", j.WorkItemID, j.Project, p))
		return
	}
	if strings.TrimSpace(q.Get("q")) == "" {
		writeErr(w, api.ErrDaemonBadInput, "a query (q) is required")
		return
	}
	v := url.Values{}
	v.Set("project", j.Project)
	v.Set("q", q.Get("q"))
	v.Set("excerpt", strconv.Itoa(stepSearchExcerptRunes))
	for _, k := range []string{"type", "tags", "limit", "match"} {
		if s := q.Get(k); s != "" {
			v.Set(k, s)
		}
	}
	var raw json.RawMessage
	if err := d.hub.Do("GET", "/api/v1/memories/search?"+v.Encode(), nil, &raw); err != nil {
		writeErrOf(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// stepMemoryRoute reads one memory (metadata + body) when it belongs to
// the caller's project; any other memory answers the same not-found.
func (d *Daemon) stepMemoryRoute(w http.ResponseWriter, r *http.Request) {
	j, ok := d.stepCaller(w, r)
	if !ok {
		return
	}
	id := r.PathValue("id")
	var meta json.RawMessage
	if err := d.hub.Do("GET", "/api/v1/memories/"+url.PathEscape(id), nil, &meta); err != nil {
		writeErrOf(w, err)
		return
	}
	var entry struct {
		ProjectID string `json:"project_id"`
	}
	_ = json.Unmarshal(meta, &entry)
	pid, err := d.projectID(j.Project)
	if err != nil {
		writeErrOf(w, err)
		return
	}
	if entry.ProjectID == "" || entry.ProjectID != pid {
		writeErr(w, api.ErrMemNotFound, "memory "+id+" not found in project "+j.Project)
		return
	}
	cr, ok := d.hub.(memContentReader)
	if !ok {
		writeErr(w, api.ErrDaemonInternal, "the daemon's aihub client cannot read memory bodies")
		return
	}
	body, err := cr.GetMemoryContent(id)
	if err != nil {
		writeErrOf(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"meta": meta, "content": string(body)})
}

// projectID answers the aihub id of a project name.
func (d *Daemon) projectID(name string) (string, error) {
	if v, ok := d.projectIDs.Load(name); ok {
		return v.(string), nil
	}
	var p struct {
		ID string `json:"id"`
	}
	if err := d.hub.Do("GET", "/api/v1/projects/"+url.PathEscape(name), nil, &p); err != nil {
		return "", err
	}
	if p.ID == "" {
		return "", &api.Error{Code: api.ErrDaemonInternal, Detail: "aihub answered project " + name + " without an id"}
	}
	d.projectIDs.Store(name, p.ID)
	return p.ID, nil
}
