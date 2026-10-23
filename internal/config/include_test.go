package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoad_IncludesServers(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "config.yaml"), `
version: 1
include:
  - "conf.d/*.yaml"
proxy:
  tool_prefix: "meta"
servers:
  - name: main
    command: /bin/true
    enabled: true
`)
	writeFile(t, filepath.Join(dir, "conf.d/git.yaml"), `
servers:
  - name: git
    command: /bin/true
    enabled: true
`)
	writeFile(t, filepath.Join(dir, "conf.d/tasks.yaml"), `
servers:
  - name: tasks
    command: /bin/true
    enabled: true
`)

	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 3 {
		t.Fatalf("servers: got %d, want 3", len(cfg.Servers))
	}
	names := map[string]bool{}
	for _, s := range cfg.Servers {
		names[s.Name] = true
	}
	for _, want := range []string{"main", "git", "tasks"} {
		if !names[want] {
			t.Fatalf("missing server %q", want)
		}
	}
}

func TestLoad_DuplicateServerLastWins(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "config.yaml"), `
include: ["inc.yaml"]
servers:
  - name: x
    command: /bin/true
    enabled: true
    description: "from main"
`)
	writeFile(t, filepath.Join(dir, "inc.yaml"), `
servers:
  - name: x
    command: /bin/true
    enabled: true
    description: "from include"
`)

	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Servers) != 1 {
		t.Fatalf("expected one server after merge, got %d", len(cfg.Servers))
	}
	if cfg.Servers[0].Description != "from main" {
		t.Fatalf("main should win: %q", cfg.Servers[0].Description)
	}
	warn := DrainWarnings()
	if len(warn) == 0 || !strings.Contains(warn[0], "more than once") {
		t.Fatalf("expected duplicate warning, got %v", warn)
	}
}

func TestLoad_ProxyInIncludeRejected(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "config.yaml"), `
include: ["inc.yaml"]
proxy:
  tool_prefix: "meta"
`)
	writeFile(t, filepath.Join(dir, "inc.yaml"), `
proxy:
  tool_prefix: "other"
`)

	_, err := Load(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "proxy may only appear") {
		t.Fatalf("expected proxy-in-include error, got %v", err)
	}
}

func TestLoad_AuthInIncludeRejected(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "config.yaml"), `include: ["inc.yaml"]`)
	writeFile(t, filepath.Join(dir, "inc.yaml"), `
auth:
  enabled: true
  users: []
`)

	_, err := Load(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "auth may only appear") {
		t.Fatalf("expected auth-in-include error, got %v", err)
	}
}

func TestLoad_NestedIncludeRejected(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "config.yaml"), `include: ["inc.yaml"]`)
	writeFile(t, filepath.Join(dir, "inc.yaml"), `include: ["deeper.yaml"]`)

	_, err := Load(filepath.Join(dir, "config.yaml"))
	if err == nil || !strings.Contains(err.Error(), "nested include") {
		t.Fatalf("expected nested include error, got %v", err)
	}
}

func TestLoad_IncludeExpandsEnv(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "config.yaml"), `
include: ["inc.yaml"]
servers:
  - name: a
    command: /bin/true
    enabled: true
`)
	writeFile(t, filepath.Join(dir, "inc.yaml"), `
servers:
  - name: git
    command: /bin/true
    enabled: true
    env:
      TOKEN: "${TEST_INCLUDE_TOKEN}"
`)
	t.Setenv("TEST_INCLUDE_TOKEN", "s3cret")

	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var git *ServerConfig
	for i := range cfg.Servers {
		if cfg.Servers[i].Name == "git" {
			git = &cfg.Servers[i]
			break
		}
	}
	if git == nil {
		t.Fatal("git missing")
	}
	if git.Env["TOKEN"] != "s3cret" {
		t.Fatalf("env not expanded: %q", git.Env["TOKEN"])
	}
}

func TestLoad_IncludeEmptyGlobIsOK(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, filepath.Join(dir, "config.yaml"), `
include: ["conf.d/*.yaml"]
servers:
  - name: a
    command: /bin/true
    enabled: true
`)
	// No conf.d/ directory at all.

	cfg, err := Load(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("empty glob should not fail: %v", err)
	}
	if len(cfg.Servers) != 1 || cfg.Servers[0].Name != "a" {
		t.Fatalf("servers: %+v", cfg.Servers)
	}
}
