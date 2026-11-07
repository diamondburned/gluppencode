package opencode

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func TestFormatContextUsage(t *testing.T) {
	t.Parallel()

	makeTokens := func(input, cacheRead, output int) TokenUsage {
		var u TokenUsage
		u.Input = input
		u.Output = output
		u.Cache.Read = cacheRead
		return u
	}

	tests := []struct {
		name          string
		messages      []Message
		sessionTokens TokenUsage
		contextLimit  int
		want          string
	}{
		{
			name:          "zero tokens no limit",
			messages:      nil,
			sessionTokens: TokenUsage{},
			contextLimit:  0,
			want:          "0K",
		},
		{
			name:          "zero tokens with limit",
			messages:      nil,
			sessionTokens: TokenUsage{},
			contextLimit:  200000,
			want:          "0K",
		},
		{
			name:          "under 500 tokens without limit",
			messages:      []Message{{Type: "assistant", Tokens: makeTokens(300, 100, 50)}},
			sessionTokens: TokenUsage{},
			contextLimit:  0,
			want:          "<1K",
		},
		{
			name:          "under 500 tokens with limit",
			messages:      []Message{{Type: "assistant", Tokens: makeTokens(400, 50, 20)}},
			sessionTokens: TokenUsage{},
			contextLimit:  1000,
			want:          "<1K (45%)",
		},
		{
			name:          "single message without limit",
			messages:      []Message{{Type: "assistant", Tokens: makeTokens(25000, 54000, 500)}},
			sessionTokens: TokenUsage{},
			contextLimit:  0,
			want:          "79K",
		},
		{
			name:          "single message with limit",
			messages:      []Message{{Type: "assistant", Tokens: makeTokens(25000, 54000, 500)}},
			sessionTokens: TokenUsage{},
			contextLimit:  180000,
			want:          "79K (44%)",
		},
		{
			name: "multi-message timeline picks latest assistant message with tokens",
			messages: []Message{
				{ID: "m1", Type: "user", Text: "hello"},
				{ID: "m2", Type: "assistant", Tokens: makeTokens(10000, 20000, 300)},
				{ID: "m3", Type: "user", Text: "next"},
				{ID: "m4", Type: "assistant", Tokens: makeTokens(15000, 45000, 400)},
				{ID: "m5", Type: "assistant", Tokens: makeTokens(0, 0, 0)}, // in-progress step without token count yet
				{ID: "m6", Type: "user", Tokens: makeTokens(99999, 99999, 0)},
			},
			sessionTokens: makeTokens(100000, 200000, 1000),
			contextLimit:  100000,
			want:          "60K (60%)",
		},
		{
			name: "fallback to session tokens when messages have no tokens",
			messages: []Message{
				{ID: "m1", Type: "user", Text: "hi"},
				{ID: "m2", Type: "assistant", Text: "streaming..."},
			},
			sessionTokens: makeTokens(30200, 49600, 500),
			contextLimit:  200000,
			want:          "80K (40%)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FormatContextUsage(tt.messages, tt.sessionTokens, tt.contextLimit)
			if got != tt.want {
				t.Errorf("FormatContextUsage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractToolModification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		toolName  string
		rawInput  any
		wantPath  string
		wantLines []string
		wantMod   bool
	}{
		{
			name:     "edit tool with path and oldString/newString",
			toolName: "edit",
			rawInput: map[string]any{
				"path":      "internal/opencode/client.go",
				"oldString": "func old() {\n\treturn 1\n}",
				"newString": "func old() {\n\treturn 2\n}",
			},
			wantPath: "internal/opencode/client.go",
			wantLines: []string{
				"--- a/internal/opencode/client.go",
				"+++ b/internal/opencode/client.go",
				" func old() {",
				"-\treturn 1",
				"+\treturn 2",
				" }",
			},
			wantMod: true,
		},
		{
			name:     "edit tool via json.RawMessage with filePath and old_str/new_str",
			toolName: "edit",
			rawInput: json.RawMessage(`{"filePath":"main.go","old_str":"a\nb","new_str":"a\nc"}`),
			wantPath: "main.go",
			wantLines: []string{
				"--- a/main.go",
				"+++ b/main.go",
				" a",
				"-b",
				"+c",
			},
			wantMod: true,
		},
		{
			name:     "write tool produces plus lines",
			toolName: "write",
			rawInput: map[string]any{
				"path":    "hello.txt",
				"content": "line 1\nline 2\n",
			},
			wantPath: "hello.txt",
			wantLines: []string{
				"--- /dev/null",
				"+++ b/hello.txt",
				"+line 1",
				"+line 2",
			},
			wantMod: true,
		},
		{
			name:     "multiedit tool combines edits",
			toolName: "multiedit",
			rawInput: map[string]any{
				"path": "pkg/foo.go",
				"edits": []any{
					map[string]any{"oldString": "x := 1", "newString": "x := 2"},
					map[string]any{"oldString": "y := 3", "newString": "y := 4"},
				},
			},
			wantPath: "pkg/foo.go",
			wantLines: []string{
				"--- a/pkg/foo.go",
				"+++ b/pkg/foo.go",
				"-x := 1",
				"+x := 2",
				"@@ ... @@",
				"-y := 3",
				"+y := 4",
			},
			wantMod: true,
		},
		{
			name:     "apply_patch extracts file header",
			toolName: "apply_patch",
			rawInput: map[string]any{
				"patch": "*** Begin Patch\n*** Update File: src/app.go\n@@\n-old\n+new",
			},
			wantPath: "src/app.go",
			wantLines: []string{
				"*** Update File: src/app.go",
				"-old",
				"+new",
			},
			wantMod: true,
		},
		{
			name:      "non-file tool shell returns false",
			toolName:  "shell",
			rawInput:  map[string]any{"command": "go test ./..."},
			wantPath:  "",
			wantLines: nil,
			wantMod:   false,
		},
		{
			name:      "non-file tool read returns false",
			toolName:  "read",
			rawInput:  map[string]any{"path": "README.md"},
			wantPath:  "",
			wantLines: nil,
			wantMod:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotPath, gotDiff, gotMod := ExtractToolModification(tt.toolName, tt.rawInput)
			if gotMod != tt.wantMod {
				t.Fatalf("ExtractToolModification() isFileMod = %v, want %v", gotMod, tt.wantMod)
			}
			if gotPath != tt.wantPath {
				t.Errorf("ExtractToolModification() filePath = %q, want %q", gotPath, tt.wantPath)
			}
			for _, wantLine := range tt.wantLines {
				if !strings.Contains(gotDiff, wantLine) {
					t.Errorf("ExtractToolModification() diff missing line %q in:\n%s", wantLine, gotDiff)
				}
			}
		})
	}
}

func TestAgentColor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name            string
		agentID         string
		configuredColor string
		want            string
	}{
		{
			name:            "build default",
			agentID:         "build",
			configuredColor: "",
			want:            "#3b82f6",
		},
		{
			name:            "plan default",
			agentID:         "plan",
			configuredColor: "",
			want:            "#a855f7",
		},
		{
			name:            "bare default",
			agentID:         "bare",
			configuredColor: "",
			want:            "#10b981",
		},
		{
			name:            "orchestrator default",
			agentID:         "orchestrator",
			configuredColor: "",
			want:            "#f59e0b",
		},
		{
			name:            "configured color overrides known agent",
			agentID:         "build",
			configuredColor: "#ff0055",
			want:            "#ff0055",
		},
		{
			name:            "configured color overrides custom agent",
			agentID:         "custom-agent",
			configuredColor: "#123456",
			want:            "#123456",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := AgentColor(tt.agentID, tt.configuredColor)
			if got != tt.want {
				t.Errorf("AgentColor(%q, %q) = %q, want %q", tt.agentID, tt.configuredColor, got, tt.want)
			}
		})
	}

	t.Run("deterministic fallback from palette", func(t *testing.T) {
		t.Parallel()
		for _, id := range []string{"explore", "general", "researcher", "auditor"} {
			c1 := AgentColor(id, "")
			c2 := AgentColor(id, "")
			if c1 != c2 {
				t.Errorf("AgentColor(%q) not deterministic: %q != %q", id, c1, c2)
			}
			found := false
			for _, p := range defaultAgentPalette {
				if c1 == p {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("AgentColor(%q) = %q, not in defaultAgentPalette %v", id, c1, defaultAgentPalette)
			}
		}
	})
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestSSESubscribeAndReconnectSynctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		var (
			mu       sync.Mutex
			attempts int
			received []Event
		)

		client := &Client{
			BaseURL: "http://opencode.local",
			HTTP: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.Path != "/api/event" {
						return &http.Response{
							StatusCode: http.StatusNotFound,
							Body:       io.NopCloser(strings.NewReader("not found")),
						}, nil
					}
					mu.Lock()
					attempts++
					attempt := attempts
					mu.Unlock()

					pr, pw := io.Pipe()
					go func() {
						defer pw.Close()
						switch attempt {
						case 1:
							// Emit heartbeat, comment, and first batch of events, then drop connection to trigger reconnect.
							_, _ = io.WriteString(pw, ": heartbeat\n\n")
							_, _ = io.WriteString(pw, "data: {\"id\":\"evt_1\",\"type\":\"session.agent.selected\",\"data\":{\"sessionID\":\"ses_1\",\"agent\":\"plan\"}}\n\n")
							_, _ = io.WriteString(pw, "id: evt_2\nevent: session.model.selected\ndata: {\"sessionID\":\"ses_1\",\"model\":{\"id\":\"claude-4\",\"providerID\":\"anthropic\"}}\n\n")
						case 2:
							// Second connection emits step.ended and session.created, then blocks until ctx is canceled.
							_, _ = io.WriteString(pw, "data: {\"id\":\"evt_3\",\"type\":\"session.step.ended\",\"data\":{\"sessionID\":\"ses_1\",\"assistantMessageID\":\"msg_1\",\"cost\":0.015,\"tokens\":{\"input\":1200,\"output\":300,\"reasoning\":50,\"cache\":{\"read\":4000,\"write\":0}}}}\n\n")
							_, _ = io.WriteString(pw, "data: {\"id\":\"evt_4\",\"type\":\"session.created\",\"data\":{\"session\":{\"id\":\"ses_2\",\"title\":\"New Session\",\"agent\":\"build\"}}}\n\n")
							<-req.Context().Done()
						}
					}()

					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       pr,
					}, nil
				}),
			},
		}

		done := make(chan error, 1)
		go func() {
			done <- client.SubscribeEventsLoop(ctx, 2*time.Second, func(ev Event) {
				mu.Lock()
				received = append(received, ev)
				count := len(received)
				mu.Unlock()
				if count == 4 {
					cancel()
				}
			}, nil)
		}()

		// Wait for attempt 1 to complete and enter the 2s retry timer.
		synctest.Wait()
		// Advance fake time by 2s so SubscribeEventsLoop reconnects for attempt 2.
		time.Sleep(2 * time.Second)
		synctest.Wait()

		err := <-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("SubscribeEventsLoop returned %v, want context.Canceled", err)
		}

		mu.Lock()
		defer mu.Unlock()
		if attempts != 2 {
			t.Fatalf("attempts = %d, want 2", attempts)
		}
		if len(received) != 4 {
			t.Fatalf("len(received) = %d, want 4", len(received))
		}

		var agentEv AgentSelectedEvent
		if err := json.Unmarshal(received[0].Data, &agentEv); err != nil {
			t.Fatalf("unmarshal AgentSelectedEvent: %v", err)
		}
		if agentEv.SessionID != "ses_1" || agentEv.Agent != "plan" {
			t.Errorf("AgentSelectedEvent = %+v, want sessionID=ses_1 agent=plan", agentEv)
		}

		var modelEv ModelSelectedEvent
		if err := json.Unmarshal(received[1].Data, &modelEv); err != nil {
			t.Fatalf("unmarshal ModelSelectedEvent: %v", err)
		}
		if modelEv.SessionID != "ses_1" || modelEv.Model.String() != "anthropic/claude-4" {
			t.Errorf("ModelSelectedEvent = %+v, want sessionID=ses_1 model=anthropic/claude-4", modelEv)
		}

		var stepEv StepEndedEvent
		if err := json.Unmarshal(received[2].Data, &stepEv); err != nil {
			t.Fatalf("unmarshal StepEndedEvent: %v", err)
		}
		if stepEv.SessionID != "ses_1" || stepEv.Tokens.Input != 1200 || stepEv.Tokens.Cache.Read != 4000 {
			t.Errorf("StepEndedEvent = %+v", stepEv)
		}

		var createdEv SessionCreatedEvent
		if err := json.Unmarshal(received[3].Data, &createdEv); err != nil {
			t.Fatalf("unmarshal SessionCreatedEvent: %v", err)
		}
		if createdEv.SessionID != "ses_2" || createdEv.Session.ID != "ses_2" || createdEv.Session.Title != "New Session" {
			t.Errorf("SessionCreatedEvent = %+v", createdEv)
		}
	})
}

