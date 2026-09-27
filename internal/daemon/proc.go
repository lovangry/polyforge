package daemon

// proc.go: WI-Agent liveness with pid-reuse protection (04 §7.1 step 2): a
// recorded pid is "alive" only when the process exists AND its creation
// time matches the recorded main_agent_started_at. Linux reads
// /proc/<pid>/stat (field 22, starttime in ticks since boot) plus
// /proc/stat btime; on other platforms (or unreadable procfs) the time
// check degrades to pid-existence only.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

var (
	bootOnce  sync.Once
	bootTimeN int64
	hertz     int64 = 100 // USER_HZ
	// procRoot is the procfs mount the process checks read; a variable
	// only so the fail-closed branch of ProcessGroupEmpty is testable
	// (point it at a nonexistent path and the check must return
	// ErrGroupUnverifiable, never a fail-open "empty").
	procRoot = "/proc"
	// killProbe is the signal(2) face the identity guards probe with; a
	// variable only so the EPERM seams are testable (R4N-5: a kill(pid,0)
	// answering EPERM PROVES a process exists — it must fail closed as
	// occupied/unverifiable, never be read as "pid gone").
	killProbe = syscall.Kill
)

func bootTime() int64 {
	bootOnce.Do(func() {
		raw, err := os.ReadFile("/proc/stat")
		if err != nil {
			return
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "btime ") {
				v, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "btime ")), 10, 64)
				if err == nil {
					bootTimeN = v
				}
				return
			}
		}
	})
	return bootTimeN
}

// procStartWall returns the process creation wall-clock time, or 0 when
// unavailable.
func procStartWall(pid int) int64 {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// Field 22 is starttime; comm may contain spaces inside parentheses, so
	// split after the LAST ')'.
	s := string(raw)
	idx := strings.LastIndex(s, ")")
	if idx < 0 || idx+2 > len(s) {
		return 0
	}
	fields := strings.Fields(s[idx+2:])
	// fields[0] is state (field 3); starttime is field 22 → index 19.
	if len(fields) < 20 {
		return 0
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	bt := bootTime()
	if bt == 0 {
		return 0
	}
	return bt + ticks/hertz
}

func itoa(n int) string { return strconv.Itoa(n) }

// procStateOf extracts the state letter (field 3) from one raw
// /proc/<pid>/stat read. The comm field may contain spaces and
// parentheses, so the split happens after the LAST ')'.
func procStateOf(statRaw string) (string, bool) {
	idx := strings.LastIndex(statRaw, ")")
	if idx < 0 || idx+2 > len(statRaw) {
		return "", false
	}
	fields := strings.Fields(statRaw[idx+2:])
	if len(fields) < 1 {
		return "", false
	}
	return fields[0], true
}

// procZombie reports whether pid exists and sits in state Z: the process
// exited and only its parent's pending wait keeps the pid occupied. A
// zombie cannot write anything and cannot be a supervision target — for
// adoption purposes it is DEAD (04 §7.1: recovery must collect the
// residual group and re-derive, never adopt a reapable corpse).
func procZombie(pid int) bool {
	raw, err := os.ReadFile(filepath.Join(procRoot, itoa(pid), "stat"))
	if err != nil {
		return false
	}
	state, ok := procStateOf(string(raw))
	return ok && state == "Z"
}

// processAlive reports liveness with pid-reuse protection. A ZOMBIE is
// dead: kill(pid,0) succeeds for a corpse its parent has not reaped yet,
// but a state-Z process holds no writers and no control channel — the
// caller must take the collect + re-derive path, not adopt it.
func processAlive(pid int, startedAt string) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false // ESRCH: no such process
	}
	if procZombie(pid) {
		return false // state Z: exited, awaiting reap — dead for adoption
	}
	if startedAt == "" {
		return true
	}
	rec, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return true // unparseable record: fall back to pid existence
	}
	start := procStartWall(pid)
	if start == 0 {
		return true
	}
	// Tolerance covers HZ granularity and scheduling jitter.
	recUnix := rec.Unix()
	if start > recUnix-2 && start < recUnix+2 {
		return true
	}
	return false // pid exists but belongs to an unrelated new process
}

