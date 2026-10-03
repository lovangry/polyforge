package main

// window_test.go: --window-since（D6）的 12 个枚举用例。全部走 computeReport
// 断言 report 结构（及其 JSON 编码），不解析 stdout；夹具见
// root_fixture_test.go。now 一律显式传入（fxNow），不依赖 time.Now()。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 固定时钟：窗口边界 since、读取时刻 now、边界两侧的 created_at 字面量。
var (
	fxSinceStr = "2026-10-01T00:00:00Z"
	fxSince    = fxMustTime(fxSinceStr)
	fxNow      = fxMustTime("2026-10-02T12:00:00Z")
	fxPreCre   = "2026-09-20T08:00:00Z" // 窗口前创建
	fxPostCre  = "2026-10-01T06:00:00Z" // 窗口内创建
	fxStuckAt  = "2026-09-30T00:00:00Z" // 距 now > 24h 的 in_progress 起始
	fxWf       = "---\nsteps:\n  - id: code_1\n    kind: code\n  - id: verification\n    kind: verification\n---\n"
)

func fxMustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// runUnwindowed: 不带 --window-since 的读数（win == nil，覆盖全部历史）。
func runUnwindowed(t *testing.T, root string) report {
	t.Helper()
	rep, err := computeReport(root, fxNow, false, 0, false, 0, nil)
	if err != nil {
		t.Fatalf("computeReport(无窗口): %v", err)
	}
	return rep
}

// runWindowed: --window-since fxSinceStr 的读数。
func runWindowed(t *testing.T, root string) report {
	t.Helper()
	rep, err := computeReport(root, fxNow, false, 0, false, 0, &windowSpec{Since: fxSince, Raw: fxSinceStr})
	if err != nil {
		t.Fatalf("computeReport(窗口): %v", err)
	}
	return rep
}

func findVeto(t *testing.T, rep report, id string) Veto {
	t.Helper()
	for _, v := range rep.Vetoes {
		if v.ID == id {
			return v
		}
	}
	t.Fatalf("未找到否决项 %s", id)
	return Veto{}
}

func m9Find(t *testing.T, rep report, wi string) M9WI {
	t.Helper()
	for _, w := range rep.M9.PerWI {
		if w.WI == wi {
			return w
		}
	}
	t.Fatalf("M9 未列出 %s", wi)
	return M9WI{}
}

// simpleRepo: 单提交裸仓（capsule.json created_at + 最小 execution.json）。
func simpleRepo(fx *rootFixture, wi, createdAt string) {
	fx.repo(wi, []fxCommit{{
		Message: "capsule publish",
		Files: map[string]string{
			"capsule.json":                 fxCapsule(wi, createdAt),
			"runtime-state/execution.json": mustJSON(fx.t, map[string]any{"steps": map[string]any{}}),
		},
	}})
}

// 用例 1：不带 flag = 今天的读数 —— Window == nil、JSON 无 window 键、
// 指标计入全部 WI/事件。
func TestNoFlagIdentity(t *testing.T) {
	fx := newRootFixture(t)
	simpleRepo(fx, "wi_pre", fxPreCre)
	simpleRepo(fx, "wi_post", fxPostCre)
	fx.event("2026-09-25T10:00:00Z", "published", "wi_pre", nil)
	fx.event("2026-10-01T10:00:00Z", "published", "wi_post", nil)
	fx.aihub([]fxMachineRow{{ID: "m1", LastHeartbeat: "2026-10-02T11:59:00Z"}},
		[]fxWorkItemRow{{ID: "wi_pre", CreatedAt: fxPreCre}, {ID: "wi_post", CreatedAt: fxPostCre}})
	root := fx.done()

	rep := runUnwindowed(t, root)
	if rep.Window != nil {
		t.Fatalf("无 flag 时 Window 应为 nil，得到 %+v", rep.Window)
	}
	if rep.M8.Published != 2 {
		t.Fatalf("无 flag 应计入全部事件：M8.Published = %d，want 2", rep.M8.Published)
	}
	if len(rep.M9.PerWI) != 2 {
		t.Fatalf("无 flag 应列出全部裸仓：M9 PerWI %d 项，want 2", len(rep.M9.PerWI))
	}
	if rep.Machines[0].Events != 2 {
		t.Fatalf("无 flag 应读入全部事件行：got %d，want 2", rep.Machines[0].Events)
	}
	b, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["window"]; ok {
		t.Fatal("无 flag 的 JSON 输出不得含 window 键")
	}
}

