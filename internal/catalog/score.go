package catalog

import "strings"

// score returns a relevance score for query q against entry e.
// Higher = more relevant. 0 = no match.
//
// The query is tokenized on whitespace; each word is scored independently
// against the tool name, ref, server, tags and summary. Multi-word queries
// therefore match an entry whose description contains those words in any
// order and with arbitrary text between them.
func score(q string, e ToolEntry) float64 {
	q = strings.ToLower(strings.TrimSpace(q))
	if q == "" {
		return 1 // empty query matches everything
	}
	words := strings.Fields(q)
	if len(words) == 0 {
		return 1
	}

	toolLower := strings.ToLower(e.Tool)
	refLower := strings.ToLower(e.Ref)
	serverLower := strings.ToLower(e.Server)
	summaryLower := strings.ToLower(e.Summary)
	tagsLower := make([]string, len(e.Tags))
	for i, t := range e.Tags {
		tagsLower[i] = strings.ToLower(t)
	}

	var s float64
	matched := 0
	for _, w := range words {
		var wordScore float64
		// Exact tool name or ref is the strongest signal.
		if toolLower == w {
			wordScore += 5
		}
		if refLower == w {
			wordScore += 5
		}
		// Prefix and substring on the tool name.
		if strings.HasPrefix(toolLower, w) {
			wordScore += 3
		}
		if strings.Contains(toolLower, w) {
			wordScore += 2
		}
		// Ref / server / tags / summary — narrower weights.
		if strings.Contains(refLower, w) {
			wordScore += 1.5
		}
		for _, t := range tagsLower {
			if strings.Contains(t, w) {
				wordScore += 1.5
				break
			}
		}
		if strings.Contains(serverLower, w) {
			wordScore += 1
		}
		if strings.Contains(summaryLower, w) {
			wordScore += 1
		}
		if wordScore > 0 {
			matched++
			s += wordScore
		}
	}

	// Require every word to match something. A single unmatched word drops
	// the entry: "ssh password" should not surface ssh_exec just because
	// "ssh" matched.
	if matched < len(words) {
		return 0
	}
	return s
}