// procCreationMatches reports whether the CURRENT occupant of pid was
// created at the recorded started-at (± the same tolerance processAlive
// uses). A corpse (state Z) is deliberately dead for processAlive BEFORE
// the identity comparison, but a zombie still carries its own creation
// time in /proc — so a corpse's identity remains checkable, and residual
// collection must use exactly that check to tell OUR zombie (collect the
// surviving children) from a FOREIGN corpse that happened to reuse the
// recorded pid (typed skip, never signal).
func procCreationMatches(pid int, startedAt string) bool {
	if startedAt == "" {
		return true // no recorded identity: nothing to compare against
	}
	rec, err := time.Parse(time.RFC3339Nano, startedAt)
	if err != nil {
		return true // unparseable record: fall back to pid existence
	}
	start := procStartWall(pid)
	if start == 0 {
		return true
	}
	recUnix := rec.Unix()
	return start > recUnix-2 && start < recUnix+2
}

// ErrGroupUnverifiable is the typed fail-closed signal of an emptiness
// check that could not be PERFORMED (unreadable procfs): the caller must
// not grant the dead-writers shortcut on an unverifiable group and must
// log the failure loudly.
var ErrGroupUnverifiable = errors.New("daemon: process-group emptiness unverifiable (procfs unreadable)")

// ProcessGroupEmpty reports whether NO live process remains in the process
// group pgid (05 §2.7 shortcut condition ①: every member confirmed gone,
// not just the group leader). On Linux it scans /proc for entries whose
// pgrp field equals pgid; zombies are excluded — a zombie cannot write and
// is on its way to being reaped by its parent.
//
// FAILS CLOSED: the emptiness of the group can only be PROVEN by reading
// EVERY member's stat. R3N-F2: a per-entry read failure or a malformed
// stat makes the whole answer UNPROVABLE — the old code treated such an
// entry as exited ("raced exit: gone"), so a readable proc root with one
// unreadable or malformed member stat could answer (true, nil) without
// proof, granting the dead-writers shortcut on a group it never saw.
// Such an answer must never grant the shortcut: (bool, ErrGroupUnverifiable)
// is returned and callers deny. Only a pgid <= 0 is provably empty (no such
// group can hold writers).
func ProcessGroupEmpty(pgid int) (bool, error) {
	if pgid <= 0 {
		return true, nil
	}
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrGroupUnverifiable, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == 1 {
			continue // pid 1 never belongs to an agent group
		}
		raw, err := os.ReadFile(filepath.Join(procRoot, e.Name(), "stat"))
		if err != nil {
			// R3N-F2: an unreadable member stat means the entry's pgrp is
			// unknown — UNLESS the member itself is already gone: an ENOENT
			// whose pid directory also vanished is PROOF the member exited
			// between the readdir and this read (procfs is kernel-owned;
			// a live or zombie process always exposes its stat). Only a
			// still-present pid with an unreadable/missing stat makes the
			// whole answer unprovable — never a fail-open "empty".
			if os.IsNotExist(err) {
				if _, derr := os.Stat(filepath.Join(procRoot, e.Name())); os.IsNotExist(derr) {
					continue // the member exited mid-scan: provably gone
				}
			}
			return false, fmt.Errorf("%w: member %d stat unreadable: %v", ErrGroupUnverifiable, pid, err)
		}
		// comm may contain spaces inside parentheses: split after the LAST
		// ')'. fields[0]=state(3), fields[1]=ppid(4), fields[2]=pgrp(5).
		s := string(raw)
		idx := strings.LastIndex(s, ")")
		if idx < 0 || idx+2 > len(s) {
			return false, fmt.Errorf("%w: member %d stat malformed (no comm terminator)", ErrGroupUnverifiable, pid)
		}
		fields := strings.Fields(s[idx+2:])
		if len(fields) < 3 {
			return false, fmt.Errorf("%w: member %d stat malformed (truncated)", ErrGroupUnverifiable, pid)
		}
		if fields[0] == "Z" {
			continue // reaped-in-progress: cannot write
		}
		// R4N-1: a pgrp field that is not a number is a per-entry FAILURE,
		// same as an unreadable stat — the old code silently treated a
		// malformed pgrp as "not this group" and kept scanning, so one
		// corrupted member could hide a live writer and let the scan
		// answer (true, nil) it never proved. Fail closed, whole answer
		// unprovable.
		pgrp, err := strconv.Atoi(fields[2])
		if err != nil {
			return false, fmt.Errorf("%w: member %d pgrp field malformed: %v", ErrGroupUnverifiable, pid, err)
		}
		if pgrp == pgid {
			return false, nil // a live member remains
		}
	}
	return true, nil
}

