package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func testExpander(env, files map[string]string) *expander {
	return &expander{
		lookup: func(k string) (string, bool) {
			v, ok := env[k]
			return v, ok
		},
		readFile: func(p string) ([]byte, error) {
			if v, ok := files[p]; ok {
				return []byte(v), nil
			}
			return nil, errors.New("no such file")
		},
	}
}

func TestExpand_Table(t *testing.T) {
	env := map[string]string{
		"SET":    "value",
		"EMPTY":  "",
		"NESTED": "inner",
	}
	files := map[string]string{
		"/tmp/secret":   "s3cret\n",
		"/tmp/multi":    "line1\nline2\n",
		"/tmp/trailing": "value\n\n",
	}
	e := testExpander(env, files)

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "no vars here", "no vars here"},
		{"simple", "${SET}", "value"},
		{"embedded", "a-${SET}-b", "a-value-b"},
		{"two vars", "${SET}-${SET}", "value-value"},
		{"default used", "${UNSET:-fallback}", "fallback"},
		{"default skipped when set", "${SET:-fallback}", "value"},
		{"default on empty", "${EMPTY:-fallback}", "fallback"},
		{"nested default", "${UNSET:-${NESTED}}", "inner"},
		{"dollar escape", "$${SET}", "${SET}"},
		{"dollar alone", "cost: $5", "cost: $5"},
		{"file simple", "${file:/tmp/secret}", "s3cret"},
		{"file multiline", "${file:/tmp/multi}", "line1\nline2"},
		{"file trailing newlines trimmed", "${file:/tmp/trailing}", "value"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := e.expand(tt.in)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExpand_Errors(t *testing.T) {
	e := testExpander(map[string]string{"SET": "v"}, nil)

	tests := []struct {
		name string
		in   string
		want string // substring
	}{
		{"unset required", "${MISSING}", "MISSING is not set"},
		{"empty required", "${EMPTYISH}", "EMPTYISH is not set"},
		{"custom message", "${NOPE:?set NOPE first}", "set NOPE first"},
		{"unterminated", "${SET", "unterminated"},
		{"invalid name", "${1BAD}", "invalid variable name"},
		{"empty name", "${}", "invalid variable name"},
		{"file missing", "${file:/no/such}", "read ${file:/no/such}"},
		{"file empty path", "${file:}", "empty path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := e.expand(tt.in)
			if err == nil {
				t.Fatalf("expected error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), tt.want)
			}
		})
	}
}

func TestExpandNode_OnlyStringScalars(t *testing.T) {
	// ints, bools and nulls must pass through untouched.
	src := `
n: 42
b: true
s: "hello"
`
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(src), &root); err != nil {
		t.Fatal(err)
	}
	if err := testExpander(nil, nil).expandNode(&root); err != nil {
		t.Fatal(err)
	}

	var out map[string]any
	if err := root.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out["n"] != 42 {
		t.Fatalf("n: %v", out["n"])
	}
	if out["b"] != true {
		t.Fatalf("b: %v", out["b"])
	}
	if out["s"] != "hello" {
		t.Fatalf("s: %v", out["s"])
	}
}

func TestExpandNode_RetagBoolAndInt(t *testing.T) {
	src := `
enabled: ${FLAG}
size: ${NUM}
`
	env := map[string]string{"FLAG": "true", "NUM": "1024"}
	var root yaml.Node
	if err := yaml.Unmarshal([]byte(src), &root); err != nil {
		t.Fatal(err)
	}
	if err := testExpander(env, nil).expandNode(&root); err != nil {
		t.Fatal(err)
	}

	var out struct {
		Enabled bool `yaml:"enabled"`
		Size    int  `yaml:"size"`
	}
	if err := root.Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !out.Enabled {
		t.Fatalf("enabled should be true")
	}
	if out.Size != 1024 {
		t.Fatalf("size: %d", out.Size)
	}
}

func TestLoad_ExpandsEnvAndFile(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, "config.yaml")
	body := fmt.Sprintf(`
version: 1
proxy:
  tool_prefix: "%s"
servers:
  - name: test
    command: /bin/echo
    enabled: true
    env:
      FROM_ENV: "${TEST_52_TOKEN}"
      FROM_FILE: "${file:%s}"
`, "${TEST_52_PREFIX}", secretPath)

	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TEST_52_PREFIX", "meta")
	t.Setenv("TEST_52_TOKEN", "s3cret")

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Proxy.ToolPrefix != "meta" {
		t.Fatalf("tool_prefix: %q", cfg.Proxy.ToolPrefix)
	}
	if got := cfg.Servers[0].Env["FROM_ENV"]; got != "s3cret" {
		t.Fatalf("FROM_ENV: %q", got)
	}
	if got := cfg.Servers[0].Env["FROM_FILE"]; got != "from-file" {
		t.Fatalf("FROM_FILE: %q", got)
	}
}

func TestLoad_UnsetVarFails(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	body := `
servers:
  - name: test
    command: /bin/echo
    enabled: true
    env:
      TOKEN: "${TEST_52_MISSING}"
`
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	os.Unsetenv("TEST_52_MISSING")

	if _, err := Load(cfgPath); err == nil {
		t.Fatal("expected error for unset variable")
	} else if !strings.Contains(err.Error(), "TEST_52_MISSING") {
		t.Fatalf("error should mention the variable: %v", err)
	}
}