func TestSubscribeEventsLoopImmediateCleanEOFNoBusySpin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()

		var (
			mu       sync.Mutex
			attempts int
		)

		client := &Client{
			BaseURL: "http://opencode.local",
			HTTP: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					mu.Lock()
					attempts++
					mu.Unlock()

					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				}),
			},
		}

		done := make(chan error, 1)
		go func() {
			done <- client.SubscribeEventsLoop(ctx, 2*time.Second, func(ev Event) {}, nil)
		}()

		// Wait for attempt 1 to complete and pause at the 2s retry timer
		synctest.Wait()

		mu.Lock()
		att1 := attempts
		mu.Unlock()

		if att1 != 1 {
			t.Fatalf("attempts = %d after clean EOF, want 1 (should be waiting retry delay)", att1)
		}

		// Advance time by 2s -> attempt 2
		time.Sleep(2 * time.Second)
		synctest.Wait()

		mu.Lock()
		att2 := attempts
		mu.Unlock()

		if att2 != 2 {
			t.Fatalf("attempts = %d after 2s delay, want 2", att2)
		}

		cancel()
		err := <-done
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got err %v, want context.Canceled", err)
		}
	})
}

func TestClientEndpointsSynctest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := t.Context()

		client := &Client{
			BaseURL:  "http://opencode.local",
			Password: "secret",
			HTTP: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					user, pass, ok := req.BasicAuth()
					if !ok || user != "opencode" || pass != "secret" {
						return &http.Response{
							StatusCode: http.StatusUnauthorized,
							Body:       io.NopCloser(strings.NewReader("unauthorized")),
						}, nil
					}

					var respBody string
					switch {
					case req.Method == http.MethodGet && req.URL.Path == "/api/session/ses_123":
						respBody = `{"data":{"id":"ses_123","projectID":"proj_1","title":"Test Session","agent":"build","model":{"id":"gemini-4","providerID":"google"},"tokens":{"input":1000,"output":200,"reasoning":50,"cache":{"read":3000,"write":0}},"time":{"created":100,"updated":200},"location":{"directory":"/workspace"}}}`
					case req.Method == http.MethodGet && req.URL.Path == "/api/agent":
						respBody = `{"location":{"directory":"/workspace"},"data":[{"id":"build","name":"Build","mode":"primary","hidden":false},{"id":"plan","name":"Plan","mode":"primary","hidden":false},{"id":"explore","name":"Explore","mode":"subagent","hidden":false},{"id":"title","name":"Title","mode":"primary","hidden":true}]}`
					case req.Method == http.MethodPost && req.URL.Path == "/api/session/ses_123/agent":
						b, _ := io.ReadAll(req.Body)
						if !strings.Contains(string(b), `"agent":"plan"`) {
							t.Errorf("SwitchAgent body = %s", string(b))
						}
						return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
					case req.Method == http.MethodGet && req.URL.Path == "/api/model":
						respBody = `{"location":{"directory":"/workspace"},"data":[{"id":"gemini-4","modelID":"gemini-4","providerID":"google","name":"Gemini 4","family":"gemini","type":"chat","status":"active","enabled":true,"limit":{"context":1000000,"input":900000,"output":65536}}]}`
					case req.Method == http.MethodPost && req.URL.Path == "/api/session/ses_123/model":
						b, _ := io.ReadAll(req.Body)
						if !strings.Contains(string(b), `"providerID":"google"`) {
							t.Errorf("SwitchModel body = %s", string(b))
						}
						return &http.Response{StatusCode: http.StatusNoContent, Body: io.NopCloser(strings.NewReader(""))}, nil
					case req.Method == http.MethodGet && req.URL.Path == "/api/session/ses_123/diff":
						respBody = `{"data":[{"file":"internal/opencode/client.go","patch":"--- a/internal/opencode/client.go\n+++ b/internal/opencode/client.go\n@@ -1 +1 @@\n-old\n+new","additions":1,"deletions":1,"status":"modified"}]}`
					default:
						return &http.Response{
							StatusCode: http.StatusNotFound,
							Body:       io.NopCloser(strings.NewReader("not found")),
						}, nil
					}

					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(respBody)),
					}, nil
				}),
			},
		}

		sess, err := client.GetSession(ctx, "ses_123")
		if err != nil {
			t.Fatalf("GetSession: %v", err)
		}
		if sess.ID != "ses_123" || sess.ProjectID != "proj_1" || sess.Tokens.Cache.Read != 3000 {
			t.Errorf("GetSession = %+v", sess)
		}

		agents, err := client.ListAgents(ctx, "/workspace")
		if err != nil {
			t.Fatalf("ListAgents: %v", err)
		}
		primary := PrimaryAgents(agents)
		if len(primary) != 2 || primary[0].ID != "build" || primary[1].ID != "plan" {
			t.Errorf("PrimaryAgents = %+v, want [build, plan]", primary)
		}

		if err := client.SwitchAgent(ctx, "ses_123", "plan"); err != nil {
			t.Fatalf("SwitchAgent: %v", err)
		}

		models, err := client.ListModels(ctx)
		if err != nil {
			t.Fatalf("ListModels: %v", err)
		}
		if len(models) != 1 || models[0].Limit.Context != 1000000 {
			t.Errorf("ListModels = %+v", models)
		}

		if err := client.SwitchModel(ctx, "ses_123", models[0].Ref()); err != nil {
			t.Fatalf("SwitchModel: %v", err)
		}

		diffs, err := client.SessionDiff(ctx, "ses_123")
		if err != nil {
			t.Fatalf("SessionDiff: %v", err)
		}
		if len(diffs) != 1 || diffs[0].File != "internal/opencode/client.go" || diffs[0].Additions != 1 {
			t.Errorf("SessionDiff = %+v", diffs)
		}
	})
}

