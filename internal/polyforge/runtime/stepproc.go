package runtime

// stepproc.go: is a process part of a step agent's tree? (polyforge#122)
//
// Step agents are untrusted, and they can reach the daemon's UDS face:
// in #122 a grill-review agent answered its own human question through
// `pf answer` and the workflow moved on. The daemon therefore refuses an
// answer whose UDS peer (SO_PEERCRED pid) belongs to a step process
// tree. Membership is read from procfs with the markers every step
// process is spawned with (PF_WI_ID + PF_DAEMON_ID, the same markers the
// layer-4 cleanup sweep keys on): a process carries them itself, or one
// of its ancestors does (an `env -u` exec still has a marked parent), or
// the leader of its process group does (a double-forked child that was
// reparented to init stays in the step's group — the step agent is its
// own group leader). A process that left both the tree and the group
// (setsid) AND cleared its environment is outside this check.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// maxAncestry bounds the parent walk (a cycle cannot occur in procfs,
// but a racing reparent must not loop forever).
const maxAncestry = 64

// StepProcessOrigin reports whether pid belongs to a step process tree of
// the daemon whose machine id is daemonID, and the PF_WI_ID of the step
// it belongs to. Unreadable procfs entries (another uid, an exit race)
// are skipped: the walk answers from what it can read.
func StepProcessOrigin(pid int, daemonID string) (wi string, ok bool) {
	seenGroups := map[int64]bool{}
	cur := pid
	for i := 0; i < maxAncestry && cur > 1; i++ {
		if wi, ok := stepMarkers(cur, daemonID); ok {
			return wi, true
		}
		_, ppid, pgrp, _, statOK := procStat(cur)
		if !statOK {
			return "", false
		}
		if pgrp > 1 && int(pgrp) != cur && !seenGroups[pgrp] {
			seenGroups[pgrp] = true
			if wi, ok := stepMarkers(int(pgrp), daemonID); ok {
				return wi, true
			}
		}
		cur = int(ppid)
	}
	return "", false
}

// stepMarkers reads /proc/<pid>/environ: a step process of this daemon
// carries PF_DAEMON_ID=<daemonID> and a non-empty PF_WI_ID.
func stepMarkers(pid int, daemonID string) (string, bool) {
	raw, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "environ"))
	if err != nil {
		return "", false
	}
	var wi string
	var ours bool
	for _, kv := range strings.Split(string(raw), "\x00") {
		switch {
		case strings.HasPrefix(kv, "PF_WI_ID="):
			wi = strings.TrimPrefix(kv, "PF_WI_ID=")
		case kv == "PF_DAEMON_ID="+daemonID:
			ours = true
		}
	}
	return wi, ours && wi != ""
}
