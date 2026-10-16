package cli

// stepctx.go: `pf` inside a workflow step. A step agent holds no aihub
// credential path: its memory access is mediated by the daemon. The step
// context is recognised by the marker every step process is spawned with
// — PF_WI_ID (with PF_DAEMON_ID), the same marker the daemon's
// step-origin guard and the reaper read; the step sanitizer strips every
// other PF_* variable, so a step's environment never carries PF_API_KEY.
// Inside a step:
//
//   - `pf recall` and `pf memory show` read through the daemon socket
//     (GET /step/memories/...): the daemon searches with its own
//     credential, scoped to the calling step's own work item's project;
//   - `pf remember` writes the memory into the capsule's memory outbox
//     (capsule.DirMemoryOutbox) — no network call — and the daemon
//     validates and uploads it after the step;
//   - every other verb that reaches aihub directly is refused, and the
//     aihub client of the invocation refuses every request (no request
//     leaves with or without cli.toml's api_key).
//
// cli.toml stays readable under the shared HOME: this guards against
// accidents, not against an agent that reads the file on purpose.

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/polyforge/aihubclient"
)

// stepMarkerEnv is the step-process marker (runtime/spawn.go sets it).
const stepMarkerEnv = "PF_WI_ID"

// stepWorkItem answers the work item of the step this pf runs in ("" =
// not inside a step).
func stepWorkItem() string { return strings.TrimSpace(os.Getenv(stepMarkerEnv)) }

// stepAllowed are the aihub-facing verbs a step may run (daemon-mediated).
var stepAllowed = map[string]bool{"recall": true, "remember": true, "memory": true}

// stepRefusal answers the typed refusal of a verb inside a step, nil when
// the verb may run there: every data-plane and management verb other than
// recall / remember / memory reaches aihub directly.
func stepRefusal(cmd *Command) error {
	if stepAllowed[cmd.Name] || (cmd.Group != GroupData && cmd.Group != GroupMgmt) {
		return nil
	}
	return &api.Error{Code: api.ErrDaemonForbidden, Detail: fmt.Sprintf(
		"pf %s refused inside a workflow step (%s is set): step agents hold no aihub access — they use `pf recall`, `pf memory show` and `pf remember` only, which the daemon mediates",
		cmd.Name, stepMarkerEnv)}
}

// errStepNoAihub is what the aihub client of a step invocation answers.
var errStepNoAihub = errors.New("inside a workflow step pf never connects to aihub directly (memory access goes through the daemon)")

type refuseTransport struct{}

func (refuseTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, errStepNoAihub }

// stepAihubClient is the aihub client of a step invocation: every request
// is refused before it leaves the process.
func stepAihubClient() *aihubclient.Client {
	c := aihubclient.New("http://aihub.invalid", "")
	c.HTTP = &http.Client{Transport: refuseTransport{}}
	return c
}

// stepContext is the daemon's GET /step/context answer.
type stepContext struct {
	WorkItemID string `json:"work_item_id"`
	Slug       string `json:"slug"`
	Project    string `json:"project"`
	CapsuleDir string `json:"capsule_dir"`
}

func (ctx *Context) stepContext() (stepContext, error) {
	var sc stepContext
	raw, err := ctx.daemon().do("GET", "/step/context", "", nil, nil)
	if err != nil {
		return sc, fmt.Errorf("the daemon (the step's memory path) did not answer: %w", err)
	}
	if err := json.Unmarshal(raw, &sc); err != nil || sc.CapsuleDir == "" {
		return sc, fmt.Errorf("the daemon's step context is unreadable")
	}
	return sc, nil
}

// stepRecall runs `pf recall` through the daemon.
func stepRecall(ctx *Context, proj, q, memType string, tags []string, limit int, match string) (json.RawMessage, error) {
	v := url.Values{}
	v.Set("q", q)
	if proj != "" {
		v.Set("project", proj)
	}
	if memType != "" {
		v.Set("type", memType)
	}
	if len(tags) > 0 {
		v.Set("tags", strings.Join(tags, ","))
	}
	v.Set("limit", strconv.Itoa(limit))
	if match != "" {
		v.Set("match", match)
	}
	return ctx.daemon().do("GET", "/step/memories/search", v.Encode(), nil, nil)
}

// stepMemory reads one memory (metadata + body) through the daemon.
func stepMemory(ctx *Context, id string) (json.RawMessage, []byte, error) {
	raw, err := ctx.daemon().do("GET", "/step/memories/"+url.PathEscape(id), "", nil, nil)
	if err != nil {
		return nil, nil, err
	}
	var doc struct {
		Meta    json.RawMessage `json:"meta"`
		Content string          `json:"content"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.Meta) == 0 {
		return nil, nil, fmt.Errorf("memory show: the daemon's answer is unreadable")
	}
	return doc.Meta, []byte(doc.Content), nil
}

// stepRemember writes one memory into the capsule's outbox. The daemon
// judges it (type, title, size, credentials, one per step) and uploads
// it after the step; the checks here only fail fast on what it would
// refuse anyway.
func stepRemember(ctx *Context, fs *flag.FlagSet, memType, proj, content string) error {
	title := strings.TrimSpace(stringFlag(fs, "title"))
	switch {
	case !strings.HasPrefix(memType, "experience.") || len(memType) == len("experience."):
		return fmt.Errorf("remember: inside a workflow step --type must be experience.<kind> (got %q)", memType)
	case title == "":
		return fmt.Errorf("remember: inside a workflow step --title is required")
	case len(content) > 2048:
		return fmt.Errorf("remember: the body is %d bytes; a step memory is at most 2048", len(content))
	}
	if v := stringFlag(fs, "visibility"); v != "" && v != "project" {
		return fmt.Errorf("remember: a step memory is project-visible (got --visibility %s)", v)
	}
	sc, err := ctx.stepContext()
	if err != nil {
		return fmt.Errorf("remember: %w", err)
	}
	if proj != sc.Project {
		return fmt.Errorf("remember: this step belongs to project %q (got --project %s)", sc.Project, proj)
	}
	if wi := stringFlag(fs, "work-item"); wi != "" && wi != sc.WorkItemID && wi != sc.Slug {
		return fmt.Errorf("remember: a step memory is attributed to its own work item %s (got --work-item %s)", sc.WorkItemID, wi)
	}
	m := capsule.OutboxMemory{Type: memType, Title: title, Body: content}
	if tags := stringFlag(fs, "tags"); tags != "" {
		for _, t := range strings.Split(tags, ",") {
			if t = strings.TrimSpace(t); t != "" {
				m.Tags = append(m.Tags, t)
			}
		}
	}
	data, err := m.Marshal()
	if err != nil {
		return err
	}
	var rnd [4]byte
	_, _ = rand.Read(rnd[:])
	name := time.Now().UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(rnd[:]) + ".md"
	rel := capsule.DirMemoryOutbox + "/" + name
	dir := filepath.Join(sc.CapsuleDir, filepath.FromSlash(capsule.DirMemoryOutbox))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+name+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		os.Remove(tmp)
		return err
	}
	if ctx.JSON {
		b, _ := json.Marshal(map[string]string{"queued": rel, "type": memType, "title": title})
		ctx.Out(b)
		return nil
	}
	fmt.Fprintf(ctx.Stdout, "queued %s (type %s) — the daemon uploads it after this step; the outcome lands in %s\n", rel, memType, capsule.MemoryLogMD)
	return nil
}
