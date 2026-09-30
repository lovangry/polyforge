//go:build ignore

// gate-metrics computes the nine M1–M9 pilot gate-metric readings defined by
// docs/pilot/v1-phase-0-report.md §2a (mechanical computation formulas on the
// v1 live stack) and mechanically evaluates the PILOT-V1…PILOT-V8 veto gates
// whose formulas are fixed verbatim in docs/design/polyforge-vnext/
// 07-deployment-pilot.md §9.3. It is a purely offline, strictly read-only
// calculator over a pilot data root:
//
//	<root>/<machine>/events.log      daemon event logs (JSONL), one dir per machine
//	<root>/<machine>/git/*.git       per-WI bare repositories (read-only git)
//	<root>/aihub-data/git/*.git      Core-side copies of the same bare repos
//	                                (deduplicated by repo name against the
//	                                machine-side copies)
//	<root>/aihub-data/aihub.db      Core SQLite database, opened read-only
//	                                (machines heartbeat face + WI sample size)
//
// runtime-state/execution.json is read read-through from each WI bare repo at
// HEAD (and, for M5, at every first-parent commit). releases/ is not used by
// any formula and is never opened. The tool writes no files; everything goes
// to stdout. Missing configuration (the M8 latency threshold T and the M3
// heartbeat liveness threshold are optional flags with NO defaults) and
// missing data faces (machines table with 0 rows, event bodies predating the
// write_ms/commit_ms/push_ms observation fields, zero-sample windows, zero
// denominators) are reported honestly as such — the tool never invents a
// number and never silently exempts a component.
//
// Usage:
//
//	gate-metrics [--json] [--m8-threshold-ms T] [--m3-heartbeat-max-age D] <pilot-data-root>
//
// Run from the repo root via:
//
//	go run ./tools/gate-metrics.go /root/polyforge-pilot-v1
//
// Kept out of the normal build path via the `ignore` build tag (same
// convention as v0/api/openapi-lint.go) so it does not collide with the main
// function of tools/capsule-audit.go in the same directory.
package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	_ "modernc.org/sqlite"
)

// ---------------------------------------------------------------------------
// Metric result structures (shared by the human table and the JSON output).
// ---------------------------------------------------------------------------

// M1 现场一致性: HEAD regression count along each bare repo's first-parent
// chain, read from the per-commit Polyforge-Seq trailer.
type M1 struct {
	Incidents               int      `json:"incidents"`
	ReposScanned            int      `json:"repos_scanned"`
	EmptyRepos              int      `json:"empty_repos"`
	CommitsScanned          int      `json:"commits_scanned"`
	MissingTrailer          int      `json:"commits_missing_seq_trailer"`
	PushFailedTotal         int      `json:"push_failed_events_total"`
	CorroboratingPushFailed int      `json:"push_failed_hook_rejection_corroboration_not_counted"`
	Details                 []string `json:"incident_details,omitempty"`
}

// M2 误删: v1 has no delete face, so numerator = denominator = 0 is the
// mechanical true reading (a structure, not an exemption).
type M2 struct {
	Numerator   int    `json:"numerator"`
	Denominator int    `json:"denominator"`
	Note        string `json:"note"`
}

// M3 卡死步: steps whose status is in_progress for more than 24h. The
// heartbeat liveness half of the definition is only judgeable when the
// --m3-heartbeat-max-age flag is provided AND the machines table has rows.
type M3 struct {
	StuckSteps          int      `json:"in_progress_over_24h"`
	InProgressTotal     int      `json:"in_progress_total"`
	OldestInProgress    string   `json:"oldest_in_progress_age,omitempty"`
	HeartbeatConfigured bool     `json:"heartbeat_threshold_configured"`
	MachinesRows        int      `json:"machines_rows"`
	HeartbeatNote       string   `json:"heartbeat_note"`
	WISample            int      `json:"wi_sample"`
	Note                string   `json:"note"`
	Details             []string `json:"stuck_steps,omitempty"`
}

// M4 校验真实性: evidence ratio over executed verification steps.
type M4 struct {
	Numerator   int     `json:"numerator"`
	Denominator int     `json:"denominator"`
	RatioPct    float64 `json:"ratio_pct"`
	// Denominator splits for honest reporting:
	NotExecuted          int    `json:"verification_steps_not_executed_pending"`
	NoEvidenceOldSchema  int    `json:"no_evidence_commands_field_absent"`
	NoEvidenceUnreadable int    `json:"no_evidence_commands_not_readable"`
	PerWI                []M4WI `json:"per_wi"`
}

type M4WI struct {
	WI          string `json:"wi"`
	Status      string `json:"status"`
	Executed    bool   `json:"executed"`
	RawCommands int    `json:"raw_commands"` // -1 = field absent (old schema)
	Readable    int    `json:"readable_commands"`
}

// M5 谎报逃逸: steps of kind ∈ {code, author} whose completion claim shows
// no output on any of the three independent channels (§9.2a).
type M5 struct {
	Reading     int      `json:"reading"`
	Determinate bool     `json:"determinate"`
	Candidates  int      `json:"candidates_b_and_c"`
	Evaluated   int      `json:"steps_evaluated_done"`
	NotDone     int      `json:"steps_skipped_not_done"`
	UnknownKind int      `json:"steps_skipped_kind_undetermined"`
	ConditionA  string   `json:"condition_a_worktree_diff"`
	Details     []string `json:"candidate_details,omitempty"`
	Notes       []string `json:"notes"`
}

// M6 检索实时性: rolling-24h-window p99 of memory_remember.write_ms samples.
type M6 struct {
	Samples         int     `json:"samples"`
	MemoryEvents    int     `json:"memory_remember_events"`
	EventsWithField int     `json:"events_with_write_ms_field"`
	MaxRollingP99   float64 `json:"max_rolling_24h_p99_ms"`
	LatestP99       float64 `json:"latest_window_p99_ms"`
	LatestSamples   int     `json:"latest_window_samples"`
	Threshold       float64 `json:"threshold_ms"`
	ThresholdSrc    string  `json:"threshold_source"`
	Judged          bool    `json:"judged"`
	Note            string  `json:"note,omitempty"`
}

// M7 同步收敛: count of ≥3-consecutive-failed-publish-attempt segments per WI.
type M7 struct {
	Count  int      `json:"count"`
	MaxRun int      `json:"max_consecutive_failed_attempts"`
	PerWI  []string `json:"per_wi_runs,omitempty"`
	Notes  []string `json:"notes"`
}

// M8 发布时延与结果: over-threshold / persistent-failure share of publish
// attempts. T is the optional --m8-threshold-ms flag; without it the latency
// component is reported as not configured / not judged, while the failure
// component and the denominator still count.
type M8 struct {
	Published           int      `json:"published"`
	PushFailed          int      `json:"push_failed"`
	WrapBlocked         int      `json:"wrap_blocked"`
	PushBackpressure    int      `json:"push_backpressure_excluded"`
	Denominator         int      `json:"denominator"`
	LatencyConfigured   bool     `json:"latency_threshold_configured"`
	LatencyThresholdMs  float64  `json:"latency_threshold_ms,omitempty"`
	LatencyCarriers     int      `json:"published_events_with_commit_ms_push_ms"`
	LatencyOver         int      `json:"latency_over_threshold"`
	LatencyStatus       string   `json:"latency_component_status"`
	FailurePersistent   int      `json:"failure_component_persistent"`
	FailureTransient    int      `json:"failure_component_transient_excluded"`
	FailureUnclassified int      `json:"failure_component_unclassified_not_counted"`
	FailureDetails      []string `json:"failure_details,omitempty"`
	Numerator           int      `json:"numerator"`
	RatioPct            float64  `json:"ratio_pct"`
	Judged              bool     `json:"judged"`
}

