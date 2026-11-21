package runtime

import (
	"encoding/json"
	"fmt"
	"github.com/silgrid/polyforge/internal/common/capsule"
	"github.com/silgrid/polyforge/internal/polyforge/workspace"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const cacheMaxBytes int64 = 512 << 20

type cacheIdentity struct {
	Project string            `json:"project"`
	Repos   map[string]string `json:"repos"`
}

func warmCapsuleCache(capsuleDir, project string, baselines map[string]string) {
	for name, base := range baselines {
		repo := filepath.Join(capsuleDir, capsule.DirRepos, name)
		cfg, err := workspace.LoadCacheConfig(repo)
		if err != nil {
			cacheLog(capsuleDir, "cache config ignored for "+name+": "+err.Error())
			continue
		}
		if cfg == nil {
			continue
		}
		if base == "" {
			cacheLog(capsuleDir, "cache skipped for "+name+": repository has no baseline")
			continue
		}
		src := findWarmSource(capsuleDir, project, name, base)
		if src == "" {
			cacheLog(capsuleDir, "cache skipped for "+name+": no warmed capsule with matching baseline")
			if cfg.Install != "" {
				runCacheInstall(repo, cfg.Install, capsuleDir)
			}
			continue
		}
		ok := true
		if cfg.NodeModules {
			ok = hardlinkTree(filepath.Join(src, capsule.DirRepos, name, "node_modules"), filepath.Join(repo, "node_modules"))
		}
		if ok {
			for _, pat := range cfg.BuildPaths {
				if !copyBuildPath(filepath.Join(src, capsule.DirRepos, name), repo, pat) {
					ok = false
					cacheLog(capsuleDir, "cache path unavailable or over limit for "+name+": "+pat)
				}
			}
		}
		if !ok {
			_ = os.RemoveAll(filepath.Join(repo, "node_modules"))
			for _, pat := range cfg.BuildPaths {
				matches, _ := filepath.Glob(filepath.Join(repo, filepath.FromSlash(pat)))
				for _, match := range matches {
					_ = os.RemoveAll(match)
				}
			}
			if cfg.Install != "" {
				runCacheInstall(repo, cfg.Install, capsuleDir)
			}
		} else {
			_ = os.WriteFile(filepath.Join(repo, ".polyforge-warmed"), []byte("warmed\n"), 0644)
			cacheLog(capsuleDir, "cache warmed for "+name)
		}
	}
}
func cacheLog(root, msg string) {
	p := filepath.Join(root, "logs", "cache.log")
	if e := os.MkdirAll(filepath.Dir(p), 0755); e != nil {
		return
	}
	f, e := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if e == nil {
		defer f.Close()
		fmt.Fprintln(f, msg)
	}
}
func findWarmSource(cur, project, name, base string) string {
	ents, _ := os.ReadDir(filepath.Dir(cur))
	for _, e := range ents {
		if !e.IsDir() || filepath.Join(filepath.Dir(cur), e.Name()) == cur {
			continue
		}
		d := filepath.Join(filepath.Dir(cur), e.Name())
		var id cacheIdentity
		raw, e2 := os.ReadFile(filepath.Join(d, capsule.CapsuleJSON))
		if e2 != nil || json.Unmarshal(raw, &id) != nil || id.Project != project {
			continue
		}
		if _, e2 = os.Stat(filepath.Join(d, capsule.DirRepos, name, ".polyforge-warmed")); e2 == nil {
			return d
		}
	}
	return ""
}
func hardlinkTree(src, dst string) bool {
	st, e := os.Stat(src)
	if e != nil || !st.IsDir() {
		return false
	}
	if e = os.MkdirAll(dst, 0755); e != nil {
		return false
	}
	ok := true
	walkErr := filepath.Walk(src, func(p string, i os.FileInfo, e error) error {
		if e != nil || !ok {
			return e
		}
		rel, _ := filepath.Rel(src, p)
		if rel == "." {
			return nil
		}
		q := filepath.Join(dst, rel)
		if i.IsDir() {
			return os.MkdirAll(q, 0755)
		}
		if i.Size() > cacheMaxBytes {
			ok = false
			return nil
		}
		if e = os.Link(p, q); e != nil {
			ok = false
		}
		return nil
	})
	if !ok || walkErr != nil {
		_ = os.RemoveAll(dst)
		return false
	}
	return true
}
func copyBuildPath(src, dst, pat string) bool {
	matches, e := filepath.Glob(filepath.Join(src, filepath.FromSlash(pat)))
	if e != nil || len(matches) == 0 {
		return false
	}
	for _, p := range matches {
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if !copyTree(p, target) {
			_ = os.RemoveAll(target)
			return false
		}
	}
	return true
}
func copyTree(src, dst string) bool {
	var total int64
	return filepath.Walk(src, func(p string, i os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		rel, _ := filepath.Rel(src, p)
		q := dst
		if rel != "." {
			q = filepath.Join(dst, rel)
		}
		if i.IsDir() {
			return os.MkdirAll(q, 0755)
		}
		total += i.Size()
		if total > cacheMaxBytes {
			return fmt.Errorf("cache limit")
		}
		in, e := os.Open(p)
		if e != nil {
			return e
		}
		defer in.Close()
		out, e := os.Create(q)
		if e != nil {
			return e
		}
		_, e = io.Copy(out, in)
		ce := out.Close()
		if e == nil {
			e = ce
		}
		return e
	}) == nil
}
func runCacheInstall(repo, cmdline, root string) {
	_ = os.RemoveAll(filepath.Join(repo, "node_modules"))
	cmd := exec.Command("sh", "-c", cmdline)
	cmd.Dir = repo
	out, e := cmd.CombinedOutput()
	if e != nil {
		cacheLog(root, "cache install failed: "+strings.TrimSpace(string(out)))
	} else {
		_ = os.WriteFile(filepath.Join(repo, ".polyforge-warmed"), []byte("warmed\n"), 0644)
	}
}