func TestListMessagesPage(t *testing.T) {
	t.Parallel()

	t.Run("initial page load without cursor", func(t *testing.T) {
		t.Parallel()
		var capturedURL *http.Request
		client := &Client{
			BaseURL: "http://opencode.local",
			HTTP: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					capturedURL = req
					respBody := `{
						"cursor": {
							"previous": "cur_newer",
							"next": "cur_older"
						},
						"data": [
							{"id": "msg_2", "text": "hello newer"},
							{"id": "msg_1", "text": "hello older"}
						]
					}`
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(respBody)),
					}, nil
				}),
			},
		}

		page, err := client.ListMessagesPage(context.Background(), "ses_abc", 0, "")
		if err != nil {
			t.Fatalf("ListMessagesPage failed: %v", err)
		}

		if capturedURL == nil {
			t.Fatal("request was not captured")
		}
		q := capturedURL.URL.Query()
		if got := q.Get("order"); got != "desc" {
			t.Errorf("query order = %q, want desc", got)
		}
		if got := q.Get("limit"); got != "30" {
			t.Errorf("query limit = %q, want 30 (default)", got)
		}
		if q.Has("cursor") {
			t.Errorf("query contains cursor when empty cursor was provided")
		}

		if page.NextCursor != "cur_older" {
			t.Errorf("NextCursor = %q, want cur_older", page.NextCursor)
		}
		if len(page.Messages) != 2 {
			t.Fatalf("len(Messages) = %d, want 2", len(page.Messages))
		}
		if page.Messages[0].ID != "msg_1" || page.Messages[1].ID != "msg_2" {
			t.Errorf("Messages not reversed to chronological order: got [%s, %s], want [msg_1, msg_2]",
				page.Messages[0].ID, page.Messages[1].ID)
		}
	})

	t.Run("limit capped at 200", func(t *testing.T) {
		t.Parallel()
		var capturedLimit string
		client := &Client{
			BaseURL: "http://opencode.local",
			HTTP: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					capturedLimit = req.URL.Query().Get("limit")
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(`{"cursor": null, "data": []}`)),
					}, nil
				}),
			},
		}

		_, err := client.ListMessagesPage(context.Background(), "ses_abc", 500, "")
		if err != nil {
			t.Fatalf("ListMessagesPage failed: %v", err)
		}
		if capturedLimit != "200" {
			t.Errorf("captured limit = %q, want 200", capturedLimit)
		}
	})

	t.Run("older history with cursor omits order param", func(t *testing.T) {
		t.Parallel()
		var capturedReq *http.Request
		client := &Client{
			BaseURL: "http://opencode.local",
			HTTP: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					capturedReq = req
					respBody := `{
						"cursor": null,
						"data": [
							{"id": "msg_0", "text": "earliest"}
						]
					}`
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(respBody)),
					}, nil
				}),
			},
		}

		page, err := client.ListMessagesPage(context.Background(), "ses_abc", 50, "cur_older_token")
		if err != nil {
			t.Fatalf("ListMessagesPage with cursor failed: %v", err)
		}

		if capturedReq == nil {
			t.Fatal("request was not captured")
		}
		q := capturedReq.URL.Query()
		if q.Has("order") {
			t.Errorf("query unexpectedly contains order=%q when cursor is set (crucial: must NOT pass order=desc with cursor)", q.Get("order"))
		}
		if got := q.Get("cursor"); got != "cur_older_token" {
			t.Errorf("query cursor = %q, want cur_older_token", got)
		}
		if got := q.Get("limit"); got != "50" {
			t.Errorf("query limit = %q, want 50", got)
		}

		if page.NextCursor != "" {
			t.Errorf("NextCursor = %q, want empty", page.NextCursor)
		}
		if len(page.Messages) != 1 || page.Messages[0].ID != "msg_0" {
			t.Errorf("Messages = %+v, want [msg_0]", page.Messages)
		}
	})

	t.Run("ListMessages calls ListMessagesPage and returns chronological slice", func(t *testing.T) {
		t.Parallel()
		client := &Client{
			BaseURL: "http://opencode.local",
			HTTP: &http.Client{
				Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					q := req.URL.Query()
					if q.Get("order") != "desc" {
						t.Errorf("ListMessages did not request order=desc, got %q", q.Get("order"))
					}
					respBody := `{
						"cursor": {"next": "cur_next"},
						"data": [
							{"id": "m2"},
							{"id": "m1"}
						]
					}`
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(respBody)),
					}, nil
				}),
			},
		}

		msgs, err := client.ListMessages(context.Background(), "ses_abc", 20)
		if err != nil {
			t.Fatalf("ListMessages failed: %v", err)
		}
		if len(msgs) != 2 || msgs[0].ID != "m1" || msgs[1].ID != "m2" {
			t.Errorf("ListMessages messages = %+v, want [m1, m2]", msgs)
		}
	})
}