// M9 rewind 收敛 (observation item, no gate): per-WI rewind_stats read
// verbatim from execution.json at HEAD; no cross-WI aggregation.
type M9 struct {
	PerWI       []M9WI   `json:"per_wi"`
	AbsentCount int      `json:"wis_missing_rewind_stats_field"`
	Notes       []string `json:"notes"`
}

type M9WI struct {
	WI          string          `json:"wi"`
	RewindStats json.RawMessage `json:"rewind_stats,omitempty"`
	Absent      bool            `json:"absent"`
}

type Veto struct {
	ID       string `json:"id"`
	Formula  string `json:"formula"`
	Reading  string `json:"reading"`
	Judgment string `json:"judgment"`
	Note     string `json:"note,omitempty"`
}

// ---------------------------------------------------------------------------
// Input loading.
// ---------------------------------------------------------------------------

// event is one JSONL line of a machine's events.log.
type event struct {
	At       time.Time
	Kind     string
	WorkItem string
	Body     map[string]any
}

// bareRepo is one canonical bare repository (deduplicated across locations).
type bareRepo struct {
	Name            string
	Canonical       string   // path whose chain is scanned
	Locations       []string // every location a copy was found in
	Head            string
	Chain           []string         // first-parent commits, oldest first
	Empty           bool             // bare repo with no commits
	Execs           []map[string]any // execution.json per chain commit; nil where absent
	ExecAtHead      map[string]any
	ExecAtHeadOK    bool
	ExecParseErrors int
	Notes           []string
}

// aihubInfo is what the tool reads from aihub-data/aihub.db (read-only).
type aihubInfo struct {
	Available    bool
	Note         string
	MachinesRows int
	Machines     map[string]string // machine id -> last_heartbeat_at
	WITotal      int
	ActiveOwners map[string]string // wi id -> machine id
}

func main() {
	jsonOut := flag.Bool("json", false, "JSON 输出到 stdout（默认输出人读表）")
	m8T := flag.String("m8-threshold-ms", "", "M8 耗时阈值 T（毫秒；可选，无默认值；未提供时 M8 耗时分量输出为未配置/不参与判定，失败分量与分母仍计入）")
	hbAge := flag.String("m3-heartbeat-max-age", "", "M3 心跳存活阈值（如 30m、24h；可选，无默认值；未提供时心跳数据不可用/未配置，不参与判定）")
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: gate-metrics [--json] [--m8-threshold-ms T] [--m3-heartbeat-max-age D] <pilot-data-root>")
		fmt.Fprintln(os.Stderr, "计算 M1–M9 当前读数并按 07-deployment-pilot.md §9.3 机械判定 PILOT-V1…V8；纯只读，全部输出走 stdout")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	var m8Ms float64
	m8Configured := false
	if *m8T != "" {
		v, err := strconv.ParseFloat(*m8T, 64)
		if err != nil || v < 0 {
			fmt.Fprintf(os.Stderr, "gate-metrics: --m8-threshold-ms: 无效数值 %q（不设默认值，绝不代填）\n", *m8T)
			os.Exit(2)
		}
		m8Ms, m8Configured = v, true
	}
	var hbMax time.Duration
	hbConfigured := false
	if *hbAge != "" {
		v, err := time.ParseDuration(*hbAge)
		if err != nil || v <= 0 {
			fmt.Fprintf(os.Stderr, "gate-metrics: --m3-heartbeat-max-age: 无效时长 %q（不设默认值，绝不代填）\n", *hbAge)
			os.Exit(2)
		}
		hbMax, hbConfigured = v, true
	}
	if err := run(flag.Arg(0), *jsonOut, m8Configured, m8Ms, hbConfigured, hbMax); err != nil {
		fmt.Fprintf(os.Stderr, "gate-metrics: %v\n", err)
		os.Exit(1)
	}
}

type machineStat struct {
	Dir         string `json:"dir"`
	Events      int    `json:"events"`
	ParseErrors int    `json:"parse_errors"`
}

type reposStat struct {
	Canonical        int      `json:"canonical_repos"`
	MergedDuplicates int      `json:"merged_duplicate_copies"`
	Empty            int      `json:"empty_repos"`
	HeadMismatches   int      `json:"head_mismatches"`
	Notes            []string `json:"notes"`
}

type report struct {
	DataRoot    string        `json:"data_root"`
	ReadingTime string        `json:"reading_time"`
	Machines    []machineStat `json:"machines"`
	Repos       reposStat     `json:"repos"`
	AIhub       aihubJSON     `json:"aihub"`
	M1          M1            `json:"m1"`
	M2          M2            `json:"m2"`
	M3          M3            `json:"m3"`
	M4          M4            `json:"m4"`
	M5          M5            `json:"m5"`
	M6          M6            `json:"m6"`
	M7          M7            `json:"m7"`
	M8          M8            `json:"m8"`
	M9          M9            `json:"m9"`
	Vetoes      []Veto        `json:"pilot_vetoes"`
	M9Note      string        `json:"m9_note"`
}

type aihubJSON struct {
	Available    bool   `json:"available"`
	MachinesRows int    `json:"machines_rows"`
	WITotal      int    `json:"work_items_total"`
	Note         string `json:"note,omitempty"`
}

func run(root string, jsonOut, m8Configured bool, m8Ms float64, hbConfigured bool, hbMax time.Duration) error {
	now := time.Now()
	fi, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s: not a directory", root)
	}

	// -- machine dirs & events ------------------------------------------------
	machineDirs, err := discoverMachineDirs(root)
	if err != nil {
		return err
	}
	if len(machineDirs) == 0 {
		return fmt.Errorf("%s: no machine directories containing events.log", root)
	}
	var events []event
	var machineStats []machineStat
	for _, dir := range machineDirs {
		evs, parseErrs, err := readEvents(dir)
		if err != nil {
			return err
		}
		events = append(events, evs...)
		machineStats = append(machineStats, machineStat{Dir: filepath.Base(dir), Events: len(evs), ParseErrors: parseErrs})
	}
	sort.Slice(events, func(i, j int) bool {
		if events[i].At.Equal(events[j].At) {
			return events[i].Kind < events[j].Kind
		}
		return events[i].At.Before(events[j].At)
	})

	// -- bare repos (deduplicated) ---------------------------------------------
	repos, reposNotes, mergedDup, headMismatch := discoverRepos(root, machineDirs)
	var emptyRepos int
	for _, repo := range repos {
		if err := loadRepoChain(repo); err != nil {
			return err
		}
		if repo.Empty {
			emptyRepos++
		}
		reposNotes = append(reposNotes, repo.Notes...)
	}

	// -- aihub (read-only) -------------------------------------------------------
	ai := readAihub(filepath.Join(root, "aihub-data", "aihub.db"))

	// -- metrics ---------------------------------------------------------------
	m1 := computeM1(repos, events)
	m2 := M2{
		Numerator:   0,
		Denominator: 0,
		Note:        "v1 无删除面（13 路由 verb 无 delete）⇒ 分子=分母=0 为机械真读（结构读数，非豁免）；PILOT-V2 以分子>0 判，未来引入删除面后读数自然受判",
	}
	m3 := computeM3(repos, ai, now, hbConfigured, hbMax)
	m4 := computeM4(repos)
	m5 := computeM5(repos)
	m6 := computeM6(events)
	m7 := computeM7(events)
	m8 := computeM8(events, m8Configured, m8Ms)
	m9 := computeM9(repos)

	rep := report{
		DataRoot:    root,
		ReadingTime: now.UTC().Format(time.RFC3339),
		Machines:    machineStats,
		Repos: reposStat{
			Canonical: len(repos), MergedDuplicates: mergedDup, Empty: emptyRepos,
			HeadMismatches: headMismatch, Notes: reposNotes,
		},
		AIhub: aihubJSON{Available: ai.Available, MachinesRows: ai.MachinesRows, WITotal: ai.WITotal, Note: ai.Note},
		M1:    m1, M2: m2, M3: m3, M4: m4, M5: m5, M6: m6, M7: m7, M8: m8, M9: m9,
	}
	rep.Vetoes = computeVetoes(rep)
	rep.M9Note = "M9 不设否决门槛（观测项）：rewind 数量上界已由 01-workflow-skills.md §6.2 max_per_step/max_total 收敛（07 §9.3 收尾注）"

	if jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		enc.SetEscapeHTML(false)
		return enc.Encode(rep)
	}
	printTable(rep)
	return nil
}

