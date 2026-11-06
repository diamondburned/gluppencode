// Package window implements the main Gluppencode GTK4 window, session timeline, and widgets.
package window

import (
	"strings"
)

// fuzzyMatchScore calculates a fuzzy match score for query against candidate.
// Returns (score, ok). Higher scores indicate better matches.
func fuzzyMatchScore(query, candidate string) (int, bool) {
	query = strings.ToLower(strings.TrimSpace(query))
	candidate = strings.ToLower(candidate)
	if query == "" {
		return 0, true
	}

	// Exact substring match bonus
	if idx := strings.Index(candidate, query); idx != -1 {
		score := 2000 - idx
		if idx == 0 || candidate[idx-1] == ' ' || candidate[idx-1] == '/' || candidate[idx-1] == '-' || candidate[idx-1] == '_' || candidate[idx-1] == '.' {
			score += 500 // word boundary
		}
		return score, true
	}

	// Multi-word / token search (e.g. "switch run0")
	tokens := strings.Fields(query)
	if len(tokens) > 1 {
		allMatch := true
		totalScore := 0
		for _, tok := range tokens {
			s, ok := fuzzyMatchScore(tok, candidate)
			if !ok {
				allMatch = false
				break
			}
			totalScore += s
		}
		if allMatch {
			return totalScore, true
		}
	}

	// Subsequence match
	qRunes := []rune(query)
	cRunes := []rune(candidate)
	qi := 0
	score := 0
	consecutive := 0

	for ci := 0; ci < len(cRunes) && qi < len(qRunes); ci++ {
		if cRunes[ci] == qRunes[qi] {
			score += 10
			if ci == 0 || cRunes[ci-1] == ' ' || cRunes[ci-1] == '/' || cRunes[ci-1] == '-' || cRunes[ci-1] == '_' || cRunes[ci-1] == '.' {
				score += 30 // word boundary bonus
			}
			if consecutive > 0 {
				score += consecutive * 15 // consecutive match bonus
			}
			consecutive++
			qi++
		} else {
			consecutive = 0
		}
	}

	if qi == len(qRunes) {
		return score, true
	}
	return 0, false
}