// 用例 2：窗口前创建的 WI 对 M1/M3/M4/M5/M9 零贡献（其 seq 回退、卡死步、
// 已执行 verification、code step 候选、rewind_stats 全部不可见）。
func TestPreWindowWIExcludedFromRepoMetrics(t *testing.T) {
	fx := newRootFixture(t)
	exec1 := mustJSON(fx.t, map[string]any{
		"bundle_name": "b1",
		"steps": map[string]any{
			"code_1": map[string]any{"status": "in_progress", "step_started_at": fxStuckAt},
		},
	})
	exec2 := mustJSON(fx.t, map[string]any{
		"bundle_name": "b1",
		"steps": map[string]any{
			"code_1":       map[string]any{"status": "done"},
			"verification": map[string]any{"status": "done", "output": map[string]any{"commands": []any{map[string]any{"argv": []any{"go", "test", "./..."}, "exit_code": 0, "duration_ms": 1200}}}},
			"stuck_1":      map[string]any{"status": "in_progress", "step_started_at": fxStuckAt},
		},
		"rewind_stats": map[string]any{"total": 2, "by_step": map[string]any{"code_1": 2}},
	})
	// wi_pre：两提交链带 seq 回退（2 → 1），HEAD 有卡死步/已执行 verification/
	// done code step/rewind_stats。
	fx.repo("wi_pre", []fxCommit{
		{Message: "capsule seq 2\n\nPolyforge-Seq: 2\n", Files: map[string]string{
			"capsule.json": fxCapsule("wi_pre", fxPreCre), "runtime-state/execution.json": exec1, "workflows/b1/workflow.md": fxWf,
		}},
		{Message: "capsule seq 1\n\nPolyforge-Seq: 1\n", Files: map[string]string{
			"capsule.json": fxCapsule("wi_pre", fxPreCre), "runtime-state/execution.json": exec2, "workflows/b1/workflow.md": fxWf,
		}},
	})
	// wi_post：窗口内创建，仅一个已执行 verification step（可读证据）。
	fx.repo("wi_post", []fxCommit{{Message: "capsule publish", Files: map[string]string{
		"capsule.json": fxCapsule("wi_post", fxPostCre),
		"runtime-state/execution.json": mustJSON(fx.t, map[string]any{
			"bundle_name": "b1",
			"steps":       map[string]any{"verification": map[string]any{"status": "done", "output": map[string]any{"commands": []any{map[string]any{"argv": []any{"make", "check"}, "exit_code": 0, "duration_ms": 900}}}}},
		}),
		"workflows/b1/workflow.md": fxWf,
	}}})
	root := fx.done()

	all, win := runUnwindowed(t, root), runWindowed(t, root)

	// 夹具先证明有意义：无窗口时 wi_pre 的各项贡献都在。
	if all.M1.Incidents != 1 || all.M1.ReposScanned != 2 || all.M1.CommitsScanned != 3 {
		t.Fatalf("夹具自检失败：M1 = %+v", all.M1)
	}
	if all.M3.StuckSteps != 1 || all.M3.InProgressTotal != 1 {
		t.Fatalf("夹具自检失败：M3 = %+v", all.M3)
	}
	if all.M4.Numerator != 2 || all.M4.Denominator != 2 {
		t.Fatalf("夹具自检失败：M4 = %+v", all.M4)
	}
	if all.M5.Candidates != 1 || all.M5.Evaluated != 1 {
		t.Fatalf("夹具自检失败：M5 = %+v", all.M5)
	}
	if w := m9Find(t, all, "wi_pre"); w.Absent || string(w.RewindStats) == "" {
		t.Fatalf("夹具自检失败：wi_pre 的 rewind_stats 应可见，得到 %+v", w)
	}

	// 窗口报告：1 计入（wi_post），1 排除（创建于边界前）。
	if win.Window.WIsIncluded != 1 || win.Window.WIsExcludedAtOrBefore != 1 || win.Window.WIsNoFaceExcluded != 0 {
		t.Fatalf("window = %+v", win.Window)
	}
	// M1：只扫 wi_post（1 仓 1 提交），seq 回退不可见。
	if win.M1.Incidents != 0 || win.M1.ReposScanned != 1 || win.M1.CommitsScanned != 1 {
		t.Fatalf("窗口化 M1 = %+v", win.M1)
	}
	// M3：卡死步不可见。
	if win.M3.StuckSteps != 0 || win.M3.InProgressTotal != 0 {
		t.Fatalf("窗口化 M3 = %+v", win.M3)
	}
	// M4：只剩 wi_post 的 1/1。
	if win.M4.Numerator != 1 || win.M4.Denominator != 1 {
		t.Fatalf("窗口化 M4 = %+v", win.M4)
	}
	// M5：wi_pre 的 code step 不再评估、候选归零。
	if win.M5.Candidates != 0 || win.M5.Evaluated != 0 {
		t.Fatalf("窗口化 M5 = %+v", win.M5)
	}
	// M9：只列 wi_post。
	if len(win.M9.PerWI) != 1 || win.M9.PerWI[0].WI != "wi_post" {
		t.Fatalf("窗口化 M9 PerWI = %+v", win.M9.PerWI)
	}
	// 否决随窗口翻转：无窗口 PILOT-V1 触发，窗口化未触发。
	if j := findVeto(t, all, "PILOT-V1").Judgment; j != "触发（否决门槛成立）" {
		t.Fatalf("无窗口 PILOT-V1 判定 = %q", j)
	}
	if j := findVeto(t, win, "PILOT-V1").Judgment; j != "未触发" {
		t.Fatalf("窗口化 PILOT-V1 判定 = %q", j)
	}
}