// discoverMachineDirs returns the subdirectories of root that contain an
// events.log file (the per-machine daemon data directories).
func discoverMachineDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if fi, err := os.Stat(filepath.Join(dir, "events.log")); err == nil && !fi.IsDir() {
			dirs = append(dirs, dir)
		}
	}
	sort.Strings(dirs)
	return dirs, nil
}

// readEvents parses one machine dir's events.log (JSONL, one event per
// line). Unparsable lines are skipped and counted, never silently invented.
func readEvents(dir string) (evs []event, parseErrors int, err error) {
	f, err := os.Open(filepath.Join(dir, "events.log"))
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var raw struct {
			At       string         `json:"at"`
			Kind     string         `json:"kind"`
			WorkItem string         `json:"work_item"`
			Body     map[string]any `json:"body"`
		}
		if err := json.Unmarshal(line, &raw); err != nil {
			parseErrors++
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, raw.At)
		if err != nil {
			parseErrors++
			continue
		}
		evs = append(evs, event{At: at, Kind: raw.Kind, WorkItem: raw.WorkItem, Body: raw.Body})
	}
	if err := sc.Err(); err != nil {
		return nil, parseErrors, err
	}
	return evs, parseErrors, nil
}

// discoverRepos collects every *.git directory under each machine dir's git/
// plus the Core-side copies under aihub-data/git/, deduplicating by repo
// name. The machine-side copy is canonical; the Core-side copy is only used
// for names that exist nowhere else. HEAD mismatches between copies of the
// same name are reported (the longest chain wins).
func discoverRepos(root string, machineDirs []string) (repos []*bareRepo, notes []string, merged, headMismatch int) {
	type sighting struct{ path string }
	byName := map[string][]string{} // name -> locations (paths)
	add := func(dir string) {
		ents, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, e := range ents {
			if !e.IsDir() || !strings.HasSuffix(e.Name(), ".git") {
				continue
			}
			byName[e.Name()] = append(byName[e.Name()], filepath.Join(dir, e.Name()))
		}
	}
	for _, m := range machineDirs {
		add(filepath.Join(m, "git"))
	}
	add(filepath.Join(root, "aihub-data", "git")) // Core-side copies (dedup'd below)

	names := make([]string, 0, len(byName))
	for n := range byName {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		paths := byName[name]
		merged += len(paths) - 1
		var machinePaths []string
		for _, p := range paths {
			if !strings.Contains(p, filepath.Join("aihub-data", "git")) {
				machinePaths = append(machinePaths, p)
			}
		}
		candidates := machinePaths
		if len(candidates) == 0 {
			candidates = paths // only Core-side copy exists
			notes = append(notes, fmt.Sprintf("%s: 仅见于 aihub-data/git（机器目录无此仓）", strings.TrimSuffix(name, ".git")))
		}
		// Canonical: longest first-parent chain among candidates; verify the
		// other copies agree on HEAD.
		best := ""
		bestLen := -1
		heads := map[string]string{}
		for _, p := range paths {
			h, ok := gitRevParseHead(p)
			heads[p] = h
			if !ok {
				continue
			}
			n, err := gitRevListCount(p)
			if err != nil {
				continue
			}
			if n > bestLen {
				best, bestLen = p, n
			}
		}
		if best == "" {
			best = candidates[0]
		}
		repo := &bareRepo{Name: strings.TrimSuffix(name, ".git"), Canonical: best, Locations: paths}
		headSet := map[string]bool{}
		for _, h := range heads {
			headSet[h] = true
		}
		if len(headSet) > 1 {
			headMismatch++
			parts := []string{}
			for p, h := range heads {
				parts = append(parts, filepath.Base(filepath.Dir(filepath.Dir(p)))+":"+h)
			}
			sort.Strings(parts)
			repo.Notes = append(repo.Notes, fmt.Sprintf("%s: HEAD 不一致（%s），取链最长者为规范仓", strings.TrimSuffix(name, ".git"), strings.Join(parts, ", ")))
		}
		repos = append(repos, repo)
	}
	return repos, notes, merged, headMismatch
}

// loadRepoChain reads the first-parent commit chain (oldest first) and the
// runtime-state/execution.json document at every chain commit.
func loadRepoChain(repo *bareRepo) error {
	out, ok := gitRevParseHead(repo.Canonical)
	if !ok {
		repo.Empty = true
		return nil
	}
	repo.Head = out
	chain, err := git(repo.Canonical, "rev-list", "--first-parent", "--reverse", "HEAD")
	if err != nil {
		return fmt.Errorf("%s: %w", repo.Name, err)
	}
	repo.Chain = strings.Fields(chain)
	repo.Execs = make([]map[string]any, len(repo.Chain))
	for i, c := range repo.Chain {
		doc, parseErr := readExecAt(repo.Canonical, c)
		if parseErr != nil {
			repo.ExecParseErrors++
		}
		repo.Execs[i] = doc
	}
	if n := len(repo.Chain); n > 0 {
		repo.ExecAtHead = repo.Execs[n-1]
		repo.ExecAtHeadOK = repo.Execs[n-1] != nil
	}
	return nil
}

// readExecAt reads runtime-state/execution.json at one commit (read-through).
// A missing file or unparsable body yields (nil, error).
func readExecAt(repoPath, commit string) (map[string]any, error) {
	out, ok := gitShowPath(repoPath, commit, "runtime-state/execution.json")
	if !ok {
		return nil, fmt.Errorf("no runtime-state/execution.json at %s", commit)
	}
	var doc map[string]any
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		return nil, err
	}
	return doc, nil
}

// ---------------------------------------------------------------------------
// M1 — 现场一致性 (seq trailer regression scan over first-parent chains).
// ---------------------------------------------------------------------------

var seqRe = regexp.MustCompile(`(?m)^Polyforge-Seq:\s*(\d+)\s*$`)