// waitForGroupEmpty polls ProcessGroupEmpty until it holds or the bound
// elapses (04 §7.1 step 4 / §7.3 step 2: bounded wait between SIGTERM and
// the SIGKILL escalation). An UNVERIFIABLE group fails closed: false is
// returned immediately and the typed error is logged — a kill loop must
// never conclude "empty" from a check it could not perform.
func waitForGroupEmpty(pgid int, bound time.Duration) bool {
	deadline := time.Now().Add(bound)
	for {
		empty, err := ProcessGroupEmpty(pgid)
		if err != nil {
			fmt.Fprintf(os.Stderr, "pf-daemon: process group %d emptiness unverifiable, failing closed: %v\n", pgid, err)
			return false
		}
		if empty {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// ErrPIDReused is the typed signal that the recorded agent pid is now
// occupied by an UNRELATED live process: the residual group's id can no
// longer be proven ours, so collection is skipped rather than risking an
// innocent group. Callers must NOT re-derive on this error without a
// separate proof that no residual writers remain (single-writer domain,
// 03 §7).
var ErrPIDReused = errors.New("daemon: recorded agent pid reused by an unrelated process")

// ErrResidualOwnershipUnprovable is the typed fail-closed signal of the
// "recorded pid is gone" collection branch (R4N-2): the process group that
// branch was about to signal could not be PROVEN to still belong to the
// dead recorded agent — the pid reappeared between the check and the
// signal, a probe answered anything but ESRCH, or a live group member was
// born outside the dead agent's provable lifetime (the pid number may
// have been reused as a FRESH group leader in the race window, and
// kill(-pgid) would then take an innocent group down). NOTHING was
// signaled; the caller must surface this for human attention instead of
// collecting the group.
var ErrResidualOwnershipUnprovable = errors.New("daemon: residual process group ownership unprovable")

// ErrResidualRefusedToDie is the typed signal that the residual group's
// collection ran but could not PROVE the group dead (members survived
// SIGKILL, or the emptiness check was unverifiable). Re-derivation must
// not proceed: an unproven collection means possible residual writers
// next to the new instance (04 §7.1 step 4 requires CONFIRMED absence).
var ErrResidualRefusedToDie = errors.New("daemon: residual process group could not be proven dead")

// collectResidualProcessGroup collects the process group of a DEAD
// recorded WI Agent before a re-derivation (04 §7.1 step 4: "collect the
// residual process group first, then re-derive" — orphans a dead agent
// left behind would keep writing the capsule while the new instance
// starts). It returns nil ONLY when the group's death (or vacuous
// emptiness) is PROVEN; every other outcome is a typed error and the
// caller must deny the re-derivation (R3N-F3: collection used to return
// nothing, so an unprovable death silently let re-derivation proceed).
//
// Identity cases, in order:
//   - pid <= 0: nothing recorded — vacuously nothing to collect.
//   - ALIVE and ours (pid + creation time match): not a residual — nil.
//   - ZOMBIE of ours (state Z): R3N-F3 — the state-Z process still IS our
//     exited leader (kill(pid,0) succeeds for a corpse, so the old code
//     mistook it for pid reuse and skipped the surviving children,
//     leaving two writer generations). Collect the group's live members,
//     then attempt a non-blocking reap (only its PARENT can reap — after
//     a daemon crash that is init; if the caller happens to be the
//     parent, the wait clears the corpse).
//   - live with MISMATCHED identity: genuine pid reuse. If procfs PROVES
//     the group empty there are no residual writers (nil, nothing
//     signaled); otherwise the group id can no longer be proven ours —
//     typed ErrPIDReused, never a signal.
//   - probe answers anything but success/ESRCH (e.g. EPERM): the pid is
//     occupied or unverifiable — typed ErrResidualOwnershipUnprovable,
//     never a signal (R4N-5 discipline: EPERM PROVES a process exists).
//   - pid gone (ESRCH): the leader is dead and reaped, but R4N-2: "gone"
//     no longer proves the GROUP ours — the pid number can be reused as
//     a NEW group leader between the check and the signal, and kill(-pgid)
//     would then kill an innocent group. Ownership is PROVEN before any
//     group signal (see proveResidualGroupOurs); an unprovable group is a
//     typed ErrResidualOwnershipUnprovable and the caller must deny —
//     human attention, never a blind group signal.
func collectResidualProcessGroup(pid int, startedAt string) error {
	if pid <= 0 {
		return nil
	}
	if processAlive(pid, startedAt) {
		return nil // alive (and ours): not a residual to collect
	}
	if procZombie(pid) && procCreationMatches(pid, startedAt) {
		// R3N-F3: OUR OWN corpse still pins the group id — collect the
		// surviving children instead of mistaking the corpse for reuse.
		// (A FOREIGN zombie — creation time mismatched — falls through to
		// the reuse branch below and is never signaled.)
		if err := killResidualGroup(pid); err != nil {
			return err
		}
		// Best-effort reap: succeeds only when the caller is the zombie's
		// parent (after a daemon crash the corpse was reparented to init,
		// which reaps it on its own); ECHILD for everyone else is fine.
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &ws, syscall.WNOHANG, nil)
		return nil
	}
	if probe := killProbe(pid, 0); probe == nil {
		// pid exists but is neither ours nor a zombie: reused by an
		// unrelated live process. Nothing may be signaled; emptiness can
		// still be PROVEN read-only through procfs.
		empty, perr := ProcessGroupEmpty(pid)
		if perr != nil {
			return fmt.Errorf("%w: group %d emptiness unverifiable, collection skipped: %v", ErrPIDReused, pid, perr)
		}
		if empty {
			// Proven: no member remains in the group id, so no residual
			// writer of ours exists. (The unrelated replacement process sits
			// in its own group — it is invisible to this check and untouched.)
			return nil
		}
		return fmt.Errorf("%w: group %d still holds members the collection cannot safely signal", ErrPIDReused, pid)
	} else if !errors.Is(probe, syscall.ESRCH) {
		// R4N-5 discipline at this seam: only ESRCH PROVES the pid gone.
		// EPERM proves a process EXISTS (one this daemon may not probe),
		// and any other error leaves the pid's occupancy unverifiable —
		// neither may reach the group signal. Fail closed, typed.
		return fmt.Errorf("%w: pid %d probe answered %v (occupied or unverifiable), no signal was sent", ErrResidualOwnershipUnprovable, pid, probe)
	}
	// The recorded pid is gone (ESRCH). R4N-2: the old code went straight
	// to killResidualGroup on the assumption that any live member of
	// group `pid` must be an orphan of OUR dead agent — but the pid number
	// can be REUSED as a new group leader in the window between this
	// check and the signal, and the group would then be an innocent one.
	// Prove ownership before any signal; an unprovable group is a typed
	// error and nothing is signaled.
	observedGone := time.Now()
	if reprobe := killProbe(pid, 0); !errors.Is(reprobe, syscall.ESRCH) {
		return fmt.Errorf("%w: recorded pid %d reappeared between the check and the signal (probe: %v) — a fresh group leader cannot be excluded, no signal was sent", ErrResidualOwnershipUnprovable, pid, reprobe)
	}
	if err := proveResidualGroupOurs(pid, startedAt, observedGone); err != nil {
		return err
	}
	return killResidualGroup(pid)
}

// proveResidualGroupOurs proves — before ANY group signal in the
// "recorded pid is gone" branch — that the process group pgid still
// belongs to the dead recorded agent (R4N-2). The threat: the recorded
// pid number, freed by the leader's death and reaping, is reused as a
// NEW group leader somewhere in the race window, and kill(-pgid) would
// then collect an innocent group. The proof walks every LIVE member of
// the group through procfs and requires each member's creation time to
// fall inside the dead agent's provable lifetime:
//
//   - at/after the recorded agent start (a member born BEFORE the old
//     agent started cannot be its child — provably not ours), and
//   - no later than the moment the leader pid was observed gone, on the
//     stat file's own ticks-since-boot scale with the same ±2s tolerance
//     the pid-reuse guard uses (a member born clearly AFTER the
//     observation cannot be a child of the already-dead agent —
//     provably an innocent reuse of the pid number). The tolerance is
//     deliberate: USER_HZ granularity and the wall-to-uptime mapping
//     cannot honestly cut finer, and a strict reading would refuse the
//     agent's own just-forked children at every fast collection; the
//     pid-still-gone re-probe above covers the live innocent leader.
//
// Every member that cannot be proven (unreadable or malformed stat,
// unparseable creation time, unavailable boot time) makes the WHOLE
// answer unprovable — the same per-entry fail-closed discipline as
// ProcessGroupEmpty (R3N-F2/R4N-1). An empty group is vacuously ours:
// signaling it is a no-op either way.
func proveResidualGroupOurs(pgid int, startedAt string, observedGone time.Time) error {
	if pgid <= 0 {
		return nil
	}
	// Lower bound: the recorded agent start, when parseable. An empty or
	// unparseable record drops the lower bound (the upper bound still
	// guards the fresh-reuse race); processAlive/procCreationMatches use
	// the same fallback convention.
	recUnix := int64(0)
	if rec, err := time.Parse(time.RFC3339Nano, startedAt); err == nil {
		recUnix = rec.Unix()
	}
	bt := bootTime()
	if bt == 0 {
		return fmt.Errorf("%w: boot time unavailable, member creation times unprovable", ErrResidualOwnershipUnprovable)
	}
	// The gone-observation mapped onto the stat file's own tick scale
	// (starttime is ticks since boot at USER_HZ), plus the ±2s tolerance.
	obsTicks := (observedGone.Unix()-bt)*hertz + int64(observedGone.Nanosecond())*hertz/1e9
	obsBound := obsTicks + 2*hertz
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return fmt.Errorf("%w: procfs unreadable: %v", ErrResidualOwnershipUnprovable, err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == 1 {
			continue // pid 1 never belongs to an agent group
		}
		raw, rerr := os.ReadFile(filepath.Join(procRoot, e.Name(), "stat"))
		if rerr != nil {
			// Same exit-mid-scan proof as ProcessGroupEmpty: an ENOENT
			// whose pid directory also vanished is a member that exited
			// between the readdir and this read — provably gone.
			if os.IsNotExist(rerr) {
				if _, derr := os.Stat(filepath.Join(procRoot, e.Name())); os.IsNotExist(derr) {
					continue
				}
			}
			return fmt.Errorf("%w: member %d stat unreadable: %v", ErrResidualOwnershipUnprovable, pid, rerr)
		}
		// comm may contain spaces inside parentheses: split after the LAST
		// ')'. fields[0]=state(3), fields[2]=pgrp(5), fields[19]=starttime(22).
		s := string(raw)
		idx := strings.LastIndex(s, ")")
		if idx < 0 || idx+2 > len(s) {
			return fmt.Errorf("%w: member %d stat malformed (no comm terminator)", ErrResidualOwnershipUnprovable, pid)
		}
		fields := strings.Fields(s[idx+2:])
		if len(fields) < 20 {
			return fmt.Errorf("%w: member %d stat malformed (truncated before starttime)", ErrResidualOwnershipUnprovable, pid)
		}
		if fields[0] == "Z" {
			continue // reaped-in-progress: holds no writers and dies with the parent
		}
		pgrp, perr := strconv.Atoi(fields[2])
		if perr != nil {
			return fmt.Errorf("%w: member %d pgrp field malformed: %v", ErrResidualOwnershipUnprovable, pid, perr)
		}
		if pgrp != pgid {
			continue
		}
		ticks, terr := strconv.ParseInt(fields[19], 10, 64)
		if terr != nil {
			return fmt.Errorf("%w: member %d starttime field malformed: %v", ErrResidualOwnershipUnprovable, pid, terr)
		}
		start := bt + ticks/hertz
		if recUnix != 0 && start < recUnix-2 {
			// Born before the old agent started (same ±2s tolerance the
			// pid-reuse guard uses): provably not our agent's child.
			return fmt.Errorf("%w: group %d member %d was created at %d, before the recorded agent start %d — provably not ours, no signal was sent", ErrResidualOwnershipUnprovable, pgid, pid, start, recUnix)
		}
		if ticks > obsBound {
			// Born beyond the tolerance of the leader-gone observation: our
			// agent was already dead, so this member can only be an innocent
			// reuse of the pid number as a fresh group's member.
			return fmt.Errorf("%w: group %d member %d was created at %d, after the gone-observation — a pid reused as a fresh group leader cannot be excluded, no signal was sent", ErrResidualOwnershipUnprovable, pgid, pid, start)
		}
	}
	return nil
}

// killResidualGroup signals the residual group SIGTERM→bounded wait→
// SIGKILL→bounded wait and returns nil only when the group is PROVEN
// empty afterwards (R3N-F3: the emptiness check must be a proof, not an
// assumption — an unverifiable or non-empty group is a typed error).
func killResidualGroup(pgid int) error {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	if !waitForGroupEmpty(pgid, 10*time.Second) {
		_ = syscall.Kill(-pgid, syscall.SIGKILL)
		if !waitForGroupEmpty(pgid, 5*time.Second) {
			return fmt.Errorf("%w: group %d (unverifiable or surviving SIGKILL)", ErrResidualRefusedToDie, pgid)
		}
	}
	// Final proof: waitForGroupEmpty returned true only from a clean
	// (bool, nil) answer, so the group death is confirmed.
	return nil
}