// 用例 3：边界相等（created_at == since）不计入 —— 钉死 D2 的严格 >。
func TestBoundaryEqualExcluded(t *testing.T) {
	fx := newRootFixture(t)
	fx.repo("wi_boundary", []fxCommit{{Message: "capsule publish", Files: map[string]string{
		"capsule.json":                 fxCapsule("wi_boundary", fxSinceStr), // == since，严格 > 之下排除
		"runtime-state/execution.json": mustJSON(fx.t, map[string]any{"steps": map[string]any{}, "rewind_stats": map[string]any{"total": 1}}),
	}}})
	fx.event("2026-10-01T07:00:00Z", "published", "wi_boundary", nil)
	root := fx.done()

	all, win := runUnwindowed(t, root), runWindowed(t, root)
	if len(all.M9.PerWI) != 1 || all.M8.Published != 1 {
		t.Fatalf("夹具自检失败：all.M9 = %+v all.M8 = %+v", all.M9, all.M8)
	}
	if win.Window.WIsIncluded != 0 || win.Window.WIsExcludedAtOrBefore != 1 {
		t.Fatalf("window = %+v", win.Window)
	}
	if len(win.M9.PerWI) != 0 {
		t.Fatalf("边界相等 WI 不得进 M9： %+v", win.M9.PerWI)
	}
	if win.M8.Published != 0 || win.Window.EventsExcluded != 1 {
		t.Fatalf("边界相等 WI 的事件不得计入：M8 = %+v window = %+v", win.M8, win.Window)
	}
}

