package workspace

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// CacheConfig is the optional per-repository capsule warm-cache declaration.
type CacheConfig struct {
	BuildPaths  []string
	NodeModules bool
	Install     string
}

func CacheConfigPath(repoRoot string) string {
	return repoRoot + string(os.PathSeparator) + ".polyforge/capsule-cache.conf"
}

// LoadCacheConfig returns nil,nil when the declaration is absent. It deliberately
// performs no filesystem work beyond the requested declaration path.
func LoadCacheConfig(repoRoot string) (*CacheConfig, error) {
	f, err := os.Open(CacheConfigPath(repoRoot))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	c := &CacheConfig{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		p := strings.SplitN(line, "=", 2)
		if len(p) != 2 {
			return nil, &Error{Path: CacheConfigPath(repoRoot), Reason: "cache declaration must use key = value"}
		}
		k, v := strings.TrimSpace(p[0]), strings.TrimSpace(p[1])
		switch k {
		case "build_paths":
			for _, x := range strings.Split(v, ",") {
				x = strings.TrimSpace(x)
				if x != "" {
					if filepath.IsAbs(x) || filepath.Clean(x) == ".." || strings.HasPrefix(filepath.Clean(x), ".."+string(os.PathSeparator)) {
						return nil, &Error{Path: CacheConfigPath(repoRoot), Reason: "build_paths must stay within repository"}
					}
					c.BuildPaths = append(c.BuildPaths, x)
				}
			}
		case "node_modules":
			c.NodeModules = v == "true"
			if v != "true" && v != "false" {
				return nil, &Error{Path: CacheConfigPath(repoRoot), Reason: "node_modules must be true or false"}
			}
		case "install":
			c.Install = v
		default:
			return nil, &Error{Path: CacheConfigPath(repoRoot), Reason: "unknown cache key " + k}
		}
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return c, nil
}
