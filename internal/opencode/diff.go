package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// FileDiff represents a single file's diff from GET /api/session/{sessionID}/diff.
type FileDiff struct {
	File      string `json:"file"`
	Patch     string `json:"patch"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Status    string `json:"status"`
}

// SessionDiff fetches structured file diffs for a session (GET /api/session/{sessionID}/diff).
func (c *Client) SessionDiff(ctx context.Context, sessionID string) ([]FileDiff, error) {
	var raw json.RawMessage
	path := fmt.Sprintf("/api/session/%s/diff", url.PathEscape(sessionID))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var diffs []FileDiff
	if err := decodeDataOrDirect(raw, &diffs); err != nil {
		return nil, err
	}
	return diffs, nil
}

// ExtractToolModification inspects a tool invocation and, if it is a file-modifying
// tool (edit, write, patch, apply_patch, multiedit), returns the target file path
// and a unified diff or content preview.
func ExtractToolModification(toolName string, rawInput any) (filePath string, diffOrContent string, isFileMod bool) {
	name := strings.ToLower(strings.TrimSpace(toolName))
	switch name {
	case "edit", "write", "patch", "apply_patch", "multiedit", "multi_edit":
	default:
		return "", "", false
	}

	m := ParseToolInputMap(rawInput)

	switch name {
	case "edit":
		if m == nil {
			return "", "", false
		}
		filePath = firstString(m, "path", "filePath", "file_path", "file", "filename")
		oldStr, hasOld := firstStringOK(m, "oldString", "old_str", "old_string", "old")
		newStr, hasNew := firstStringOK(m, "newString", "new_str", "new_string", "new")
		if filePath == "" && !hasOld && !hasNew {
			return "", "", false
		}
		return filePath, formatUnifiedEditDiff(filePath, oldStr, newStr), true

	case "multiedit", "multi_edit":
		if m == nil {
			return "", "", false
		}
		filePath = firstString(m, "path", "filePath", "file_path", "file", "filename")
		var sb strings.Builder
		displayPath := filePath
		if displayPath == "" {
			displayPath = "file"
		}
		sb.WriteString("--- a/" + displayPath + "\n")
		sb.WriteString("+++ b/" + displayPath + "\n")
		if edits, ok := m["edits"].([]any); ok {
			for i, rawEdit := range edits {
				em, ok := rawEdit.(map[string]any)
				if !ok {
					continue
				}
				if i > 0 {
					sb.WriteString("@@ ... @@\n")
				}
				oldStr := firstString(em, "oldString", "old_str", "old_string", "old")
				newStr := firstString(em, "newString", "new_str", "new_string", "new")
				writeMinimalLineDiff(&sb, oldStr, newStr)
			}
		}
		return filePath, strings.TrimRight(sb.String(), "\n"), true

	case "write":
		if m == nil {
			return "", "", false
		}
		filePath = firstString(m, "path", "filePath", "file_path", "file", "filename")
		content, hasContent := firstStringOK(m, "content", "text", "data")
		if filePath == "" && !hasContent {
			return "", "", false
		}
		return filePath, formatWriteDiff(filePath, content), true

	case "patch", "apply_patch":
		var patchText string
		if m != nil {
			filePath = firstString(m, "path", "filePath", "file_path", "file", "filename")
			patchText = firstString(m, "patch", "diff", "content", "text")
		} else if s, ok := rawInput.(string); ok {
			patchText = s
		}
		if filePath == "" && patchText != "" {
			filePath = extractFileFromPatch(patchText)
		}
		if filePath == "" && strings.TrimSpace(patchText) == "" {
			return "", "", false
		}
		return filePath, strings.TrimRight(patchText, "\n"), true
	}

	return "", "", false
}

// ParseToolInputMap converts raw input (map, JSON raw message, string, bytes)
// into a generic map[string]any.
func ParseToolInputMap(rawInput any) map[string]any {
	if rawInput == nil {
		return nil
	}
	switch v := rawInput.(type) {
	case map[string]any:
		return v
	case json.RawMessage:
		var m map[string]any
		if json.Unmarshal(v, &m) == nil {
			return m
		}
	case []byte:
		var m map[string]any
		if json.Unmarshal(v, &m) == nil {
			return m
		}
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(v), &m) == nil {
			return m
		}
	default:
		b, err := json.Marshal(v)
		if err == nil {
			var m map[string]any
			if json.Unmarshal(b, &m) == nil {
				return m
			}
		}
	}
	return nil
}

func firstString(m map[string]any, keys ...string) string {
	val, _ := firstStringOK(m, keys...)
	return val
}

func firstStringOK(m map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if raw, exists := m[k]; exists {
			if s, ok := raw.(string); ok {
				return s, true
			}
		}
	}
	return "", false
}

func formatUnifiedEditDiff(filePath, oldStr, newStr string) string {
	displayPath := filePath
	if displayPath == "" {
		displayPath = "file"
	}
	var sb strings.Builder
	sb.WriteString("--- a/" + displayPath + "\n")
	sb.WriteString("+++ b/" + displayPath + "\n")
	writeMinimalLineDiff(&sb, oldStr, newStr)
	return strings.TrimRight(sb.String(), "\n")
}

func formatWriteDiff(filePath, content string) string {
	displayPath := filePath
	if displayPath == "" {
		displayPath = "file"
	}
	var sb strings.Builder
	sb.WriteString("--- /dev/null\n")
	sb.WriteString("+++ b/" + displayPath + "\n")
	for _, line := range splitLines(content) {
		sb.WriteString("+" + line + "\n")
	}
	return strings.TrimRight(sb.String(), "\n")
}

// CountDiffChanges counts added and deleted lines in a unified diff,
// counting lines starting with '+' (excluding '+++') and '-' (excluding '---').
func CountDiffChanges(diffText string) (added, deleted int) {
	for _, line := range splitLines(diffText) {
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		if strings.HasPrefix(line, "+") {
			added++
		} else if strings.HasPrefix(line, "-") {
			deleted++
		}
	}
	return added, deleted
}

func splitLines(s string) []string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

func writeMinimalLineDiff(sb *strings.Builder, oldStr, newStr string) {
	oldLines := splitLines(oldStr)
	newLines := splitLines(newStr)

	n, m := len(oldLines), len(newLines)
	if n == 0 && m == 0 {
		return
	}

	// Guard against quadratic blowup on very large inputs.
	if n*m > 250000 {
		for _, l := range oldLines {
			sb.WriteString("-" + l + "\n")
		}
		for _, l := range newLines {
			sb.WriteString("+" + l + "\n")
		}
		return
	}

	dp := make([][]int, n+1)
	for i := range dp {
		dp[i] = make([]int, m+1)
	}
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] >= dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}

	i, j := 0, 0
	for i < n || j < m {
		if i < n && j < m && oldLines[i] == newLines[j] {
			sb.WriteString(" " + oldLines[i] + "\n")
			i++
			j++
		} else if i < n && (j == m || dp[i+1][j] >= dp[i][j+1]) {
			sb.WriteString("-" + oldLines[i] + "\n")
			i++
		} else {
			sb.WriteString("+" + newLines[j] + "\n")
			j++
		}
	}
}

func extractFileFromPatch(patch string) string {
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{
			"+++ b/",
			"--- a/",
			"*** Update File: ",
			"*** Add File: ",
			"*** Delete File: ",
		} {
			if after, ok := strings.CutPrefix(line, prefix); ok {
				after = strings.TrimSpace(after)
				if after != "" && after != "/dev/null" {
					return after
				}
			}
		}
	}
	return ""
}
