package mainagent

// envsanitize_test.go: the 04 §8 environment pins for spawned WI Agents.
// The gap this file guards: a daemon started with `--api-key` /
// POLYFORGE_API_KEY carries the machine api key in its OWN environment,
// and the old wholesale `append(os.Environ(), ...)` inherit handed it to
// every spawned agent verbatim. The pins:
//
//   (a) END-TO-END: a fake harness that prints its env, spawned by a
//       supervisor constructed while the key sits in the daemon's
//       environment, sees NEITHER any POLYFORGE_* variable NOR the exact
//       key value (the renamed-variable belt-and-braces path included);
//   (b) the four capsule/session additions (PI_SESSION_DIR,
//       PF_SESSION_ID, PF_WORK_ITEM_ID, PF_GENERATION) survive
//       sanitization — they are appended after it, not inherited;
//   (d) the pi and claude adapters route through the SAME sanitizer
//       instance (a shared pin), so no adapter can drift back to a raw
//       os.Environ() inherit.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/silgrid/polyforge/internal/capsule"
	"github.com/silgrid/polyforge/internal/daemon"
)

// envDumpHarness writes a shell harness that dumps its OWN environment to
// $MAINDUMP_FILE (the file lives in the test's temp dir, reachable through
// an inherited non-credential variable) and then idles on stdin like a
// real long-lived harness.
func envDumpHarness(t *testing.T) string {
	t.Helper()
	script := "#!/bin/sh\n" +
		"env > \"$MAINDUMP_FILE\"\n" +
		"while IFS= read -r line; do :; done\n"
	return writeHarness(t, "envdumpharness", script)
}

// TestSpawnedAgentEnvCarriesNoCredentials is the (a)+(b) end-to-end pin:
// the daemon's key is IN its environment (the `pf daemon --api-key` shape
// the sanitizer exists for), a spawned agent's environment must contain
// neither the POLYFORGE_*/PF_* credential namespace nor the exact key
// value under any name, while the capsule/session additions and unrelated
// harness variables survive.
func TestSpawnedAgentEnvCarriesNoCredentials(t *testing.T) {
	const key = "pfk-machine-9f2c7a51d4e8b306"
	dump := filepath.Join(t.TempDir(), "env.txt")
	// The daemon's own environment under `--api-key` (flag default is
	// POLYFORGE_API_KEY): the key sits in env, plus the wider credential
	// namespace and a RENAMED variable carrying the exact key value.
	t.Setenv("POLYFORGE_API_KEY", key)
	t.Setenv("POLYFORGE_AIHUB_URL", "http://127.0.0.1:8420")
	t.Setenv("PF_MACHINE_ID", "m-test")
	t.Setenv("OPS_RENAMED_KEY", key) // belt-and-braces path: same value, foreign name
	t.Setenv("HARNESS_KEEP_ME", "unrelated")
	t.Setenv("MAINDUMP_FILE", dump)
	bin := envDumpHarness(t)
	// The supervisor is constructed with the daemon's configured key
	// (constructor injection): the sanitizer knows what to filter.
	s := NewSupervisor(bin, bin, key)
	p := newSpawnParams(t, capsule.AdapterPi, "sess_env1")
	h, err := s.Spawn(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Terminate(2 * time.Second) })
	waitForFileLines(t, dump, 1)
	raw, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		name, val, _ := strings.Cut(l, "=")
		env[name] = val
		// (a) the credential namespace never reaches the agent.
		if strings.HasPrefix(name, "POLYFORGE_") {
			t.Fatalf("spawned agent env must not carry POLYFORGE_* var %s", name)
		}
		// (a) belt and braces: the exact key value under ANY name.
		if val == key {
			t.Fatalf("spawned agent env carries the machine api key value in %s", name)
		}
		// An inherited PF_* var (not one of the session additions) must
		// be stripped too.
		if name == "PF_MACHINE_ID" {
			t.Fatal("inherited PF_* var must be stripped")
		}
	}
	// (b) the four capsule/session additions survive sanitization.
	if env["PI_SESSION_DIR"] == "" {
		t.Fatal("PI_SESSION_DIR must survive sanitization")
	}
	if env["PF_SESSION_ID"] != "sess_env1" {
		t.Fatalf("PF_SESSION_ID must survive sanitization, got %q", env["PF_SESSION_ID"])
	}
	if env["PF_WORK_ITEM_ID"] != "wi_ag01" {
		t.Fatalf("PF_WORK_ITEM_ID must survive sanitization, got %q", env["PF_WORK_ITEM_ID"])
	}
	if env["PF_GENERATION"] != "3" {
		t.Fatalf("PF_GENERATION must survive sanitization, got %q", env["PF_GENERATION"])
	}
	// Unrelated inherited variables keep flowing (blacklist, not a
	// whitelist): the harness needs PATH/HOME and its own config vars.
	if env["PATH"] == "" {
		t.Fatal("PATH must survive sanitization")
	}
	if env["HARNESS_KEEP_ME"] != "unrelated" {
		t.Fatal("an unrelated variable must survive sanitization")
	}
	if strings.Contains(string(raw), key) {
		t.Fatal("the machine api key value must appear nowhere in the spawned agent's environment")
	}
}

