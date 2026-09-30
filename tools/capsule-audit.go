// Command capsule-audit prints a read-only volume audit of a capsule bare
// git repository: for every commit in the HEAD revision chain (oldest
// first) it reports the transcript byte share, the transcript and
// whole-tree growth versus the parent commit, and the rate at which the
// commit's new blobs are rewrites of blobs that already exist in earlier
// history — the signature of transcripts being fully re-copied every step.
//
// Transcript paths are session/** and logs/session/** (per internal
// capsule conventions). A summary line after the table reports the
// chain-wide averages.
//
// Usage:
//
//	capsule-audit <bare-repo-path>
//
// The tool never writes to the target repo: only read-only git plumbing
// (rev-parse, rev-list, ls-tree) is invoked, with optional locks disabled.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: capsule-audit <bare-repo-path>")
		fmt.Fprintln(os.Stderr, "prints a per-commit transcript-share/growth/rewrite-rate table for the repo's HEAD chain")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintf(os.Stderr, "capsule-audit: %v\n", err)
		os.Exit(1)
	}
}

// treeStats is the per-commit full-tree measurement (git ls-tree -r -l).
type treeStats struct {
	blobOIDs   map[string]bool // all blob OIDs in the commit's tree
	treeBytes  int64           // sum of all blob sizes
	transBytes int64           // sum of blob sizes under session/** or logs/session/**
}

// row is one table row: one commit of the chain.
type row struct {
	sha      string
	step     int     // 1-based ordinal, oldest first
	share    float64 // transcript bytes / tree bytes, percent
	transInc int64   // transcript bytes minus parent's (first commit: full size)
	treeInc  int64   // tree bytes minus parent's (first commit: full size)
	rate     float64 // new blobs already in earlier history / new blobs, percent
}

func run(repoPath string) error {
	if fi, err := os.Stat(repoPath); err != nil {
		return err
	} else if !fi.IsDir() {
		return fmt.Errorf("%s: not a directory", repoPath)
	}
	if _, err := git(repoPath, "rev-parse", "--verify", "HEAD^{}"); err != nil {
		return fmt.Errorf("resolving HEAD (empty or unborn repo?): %w", err)
	}
	out, err := git(repoPath, "rev-list", "--reverse", "--topo-order", "HEAD")
	if err != nil {
		return err
	}
	chain := strings.Fields(out)
	if len(chain) == 0 {
		return fmt.Errorf("HEAD resolves to no commits")
	}

	trees := make([]treeStats, len(chain))
	for i, sha := range chain {
		ts, err := readTree(repoPath, sha)
		if err != nil {
			return err
		}
		trees[i] = ts
	}

	// Walk oldest-first. The chain is assumed linear (per the memo):
	// the "parent" of commit i is commit i-1, and non-chain parents are
	// ignored. historyBlobs accumulates every blob OID seen in commits
	// 1..i-1, i.e. all of commit i's earlier history.
	historyBlobs := make(map[string]bool)
	rows := make([]row, len(chain))
	for i, sha := range chain {
		ts := trees[i]
		r := row{sha: sha, step: i + 1}
		if ts.treeBytes > 0 {
			r.share = float64(ts.transBytes) / float64(ts.treeBytes) * 100
		}
		if i == 0 {
			// First commit: no parent and no earlier history — its
			// increments are its full size, rewrite rate 0.
			r.transInc = ts.transBytes
			r.treeInc = ts.treeBytes
		} else {
			parent := trees[i-1]
			r.transInc = ts.transBytes - parent.transBytes
			r.treeInc = ts.treeBytes - parent.treeBytes
			var newBlobs, rewritten int
			for oid := range ts.blobOIDs {
				if parent.blobOIDs[oid] {
					continue // carried over from the parent: not new
				}
				newBlobs++
				if historyBlobs[oid] {
					rewritten++ // already somewhere in earlier history
				}
			}
			if newBlobs > 0 {
				r.rate = float64(rewritten) / float64(newBlobs) * 100
			}
		}
		rows[i] = r
		for oid := range ts.blobOIDs {
			historyBlobs[oid] = true
		}
	}

	printTable(rows)
	return nil
}

// readTree measures one commit's full tree via ls-tree -r -l (sizes inline,
// no cat-file needed). Only blob entries are counted; non-blob entries
// (submodules) carry a "-" size and are skipped.
func readTree(repoPath, commit string) (treeStats, error) {
	out, err := git(repoPath, "ls-tree", "-r", "-l", commit)
	if err != nil {
		return treeStats{}, err
	}
	ts := treeStats{blobOIDs: make(map[string]bool)}
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		tab := strings.IndexByte(line, '\t')
		if tab < 0 {
			return treeStats{}, fmt.Errorf("unparsable ls-tree line for %s: %q", commit, line)
		}
		path := line[tab+1:]
		fields := strings.Fields(line[:tab])
		if len(fields) != 4 || fields[1] != "blob" {
			continue
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			continue
		}
		ts.blobOIDs[fields[2]] = true
		ts.treeBytes += size
		if strings.HasPrefix(path, "session/") || strings.HasPrefix(path, "logs/session/") {
			ts.transBytes += size
		}
	}
	return ts, nil
}

func printTable(rows []row) {
	fmt.Println("sha\t步数\ttranscript 占比\ttranscript 增量\t全树增量\t重写率")
	var sumShare, sumTransInc, sumTreeInc, sumRate float64
	for _, r := range rows {
		fmt.Printf("%s\t%d\t%.2f%%\t%d\t%d\t%.2f%%\n",
			r.sha, r.step, r.share, r.transInc, r.treeInc, r.rate)
		sumShare += r.share
		sumTransInc += float64(r.transInc)
		sumTreeInc += float64(r.treeInc)
		sumRate += r.rate
	}
	n := float64(len(rows))
	fmt.Printf("平均 (%.0f commits)\ttranscript 占比 %.2f%%\ttranscript 增量 %.0f\t全树增量 %.0f\t重写率 %.2f%%\n",
		n, sumShare/n, sumTransInc/n, sumTreeInc/n, sumRate/n)
}

// git runs a read-only git plumbing command against repoPath and returns
// its stdout. Optional locks are disabled so git cannot even momentarily
// write lock files in the target repo.
func git(repoPath string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repoPath}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	var stdout, stderr bytes.Buffer
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
