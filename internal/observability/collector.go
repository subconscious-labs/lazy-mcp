// Package observability collects lightweight metrics and writes them in
// Prometheus text exposition format to a file consumed by node_exporter's
// textfile collector.
//
// Deliberately dependency-free: we render counter/gauge/histogram formats
// by hand instead of pulling in prometheus/client_golang, since the file
// path does not need an HTTP handler, content negotiation or gzip.
package observability

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Version is exposed as the lazy_mcp_info{version="..."} label.
const Version = "0.1.0"

// defaultBounds are the histogram bucket upper bounds, in seconds.
// Chosen to cover 10ms..60s with fine granularity in the 0.1..5s range,
// which is where real tool calls live. The final +Inf bucket is implicit.
var defaultBounds = []float64{0.01, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60}

// Call status values used as the "status" label on tool_calls_total.
const (
	StatusOK          = "ok"
	StatusToolError   = "tool_error"
	StatusClientError = "client_error"
	StatusError       = "error"
	StatusCircuitOpen = "circuit_open"
	StatusTimeout     = "timeout"
	StatusCanceled    = "canceled"
)

// Circuit state codes for the lazy_mcp_circuit_state gauge.
const (
	CircuitDisabled = -1
	CircuitClosed   = 0
	CircuitHalfOpen = 1
	CircuitOpen     = 2
)

// CircuitCode maps a broker circuit state name to its gauge code.
func CircuitCode(name string) int {
	switch name {
	case "closed":
		return CircuitClosed
	case "half-open":
		return CircuitHalfOpen
	case "open":
		return CircuitOpen
	case "disabled":
		return CircuitDisabled
	}
	return CircuitDisabled
}

// ---- global default ----

var defaultCol = NewCollector()

// Default returns the process-wide collector.
func Default() *Collector { return defaultCol }

// Package-level wrappers used by broker/catalog/process.
func ObserveCall(server, tool, status string, dur time.Duration) {
	defaultCol.ObserveCall(server, tool, status, dur)
}
func SetCircuit(server string, code int)  { defaultCol.SetCircuit(server, code) }
func IncBackendStart(server string)       { defaultCol.IncBackendStart(server) }
func SetCatalogSize(server string, n int) { defaultCol.SetCatalogSize(server, n) }

// ---- collector ----

type callKey struct{ server, tool, status string }
type histoKey struct{ server, tool string }

type histoStat struct {
	counts []uint64 // len(bounds) + 1; last slot is +Inf
	sum    float64
}

// Collector accumulates metrics in memory. Safe for concurrent use.
type Collector struct {
	mu      sync.Mutex
	bounds  []float64
	calls   map[callKey]uint64
	histos  map[histoKey]*histoStat
	circuit map[string]int
	starts  map[string]uint64
	catalog map[string]int
}

// NewCollector returns a fresh collector with default histogram bounds.
func NewCollector() *Collector {
	return &Collector{
		bounds:  defaultBounds,
		calls:   make(map[callKey]uint64),
		histos:  make(map[histoKey]*histoStat),
		circuit: make(map[string]int),
		starts:  make(map[string]uint64),
		catalog: make(map[string]int),
	}
}

// ObserveCall records one tools/call outcome.
func (c *Collector) ObserveCall(server, tool, status string, dur time.Duration) {
	secs := dur.Seconds()

	c.mu.Lock()
	defer c.mu.Unlock()

	c.calls[callKey{server, tool, status}]++

	hk := histoKey{server, tool}
	h := c.histos[hk]
	if h == nil {
		h = &histoStat{counts: make([]uint64, len(c.bounds)+1)}
		c.histos[hk] = h
	}
	// Cumulative buckets: increment every bucket whose bound >= secs.
	for i, b := range c.bounds {
		if secs <= b {
			h.counts[i]++
		}
	}
	h.counts[len(c.bounds)]++ // +Inf
	h.sum += secs
}

// SetCircuit records the current circuit state for a backend.
func (c *Collector) SetCircuit(server string, code int) {
	c.mu.Lock()
	c.circuit[server] = code
	c.mu.Unlock()
}

// IncBackendStart increments the backend-start counter.
func (c *Collector) IncBackendStart(server string) {
	c.mu.Lock()
	c.starts[server]++
	c.mu.Unlock()
}

// SetCatalogSize records how many tools are indexed for a backend.
func (c *Collector) SetCatalogSize(server string, n int) {
	c.mu.Lock()
	c.catalog[server] = n
	c.mu.Unlock()
}

