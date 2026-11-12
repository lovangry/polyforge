package daemon

// submit_models_preflight_test.go: the submit route's manual --models
// refusal (polyforge#19) — a list whose every candidate fails the
// dispatch-time preflight is refused DAEMON-400 naming each candidate
// before any queue entry or claim state exists, while a mixed list is
// accepted (its unresolvable entries are the dispatch boundary's
// business) and a step-agent dir that proves nothing fails open.

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/common/api"
)

// w19pSubmitModelsJSON declares one known provider (w19p-good); every
// other provider is unresolvable under this step-agent dir.
const w19pSubmitModelsJSON = `{
  "providers": {
    "w19p-good": {
      "baseUrl": "https://w19p.invalid",
      "apiKey": "w19p-fixture",
      "api": "openai-completions",
      "models": [{"id": "w19p-model", "name": "w19p-model"}]
    }
  }
}`

// plantStepAgentModels writes the fixture models.json into the daemon's
// dedicated step-agent dir (the sync at start left it empty).
func plantStepAgentModels(t *testing.T, dataDir string) {
	t.Helper()
	p := filepath.Join(dataDir, "step-agent", "models.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(w19pSubmitModelsJSON), 0o600); err != nil {
		t.Fatal(err)
	}
}

// postSubmitBody answers the submit route's status code and body text.
func postSubmitBody(t *testing.T, sock, path, body string) (int, string) {
	t.Helper()
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := hc.Post("http://pf"+path, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// An all-unresolvable manual --models list is refused loudly, naming
// every candidate, before anything is queued.
func TestSubmitRefusesAllUnresolvableManualModels(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	const wi = "wi_q_preflight_refused"
	var data string
	td := newTestDaemon(t, func(c *Config) {
		c.Hub = hub
		c.DispatchTick = time.Hour
		data = c.DataDir
	})
	plantStepAgentModels(t, data)

	code, body := postSubmitBody(t, td.sock, "/jobs/"+wi+"/claim", `{"models":["pi:w19p-bad/x","pi:w19p-bad/y"]}`)
	if code != http.StatusBadRequest {
		t.Fatalf("an all-unresolvable --models list must be refused DAEMON-400, got %d: %s", code, body)
	}
	for _, m := range []string{"w19p-bad/x", "w19p-bad/y"} {
		if !strings.Contains(body, m) {
			t.Fatalf("the refusal must name the unresolvable candidate %s, got %q", m, body)
		}
	}
	for _, e := range readQueueFile(t, data) {
		if e.WorkItemID == wi {
			t.Fatalf("a refused submit must leave no queue entry behind: %+v", e)
		}
	}
}

// A mixed manual --models list is accepted in full — the unresolvable
// entry is skipped at dispatch, not refused here.
func TestSubmitAcceptsMixedManualModels(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	const wi = "wi_q_preflight_mixed"
	var data, hubBase string
	td := newTestDaemon(t, func(c *Config) {
		c.Hub = hub
		c.DispatchTick = time.Hour
		data, hubBase = c.DataDir, c.GitRemoteBase
	})
	mkBare(t, hubBase, wi)
	hub.status[wi] = api.StatusQueued
	plantStepAgentModels(t, data)

	if code := postSubmit(t, td.sock, "/jobs/"+wi+"/claim", `{"models":["pi:w19p-bad/x","pi:w19p-good/w19p-model"]}`); code != http.StatusOK {
		t.Fatalf("a mixed --models list must be accepted, got %d", code)
	}
	ents := readQueueFile(t, data)
	if len(ents) != 1 || strings.Join(ents[0].Models, ",") != "pi:w19p-bad/x,pi:w19p-good/w19p-model" {
		t.Fatalf("the mixed list must persist in full, got %+v", ents)
	}
}

// A step-agent dir that proves nothing fails open: the same manual list
// is accepted (the skip decision is the spawn boundary's, against the
// state the spawned harness itself will resolve).
func TestSubmitModelsPreflightFailsOpenWithoutModelsJSON(t *testing.T) {
	setGitIdentity(t)
	hub := newFakeHub()
	const wi = "wi_q_preflight_open"
	var hubBase string
	td := newTestDaemon(t, func(c *Config) {
		c.Hub = hub
		c.DispatchTick = time.Hour
		hubBase = c.GitRemoteBase
	})
	mkBare(t, hubBase, wi)
	hub.status[wi] = api.StatusQueued

	if code := postSubmit(t, td.sock, "/jobs/"+wi+"/claim", `{"models":["pi:w19p-bad/x"]}`); code != http.StatusOK {
		t.Fatalf("without a models.json the manual list must be accepted, got %d", code)
	}
}
