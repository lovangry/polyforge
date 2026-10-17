package cli

// stepctx.go: `pf` inside a workflow step (the PF_WI_ID + PF_DAEMON_ID
// markers every step process carries). A step holds no aihub credential
// path: recall, memory show and remember go through the daemon's
// /step/memories face; every other aihub verb is refused and the
// invocation carries no key. cli.toml stays readable under the shared
// HOME — a guard against accidents, not malice.

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/silgrid/polyforge/internal/common/api"
	"github.com/silgrid/polyforge/internal/common/stepmem"
)

const (
	stepMarkerEnv   = "PF_WI_ID"
	stepDaemonIDEnv = "PF_DAEMON_ID"
)

// stepWorkItem answers PF_WI_ID when both markers are set ("" = not a
// step). It only selects step mode; the daemon resolves the real work
// item from the process tree.
func stepWorkItem() string {
	wi := strings.TrimSpace(os.Getenv(stepMarkerEnv))
	if wi == "" || strings.TrimSpace(os.Getenv(stepDaemonIDEnv)) == "" {
		return ""
	}
	return wi
}

// stepModeNote names what switched step mode on.
func stepModeNote() string {
	return fmt.Sprintf("step mode: %s=%s and %s=%s are set — unset both if this is not a workflow step",
		stepMarkerEnv, os.Getenv(stepMarkerEnv), stepDaemonIDEnv, os.Getenv(stepDaemonIDEnv))
}

var stepAllowed = map[string]bool{"recall": true, "remember": true, "memory": true}

// stepRefusal refuses, inside a step, every data-plane and management verb
// but recall / remember / memory, and doctor (it probes aihub with the key).
func stepRefusal(cmd *Command) error {
	if stepAllowed[cmd.Name] || (cmd.Group != GroupData && cmd.Group != GroupMgmt && cmd.Name != "doctor") {
		return nil
	}
	return &api.Error{Code: api.ErrDaemonForbidden, Detail: fmt.Sprintf(
		"pf %s refused inside a workflow step: step agents hold no aihub access — they use `pf recall`, `pf memory show` and `pf remember` only, which the daemon mediates (%s)",
		cmd.Name, stepModeNote())}
}

// searchMemories is `pf recall`'s search (via the daemon in a step).
func (ctx *Context) searchMemories(proj, q, memType string, tags []string, limit int, match string) (json.RawMessage, error) {
	if ctx.Step == "" {
		return ctx.aihub().SearchMemories(proj, q, memType, tags, limit, match)
	}
	v := url.Values{"q": {q}, "project": {proj}, "type": {memType}, "tags": {strings.Join(tags, ",")},
		"limit": {strconv.Itoa(limit)}, "match": {match}}
	return ctx.daemon().do("GET", "/step/memories/search", v.Encode(), nil, nil)
}

// readMemory is `pf memory show`'s read (via the daemon in a step).
func (ctx *Context) readMemory(id string) (json.RawMessage, []byte, error) {
	if ctx.Step == "" {
		meta, err := ctx.aihub().GetMemory(id)
		if err != nil {
			return nil, nil, err
		}
		body, err := ctx.aihub().GetMemoryContent(id)
		return meta, body, err
	}
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

// stepRemember posts the step's memory to the daemon, which checks it
// again and stores it for the step's own work item; it fails fast here.
func stepRemember(ctx *Context, fs *flag.FlagSet, memType, proj, content string) error {
	title := strings.TrimSpace(stringFlag(fs, "title"))
	var tags []string
	for _, t := range strings.Split(stringFlag(fs, "tags"), ",") {
		if t = strings.TrimSpace(t); t != "" {
			tags = append(tags, t)
		}
	}
	if why := stepmem.Check(memType, title, tags, content); why != "" {
		return fmt.Errorf("remember: refused — %s", why)
	}
	if v := stringFlag(fs, "visibility"); v != "project" {
		return fmt.Errorf("remember: a step memory is project-visible (got --visibility %s)", v)
	}
	raw, err := ctx.daemon().do("POST", "/step/memories", "", map[string]any{
		"type": memType, "title": title, "tags": tags, "content": content,
		"project": proj, "work_item": stringFlag(fs, "work-item"),
	}, nil)
	if err != nil {
		return fmt.Errorf("remember: not stored (the step goes on): %w", err)
	}
	finishAs(ctx, raw, func(m *api.Memory) {
		fmt.Fprintf(ctx.Stdout, "stored %s (type %s, visibility %s)\n", m.ID, m.Type, m.Visibility)
	})
	return nil
}