// 用例 4：无创建面的 WI 如实排除并具名 —— (a) 裸仓 capsule.json 无
// created_at（旧 schema），(b) 事件面 WI 不在 aihub.db；互补面：db 回退
// （事件面 WI + db created_at 在窗口内）则计入。
func TestNoCreationFaceExcludedAndReported(t *testing.T) {
	fx := newRootFixture(t)
	// (a) 旧 schema capsule：无 created_at 字段。
	fx.repo("wi_oldschema", []fxCommit{{Message: "capsule publish", Files: map[string]string{
		"capsule.json":                 fxCapsule("wi_oldschema", ""), // 旧 schema：无 created_at
		"runtime-state/execution.json": mustJSON(fx.t, map[string]any{"steps": map[string]any{}, "rewind_stats": map[string]any{"total": 1}}),
	}}})
	// (b) 事件面 WI：不在 aihub.db，无裸仓。
	fx.event("2026-10-01T08:00:00Z", "published", "wi_eventonly", nil)
	// 互补：事件面 WI，无裸仓，db created_at 窗口内 → db 回退面计入。
	fx.event("2026-10-01T09:00:00Z", "published", "wi_dbface", nil)
	fx.aihub([]fxMachineRow{{ID: "m1", LastHeartbeat: "2026-10-02T11:59:00Z"}},
		[]fxWorkItemRow{{ID: "wi_dbface", CreatedAt: fxPostCre}})
	root := fx.done()

	all, win := runUnwindowed(t, root), runWindowed(t, root)
	if len(all.M9.PerWI) != 1 || all.M8.Published != 2 {
		t.Fatalf("夹具自检失败：all.M9 = %+v all.M8 = %+v", all.M9, all.M8)
	}
	w := win.Window
	if w.WIsNoFaceExcluded != 2 || len(w.WIsNoFaceNames) != 2 || w.WIsNoFaceNames[0] != "wi_eventonly" || w.WIsNoFaceNames[1] != "wi_oldschema" {
		t.Fatalf("无创建面 WI 应排除并具名：window = %+v", w)
	}
	if w.WIsIncluded != 1 || w.EventsIncluded != 1 || w.EventsExcluded != 1 {
		t.Fatalf("db 回退面应计入：window = %+v", w)
	}
	if len(win.M9.PerWI) != 0 {
		t.Fatalf("无创建面裸仓不得进 M9： %+v", win.M9.PerWI)
	}
	if win.M8.Published != 1 {
		t.Fatalf("窗口化 M8.Published = %d，want 1（仅 db 回退面 WI）", win.M8.Published)
	}
}

// 用例 5：被排除 WI 的事件从 M7 滤除 —— 窗口前 WI 的 3 连败段不可见，
// 窗口内 WI 的 3 连败段可见。
func TestEventsOfExcludedWIsFilteredFromM7(t *testing.T) {
	fx := newRootFixture(t)
	simpleRepo(fx, "wi_pre", fxPreCre)
	simpleRepo(fx, "wi_post", fxPostCre)
	for j := 0; j < 3; j++ {
		fx.event(fmt.Sprintf("2026-09-28T09:%02d:00Z", j), "push_failed", "wi_pre", nil)
	}
	for j := 0; j < 3; j++ {
		fx.event(fmt.Sprintf("2026-10-01T09:%02d:00Z", j), "push_failed", "wi_post", nil)
	}
	root := fx.done()

	all, win := runUnwindowed(t, root), runWindowed(t, root)
	if all.M7.Count != 2 || all.M7.MaxRun != 3 || len(all.M7.PerWI) != 2 {
		t.Fatalf("夹具自检失败：all.M7 = %+v", all.M7)
	}
	if win.M7.Count != 1 || win.M7.MaxRun != 3 || len(win.M7.PerWI) != 1 {
		t.Fatalf("窗口化 M7 = %+v", win.M7)
	}
	if s := win.M7.PerWI[0]; !strings.Contains(s, "wi_post") || strings.Contains(s, "wi_pre") {
		t.Fatalf("窗口化 M7 PerWI 应只含 wi_post：%q", s)
	}
}

// 用例 6：被排除 WI 的事件从 M6（write_ms 样本）、M8（published/
// push_failed/wrap_blocked）、M1 push_failed 佐证中滤除。
func TestEventsFilteredFromM6M8M1Corroboration(t *testing.T) {
	fx := newRootFixture(t)
	simpleRepo(fx, "wi_pre", fxPreCre)
	simpleRepo(fx, "wi_post", fxPostCre)
	fx.event("2026-09-25T10:00:00Z", "memory_remember", "wi_pre", map[string]any{"write_ms": 100})
	fx.event("2026-09-25T11:00:00Z", "published", "wi_pre", map[string]any{"commit_ms": 800, "push_ms": 200})
	fx.event("2026-09-25T12:00:00Z", "push_failed", "wi_pre", map[string]any{"error": "remote: pre-receive hook declined"})
	fx.event("2026-09-25T13:00:00Z", "wrap_blocked", "wi_pre", map[string]any{"error": "pre-receive: polyforge-seq violation"})
	fx.event("2026-10-01T08:00:00Z", "memory_remember", "wi_post", map[string]any{"write_ms": 100})
	root := fx.done()

	all, win := runUnwindowed(t, root), runWindowed(t, root)

	// 夹具自检：无窗口时全量可见。
	if all.M6.Samples != 2 || !all.M6.Judged {
		t.Fatalf("夹具自检失败：all.M6 = %+v", all.M6)
	}
	if all.M8.Published != 1 || all.M8.PushFailed != 1 || all.M8.WrapBlocked != 1 || all.M8.FailurePersistent != 2 {
		t.Fatalf("夹具自检失败：all.M8 = %+v", all.M8)
	}
	if all.M1.PushFailedTotal != 1 || all.M1.CorroboratingPushFailed != 1 {
		t.Fatalf("夹具自检失败：all.M1 = %+v", all.M1)
	}
	// 窗口化：wi_pre 的事件全部滤除，只剩 wi_post 的 memory_remember。
	if win.M6.Samples != 1 || win.M6.MemoryEvents != 1 || win.M6.MaxRollingP99 != 100 {
		t.Fatalf("窗口化 M6 = %+v", win.M6)
	}
	if win.M8.Published != 0 || win.M8.PushFailed != 0 || win.M8.WrapBlocked != 0 || win.M8.Denominator != 0 {
		t.Fatalf("窗口化 M8 = %+v", win.M8)
	}
	if win.M1.PushFailedTotal != 0 || win.M1.CorroboratingPushFailed != 0 {
		t.Fatalf("窗口化 M1 佐证 = %+v", win.M1)
	}
}

