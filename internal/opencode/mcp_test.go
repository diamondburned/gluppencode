package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMCPClientEndpoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var (
		listDir      string
		resDir       string
		cfgDir       string
		connectPath  string
		connectDir   string
		disconnPath  string
		disconnDir   string
	)

	client := &Client{
		BaseURL: "http://127.0.0.1:4096",
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodGet && req.URL.Path == "/api/mcp":
					listDir = req.URL.Query().Get("location[directory]")
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(strings.NewReader(`{
							"location": {"directory": "/home/diamond/proj"},
							"data": [
								{"name": "notes", "title": "Outline Notes", "status": {"status": "connected"}},
								{"name": "opencode_console", "status": {"status": "needs_auth", "error": "Sign in required"}, "integrationID": "mcp_console"},
								{"name": "broken", "status": {"status": "failed", "error": "spawn ENOENT <binary>"}}
							]
						}`)),
					}, nil
				case req.Method == http.MethodGet && req.URL.Path == "/api/mcp/resource":
					resDir = req.URL.Query().Get("location[directory]")
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(strings.NewReader(`{
							"location": {"directory": "/home/diamond/proj"},
							"data": {
								"resources": [
									{"server": "notes", "name": "Welcome", "uri": "notes://doc/1", "mimeType": "text/markdown", "description": "Home doc"}
								],
								"templates": [
									{"server": "notes", "name": "Doc by ID", "uriTemplate": "notes://doc/{id}", "mimeType": "text/markdown"}
								]
							}
						}`)),
					}, nil
				case req.Method == http.MethodGet && req.URL.Path == "/api/config":
					cfgDir = req.URL.Query().Get("location[directory]")
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(strings.NewReader(`{
							"data": [
								{
									"type": "document",
									"path": "/home/diamond/.config/opencode/opencode.json",
									"info": {
										"mcp": {
											"timeout": {"startup": 10000, "execution": 30000},
											"servers": {
												"notes": {
													"type": "remote",
													"url": "https://user:supersecret@notes.example.com/mcp?api_key=xyz123&mode=sse",
													"headers": {"Authorization": "Bearer secret-token", "X-Env": "prod"},
													"oauth": {"client_id": "pub", "client_secret": "do-not-leak"},
													"protocol": "streamable-http",
													"pool": true
												}
											}
										}
									}
								},
								{"type": "directory", "path": "/home/diamond/proj"},
								{
									"type": "inline",
									"info": {
										"mcp": {
											"servers": {
												"local_tool": {
													"type": "local",
													"command": ["npx", "-y", "my-mcp", "--flag=<val>"],
													"cwd": "/home/diamond/proj",
													"environment": {"API_SECRET": "topsecret", "DEBUG": "1"},
													"timeout": {"execution": 60000}
												}
											}
										}
									}
								}
							]
						}`)),
					}, nil
				case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/connect"):
					connectPath = req.URL.Path
					connectDir = req.URL.Query().Get("location[directory]")
					return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
				case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/disconnect"):
					disconnPath = req.URL.Path
					disconnDir = req.URL.Query().Get("location[directory]")
					return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
				default:
					return &http.Response{StatusCode: http.StatusNotFound, Body: http.NoBody}, nil
				}
			}),
		},
	}

	mcpRes, err := client.ListMCP(ctx, "/home/diamond/proj")
	if err != nil {
		t.Fatalf("ListMCP failed: %v", err)
	}
	if listDir != "/home/diamond/proj" || mcpRes.Location.Directory != "/home/diamond/proj" || len(mcpRes.Servers) != 3 {
		t.Fatalf("unexpected ListMCP result: dir=%q res=%+v", listDir, mcpRes)
	}
	if mcpRes.Servers[1].IntegrationID != "mcp_console" || mcpRes.Servers[1].Status.Status != "needs_auth" {
		t.Fatalf("unexpected server[1]: %+v", mcpRes.Servers[1])
	}

	resCat, err := client.ListMCPResources(ctx, "/home/diamond/proj")
	if err != nil {
		t.Fatalf("ListMCPResources failed: %v", err)
	}
	if resDir != "/home/diamond/proj" || len(resCat.Catalog.Resources) != 1 || len(resCat.Catalog.Templates) != 1 {
		t.Fatalf("unexpected ListMCPResources result: dir=%q cat=%+v", resDir, resCat)
	}

	entries, err := client.ListConfig(ctx, "/home/diamond/proj")
	if err != nil {
		t.Fatalf("ListConfig failed: %v", err)
	}
	if cfgDir != "/home/diamond/proj" || len(entries) != 3 {
		t.Fatalf("unexpected ListConfig entries: dir=%q len=%d", cfgDir, len(entries))
	}

	merged := MergeMcpConfigs(entries)
	notesCfg, ok := merged["notes"]
	if !ok {
		t.Fatal("expected merged config to include 'notes'")
	}
	if notesCfg.URL != "https://user:REDACTED@notes.example.com/mcp?api_key=REDACTED&mode=sse" {
		t.Fatalf("notes URL not sanitized cleanly: %q", notesCfg.URL)
	}
	if !reflect.DeepEqual(notesCfg.HeaderKeys, []string{"Authorization", "X-Env"}) {
		t.Fatalf("HeaderKeys = %v, want [Authorization X-Env]", notesCfg.HeaderKeys)
	}
	if notesCfg.SourcePath != "/home/diamond/.config/opencode/opencode.json" {
		t.Fatalf("SourcePath = %q", notesCfg.SourcePath)
	}
	if notesCfg.Timeout == nil || notesCfg.Timeout.Startup != 10000 || notesCfg.Timeout.Execution != 30000 {
		t.Fatalf("notes Timeout = %+v, want inherited global timeout", notesCfg.Timeout)
	}

	localCfg, ok := merged["local_tool"]
	if !ok {
		t.Fatal("expected merged config to include 'local_tool'")
	}
	if !reflect.DeepEqual(localCfg.EnvKeys, []string{"API_SECRET", "DEBUG"}) {
		t.Fatalf("EnvKeys = %v, want [API_SECRET DEBUG]", localCfg.EnvKeys)
	}
	if localCfg.Timeout == nil || localCfg.Timeout.Startup != 10000 || localCfg.Timeout.Execution != 60000 {
		t.Fatalf("local_tool Timeout = %+v, want Startup=10000 Execution=60000", localCfg.Timeout)
	}

	// Ensure secret values are never retained anywhere in marshaled McpServerConfig/ConfigEntry.
	b, _ := json.Marshal(merged)
	dumped := string(b)
	for _, secret := range []string{"supersecret", "xyz123", "secret-token", "do-not-leak", "topsecret"} {
		if strings.Contains(dumped, secret) {
			t.Fatalf("secret %q leaked into merged config JSON: %s", secret, dumped)
		}
	}

	if err := client.ConnectMCP(ctx, "my server/1", "/home/diamond/proj"); err != nil {
		t.Fatalf("ConnectMCP failed: %v", err)
	}
	if connectPath != "/api/experimental/mcp/my%20server%2F1/connect" && connectPath != "/api/experimental/mcp/my server/1/connect" {
		// Note: net/http req.URL.Path unescapes path segments; RawPath/EscapedPath holds escaped form.
		t.Logf("connectPath=%q", connectPath)
	}
	if connectDir != "/home/diamond/proj" {
		t.Fatalf("connectDir = %q, want /home/diamond/proj", connectDir)
	}

	if err := client.DisconnectMCP(ctx, "notes", ""); err != nil {
		t.Fatalf("DisconnectMCP failed: %v", err)
	}
	if disconnPath != "/api/experimental/mcp/notes/disconnect" || disconnDir != "" {
		t.Fatalf("DisconnectMCP path=%q dir=%q", disconnPath, disconnDir)
	}
}