func TestFormatCompactionTokens(t *testing.T) {
	t.Parallel()

	makeTokens := func(in, out, reasoning, cacheRead, cacheWrite int) TokenUsage {
		var u TokenUsage
		u.Input = in
		u.Output = out
		u.Reasoning = reasoning
		u.Cache.Read = cacheRead
		u.Cache.Write = cacheWrite
		return u
	}

	tests := []struct {
		name   string
		tokens TokenUsage
		want   string
	}{
		{
			name:   "all zero tokens",
			tokens: TokenUsage{},
			want:   "",
		},
		{
			name:   "sub-1000 tokens",
			tokens: makeTokens(350, 120, 0, 0, 0),
			want:   "350 in · 120 out",
		},
		{
			name:   "exact example from requirements",
			tokens: makeTokens(200000, 3000, 28, 57765, 0),
			want:   "257.8K in · 3.0K out",
		},
		{
			name:   "cache read and write included in input tokens",
			tokens: makeTokens(500, 200, 0, 1500, 500),
			want:   "2.5K in · 200 out",
		},
		{
			name:   "zero in tokens but non-zero out tokens",
			tokens: makeTokens(0, 1500, 0, 0, 0),
			want:   "0 in · 1.5K out",
		},
		{
			name:   "non-zero in tokens but zero out tokens",
			tokens: makeTokens(950, 0, 0, 0, 0),
			want:   "950 in · 0 out",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FormatCompactionTokens(tt.tokens)
			if got != tt.want {
				t.Errorf("FormatCompactionTokens() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCountDiffChanges(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		diff    string
		wantAdd int
		wantDel int
	}{
		{
			name:    "empty diff",
			diff:    "",
			wantAdd: 0,
			wantDel: 0,
		},
		{
			name: "standard unified diff with additions and deletions",
			diff: `--- a/main.go
+++ b/main.go
@@ -1,5 +1,6 @@
 package main
 
-func oldFunc() {
+func newFunc() {
+	// added comment
 }
-`,
			wantAdd: 2,
			wantDel: 2,
		},
		{
			name: "only additions (new file)",
			diff: `--- /dev/null
+++ b/hello.txt
+hello
+world
+!
`,
			wantAdd: 3,
			wantDel: 0,
		},
		{
			name:    "crlf line endings",
			diff:    "--- a/file\r\n+++ b/file\r\n@@ -1 +1 @@\r\n-old\r\n+new\r\n",
			wantAdd: 1,
			wantDel: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			added, deleted := CountDiffChanges(tt.diff)
			if added != tt.wantAdd || deleted != tt.wantDel {
				t.Errorf("CountDiffChanges() = (+%d, -%d), want (+%d, -%d)",
					added, deleted, tt.wantAdd, tt.wantDel)
			}
		})
	}
}

func TestDecodeMessagesAndEvents(t *testing.T) {
	t.Parallel()

	t.Run("decode compaction message", func(t *testing.T) {
		t.Parallel()
		raw := `{
			"id": "msg_compact_1",
			"type": "compaction",
			"summary": "Compacted conversation turns",
			"status": "completed",
			"reason": "auto",
			"metadata": {"compactedTurns": 8},
			"tokens": {
				"input": 120000,
				"output": 1500
			}
		}`
		var msg Message
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatalf("Unmarshal compaction message: %v", err)
		}
		if msg.ID != "msg_compact_1" || msg.Type != "compaction" {
			t.Errorf("unexpected ID/Type: %+v", msg)
		}
		if msg.Summary != "Compacted conversation turns" || msg.Status != "completed" || msg.Reason != "auto" {
			t.Errorf("unexpected compaction fields: summary=%q status=%q reason=%q", msg.Summary, msg.Status, msg.Reason)
		}
		if msg.Metadata["compactedTurns"] != float64(8) {
			t.Errorf("unexpected metadata: %+v", msg.Metadata)
		}
	})

	t.Run("decode agent-switched message", func(t *testing.T) {
		t.Parallel()
		raw := `{
			"id": "msg_switched_1",
			"type": "agent-switched",
			"agent": "coder",
			"previous": "architect",
			"text": "Switched to coder"
		}`
		var msg Message
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatalf("Unmarshal agent-switched message: %v", err)
		}
		if msg.Type != "agent-switched" || msg.Agent != "coder" || msg.Previous != "architect" {
			t.Errorf("unexpected agent-switched message: %+v", msg)
		}
	})

	t.Run("decode model-switched message with object previous", func(t *testing.T) {
		t.Parallel()
		raw := `{
			"id": "msg_mod_1",
			"type": "model-switched",
			"model": {"id": "gemini-4-argon", "providerID": "opencode-go"},
			"previous": {"id": "antigravity-gemini-4-argon-medium", "providerID": "google", "variant": "default"}
		}`
		var msg Message
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatalf("Unmarshal model-switched message: %v", err)
		}
		if msg.Type != "model-switched" || msg.Model.ID != "gemini-4-argon" || msg.Previous != "" {
			t.Errorf("unexpected model-switched message: %+v", msg)
		}
	})

	t.Run("decode synthetic message", func(t *testing.T) {
		t.Parallel()
		raw := `{
			"id": "msg_synth_1",
			"type": "synthetic",
			"description": "System generated event",
			"metadata": {"key": "val"}
		}`
		var msg Message
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatalf("Unmarshal synthetic message: %v", err)
		}
		if msg.Type != "synthetic" || msg.Description != "System generated event" || msg.Metadata["key"] != "val" {
			t.Errorf("unexpected synthetic message: %+v", msg)
		}
	})

	t.Run("decode message with files", func(t *testing.T) {
		t.Parallel()
		raw := `{
			"id": "msg_user_1",
			"type": "user",
			"text": "See attached image",
			"files": [
				{
					"mime": "image/png",
					"name": "screenshot.png",
					"data": "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg=="
				}
			]
		}`
		var msg Message
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatalf("Unmarshal message with files: %v", err)
		}
		if len(msg.Files) != 1 {
			t.Fatalf("expected 1 file attachment, got %d", len(msg.Files))
		}
		f := msg.Files[0]
		if f.Mime != "image/png" || f.Name != "screenshot.png" || !strings.HasPrefix(f.Data, "iVBORw") {
			t.Errorf("unexpected file attachment: %+v", f)
		}
	})

	t.Run("decode tool content item with file", func(t *testing.T) {
		t.Parallel()
		raw := `{
			"type": "file",
			"mime": "image/jpeg",
			"name": "photo.jpg",
			"data": "/9j/4AAQSkZJRg=="
		}`
		var item ToolContentItem
		if err := json.Unmarshal([]byte(raw), &item); err != nil {
			t.Fatalf("Unmarshal ToolContentItem: %v", err)
		}
		if item.Type != "file" || item.Mime != "image/jpeg" || item.Name != "photo.jpg" || item.Data != "/9j/4AAQSkZJRg==" {
			t.Errorf("unexpected ToolContentItem: %+v", item)
		}
	})

	t.Run("decode compaction SSE event", func(t *testing.T) {
		t.Parallel()
		raw := `{
			"sessionID": "ses_456",
			"reason": "auto",
			"inputID": "inp_789",
			"text": "Compacting conversation...",
			"model": {"providerID": "anthropic", "id": "claude-3-5"},
			"tokens": {"input": 45000, "output": 800}
		}`
		var ev CompactionEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("Unmarshal CompactionEvent: %v", err)
		}
		if ev.SessionID != "ses_456" || ev.Reason != "auto" || ev.InputID != "inp_789" || ev.Model.ID != "claude-3-5" || ev.Model.ProviderID != "anthropic" {
			t.Errorf("unexpected CompactionEvent: %+v", ev)
		}
	})

	t.Run("decode message with streamed time", func(t *testing.T) {
		t.Parallel()
		raw := `{
			"id": "msg_ast_1",
			"type": "assistant",
			"tokens": {"output": 100, "reasoning": 20},
			"time": {
				"created": 1700000000000,
				"streamed": 1700000002000,
				"completed": 1700000005000
			}
		}`
		var msg Message
		if err := json.Unmarshal([]byte(raw), &msg); err != nil {
			t.Fatalf("Unmarshal message with streamed time: %v", err)
		}
		if msg.Time.Created != 1700000000000 || msg.Time.Streamed != 1700000002000 || msg.Time.Completed != 1700000005000 {
			t.Errorf("unexpected msg.Time: %+v", msg.Time)
		}
	})
}

