package window

import (
	"os"
	"path/filepath"
	"testing"

	"libdb.so/gluppencode/internal/opencode"
)

func TestParseMentions(t *testing.T) {
	tempDir := t.TempDir()
	testFile := filepath.Join(tempDir, "main.go")
	if err := os.WriteFile(testFile, []byte("package main"), 0644); err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}

	skills := []opencode.SkillInfo{
		{ID: "review", Name: "code-review", Description: "Reviews code"},
		{ID: "compact", Name: "compact-changes", Description: "Compacts changelogs"},
	}
	agents := []opencode.Agent{
		{Name: "orchestrator", Description: "Coordinates tasks"},
		{Name: "explore", Description: "Explores codebase"},
	}

	tests := []struct {
		name       string
		text       string
		wantAgents int
		wantSkills int
		wantPaths  int
	}{
		{
			name:       "plain text without mentions",
			text:       "Hello world, please fix this bug",
			wantAgents: 0,
			wantSkills: 0,
			wantPaths:  0,
		},
		{
			name:       "agent mention",
			text:       "Hey @explore can you look at this?",
			wantAgents: 1,
			wantSkills: 0,
			wantPaths:  0,
		},
		{
			name:       "skill mention",
			text:       "Run @review on this PR",
			wantAgents: 0,
			wantSkills: 1,
			wantPaths:  0,
		},
		{
			name:       "workspace file mention",
			text:       "Inspect @main.go for issues",
			wantAgents: 0,
			wantSkills: 0,
			wantPaths:  1,
		},
		{
			name:       "multiple mentions combined",
			text:       "@orchestrator please use @review on @main.go now",
			wantAgents: 1,
			wantSkills: 1,
			wantPaths:  1,
		},
		{
			name:       "file with path and extension",
			text:       "Check @internal/window/window.go please",
			wantAgents: 0,
			wantSkills: 0,
			wantPaths:  1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := parseMentions(tt.text, skills, agents, tempDir)
			if len(opts.Agents) != tt.wantAgents {
				t.Errorf("len(Agents) = %d, want %d", len(opts.Agents), tt.wantAgents)
			}
			if len(opts.Skills) != tt.wantSkills {
				t.Errorf("len(Skills) = %d, want %d", len(opts.Skills), tt.wantSkills)
			}
			if len(opts.Paths) != tt.wantPaths {
				t.Errorf("len(Paths) = %d, want %d", len(opts.Paths), tt.wantPaths)
			}

			// Validate mention offsets
			for _, a := range opts.Agents {
				if a.Mention == nil || a.Mention.Text == "" {
					t.Errorf("agent mention missing mention details: %+v", a)
				} else if tt.text[a.Mention.Start:a.Mention.End] != a.Mention.Text {
					t.Errorf("agent offset slice %q != mention text %q", tt.text[a.Mention.Start:a.Mention.End], a.Mention.Text)
				}
			}
			for _, s := range opts.Skills {
				if s.Mention == nil || s.Mention.Text == "" {
					t.Errorf("skill mention missing mention details: %+v", s)
				} else if tt.text[s.Mention.Start:s.Mention.End] != s.Mention.Text {
					t.Errorf("skill offset slice %q != mention text %q", tt.text[s.Mention.Start:s.Mention.End], s.Mention.Text)
				}
			}
			for _, p := range opts.Paths {
				if p.Mention == nil || p.Mention.Text == "" {
					t.Errorf("path mention missing mention details: %+v", p)
				} else if tt.text[p.Mention.Start:p.Mention.End] != p.Mention.Text {
					t.Errorf("path offset slice %q != mention text %q", tt.text[p.Mention.Start:p.Mention.End], p.Mention.Text)
				}
			}
		})
	}
}
