// Package testenv gives test binaries a hermetic process environment.
//
// Several production paths default to the operator's real home: the daemon
// data dir (~/.local/share/polyforge), the shared cli.toml
// (~/.config/polyforge), $XDG_RUNTIME_DIR scratch, the harness session
// store (~/.pi). A test that forgets an explicit flag would otherwise read
// or WRITE the real install. Every test package calls Main from its
// TestMain so HOME and the XDG/POLYFORGE_* roots point at a throwaway
// directory for the whole package run.
//
// tmux is hermetic as well: TMUX_TMPDIR names a scratch dir and TMUX is
// unset, so a test that reaches tmux starts its own private server.
//
// Git configuration is hermetic too: GIT_CONFIG_GLOBAL names a scratch
// global config carrying only a test identity, and GIT_CONFIG_NOSYSTEM
// drops the system file, so no test reads (or, through
// `git config --global`, WRITES) the operator's real ~/.gitconfig, and no
// operator setting (core.hooksPath, url.insteadOf, ...) changes what a
// test observes. Main also guards the real file: its content is hashed
// before and after the run and a change fails the package.
package testenv

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

const activeEnv = "TESTENV_ISOLATED"

// Test git identity written into the scratch global config.
const (
	TestGitName  = "pf-test"
	TestGitEmail = "pf-test@invalid"
)

// Main isolates the process environment, runs the package's tests and
// exits. Use as: func TestMain(m *testing.M) { testenv.Main(m) }.
//
// atExit checks run after the tests, while the isolated environment is
// still in place; an error from any of them is reported and fails the
// run (e.g. the acceptance package asserts no process it started is
// still alive).
func Main(m *testing.M, atExit ...func() error) {
	code, err := run(m, atExit)
	if err != nil {
		fmt.Fprintln(os.Stderr, "testenv:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func run(m *testing.M, atExit []func() error) (int, error) {
	// A test binary re-exec'd as a helper subprocess inherits the already
	// isolated environment: do not create (and leak) another home.
	if os.Getenv(activeEnv) != "" {
		return m.Run(), nil
	}
	// The operator's real global git config files, located BEFORE HOME is
	// relocated, and their content fingerprint: the guard below fails the
	// run if a test modified (or created) one.
	guarded := realGitConfigs()
	before := fingerprint(guarded)

	os.Setenv(activeEnv, "1")
	home, err := os.MkdirTemp("", "pf-test-home-")
	if err != nil {
		return 0, err
	}
	defer os.RemoveAll(home)

	// Hermetic git: a scratch global config holding only the test
	// identity (commits in test repos need one), and no system config.
	gitGlobal := filepath.Join(home, ".gitconfig")
	ident := "[user]\n\tname = " + TestGitName + "\n\temail = " + TestGitEmail + "\n"
	if err := os.WriteFile(gitGlobal, []byte(ident), 0o600); err != nil {
		return 0, err
	}
	os.Setenv("GIT_CONFIG_GLOBAL", gitGlobal)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")

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
	// A private tmux server (polyforge#174): `pf daemon ensure` and the
	// doctor talk to the default tmux server, which is keyed by
	// TMUX_TMPDIR once TMUX is unset — never the operator's live one.
	tmuxDir := filepath.Join(home, "tmux")
	if err := os.MkdirAll(tmuxDir, 0o700); err != nil {
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
		"TMUX_TMPDIR":        tmuxDir,
	} {
		os.Setenv(k, v)
	}
	// Ambient daemon/connection settings must not leak in either.
	for _, k := range []string{"PF_DAEMON_SOCKET", "PF_WI_ID", "PF_DAEMON_ID", "POLYFORGE_MACHINE_ID", "TMUX", "TMUX_PANE", "PI_CODING_AGENT_DIR"} {
		os.Unsetenv(k)
	}
	code := m.Run()
	for _, check := range atExit {
		if cerr := check(); cerr != nil {
			fmt.Fprintln(os.Stderr, "testenv: FAIL:", cerr)
			code = 1
		}
	}
	if after := fingerprint(guarded); !bytes.Equal(before, after) {
		fmt.Fprintf(os.Stderr, "testenv: FAIL: the operator's real global git config changed during the test run (%v): a test wrote past the hermetic GIT_CONFIG_GLOBAL\n", guarded)
		code = 1
	}
	return code, nil
}

// realGitConfigs names the operator's real global git config files: the
// classic ~/.gitconfig and the XDG $XDG_CONFIG_HOME/git/config (default
// ~/.config/git/config). nil when no home is resolvable.
func realGitConfigs() []string {
	realHome, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	xdg := os.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(realHome, ".config")
	}
	return []string{filepath.Join(realHome, ".gitconfig"), filepath.Join(xdg, "git", "config")}
}

// fingerprint hashes the files' contents (a missing file hashes as
// absent, so creating one is a change too).
func fingerprint(paths []string) []byte {
	h := sha256.New()
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		switch {
		case err == nil:
			fmt.Fprintf(h, "%s present %d\n", p, len(raw))
			h.Write(raw)
		case errors.Is(err, fs.ErrNotExist):
			fmt.Fprintf(h, "%s absent\n", p)
		default:
			fmt.Fprintf(h, "%s unreadable %v\n", p, err)
		}
	}
	return h.Sum(nil)
}