func TestFormatMessageTokPerSec(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		messages []Message
		want     string
	}{
		{
			name:     "nil messages",
			messages: nil,
			want:     "",
		},
		{
			name:     "empty messages",
			messages: []Message{},
			want:     "",
		},
		{
			name: "only user messages",
			messages: []Message{
				{Type: "user", Text: "hello"},
			},
			want: "",
		},
		{
			name: "assistant message with zero tokens",
			messages: []Message{
				{
					Type: "assistant",
					Time: struct {
						Created   int64 `json:"created"`
						Streamed  int64 `json:"streamed,omitempty"`
						Completed int64 `json:"completed,omitempty"`
					}{Created: 1000, Streamed: 5000, Completed: 5000},
					Tokens: TokenUsage{Output: 0, Reasoning: 0},
				},
			},
			want: "",
		},
		{
			name: "assistant message with streamed time and output tokens",
			messages: []Message{
				{
					Type: "assistant",
					Time: struct {
						Created   int64 `json:"created"`
						Streamed  int64 `json:"streamed,omitempty"`
						Completed int64 `json:"completed,omitempty"`
					}{Created: 1000, Streamed: 11000, Completed: 15000},
					Tokens: TokenUsage{Output: 171},
				},
			},
			want: "17.1 tok/s",
		},
		{
			name: "assistant message with reasoning tokens included",
			messages: []Message{
				{
					Type: "assistant",
					Time: struct {
						Created   int64 `json:"created"`
						Streamed  int64 `json:"streamed,omitempty"`
						Completed int64 `json:"completed,omitempty"`
					}{Created: 2000, Streamed: 5000, Completed: 7000},
					Tokens: TokenUsage{Output: 100, Reasoning: 50}, // 150 tokens in 3000ms = 50 tok/s
				},
			},
			want: "50.0 tok/s",
		},
		{
			name: "fallback to completed time when streamed is zero",
			messages: []Message{
				{
					Type: "assistant",
					Time: struct {
						Created   int64 `json:"created"`
						Streamed  int64 `json:"streamed,omitempty"`
						Completed int64 `json:"completed,omitempty"`
					}{Created: 1000, Streamed: 0, Completed: 5000},
					Tokens: TokenUsage{Output: 100}, // 100 tokens in 4000ms = 25.0 tok/s
				},
			},
			want: "25.0 tok/s",
		},
		{
			name: "duration less than 50ms returns empty",
			messages: []Message{
				{
					Type: "assistant",
					Time: struct {
						Created   int64 `json:"created"`
						Streamed  int64 `json:"streamed,omitempty"`
						Completed int64 `json:"completed,omitempty"`
					}{Created: 1000, Streamed: 1040, Completed: 1040},
					Tokens: TokenUsage{Output: 100},
				},
			},
			want: "",
		},
		{
			name: "missing created time",
			messages: []Message{
				{
					Type: "assistant",
					Time: struct {
						Created   int64 `json:"created"`
						Streamed  int64 `json:"streamed,omitempty"`
						Completed int64 `json:"completed,omitempty"`
					}{Created: 0, Streamed: 5000, Completed: 5000},
					Tokens: TokenUsage{Output: 100},
				},
			},
			want: "",
		},
		{
			name: "picks newest assistant message and ignores non-assistant messages",
			messages: []Message{
				{
					ID:   "m1",
					Type: "assistant",
					Time: struct {
						Created   int64 `json:"created"`
						Streamed  int64 `json:"streamed,omitempty"`
						Completed int64 `json:"completed,omitempty"`
					}{Created: 1000, Streamed: 2000},
					Tokens: TokenUsage{Output: 10}, // 10 tok/s
				},
				{
					ID:   "m2",
					Type: "user",
					Text: "next step",
				},
				{
					ID:   "m3",
					Type: "assistant",
					Time: struct {
						Created   int64 `json:"created"`
						Streamed  int64 `json:"streamed,omitempty"`
						Completed int64 `json:"completed,omitempty"`
					}{Created: 5000, Streamed: 9000},
					Tokens: TokenUsage{Output: 160}, // 160 / 4s = 40.0 tok/s
				},
			},
			want: "40.0 tok/s",
		},
		{
			name: "skips in-progress latest assistant message to find completed one",
			messages: []Message{
				{
					ID:   "m1",
					Type: "assistant",
					Time: struct {
						Created   int64 `json:"created"`
						Streamed  int64 `json:"streamed,omitempty"`
						Completed int64 `json:"completed,omitempty"`
					}{Created: 1000, Streamed: 3000},
					Tokens: TokenUsage{Output: 60}, // 60 / 2s = 30.0 tok/s
				},
				{
					ID:   "m2",
					Type: "assistant",
					Time: struct {
						Created   int64 `json:"created"`
						Streamed  int64 `json:"streamed,omitempty"`
						Completed int64 `json:"completed,omitempty"`
					}{Created: 4000, Streamed: 0, Completed: 0},
					Tokens: TokenUsage{Output: 0},
				},
			},
			want: "30.0 tok/s",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FormatMessageTokPerSec(tt.messages)
			if got != tt.want {
				t.Errorf("FormatMessageTokPerSec() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestPromptWithFiles(t *testing.T) {
	t.Parallel()

	var reqBody PromptRequest
	client := &Client{
		BaseURL: "http://opencode.local",
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/api/session/ses_123/prompt" {
					t.Fatalf("unexpected path: %s", req.URL.Path)
				}
				if err := json.NewDecoder(req.Body).Decode(&reqBody); err != nil {
					t.Fatalf("decode body: %v", err)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{}`)),
				}, nil
			}),
		},
	}

	err := client.PromptWithFiles(context.Background(), "ses_123", "", []FileAttachment{
		{
			Name: "test.png",
			Mime: "image/png",
			Data: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
		},
	})
	if err != nil {
		t.Fatalf("PromptWithFiles failed: %v", err)
	}

	if len(reqBody.Files) != 1 {
		t.Fatalf("expected 1 file, got %d", len(reqBody.Files))
	}
	f := reqBody.Files[0]
	if !strings.HasPrefix(f.URI, "data:image/png;base64,") {
		t.Errorf("expected data URI prefix, got %s", f.URI)
	}
	if f.Source == nil || f.Source.Type != "inline" {
		t.Errorf("expected source {type: inline}, got %+v", f.Source)
	}
	if f.Name != "test.png" {
		t.Errorf("expected name test.png, got %s", f.Name)
	}
	if reqBody.Text == "" {
		t.Errorf("expected non-empty fallback text when text is empty")
	}
}

func TestRevertEndpoints(t *testing.T) {
	t.Parallel()

	var (
		capturedStageBody  map[string]any
		capturedClearReq   *http.Request
		capturedCommitReq  *http.Request
		capturedCompactReq *http.Request
	)

	client := &Client{
		BaseURL: "http://opencode.local",
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodPost && req.URL.Path == "/api/session/ses_123/revert/stage":
					if err := json.NewDecoder(req.Body).Decode(&capturedStageBody); err != nil {
						t.Fatalf("decode stage body: %v", err)
					}
					resp := `{"data":{"id":"ses_123","title":"Staged","revert":{"messageID":"msg_target","snapshot":"snap_1","diff":"+added"}}}`
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(resp)),
					}, nil
				case req.Method == http.MethodDelete && req.URL.Path == "/api/session/ses_123/revert":
					capturedClearReq = req
					resp := `{"data":{"id":"ses_123","title":"Cleared","revert":null}}`
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(resp)),
					}, nil
				case req.Method == http.MethodPost && req.URL.Path == "/api/session/ses_123/revert/commit":
					capturedCommitReq = req
					resp := `{"data":{"id":"ses_123","title":"Committed"}}`
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(resp)),
					}, nil
				case req.Method == http.MethodPost && req.URL.Path == "/api/session/ses_123/compact":
					capturedCompactReq = req
					return &http.Response{
						StatusCode: http.StatusNoContent,
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				default:
					return &http.Response{
						StatusCode: http.StatusNotFound,
						Body:       io.NopCloser(strings.NewReader("not found")),
					}, nil
				}
			}),
		},
	}

	ctx := context.Background()

	// StageRevert
	sess, err := client.StageRevert(ctx, "ses_123", "msg_target", true)
	if err != nil {
		t.Fatalf("StageRevert failed: %v", err)
	}
	if sess.Revert == nil || sess.Revert.MessageID != "msg_target" || sess.Revert.Diff != "+added" {
		t.Errorf("unexpected sess.Revert: %+v", sess.Revert)
	}
	if capturedStageBody["messageID"] != "msg_target" || capturedStageBody["files"] != true {
		t.Errorf("unexpected StageRevert payload: %+v", capturedStageBody)
	}

	// ClearRevert
	sess, err = client.ClearRevert(ctx, "ses_123")
	if err != nil {
		t.Fatalf("ClearRevert failed: %v", err)
	}
	if capturedClearReq == nil {
		t.Fatal("ClearRevert request not captured")
	}
	if sess.Revert != nil {
		t.Errorf("expected nil Revert after clear, got %+v", sess.Revert)
	}

	// CommitRevert
	sess, err = client.CommitRevert(ctx, "ses_123")
	if err != nil {
		t.Fatalf("CommitRevert failed: %v", err)
	}
	if capturedCommitReq == nil {
		t.Fatal("CommitRevert request not captured")
	}
	if sess.ID != "ses_123" {
		t.Errorf("unexpected session ID: %s", sess.ID)
	}

	// CompactSession
	if err := client.CompactSession(ctx, "ses_123"); err != nil {
		t.Fatalf("CompactSession failed: %v", err)
	}
	if capturedCompactReq == nil {
		t.Fatal("CompactSession request not captured")
	}
}

func TestRevertEvents(t *testing.T) {
	t.Parallel()

	t.Run("RevertStagedEvent", func(t *testing.T) {
		t.Parallel()
		raw := `{"sessionID":"ses_1","revert":{"messageID":"msg_10","snapshot":"snap_x","diff":"--- diff"}}`
		var ev RevertStagedEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("unmarshal RevertStagedEvent: %v", err)
		}
		if ev.SessionID != "ses_1" || ev.Revert.MessageID != "msg_10" || ev.Revert.Snapshot != "snap_x" || ev.Revert.Diff != "--- diff" {
			t.Errorf("unexpected RevertStagedEvent: %+v", ev)
		}
	})

	t.Run("RevertClearedEvent", func(t *testing.T) {
		t.Parallel()
		raw := `{"sessionID":"ses_1"}`
		var ev RevertClearedEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("unmarshal RevertClearedEvent: %v", err)
		}
		if ev.SessionID != "ses_1" {
			t.Errorf("unexpected RevertClearedEvent: %+v", ev)
		}
	})

	t.Run("RevertCommittedEvent", func(t *testing.T) {
		t.Parallel()
		raw := `{"sessionID":"ses_1","messageID":"msg_10"}`
		var ev RevertCommittedEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("unmarshal RevertCommittedEvent: %v", err)
		}
		if ev.SessionID != "ses_1" || ev.MessageID != "msg_10" {
			t.Errorf("unexpected RevertCommittedEvent: %+v", ev)
		}
	})
}

func TestCommandsAndSkillsAndFiles(t *testing.T) {
	t.Parallel()

	var (
		capturedCommandReq *http.Request
		capturedRunBody    map[string]any
		capturedSkillReq   *http.Request
		capturedFindReq    *http.Request
	)

	client := &Client{
		BaseURL: "http://opencode.local",
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodGet && req.URL.Path == "/api/command":
					capturedCommandReq = req
					resp := `{"data":[{"name":"test","description":"run test suite"},{"name":"build","description":"build binary"}]}`
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(resp)),
					}, nil
				case req.Method == http.MethodPost && req.URL.Path == "/api/session/ses_123/command":
					if err := json.NewDecoder(req.Body).Decode(&capturedRunBody); err != nil {
						t.Fatalf("decode run command body: %v", err)
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{}`)),
					}, nil
				case req.Method == http.MethodGet && req.URL.Path == "/api/skill":
					capturedSkillReq = req
					resp := `{"data":[{"id":"git","name":"Git Helper","description":"Git commands","path":"/skills/git.md"}]}`
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(resp)),
					}, nil
				case req.Method == http.MethodGet && req.URL.Path == "/api/fs/find":
					capturedFindReq = req
					resp := `{"data":[{"path":"src/app.go","type":"file"},{"path":"src","type":"directory"}]}`
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(resp)),
					}, nil
				default:
					return &http.Response{
						StatusCode: http.StatusNotFound,
						Body:       io.NopCloser(strings.NewReader("not found")),
					}, nil
				}
			}),
		},
	}

	ctx := context.Background()

	// ListCommands with directory
	cmds, err := client.ListCommands(ctx, "/home/project")
	if err != nil {
		t.Fatalf("ListCommands failed: %v", err)
	}
	if capturedCommandReq == nil || capturedCommandReq.URL.Query().Get("location[directory]") != "/home/project" {
		t.Errorf("ListCommands query unexpected: %v", capturedCommandReq.URL.RawQuery)
	}
	if len(cmds) != 2 || cmds[0].Name != "test" || cmds[1].Name != "build" {
		t.Errorf("unexpected cmds: %+v", cmds)
	}

	// RunCommand
	err = client.RunCommand(ctx, "ses_123", "test", "--unit")
	if err != nil {
		t.Fatalf("RunCommand failed: %v", err)
	}
	if capturedRunBody["name"] != "test" || capturedRunBody["text"] != "--unit" {
		t.Errorf("unexpected RunCommand body: %+v", capturedRunBody)
	}

	// ListSkills
	skills, err := client.ListSkills(ctx, "/home/project")
	if err != nil {
		t.Fatalf("ListSkills failed: %v", err)
	}
	if capturedSkillReq == nil || capturedSkillReq.URL.Query().Get("location[directory]") != "/home/project" {
		t.Errorf("ListSkills query unexpected: %v", capturedSkillReq.URL.RawQuery)
	}
	if len(skills) != 1 || skills[0].ID != "git" || skills[0].Path != "/skills/git.md" {
		t.Errorf("unexpected skills: %+v", skills)
	}

	// FindFiles
	files, err := client.FindFiles(ctx, "/home/project", "app", 15)
	if err != nil {
		t.Fatalf("FindFiles failed: %v", err)
	}
	if capturedFindReq == nil {
		t.Fatal("FindFiles request not captured")
	}
	q := capturedFindReq.URL.Query()
	if q.Get("location[directory]") != "/home/project" || q.Get("query") != "app" || q.Get("limit") != "15" {
		t.Errorf("unexpected FindFiles query params: %v", q)
	}
	if len(files) != 2 || files[0].Path != "src/app.go" || files[0].Type != "file" || files[1].Type != "directory" {
		t.Errorf("unexpected FindFiles results: %+v", files)
	}
}

