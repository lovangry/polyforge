package mainagent

// skills_contract_test.go: the drift guard for the two-sided stdio control
// channel. internal/mainagent (the Daemon-side supervisor) declares the
// method names as constants; skills/pf-execute/SKILL.md (the WI-Agent-side
// contract, injected into every spawned agent) must mention each name
// byte-identically. If either side drifts, this test fails — a renamed
// method would otherwise silently hang the supervisor's ack wait.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPfExecuteSkillDeclaresControlChannel(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "skills", "pf-execute", "SKILL.md"))
	if err != nil {
		t.Fatalf("pf-execute skill not readable from the daemon checkout: %v", err)
	}
	skill := string(raw)
	for _, m := range []string{MethodQuiesce, MethodResume, MethodStopNotice, MethodAnswer, MethodProbe} {
		if !strings.Contains(skill, m) {
			t.Errorf("skills/pf-execute/SKILL.md must declare the control method %q byte-identically with internal/mainagent", m)
		}
	}
	// The ack shape and the boundary discipline are part of the contract;
	// guard the ack shape too so a supervisor-side framing change cannot
	// pass silently.
	if !strings.Contains(skill, `"result":{"ack":"<method>"}`) {
		t.Error("skills/pf-execute/SKILL.md must declare the stdout ack shape {\"method\":\"<method>\",\"result\":{\"ack\":\"<method>\"}}")
	}
}