// 用例 7：daemon 域事件（work_item == ""）保留并计数，不喂 M6/M7/M8。
func TestDaemonScopeEventsRetained(t *testing.T) {
	fx := newRootFixture(t)
	simpleRepo(fx, "wi_pre", fxPreCre)
	simpleRepo(fx, "wi_post", fxPostCre)
	fx.event("2026-10-01T09:00:00Z", "answer_ledger", "", nil)
	fx.event("2026-10-01T09:01:00Z", "drain_degraded", "", nil)
	fx.event("2026-10-01T09:02:00Z", "published", "wi_post", nil)
	fx.event("2026-09-25T09:00:00Z", "published", "wi_pre", nil)
	root := fx.done()

	all, win := runUnwindowed(t, root), runWindowed(t, root)
	w := win.Window
	if w.EventsTotal != 4 || w.EventsIncluded != 1 || w.EventsExcluded != 1 || w.EventsDaemonRetained != 2 {
		t.Fatalf("window = %+v", w)
	}
	// daemon 域事件保留在事件流里但 kind 不喂任何 M6/M7/M8。
	if win.M6.MemoryEvents != 0 || win.M7.Count != 0 || win.M8.Denominator != 1 {
		t.Fatalf("daemon 事件不得喂 M6/M7/M8：M6=%+v M7=%+v M8=%+v", win.M6, win.M7, win.M8)
	}
	if all.M8.Published != 2 {
		t.Fatalf("夹具自检失败：all.M8 = %+v", all.M8)
	}
}

// 用例 8：M3 WI 样本按窗口计数（db 行 Go 侧解析 created_at > since），
// 不可解析行浮出；PILOT-V3 的样本注记描述窗口化样本。
func TestM3WISampleWindowed(t *testing.T) {
	fx := newRootFixture(t)
	simpleRepo(fx, "wi_repo_pre", fxPreCre)
	simpleRepo(fx, "wi_repo_post", fxPostCre)
	fx.aihub([]fxMachineRow{{ID: "m1", LastHeartbeat: "2026-10-02T11:59:00Z"}},
		[]fxWorkItemRow{
			{ID: "wi_repo_pre", CreatedAt: fxPreCre},
			{ID: "wi_repo_post", CreatedAt: fxPostCre},
			{ID: "wi_db_post1", CreatedAt: fxPostCre},
			{ID: "wi_db_post2", CreatedAt: "2026-10-01T07:00:00Z"},
			{ID: "wi_bad_created", CreatedAt: "not-a-timestamp"}, // 不可解析：计数浮出，绝不静默丢弃
		})
	root := fx.done()

	all, win := runUnwindowed(t, root), runWindowed(t, root)
	if all.M3.WISample != 5 {
		t.Fatalf("无窗口 M3 WI 样本 = %d，want 5（db 全量）", all.M3.WISample)
	}
	if win.M3.WISample != 3 {
		t.Fatalf("窗口化 M3 WI 样本 = %d，want 3（窗口内可解析行）", win.M3.WISample)
	}
	if !win.Window.M3WISampleWindowed {
		t.Fatal("m3_wi_sample_windowed 应为 true")
	}
	if win.Window.M3SampleUnparseable != 1 {
		t.Fatalf("不可解析 created_at 行应浮出为 1，得到 %d", win.Window.M3SampleUnparseable)
	}
	if n := findVeto(t, win, "PILOT-V3").Note; !strings.Contains(n, "样本 3 张 WI") {
		t.Fatalf("PILOT-V3 样本注记应描述窗口化样本：%q", n)
	}
}

