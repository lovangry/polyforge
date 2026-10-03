package main

// root_fixture_test.go: 合成 pilot 数据根的测试夹具（仅 t.TempDir()，无网络，
// 不依赖真实 pilot 数据根，无 time.Now() 依赖——now 由调用方显式传入）。
// 形态遵循 tools/v0export/fixture.go 的 DDL/insert 模式：
//   - <root>/m1/events.log      机器事件日志（JSONL：at/kind/work_item/body）
//   - <root>/m1/git/<wi>.git     逐 WI 裸仓，只读兼容的 git plumbing 构建
//                                （hash-object/mktree/commit-tree/update-ref），
//                                HEAD 携带受控的 capsule.json（created_at）与
//                                runtime-state/execution.json
//   - <root>/aihub-data/aihub.db modernc.org/sqlite 建库（machines + work_items，
//                                边界两侧各若干行）

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

// fxCommit is one commit of a synthetic bare repo: a full file snapshot plus
// the commit message (M1 seq trailers live in the message).
type fxCommit struct {
	Message string
	Files   map[string]string
}

// fxMachineRow / fxWorkItemRow: aihub.db rows (the faces readAihub loads).
type fxMachineRow struct {
	ID            string
	LastHeartbeat string
}

type fxWorkItemRow struct {
	ID             string
	CreatedAt      string
	SessionMachine string // "" 插入为 NULL
}

// rootFixture incrementally builds one synthetic pilot data root.
type rootFixture struct {
	t      *testing.T
	root   string
	events []string // machine m1 的 events.log JSONL 行
}

func newRootFixture(t *testing.T) *rootFixture {
	fx := &rootFixture{t: t, root: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(fx.root, "m1", "git"), 0o750); err != nil {
		t.Fatal(err)
	}
	return fx
}

// gitRun runs a git command (stdin 可选) and returns stdout; dir == "" 表示
// 不加 -C（仅 git init 用）。身份走显式 env，绝不依赖宿主 git config。
func (fx *rootFixture) gitRun(dir, stdin string, args ...string) string {
	fx.t.Helper()
	var cmd *exec.Cmd
	if dir == "" {
		cmd = exec.Command("git", args...)
	} else {
		cmd = exec.Command("git", append([]string{"-C", dir}, args...)...)
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		fx.t.Fatalf("git %v: %v: %s", args, err, errb.String())
	}
	return out.String()
}

// event appends one JSONL line to machine m1's events.log. body == nil 写成
// 空 body（真实 events.log 的 body 恒为对象）。
func (fx *rootFixture) event(at, kind, wi string, body map[string]any) {
	fx.t.Helper()
	if body == nil {
		body = map[string]any{}
	}
	b, err := json.Marshal(map[string]any{"at": at, "kind": kind, "work_item": wi, "body": body})
	if err != nil {
		fx.t.Fatal(err)
	}
	fx.events = append(fx.events, string(b))
}

// repo builds the per-WI bare repo <root>/m1/git/<wi>.git via read-only-
// compatible git plumbing. commits 是整树快照，从旧到新逐个 commit-tree
// 串成 first-parent 链，HEAD（symbolic-ref 所指的分支）指向最后一个。
func (fx *rootFixture) repo(wi string, commits []fxCommit) {
	fx.t.Helper()
	if len(commits) == 0 {
		fx.t.Fatal("fixture repo needs at least one commit")
	}
	dir := filepath.Join(fx.root, "m1", "git", wi+".git")
	fx.gitRun("", "", "init", "--bare", dir)
	var parent string
	for _, c := range commits {
		tree := fx.buildTreeAt(dir, c.Files, "")
		args := []string{"commit-tree", tree}
		if c.Message != "" {
			args = append(args, "-m", c.Message)
		}
		if parent != "" {
			args = append(args, "-p", parent)
		}
		parent = strings.TrimSpace(fx.gitRun(dir, "", args...))
	}
	head := strings.TrimSpace(fx.gitRun(dir, "", "symbolic-ref", "HEAD"))
	fx.gitRun(dir, "", "update-ref", head, parent)
}

