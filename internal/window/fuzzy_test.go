package window

import (
	"testing"
)

func TestFuzzyMatchScore(t *testing.T) {
	tests := []struct {
		name      string
		query     string
		candidate string
		wantMatch bool
	}{
		{"empty query", "", "hello world", true},
		{"exact match", "switch", "just switch run0", true},
		{"multi-token match", "switch run0", "just switch run0 in .nix", true},
		{"case insensitive", "RUN0", "just switch run0", true},
		{"subsequence", "swr0", "just switch run0", true},
		{"subsequence letters", "jswr", "just switch run0", true},
		{"no match", "foobar", "just switch run0", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			score, ok := fuzzyMatchScore(tt.query, tt.candidate)
			if ok != tt.wantMatch {
				t.Fatalf("fuzzyMatchScore(%q, %q) ok = %v, want %v", tt.query, tt.candidate, ok, tt.wantMatch)
			}
			if ok && score < 0 {
				t.Fatalf("expected non-negative score, got %d", score)
			}
		})
	}

	// Verify exact substring ranks higher than loose subsequence
	exactScore, _ := fuzzyMatchScore("switch", "just switch run0")
	looseScore, _ := fuzzyMatchScore("switch", "s_w_i_t_c_h")
	if exactScore <= looseScore {
		t.Errorf("exact score %d should be > loose score %d", exactScore, looseScore)
	}
}