func computeM1(repos []*bareRepo, events []event) M1 {
	r := M1{}
	for _, repo := range repos {
		r.ReposScanned++
		if repo.Empty {
			r.EmptyRepos++
			continue
		}
		r.CommitsScanned += len(repo.Chain)
		prev := -1
		havePrev := false
		for _, c := range repo.Chain {
			body, err := git(repo.Canonical, "cat-file", "commit", c)
			if err != nil {
				continue
			}
			m := seqRe.FindStringSubmatch(body)
			if m == nil {
				r.MissingTrailer++
				continue
			}
			seq, _ := strconv.Atoi(m[1])
			if havePrev && seq <= prev {
				r.Incidents++
				kind := "seq 回退"
				if seq == prev {
					kind = "同 seq 异 commit（HEAD 后移）"
				}
				r.Details = append(r.Details, fmt.Sprintf("%s %s: seq %d → %d（%s）", repo.Name, c, prev, seq, kind))
			}
			prev, havePrev = seq, true
		}
	}
	// 未遂回退：hook 拒绝类的 push_failed（pre-receive / 非 FF / ref 删除 /
	// gitlink / blob 超限 / seq 违规 / attempt-id 不匹配）记为佐证，不计入
	// 事故数；其余 push_failed（如鉴权失败）不是回退佐证，不冒充（§2a M1）。
	for _, e := range events {
		if e.Kind != "push_failed" {
			continue
		}
		r.PushFailedTotal++
		s := strings.ToLower(pushErrorTail(str(e.Body["error"])))
		for _, p := range m1Rejection {
			if strings.Contains(s, p) {
				r.CorroboratingPushFailed++
				break
			}
		}
	}
	return r
}

// pushErrorTail strips the echoed push command line from a push_failed error
// string (the error body embeds "git push --push-option=attempt-id=… <url>
// HEAD:refs/heads/main: <actual git error>"); classification must run on the
// actual error, otherwise the echoed "--push-option=attempt-id=" would
// false-positive the attempt-id rejection class.
func pushErrorTail(err string) string {
	const marker = "head:refs/heads/main: "
	if i := strings.LastIndex(strings.ToLower(err), marker); i >= 0 {
		return err[i+len(marker):]
	}
	return err
}

// m1Rejection lists the pre-receive hook rejection classes whose push_failed
// errors count as attempted-regression corroboration (§2a M1 口径).
var m1Rejection = []string{
	"pre-receive", "non-fast-forward", "non-fast forward", "refusing to delete",
	"deleting current", "gitlink", "attempt-id", "polyforge-seq",
	"seq violation", "blob",
}

// ---------------------------------------------------------------------------
// M3 — 卡死步 (in_progress > 24h) + heartbeat face.
// ---------------------------------------------------------------------------

func computeM3(repos []*bareRepo, ai aihubInfo, now time.Time, hbConfigured bool, hbMax time.Duration) M3 {
	r := M3{HeartbeatConfigured: hbConfigured, MachinesRows: ai.MachinesRows, WISample: ai.WITotal}
	var oldest time.Duration
	haveOldest := false
	for _, repo := range repos {
		if !repo.ExecAtHeadOK {
			continue
		}
		steps, _ := repo.ExecAtHead["steps"].(map[string]any)
		for _, sid := range sortedKeys(steps) {
			step, _ := steps[sid].(map[string]any)
			if step == nil || str(step["status"]) != "in_progress" {
				continue
			}
			// Start time: step_started_at (newer schema) with started_at fallback.
			ts := str(step["step_started_at"])
			if ts == "" {
				ts = str(step["started_at"])
			}
			if ts == "" {
				r.Details = append(r.Details, fmt.Sprintf("%s/%s: in_progress 但无起始时间戳，不计入（无法判龄）", repo.Name, sid))
				continue
			}
			t, err := time.Parse(time.RFC3339, ts)
			if err != nil {
				r.Details = append(r.Details, fmt.Sprintf("%s/%s: 起始时间不可解析（%s），不计入", repo.Name, sid, ts))
				continue
			}
			age := now.Sub(t)
			r.InProgressTotal++
			if !haveOldest || age > oldest {
				oldest, haveOldest = age, true
			}
			if age <= 24*time.Hour {
				continue
			}
			r.StuckSteps++
			// 心跳半边：仅当阈值已配置且 machines 表有据时才可判。
			hbState := "心跳数据不可用（未配置阈值）"
			if hbConfigured {
				if ai.MachinesRows == 0 {
					hbState = "心跳数据不可用（machines 表 0 行，心跳面未接线为已知偏差）"
				} else if m := ai.ActiveOwners[repo.Name]; m == "" {
					hbState = "心跳数据不可用（无活跃 owner generation 记录机器）"
				} else if hb, ok := ai.Machines[m]; !ok {
					hbState = fmt.Sprintf("心跳数据不可用（machine %s 不在 machines 表）", m)
				} else {
					hbt, err := time.Parse(time.RFC3339, hb)
					if err != nil {
						hbState = "心跳时间不可解析"
					} else if now.Sub(hbt) > hbMax {
						hbState = fmt.Sprintf("机器 %s 无心跳（last_heartbeat_at=%s 超过存活阈值）", m, hb)
					} else {
						hbState = fmt.Sprintf("机器 %s 心跳存活（不满足无心跳条件，不计入）", m)
						r.StuckSteps--
					}
				}
			}
			r.Details = append(r.Details, fmt.Sprintf("%s/%s: in_progress %s（%s）", repo.Name, sid, age.Round(time.Second), hbState))
		}
	}
	if haveOldest {
		r.OldestInProgress = oldest.Round(time.Second).String()
	}
	switch {
	case !hbConfigured:
		r.HeartbeatNote = "心跳数据不可用/未配置，不参与判定（--m3-heartbeat-max-age 未提供，不设默认值）"
	case ai.MachinesRows == 0:
		r.HeartbeatNote = "心跳阈值已配置，但 machines 表 0 行（心跳面未接线为已知偏差，#11 登记表有据），不参与判定"
	default:
		r.HeartbeatNote = "心跳阈值已配置，按 machines.last_heartbeat_at 判定"
	}
	r.Note = "step 状态读自已发布 HEAD 的 execution.json read-through（发布失败期间的机器本地最新状态不可见于离线数据），如实呈现"
	return r
}

// ---------------------------------------------------------------------------
// M4 — 校验真实性 (structured command evidence ratio).
// ---------------------------------------------------------------------------

func computeM4(repos []*bareRepo) M4 {
	r := M4{}
	for _, repo := range repos {
		if !repo.ExecAtHeadOK {
			continue
		}
		steps, _ := repo.ExecAtHead["steps"].(map[string]any)
		step, _ := steps["verification"].(map[string]any)
		if step == nil {
			continue
		}
		status := str(step["status"])
		row := M4WI{WI: repo.Name, Status: status, RawCommands: -1}
		if status == "pending" || status == "" {
			// 未执行：不计分母。
			r.NotExecuted++
			row.Executed = false
			r.PerWI = append(r.PerWI, row)
			continue
		}
		row.Executed = true
		r.Denominator++
		output, _ := step["output"].(map[string]any)
		if output != nil {
			if cmds, ok := output["commands"].([]any); ok {
				row.RawCommands = len(cmds)
				for _, c := range cmds {
					if commandEvidence(c) {
						row.Readable++
					}
				}
			}
		}
		if row.Readable > 0 {
			r.Numerator++
		} else if row.RawCommands < 0 {
			r.NoEvidenceOldSchema++ // 旧 schema：无 commands 字段，不计分子
		} else {
			r.NoEvidenceUnreadable++ // 有 commands 字段但无可读条目
		}
		r.PerWI = append(r.PerWI, row)
	}
	if r.Denominator > 0 {
		r.RatioPct = float64(r.Numerator) / float64(r.Denominator) * 100
	}
	return r
}

