package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultOAuthAttemptLifetime = 10 * time.Minute

// IntegrationMethod describes an authentication method supported by an OpenCode integration.
type IntegrationMethod struct {
	ID    string `json:"id,omitempty"`
	Type  string `json:"type"` // "oauth", "command", "key", "external", "env"
	Label string `json:"label,omitempty"`
}

// IntegrationInfo describes an integration returned by GET /api/integration/{integrationID}.
type IntegrationInfo struct {
	ID       string              `json:"id"`
	Name     string              `json:"name"`
	Metadata map[string]any      `json:"metadata,omitempty"`
	Methods  []IntegrationMethod `json:"methods"`
}

// OAuthAttemptTime holds server-side creation and expiration timestamps in Unix milliseconds.
type OAuthAttemptTime struct {
	Created int64 `json:"created"`
	Expires int64 `json:"expires"`
}

// OAuthAttempt describes an active OAuth sign-in attempt returned by POST /api/integration/{id}/connect/oauth.
type OAuthAttempt struct {
	AttemptID    string           `json:"attemptID"`
	URL          string           `json:"url"`
	Instructions string           `json:"instructions"`
	Mode         string           `json:"mode"` // "auto" or "code"
	Paste        bool             `json:"paste,omitempty"`
	Time         OAuthAttemptTime `json:"time"`
}

// Lifetime returns the attempt's server-specified duration (Expires - Created),
// falling back to 10 minutes when timestamps are missing or invalid.
func (a OAuthAttempt) Lifetime() time.Duration {
	if a.Time.Expires > a.Time.Created {
		return time.Duration(a.Time.Expires-a.Time.Created) * time.Millisecond
	}
	return defaultOAuthAttemptLifetime
}

// OAuthAttemptStatus describes the current state of an OAuth sign-in attempt.
type OAuthAttemptStatus struct {
	Status  string           `json:"status"` // "pending", "complete", "failed", "expired"
	Message string           `json:"message,omitempty"`
	Time    OAuthAttemptTime `json:"time"`
}

// OAuthConnectRequest is the payload for POST /api/integration/{id}/connect/oauth.
type OAuthConnectRequest struct {
	MethodID string         `json:"methodID"`
	Answer   map[string]any `json:"answer,omitempty"`
	Label    string         `json:"label,omitempty"`
}

// SelectOAuthMethod picks the first OAuth authentication method with a non-empty ID.
func SelectOAuthMethod(info *IntegrationInfo) (IntegrationMethod, error) {
	if info == nil {
		return IntegrationMethod{}, fmt.Errorf("integration info is missing")
	}
	for _, m := range info.Methods {
		if m.Type == "oauth" && strings.TrimSpace(m.ID) != "" {
			return m, nil
		}
	}
	return IntegrationMethod{}, fmt.Errorf("integration %q does not support OAuth sign-in", info.ID)
}

// ValidateExternalAuthURL verifies that an authorization URL uses http or https
// and has a non-empty host before handing it to the desktop URI launcher.
func ValidateExternalAuthURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("authorization URL is empty")
	}
	if strings.ContainsAny(raw, "\r\n\x00") {
		return "", fmt.Errorf("authorization URL contains invalid control characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid authorization URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("unsupported authorization URL scheme %q", u.Scheme)
	}
	if strings.TrimSpace(u.Host) == "" {
		return "", fmt.Errorf("authorization URL is missing a host")
	}
	return raw, nil
}

// GetIntegration fetches metadata and authentication methods for an integration
// (GET /api/integration/{integrationID}).
func (c *Client) GetIntegration(ctx context.Context, integrationID, directory string) (*IntegrationInfo, error) {
	if strings.TrimSpace(integrationID) == "" {
		return nil, fmt.Errorf("integrationID is required")
	}
	path := withLocationDirectory(fmt.Sprintf("/api/integration/%s", url.PathEscape(integrationID)), directory)
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var info IntegrationInfo
	if err := decodeDataOrDirect(raw, &info); err != nil {
		return nil, err
	}
	return &info, nil
}

// StartIntegrationOAuth starts a server-side OAuth authorization attempt
// (POST /api/integration/{integrationID}/connect/oauth).
func (c *Client) StartIntegrationOAuth(
	ctx context.Context,
	integrationID, directory string,
	req OAuthConnectRequest,
) (*OAuthAttempt, error) {
	if strings.TrimSpace(integrationID) == "" {
		return nil, fmt.Errorf("integrationID is required")
	}
	if strings.TrimSpace(req.MethodID) == "" {
		return nil, fmt.Errorf("methodID is required")
	}
	path := withLocationDirectory(fmt.Sprintf("/api/integration/%s/connect/oauth", url.PathEscape(integrationID)), directory)
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodPost, path, req, &raw); err != nil {
		return nil, err
	}
	var attempt OAuthAttempt
	if err := decodeDataOrDirect(raw, &attempt); err != nil {
		return nil, err
	}
	return &attempt, nil
}

// GetIntegrationOAuthStatus polls the status of an OAuth authorization attempt
// (GET /api/integration/{integrationID}/connect/oauth/{attemptID}).
func (c *Client) GetIntegrationOAuthStatus(
	ctx context.Context,
	integrationID, attemptID, directory string,
) (*OAuthAttemptStatus, error) {
	if strings.TrimSpace(integrationID) == "" {
		return nil, fmt.Errorf("integrationID is required")
	}
	if strings.TrimSpace(attemptID) == "" {
		return nil, fmt.Errorf("attemptID is required")
	}
	path := withLocationDirectory(
		fmt.Sprintf("/api/integration/%s/connect/oauth/%s", url.PathEscape(integrationID), url.PathEscape(attemptID)),
		directory,
	)
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var status OAuthAttemptStatus
	if err := decodeDataOrDirect(raw, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// CompleteIntegrationOAuth submits a pasted redirect URL or authorization code for an OAuth attempt
// (POST /api/integration/{integrationID}/connect/oauth/{attemptID}/complete).
func (c *Client) CompleteIntegrationOAuth(
	ctx context.Context,
	integrationID, attemptID, directory, code string,
) error {
	if strings.TrimSpace(integrationID) == "" {
		return fmt.Errorf("integrationID is required")
	}
	if strings.TrimSpace(attemptID) == "" {
		return fmt.Errorf("attemptID is required")
	}
	path := withLocationDirectory(
		fmt.Sprintf(
			"/api/integration/%s/connect/oauth/%s/complete",
			url.PathEscape(integrationID),
			url.PathEscape(attemptID),
		),
		directory,
	)
	var body any = map[string]any{}
	if code != "" {
		body = map[string]string{"code": code}
	}
	return c.doJSON(ctx, http.MethodPost, path, body, nil)
}

// CancelIntegrationOAuth cancels a pending OAuth attempt on the server
// (DELETE /api/integration/{integrationID}/connect/oauth/{attemptID}).
func (c *Client) CancelIntegrationOAuth(ctx context.Context, integrationID, attemptID, directory string) error {
	if strings.TrimSpace(integrationID) == "" {
		return fmt.Errorf("integrationID is required")
	}
	if strings.TrimSpace(attemptID) == "" {
		return fmt.Errorf("attemptID is required")
	}
	path := withLocationDirectory(
		fmt.Sprintf("/api/integration/%s/connect/oauth/%s", url.PathEscape(integrationID), url.PathEscape(attemptID)),
		directory,
	)
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}