// 用例 9：M2 与 WI 创建无关 —— 开窗后仍为 0/0、注记不变；窗口 note 带
// 创建无关子句；PILOT-V2 不受影响。
func TestM2UnchangedUnderWindow(t *testing.T) {
	fx := newRootFixture(t)
	simpleRepo(fx, "wi_pre", fxPreCre)
	simpleRepo(fx, "wi_post", fxPostCre)
	root := fx.done()

	all, win := runUnwindowed(t, root), runWindowed(t, root)
	if win.M2 != all.M2 {
		t.Fatalf("M2 读数不得随窗口变化：win = %+v all = %+v", win.M2, all.M2)
	}
	if win.M2.Numerator != 0 || win.M2.Denominator != 0 {
		t.Fatalf("M2 应为 0/0 机械真读：%+v", win.M2)
	}
	if !strings.Contains(win.M2.Note, "无删除面") {
		t.Fatalf("M2 注记缺失：%q", win.M2.Note)
	}
	if !strings.Contains(win.Window.Note, "M2 与 WI 创建无关，不受窗口影响") {
		t.Fatalf("窗口 note 应带 M2 子句：%q", win.Window.Note)
	}
	if j := findVeto(t, win, "PILOT-V2").Judgment; j != "未触发" {
		t.Fatalf("窗口化 PILOT-V2 判定 = %q", j)
	}
}

// 用例 10：--json 下 window 对象的键/计数/边界文本。
func TestJSONWindowFields(t *testing.T) {
	fx := newRootFixture(t)
	simpleRepo(fx, "wi_pre", fxPreCre)
	simpleRepo(fx, "wi_post", fxPostCre)
	fx.event("2026-09-25T10:00:00Z", "published", "wi_pre", nil)       // 排除
	fx.event("2026-10-01T10:00:00Z", "published", "wi_post", nil)      // 计入
	fx.event("2026-10-01T11:00:00Z", "published", "wi_eventonly", nil) // 无创建面排除
	fx.event("2026-10-01T12:00:00Z", "answer_ledger", "", nil)         // daemon 域保留
	root := fx.done()

	win := runWindowed(t, root)
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf) // 与 run() 的 JSON 输出同构
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	if err := enc.Encode(win); err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	w, ok := m["window"].(map[string]any)
	if !ok {
		t.Fatalf("JSON 顶层缺 window 对象：%s", buf.String())
	}
	if w["since"] != fxSinceStr {
		t.Fatalf("window.since = %v，want %q", w["since"], fxSinceStr)
	}
	if w["boundary"] != windowBoundaryRule {
		t.Fatalf("window.boundary = %v", w["boundary"])
	}
	for k, want := range map[string]float64{
		"wis_included":                      1,
		"wis_excluded_created_before_or_at": 1,
		"wis_no_creation_face_excluded":     1,
		"events_total":                      4,
		"events_included":                   1,
		"events_excluded":                   2,
		"events_daemon_scope_retained":      1,
	} {
		if w[k] != want {
			t.Fatalf("window.%s = %v，want %v", k, w[k], want)
		}
	}
	if names, _ := w["wis_no_creation_face_names"].([]any); len(names) != 1 || names[0] != "wi_eventonly" {
		t.Fatalf("window.wis_no_creation_face_names = %v", w["wis_no_creation_face_names"])
	}
	if w["m3_wi_sample_windowed"] != true {
		t.Fatalf("window.m3_wi_sample_windowed = %v", w["m3_wi_sample_windowed"])
	}
	if _, ok := w["m3_wi_sample_created_at_unparseable"]; ok {
		t.Fatal("全量可解析时不得出现 m3_wi_sample_created_at_unparseable 键")
	}
	if n, _ := w["note"].(string); !strings.Contains(n, "M2 与 WI 创建无关") {
		t.Fatalf("window.note = %q", n)
	}
}