// buildTreeAt recursively writes blobs (hash-object -w --stdin) and builds
// nested trees (mktree) for the files under prefix, returning the tree sha.
// 每一层只消费位于 prefix 之下的路径：不在 prefix 下的键必须跳过，否则其
// 仍含 "/" 的 rel 会在子层被再次当作目录名，同一目录无限重现；跳过后的
// 剩余部分用切片（rel := p[len(prefix):]）取出，杜绝 TrimPrefix 对非前缀
// 路径原样返回的坑。基例：names 全为 blob（无子目录）时不再递归。
func (fx *rootFixture) buildTreeAt(repo string, files map[string]string, prefix string) string {
	fx.t.Helper()
	isDir := map[string]bool{}
	var names []string
	for p := range files {
		if !strings.HasPrefix(p, prefix) {
			continue // 不在当前 prefix 之下的路径不属于本层
		}
		rel := p[len(prefix):]
		name := rel
		if i := strings.Index(rel, "/"); i >= 0 {
			// 注意：不能写成 `name, isDir[name] = rel[:i], true`——赋值第一阶段
			// 就用旧 name 求下标，会把整路径当成键。
			name = rel[:i]
			isDir[name] = true
		}
		if !contains(names, name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var lines []string
	for _, name := range names {
		if isDir[name] {
			sub := fx.buildTreeAt(repo, files, prefix+name+"/")
			lines = append(lines, fmt.Sprintf("040000 tree %s\t%s", sub, name))
		} else {
			sha := strings.TrimSpace(fx.gitRun(repo, files[prefix+name], "hash-object", "-w", "--stdin"))
			lines = append(lines, fmt.Sprintf("100644 blob %s\t%s", sha, name))
		}
	}
	return strings.TrimSpace(fx.gitRun(repo, strings.Join(lines, "\n")+"\n", "mktree"))
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// fxAihubDDL is the aihub.db subset readAihub queries (v0export 模式：const
// DDL + 参数化 insert；建库仅测试用，工具侧仍只读打开).
const fxAihubDDL = `
CREATE TABLE machines (
  id                TEXT PRIMARY KEY,
  last_heartbeat_at TEXT NOT NULL
);
CREATE TABLE work_items (
  id              TEXT PRIMARY KEY,
  created_at      TEXT NOT NULL,
  session_machine TEXT
);
`

// aihub writes <root>/aihub-data/aihub.db with the given machines and
// work_items rows (created_at / session_machine 落在边界两侧由调用方控制).
func (fx *rootFixture) aihub(machines []fxMachineRow, wis []fxWorkItemRow) {
	fx.t.Helper()
	path := filepath.Join(fx.root, "aihub-data", "aihub.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		fx.t.Fatal(err)
	}
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		fx.t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(fxAihubDDL); err != nil {
		fx.t.Fatal(err)
	}
	for _, m := range machines {
		if _, err := db.Exec(`INSERT INTO machines(id, last_heartbeat_at) VALUES (?1,?2)`, m.ID, m.LastHeartbeat); err != nil {
			fx.t.Fatal(err)
		}
	}
	for _, w := range wis {
		var sm any // "" → NULL（session_machine is not null 才是活跃持有者）
		if w.SessionMachine != "" {
			sm = w.SessionMachine
		}
		if _, err := db.Exec(`INSERT INTO work_items(id, created_at, session_machine) VALUES (?1,?2,?3)`, w.ID, w.CreatedAt, sm); err != nil {
			fx.t.Fatal(err)
		}
	}
}

// done writes machine m1's events.log（恒存在，可为 0 行）并返回 fixture 根。
func (fx *rootFixture) done() string {
	fx.t.Helper()
	content := ""
	if len(fx.events) > 0 {
		content = strings.Join(fx.events, "\n") + "\n"
	}
	if err := os.WriteFile(filepath.Join(fx.root, "m1", "events.log"), []byte(content), 0o640); err != nil {
		fx.t.Fatal(err)
	}
	return fx.root
}

// mustJSON marshals deterministically and fails the test on error.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// fxCapsule builds a capsule.json body carrying the D1.1 primary creation
// face. created_at == "" 生成无 created_at 的旧 schema capsule（no-face 用例）。
func fxCapsule(wi, createdAt string) string {
	doc := map[string]any{"id": wi}
	if createdAt != "" {
		doc["created_at"] = createdAt
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err) // map[string]string 序列化不会失败；防御性保留
	}
	return string(b)
}
