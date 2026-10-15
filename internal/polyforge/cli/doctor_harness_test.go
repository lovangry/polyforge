package cli

// doctor_harness_test.go: polyforge#159 — the "harness claude" doctor
// check (the daemon's view of the Claude Code CLI, only when some
// resolved candidate is claude:…, polyforge#175) and the cli.toml claude
// keys.

import (
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/silgrid/polyforge/internal/polyforge/workspace"
)

func TestClaudeHarnessCheck(t *testing.T) {
	steps := []workspace.StepResolution{{Project: "p", Bundle: "bug-fix", Step: "review",
		Models: []string{"claude:claude-opus-5-5", "pi:sub2api-glm/glm-5.3"}, ModelsFrom: "steps.p.review.models"}}
	ok := HarnessStatus{Bin: "claude", Found: true, Path: "/bin/claude", Version: "2.1.296", Verified: true,
		AuthTokenEnv: "set", APIKeyEnv: "unset", BaseURLEnv: "unset", BackgroundTasks: "disabled"}
	cases := []struct {
		name   string
		steps  []workspace.StepResolution
		up     bool
		hs     map[string]HarnessStatus
		want   string
		needle string
	}{
		{"no claude step", nil, true, nil, "pass", "no resolved candidate is claude:"},
		{"healthy", steps, true, map[string]HarnessStatus{"claude": ok}, "pass", "1 step(s) have claude candidates: p/bug-fix/review: [claude:claude-opus-5-5, pi:sub2api-glm/glm-5.3] (steps.p.review.models)"},
		{"daemon down", steps, false, nil, "skip", "daemon is unreachable"},
		{"older daemon", steps, true, map[string]HarnessStatus{}, "warn", "no harness status"},
		{"binary missing", steps, true, map[string]HarnessStatus{"claude": {Bin: "claude"}}, "fail", "does not resolve"},
		{"unverified version", steps, true, map[string]HarnessStatus{"claude": func() HarnessStatus { h := ok; h.Version, h.Verified = "9.9.9", false; return h }()}, "warn", "9.9.9 is not in the verified set"},
		{"no credential variable", steps, true, map[string]HarnessStatus{"claude": func() HarnessStatus { h := ok; h.AuthTokenEnv = "unset"; return h }()}, "warn", "must live under the claude-config dir"},
		{"base url set", steps, true, map[string]HarnessStatus{"claude": func() HarnessStatus { h := ok; h.BaseURLEnv = "set"; return h }()}, "warn", "ANTHROPIC_BASE_URL is set"},
		{"settings token set", steps, true, map[string]HarnessStatus{"claude": func() HarnessStatus {
			h := ok
			h.AuthTokenEnv = "unset"
			h.SettingsEnv = map[string]string{"ANTHROPIC_AUTH_TOKEN": "set", "ANTHROPIC_BASE_URL": "set", "CLAUDE_CONFIG_DIR": "unset"}
			return h
		}()}, "pass", "claude-config settings.json env: ANTHROPIC_AUTH_TOKEN set"},
		{"dirs reported", steps, true, map[string]HarnessStatus{"claude": func() HarnessStatus {
			h := ok
			h.HarnessDirs = map[string]HarnessDir{
				"step_agent":          {Path: "/r/step-agent", Exists: true},
				"step_agent_sessions": {Path: "/r/step-agent-sessions", Exists: true},
				"claude_config":       {Path: "/r/claude-config", Exists: false},
			}
			return h
		}()}, "pass", "harness dirs: step agent dir present, step agent sessions present, claude config dir missing"},
		{"oat+gateway", steps, true, map[string]HarnessStatus{"claude": func() HarnessStatus {
			h := ok
			h.SettingsWarning = "OAuth token (sk-ant-oat01-...) paired with a gateway BASE_URL — that combination is rejected"
			return h
		}()}, "warn", "sk-ant-oat01"},
		{"config dir override", steps, true, map[string]HarnessStatus{"claude": func() HarnessStatus {
			h := ok
			h.SettingsWarning = "the claude-config settings.json env block sets CLAUDE_CONFIG_DIR: remove it — the adapter hard-sets the step's config dir and the settings value overrides the adapter's"
			return h
		}()}, "warn", "overrides the adapter's"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st, detail := claudeHarnessCheck(c.steps, "/wf", c.up, c.hs)
			if st != c.want || !strings.Contains(detail, c.needle) {
				t.Fatalf("got %s %q, want %s containing %q", st, detail, c.want, c.needle)
			}
			if strings.Contains(detail, "sk-ant-oat01-SEN") {
				t.Errorf("the detail carries a token value")
			}
		})
	}
}

func TestConfigClaudeKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cli.toml")
	if err := os.WriteFile(path, []byte("claude_bin = \"/opt/claude\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ClaudeBin != "/opt/claude" {
		t.Fatalf("cfg %+v", cfg)
	}
}

// polyforge#175 through the doctor: an unprefixed yaml candidate and a
// bundle candidate without a prefix both fail `workspace yaml` naming
// where; `harness claude` runs only when some resolved candidate is
// claude:… and lists the whole chain (each candidate's harness is its
// prefix).
func TestDoctorCandidatePrefixAndClaudeTrigger(t *testing.T) {
	wf := t.TempDir()
	write := func(bundle, fm string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(wf, bundle), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(wf, bundle, "workflow.md"), []byte(fm), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("quick-fix", "---\ndefaults:\n  model_candidates: [pi:g/glm]\nsteps:\n  - { id: fix }\n---\n")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"machine_id":"m-y","workflows_dir":"`+wf+`","jobs":[],"queue":{}}`)
	})
	sock := newFakeDaemon(t, mux)
	srv := aiSever(t, &reqRecorder{}, map[string]bool{"polyforge": true}, 0, "")
	env := doctorServerConfig(t, srv.URL, "k", sock, "polyforge")
	yaml := filepath.Join(filepath.Dir(env["XDG_CONFIG_HOME"]), "ws-polyforge", ".polyforge.yaml")
	put := func(body string) {
		t.Helper()
		if err := os.WriteFile(yaml, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	// every candidate on pi: no claude check
	put("steps:\n  polyforge:\n    fix: { models: [pi:g/glm] }\n")
	_, _, doc := doctorStatus(t, env, "--json", "doctor")
	if st, detail := doctorFind(t, doc, "workspace yaml"); st != "pass" {
		t.Fatalf("all-pi yaml: %s %q", st, detail)
	}
	if st, detail := doctorFind(t, doc, "harness claude"); st != "pass" || !strings.Contains(detail, "no resolved candidate is claude:") {
		t.Fatalf("all-pi claude check: %s %q", st, detail)
	}

	// one claude candidate in a mixed chain: the claude check runs and
	// shows the chain with each candidate's harness
	put("steps:\n  polyforge:\n    fix: { models: [claude:claude-opus-5-5, pi:g/glm] }\n")
	_, _, doc = doctorStatus(t, env, "--json", "doctor")
	if st, detail := doctorFind(t, doc, "harness claude"); st != "warn" || !strings.Contains(detail, "polyforge/quick-fix/fix: [claude:claude-opus-5-5, pi:g/glm] (steps.polyforge.fix.models)") {
		t.Fatalf("mixed chain claude check: %s %q", st, detail)
	}

	// an unprefixed yaml candidate / a retired harness key: fail, named
	put("steps:\n  polyforge:\n    fix: { models: [pi:g/glm, sub2api-glm/glm-5.3] }\n")
	_, _, doc = doctorStatus(t, env, "--json", "doctor")
	if st, detail := doctorFind(t, doc, "workspace yaml"); st != "fail" || !strings.Contains(detail, `steps.polyforge.fix.models[1]: "sub2api-glm/glm-5.3" needs a harness prefix (pi:… or claude:…)`) {
		t.Fatalf("unprefixed yaml: %s %q", st, detail)
	}
	put("steps:\n  polyforge:\n    fix: { models: [claude:claude-opus-5-5], harness: claude }\n")
	_, _, doc = doctorStatus(t, env, "--json", "doctor")
	if st, detail := doctorFind(t, doc, "workspace yaml"); st != "fail" || !strings.Contains(detail, "steps.polyforge.fix.harness: the `harness` key was removed") {
		t.Fatalf("retired harness key: %s %q", st, detail)
	}

	// a bundle with an unprefixed candidate: fail naming file and step
	put("steps:\n  polyforge:\n    fix: { models: [pi:g/glm] }\n")
	write("bug-fix", "---\nsteps:\n  - { id: fix }\n  - { id: review, model_candidates: [claude-opus-5-5] }\n---\n")
	_, _, doc = doctorStatus(t, env, "--json", "doctor")
	if st, detail := doctorFind(t, doc, "workspace yaml"); st != "fail" || !strings.Contains(detail, "bundle bug-fix: "+filepath.Join(wf, "bug-fix", "workflow.md")+`: step review: model_candidates[0]: "claude-opus-5-5" needs a harness prefix`) {
		t.Fatalf("bad bundle: %s %q", st, detail)
	}
}