// TestAdaptersShareSanitizer is the (d) pin: BOTH v1 adapters route their
// cmd.Env through the SAME envSanitizer instance (the one built from the
// constructor-injected key), so neither can drift back to a wholesale
// os.Environ() inherit. Verified at BuildCmd level: with the key and its
// renamed twin in the daemon's environment, each adapter's cmd.Env is
// clean of them and carries only its own capsule/session additions on top
// of the sanitized remainder.
func TestAdaptersShareSanitizer(t *testing.T) {
	const key = "pfk-machine-1a2b3c4d5e6f"
	t.Setenv("POLYFORGE_API_KEY", key)
	t.Setenv("POLYFORGE_AIHUB_URL", "http://127.0.0.1:8420")
	t.Setenv("OPS_RENAMED_KEY", key) // same value, foreign name
	t.Setenv("HARNESS_KEEP_ME", "unrelated")
	s := NewSupervisor("pi-unused", "claude-unused", key)
	pi := s.AdapterFor(capsule.AdapterPi).(*PiAdapter)
	cl := s.AdapterFor(capsule.AdapterClaude).(*ClaudeAdapter)
	if pi.env == nil || cl.env == nil || pi.env != cl.env {
		t.Fatal("the pi and claude adapters must share ONE envSanitizer instance")
	}
	for _, tc := range []struct {
		name      string
		adapter   Adapter
		additions []string
	}{
		{"pi", pi, []string{
			"PI_SESSION_DIR=/caps/harness/pi/session-data",
			"PF_SESSION_ID=sess_1", "PF_WORK_ITEM_ID=wi_1", "PF_GENERATION=2",
		}},
		{"claude", cl, []string{
			"PF_SESSION_ID=sess_1", "PF_WORK_ITEM_ID=wi_1",
		}},
	} {
		cmd, err := tc.adapter.BuildCmd(daemon.SpawnParams{
			CapsuleDir: "/caps", SessionID: "sess_1", WorkItemID: "wi_1",
			Generation: 2, InitialPrompt: "X",
		}, "/caps/harness/pi/session-data")
		if err != nil {
			t.Fatal(err)
		}
		env := map[string]string{}
		for _, kv := range cmd.Env {
			name, val, _ := strings.Cut(kv, "=")
			env[name] = val
			if strings.HasPrefix(name, "POLYFORGE_") {
				t.Fatalf("%s adapter env must not carry POLYFORGE_* var %s", tc.name, name)
			}
			if val == key {
				t.Fatalf("%s adapter env carries the machine api key value in %s", tc.name, name)
			}
		}
		for _, add := range tc.additions {
			name, want, _ := strings.Cut(add, "=")
			if env[name] != want {
				t.Fatalf("%s adapter env must carry addition %s, got %q", tc.name, add, env[name])
			}
		}
		if env["OPS_RENAMED_KEY"] != "" {
			t.Fatalf("%s adapter env must strip the renamed key-value variable", tc.name)
		}
		if env["HARNESS_KEEP_ME"] != "unrelated" {
			t.Fatalf("%s adapter env must keep unrelated variables", tc.name)
		}
		if env["PATH"] == "" {
			t.Fatalf("%s adapter env must keep PATH", tc.name)
		}
	}
}
