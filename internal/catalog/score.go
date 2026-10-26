package catalog

import "strings"

// scoreWord returns a per-word relevance in [0, 1] for one query word
// against e. Zero means no match. Values are calibrated for the human
// reader (and the model), not for ranking precision — ranking only
// needs monotonicity, which is preserved.
func scoreWord(w string, e *ToolEntry) float64 {
	if w == "" {
		return 0
	}

	best := 0.0

	// Strongest: exact name or exact ref.
	if e.toolLower == w || e.refLower == w {
		return 1.0
	}

	if strings.HasPrefix(e.toolLower, w) {
		best = 0.7
	}
	if strings.Contains(e.toolLower, w) && best < 0.5 {
		best = 0.5
	}
	if strings.Contains(e.refLower, w) && best < 0.4 {
		best = 0.4
	}
	for _, t := range e.tagsLower {
		if strings.Contains(t, w) {
			if best < 0.35 {
				best = 0.35
			}
			break
		}
	}
	if strings.Contains(e.serverLower, w) && best < 0.25 {
		best = 0.25
	}
	if strings.Contains(e.summaryLower, w) && best < 0.2 {
		best = 0.2
	}
	return best
}

// wordMatchesServerMeta reports whether w matches a server's own
// metadata (name, description, tags) rather than its tools.
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