// commandEvidence reports whether one output.commands entry is a readable
// {argv, exit_code, duration_ms} triple (INV-D3: exit_code 可读；任意非空
// output 不构成证据).
func commandEvidence(v any) bool {
	e, ok := v.(map[string]any)
	if !ok {
		return false
	}
	switch argv := e["argv"].(type) {
	case []any:
		if len(argv) == 0 {
			return false
		}
	case string:
		if strings.TrimSpace(argv) == "" {
			return false
		}
	default:
		return false
	}
	return e["exit_code"] != nil && e["duration_ms"] != nil
}

// ---------------------------------------------------------------------------
// M5 — 谎报逃逸 (three-condition mechanical signal, §9.2a, kind ∈ {code,author}).
// ---------------------------------------------------------------------------

func computeM5(repos []*bareRepo) M5 {
	r := M5{
		ConditionA: "不可评估：step_start_refs 指向 repos/<name> 内层仓库的 commit，而 /repos/ 不入 capsule 同步（.gitignore），内层仓库对象不存在于 pilot 数据根 —— 条件(a)离线不可解析，如实呈现",
		Notes: []string{
			"条件(b) 基线取该 step 完成提交的前一个 capsule 发布提交（发布发生在 step 交界；中途恢复性发布会使基线偏晚，已登记）",
			"命中 ⊆ 候选：候选 = 条件(b)∧(c) 同时成立的 step。候选为 0 时，三条件不可能同时成立，读数 0 机械成立",
		},
	}
	for _, repo := range repos {
		if !repo.ExecAtHeadOK {
			continue
		}
		kinds := stepKinds(repo)
		steps, _ := repo.ExecAtHead["steps"].(map[string]any)
		for _, sid := range sortedKeys(steps) {
			kind, known := kinds[sid]
			if !known {
				if isWorkStep(steps, sid) {
					r.UnknownKind++
				}
				continue
			}
			if kind != "code" && kind != "author" {
				continue
			}
			if statusOf(steps, sid) != "done" {
				r.NotDone++ // 未声称完成，不参与逃逸判定
				continue
			}
			// Find every not-done → done transition along the chain (rewinds
			// can re-complete a step); evaluate each transition.
			transitions := 0
			stepCandidate := false
			for i := range repo.Chain {
				doc := repo.Execs[i]
				if doc == nil {
					continue
				}
				stepsI, _ := doc["steps"].(map[string]any)
				stepI, _ := stepsI[sid].(map[string]any)
				nowDone := stepI != nil && str(stepI["status"]) == "done"
				var prevDone bool
				if i > 0 && repo.Execs[i-1] != nil {
					stepsP, _ := repo.Execs[i-1]["steps"].(map[string]any)
					stepP, _ := stepsP[sid].(map[string]any)
					prevDone = stepP != nil && str(stepP["status"]) == "done"
				}
				if nowDone && !prevDone {
					transitions++
					if evalM5Transition(repo, i, sid, stepI) {
						stepCandidate = true
					}
				}
			}
			if transitions == 0 {
				// Step is done at HEAD but no transition observed (e.g. chain
				// truncation); evaluate at HEAD conservatively.
				if evalM5Transition(repo, len(repo.Chain)-1, sid, nil) {
					stepCandidate = true
				}
			}
			r.Evaluated++
			if stepCandidate {
				r.Candidates++
				r.Details = append(r.Details, fmt.Sprintf("%s/%s (kind=%s): 条件(b)(c) 同时成立，待条件(a)核验", repo.Name, sid, kind))
			}
		}
	}
	if r.Candidates == 0 {
		r.Reading = 0
		r.Determinate = true
	} else {
		r.Reading = r.Candidates
		r.Determinate = false
	}
	return r
}

// evalM5Transition evaluates conditions (b) and (c) for one done-transition of
// step sid at chain index i. If step is nil it is re-read from the doc.
func evalM5Transition(repo *bareRepo, i int, sid string, step map[string]any) bool {
	doc := repo.Execs[i]
	if step == nil {
		steps, _ := doc["steps"].(map[string]any)
		step, _ = steps[sid].(map[string]any)
	}
	if step == nil {
		return false
	}
	// (c) output 为空（含 output.commands 为空/缺失）。
	condC := outputEmpty(step["output"])
	// (b) artifacts/ 无新增：end 提交相对前一 capsule 提交。
	condB := false
	if i == 0 {
		out, err := git(repo.Canonical, "ls-tree", "--name-only", repo.Chain[i], "artifacts/")
		condB = err == nil && strings.TrimSpace(out) == ""
	} else {
		out, err := git(repo.Canonical, "diff", "--name-only", "--diff-filter=A", repo.Chain[i-1], repo.Chain[i], "--", "artifacts/")
		condB = err == nil && strings.TrimSpace(out) == ""
	}
	return condB && condC
}

func isWorkStep(steps map[string]any, sid string) bool {
	return statusOf(steps, sid) == "done"
}

func statusOf(steps map[string]any, sid string) string {
	step, _ := steps[sid].(map[string]any)
	if step == nil {
		return ""
	}
	return str(step["status"])
}

// outputEmpty implements M5 condition (c): the step's output field carries no
// non-empty content (null, empty object/array/string).
func outputEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case map[string]any:
		return len(t) == 0
	case []any:
		return len(t) == 0
	case string:
		return strings.TrimSpace(t) == ""
	default:
		return false
	}
}

// stepKinds reads the bundle workflow frontmatter at HEAD and returns the
// step id → kind mapping (01-workflow-skills.md §4.4).
func stepKinds(repo *bareRepo) map[string]string {
	kinds := map[string]string{}
	addWorkflow := func(path string) {
		out, ok := gitShowPath(repo.Canonical, "HEAD", path)
		if !ok {
			return
		}
		fm, ok := frontmatter(out)
		if !ok {
			return
		}
		var wf struct {
			Steps []struct {
				ID   string `yaml:"id"`
				Kind string `yaml:"kind"`
			} `yaml:"steps"`
		}
		if err := yaml.Unmarshal([]byte(fm), &wf); err != nil {
			return
		}
		for _, s := range wf.Steps {
			if s.ID != "" {
				kinds[s.ID] = s.Kind
			}
		}
	}
	if doc := repo.ExecAtHead; doc != nil {
		if bundle := str(doc["bundle_name"]); bundle != "" {
			addWorkflow("workflows/" + bundle + "/workflow.md")
		}
	}
	if len(kinds) == 0 {
		out, err := git(repo.Canonical, "ls-tree", "--name-only", "HEAD", "workflows/")
		if err != nil {
			return kinds
		}
		for _, w := range strings.Fields(out) {
			addWorkflow(strings.TrimSuffix(w, "/") + "/workflow.md")
		}
	}
	return kinds
}

// frontmatter extracts the YAML frontmatter block of a markdown file.
func frontmatter(content string) (string, bool) {
	lines := strings.Split(content, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return "", false
	}
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			return strings.Join(lines[1:i], "\n"), true
		}
	}
	return "", false
}

// ---------------------------------------------------------------------------
// M6 — 检索实时性 (rolling 24h p99 of memory_remember.write_ms samples).
// ---------------------------------------------------------------------------

