package observability

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRender_InfoAlwaysPresent(t *testing.T) {
	c := NewCollector()
	out := string(c.Render())
	if !strings.Contains(out, "lazy_mcp_info") {
		t.Fatalf("info metric missing:\n%s", out)
	}
}

func TestObserveCall_CumulativeBuckets(t *testing.T) {
	c := NewCollector()
	c.ObserveCall("srv", "tool", StatusOK, 100*time.Millisecond)
	c.ObserveCall("srv", "tool", StatusOK, 700*time.Millisecond)
	c.ObserveCall("srv", "tool", StatusToolError, 3*time.Second)

	out := string(c.Render())

	cases := []string{
		// counters
		`lazy_mcp_tool_calls_total{server="srv",tool="tool",status="ok"} 2`,
		`lazy_mcp_tool_calls_total{server="srv",tool="tool",status="tool_error"} 1`,
		// cumulative buckets
		`lazy_mcp_tool_call_duration_seconds_bucket{server="srv",tool="tool",le="0.1"} 1`,
		`lazy_mcp_tool_call_duration_seconds_bucket{server="srv",tool="tool",le="0.5"} 1`,
		`lazy_mcp_tool_call_duration_seconds_bucket{server="srv",tool="tool",le="1"} 2`,
		`lazy_mcp_tool_call_duration_seconds_bucket{server="srv",tool="tool",le="5"} 3`,
		`lazy_mcp_tool_call_duration_seconds_bucket{server="srv",tool="tool",le="+Inf"} 3`,
		`lazy_mcp_tool_call_duration_seconds_count{server="srv",tool="tool"} 3`,
	}
	for _, want := range cases {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestSetCircuitAndCatalogAndStarts(t *testing.T) {
	c := NewCollector()
	c.SetCircuit("a", CircuitOpen)
	c.SetCatalogSize("a", 42)
	c.IncBackendStart("a")
	c.IncBackendStart("a")

	out := string(c.Render())
	for _, want := range []string{
		`lazy_mcp_circuit_state{server="a"} 2`,
		`lazy_mcp_catalog_tools{server="a"} 42`,
		`lazy_mcp_backend_starts_total{server="a"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestWriteFile_CreatesAndReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lazy-mcp.prom")

	c := NewCollector()
	c.IncBackendStart("a")
	if err := c.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `lazy_mcp_backend_starts_total{server="a"} 1`) {
		t.Fatalf("first write missing metric:\n%s", data)
	}

	c.IncBackendStart("a")
	if err := c.WriteFile(path); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(path)
	if !strings.Contains(string(data), `lazy_mcp_backend_starts_total{server="a"} 2`) {
		t.Fatalf("second write missing updated metric:\n%s", data)
	}

	// No temp files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "lazy-mcp.prom" {
			t.Errorf("unexpected leftover file: %s", e.Name())
		}
	}
}

func TestWriteFile_MissingDir(t *testing.T) {
	c := NewCollector()
	if err := c.WriteFile("/nonexistent/dir/lazy-mcp.prom"); err == nil {
		t.Fatal("expected error for missing directory")
	}
}

func TestEscapeLabel(t *testing.T) {
	cases := map[string]string{
		"plain":          "plain",
		`with"quote`:     `with\"quote`,
		`with\backslash`: `with\\backslash`,
		"with\nnewline":  `with\nnewline`,
	}
	for in, want := range cases {
		if got := escapeLabel(in); got != want {
			t.Errorf("escapeLabel(%q) = %q, want %q", in, got, want)
		}
	}
}
