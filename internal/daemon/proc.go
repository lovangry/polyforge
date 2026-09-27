package daemon

// proc.go: WI-Agent liveness with pid-reuse protection (04 §7.1 step 2): a
// recorded pid is "alive" only when the process exists AND its creation
// time matches the recorded main_agent_started_at. Linux reads
// /proc/<pid>/stat (field 22, starttime in ticks since boot) plus
// /proc/stat btime; on other platforms (or unreadable procfs) the time
// check degrades to pid-existence only.

import (
	"os"
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

// processAlive reports liveness with pid-reuse protection.
func processAlive(pid int, startedAt string) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false // ESRCH: no such process
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