func computeM6(events []event) M6 {
	r := M6{
		Threshold:    5000,
		ThresholdSrc: "06-memory-search.md#§4.5「写入→可检索 p99 ≤ 5s（5000 ms；固定值，不可配置）」（仅引用，不新造）",
	}
	type sample struct {
		t  time.Time
		ms float64
	}
	var samples []sample
	for _, e := range events {
		if e.Kind != "memory_remember" {
			continue
		}
		r.MemoryEvents++
		if ms, ok := num(e.Body["write_ms"]); ok {
			samples = append(samples, sample{t: e.At, ms: ms})
			r.EventsWithField++
		}
	}
	r.Samples = len(samples)
	if r.Samples == 0 {
		r.Note = "无样本：events.log 中 0 条 memory_remember 事件携带 write_ms 字段（旧 schema 事件面未含观测字段；迁移/服务端 reconcile 回写不产生事件，为已登记的样本集边界）→ 该窗口不参与否决判定"
		return r
	}
	// Rolling window anchored at every sample: (t-24h, t]; p99 per window.
	for i := range samples {
		j := i
		for j > 0 && samples[i].t.Sub(samples[j-1].t) <= 24*time.Hour {
			j--
		}
		w := make([]float64, 0, i-j+1)
		for k := j; k <= i; k++ {
			w = append(w, samples[k].ms)
		}
		p := p99(w)
		if p > r.MaxRollingP99 {
			r.MaxRollingP99 = p
		}
	}
	// Latest window: trailing 24h from the newest sample.
	latestEnd := samples[len(samples)-1].t
	w := []float64{}
	for _, s := range samples {
		if s.t.After(latestEnd.Add(-24 * time.Hour)) {
			w = append(w, s.ms)
		}
	}
	r.LatestSamples = len(w)
	r.LatestP99 = p99(w)
	r.Judged = true
	return r
}

// p99 is the nearest-rank percentile of a sample window.
func p99(sorted []float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	s := append([]float64(nil), sorted...)
	sort.Float64s(s)
	rank := int(math.Ceil(0.99 * float64(len(s))))
	if rank < 1 {
		rank = 1
	}
	return s[rank-1]
}

// ---------------------------------------------------------------------------
// M7 — 同步收敛 (≥3 consecutive failed publish attempts per WI).
// ---------------------------------------------------------------------------

func computeM7(events []event) M7 {
	r := M7{
		Notes: []string{
			"聚合口径：每次尝试恰一条 published 或 push_failed；任一 published 即收敛、终结该段",
			"push_backpressure 为降级段标记不计入；seal 路径内部重试聚合成一条 wrap_blocked，不产生逐次扫描样本",
		},
	}
	type st struct{ run, maxRun int }
	byWI := map[string]*st{}
	var wis []string
	get := func(wi string) *st {
		if s, ok := byWI[wi]; ok {
			return s
		}
		byWI[wi] = &st{}
		wis = append(wis, wi)
		return byWI[wi]
	}
	for _, e := range events {
		switch e.Kind {
		case "push_failed":
			s := get(e.WorkItem)
			s.run++
			if s.run > s.maxRun {
				s.maxRun = s.run
			}
		case "published":
			get(e.WorkItem).run = 0
		}
	}
	sort.Strings(wis)
	for _, wi := range wis {
		s := byWI[wi]
		if s.maxRun > r.MaxRun {
			r.MaxRun = s.maxRun
		}
		if s.maxRun >= 3 {
			r.Count++
			r.PerWI = append(r.PerWI, fmt.Sprintf("%s: 最长连续失败 %d 次", wi, s.maxRun))
		}
	}
	return r
}

// ---------------------------------------------------------------------------
// M8 — 发布时延与结果.
// ---------------------------------------------------------------------------

// Persistent (non-idempotent) failure classes per §2a M8: pre-receive
// rejections (non-FF / ref deletion / gitlink / blob over-limit / seq
// violation / attempt-id mismatch), bare-repo 404, auth/credential classes.
// Transient network/timeout classes are excluded. #11 register F2 404
// samples count per the existing口径 — no exemption.
var m8Persistent = []string{
	"pre-receive", "non-fast-forward", "non-fast forward", "rejected",
	"refusing to delete", "deleting current", "gitlink", "attempt-id",
	"polyforge-seq", "authentication", "credential", "unauthorized",
	"permission denied", "forbidden", "401", "403", "not found", "404",
}
var m8Transient = []string{
	"timeout", "timed out", "connection reset", "connection refused",
	"connection error", "connection aborted", "temporary", "dial tcp",
	"no route", "network", "broken pipe", "unexpected eof", "unreachable",
	"i/o error", "deadline",
}

func classifyPublishError(err string) string {
	s := strings.ToLower(pushErrorTail(err))
	for _, p := range m8Persistent {
		if strings.Contains(s, p) {
			return "persistent"
		}
	}
	for _, p := range m8Transient {
		if strings.Contains(s, p) {
			return "transient"
		}
	}
	return "unclassified"
}

func computeM8(events []event, tConfigured bool, tMs float64) M8 {
	r := M8{LatencyConfigured: tConfigured, LatencyThresholdMs: tMs}
	for _, e := range events {
		switch e.Kind {
		case "published":
			r.Published++
			cm, ok1 := num(e.Body["commit_ms"])
			pm, ok2 := num(e.Body["push_ms"])
			if ok1 && ok2 {
				r.LatencyCarriers++
				if tConfigured && cm+pm > tMs {
					r.LatencyOver++
				}
			}
		case "push_failed":
			r.PushFailed++
			cls := classifyPublishError(str(e.Body["error"]))
			r.classifyFailure(cls, e)
		case "wrap_blocked":
			r.WrapBlocked++
			cls := classifyPublishError(str(e.Body["error"]))
			r.classifyFailure(cls, e)
		case "push_backpressure":
			r.PushBackpressure++ // 降级标记：不计入分子与分母
		}
	}
	r.Denominator = r.Published + r.PushFailed + r.WrapBlocked
	switch {
	case !tConfigured:
		r.LatencyStatus = "未配置/不参与判定（--m8-threshold-ms 未提供，无默认值，不代填）"
	case r.LatencyCarriers == 0:
		r.LatencyStatus = fmt.Sprintf("已配置 T=%g ms，但 0 条 published 事件携带 commit_ms+push_ms 字段，该分量无样本", tMs)
	default:
		r.LatencyStatus = fmt.Sprintf("T=%g ms：超阈值 %d/%d 条（携带字段者）", tMs, r.LatencyOver, r.LatencyCarriers)
	}
	r.Numerator = r.FailurePersistent
	if tConfigured {
		r.Numerator += r.LatencyOver
	}
	if r.Denominator > 0 {
		r.RatioPct = float64(r.Numerator) / float64(r.Denominator) * 100
		r.Judged = true
	}
	return r
}

func (r *M8) classifyFailure(cls string, e event) {
	switch cls {
	case "persistent":
		r.FailurePersistent++
		r.FailureDetails = append(r.FailureDetails, fmt.Sprintf("%s %s %s: %s", e.At.UTC().Format(time.RFC3339), e.WorkItem, e.Kind, truncate(str(e.Body["error"]), 160)))
	case "transient":
		r.FailureTransient++
		r.FailureDetails = append(r.FailureDetails, fmt.Sprintf("%s %s %s（瞬时类，排除）: %s", e.At.UTC().Format(time.RFC3339), e.WorkItem, e.Kind, truncate(str(e.Body["error"]), 160)))
	default:
		r.FailureUnclassified++
		r.FailureDetails = append(r.FailureDetails, fmt.Sprintf("%s %s %s（未分类，不计入）: %s", e.At.UTC().Format(time.RFC3339), e.WorkItem, e.Kind, truncate(str(e.Body["error"]), 160)))
	}
}

// ---------------------------------------------------------------------------
// M9 — rewind 收敛 (per-WI verbatim read-through, observation item).
// ---------------------------------------------------------------------------

