package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// rawConfig mirrors Config's top-level shape but keeps include as a
// first-class field so we can reject it in included files, and turns
// proxy/auth into pointers so we can distinguish "absent" from "present
// but empty" during merge.
type rawConfig struct {
	Include []string       `yaml:"include"`
	Version int            `yaml:"version"`
	Proxy   *ProxyConfig   `yaml:"proxy"`
	Auth    *AuthConfig    `yaml:"auth"`
	Servers []ServerConfig `yaml:"servers"`
}

// loadWithIncludes reads path, resolves include patterns relative to
// path's directory, and merges everything into a single rawConfig.
//
// Merge rules:
//   - proxy and auth may appear only in the top-level file (errors).
//   - servers merge by name; the last definition wins, with a warning.
//   - include is not allowed in included files (no cycles).
//   - paths are relative to the directory of the top-level file.
func loadWithIncludes(path string) (*rawConfig, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var root rawConfig
	if err := yaml.Unmarshal(data, &root); err != nil {
		return nil, nil, fmt.Errorf("parse config %q: %w", path, err)
	}

	var warnings []string
	merged := &rawConfig{
		Version: root.Version,
		Proxy:   root.Proxy,
		Auth:    root.Auth,
	}

	dir := filepath.Dir(path)
	for _, pat := range root.Include {
		if !filepath.IsAbs(pat) {
			pat = filepath.Join(dir, pat)
		}
		matches, err := filepath.Glob(pat)
		if err != nil {
			return nil, nil, fmt.Errorf("include pattern %q: %w", pat, err)
		}
		sort.Strings(matches)
		for _, incPath := range matches {
			inc, err := os.ReadFile(incPath)
			if err != nil {
				return nil, nil, fmt.Errorf("read include %q: %w", incPath, err)
			}
			var sub rawConfig
			if err := yaml.Unmarshal(inc, &sub); err != nil {
				return nil, nil, fmt.Errorf("parse include %q: %w", incPath, err)
			}
			if len(sub.Include) > 0 {
				return nil, nil, fmt.Errorf("include %q: nested include is not allowed", incPath)
			}
			if sub.Proxy != nil {
				return nil, nil, fmt.Errorf("include %q: proxy may only appear in the top-level file", incPath)
			}
			if sub.Auth != nil {
				return nil, nil, fmt.Errorf("include %q: auth may only appear in the top-level file", incPath)
			}
			merged.Servers = append(merged.Servers, sub.Servers...)
		}
	}

	merged.Servers = append(merged.Servers, root.Servers...)

	// Merge servers by name: last wins, warn on duplicates.
	seen := make(map[string]int, len(merged.Servers))
	unique := make([]ServerConfig, 0, len(merged.Servers))
	for _, s := range merged.Servers {
		if prev, ok := seen[s.Name]; ok {
			warnings = append(warnings,
				fmt.Sprintf("server %q defined more than once (last wins)", s.Name))
			unique[prev] = s
			continue
		}
		seen[s.Name] = len(unique)
		unique = append(unique, s)
	}
	merged.Servers = unique

	return merged, warnings, nil
}
