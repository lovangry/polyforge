package daemon

// stepmem.go: the step agent's memory read face. A step agent holds no
// aihub credential path: inside a step (PF_WI_ID set — the same marker
// the step-origin guard recognises) `pf recall` and `pf memory show`
// come here, and the daemon forwards them to aihub with its own machine
// credential. The work item — and with it the project every search is
// scoped to — comes from the caller's process tree (peerStepOrigin: the
// journaled step process this daemon spawned among the UDS peer's
// ancestors), never from the request or the caller's own environment: a
// step reads its own work item's project and nothing else. Every route is a GET, so the
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
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
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

// stepJob is the work item (and dispatch) a step caller belongs to.
type stepJob struct {
	WorkItemID string `json:"work_item_id"`
	Slug       string `json:"slug,omitempty"`
	Project    string `json:"project"`
	CapsuleDir string `json:"capsule_dir"`
	// Step and Attempt are the dispatch the caller runs under (the spawn
	// journal entry of the step process it descends from).
	Step    string `json:"step"`
	Attempt int    `json:"attempt"`
}

// stepOrigin is a resolved step caller.
type stepOrigin struct {
	wi      string
	step    string
	attempt int
}

// stepCaller resolves the request's step process to its live job, or
// writes the refusal and answers false.
func (d *Daemon) stepCaller(w http.ResponseWriter, r *http.Request) (stepJob, bool) {
	origin := d.stepOriginOf
	if origin == nil {
		origin = d.peerStepOrigin
	}
	o, reason := origin(r)
	if reason != "" {
		writeErr(w, api.ErrDaemonForbidden, r.URL.Path+": "+reason)
		return stepJob{}, false
	}
	ji, live := d.rt.Info(d.canonWI(o.wi))
	if !live || ji.Project == "" {
		writeErr(w, api.ErrDaemonNotFound, "work item "+o.wi+" of the calling step is not running on this daemon")
		return stepJob{}, false
	}
	return stepJob{WorkItemID: ji.WorkItem, Slug: ji.Slug, Project: ji.Project, CapsuleDir: ji.CapsuleDir,
		Step: o.step, Attempt: o.attempt}, true
}

// peerStepOrigin is the production origin. The caller's own environment
// is never the authority (any process may set PF_WI_ID): the kernel's
// peer credential is walked up its ancestry (and process groups) to a
// step process THIS daemon spawned — the pid of a live job's
// spawn-journal entry (any of its entries; the newest match names the
// step and attempt) that still carries that job's markers. Exactly one
// job may match — a tree reaching several jobs' steps (a process that
// joined another step's process group) is refused — and the caller's
// nearest PF_WI_ID must name it.
func (d *Daemon) peerStepOrigin(r *http.Request) (stepOrigin, string) {
	const notStep = "serves step agents only (the caller is no step process of this daemon) — outside a step, `pf recall` / `pf memory show` read aihub directly"
	cred, ok := requestPeerCred(r.Context())
	if !ok {
		return stepOrigin{}, notStep
	}
	claimed, ok := runtime.StepProcessOrigin(cred.pid, d.cfg.MachineID)
	if !ok {
		return stepOrigin{}, notStep
	}
	chain := ancestry(cred.pid)
	var matches []stepOrigin
	for _, ji := range d.rt.List() {
		if ji.CapsuleDir == "" {
			continue
		}
		entries, err := capsule.ReadSpawnJournal(ji.CapsuleDir)
		if err != nil {
			continue
		}
		// the newest journaled dispatch of this job among the caller's
		// ancestors whose process still carries the job's markers
		for i := len(entries) - 1; i >= 0; i-- {
			e := entries[i]
			if e.PID <= 1 || !chain[e.PID] {
				continue
			}
			if owner, ok := runtime.StepProcessOrigin(e.PID, d.cfg.MachineID); !ok || owner != ji.WorkItem {
				continue
			}
			matches = append(matches, stepOrigin{wi: ji.WorkItem, step: e.StepID, attempt: e.SpawnN})
			break
		}
	}
	switch {
	case len(matches) == 0:
		return stepOrigin{}, notStep + " (no live step process of this daemon is among its ancestors)"
	case len(matches) > 1:
		var wis []string
		for _, m := range matches {
			wis = append(wis, m.wi)
		}
		sort.Strings(wis)
		return stepOrigin{}, "refused: the caller's process tree reaches the step processes of several work items (" + strings.Join(wis, ", ") + ")"
	case claimed != matches[0].wi:
		return stepOrigin{}, fmt.Sprintf("refused: the caller descends from the step process of %s but claims PF_WI_ID=%s", matches[0].wi, claimed)
	}
	return matches[0], ""
}

// ancestry answers the pids of pid, its ancestors and their process
// group leaders (procfs; bounded).
func ancestry(pid int) map[int]bool {
	out := map[int]bool{}
	for cur, i := pid, 0; cur > 1 && i < 64; i++ {
		out[cur] = true
		ppid, pgrp, ok := procParent(cur)
		if !ok {
			break
		}
		if pgrp > 1 {
			out[pgrp] = true
		}
		cur = ppid
	}
	return out
}

// procParent reads the parent pid and process group of pid.
func procParent(pid int) (ppid, pgrp int, ok bool) {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, 0, false
	}
	s := string(raw)
	i := strings.LastIndex(s, ")")
	if i < 0 || i+2 > len(s) {
		return 0, 0, false
	}
	f := strings.Fields(s[i+2:])
	if len(f) < 3 {
		return 0, 0, false
	}
	pp, err1 := strconv.Atoi(f[1])
	pg, err2 := strconv.Atoi(f[2])
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return pp, pg, true
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
	notFound := func() { writeErr(w, api.ErrMemNotFound, "memory "+id+" not found in project "+j.Project) }
	var meta json.RawMessage
	if err := d.hub.Do("GET", "/api/v1/memories/"+url.PathEscape(id), nil, &meta); err != nil {
		if !transient(err) {
			notFound() // whatever aihub says about an id: the step sees not-found
			return
		}
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
		notFound()
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