func computeM9(repos []*bareRepo) M9 {
	r := M9{
		Notes: []string{
			"逐 WI read-through 读取 execution.json rewind_stats，单 WI 生命周期累计值，不跨 WI 聚合；观测项，不设否决门槛（07 §9.3 收尾注）",
			"字段形态随 execution.json schema 版本而异（{total,by_step}/{count,to_step}/{total,per_step}），逐字原样呈现，不做归一化",
			"读数取自已发布 HEAD 的 execution.json；发布失败期间的机器本地最新状态不可见于离线数据",
		},
	}
	for _, repo := range repos {
		if !repo.ExecAtHeadOK {
			continue
		}
		v, ok := repo.ExecAtHead["rewind_stats"]
		if !ok || v == nil {
			r.AbsentCount++
			r.PerWI = append(r.PerWI, M9WI{WI: repo.Name, Absent: true})
			continue
		}
		raw, err := json.Marshal(v)
		if err != nil {
			r.AbsentCount++
			r.PerWI = append(r.PerWI, M9WI{WI: repo.Name, Absent: true})
			continue
		}
		r.PerWI = append(r.PerWI, M9WI{WI: repo.Name, RewindStats: raw})
	}
	return r
}

// ---------------------------------------------------------------------------
// PILOT-V1…V8 mechanical veto judgments (07 §9.3, formulas verbatim).
// ---------------------------------------------------------------------------

func computeVetoes(rep report) []Veto {
	v := []Veto{}
	add := func(id, formula, reading, judgment, note string) {
		v = append(v, Veto{ID: id, Formula: formula, Reading: reading, Judgment: judgment, Note: note})
	}

	add("PILOT-V1", "M1 计数 > 0（Pilot 累计运行以来任何一次 HEAD 回退）",
		strconv.Itoa(rep.M1.Incidents),
		judge(rep.M1.Incidents > 0), "")

	add("PILOT-V2", "M2 分子（实际移除数）> 0",
		strconv.Itoa(rep.M2.Numerator),
		judge(rep.M2.Numerator > 0), "分母口径 0（v1 无删除面的机械真读，不豁免）")

	sampleNote := ""
	if rep.M3.WISample > 0 {
		sampleNote = fmt.Sprintf("样本 %d 张 WI（门槛适用面为 ≥40 张 WI 的样本）", rep.M3.WISample)
	}
	add("PILOT-V3", "M3 计数 > 1（在 ≥ 40 张 WI 的样本内）",
		strconv.Itoa(rep.M3.StuckSteps),
		judge(rep.M3.StuckSteps > 1), sampleNote+"; "+rep.M3.HeartbeatNote)

	if rep.M4.Denominator == 0 {
		add("PILOT-V4", "M4 比例 < 100%（分母口径见 §9.2）", "分母 0", "不参与判定", "")
	} else {
		add("PILOT-V4", "M4 比例 < 100%（分母口径见 §9.2）",
			fmt.Sprintf("%d/%d = %.2f%%", rep.M4.Numerator, rep.M4.Denominator, rep.M4.RatioPct),
			judge(rep.M4.RatioPct < 100), "旧 schema（无 commands 字段）不计分子，仍计分母")
	}

	if !rep.M5.Determinate {
		add("PILOT-V5", "M5 计数（§9.2a 机械定义）> 0",
			fmt.Sprintf("候选 %d（条件(a)不可离线核验）", rep.M5.Candidates),
			"判定不完整", rep.M5.ConditionA)
	} else {
		add("PILOT-V5", "M5 计数（§9.2a 机械定义）> 0",
			strconv.Itoa(rep.M5.Reading),
			judge(rep.M5.Reading > 0), "条件(b)(c) 已全量核验且无同时成立者")
	}

	if !rep.M6.Judged {
		add("PILOT-V6", "M6 滚动 24h p99 > 5000 ms（06-memory-search.md#§4.5）", "无样本", "不参与判定", rep.M6.Note)
	} else {
		add("PILOT-V6", "M6 滚动 24h p99 > 5000 ms（06-memory-search.md#§4.5）",
			fmt.Sprintf("最大滚动窗口 p99 = %.0f ms", rep.M6.MaxRollingP99),
			judge(rep.M6.MaxRollingP99 > 5000), "")
	}

	add("PILOT-V7", "M7 计数 > 3",
		strconv.Itoa(rep.M7.Count),
		judge(rep.M7.Count > 3), fmt.Sprintf("最长连续失败尝试 %d 次", rep.M7.MaxRun))

	if !rep.M8.Judged {
		add("PILOT-V8", "M8 比例 > 1%", "分母 0", "不参与判定", "")
	} else {
		add("PILOT-V8", "M8 比例 > 1%",
			fmt.Sprintf("%d/%d = %.2f%%", rep.M8.Numerator, rep.M8.Denominator, rep.M8.RatioPct),
			judge(rep.M8.RatioPct > 1), "耗时分量: "+rep.M8.LatencyStatus)
	}
	return v
}

func judge(triggered bool) string {
	if triggered {
		return "触发（否决门槛成立）"
	}
	return "未触发"
}

// ---------------------------------------------------------------------------
// aihub.db (strictly read-only).
// ---------------------------------------------------------------------------

func readAihub(path string) aihubInfo {
	ai := aihubInfo{Machines: map[string]string{}, ActiveOwners: map[string]string{}}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		ai.Note = fmt.Sprintf("aihub.db 不可用（%v），心跳面与 WI 样本数不可读，如实呈现", err)
		return ai
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		ai.Note = fmt.Sprintf("aihub.db 不可用（%v），心跳面与 WI 样本数不可读，如实呈现", err)
		return ai
	}
	ai.Available = true
	if err := db.QueryRow("select count(*) from machines").Scan(&ai.MachinesRows); err != nil {
		ai.Note = "machines 表不可读: " + err.Error()
	}
	rows, err := db.Query("select id, last_heartbeat_at from machines")
	if err == nil {
		for rows.Next() {
			var id, hb string
			if rows.Scan(&id, &hb) == nil {
				ai.Machines[id] = hb
			}
		}
		rows.Close()
	}
	if err := db.QueryRow("select count(*) from work_items").Scan(&ai.WITotal); err != nil {
		ai.WITotal = 0
	}
	rows2, err := db.Query("select work_item_id, coalesce(machine_id,'') from owner_generations where ended_at is null")
	if err == nil {
		for rows2.Next() {
			var wi, m string
			if rows2.Scan(&wi, &m) == nil {
				ai.ActiveOwners[wi] = m
			}
		}
		rows2.Close()
	}
	return ai
}

// ---------------------------------------------------------------------------
// Human-readable table.
// ---------------------------------------------------------------------------

