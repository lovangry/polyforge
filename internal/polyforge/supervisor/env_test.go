package supervisor

import (
	"slices"
	"strings"
	"testing"
)

func TestDaemonEnvAllowList(t *testing.T) {
	in := []string{"PATH=/bin", "HOME=/h", "LC_ALL=C", "https_proxy=x", "NO_PROXY=y", "POLYFORGE_DATA_DIR=/d",
		"ANTHROPIC_AUTH_TOKEN=junk", "FOO_TOKEN=junk", "POLYFORGE_V1_API_KEY=junk", "TMUX=/t", "CLAUDECODE=1",
		"PF_CHAT_ID=c", MarkerEnv + "=caller", "PATH=/dup", "malformed", "=x"}
	var names []string
	for _, kv := range DaemonEnv(in) {
		name, _, _ := strings.Cut(kv, "=")
		names = append(names, name)
	}
	want := []string{"HOME", "LC_ALL", "NO_PROXY", "PATH", "POLYFORGE_DATA_DIR", "https_proxy"}
	if !slices.Equal(names, want) {
		t.Fatalf("DaemonEnv names = %v, want %v", names, want)
	}
	if got := DaemonEnv(in); !slices.Contains(got, "PATH=/bin") {
		t.Fatal("the first PATH entry must win")
	}
}

func TestSuspectDaemonEnv(t *testing.T) {
	got := SuspectDaemonEnv([]string{"PATH", "HOME", "ANTHROPIC_MODEL", "anthropic_x", "FOO_TOKEN", "API_KEY", "MY_SECRET",
		"DB_PASSWORD", "KEYBOARD", "TOKENIZER_DIR", "POLYFORGE_SUPERVISOR", "FOO_TOKEN"})
	want := []string{"ANTHROPIC_MODEL", "API_KEY", "DB_PASSWORD", "FOO_TOKEN", "MY_SECRET", "anthropic_x"}
	if !slices.Equal(got, want) {
		t.Fatalf("SuspectDaemonEnv = %v, want %v", got, want)
	}
	if got := EnvNames([]string{"B=1", "A=2", "B=3"}); !slices.Equal(got, []string{"A", "B"}) {
		t.Fatalf("EnvNames = %v", got)
	}
}