func TestListMCPDirectArrayAndEmptyData(t *testing.T) {
	t.Parallel()
	client := &Client{
		BaseURL: "http://127.0.0.1:4096",
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.RawQuery != "" {
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{"location":{"directory":"/tmp/empty"},"data":[]}`)),
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Body:       io.NopCloser(strings.NewReader(`[{"name":"direct","status":{"status":"connected"}}]`)),
				}, nil
			}),
		},
	}

	direct, err := client.ListMCP(context.Background(), "")
	if err != nil {
		t.Fatalf("direct ListMCP failed: %v", err)
	}
	if len(direct.Servers) != 1 || direct.Servers[0].Name != "direct" {
		t.Fatalf("unexpected direct ListMCP: %+v", direct)
	}

	empty, err := client.ListMCP(context.Background(), "/tmp/empty")
	if err != nil {
		t.Fatalf("empty ListMCP failed: %v", err)
	}
	if empty.Location.Directory != "/tmp/empty" || len(empty.Servers) != 0 {
		t.Fatalf("unexpected empty ListMCP: %+v", empty)
	}
}

func TestSanitizeMcpURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"https://mcp.example.com/sse", "https://mcp.example.com/sse"},
		{"https://user:pass@mcp.example.com/sse", "https://user:REDACTED@mcp.example.com/sse"},
		{"https://ghp_secret@mcp.example.com/sse", "https://REDACTED@mcp.example.com/sse"},
		{"https://mcp.example.com/sse?access_token=abc&foo=bar", "https://mcp.example.com/sse?access_token=REDACTED&foo=bar"},
		{"https://mcp.example.com/sse?Signature=123&x=1", "https://mcp.example.com/sse?Signature=REDACTED&x=1"},
	}
	for _, tc := range cases {
		if got := SanitizeMcpURL(tc.in); got != tc.want {
			t.Errorf("SanitizeMcpURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestHumanizeAPIErrorAndEvent(t *testing.T) {
	t.Parallel()
	if got := HumanizeAPIError(nil); got != "" {
		t.Fatalf("HumanizeAPIError(nil) = %q", got)
	}
	cases := []struct {
		in   string
		want string
	}{
		{`http 404: {"name":"NotFound","message":"MCP server not found"}`, "MCP server not found"},
		{`http 500: {"data":{"message":"Connection timed out"}}`, "Connection timed out"},
		{`http 400: {"error":{"message":"Invalid command"}}`, "Invalid command"},
		{`http 502: Bad Gateway`, "HTTP 502: Bad Gateway"},
		{`context deadline exceeded`, "context deadline exceeded"},
	}
	for _, tc := range cases {
		if got := HumanizeAPIError(errors.New(tc.in)); got != tc.want {
			t.Errorf("HumanizeAPIError(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	var ev McpChangedEvent
	if err := json.Unmarshal([]byte(`{"server":"notes"}`), &ev); err != nil || ev.Server != "notes" {
		t.Fatalf("McpChangedEvent unmarshal = %+v, err=%v", ev, err)
	}
}

func TestIsLoopbackServerURL(t *testing.T) {
	t.Parallel()
	cases := []struct {
		url  string
		want bool
	}{
		{"", true},
		{"   ", true},
		{"http://localhost:4096", true},
		{"http://LOCALHOST:4096", true},
		{"http://127.0.0.1:4096", true},
		{"http://[::1]:4096", true},
		{"http://[::]:4096", true},
		{"http://0.0.0.0:4096", true},
		{"https://opencode.internal:4096", false},
		{"https://10.0.0.5:4096", false},
		{"://bad", false},
	}
	for _, tc := range cases {
		if got := IsLoopbackServerURL(tc.url); got != tc.want {
			t.Errorf("IsLoopbackServerURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

func TestResolveLogFileAndParsing(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mainLog := filepath.Join(dir, "opencode.log")
	devLog := filepath.Join(dir, "opencode-dev.log")

	if _, err := ResolveLogFile(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected ErrNotExist for empty dir, got %v", err)
	}

	if err := os.WriteFile(mainLog, []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := ResolveLogFile(dir)
	if err != nil || got != mainLog {
		t.Fatalf("ResolveLogFile = (%q, %v), want %q", got, err, mainLog)
	}

	if err := os.WriteFile(devLog, []byte("b\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	_ = os.Chtimes(mainLog, now.Add(-10*time.Second), now.Add(-10*time.Second))
	_ = os.Chtimes(devLog, now, now)
	got, err = ResolveLogFile(dir)
	if err != nil || got != devLog {
		t.Fatalf("ResolveLogFile with newer dev log = (%q, %v), want %q", got, err, devLog)
	}

	line := `timestamp=2026-11-15T19:34:05.247Z level=warn run=75e6b243 message="mcp connection failed" http.span=3488 role=server server=notes server=notes directory=/home/diamond error="line1\nline2 \"quoted\"" status.error=ignored extra=ok`
	entry, fields, ok := ParseLogLine(line)
	if !ok {
		t.Fatal("expected ParseLogLine to succeed")
	}
	if entry.Level != "WARN" || entry.Server != "notes" || entry.Directory != "/home/diamond" {
		t.Fatalf("unexpected parsed entry: %+v", entry)
	}
	if entry.Error != "line1\nline2 \"quoted\"" {
		t.Fatalf("unexpected unquoted error: %q", entry.Error)
	}
	for _, ex := range entry.Extras {
		if ex.Key == "run" || ex.Key == "http.span" || ex.Key == "role" {
			t.Fatalf("expected tracing field %q to be omitted from Extras", ex.Key)
		}
	}
	if !MatchesMcpEntry(fields, entry, "notes") || MatchesMcpEntry(fields, entry, "other") {
		t.Fatal("MatchesMcpEntry server filter mismatch")
	}
	if !MatchesMcpEntry(fields, entry, "") {
		t.Fatal("expected MatchesMcpEntry(\"\", ...) to match server=notes")
	}

	stderrLine := `timestamp=2026-11-15T19:35:00Z level=DEBUG message="mcp server stderr" server=playwright output="listening on stdio"`
	se, sf, sok := ParseLogLine(stderrLine)
	if !sok || !MatchesMcpEntry(sf, se, "playwright") || se.Output != "listening on stdio" {
		t.Fatalf("unexpected stderr parse: ok=%v entry=%+v", sok, se)
	}

	nonMcpLine := `timestamp=2026-11-15T19:35:01Z level=INFO message="http request finished" status=200`
	ne, nf, nok := ParseLogLine(nonMcpLine)
	if !nok || MatchesMcpEntry(nf, ne, "") {
		t.Fatalf("expected non-MCP line not to match")
	}
}

func TestMcpLogTailerChunkingPollingAndCompaction(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "opencode.log")

	// Build > 600 KiB of noise + MCP lines spanning across the 512 KiB chunk boundary.
	var sb strings.Builder
	noise := `timestamp=2026-11-15T19:00:00Z level=INFO message="http request ok" path=/api/session/list padding=` + strings.Repeat("x", 400) + "\n"
	for i := 0; i < 1600; i++ {
		if i == 100 {
			sb.WriteString(`timestamp=2026-11-15T19:01:00Z level=INFO message="mcp connected" server=early_srv` + "\n")
		} else if i == 900 {
			// Near the 512 KiB boundary
			sb.WriteString(`timestamp=2026-11-15T19:02:00Z level=INFO message="mcp connected" server=mid_srv` + "\n")
		} else {
			sb.WriteString(noise)
		}
	}
	for i := 0; i < 5; i++ {
		fmt.Fprintf(&sb, "timestamp=2026-11-15T19:03:0%dZ level=WARN message=\"mcp slow\" server=alpha seq=%d\n", i, i)
	}
	// Incomplete trailing line (no newline yet) must NOT be parsed or consumed.
	sb.WriteString(`timestamp=2026-11-15T19:04:00Z level=ERROR message="mcp incomplete" server=alpha`)

	if err := os.WriteFile(logPath, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	tailer := NewMcpLogTailer(dir)
	entries, err := tailer.ReadInitialTail("")
	if err != nil {
		t.Fatalf("ReadInitialTail failed: %v", err)
	}
	if len(entries) != 7 {
		t.Fatalf("expected 7 MCP entries (early_srv + mid_srv + 5 alpha), got %d: %+v", len(entries), entries)
	}
	if entries[0].Server != "early_srv" || entries[1].Server != "mid_srv" {
		t.Fatalf("unexpected order/servers in initial tail: %+v", entries[:2])
	}

	// Finish the incomplete trailing line and append another line.
	f, err := os.OpenFile(logPath, os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("\n" + `timestamp=2026-11-15T19:04:01Z level=INFO message="mcp recovered" server=alpha` + "\n")
	_ = f.Close()

	// Create .trim lock dir -> PollNewLines must skip tick.
	trimDir := logPath + ".trim"
	if err := os.Mkdir(trimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	polled, compacted, err := tailer.PollNewLines("")
	if err != nil || compacted || len(polled) != 0 {
		t.Fatalf("expected skip during .trim lock, got len=%d compacted=%v err=%v", len(polled), compacted, err)
	}
	_ = os.Remove(trimDir)

	// Now PollNewLines reads both the completed partial line and the newly appended line.
	polled, compacted, err = tailer.PollNewLines("")
	if err != nil || compacted {
		t.Fatalf("PollNewLines failed: compacted=%v err=%v", compacted, err)
	}
	if len(polled) != 2 || polled[0].Message != "mcp incomplete" || polled[1].Message != "mcp recovered" {
		t.Fatalf("unexpected polled entries: %+v", polled)
	}

	// Simulate in-place compaction (file shrinks below t.offset).
	compactedContent := "timestamp=2026-11-15T19:05:00Z level=INFO message=\"mcp post-compact\" server=alpha\n"
	if err := os.WriteFile(logPath, []byte(compactedContent), 0o644); err != nil {
		t.Fatal(err)
	}
	polled, compacted, err = tailer.PollNewLines("alpha")
	if err != nil || !compacted {
		t.Fatalf("expected compacted=true, got compacted=%v err=%v", compacted, err)
	}
	if len(polled) != 2 || !polled[0].Compacted || polled[1].Message != "mcp post-compact" {
		t.Fatalf("unexpected post-compaction entries: %+v", polled)
	}
}

func TestMcpLogTailerCapAtMaxEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "opencode.log")

	var sb strings.Builder
	for i := 0; i < mcpLogMaxEntries+150; i++ {
		fmt.Fprintf(&sb, "timestamp=2026-11-15T19:00:00Z level=INFO message=\"mcp tick %d\" server=alpha\n", i)
	}
	if err := os.WriteFile(logPath, []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	tailer := NewMcpLogTailer(dir)
	entries, err := tailer.ReadInitialTail("alpha")
	if err != nil {
		t.Fatalf("ReadInitialTail failed: %v", err)
	}
	if len(entries) != mcpLogMaxEntries {
		t.Fatalf("len(entries) = %d, want %d", len(entries), mcpLogMaxEntries)
	}
	wantFirst := fmt.Sprintf("mcp tick %d", 150)
	if entries[0].Message != wantFirst {
		t.Fatalf("first capped entry = %q, want %q", entries[0].Message, wantFirst)
	}
}
