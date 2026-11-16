package opencode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestSelectOAuthMethod(t *testing.T) {
	t.Parallel()

	if _, err := SelectOAuthMethod(nil); err == nil {
		t.Fatal("expected error for nil IntegrationInfo")
	}

	_, err := SelectOAuthMethod(&IntegrationInfo{
		ID: "int_1",
		Methods: []IntegrationMethod{
			{Type: "key", Label: "API Key"},
			{Type: "env", Label: "ENV_VAR"},
			{ID: "", Type: "oauth", Label: "Missing ID"},
		},
	})
	if err == nil {
		t.Fatal("expected error when no valid oauth method is present")
	}

	m, err := SelectOAuthMethod(&IntegrationInfo{
		ID: "mcp_abc",
		Methods: []IntegrationMethod{
			{Type: "env", Label: "ENV"},
			{ID: "method_oauth_1", Type: "oauth", Label: "notes"},
			{ID: "method_oauth_2", Type: "oauth", Label: "other"},
		},
	})
	if err != nil {
		t.Fatalf("SelectOAuthMethod: %v", err)
	}
	if m.ID != "method_oauth_1" || m.Type != "oauth" {
		t.Fatalf("unexpected method: %+v", m)
	}
}

func TestValidateExternalAuthURL(t *testing.T) {
	t.Parallel()

	good := []string{
		"https://auth.example.com/oauth/authorize?client_id=abc&state=1%2B2&redirect_uri=http%3A%2F%2F127.0.0.1%3A4321%2Fcb",
		"  http://localhost:8080/login?x=1  ",
	}
	for _, raw := range good {
		got, err := ValidateExternalAuthURL(raw)
		if err != nil {
			t.Fatalf("ValidateExternalAuthURL(%q) unexpected error: %v", raw, err)
		}
		if got != strings.TrimSpace(raw) {
			t.Fatalf("ValidateExternalAuthURL(%q) = %q, want verbatim trimmed string", raw, got)
		}
	}

	bad := []string{
		"",
		"   ",
		"javascript:alert(1)",
		"file:///etc/passwd",
		"smb://host/share",
		"https:///missing-host",
		"https://example.com/auth\nX-Injected: 1",
		"/relative/path",
	}
	for _, raw := range bad {
		if got, err := ValidateExternalAuthURL(raw); err == nil {
			t.Fatalf("ValidateExternalAuthURL(%q) = %q, want error", raw, got)
		}
	}
}

func TestOAuthAttemptLifetime(t *testing.T) {
	t.Parallel()

	if got := (OAuthAttempt{}).Lifetime(); got != defaultOAuthAttemptLifetime {
		t.Fatalf("default Lifetime = %v, want %v", got, defaultOAuthAttemptLifetime)
	}
	if got := (OAuthAttempt{Time: OAuthAttemptTime{Created: 5000, Expires: 4000}}).Lifetime(); got != defaultOAuthAttemptLifetime {
		t.Fatalf("inverted Lifetime = %v, want %v", got, defaultOAuthAttemptLifetime)
	}
	if got := (OAuthAttempt{Time: OAuthAttemptTime{Created: 1000, Expires: 2250}}).Lifetime(); got != 1250*time.Millisecond {
		t.Fatalf("Lifetime = %v, want 1250ms", got)
	}
}