func printTable(rep report) {
	fmt.Printf("gate-metrics: pilot 数据根 %s（读取时刻 %s）\n", rep.DataRoot, rep.ReadingTime)
	for _, m := range rep.Machines {
		fmt.Printf("  机器目录 %s: events.log %d 行（解析失败 %d 行）\n", m.Dir, m.Events, m.ParseErrors)
	}
	fmt.Printf("  裸仓: 规范仓 %d（合并同仓副本 %d 份，HEAD 不一致 %d 处，空仓 %d 个）\n",
		rep.Repos.Canonical, rep.Repos.MergedDuplicates, rep.Repos.HeadMismatches, rep.Repos.Empty)
	for _, n := range rep.Repos.Notes {
		fmt.Printf("    - %s\n", n)
	}
	fmt.Printf("  aihub.db: 可读=%v，machines 表 %d 行，work_items %d 张\n", rep.AIhub.Available, rep.AIhub.MachinesRows, rep.AIhub.WITotal)
	if rep.AIhub.Note != "" {
		fmt.Printf("    - %s\n", rep.AIhub.Note)
	}
	fmt.Println()

	fmt.Printf("M1 现场一致性        %d（计数；first-parent 链 Polyforge-Seq 回退/HEAD 后移点）\n", rep.M1.Incidents)
	fmt.Printf("  扫描 %d 仓 / %d 提交（空仓 %d，缺 seq trailer 提交 %d）\n",
		rep.M1.ReposScanned, rep.M1.CommitsScanned, rep.M1.EmptyRepos, rep.M1.MissingTrailer)
	fmt.Printf("  push_failed 共 %d 条，其中 hook 拒绝类 %d 条记为未遂回退佐证（不计入事故数）\n",
		rep.M1.PushFailedTotal, rep.M1.CorroboratingPushFailed)
	for _, d := range rep.M1.Details {
		fmt.Printf("    - %s\n", d)
	}
	fmt.Println()

	fmt.Printf("M2 误删              %d/%d（分子/分母）\n", rep.M2.Numerator, rep.M2.Denominator)
	fmt.Printf("  %s\n", rep.M2.Note)
	fmt.Println()

	fmt.Printf("M3 卡死步            %d（in_progress 超 24h 的 step 数）\n", rep.M3.StuckSteps)
	fmt.Printf("  in_progress step 共 %d 个", rep.M3.InProgressTotal)
	if rep.M3.OldestInProgress != "" {
		fmt.Printf("，最长 %s", rep.M3.OldestInProgress)
	}
	fmt.Println()
	fmt.Printf("  心跳面: %s\n  %s\n", rep.M3.HeartbeatNote, rep.M3.Note)
	for _, d := range rep.M3.Details {
		fmt.Printf("    - %s\n", d)
	}
	fmt.Println()

	fmt.Printf("M4 校验真实性        %d/%d = %.2f%%\n", rep.M4.Numerator, rep.M4.Denominator, rep.M4.RatioPct)
	fmt.Printf("  已执行 verification step %d（另有 pending 未执行 %d 不计分母）；无证据分子外 %d 个旧 schema（无 commands 字段）+ %d 个 commands 无可读条目\n",
		rep.M4.Denominator, rep.M4.NotExecuted, rep.M4.NoEvidenceOldSchema, rep.M4.NoEvidenceUnreadable)
	fmt.Println()

	fmt.Printf("M5 谎报逃逸          %d（计数；§9.2a 三条件机械信号，kind ∈ {code, author}）\n", rep.M5.Reading)
	fmt.Printf("  已完成 code/author step 评估 %d 个（未完成跳过 %d 个，kind 不可判定跳过 %d 个）；候选（条件(b)∧(c)）%d 个\n",
		rep.M5.Evaluated, rep.M5.NotDone, rep.M5.UnknownKind, rep.M5.Candidates)
	fmt.Printf("  条件(a) 工作树 diff: %s\n", rep.M5.ConditionA)
	for _, n := range rep.M5.Notes {
		fmt.Printf("  - %s\n", n)
	}
	for _, d := range rep.M5.Details {
		fmt.Printf("    - %s\n", d)
	}
	fmt.Println()

	if !rep.M6.Judged {
		fmt.Printf("M6 检索实时性        无样本 → 不参与判定\n")
	} else {
		fmt.Printf("M6 检索实时性        最大滚动 24h 窗口 p99 = %.0f ms（最新窗口 %d 样本 p99 = %.0f ms）\n",
			rep.M6.MaxRollingP99, rep.M6.LatestSamples, rep.M6.LatestP99)
	}
	fmt.Printf("  memory_remember 事件 %d 条，其中携带 write_ms 字段 %d 条（样本 %d 个）；阈值权威 %s\n",
		rep.M6.MemoryEvents, rep.M6.EventsWithField, rep.M6.Samples, rep.M6.ThresholdSrc)
	if rep.M6.Note != "" {
		fmt.Printf("  %s\n", rep.M6.Note)
	}
	fmt.Println()

	fmt.Printf("M7 同步收敛          %d（计数；连续 ≥3 次发布尝试仍未收敛的段数）\n", rep.M7.Count)
	fmt.Printf("  最长连续失败尝试 %d 次\n", rep.M7.MaxRun)
	for _, d := range rep.M7.PerWI {
		fmt.Printf("    - %s\n", d)
	}
	fmt.Println()

	fmt.Printf("M8 发布时延与结果    %d/%d = %.2f%%\n", rep.M8.Numerator, rep.M8.Denominator, rep.M8.RatioPct)
	fmt.Printf("  分母 = published %d + push_failed %d + wrap_blocked %d（push_backpressure %d 条为降级标记，不计入分子与分母）\n",
		rep.M8.Published, rep.M8.PushFailed, rep.M8.WrapBlocked, rep.M8.PushBackpressure)
	fmt.Printf("  耗时分量: %s（携带 commit_ms+push_ms 字段的 published %d 条，超阈值 %d 条）\n",
		rep.M8.LatencyStatus, rep.M8.LatencyCarriers, rep.M8.LatencyOver)
	fmt.Printf("  失败分量: 持续类 %d（计入），瞬时类 %d（排除），未分类 %d（不计入）\n",
		rep.M8.FailurePersistent, rep.M8.FailureTransient, rep.M8.FailureUnclassified)
	for _, d := range rep.M8.FailureDetails {
		fmt.Printf("    - %s\n", d)
	}
	fmt.Println()

	fmt.Println("M9 rewind 收敛（观测项，不设否决门槛）— 逐 WI read-through，不跨 WI 聚合:")
	for _, w := range rep.M9.PerWI {
		if w.Absent {
			fmt.Printf("    %-24s （rewind_stats 字段缺失，旧 schema）\n", w.WI)
		} else {
			fmt.Printf("    %-24s %s\n", w.WI, string(w.RewindStats))
		}
	}
	fmt.Printf("  缺 rewind_stats 字段的 WI: %d 个\n", rep.M9.AbsentCount)
	fmt.Println()

	fmt.Println("PILOT-V1…V8 机械判定（判定式逐字引自 07-deployment-pilot.md#§9.3）:")
	for _, v := range rep.Vetoes {
		fmt.Printf("  %-9s 判定: %s\n            读数: %s\n", v.ID, v.Formula, v.Reading+" → "+v.Judgment)
		if v.Note != "" {
			fmt.Printf("            注: %s\n", v.Note)
		}
	}
	fmt.Printf("  M9        %s\n", rep.M9Note)
}

// ---------------------------------------------------------------------------
// Small helpers.
// ---------------------------------------------------------------------------

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func num(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, true
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	default:
		return 0, false
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// git runs a read-only git plumbing command against repoPath and returns its
// stdout. Optional locks are disabled so git cannot even momentarily write
// lock files into the target repo (same convention as capsule-audit).
func git(repoPath string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repoPath}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

// gitShowPath reads one blob at repoPath; ok=false when the path is absent.
func gitShowPath(repoPath, commit, path string) (string, bool) {
	out, err := git(repoPath, "show", commit+":"+path)
	if err != nil {
		return "", false
	}
	return out, true
}

func gitRevParseHead(repoPath string) (string, bool) {
	out, err := git(repoPath, "rev-parse", "--verify", "HEAD")
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(out), true
}

func gitRevListCount(repoPath string) (int, error) {
	out, err := git(repoPath, "rev-list", "--count", "HEAD")
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(out))
}