func TestPromptWithOptions(t *testing.T) {
	t.Parallel()

	var reqBody PromptRequest
	client := &Client{
		BaseURL: "http://opencode.local",
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/api/session/ses_456/prompt" {
					t.Fatalf("unexpected path: %s", req.URL.Path)
				}
				if err := json.NewDecoder(req.Body).Decode(&reqBody); err != nil {
					t.Fatalf("decode prompt request body: %v", err)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{}`)),
				}, nil
			}),
		},
	}

	opts := PromptOptions{
		Text: "Please check @architect with @skill:refactor on file:///main.go",
		Files: []FileAttachment{
			{
				Name: "diagram.png",
				Mime: "image/png",
				Data: "aGVsbG8=",
			},
		},
		Paths: []PromptFileInput{
			{
				URI:         "file:///main.go",
				Name:        "main.go",
				Description: "entry file",
				Mention: &PromptMention{
					Start: 45,
					End:   61,
					Text:  "file:///main.go",
				},
			},
		},
		Agents: []PromptAgentInput{
			{
				Name: "architect",
				Mention: &PromptMention{
					Start: 13,
					End:   23,
					Text:  "@architect",
				},
			},
		},
		Skills: []PromptSkillInput{
			{
				ID: "refactor",
				Mention: &PromptMention{
					Start: 29,
					End:   44,
					Text:  "@skill:refactor",
				},
			},
		},
	}

	err := client.PromptWithOptions(context.Background(), "ses_456", opts)
	if err != nil {
		t.Fatalf("PromptWithOptions failed: %v", err)
	}

	if reqBody.Text != opts.Text {
		t.Errorf("reqBody.Text = %q, want %q", reqBody.Text, opts.Text)
	}
	if !reqBody.Resume {
		t.Errorf("reqBody.Resume = false, want true")
	}
	if len(reqBody.Files) != 2 {
		t.Fatalf("len(reqBody.Files) = %d, want 2", len(reqBody.Files))
	}
	// First file: inline image
	if reqBody.Files[0].Name != "diagram.png" || reqBody.Files[0].URI != "data:image/png;base64,aGVsbG8=" || reqBody.Files[0].Source == nil || reqBody.Files[0].Source.Type != "inline" {
		t.Errorf("unexpected reqBody.Files[0]: %+v", reqBody.Files[0])
	}
	// Second file: path URI
	if reqBody.Files[1].URI != "file:///main.go" || reqBody.Files[1].Description != "entry file" || reqBody.Files[1].Mention == nil || reqBody.Files[1].Mention.Text != "file:///main.go" {
		t.Errorf("unexpected reqBody.Files[1]: %+v", reqBody.Files[1])
	}
	// Agents
	if len(reqBody.Agents) != 1 || reqBody.Agents[0].Name != "architect" || reqBody.Agents[0].Mention.Start != 13 {
		t.Errorf("unexpected reqBody.Agents: %+v", reqBody.Agents)
	}
	// Skills
	if len(reqBody.Skills) != 1 || reqBody.Skills[0].ID != "refactor" || reqBody.Skills[0].Mention.Start != 29 {
		t.Errorf("unexpected reqBody.Skills: %+v", reqBody.Skills)
	}
}

func TestShellEndpoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var killedID string
	var capturedDir string

	client := &Client{
		BaseURL: "http://opencode.local",
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet && req.URL.Path == "/api/shell" {
					capturedDir = req.URL.Query().Get("location[directory]")
					respBody := `{"data":[{"id":"sh_1","pid":1234,"sessionID":"ses_1","command":"npm run dev","description":"Dev server","status":"running","time":{"created":1000}}]}`
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(respBody)),
					}, nil
				}
				if req.Method == http.MethodDelete && strings.HasPrefix(req.URL.Path, "/api/shell/") {
					killedID = strings.TrimPrefix(req.URL.Path, "/api/shell/")
					return &http.Response{
						StatusCode: http.StatusOK,
						Body:       io.NopCloser(strings.NewReader(`{"success":true}`)),
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(strings.NewReader("not found")),
				}, nil
			}),
		},
	}

	shells, err := client.ListShells(ctx, "/path/to/project")
	if err != nil {
		t.Fatalf("ListShells failed: %v", err)
	}
	if capturedDir != "/path/to/project" {
		t.Errorf("capturedDir = %q, want /path/to/project", capturedDir)
	}
	if len(shells) != 1 || shells[0].ID != "sh_1" || shells[0].PID != 1234 || shells[0].Status != "running" {
		t.Errorf("unexpected shells: %+v", shells)
	}

	if err := client.KillShell(ctx, "sh_1"); err != nil {
		t.Fatalf("KillShell failed: %v", err)
	}
	if killedID != "sh_1" {
		t.Errorf("killedID = %q, want sh_1", killedID)
	}
}

func TestShellEvents(t *testing.T) {
	t.Parallel()

	t.Run("shell.created with info", func(t *testing.T) {
		raw := `{"info":{"id":"sh_1","pid":456,"command":"make build","status":"running"}}`
		var ev ShellEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if ev.Info.ID != "sh_1" || ev.Info.PID != 456 || ev.Info.Status != "running" {
			t.Errorf("unexpected ev.Info: %+v", ev.Info)
		}
	})

	t.Run("shell.exited with shellID and id", func(t *testing.T) {
		raw := `{"id":"sh_2","status":"exited"}`
		var ev ShellEvent
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if ev.ID != "sh_2" || ev.Status != "exited" {
			t.Errorf("unexpected ev: %+v", ev)
		}
	})
}

func TestDiscover(t *testing.T) {
	ctx := context.Background()

	t.Run("uses OPENCODE_SERVER_URL and OPENCODE_SERVER_PASSWORD", func(t *testing.T) {
		t.Setenv("OPENCODE_SERVER_URL", "http://127.0.0.1:5555/")
		t.Setenv("OPENCODE_SERVER_PASSWORD", "secret123")

		c, err := Discover(ctx)
		if err != nil {
			t.Fatalf("Discover failed: %v", err)
		}
		if c.BaseURL != "http://127.0.0.1:5555" {
			t.Errorf("BaseURL = %q, want http://127.0.0.1:5555", c.BaseURL)
		}
		if c.Password != "secret123" {
			t.Errorf("Password = %q, want secret123", c.Password)
		}
	})
}

func TestInboxEndpoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var (
		capturedPromptBody PromptRequest
		capturedPatchBody  map[string]any
		cancelledInboxID   string
	)

	client := &Client{
		BaseURL: "http://opencode.local",
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodPost && req.URL.Path == "/api/session/ses_123/prompt":
					if err := json.NewDecoder(req.Body).Decode(&capturedPromptBody); err != nil {
						t.Fatalf("decode prompt body: %v", err)
					}
					resp := `{"data":{"id":"inb_1","sessionID":"ses_123","time":{"created":1794007286293},"type":"user","payload":{"text":"queued prompt"},"delivery":"queue"}}`
					return &http.Response{
						StatusCode: http.StatusCreated,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(resp)),
					}, nil
				case req.Method == http.MethodGet && req.URL.Path == "/api/session/ses_123/inbox":
					resp := `{"data":[{"id":"inb_1","sessionID":"ses_123","time":{"created":1794007286293},"type":"user","payload":{"text":"queued prompt","files":[{"data":"aGk=","mime":"image/png","name":"a.png","source":{"type":"inline"}}]},"delivery":"queue"}]}`
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(resp)),
					}, nil
				case req.Method == http.MethodPatch && req.URL.Path == "/api/session/ses_123/inbox/inb_1":
					if err := json.NewDecoder(req.Body).Decode(&capturedPatchBody); err != nil {
						t.Fatalf("decode patch body: %v", err)
					}
					return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
				case req.Method == http.MethodDelete && req.URL.Path == "/api/session/ses_123/inbox/inb_1":
					cancelledInboxID = "inb_1"
					return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody}, nil
				default:
					return &http.Response{
						StatusCode: http.StatusNotFound,
						Body:       io.NopCloser(strings.NewReader("not found")),
					}, nil
				}
			}),
		},
	}

	item, err := client.SendPrompt(ctx, "ses_123", PromptOptions{
		Text:     "queued prompt",
		Delivery: DeliveryQueue,
	})
	if err != nil {
		t.Fatalf("SendPrompt failed: %v", err)
	}
	if capturedPromptBody.Delivery != DeliveryQueue {
		t.Errorf("capturedPromptBody.Delivery = %q, want %q", capturedPromptBody.Delivery, DeliveryQueue)
	}
	if item == nil || item.ID != "inb_1" || item.Delivery != DeliveryQueue {
		t.Errorf("SendPrompt returned unexpected item: %+v", item)
	}

	items, err := client.ListInbox(ctx, "ses_123")
	if err != nil {
		t.Fatalf("ListInbox failed: %v", err)
	}
	if len(items) != 1 || items[0].ID != "inb_1" || items[0].Payload.Text != "queued prompt" ||
		len(items[0].Payload.Files) != 1 || items[0].Payload.Files[0].Name != "a.png" {
		t.Errorf("ListInbox returned unexpected items: %+v", items)
	}

	if err := client.UpdateInboxDelivery(ctx, "ses_123", "inb_1", DeliverySteer); err != nil {
		t.Fatalf("UpdateInboxDelivery failed: %v", err)
	}
	if capturedPatchBody["delivery"] != "steer" {
		t.Errorf("capturedPatchBody = %+v, want delivery=steer", capturedPatchBody)
	}

	if err := client.CancelInbox(ctx, "ses_123", "inb_1"); err != nil {
		t.Fatalf("CancelInbox failed: %v", err)
	}
	if cancelledInboxID != "inb_1" {
		t.Errorf("cancelledInboxID = %q, want inb_1", cancelledInboxID)
	}
}

func TestInboxEventDecoding(t *testing.T) {
	t.Parallel()

	// Payload captured from an OpenCode 2.2.5 /api/event stream: the item
	// carries no ID of its own, and the ID lives on the event as inboxID.
	const data = `{"inboxID":"msg_1","sessionID":"ses_1","item":{"type":"user","payload":{"text":"hello queued"},"delivery":"queue"}}`

	var ev InboxEvent
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Fatalf("Unmarshal failed: %v", err)
	}
	if ev.InboxID != "msg_1" || ev.Item == nil {
		t.Fatalf("unexpected event: %+v", ev)
	}
	if ev.Item.Payload.Text != "hello queued" || ev.Item.Delivery != DeliveryQueue {
		t.Errorf("unexpected item: %+v", ev.Item)
	}
}

func TestSearchSessions(t *testing.T) {
	t.Parallel()

	var capturedQuery string
	var capturedParent string
	var capturedLimit string
	client := &Client{
		BaseURL: "http://opencode.local",
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet && req.URL.Path == "/api/session" {
					q := req.URL.Query()
					capturedQuery = q.Get("search")
					capturedParent = q.Get("parentID")
					capturedLimit = q.Get("limit")
					resp := `{"data":[{"id":"ses_archived","title":"Archived feature work","location":{"directory":"/home/user/proj"}}]}`
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body:       io.NopCloser(strings.NewReader(resp)),
					}, nil
				}
				return &http.Response{
					StatusCode: http.StatusNotFound,
					Body:       io.NopCloser(strings.NewReader("not found")),
				}, nil
			}),
		},
	}

	sessions, err := client.SearchSessions(context.Background(), "Archived feature", 50)
	if err != nil {
		t.Fatalf("SearchSessions failed: %v", err)
	}
	if capturedQuery != "Archived feature" || capturedParent != "null" || capturedLimit != "50" {
		t.Errorf("query params = (search=%q, parentID=%q, limit=%q), want (Archived feature, null, 50)",
			capturedQuery, capturedParent, capturedLimit)
	}
	if len(sessions) != 1 || sessions[0].ID != "ses_archived" {
		t.Errorf("SearchSessions results = %+v, want [ses_archived]", sessions)
	}
}