func TestIntegrationOAuthClientEndpoints(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	var (
		getDir       string
		startDir     string
		startReq     OAuthConnectRequest
		statusDir    string
		completeDir  string
		completeCode string
		cancelDir    string
	)

	client := &Client{
		BaseURL: "http://127.0.0.1:4096",
		HTTP: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch {
				case req.Method == http.MethodGet && req.URL.Path == "/api/integration/mcp_123":
					getDir = req.URL.Query().Get("location[directory]")
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(strings.NewReader(`{
							"location": {"directory": "/proj"},
							"data": {
								"id": "mcp_123",
								"name": "notes",
								"methods": [{"id": "m_oauth", "type": "oauth", "label": "notes"}]
							}
						}`)),
					}, nil
				case req.Method == http.MethodPost && req.URL.Path == "/api/integration/mcp_123/connect/oauth":
					startDir = req.URL.Query().Get("location[directory]")
					if err := json.NewDecoder(req.Body).Decode(&startReq); err != nil {
						t.Fatalf("decode startReq: %v", err)
					}
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(strings.NewReader(`{
							"location": {"directory": "/proj"},
							"data": {
								"attemptID": "con_1",
								"url": "https://auth.example.com/authorize?state=xyz",
								"instructions": "Authorize access in your browser.",
								"mode": "auto",
								"paste": true,
								"time": {"created": 1000, "expires": 61000}
							}
						}`)),
					}, nil
				case req.Method == http.MethodGet && req.URL.Path == "/api/integration/mcp_123/connect/oauth/con_1":
					statusDir = req.URL.Query().Get("location[directory]")
					return &http.Response{
						StatusCode: http.StatusOK,
						Header:     http.Header{"Content-Type": []string{"application/json"}},
						Body: io.NopCloser(strings.NewReader(`{
							"data": {
								"status": "failed",
								"message": "Access <denied> & rejected",
								"time": {"created": 1000, "expires": 61000}
							}
						}`)),
					}, nil
				case req.Method == http.MethodPost && req.URL.Path == "/api/integration/mcp_123/connect/oauth/con_1/complete":
					completeDir = req.URL.Query().Get("location[directory]")
					var body struct {
						Code string `json:"code"`
					}
					if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
						t.Fatalf("decode complete body: %v", err)
					}
					completeCode = body.Code
					return &http.Response{
						StatusCode: http.StatusNoContent,
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				case req.Method == http.MethodDelete && req.URL.Path == "/api/integration/mcp_123/connect/oauth/con_1":
					cancelDir = req.URL.Query().Get("location[directory]")
					return &http.Response{
						StatusCode: http.StatusNoContent,
						Body:       io.NopCloser(strings.NewReader("")),
					}, nil
				default:
					return &http.Response{
						StatusCode: http.StatusNotFound,
						Body:       io.NopCloser(strings.NewReader(`{"message":"not found"}`)),
					}, nil
				}
			}),
		},
	}

	info, err := client.GetIntegration(ctx, "mcp_123", "/proj")
	if err != nil {
		t.Fatalf("GetIntegration: %v", err)
	}
	if getDir != "/proj" || info.ID != "mcp_123" || len(info.Methods) != 1 || info.Methods[0].ID != "m_oauth" {
		t.Fatalf("unexpected GetIntegration result: dir=%q info=%+v", getDir, info)
	}

	attempt, err := client.StartIntegrationOAuth(ctx, "mcp_123", "/proj", OAuthConnectRequest{MethodID: "m_oauth"})
	if err != nil {
		t.Fatalf("StartIntegrationOAuth: %v", err)
	}
	if startDir != "/proj" || startReq.MethodID != "m_oauth" || attempt.AttemptID != "con_1" || !attempt.Paste || attempt.Mode != "auto" {
		t.Fatalf("unexpected StartIntegrationOAuth: dir=%q req=%+v attempt=%+v", startDir, startReq, attempt)
	}
	if attempt.Lifetime() != 60*time.Second {
		t.Fatalf("attempt.Lifetime = %v, want 60s", attempt.Lifetime())
	}

	status, err := client.GetIntegrationOAuthStatus(ctx, "mcp_123", "con_1", "/proj")
	if err != nil {
		t.Fatalf("GetIntegrationOAuthStatus: %v", err)
	}
	if statusDir != "/proj" || status.Status != "failed" || status.Message != "Access <denied> & rejected" {
		t.Fatalf("unexpected status: dir=%q status=%+v", statusDir, status)
	}

	if err := client.CompleteIntegrationOAuth(ctx, "mcp_123", "con_1", "/proj", "http://127.0.0.1:5000/cb?code=abc"); err != nil {
		t.Fatalf("CompleteIntegrationOAuth: %v", err)
	}
	if completeDir != "/proj" || completeCode != "http://127.0.0.1:5000/cb?code=abc" {
		t.Fatalf("unexpected CompleteIntegrationOAuth: dir=%q code=%q", completeDir, completeCode)
	}

	if err := client.CancelIntegrationOAuth(ctx, "mcp_123", "con_1", "/proj"); err != nil {
		t.Fatalf("CancelIntegrationOAuth: %v", err)
	}
	if cancelDir != "/proj" {
		t.Fatalf("unexpected CancelIntegrationOAuth dir=%q", cancelDir)
	}
}