// 用例 11：无效时间戳拒绝。main() 的校验内联为 time.Parse(time.RFC3339, …)
// （D5），先在语义层钉死 RFC3339 拒绝面，再构建一次二进制钉死 CLI 的
// exit-2 行为与错误行。
func TestInvalidWindowSinceRejected(t *testing.T) {
	bad := []string{"garbage", "2026-10-01 00:00:00", "2026-10-01"} // 垃圾/非 RFC3339/缺时区
	for _, s := range bad {
		if _, err := time.Parse(time.RFC3339, s); err == nil {
			t.Fatalf("%q 应被 RFC3339 校验拒绝", s)
		}
	}
	fx := newRootFixture(t)
	simpleRepo(fx, "wi_post", fxPostCre)
	root := fx.done()

	bin := filepath.Join(t.TempDir(), "gate-metrics")
	build := exec.Command("go", "build", "-o", bin, ".") // 测试进程 cwd = tools/gate-metrics
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v: %s", err, out)
	}
	for _, s := range bad {
		cmd := exec.Command(bin, "--window-since", s, root)
		out, err := cmd.CombinedOutput()
		if err == nil {
			t.Fatalf("--window-since %q 应 exit 2，得到 0（输出 %s）", s, out)
		}
		ee, ok := err.(*exec.ExitError)
		if !ok || ee.ExitCode() != 2 {
			t.Fatalf("--window-since %q 应 exit 2，得到 %v", s, err)
		}
		if !strings.Contains(string(out), "gate-metrics: --window-since: 无效时间戳") || !strings.Contains(string(out), fmt.Sprintf("%q", s)) {
			t.Fatalf("--window-since %q 的错误行不符：%s", s, out)
		}
	}
	// 对照：合法时间戳 exit 0，且人读表带窗口块。
	out, err := exec.Command(bin, "--window-since", fxSinceStr, root).CombinedOutput()
	if err != nil {
		t.Fatalf("合法 --window-since 应 exit 0：%v（输出 %s）", err, out)
	}
	if !strings.Contains(string(out), "窗口 --window-since "+fxSinceStr) {
		t.Fatalf("人读表应含窗口块：%s", out)
	}
}

// 用例 12：否决在过滤后的事件上计算 —— 窗口前 4 段 3 连败（无窗口时
// M7=4 触发 PILOT-V7）不随窗口触发；窗口内 1 段不触发。
func TestVetoesOverWindow(t *testing.T) {
	fx := newRootFixture(t)
	var rows []fxWorkItemRow
	for i := 0; i < 4; i++ {
		wi := fmt.Sprintf("wi_veto_pre_%02d", i)
		for j := 0; j < 3; j++ {
			fx.event(fmt.Sprintf("2026-09-28T09:%02d:00Z", i*10+j), "push_failed", wi, nil)
		}
		rows = append(rows, fxWorkItemRow{ID: wi, CreatedAt: fxPreCre}) // db 回退面：窗口前
	}
	for j := 0; j < 3; j++ {
		fx.event(fmt.Sprintf("2026-10-01T09:%02d:00Z", j), "push_failed", "wi_veto_post", nil)
	}
	rows = append(rows, fxWorkItemRow{ID: "wi_veto_post", CreatedAt: fxPostCre})
	fx.aihub([]fxMachineRow{{ID: "m1", LastHeartbeat: "2026-10-02T11:59:00Z"}}, rows)
	root := fx.done()

	all, win := runUnwindowed(t, root), runWindowed(t, root)
	if all.M7.Count != 5 {
		t.Fatalf("夹具自检失败：all.M7.Count = %d，want 5", all.M7.Count)
	}
	if j := findVeto(t, all, "PILOT-V7").Judgment; j != "触发（否决门槛成立）" {
		t.Fatalf("无窗口 PILOT-V7 判定 = %q", j)
	}
	if win.M7.Count != 1 || win.M7.MaxRun != 3 {
		t.Fatalf("窗口化 M7 = %+v", win.M7)
	}
	if j := findVeto(t, win, "PILOT-V7").Judgment; j != "未触发" {
		t.Fatalf("窗口化 PILOT-V7 判定 = %q", j)
	}
	if win.Window.WIsExcludedAtOrBefore != 4 || win.Window.WIsIncluded != 1 {
		t.Fatalf("window = %+v", win.Window)
	}
}
