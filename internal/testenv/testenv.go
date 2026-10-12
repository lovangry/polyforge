// Package testenv gives test binaries a hermetic process environment.
//
// Several production paths default to the operator's real home: the daemon
// data dir (~/.local/share/polyforge), the shared cli.toml
// (~/.config/polyforge), $XDG_RUNTIME_DIR scratch, the harness session
// store (~/.pi). A test that forgets an explicit flag would otherwise read
// or WRITE the real install. Every test package calls Main from its
// TestMain so HOME and the XDG/POLYFORGE_* roots point at a throwaway
// directory for the whole package run.
package testenv

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

const activeEnv = "TESTENV_ISOLATED"

// Main isolates the process environment, runs the package's tests and
// exits. Use as: func TestMain(m *testing.M) { testenv.Main(m) }.
func Main(m *testing.M) {
	code, err := run(m)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testenv:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func run(m *testing.M) (int, error) {
	// A test binary re-exec'd as a helper subprocess inherits the already
	// isolated environment: do not create (and leak) another home.
	if os.Getenv(activeEnv) != "" {
		return m.Run(), nil
	}
	os.Setenv(activeEnv, "1")
	home, err := os.MkdirTemp("", "pf-test-home-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(home)

	// Keep the operator's git identity/config readable (commits in test
	// repos need one) without letting HOME relocation hide it.
	if realHome, herr := os.UserHomeDir(); herr == nil && os.Getenv("GIT_CONFIG_GLOBAL") == "" {
		if gc := filepath.Join(realHome, ".gitconfig"); fileExists(gc) {
			os.Setenv("GIT_CONFIG_GLOBAL", gc)
		}
	}

	// Tests that shell out to the go toolchain (the acceptance harness
	// builds binaries) must keep the real build/module caches.
	if realHome, herr := os.UserHomeDir(); herr == nil {
		if os.Getenv("GOCACHE") == "" {
			cache := os.Getenv("XDG_CACHE_HOME")
			if cache == "" {
				cache = filepath.Join(realHome, ".cache")
			}
			os.Setenv("GOCACHE", filepath.Join(cache, "go-build"))
		}
		if os.Getenv("GOPATH") == "" {
			os.Setenv("GOPATH", filepath.Join(realHome, "go"))
		}
	}

	runtimeDir := filepath.Join(home, "run")
	if err := os.MkdirAll(runtimeDir, 0o700); err != nil {
		return 0, err
	}
	for k, v := range map[string]string{
		"HOME":               home,
		"XDG_CONFIG_HOME":    filepath.Join(home, ".config"),
		"XDG_DATA_HOME":      filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME":     filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME":     filepath.Join(home, ".cache"),
		"XDG_RUNTIME_DIR":    runtimeDir,
		"POLYFORGE_DATA_DIR": filepath.Join(home, ".local", "share", "polyforge"),
	} {
		os.Setenv(k, v)
	}
	// Ambient daemon/connection settings must not leak in either.
	for _, k := range []string{"PF_DAEMON_SOCKET", "PF_CONFIG", "POLYFORGE_CONFIG", "POLYFORGE_MACHINE_ID"} {
		os.Unsetenv(k)
	}
	return m.Run(), nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}
