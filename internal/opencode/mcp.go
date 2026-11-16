package opencode

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
)

// McpStatus represents the runtime connection state of an MCP server.
type McpStatus struct {
	Status string `json:"status"` // "connected", "pending", "disabled", "failed", "needs_auth"
	Error  string `json:"error,omitempty"`
}

// McpServer describes an MCP server returned by GET /api/mcp.
type McpServer struct {
	Name          string    `json:"name"`
	Status        McpStatus `json:"status"`
	Title         string    `json:"title,omitempty"`
	IntegrationID string    `json:"integrationID,omitempty"`
}

// McpListResult pairs the server-resolved Location with its MCP server list.
type McpListResult struct {
	Location Location
	Servers  []McpServer
}

// McpResource describes a static MCP resource exposed by a connected server.
type McpResource struct {
	Server      string `json:"server"`
	Name        string `json:"name"`
	URI         string `json:"uri"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// McpResourceTemplate describes a parameterized MCP resource template.
type McpResourceTemplate struct {
	Server      string `json:"server"`
	Name        string `json:"name"`
	URITemplate string `json:"uriTemplate"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// McpResourceCatalog aggregates resources and templates returned by GET /api/mcp/resource.
type McpResourceCatalog struct {
	Resources []McpResource         `json:"resources"`
	Templates []McpResourceTemplate `json:"templates"`
}

// McpResourceListResult pairs the server-resolved Location with its resource catalog.
type McpResourceListResult struct {
	Location Location
	Catalog  McpResourceCatalog
}

// McpTimeoutConfig specifies per-phase MCP timeouts in milliseconds.
type McpTimeoutConfig struct {
	Startup   int64 `json:"startup,omitempty"`
	Catalog   int64 `json:"catalog,omitempty"`
	Execution int64 `json:"execution,omitempty"`
}

// McpServerConfig holds non-secret MCP configuration metadata for UI display.
// Header and environment values as well as OAuth client secrets are discarded
// during JSON decoding and never retained in memory.
type McpServerConfig struct {
	Type       string
	Command    []string
	Cwd        string
	EnvKeys    []string
	URL        string
	HeaderKeys []string
	Disabled   *bool
	CodeMode   *bool
	Protocol   string
	Pool       *bool
	Timeout    *McpTimeoutConfig
	SourcePath string
}

// ConfigEntry represents a single entry from GET /api/config with MCP secrets stripped.
type ConfigEntry struct {
	Type       string
	Path       string
	McpTimeout *McpTimeoutConfig
	McpServers map[string]McpServerConfig
}

type rawMcpServerConfig struct {
	Type        string            `json:"type"`
	Command     []string          `json:"command,omitempty"`
	Cwd         string            `json:"cwd,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Disabled    *bool             `json:"disabled,omitempty"`
	CodeMode    *bool             `json:"codemode,omitempty"`
	Protocol    string            `json:"protocol,omitempty"`
	Pool        *bool             `json:"pool,omitempty"`
	Timeout     *McpTimeoutConfig `json:"timeout,omitempty"`
	// Note: oauth (including client_secret) is intentionally omitted so secrets are never decoded.
}

type rawConfigEntry struct {
	Type string `json:"type"`
	Path string `json:"path,omitempty"`
	Info *struct {
		Mcp *struct {
			Timeout *McpTimeoutConfig             `json:"timeout,omitempty"`
			Servers map[string]rawMcpServerConfig `json:"servers,omitempty"`
		} `json:"mcp,omitempty"`
	} `json:"info,omitempty"`
}

// UnmarshalJSON decodes a Config.Entry while stripping secret values immediately.
func (e *ConfigEntry) UnmarshalJSON(data []byte) error {
	var raw rawConfigEntry
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	e.Type = raw.Type
	e.Path = raw.Path
	e.McpTimeout = nil
	e.McpServers = nil
	if raw.Info == nil || raw.Info.Mcp == nil {
		return nil
	}
	if raw.Info.Mcp.Timeout != nil {
		t := *raw.Info.Mcp.Timeout
		e.McpTimeout = &t
	}
	if len(raw.Info.Mcp.Servers) > 0 {
		e.McpServers = make(map[string]McpServerConfig, len(raw.Info.Mcp.Servers))
		for name, srv := range raw.Info.Mcp.Servers {
			var envKeys []string
			if len(srv.Environment) > 0 {
				envKeys = make([]string, 0, len(srv.Environment))
				for k := range srv.Environment {
					envKeys = append(envKeys, k)
				}
				slices.Sort(envKeys)
			}
			var headerKeys []string
			if len(srv.Headers) > 0 {
				headerKeys = make([]string, 0, len(srv.Headers))
				for k := range srv.Headers {
					headerKeys = append(headerKeys, k)
				}
				slices.Sort(headerKeys)
			}
			var timeoutCopy *McpTimeoutConfig
			if srv.Timeout != nil {
				t := *srv.Timeout
				timeoutCopy = &t
			}
			e.McpServers[name] = McpServerConfig{
				Type:       srv.Type,
				Command:    slices.Clone(srv.Command),
				Cwd:        srv.Cwd,
				EnvKeys:    envKeys,
				URL:        SanitizeMcpURL(srv.URL),
				HeaderKeys: headerKeys,
				Disabled:   srv.Disabled,
				CodeMode:   srv.CodeMode,
				Protocol:   srv.Protocol,
				Pool:       srv.Pool,
				Timeout:    timeoutCopy,
				SourcePath: raw.Path,
			}
		}
	}
	return nil
}

// SanitizeMcpURL redacts userinfo passwords and secret-bearing query parameter values.
func SanitizeMcpURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	changed := false
	if u.User != nil {
		if _, hasPass := u.User.Password(); hasPass {
			u.User = url.UserPassword(u.User.Username(), "REDACTED")
			changed = true
		} else if u.User.Username() != "" {
			u.User = url.User("REDACTED")
			changed = true
		}
	}
	if u.RawQuery != "" {
		q := u.Query()
		queryChanged := false
		for k := range q {
			if isSensitiveQueryParam(k) {
				vals := q[k]
				for i := range vals {
					vals[i] = "REDACTED"
				}
				q[k] = vals
				queryChanged = true
			}
		}
		if queryChanged {
			u.RawQuery = q.Encode()
			changed = true
		}
	}
	if !changed {
		return raw
	}
	return u.String()
}

func isSensitiveQueryParam(key string) bool {
	lower := strings.ToLower(key)
	for _, needle := range []string{
		"token", "key", "secret", "password", "passwd", "auth",
		"sig", "signature", "credential", "bearer", "cookie", "session",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// MergeMcpConfigs merges layered ConfigEntry items in config order, propagating
// global mcp.timeout defaults to each server while preserving per-server overrides.
func MergeMcpConfigs(entries []ConfigEntry) map[string]McpServerConfig {
	var globalTimeout McpTimeoutConfig
	hasGlobalTimeout := false
	merged := make(map[string]McpServerConfig)

	for _, entry := range entries {
		if entry.McpTimeout != nil {
			if entry.McpTimeout.Startup > 0 {
				globalTimeout.Startup = entry.McpTimeout.Startup
				hasGlobalTimeout = true
			}
			if entry.McpTimeout.Catalog > 0 {
				globalTimeout.Catalog = entry.McpTimeout.Catalog
				hasGlobalTimeout = true
			}
			if entry.McpTimeout.Execution > 0 {
				globalTimeout.Execution = entry.McpTimeout.Execution
				hasGlobalTimeout = true
			}
		}
		for name, srv := range entry.McpServers {
			cfg := srv
			cfg.Command = slices.Clone(srv.Command)
			cfg.EnvKeys = slices.Clone(srv.EnvKeys)
			cfg.HeaderKeys = slices.Clone(srv.HeaderKeys)
			if srv.SourcePath == "" {
				cfg.SourcePath = entry.Path
			}
			if srv.Timeout != nil {
				t := *srv.Timeout
				cfg.Timeout = &t
			}
			merged[name] = cfg
		}
	}

	if hasGlobalTimeout {
		for name, cfg := range merged {
			t := globalTimeout
			if cfg.Timeout != nil {
				if cfg.Timeout.Startup > 0 {
					t.Startup = cfg.Timeout.Startup
				}
				if cfg.Timeout.Catalog > 0 {
					t.Catalog = cfg.Timeout.Catalog
				}
				if cfg.Timeout.Execution > 0 {
					t.Execution = cfg.Timeout.Execution
				}
			}
			cfg.Timeout = &t
			merged[name] = cfg
		}
	}

	return merged
}

// HumanizeAPIError turns wrapped OpenCode HTTP error strings (e.g. "http 404: {...}")
// into clean user-facing error text.
func HumanizeAPIError(err error) string {
	if err == nil {
		return ""
	}
	raw := strings.TrimSpace(err.Error())
	if raw == "" {
		return ""
	}
	prefix := ""
	body := raw
	if strings.HasPrefix(raw, "http ") {
		if colon := strings.Index(raw, ": "); colon > 0 {
			codePart := strings.TrimPrefix(raw[:colon], "http ")
			prefix = "HTTP " + strings.TrimSpace(codePart)
			body = strings.TrimSpace(raw[colon+2:])
		}
	}
	if strings.HasPrefix(body, "{") {
		var envelope struct {
			Message string `json:"message"`
			Data    struct {
				Message string `json:"message"`
				Error   string `json:"error"`
			} `json:"data"`
			Error any `json:"error"`
		}
		if json.Unmarshal([]byte(body), &envelope) == nil {
			if msg := strings.TrimSpace(envelope.Message); msg != "" {
				return msg
			}
			if msg := strings.TrimSpace(envelope.Data.Message); msg != "" {
				return msg
			}
			if msg := strings.TrimSpace(envelope.Data.Error); msg != "" {
				return msg
			}
			switch v := envelope.Error.(type) {
			case string:
				if msg := strings.TrimSpace(v); msg != "" {
					return msg
				}
			case map[string]any:
				if msg, ok := v["message"].(string); ok && strings.TrimSpace(msg) != "" {
					return strings.TrimSpace(msg)
				}
			}
		}
	}
	if prefix != "" {
		if body == "" {
			return prefix
		}
		return prefix + ": " + body
	}
	return raw
}

func withLocationDirectory(path, directory string) string {
	if directory == "" {
		return path
	}
	v := url.Values{}
	v.Set("location[directory]", directory)
	return path + "?" + v.Encode()
}

// ListMCP fetches MCP server statuses for directory (GET /api/mcp).
func (c *Client) ListMCP(ctx context.Context, directory string) (*McpListResult, error) {
	path := withLocationDirectory("/api/mcp", directory)
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var env struct {
		Location Location        `json:"location"`
		Data     json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 && string(env.Data) != "null" {
		var servers []McpServer
		if err := json.Unmarshal(env.Data, &servers); err != nil {
			return nil, err
		}
		return &McpListResult{
			Location: env.Location,
			Servers:  servers,
		}, nil
	}
	var servers []McpServer
	if err := json.Unmarshal(raw, &servers); err != nil {
		return nil, err
	}
	return &McpListResult{
		Servers: servers,
	}, nil
}

// ListMCPResources fetches MCP resources and templates for directory (GET /api/mcp/resource).
func (c *Client) ListMCPResources(ctx context.Context, directory string) (*McpResourceListResult, error) {
	path := withLocationDirectory("/api/mcp/resource", directory)
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var env struct {
		Location Location        `json:"location"`
		Data     json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err == nil && len(env.Data) > 0 && string(env.Data) != "null" {
		var catalog McpResourceCatalog
		if err := json.Unmarshal(env.Data, &catalog); err != nil {
			return nil, err
		}
		return &McpResourceListResult{
			Location: env.Location,
			Catalog:  catalog,
		}, nil
	}
	var catalog McpResourceCatalog
	if err := json.Unmarshal(raw, &catalog); err != nil {
		return nil, err
	}
	return &McpResourceListResult{
		Catalog: catalog,
	}, nil
}

// ListConfig fetches layered OpenCode configuration entries (GET /api/config),
// discarding secret values immediately during decode.
func (c *Client) ListConfig(ctx context.Context, directory string) ([]ConfigEntry, error) {
	path := withLocationDirectory("/api/config", directory)
	var raw json.RawMessage
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var entries []ConfigEntry
	if err := decodeDataOrDirect(raw, &entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// ConnectMCP starts or restarts an MCP server (POST /api/experimental/mcp/{server}/connect).
func (c *Client) ConnectMCP(ctx context.Context, serverName, directory string) error {
	path := withLocationDirectory(fmt.Sprintf("/api/experimental/mcp/%s/connect", url.PathEscape(serverName)), directory)
	return c.doJSON(ctx, http.MethodPost, path, nil, nil)
}

// DisconnectMCP stops and disables an MCP server (POST /api/experimental/mcp/{server}/disconnect).
func (c *Client) DisconnectMCP(ctx context.Context, serverName, directory string) error {
	path := withLocationDirectory(fmt.Sprintf("/api/experimental/mcp/%s/disconnect", url.PathEscape(serverName)), directory)
	return c.doJSON(ctx, http.MethodPost, path, nil, nil)
}
