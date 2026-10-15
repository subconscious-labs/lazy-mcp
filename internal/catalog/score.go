package catalog

import "strings"

// scoreWord returns the relevance of a single query word w against e.
// All lowercase views are precomputed on ToolEntry, so this performs no
// allocations.
func scoreWord(w string, e ToolEntry) float64 {
	var s float64

	if e.toolLower == w {
		s += 5
	}
	if e.refLower == w {
		s += 5
	}
	if strings.HasPrefix(e.toolLower, w) {
		s += 3
	}
	if strings.Contains(e.toolLower, w) {
		s += 2
	}
	if strings.Contains(e.refLower, w) {
		s += 1.5
	}
	for _, t := range e.tagsLower {
		if strings.Contains(t, w) {
			s += 1.5
			break
		}
	}
	if strings.Contains(e.serverLower, w) {
		s++
	}
	if strings.Contains(e.summaryLower, w) {
		s++
	}
	return s
}

// wordMatchesEntry reports whether w has any relevance to e.
func wordMatchesEntry(w string, e ToolEntry) bool {
	if e.toolLower == w || e.refLower == w {
		return true
	}
	if strings.Contains(e.toolLower, w) {
		return true
	}
	if strings.Contains(e.refLower, w) {
		return true
	}
	for _, t := range e.tagsLower {
		if strings.Contains(t, w) {
			return true
		}
	}
	if strings.Contains(e.serverLower, w) {
		return true
	}
	if strings.Contains(e.summaryLower, w) {
		return true
	}
	return false
}

// wordMatchesServerMeta reports whether w matches a server's own metadata
// (name, description, tags) rather than its tools. This path is not on the
// hot loop (server count is small), so it lowercases on the fly.
func wordMatchesServerMeta(w, serverName, serverDescription string, serverTags []string) bool {
	if strings.Contains(strings.ToLower(serverName), w) {
		return true
	}
	if strings.Contains(strings.ToLower(serverDescription), w) {
		return true
	}
	for _, t := range serverTags {
		if strings.Contains(strings.ToLower(t), w) {
			return true
		}
	}
	return false
}
