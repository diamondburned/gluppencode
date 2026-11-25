// Package opencode provides an HTTP and Server-Sent Events (SSE) client for the OpenCode V2 server.
package opencode

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Client is an HTTP and SSE client for the OpenCode V2 server.
type Client struct {
	mu           sync.RWMutex
	BaseURL      string
	Password     string
	HTTP         *http.Client
	autoDiscover bool
}

type serviceConfig struct {
	URL      string `json:"url"`
	Password string `json:"password"`
}

func normalizeServerURL(u string) string {
	u = strings.TrimSpace(u)
	if strings.HasPrefix(u, "http://0.0.0.0:") {
		u = "http://127.0.0.1:" + strings.TrimPrefix(u, "http://0.0.0.0:")
	}
	if strings.HasPrefix(u, "http://") || strings.HasPrefix(u, "https://") {
		return strings.TrimRight(u, "/")
	}
	return ""
}

func stateServiceFilePath() string {
	if stateDir := os.Getenv("XDG_STATE_HOME"); stateDir != "" {
		return filepath.Join(stateDir, "opencode", "service.json")
	}
	if homeDir, err := os.UserHomeDir(); err == nil && homeDir != "" {
		return filepath.Join(homeDir, ".local", "state", "opencode", "service.json")
	}
	return ""
}

func readServiceConfigFile(path string) (urlStr, password string) {
	if path == "" {
		return "", ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", ""
	}
	var cfg serviceConfig
	if json.Unmarshal(data, &cfg) != nil {
		return "", ""
	}
	return normalizeServerURL(cfg.URL), cfg.Password
}

func discoverEndpoint(ctx context.Context) (baseURL, password string) {
	baseURL = "http://127.0.0.1:4096"

	if envURL := normalizeServerURL(os.Getenv("OPENCODE_SERVER_URL")); envURL != "" {
		baseURL = envURL
		password = os.Getenv("OPENCODE_SERVER_PASSWORD")
	} else {
		stateURL, statePass := readServiceConfigFile(stateServiceFilePath())
		if stateURL != "" {
			baseURL = stateURL
		}
		if statePass != "" {
			password = statePass
		}
		if stateURL == "" {
			if out, err := exec.CommandContext(ctx, "opencode", "service", "status").Output(); err == nil {
				if u := normalizeServerURL(string(out)); u != "" {
					baseURL = u
				}
			}
		}
	}

	if password == "" {
		if cfgDir, err := os.UserConfigDir(); err == nil {
			_, cfgPass := readServiceConfigFile(filepath.Join(cfgDir, "opencode", "service.json"))
			if cfgPass != "" {
				password = cfgPass
			}
		}
	}
	if password == "" {
		password = os.Getenv("OPENCODE_SERVER_PASSWORD")
	}

	return strings.TrimRight(baseURL, "/"), password
}

// Discover discovers the running OpenCode V2 server URL and password.
func Discover(ctx context.Context) (*Client, error) {
	baseURL, password := discoverEndpoint(ctx)
	return &Client{
		BaseURL:      baseURL,
		Password:     password,
		HTTP:         &http.Client{},
		autoDiscover: os.Getenv("OPENCODE_SERVER_URL") == "",
	}, nil
}

// SetServerURL overrides the client's BaseURL. Passing an empty string
// restores automatic service discovery and refreshes the endpoint immediately.
func (c *Client) SetServerURL(ctx context.Context, rawURL string) {
	if c == nil {
		return
	}
	u := normalizeServerURL(rawURL)
	if u == "" {
		c.mu.Lock()
		c.autoDiscover = os.Getenv("OPENCODE_SERVER_URL") == ""
		c.mu.Unlock()
		baseURL, password := discoverEndpoint(ctx)
		c.mu.Lock()
		c.BaseURL, c.Password = baseURL, password
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	c.BaseURL = u
	c.autoDiscover = false
	c.mu.Unlock()
}

// Rediscover refreshes the client's BaseURL and Password from the active OpenCode
// service state file or CLI when the client was created via Discover. It reports
// whether the endpoint changed.
func (c *Client) Rediscover(ctx context.Context) bool {
	if !c.autoDiscover {
		return false
	}
	baseURL, password := discoverEndpoint(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	if baseURL == c.BaseURL && password == c.Password {
		return false
	}
	c.BaseURL, c.Password = baseURL, password
	return true
}

// do sends the request built by newReq. When the server is unreachable or
// rejects the credentials and rediscovery finds a different endpoint (e.g. the
// server restarted on a new port), it retries once against the new endpoint.
// Requests are never replayed against the same endpoint, so a POST that may have
// been processed is not duplicated.
func (c *Client) do(ctx context.Context, newReq func() (*http.Request, error)) (*http.Response, error) {
	req, err := newReq()
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	stale := (err != nil && ctx.Err() == nil) || (err == nil && resp.StatusCode == http.StatusUnauthorized)
	if !stale || !c.Rediscover(ctx) {
		return resp, err
	}
	if resp != nil {
		resp.Body.Close()
	}
	if req, err = newReq(); err != nil {
		return nil, err
	}
	return c.httpClient().Do(req)
}

// ServerURL returns the current OpenCode server base URL.
func (c *Client) ServerURL() string {
	if c == nil {
		return ""
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.BaseURL
}

func (c *Client) endpoint() (baseURL, password string) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.BaseURL, c.Password
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var r io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		r = bytes.NewReader(b)
	}
	baseURL, password := c.endpoint()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(baseURL, "/")+path, r)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if password != "" {
		req.SetBasicAuth("opencode", password)
	}
	return req, nil
}