// Render produces the Prometheus text exposition payload.
func (c *Collector) Render() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()

	var b strings.Builder

	// lazy_mcp_info — always present, serves as a heartbeat.
	help(&b, "lazy_mcp_info", "Build information.")
	typ(&b, "lazy_mcp_info", "gauge")
	fmt.Fprintf(&b, "lazy_mcp_info{version=\"%s\"} 1\n", escapeLabel(Version))

	// lazy_mcp_backend_starts_total
	if len(c.starts) > 0 {
		help(&b, "lazy_mcp_backend_starts_total", "Backend process start count.")
		typ(&b, "lazy_mcp_backend_starts_total", "counter")
		for _, s := range sortedStringKeys(c.starts) {
			fmt.Fprintf(&b, "lazy_mcp_backend_starts_total{server=\"%s\"} %d\n",
				escapeLabel(s), c.starts[s])
		}
	}

	// lazy_mcp_catalog_tools
	if len(c.catalog) > 0 {
		help(&b, "lazy_mcp_catalog_tools", "Tools indexed per backend.")
		typ(&b, "lazy_mcp_catalog_tools", "gauge")
		for _, s := range sortedStringKeys(c.catalog) {
			fmt.Fprintf(&b, "lazy_mcp_catalog_tools{server=\"%s\"} %d\n",
				escapeLabel(s), c.catalog[s])
		}
	}

	// lazy_mcp_circuit_state
	if len(c.circuit) > 0 {
		help(&b, "lazy_mcp_circuit_state", "Circuit state per backend: 0=closed 1=half-open 2=open -1=disabled.")
		typ(&b, "lazy_mcp_circuit_state", "gauge")
		for _, s := range sortedStringKeys(c.circuit) {
			fmt.Fprintf(&b, "lazy_mcp_circuit_state{server=\"%s\"} %d\n",
				escapeLabel(s), c.circuit[s])
		}
	}

	// lazy_mcp_tool_calls_total
	if len(c.calls) > 0 {
		help(&b, "lazy_mcp_tool_calls_total", "Tool calls by outcome.")
		typ(&b, "lazy_mcp_tool_calls_total", "counter")
		keys := make([]callKey, 0, len(c.calls))
		for k := range c.calls {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].server != keys[j].server {
				return keys[i].server < keys[j].server
			}
			if keys[i].tool != keys[j].tool {
				return keys[i].tool < keys[j].tool
			}
			return keys[i].status < keys[j].status
		})
		for _, k := range keys {
			fmt.Fprintf(&b, "lazy_mcp_tool_calls_total{server=\"%s\",tool=\"%s\",status=\"%s\"} %d\n",
				escapeLabel(k.server), escapeLabel(k.tool), escapeLabel(k.status), c.calls[k])
		}
	}

	// lazy_mcp_tool_call_duration_seconds (histogram)
	if len(c.histos) > 0 {
		help(&b, "lazy_mcp_tool_call_duration_seconds", "Tool call latency in seconds.")
		typ(&b, "lazy_mcp_tool_call_duration_seconds", "histogram")
		keys := make([]histoKey, 0, len(c.histos))
		for k := range c.histos {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			if keys[i].server != keys[j].server {
				return keys[i].server < keys[j].server
			}
			return keys[i].tool < keys[j].tool
		})
		for _, k := range keys {
			h := c.histos[k]
			for i, bound := range c.bounds {
				fmt.Fprintf(&b,
					"lazy_mcp_tool_call_duration_seconds_bucket{server=\"%s\",tool=\"%s\",le=\"%s\"} %d\n",
					escapeLabel(k.server), escapeLabel(k.tool),
					strconv.FormatFloat(bound, 'g', -1, 64), h.counts[i])
			}
			fmt.Fprintf(&b,
				"lazy_mcp_tool_call_duration_seconds_bucket{server=\"%s\",tool=\"%s\",le=\"+Inf\"} %d\n",
				escapeLabel(k.server), escapeLabel(k.tool), h.counts[len(c.bounds)])
			fmt.Fprintf(&b,
				"lazy_mcp_tool_call_duration_seconds_sum{server=\"%s\",tool=\"%s\"} %g\n",
				escapeLabel(k.server), escapeLabel(k.tool), h.sum)
			fmt.Fprintf(&b,
				"lazy_mcp_tool_call_duration_seconds_count{server=\"%s\",tool=\"%s\"} %d\n",
				escapeLabel(k.server), escapeLabel(k.tool), h.counts[len(c.bounds)])
		}
	}

	return []byte(b.String())
}

// WriteFile renders the current snapshot and writes it atomically to path.
// node_exporter's textfile collector reads files concurrently with our
// writes, so we create a temp file in the same directory and rename it
// over the target. Rename on the same filesystem is atomic on POSIX.
func (c *Collector) WriteFile(path string) error {
	data := c.Render()

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()

	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temp: %w", err)
	}
	// CreateTemp uses 0600. The .prom file must be readable by the
	// node_exporter process, which runs under a different user. Chmod
	// before close so the renamed file already has the correct mode —
	// no window where the target is 0600.
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename: %w", err)
	}
	cleanup = false
	return nil
}

// ---- helpers ----

func help(b *strings.Builder, name, text string) {
	fmt.Fprintf(b, "# HELP %s %s\n", name, text)
}

func typ(b *strings.Builder, name, kind string) {
	fmt.Fprintf(b, "# TYPE %s %s\n", name, kind)
}

// escapeLabel escapes a label value per the Prometheus text format:
// backslash, double quote and newline.
func escapeLabel(s string) string {
	if !strings.ContainsAny(s, "\\\"\n") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s) + 4)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sortedStringKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
