package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// migrate-memories wiring: flag validation happens before any network or
// filesystem work, so the pure usage errors are testable without a server.

func TestMigrateMemoriesUsageErrors(t *testing.T) {
	dir := t.TempDir()
	input := filepath.Join(dir, "input.jsonl")
	if err := os.WriteFile(input, []byte("\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := Config{Server: "http://127.0.0.1:1", APIKey: "k"}

	// Missing --data-dir.
	code, _, errOut := runCLI(t, cfg, "", "migrate-memories", input)
	if code != 1 || !strings.Contains(errOut, "--data-dir is required") {
		t.Errorf("missing data-dir: code=%d err=%q", code, errOut)
	}

	// Missing input path (both flags present, no positional).
	code, _, errOut = runCLI(t, cfg, "", "migrate-memories", "--data-dir", dir, "--aihub-url", cfg.Server)
	if code != 1 || !strings.Contains(errOut, "exactly one input JSONL path") {
		t.Errorf("missing input: code=%d err=%q", code, errOut)
	}

	// Bad --since timestamp.
	code, _, errOut = runCLI(t, cfg, "", "migrate-memories",
		"--data-dir", dir, "--aihub-url", cfg.Server, "--since", "not-a-time", input)
	if code != 1 || !strings.Contains(errOut, "--since") {
		t.Errorf("bad since: code=%d err=%q", code, errOut)
	}

	// Missing api key.
	code, _, errOut = runCLI(t, Config{Server: cfg.Server}, "", "migrate-memories",
		"--data-dir", dir, "--aihub-url", cfg.Server, input)
	if code != 1 || !strings.Contains(errOut, "admin api key") {
		t.Errorf("missing key: code=%d err=%q", code, errOut)
	}

	// The verb is in the registry and its usage carries the documented
	// psql export contract.
	if cmd := lookupCommand("migrate-memories"); cmd == nil {
		t.Fatal("migrate-memories not registered")
	} else if !strings.Contains(cmd.Usage, "json_build_object") {
		t.Fatalf("usage must document the psql export:\n%s", cmd.Usage)
	}
}
