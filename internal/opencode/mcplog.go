package opencode

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	mcpLogReadChunk     = 512 * 1024
	mcpLogInitialWindow = 2 * 1024 * 1024
	mcpLogMaxWindow     = 8 * 1024 * 1024
	mcpLogMinSeedCount  = 300
	mcpLogMaxEntries    = 1000
	mcpLogMaxPollBytes  = 4 * 1024 * 1024
)

// McpLogField is a single ordered key=value field parsed from an OpenCode log line.
type McpLogField struct {
	Key   string
	Value string
}

// McpLogEntry represents one structured MCP-related entry parsed from opencode.log.
type McpLogEntry struct {
	Timestamp time.Time
	RawTime   string
	Level     string // DEBUG, INFO, WARN, ERROR
	Message   string
	Server    string
	Directory string
	Error     string
	Output    string
	Extras    []McpLogField
	Compacted bool
}

// IsLoopbackServerURL reports whether raw points to the local host (or is empty).
func IsLoopbackServerURL(raw string) bool {
	if strings.TrimSpace(raw) == "" {
		return true
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// DefaultLogDir returns OpenCode's local log directory ($XDG_DATA_HOME/opencode/log).
func DefaultLogDir() string {
	if dataDir := os.Getenv("XDG_DATA_HOME"); dataDir != "" {
		return filepath.Join(dataDir, "opencode", "log")
	}
	if homeDir, err := os.UserHomeDir(); err == nil && homeDir != "" {
		return filepath.Join(homeDir, ".local", "share", "opencode", "log")
	}
	return ""
}

// ResolveLogFile locates the active OpenCode log file inside logDir.
// It prefers opencode.log unless an opencode-*.log regular file exists with a
// strictly newer modification time (used by local channel builds).
func ResolveLogFile(logDir string) (string, error) {
	if strings.TrimSpace(logDir) == "" {
		return "", os.ErrNotExist
	}
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return "", err
	}

	var mainPath string
	var mainMod time.Time
	var newerPath string
	var newerMod time.Time

	for _, entry := range entries {
		name := entry.Name()
		if name != "opencode.log" && !(strings.HasPrefix(name, "opencode-") && strings.HasSuffix(name, ".log")) {
			continue
		}
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		fullPath := filepath.Join(logDir, name)
		mod := info.ModTime()
		if name == "opencode.log" {
			mainPath = fullPath
			mainMod = mod
			continue
		}
		if newerPath == "" || mod.After(newerMod) {
			newerPath = fullPath
			newerMod = mod
		}
	}

	if mainPath != "" {
		if newerPath != "" && newerMod.After(mainMod) {
			return newerPath, nil
		}
		return mainPath, nil
	}
	if newerPath != "" {
		return newerPath, nil
	}
	return "", os.ErrNotExist
}

// ParseLogLine parses a single OpenCode logfmt line into structured fields.
func ParseLogLine(line string) (McpLogEntry, []McpLogField, bool) {
	line = strings.TrimRight(line, "\r\n")
	if strings.TrimSpace(line) == "" {
		return McpLogEntry{}, nil, false
	}
	fields := parseLogfmtFields(line)
	if len(fields) == 0 {
		return McpLogEntry{}, nil, false
	}

	var entry McpLogEntry
	var statusError string
	for _, f := range fields {
		switch f.Key {
		case "timestamp":
			if entry.RawTime == "" {
				entry.RawTime = f.Value
				if ts, err := time.Parse(time.RFC3339Nano, f.Value); err == nil {
					entry.Timestamp = ts
				}
			}
		case "level":
			if entry.Level == "" {
				entry.Level = strings.ToUpper(f.Value)
			}
		case "message":
			if entry.Message == "" {
				entry.Message = f.Value
			}
		case "server":
			if entry.Server == "" && f.Value != "" {
				entry.Server = f.Value
			}
		case "directory":
			if entry.Directory == "" {
				entry.Directory = f.Value
			}
		case "error":
			if entry.Error == "" {
				entry.Error = f.Value
			}
		case "status.error":
			if statusError == "" {
				statusError = f.Value
			}
		case "output":
			if entry.Output == "" {
				entry.Output = f.Value
			}
		case "run", "http.span", "role", "connectionID":
			// Omit high-noise span/tracing metadata from UI Extras.
		default:
			entry.Extras = append(entry.Extras, f)
		}
	}
	if entry.Error == "" && statusError != "" {
		entry.Error = statusError
	}
	if entry.Level == "" {
		entry.Level = "INFO"
	}
	return entry, fields, entry.Message != "" || entry.Server != "" || entry.Error != "" || entry.Output != ""
}

func parseLogfmtFields(line string) []McpLogField {
	var fields []McpLogField
	i := 0
	n := len(line)
	for i < n {
		for i < n && (line[i] == ' ' || line[i] == '\t') {
			i++
		}
		if i >= n {
			break
		}
		eq := strings.IndexByte(line[i:], '=')
		if eq <= 0 {
			break
		}
		key := line[i : i+eq]
		i += eq + 1
		if i >= n {
			fields = append(fields, McpLogField{Key: key, Value: ""})
			break
		}
		if line[i] == '"' {
			start := i
			i++
			escaped := false
			closed := false
			for i < n {
				c := line[i]
				if escaped {
					escaped = false
					i++
					continue
				}
				if c == '\\' {
					escaped = true
					i++
					continue
				}
				if c == '"' {
					i++
					closed = true
					break
				}
				i++
			}
			token := line[start:i]
			val := token
			if closed {
				if unq, err := strconv.Unquote(token); err == nil {
					val = unq
				} else {
					val = strings.TrimSuffix(strings.TrimPrefix(token, `"`), `"`)
				}
			} else {
				val = strings.TrimPrefix(token, `"`)
			}
			fields = append(fields, McpLogField{Key: key, Value: val})
		} else {
			start := i
			for i < n && line[i] != ' ' && line[i] != '\t' {
				i++
			}
			fields = append(fields, McpLogField{Key: key, Value: line[start:i]})
		}
	}
	return fields
}

// MatchesMcpEntry reports whether a parsed log line belongs to serverName (or to
// any MCP server when serverName is empty).
func MatchesMcpEntry(fields []McpLogField, entry McpLogEntry, serverName string) bool {
	if serverName != "" {
		for _, f := range fields {
			if f.Key == "server" && f.Value == serverName {
				return true
			}
		}
		if (strings.HasPrefix(entry.Message, "mcp") || strings.HasPrefix(entry.Message, "client MCP")) &&
			strings.Contains(entry.Message, serverName) {
			return true
		}
		return false
	}

	for _, f := range fields {
		if f.Key == "server" && strings.TrimSpace(f.Value) != "" {
			return true
		}
		if f.Key == "integrationID" && strings.HasPrefix(f.Value, "mcp_") {
			return true
		}
	}
	return strings.HasPrefix(entry.Message, "mcp ") || strings.HasPrefix(entry.Message, "client MCP")
}

// McpLogTailer incrementally tails OpenCode's local opencode.log file while
// handling bounded backward initial seeding and in-place compaction recovery.
type McpLogTailer struct {
	LogDir   string
	FilePath string
	offset   int64
}

// NewMcpLogTailer creates a tailer rooted at logDir (defaulting to DefaultLogDir).
func NewMcpLogTailer(logDir string) *McpLogTailer {
	if strings.TrimSpace(logDir) == "" {
		logDir = DefaultLogDir()
	}
	return &McpLogTailer{
		LogDir: logDir,
	}
}

// ResetOffset resets the incremental read cursor back to 0.
func (t *McpLogTailer) ResetOffset() {
	t.offset = 0
}

// ReadInitialTail scans backwards from EOF in bounded chunks to seed up to
// mcpLogMaxEntries matching entries and positions offset after the final newline.
func (t *McpLogTailer) ReadInitialTail(serverName string) ([]McpLogEntry, error) {
	if t.FilePath == "" {
		resolved, err := ResolveLogFile(t.LogDir)
		if err != nil {
			return nil, err
		}
		t.FilePath = resolved
	} else if _, err := os.Stat(t.FilePath); errors.Is(err, os.ErrNotExist) {
		resolved, rerr := ResolveLogFile(t.LogDir)
		if rerr != nil {
			return nil, rerr
		}
		t.FilePath = resolved
	}

	f, err := os.Open(t.FilePath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	t.offset = 0
	if size == 0 {
		return nil, nil
	}

	cursor := size
	var bytesRead int64
	var seenEOFNewline bool
	var prevHead []byte
	var chunkSlices [][]McpLogEntry
	totalMatches := 0

	for cursor > 0 && bytesRead < mcpLogMaxWindow {
		step := int64(mcpLogReadChunk)
		if cursor < step {
			step = cursor
		}
		if bytesRead+step > mcpLogMaxWindow {
			step = mcpLogMaxWindow - bytesRead
		}
		chunkStart := cursor - step
		buf := make([]byte, step)
		n, rerr := f.ReadAt(buf, chunkStart)
		if rerr != nil && !errors.Is(rerr, io.EOF) {
			return nil, rerr
		}
		buf = buf[:n]
		cursor = chunkStart
		bytesRead += int64(n)

		var data []byte
		if !seenEOFNewline {
			lastNL := bytes.LastIndexByte(buf, '\n')
			if lastNL < 0 {
				// Entire chunk lies inside an incomplete trailing line at EOF.
				continue
			}
			t.offset = chunkStart + int64(lastNL) + 1
			seenEOFNewline = true
			data = buf[:lastNL+1]
		} else {
			if len(prevHead) > 0 {
				data = make([]byte, len(buf)+len(prevHead))
				copy(data, buf)
				copy(data[len(buf):], prevHead)
				prevHead = nil
			} else {
				data = buf
			}
		}

		var complete []byte
		if chunkStart > 0 {
			firstNL := bytes.IndexByte(data, '\n')
			if firstNL < 0 {
				prevHead = append([]byte(nil), data...)
				continue
			}
			prevHead = append([]byte(nil), data[:firstNL+1]...)
			complete = data[firstNL+1:]
		} else {
			complete = data
		}

		matched := parseCompleteLines(complete, serverName)
		if len(matched) > 0 {
			chunkSlices = append(chunkSlices, matched)
			totalMatches += len(matched)
		}

		if bytesRead >= mcpLogInitialWindow && (totalMatches >= mcpLogMinSeedCount || totalMatches >= mcpLogMaxEntries) {
			break
		}
	}

	if len(chunkSlices) == 0 {
		return nil, nil
	}
	slices.Reverse(chunkSlices)
	out := make([]McpLogEntry, 0, min(totalMatches, mcpLogMaxEntries))
	for _, chunk := range chunkSlices {
		out = append(out, chunk...)
	}
	if len(out) > mcpLogMaxEntries {
		out = slices.Clone(out[len(out)-mcpLogMaxEntries:])
	}
	return out, nil
}

// PollNewLines reads newly appended complete lines since the last offset,
// skipping ticks while <file>.trim exists and reloading on in-place compaction.
func (t *McpLogTailer) PollNewLines(serverName string) ([]McpLogEntry, bool, error) {
	if t.FilePath == "" {
		resolved, err := ResolveLogFile(t.LogDir)
		if err != nil {
			return nil, false, err
		}
		t.FilePath = resolved
		entries, err := t.ReadInitialTail(serverName)
		return entries, false, err
	}

	// OpenCode holds <logFile>.trim as a directory lock while truncating in place.
	if _, err := os.Stat(t.FilePath + ".trim"); err == nil {
		return nil, false, nil
	}

	info, err := os.Stat(t.FilePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if resolved, rerr := ResolveLogFile(t.LogDir); rerr == nil && resolved != t.FilePath {
				t.FilePath = resolved
				t.offset = 0
				entries, rerr := t.ReadInitialTail(serverName)
				return entries, true, rerr
			}
		}
		return nil, false, err
	}

	size := info.Size()
	if size < t.offset {
		t.offset = 0
		entries, err := t.ReadInitialTail(serverName)
		if err != nil {
			return nil, true, err
		}
		divider := McpLogEntry{
			Timestamp: time.Now(),
			Level:     "INFO",
			Message:   "Log file compacted in place — reloaded tail",
			Compacted: true,
		}
		out := make([]McpLogEntry, 0, len(entries)+1)
		out = append(out, divider)
		out = append(out, entries...)
		if len(out) > mcpLogMaxEntries {
			out = slices.Clone(out[len(out)-mcpLogMaxEntries:])
		}
		return out, true, nil
	}
	if size == t.offset {
		return nil, false, nil
	}

	toRead := size - t.offset
	if toRead > mcpLogMaxPollBytes {
		toRead = mcpLogMaxPollBytes
	}
	f, err := os.Open(t.FilePath)
	if err != nil {
		return nil, false, err
	}
	defer f.Close()

	buf := make([]byte, toRead)
	n, rerr := f.ReadAt(buf, t.offset)
	if rerr != nil && !errors.Is(rerr, io.EOF) {
		return nil, false, rerr
	}
	buf = buf[:n]
	lastNL := bytes.LastIndexByte(buf, '\n')
	if lastNL < 0 {
		return nil, false, nil
	}
	t.offset += int64(lastNL) + 1
	entries := parseCompleteLines(buf[:lastNL+1], serverName)
	if len(entries) > mcpLogMaxEntries {
		entries = slices.Clone(entries[len(entries)-mcpLogMaxEntries:])
	}
	return entries, false, nil
}

func parseCompleteLines(data []byte, serverName string) []McpLogEntry {
	var out []McpLogEntry
	for len(data) > 0 {
		nl := bytes.IndexByte(data, '\n')
		var line []byte
		if nl < 0 {
			line = data
			data = nil
		} else {
			line = data[:nl]
			data = data[nl+1:]
		}
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		entry, fields, ok := ParseLogLine(string(line))
		if !ok {
			continue
		}
		if MatchesMcpEntry(fields, entry, serverName) {
			out = append(out, entry)
		}
	}
	return out
}
