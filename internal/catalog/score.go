package catalog

import (
	"encoding/json"
	"strings"
)

// jsonUnmarshal is a thin alias to keep catalog.go clean.
func jsonUnmarshal(data []byte, v any) error {
	return json.Unmarshal(data, v)
}

// score returns a relevance score for query q against entry e.
// Higher = more relevant. 0 = no match.
func score(q string, e ToolEntry) float64 {
	if q == "" {
		return 1 // return all entries for empty query
	}
	var s float64

	toolLower := strings.ToLower(e.Tool)
	refLower := strings.ToLower(e.Ref)
	summaryLower := strings.ToLower(e.Summary)

	// Exact tool name match
	if toolLower == q {
		s += 5
	}
	// Exact ref match
	if refLower == q {
		s += 5
	}
	// Prefix match on tool name
	if strings.HasPrefix(toolLower, q) {
		s += 3
	}
	// Substring match in tool name
	if strings.Contains(toolLower, q) {
		s += 2
	}
	// Substring match in ref
	if strings.Contains(refLower, q) {
		s += 1.5
	}
	// Tag match
	for _, tag := range e.Tags {
		if strings.Contains(strings.ToLower(tag), q) {
			s += 1.5
		}
	}
	// Description/summary match
	if strings.Contains(summaryLower, q) {
		s += 1
	}
	// Server name match
	if strings.Contains(strings.ToLower(e.Server), q) {
		s += 1
	}

	return s
}