func (c *Client) doJSON(ctx context.Context, method, path string, body, out any) error {
	resp, err := c.do(ctx, func() (*http.Request, error) {
		return c.newRequest(ctx, method, path, body)
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("http %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if out != nil {
		if resp.StatusCode == http.StatusNoContent {
			return nil
		}
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
	}
	return nil
}

// decodeDataOrDirect unmarshals either an OpenCode V2 {"data": T} envelope or a direct T payload.
func decodeDataOrDirect[T any](raw json.RawMessage, out *T) error {
	var env struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 && string(env.Data) != "null" {
		return json.Unmarshal(env.Data, out)
	}
	return json.Unmarshal(raw, out)
}

// ModelRef identifies an LLM model.
type ModelRef struct {
	ID         string `json:"id"`
	ProviderID string `json:"providerID"`
	Variant    string `json:"variant,omitempty"`
}

func (m ModelRef) String() string {
	if m.ProviderID != "" {
		return m.ProviderID + "/" + m.ID
	}
	return m.ID
}

// Location holds the working directory of a session.
type Location struct {
	Directory string `json:"directory"`
}

// SessionTime holds timestamps in milliseconds.
type SessionTime struct {
	Created  int64 `json:"created"`
	Updated  int64 `json:"updated"`
	Busy     int64 `json:"busy,omitempty"`
	Idle     int64 `json:"idle,omitempty"`
	Viewed   int64 `json:"viewed,omitempty"`
	Archived int64 `json:"archived,omitempty"`
}

// TokenUsage holds token counts for a message, step, or session.
type TokenUsage struct {
	Input     int `json:"input"`
	Output    int `json:"output"`
	Reasoning int `json:"reasoning"`
	Cache     struct {
		Read  int `json:"read"`
		Write int `json:"write"`
	} `json:"cache"`
}

// SessionTokens is an alias for TokenUsage on a Session.
type SessionTokens = TokenUsage

// Session represents an OpenCode V2 session.
type Session struct {
	ID        string         `json:"id"`
	ParentID  string         `json:"parentID,omitempty"`
	ProjectID string         `json:"projectID,omitempty"`
	Title     string         `json:"title"`
	Agent     string         `json:"agent"`
	Model     ModelRef       `json:"model"`
	Cost      float64        `json:"cost,omitempty"`
	Tokens    SessionTokens  `json:"tokens,omitempty"`
	Outcome   string         `json:"outcome,omitempty"`
	Revert    *SessionRevert `json:"revert,omitempty"`
	Time      SessionTime    `json:"time"`
	Location  Location       `json:"location"`
}

// IsRunning reports whether a session (such as a child subagent) is currently running.
func (s Session) IsRunning() bool {
	if s.Outcome != "" {
		return s.Time.Idle > 0 && s.Time.Busy > s.Time.Idle
	}
	if s.Time.Idle > 0 && s.Time.Idle >= s.Time.Busy {
		return false
	}
	return true
}

// ListSessions lists top-level sessions.
func (c *Client) ListSessions(ctx context.Context, limit int) ([]Session, error) {
	return c.SearchSessions(ctx, "", limit)
}

// SearchSessions searches top-level sessions matching query (GET /api/session?parentID=null&search=...).
func (c *Client) SearchSessions(ctx context.Context, query string, limit int) ([]Session, error) {
	v := url.Values{}
	v.Set("parentID", "null")
	if q := strings.TrimSpace(query); q != "" {
		v.Set("search", q)
	}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/session?" + v.Encode()
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var data []Session
	if err := decodeDataOrDirect(raw, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// ListChildSessions lists child subagent sessions for a given parent session ID.
func (c *Client) ListChildSessions(ctx context.Context, parentID string) ([]Session, error) {
	v := url.Values{}
	v.Set("parentID", parentID)
	v.Set("limit", "100")
	path := "/api/session?" + v.Encode()
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var data []Session
	if err := decodeDataOrDirect(raw, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// GetSession fetches a single session by ID (GET /api/session/{sessionID}).
func (c *Client) GetSession(ctx context.Context, sessionID string) (*Session, error) {
	var raw json.RawMessage
	path := fmt.Sprintf("/api/session/%s", url.PathEscape(sessionID))
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var sess Session
	if err := decodeDataOrDirect(raw, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

// CreateSessionRequest is the payload for creating a session.
type CreateSessionRequest struct {
	Title     string    `json:"title,omitempty"`
	Agent     string    `json:"agent,omitempty"`
	Model     *ModelRef `json:"model,omitempty"`
	Directory string    `json:"-"`
	Location  *Location `json:"location,omitempty"`
}

// CreateSession creates a new session.
func (c *Client) CreateSession(ctx context.Context, directory string) (*Session, error) {
	return c.CreateSessionWithOptions(ctx, CreateSessionRequest{Directory: directory})
}

// CreateSessionWithOptions creates a new session with optional agent, model, and location settings.
func (c *Client) CreateSessionWithOptions(ctx context.Context, req CreateSessionRequest) (*Session, error) {
	if req.Location == nil && req.Directory != "" {
		req.Location = &Location{Directory: req.Directory}
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodPost, "/api/session", req, &raw); err != nil {
		return nil, err
	}
	var sess Session
	if err := decodeDataOrDirect(raw, &sess); err != nil {
		return nil, err
	}
	if sess.Agent == "" && req.Agent != "" {
		sess.Agent = req.Agent
	}
	if sess.Model.ID == "" && req.Model != nil && req.Model.ID != "" {
		sess.Model = *req.Model
	}
	return &sess, nil
}

// RenameSession updates the title of a session (PATCH /api/session/{sessionID}).
func (c *Client) RenameSession(ctx context.Context, sessionID, title string) error {
	path := fmt.Sprintf("/api/session/%s", url.PathEscape(sessionID))
	body := map[string]string{"title": title}
	return c.doJSON(ctx, http.MethodPatch, path, body, nil)
}

// MoveSession moves a session to a different working directory (POST /api/session/{sessionID}/move).
func (c *Client) MoveSession(ctx context.Context, sessionID, directory string) error {
	path := fmt.Sprintf("/api/session/%s/move", url.PathEscape(sessionID))
	body := map[string]string{"directory": directory}
	return c.doJSON(ctx, http.MethodPost, path, body, nil)
}

// SessionRevert describes a staged revert on a session.
type SessionRevert struct {
	MessageID string     `json:"messageID"`
	Snapshot  string     `json:"snapshot,omitempty"`
	Diff      string     `json:"diff,omitempty"`
	Files     []FileDiff `json:"files,omitempty"`
}

// StageRevert stages a revert to a specific message ID in a session (POST /api/session/{sessionID}/revert/stage).
func (c *Client) StageRevert(ctx context.Context, sessionID, messageID string, revertFiles bool) (*SessionRevert, error) {
	path := fmt.Sprintf("/api/session/%s/revert/stage", url.PathEscape(sessionID))
	body := map[string]any{
		"messageID": messageID,
		"files":     revertFiles,
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodPost, path, body, &raw); err != nil {
		return nil, err
	}
	var rev SessionRevert
	if err := decodeDataOrDirect(raw, &rev); err != nil {
		return nil, err
	}
	return &rev, nil
}

// ClearRevert clears any staged revert for a session (DELETE /api/session/{sessionID}/revert).
func (c *Client) ClearRevert(ctx context.Context, sessionID string) (*Session, error) {
	path := fmt.Sprintf("/api/session/%s/revert", url.PathEscape(sessionID))
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodDelete, path, nil, &raw); err != nil {
		return nil, err
	}
	var sess Session
	if err := decodeDataOrDirect(raw, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

// CommitRevert commits a staged revert for a session (POST /api/session/{sessionID}/revert/commit).
func (c *Client) CommitRevert(ctx context.Context, sessionID string) (*Session, error) {
	path := fmt.Sprintf("/api/session/%s/revert/commit", url.PathEscape(sessionID))
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodPost, path, map[string]any{}, &raw); err != nil {
		return nil, err
	}
	var sess Session
	if err := decodeDataOrDirect(raw, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

// CompactSession manually triggers compaction on a session (POST /api/session/{sessionID}/compact).
func (c *Client) CompactSession(ctx context.Context, sessionID string) error {
	path := fmt.Sprintf("/api/session/%s/compact", url.PathEscape(sessionID))
	return c.doJSON(ctx, http.MethodPost, path, map[string]any{}, nil)
}

// ViewSession acknowledges an idle transition for a session (POST /api/session/{sessionID}/view).
func (c *Client) ViewSession(ctx context.Context, sessionID string, idle int64) error {
	path := fmt.Sprintf("/api/session/%s/view", url.PathEscape(sessionID))
	body := struct {
		Idle int64 `json:"idle"`
	}{
		Idle: idle,
	}
	return c.doJSON(ctx, http.MethodPost, path, body, nil)
}

// ActiveStatus describes an active session's execution state.
type ActiveStatus struct {
	Type string `json:"type"`
}

// ActiveSessions returns a map of currently active session IDs.
func (c *Client) ActiveSessions(ctx context.Context) (map[string]ActiveStatus, error) {
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, "/api/session/active", nil, &raw); err != nil {
		return nil, err
	}
	var data map[string]ActiveStatus
	if err := decodeDataOrDirect(raw, &data); err != nil {
		return nil, err
	}
	return data, nil
}

// JobTime holds timestamps (in epoch milliseconds) for a background job.
type JobTime struct {
	Started   int64 `json:"started"`
	Completed int64 `json:"completed,omitempty"`
}

// JobInfo describes an active or unconsumed background job from GET /api/experimental/job.
type JobInfo struct {
	ID             string  `json:"id"`
	Kind           string  `json:"kind"` // "shell", "subagent", "image"
	SessionID      string  `json:"sessionID"`
	Status         string  `json:"status"` // "running", "completed", "error", "cancelled"
	Title          string  `json:"title,omitempty"`
	Time           JobTime `json:"time"`
	ShellID        string  `json:"shellID,omitempty"`
	Command        string  `json:"command,omitempty"`
	ChildSessionID string  `json:"childSessionID,omitempty"`
	Agent          string  `json:"agent,omitempty"`
	Description    string  `json:"description,omitempty"`
	Path           string  `json:"path,omitempty"`
	Model          string  `json:"model,omitempty"`
}

// ListJobsOptions filters GET /api/experimental/job.
type ListJobsOptions struct {
	SessionID   string
	Kind        string
	Status      string
	Descendants bool
}

// ListJobs fetches active and unconsumed background jobs from GET /api/experimental/job.
func (c *Client) ListJobs(ctx context.Context, opts ListJobsOptions) ([]JobInfo, error) {
	path := "/api/experimental/job"
	v := url.Values{}
	if opts.SessionID != "" {
		v.Set("sessionID", opts.SessionID)
	}
	if opts.Kind != "" {
		v.Set("kind", opts.Kind)
	}
	if opts.Status != "" {
		v.Set("status", opts.Status)
	}
	if opts.Descendants {
		v.Set("descendants", "true")
	}
	if encoded := v.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var jobs []JobInfo
	if err := decodeDataOrDirect(raw, &jobs); err != nil {
		return nil, err
	}
	return jobs, nil
}

// Agent describes an OpenCode V2 agent.
type Agent struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Mode        string `json:"mode"` // "primary", "subagent"
	Hidden      bool   `json:"hidden"`
	Color       string `json:"color,omitempty"`
	Description string `json:"description,omitempty"`
	System      string `json:"system,omitempty"`
}

// ListAgents fetches registered agents from GET /api/agent.
func (c *Client) ListAgents(ctx context.Context, directory string) ([]Agent, error) {
	path := "/api/agent"
	if directory != "" {
		v := url.Values{}
		v.Set("location[directory]", directory)
		path += "?" + v.Encode()
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var agents []Agent
	if err := decodeDataOrDirect(raw, &agents); err != nil {
		return nil, err
	}
	return agents, nil
}

// PrimaryAgents filters a slice of agents to visible primary agents (!Hidden && Mode == "primary").
func PrimaryAgents(agents []Agent) []Agent {
	out := make([]Agent, 0, len(agents))
	for _, a := range agents {
		if !a.Hidden && a.Mode == "primary" {
			out = append(out, a)
		}
	}
	return out
}

// SwitchAgent switches the active agent for a session (POST /api/session/{sessionID}/agent).
func (c *Client) SwitchAgent(ctx context.Context, sessionID, agentID string) error {
	path := fmt.Sprintf("/api/session/%s/agent", url.PathEscape(sessionID))
	body := map[string]string{"agent": agentID}
	return c.doJSON(ctx, http.MethodPost, path, body, nil)
}

var defaultAgentPalette = []string{
	"#3b82f6",
	"#a855f7",
	"#10b981",
	"#f59e0b",
	"#ef4444",
	"#06b6d4",
}

// AgentColor returns the hex color for an agent matching OpenCode's TUI palette
// when configuredColor is empty.
func AgentColor(agentID, configuredColor string) string {
	if c := strings.TrimSpace(configuredColor); c != "" {
		return c
	}
	switch strings.ToLower(strings.TrimSpace(agentID)) {
	case "build":
		return "#3b82f6"
	case "plan":
		return "#a855f7"
	case "bare":
		return "#10b981"
	case "orchestrator":
		return "#f59e0b"
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(agentID))
	idx := int(h.Sum32()) % len(defaultAgentPalette)
	return defaultAgentPalette[idx]
}

// ModelInfo describes a model returned by GET /api/model.
type ModelInfo struct {
	ID         string `json:"id"`
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Name       string `json:"name"`
	Family     string `json:"family"`
	Type       string `json:"type"`
	Status     string `json:"status"`
	Enabled    bool   `json:"enabled"`
	Limit      struct {
		Context int `json:"context"`
		Input   int `json:"input"`
		Output  int `json:"output"`
	} `json:"limit"`
	Time struct {
		Released int64 `json:"released"`
	} `json:"time"`
}

// Ref returns the ModelRef for this ModelInfo.
func (m ModelInfo) Ref() ModelRef {
	id := m.ID
	if id == "" {
		id = m.ModelID
	}
	return ModelRef{
		ID:         id,
		ProviderID: m.ProviderID,
	}
}

// ListModels fetches available models from GET /api/model.
func (c *Client) ListModels(ctx context.Context) ([]ModelInfo, error) {
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, "/api/model", nil, &raw); err != nil {
		return nil, err
	}
	var models []ModelInfo
	if err := decodeDataOrDirect(raw, &models); err != nil {
		return nil, err
	}
	return models, nil
}

// SwitchModel switches the active model for a session (POST /api/session/{sessionID}/model).
func (c *Client) SwitchModel(ctx context.Context, sessionID string, model ModelRef) error {
	path := fmt.Sprintf("/api/session/%s/model", url.PathEscape(sessionID))
	body := map[string]ModelRef{"model": model}
	return c.doJSON(ctx, http.MethodPost, path, body, nil)
}

// GenerateText runs a stateless text generation request (POST /api/experimental/generate/text)
// without creating a session or mutating conversation history.
func (c *Client) GenerateText(ctx context.Context, prompt string, model *ModelRef) (string, error) {
	body := map[string]any{"prompt": prompt}
	if model != nil && model.ID != "" && model.ProviderID != "" {
		body["model"] = *model
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodPost, "/api/experimental/generate/text", body, &raw); err != nil {
		return "", err
	}
	var res struct {
		Text string `json:"text"`
	}
	if err := decodeDataOrDirect(raw, &res); err != nil {
		return "", err
	}
	return strings.TrimSpace(res.Text), nil
}

// CommandInfo describes an OpenCode slash command.
type CommandInfo struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

// ListCommands fetches registered commands from GET /api/command.
func (c *Client) ListCommands(ctx context.Context, directory string) ([]CommandInfo, error) {
	path := "/api/command"
	if directory != "" {
		v := url.Values{}
		v.Set("location[directory]", directory)
		path += "?" + v.Encode()
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var commands []CommandInfo
	if err := decodeDataOrDirect(raw, &commands); err != nil {
		return nil, err
	}
	return commands, nil
}

// RunCommand executes a slash command in a session (POST /api/session/{sessionID}/command).
func (c *Client) RunCommand(ctx context.Context, sessionID, name, text string) error {
	path := fmt.Sprintf("/api/session/%s/command", url.PathEscape(sessionID))
	body := map[string]any{
		"name": name,
		"text": text,
	}
	return c.doJSON(ctx, http.MethodPost, path, body, nil)
}

// SkillInfo describes an available skill.
type SkillInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Path        string `json:"path,omitempty"`
}

// ListSkills fetches registered skills from GET /api/skill.
func (c *Client) ListSkills(ctx context.Context, directory string) ([]SkillInfo, error) {
	path := "/api/skill"
	if directory != "" {
		v := url.Values{}
		v.Set("location[directory]", directory)
		path += "?" + v.Encode()
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var skills []SkillInfo
	if err := decodeDataOrDirect(raw, &skills); err != nil {
		return nil, err
	}
	return skills, nil
}

// FoundFile describes a file or directory discovered in the workspace.
type FoundFile struct {
	Path string `json:"path"`
	Type string `json:"type"` // "file" or "directory"
}

// FindFiles searches for files or directories in the workspace (GET /api/fs/find).
func (c *Client) FindFiles(ctx context.Context, directory, query string, limit int) ([]FoundFile, error) {
	v := url.Values{}
	if directory != "" {
		v.Set("location[directory]", directory)
	}
	v.Set("query", query)
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	path := "/api/fs/find?" + v.Encode()
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var files []FoundFile
	if err := decodeDataOrDirect(raw, &files); err != nil {
		return nil, err
	}
	return files, nil
}

// FileAttachment represents an image or file attachment attached to a message.
type FileAttachment struct {
	Mime string `json:"mime"`
	Name string `json:"name"`
	Data string `json:"data"` // raw base64-encoded bytes
}

// ShellItem represents a background shell process managed by OpenCode.
type ShellItem struct {
	ID          string         `json:"id"`
	PID         int            `json:"pid"`
	SessionID   string         `json:"sessionID,omitempty"`
	CallID      string         `json:"callID,omitempty"`
	Command     string         `json:"command"`
	Description string         `json:"description,omitempty"`
	Cwd         string         `json:"cwd,omitempty"`
	LocationDir string         `json:"-"`
	File        string         `json:"file,omitempty"`
	OutputPath  string         `json:"outputPath,omitempty"`
	Status      string         `json:"status"` // "running", "exited", "timeout", "killed"
	Exit        *int           `json:"exit,omitempty"`
	ExitCode    *int           `json:"exitCode,omitempty"`
	Signal      string         `json:"signal,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
	Output      *struct {
		File string `json:"file,omitempty"`
	} `json:"output,omitempty"`
	Source *struct {
		Type      string `json:"type,omitempty"`
		SessionID string `json:"sessionID,omitempty"`
		CallID    string `json:"callID,omitempty"`
	} `json:"source,omitempty"`
	Time struct {
		Started   int64 `json:"started,omitempty"`
		Created   int64 `json:"created,omitempty"`
		Completed int64 `json:"completed,omitempty"`
	} `json:"time,omitempty"`
}

// EffectiveSessionID returns the session ID associated with the shell (checking top-level, source, and metadata).
func (s ShellItem) EffectiveSessionID() string {
	if s.SessionID != "" {
		return s.SessionID
	}
	if s.Source != nil && s.Source.SessionID != "" {
		return s.Source.SessionID
	}
	if s.Metadata != nil {
		if id, ok := s.Metadata["sessionID"].(string); ok {
			return id
		}
	}
	return ""
}

// EffectiveFile returns the on-disk output capture file path for the shell.
func (s ShellItem) EffectiveFile() string {
	if s.File != "" {
		return s.File
	}
	if s.Output != nil && s.Output.File != "" {
		return s.Output.File
	}
	return s.OutputPath
}

// EffectiveLocationDir returns the location directory for the shell, falling back to Cwd.
func (s ShellItem) EffectiveLocationDir() string {
	if s.LocationDir != "" {
		return s.LocationDir
	}
	return s.Cwd
}

// ShellOutput represents a page of stdout/stderr from GET /api/shell/{id}/output.
type ShellOutput struct {
	Output    string `json:"output"`
	Cursor    int    `json:"cursor"`
	Size      int    `json:"size"`
	Truncated bool   `json:"truncated"`
}

// ListShells lists background shells from GET /api/shell.
func (c *Client) ListShells(ctx context.Context, directory string) ([]ShellItem, error) {
	path := "/api/shell"
	if directory != "" {
		v := url.Values{}
		v.Set("location[directory]", directory)
		path += "?" + v.Encode()
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var env struct {
		Location Location `json:"location"`
	}
	_ = json.Unmarshal(raw, &env)
	locDir := env.Location.Directory
	if locDir == "" {
		locDir = directory
	}
	var shells []ShellItem
	if err := decodeDataOrDirect(raw, &shells); err != nil {
		return nil, err
	}
	for i := range shells {
		if shells[i].LocationDir == "" {
			shells[i].LocationDir = locDir
		}
	}
	return shells, nil
}

// GetShellOutput fetches output from a shell (GET /api/shell/{id}/output).
func (c *Client) GetShellOutput(ctx context.Context, shellID, directory string, cursor, limit int) (*ShellOutput, error) {
	v := url.Values{}
	if directory != "" {
		v.Set("location[directory]", directory)
	}
	if cursor > 0 {
		v.Set("cursor", strconv.Itoa(cursor))
	}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	path := fmt.Sprintf("/api/shell/%s/output", url.PathEscape(shellID))
	if len(v) > 0 {
		path += "?" + v.Encode()
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var out ShellOutput
	if err := decodeDataOrDirect(raw, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// KillShell terminates a running background shell (DELETE /api/shell/{id}).
func (c *Client) KillShell(ctx context.Context, shellID string, directory ...string) error {
	path := fmt.Sprintf("/api/shell/%s", url.PathEscape(shellID))
	if len(directory) > 0 && directory[0] != "" {
		v := url.Values{}
		v.Set("location[directory]", directory[0])
		path += "?" + v.Encode()
	}
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// ToolContentItem is a piece of tool output.
type ToolContentItem struct {
	Type string `json:"type"` // "text" or "file"
	Text string `json:"text,omitempty"`
	Mime string `json:"mime,omitempty"`
	Name string `json:"name,omitempty"`
	Data string `json:"data,omitempty"`
	URI  string `json:"uri,omitempty"`
}

// ImageData returns the image payload from either Data or URI.
func (item ToolContentItem) ImageData() string {
	if item.Data != "" {
		return item.Data
	}
	return item.URI
}

// ToolState holds the execution state of a tool invocation.
type ToolState struct {
	Status   string            `json:"status"` // "streaming", "running", "completed", "error"
	Input    json.RawMessage   `json:"input,omitempty"`
	Content  []ToolContentItem `json:"content,omitempty"`
	Error    any               `json:"error,omitempty"`
	Metadata map[string]any    `json:"metadata,omitempty"`
}

// ContentPart is an item inside an assistant message's content array.
type ContentPart struct {
	Type  string    `json:"type"` // "text", "reasoning", "tool"
	Text  string    `json:"text,omitempty"`
	ID    string    `json:"id,omitempty"`
	Name  string    `json:"name,omitempty"`
	State ToolState `json:"state,omitempty"`
}

// MessageRetry describes scheduled retry metadata on an in-progress assistant message.
type MessageRetry struct {
	Attempt int   `json:"attempt"`
	At      int64 `json:"at,omitempty"`
}

// Message represents a timeline entry in a session.
type Message struct {
	ID            string           `json:"id"`
	Type          string           `json:"type"` // "user", "assistant", "synthetic", "system", "skill", "shell", "compaction", "agent-switched", "model-switched", "location-switched"
	Text          string           `json:"text,omitempty"`
	Agent         string           `json:"agent,omitempty"`
	Model         ModelRef         `json:"model,omitempty"`
	Name          string           `json:"name,omitempty"`     // for type == "skill"
	Skill         string           `json:"skill,omitempty"`    // for type == "skill"
	Location      Location         `json:"location,omitempty"` // for type == "location-switched"
	Files         []FileAttachment `json:"files,omitempty"`
	Summary       string           `json:"summary,omitempty"`     // for type == "compaction"
	Status        string           `json:"status,omitempty"`      // for type == "compaction" ("running", "completed", "failed")
	Reason        string           `json:"reason,omitempty"`      // for type == "compaction" ("auto", "manual")
	Previous      string           `json:"previous,omitempty"`    // for type == "agent-switched"
	PreviousModel *ModelRef        `json:"-"`                     // for type == "model-switched"
	Description   string           `json:"description,omitempty"` // for type == "synthetic" or "system"
	Metadata      map[string]any   `json:"metadata,omitempty"`    // for type == "synthetic", "system", or "compaction"
	Content       []ContentPart    `json:"content,omitempty"`
	Cost          float64          `json:"cost,omitempty"`
	Tokens        TokenUsage       `json:"tokens,omitempty"`
	Retry         *MessageRetry    `json:"retry,omitempty"`
	Time          struct {
		Created   int64 `json:"created"`
		Streamed  int64 `json:"streamed,omitempty"`
		Completed int64 `json:"completed,omitempty"`
	} `json:"time"`
}

// UnmarshalJSON unmarshals a Message, gracefully handling the "previous" field which can be
// either a string (for agent-switched) or an object (for model-switched / location-switched).
func (m *Message) UnmarshalJSON(b []byte) error {
	type rawMessage Message
	var raw struct {
		rawMessage
		Previous json.RawMessage `json:"previous,omitempty"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*m = Message(raw.rawMessage)
	if len(raw.Previous) > 0 {
		switch raw.Previous[0] {
		case '"':
			_ = json.Unmarshal(raw.Previous, &m.Previous)
		case '{':
			var pm ModelRef
			if json.Unmarshal(raw.Previous, &pm) == nil && pm.ID != "" {
				m.PreviousModel = &pm
			}
		}
	}
	return nil
}

// FormatCompactionTokens formats token usage for a compaction message or event.
// It returns an empty string if both inTokens and outTokens are zero.
// Numbers >= 1000 are formatted using "%.1fK" (e.g. 257765 -> "257.8K"),
// and numbers < 1000 are formatted as integers.
func FormatCompactionTokens(tokens TokenUsage) string {
	inTokens := tokens.Input + tokens.Cache.Read + tokens.Cache.Write
	outTokens := tokens.Output + tokens.Reasoning
	if inTokens == 0 && outTokens == 0 {
		return ""
	}

	formatNum := func(n int) string {
		if n >= 1000 {
			return fmt.Sprintf("%.1fK", float64(n)/1000.0)
		}
		return fmt.Sprintf("%d", n)
	}

	return fmt.Sprintf("%s in · %s out", formatNum(inTokens), formatNum(outTokens))
}

// FormatContextUsage formats the active context token count and optional context percentage.
// It inspects the latest assistant message with Input + Cache.Read > 0, falling back to sessionTokens.
func FormatContextUsage(messages []Message, sessionTokens TokenUsage, contextLimit int) string {
	var currentTokens int
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Type != "" && msg.Type != "assistant" {
			continue
		}
		if total := msg.Tokens.Input + msg.Tokens.Cache.Read; total > 0 {
			currentTokens = total
			break
		}
	}
	if currentTokens == 0 {
		currentTokens = sessionTokens.Input + sessionTokens.Cache.Read
	}
	if currentTokens <= 0 {
		return "0K"
	}

	var kStr string
	if currentTokens < 500 {
		kStr = "<1K"
	} else {
		kStr = fmt.Sprintf("%dK", (currentTokens+500)/1000)
	}

	if contextLimit > 0 {
		pct := int(math.Round(float64(currentTokens) / float64(contextLimit) * 100))
		return fmt.Sprintf("%s (%d%%)", kStr, pct)
	}
	return kStr
}

// FormatMessageTokPerSec calculates the tokens-per-second rate of the latest completed
// assistant turn from the provided message history.
func FormatMessageTokPerSec(messages []Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		msg := messages[i]
		if msg.Type != "" && msg.Type != "assistant" {
			continue
		}
		totalTok := msg.Tokens.Output + msg.Tokens.Reasoning
		if totalTok <= 0 {
			continue
		}
		end := msg.Time.Streamed
		if end <= 0 {
			end = msg.Time.Completed
		}
		if end <= 0 || msg.Time.Created <= 0 {
			continue
		}
		durMs := end - msg.Time.Created
		if durMs < 50 {
			return ""
		}
		rate := float64(totalTok) / (float64(durMs) / 1000.0)
		return fmt.Sprintf("%.1f tok/s", rate)
	}
	return ""
}

// MessagePage holds a page of messages and an optional cursor for pagination.
type MessagePage struct {
	Messages   []Message
	NextCursor string
}

// ListMessagesPage fetches a page of messages for a session.
// When cursor is empty, it requests newest-first (order=desc) with the given limit
// (capped at 200, default 30 if <= 0).
// When cursor is non-empty, it requests older history using the cursor without order=desc.
// Returned messages are always reversed to chronological order (oldest to newest within the page).
func (c *Client) ListMessagesPage(ctx context.Context, sessionID string, limit int, cursor string) (*MessagePage, error) {
	if limit <= 0 {
		limit = 30
	} else if limit > 200 {
		limit = 200
	}

	v := url.Values{}
	v.Set("limit", strconv.Itoa(limit))
	if cursor == "" {
		v.Set("order", "desc")
	} else {
		v.Set("cursor", cursor)
	}

	path := fmt.Sprintf("/api/session/%s/message?%s", url.PathEscape(sessionID), v.Encode())
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}

	trimmed := bytes.TrimSpace(raw)
	var msgs []Message
	var nextCursor string

	if len(trimmed) > 0 && trimmed[0] == '{' {
		var env struct {
			Cursor *struct {
				Previous string `json:"previous,omitempty"`
				Next     string `json:"next,omitempty"`
			} `json:"cursor,omitempty"`
			Data []Message `json:"data"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, err
		}
		msgs = env.Data
		if env.Cursor != nil {
			nextCursor = env.Cursor.Next
		}
	} else {
		if err := json.Unmarshal(raw, &msgs); err != nil {
			return nil, err
		}
	}

	slices.Reverse(msgs)
	return &MessagePage{
		Messages:   msgs,
		NextCursor: nextCursor,
	}, nil
}

// ListMessages fetches messages for a session in ascending chronological order.
func (c *Client) ListMessages(ctx context.Context, sessionID string, limit int) ([]Message, error) {
	page, err := c.ListMessagesPage(ctx, sessionID, limit, "")
	if err != nil {
		return nil, err
	}
	return page.Messages, nil
}

// InboxDelivery specifies how a prompt or inbox item should be delivered.
type InboxDelivery string

const (
	// DeliveryQueue enqueues the prompt to be delivered when the session is ready.
	DeliveryQueue InboxDelivery = "queue"
	// DeliverySteer delivers the prompt at the next step boundary, waking or steering execution.
	DeliverySteer InboxDelivery = "steer"
)

// InboxPayload holds the text and file attachments of a user InboxItem.
type InboxPayload struct {
	Text  string           `json:"text"`
	Files []FileAttachment `json:"files,omitempty"`
}

// InboxItem represents a pending item in a session's durable inbox. Its ID
// becomes the message ID once the item is delivered.
type InboxItem struct {
	ID       string        `json:"id"`
	Type     string        `json:"type"` // "user", "synthetic", "compaction", "move"
	Payload  InboxPayload  `json:"payload"`
	Delivery InboxDelivery `json:"delivery,omitempty"`
	Time     struct {
		Created int64 `json:"created"`
	} `json:"time"`
}

// PromptMention describes the text and character span of an @ mention in a prompt.
type PromptMention struct {
	Start int    `json:"start"`
	End   int    `json:"end"`
	Text  string `json:"text"`
}

// PromptAgentInput represents an @agent mention attached to a prompt.
type PromptAgentInput struct {
	Name    string         `json:"name"`
	Mention *PromptMention `json:"mention,omitempty"`
}

// PromptSkillInput represents a skill mention attached to a prompt.
type PromptSkillInput struct {
	ID      string         `json:"id"`
	Mention *PromptMention `json:"mention,omitempty"`
}

// PromptOptions holds text and rich attachments for a prompt request.
type PromptOptions struct {
	Text     string
	Files    []FileAttachment   // inline base64 images
	Paths    []PromptFileInput  // file:/// URI attachments with optional Mention
	Agents   []PromptAgentInput // @agent mentions
	Skills   []PromptSkillInput // @skill mentions
	Delivery InboxDelivery      // "queue" or "steer"
	NoResume bool               // when true, enqueues without triggering immediate session resume
}

// PromptFileSource indicates how file content is provided in prompt inputs.
type PromptFileSource struct {
	Type string `json:"type,omitempty"`
	URI  string `json:"uri,omitempty"`
}

// PromptFileInput represents an attached file payload in a prompt request.
type PromptFileInput struct {
	URI         string            `json:"uri"`            // e.g. "data:image/png;base64,..." or "file:///..."
	Name        string            `json:"name,omitempty"` // e.g. "clipboard.png"
	Description string            `json:"description,omitempty"`
	Source      *PromptFileSource `json:"source,omitempty"`
	Mention     *PromptMention    `json:"mention,omitempty"`
}

// PromptRequest is the payload sent to POST /api/session/{sessionID}/prompt.
type PromptRequest struct {
	Text     string             `json:"text"`
	Files    []PromptFileInput  `json:"files,omitempty"`
	Agents   []PromptAgentInput `json:"agents,omitempty"`
	Skills   []PromptSkillInput `json:"skills,omitempty"`
	Delivery InboxDelivery      `json:"delivery,omitempty"`
	Resume   bool               `json:"resume,omitempty"`
}

// SendPrompt sends a user prompt with options to a session and returns the created inbox item.
func (c *Client) SendPrompt(ctx context.Context, sessionID string, opts PromptOptions) (*InboxItem, error) {
	var inputFiles []PromptFileInput
	for _, f := range opts.Files {
		uri := f.Data
		if !strings.HasPrefix(uri, "data:") {
			mime := f.Mime
			if mime == "" {
				mime = "image/png"
			}
			uri = fmt.Sprintf("data:%s;base64,%s", mime, f.Data)
		}
		inputFiles = append(inputFiles, PromptFileInput{
			URI:    uri,
			Name:   f.Name,
			Source: &PromptFileSource{Type: "inline"},
		})
	}
	inputFiles = append(inputFiles, opts.Paths...)

	text := opts.Text
	trimmed := strings.TrimSpace(text)
	if trimmed == "" && len(inputFiles) > 0 {
		var parts []string
		for i, f := range inputFiles {
			name := f.Name
			if name == "" {
				name = fmt.Sprintf("Image %d", i+1)
			}
			parts = append(parts, fmt.Sprintf("[%s]", name))
		}
		text = strings.Join(parts, " ")
	}

	req := PromptRequest{
		Text:     text,
		Files:    inputFiles,
		Agents:   opts.Agents,
		Skills:   opts.Skills,
		Delivery: opts.Delivery,
		Resume:   !opts.NoResume,
	}
	path := fmt.Sprintf("/api/session/%s/prompt", url.PathEscape(sessionID))
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodPost, path, req, &raw); err != nil {
		return nil, err
	}
	var item InboxItem
	if err := decodeDataOrDirect(raw, &item); err != nil {
		return nil, nil
	}
	return &item, nil
}

// PromptWithOptions sends a user prompt with attachments, file paths, and mentions to a session.
func (c *Client) PromptWithOptions(ctx context.Context, sessionID string, opts PromptOptions) error {
	_, err := c.SendPrompt(ctx, sessionID, opts)
	return err
}

// PromptWithFiles sends a user prompt with file attachments to a session.
func (c *Client) PromptWithFiles(ctx context.Context, sessionID, text string, files []FileAttachment) error {
	return c.PromptWithOptions(ctx, sessionID, PromptOptions{
		Text:  text,
		Files: files,
	})
}

// Prompt sends a user prompt to a session.
func (c *Client) Prompt(ctx context.Context, sessionID, text string) error {
	return c.PromptWithOptions(ctx, sessionID, PromptOptions{
		Text: text,
	})
}

// ListInbox lists pending inbox items for a session (GET /api/session/{sessionID}/inbox).
func (c *Client) ListInbox(ctx context.Context, sessionID string) ([]InboxItem, error) {
	path := fmt.Sprintf("/api/session/%s/inbox", url.PathEscape(sessionID))
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var items []InboxItem
	if err := decodeDataOrDirect(raw, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// UpdateInboxDelivery updates the delivery mode of a pending inbox item
// (PATCH /api/session/{sessionID}/inbox/{inboxID}). The server answers 204, or
// 409 if the item is no longer pending.
func (c *Client) UpdateInboxDelivery(ctx context.Context, sessionID, inboxID string, delivery InboxDelivery) error {
	path := fmt.Sprintf("/api/session/%s/inbox/%s", url.PathEscape(sessionID), url.PathEscape(inboxID))
	body := map[string]any{"delivery": delivery}
	return c.doJSON(ctx, http.MethodPatch, path, body, nil)
}

// CancelInbox cancels a pending inbox item (DELETE /api/session/{sessionID}/inbox/{inboxID}).
// The server answers 204 even if the item has already been delivered.
func (c *Client) CancelInbox(ctx context.Context, sessionID, inboxID string) error {
	path := fmt.Sprintf("/api/session/%s/inbox/%s", url.PathEscape(sessionID), url.PathEscape(inboxID))
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// Interrupt stops ongoing execution in a session.
func (c *Client) Interrupt(ctx context.Context, sessionID string) error {
	path := fmt.Sprintf("/api/session/%s/interrupt", url.PathEscape(sessionID))
	return c.doJSON(ctx, http.MethodPost, path, map[string]any{}, nil)
}

// BackgroundSession moves active foreground backgroundable tools for a session into background observation
// (POST /api/session/{sessionID}/background).
func (c *Client) BackgroundSession(ctx context.Context, sessionID string) error {
	path := fmt.Sprintf("/api/session/%s/background", url.PathEscape(sessionID))
	return c.doJSON(ctx, http.MethodPost, path, map[string]any{}, nil)
}

// FormOption describes a single selectable choice in a FormField.
type FormOption struct {
	Value       string `json:"value"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// FormField describes an interactive input field or question in a FormInfo.
type FormField struct {
	Key         string       `json:"key"`
	Title       string       `json:"title"`
	Description string       `json:"description,omitempty"`
	Type        string       `json:"type"` // "string", "multiselect", "boolean"
	Options     []FormOption `json:"options,omitempty"`
	Custom      bool         `json:"custom,omitempty"`
	Optional    bool         `json:"optional,omitempty"`
}

// FormInfo represents a pending interactive form or question prompt in a session.
type FormInfo struct {
	ID          string         `json:"id"`
	SessionID   string         `json:"sessionID"`
	Title       string         `json:"title"`
	Description string         `json:"description,omitempty"`
	Fields      []FormField    `json:"fields"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// ListForms lists pending forms across sessions (GET /api/form).
func (c *Client) ListForms(ctx context.Context, directory string) ([]FormInfo, error) {
	path := "/api/form"
	if directory != "" {
		v := url.Values{}
		v.Set("directory", directory)
		path += "?" + v.Encode()
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var forms []FormInfo
	if err := decodeDataOrDirect(raw, &forms); err != nil {
		return nil, err
	}
	return forms, nil
}

// ListSessionForms lists pending forms for a session (GET /api/session/{sessionID}/form).
func (c *Client) ListSessionForms(ctx context.Context, sessionID string) ([]FormInfo, error) {
	path := fmt.Sprintf("/api/session/%s/form", url.PathEscape(sessionID))
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var forms []FormInfo
	if err := decodeDataOrDirect(raw, &forms); err != nil {
		return nil, err
	}
	return forms, nil
}

// ReplyForm submits answers for a pending form (POST /api/session/{sessionID}/form/{formID}/reply).
// The server answers 204 No Content.
func (c *Client) ReplyForm(ctx context.Context, sessionID, formID string, answer map[string]any) error {
	path := fmt.Sprintf("/api/session/%s/form/%s/reply", url.PathEscape(sessionID), url.PathEscape(formID))
	body := map[string]any{"answer": answer}
	return c.doJSON(ctx, http.MethodPost, path, body, nil)
}

// CancelForm dismisses/cancels a pending form (DELETE /api/session/{sessionID}/form/{formID}).
// The server answers 204 No Content.
func (c *Client) CancelForm(ctx context.Context, sessionID, formID, message string) error {
	path := fmt.Sprintf("/api/session/%s/form/%s", url.PathEscape(sessionID), url.PathEscape(formID))
	if message != "" {
		v := url.Values{}
		v.Set("message", message)
		path += "?" + v.Encode()
	}
	return c.doJSON(ctx, http.MethodDelete, path, nil, nil)
}

// PermissionDecision represents the reply decision for a pending permission request.
type PermissionDecision string

const (
	PermissionOnce   PermissionDecision = "once"
	PermissionAlways PermissionDecision = "always"
	PermissionReject PermissionDecision = "reject"
)

// PermissionSource links a permission request to a specific tool call.
type PermissionSource struct {
	Type      string `json:"type,omitempty"`
	MessageID string `json:"messageID,omitempty"`
	ID        string `json:"id,omitempty"`
}

// PermissionRequest represents a pending tool execution authorization request in OpenCode V2.
type PermissionRequest struct {
	ID        string            `json:"id"`
	SessionID string            `json:"sessionID"`
	Action    string            `json:"action"`
	Resources []string          `json:"resources"`
	Save      []string          `json:"save,omitempty"`
	Metadata  map[string]any    `json:"metadata,omitempty"`
	Source    *PermissionSource `json:"source,omitempty"`
	Message   string            `json:"message,omitempty"`
}

// ListPermissions lists pending permission requests across a project directory (GET /api/permission/request).
func (c *Client) ListPermissions(ctx context.Context, directory string) ([]PermissionRequest, error) {
	path := "/api/permission/request"
	if directory != "" {
		v := url.Values{}
		v.Set("location[directory]", directory)
		path += "?" + v.Encode()
	}
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var reqs []PermissionRequest
	if err := decodeDataOrDirect(raw, &reqs); err != nil {
		return nil, err
	}
	return reqs, nil
}

// ListSessionPermissions lists pending permission requests for a session (GET /api/session/{sessionID}/permission).
func (c *Client) ListSessionPermissions(ctx context.Context, sessionID string) ([]PermissionRequest, error) {
	path := fmt.Sprintf("/api/session/%s/permission", url.PathEscape(sessionID))
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var reqs []PermissionRequest
	if err := decodeDataOrDirect(raw, &reqs); err != nil {
		return nil, err
	}
	return reqs, nil
}

// ReplyPermission submits a decision ("once", "always", or "reject") for a pending permission request
// (POST /api/session/{sessionID}/permission/{requestID}/reply). The server answers 204 No Content.
func (c *Client) ReplyPermission(ctx context.Context, sessionID, requestID string, decision PermissionDecision, message string) error {
	path := fmt.Sprintf("/api/session/%s/permission/%s/reply", url.PathEscape(sessionID), url.PathEscape(requestID))
	body := map[string]any{
		"decision": string(decision),
	}
	if message != "" {
		body["message"] = message
	}
	return c.doJSON(ctx, http.MethodPost, path, body, nil)
}

// Event is an SSE event emitted by /api/event.
type Event struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Location Location        `json:"location,omitempty"`
	Data     json.RawMessage `json:"data"`
}

// SubscribeEvents connects to /api/event and invokes fn for each decoded event.
func (c *Client) SubscribeEvents(ctx context.Context, fn func(Event)) error {
	return c.subscribeEvents(ctx, fn, nil)
}

func (c *Client) subscribeEvents(ctx context.Context, fn func(Event), onConnected func()) error {
	resp, err := c.do(ctx, func() (*http.Request, error) {
		req, err := c.newRequest(ctx, http.MethodGet, "/api/event", nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set("Cache-Control", "no-cache")
		return req, nil
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("sse http %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	if onConnected != nil {
		onConnected()
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		eventID   string
		eventType string
		dataBuf   bytes.Buffer
	)
	flush := func() {
		if dataBuf.Len() == 0 {
			eventID = ""
			eventType = ""
			return
		}
		rawBytes := dataBuf.Bytes()
		var ev Event
		if err := json.Unmarshal(rawBytes, &ev); err == nil && ev.Type != "" {
			if ev.ID == "" && eventID != "" {
				ev.ID = eventID
			}
			fn(ev)
		} else if eventType != "" {
			fn(Event{
				ID:   eventID,
				Type: eventType,
				Data: append(json.RawMessage(nil), rawBytes...),
			})
		}
		dataBuf.Reset()
		eventID = ""
		eventType = ""
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		if after, ok := strings.CutPrefix(line, "id:"); ok {
			eventID = strings.TrimSpace(after)
			continue
		}
		if after, ok := strings.CutPrefix(line, "event:"); ok {
			eventType = strings.TrimSpace(after)
			continue
		}
		if after, ok := strings.CutPrefix(line, "data:"); ok {
			if dataBuf.Len() > 0 {
				dataBuf.WriteByte('\n')
			}
			dataBuf.WriteString(strings.TrimSpace(after))
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	flush()
	return ctx.Err()
}

// SubscribeEventsLoop continuously subscribes to /api/event, reconnecting after
// retryDelay whenever the stream disconnects or fails until ctx is canceled.
// Optional onReconnect callback is invoked each time a reconnection succeeds.
func (c *Client) SubscribeEventsLoop(ctx context.Context, retryDelay time.Duration, fn func(Event), onReconnect func()) error {
	if retryDelay <= 0 {
		retryDelay = 2 * time.Second
	}
	first := true
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		isReconnect := !first
		first = false

		_ = c.subscribeEvents(ctx, fn, func() {
			if isReconnect && onReconnect != nil {
				onReconnect()
			}
		})
		if err := ctx.Err(); err != nil {
			return err
		}
		timer := time.NewTimer(retryDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
