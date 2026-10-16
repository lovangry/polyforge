package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/silgrid/polyforge/internal/polyforge/workspace"
)

// doctor_workspace.go: the workspace runtime-artifact checks of `pf
// doctor` (polyforge#177; the convention is workspace/layout.go's).
// Both run per mapped workspace that exists (a missing one is the
// "workspace dirs" check's fail) and never fail — warn at most:
//
//   - "workspace layout": root entries outside the convention and
//     non-generated entries under .polyforge/, each named with its
//     suggested destination. Read-only: nothing is moved or deleted.
//   - "workspace scratch": scratch/ entries whose newest change (mtime or ctime) is
//     older than 14 days. By default they are only REPORTED (warn);
//     `pf doctor --prune-scratch` removes them and lists what it removed.
//     Report-by-default keeps doctor a read-only probe anyone may run at
//     any time without side effects; deletion is an explicit operator
//     act.

const (
	docChLayout  = "workspace layout"
	docChScratch = "workspace scratch"
)

// docRowCap bounds the rows one check's detail lists.
const docRowCap = 20

// existingWorkspaces answers the mapped workspaces that exist as
// directories.
func existingWorkspaces(wss []string) []string {
	var out []string
	for _, ws := range wss {
		if st, err := os.Stat(ws); err == nil && st.IsDir() {
			out = append(out, ws)
		}
	}
	return out
}

func capRows(rows []string) []string {
	if len(rows) > docRowCap {
		return append(rows[:docRowCap:docRowCap], fmt.Sprintf("… %d more", len(rows)-docRowCap))
	}
	return rows
}

// workspaceLayoutCheck warns about entries outside the convention.
func workspaceLayoutCheck(wss []string) doctorCheck {
	c := doctorCheck{Name: docChLayout}
	var rows, errs []string
	for _, ws := range existingWorkspaces(wss) {
		strays, err := workspace.StrayEntries(ws)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", ws, err))
			continue
		}
		for _, s := range strays {
			rows = append(rows, fmt.Sprintf("%s → %s", filepath.Join(ws, s.Path), s.Dest))
		}
	}
	switch {
	case len(rows) > 0 || len(errs) > 0:
		c.Status = "warn"
		var parts []string
		if len(rows) > 0 {
			parts = append(parts, fmt.Sprintf("%d entr(ies) outside the workspace layout: %s — share documents through aihub (`pf file edit <wi> --content @f` while the work item is open, else `pf remember --project <p> --type doc.design|doc.review|doc.decision|doc.audit [--work-item <wi>] @f`), move disposable output to scratch/; nothing was moved or deleted",
				len(rows), strings.Join(capRows(rows), "; ")))
		}
		if len(errs) > 0 {
			parts = append(parts, "unreadable: "+strings.Join(errs, "; "))
		}
		c.Detail = strings.Join(parts, "; ")
	default:
		c.Status = "pass"
		c.Detail = fmt.Sprintf("every mapped workspace keeps the layout (root: .repo/, capsules/, scratch/, .polyforge/, CLAUDE.md, CLAUDE.local.md, AGENTS.md, go.work, go.work.sum, %s and dot-entries other than %s backups; .polyforge/: the legacy v1 repo-map/, watchers/)", workspace.FileName, workspace.FileName)
	}
	return c
}

// scratchCheck reports — or, with prune, removes — the stale scratch
// entries of every mapped workspace.
func scratchCheck(wss []string, now time.Time, prune bool) doctorCheck {
	c := doctorCheck{Name: docChScratch, Status: "pass"}
	var stale, pruned, errs []string
	for _, ws := range existingWorkspaces(wss) {
		scan := workspace.StaleScratch
		if prune {
			scan = workspace.PruneScratch
		}
		rep, err := scan(ws, now, workspace.ScratchMaxAge)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", ws, err))
			continue
		}
		for _, n := range rep.Stale {
			p := filepath.Join(ws, workspace.ScratchDir, n)
			if prune {
				pruned = append(pruned, p)
			} else {
				stale = append(stale, p)
			}
		}
		for _, sk := range rep.Skipped {
			errs = append(errs, filepath.Join(ws, workspace.ScratchDir, sk))
		}
	}
	var parts []string
	switch {
	case len(stale) > 0:
		c.Status = "warn"
		parts = append(parts, fmt.Sprintf("%d scratch entr(ies) older than 14 days: %s — `pf doctor --prune-scratch` removes them", len(stale), strings.Join(capRows(stale), "; ")))
	case len(pruned) > 0:
		parts = append(parts, fmt.Sprintf("pruned %d scratch entr(ies) older than 14 days: %s", len(pruned), strings.Join(pruned, "; ")))
	case prune:
		parts = append(parts, "no scratch entry older than 14 days: nothing pruned")
	default:
		parts = append(parts, "no scratch entry older than 14 days")
	}
	if len(errs) > 0 {
		c.Status = "warn"
		parts = append(parts, "not checked: "+strings.Join(errs, "; "))
	}
	c.Detail = strings.Join(parts, "; ")
	return c
}
