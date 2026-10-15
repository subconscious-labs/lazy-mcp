package catalog

import "strings"

// scoreWord returns the relevance of a single query word w against e.
// Returns 0 if w doesn't match anything in e.
func scoreWord(w string, e ToolEntry) float64 {
	toolLower := strings.ToLower(e.Tool)
	refLower := strings.ToLower(e.Ref)
	serverLower := strings.ToLower(e.Server)
	summaryLower := strings.ToLower(e.Summary)

	var s float64
	if toolLower == w {
		s += 5
	}
	if refLower == w {
		s += 5
	}
	if strings.HasPrefix(toolLower, w) {
		s += 3
	}
	if strings.Contains(toolLower, w) {
		s += 2
	}
	if strings.Contains(refLower, w) {
		s += 1.5
	}
	for _, t := range e.Tags {
		if strings.Contains(strings.ToLower(t), w) {
			s += 1.5
			break
		}
	}
	if strings.Contains(serverLower, w) {
		s++
	}
	if strings.Contains(summaryLower, w) {
		s++
	}
	return s
}

// wordMatchesEntry reports whether w has any relevance to e.
func wordMatchesEntry(w string, e ToolEntry) bool {
	return scoreWord(w, e) > 0
}

// wordMatchesServerMeta reports whether w matches a server's own metadata
// (name, description, tags) rather than its tools.
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
