package opencode

import "encoding/json"

// TextStartedEvent is emitted on "session.text.started" and "session.reasoning.started".
type TextStartedEvent struct {
	SessionID          string `json:"sessionID"`
	AssistantMessageID string `json:"assistantMessageID"`
	Ordinal            int    `json:"ordinal"`
}

// TextDeltaEvent is emitted on "session.text.delta" and "session.reasoning.delta".
type TextDeltaEvent struct {
	SessionID          string `json:"sessionID"`
	AssistantMessageID string `json:"assistantMessageID"`
	Ordinal            int    `json:"ordinal"`
	Delta              string `json:"delta"`
}

// TextEndedEvent is emitted on "session.text.ended" and "session.reasoning.ended".
type TextEndedEvent struct {
	SessionID          string `json:"sessionID"`
	AssistantMessageID string `json:"assistantMessageID"`
	Ordinal            int    `json:"ordinal"`
	Text               string `json:"text"`
}

// StepStartedEvent is emitted on "session.step.started".
type StepStartedEvent struct {
	SessionID          string   `json:"sessionID"`
	AssistantMessageID string   `json:"assistantMessageID"`
	Agent              string   `json:"agent"`
	Model              ModelRef `json:"model"`
}

// StepStreamedEvent is emitted on "session.step.streamed".
type StepStreamedEvent struct {
	SessionID          string `json:"sessionID"`
	AssistantMessageID string `json:"assistantMessageID"`
}

// StepEndedEvent is emitted on "session.step.ended".
type StepEndedEvent struct {
	SessionID          string     `json:"sessionID"`
	AssistantMessageID string     `json:"assistantMessageID"`
	Cost               float64    `json:"cost,omitempty"`
	Tokens             TokenUsage `json:"tokens,omitempty"`
	Finish             string     `json:"finish,omitempty"`
}

// StepFailedEvent is emitted on "session.step.failed".
type StepFailedEvent struct {
	SessionID          string `json:"sessionID"`
	AssistantMessageID string `json:"assistantMessageID"`
}

// ToolEvent is emitted on "session.tool.*".
type ToolEvent struct {
	SessionID          string            `json:"sessionID"`
	AssistantMessageID string            `json:"assistantMessageID"`
	ID                 string            `json:"id"`
	Name               string            `json:"name,omitempty"`
	Delta              string            `json:"delta,omitempty"`
	Text               string            `json:"text,omitempty"`
	Input              any               `json:"input,omitempty"`
	Content            []ToolContentItem `json:"content,omitempty"`
	Error              any               `json:"error,omitempty"`
	Metadata           map[string]any    `json:"metadata,omitempty"`
}

// CompactionEvent is emitted on "session.compaction.*" (started, delta, ended, failed).
type CompactionEvent struct {
	SessionID string     `json:"sessionID"`
	Reason    string     `json:"reason,omitempty"`
	InputID   string     `json:"inputID,omitempty"`
	Text      string     `json:"text,omitempty"`
	Model     ModelRef   `json:"model,omitempty"`
	Tokens    TokenUsage `json:"tokens,omitempty"`
	Error     any        `json:"error,omitempty"`
}

// SessionIDEvent is emitted on "session.idle", "session.execution.*", etc.
type SessionIDEvent struct {
	SessionID string `json:"sessionID"`
}

// SessionViewedEvent is emitted on "session.viewed".
type SessionViewedEvent struct {
	SessionID string `json:"sessionID"`
	Idle      int64  `json:"idle"`
}

// AgentSelectedEvent is emitted on "session.agent.selected".
type AgentSelectedEvent struct {
	SessionID string `json:"sessionID"`
	Agent     string `json:"agent"`
	Previous  string `json:"previous,omitempty"`
}

// ModelSelectedEvent is emitted on "session.model.selected".
type ModelSelectedEvent struct {
	SessionID string   `json:"sessionID"`
	Model     ModelRef `json:"model"`
	Previous  ModelRef `json:"previous,omitempty"`
}

// SessionCreatedEvent is emitted on "session.created".
type SessionCreatedEvent struct {
	SessionID string  `json:"sessionID,omitempty"`
	Session   Session `json:"session,omitempty"`
}

// UnmarshalJSON supports both nested {"session": {...}} / {"info": {...}} payloads
// and flat Session / {"sessionID": "..."} payloads for session.created.
func (e *SessionCreatedEvent) UnmarshalJSON(data []byte) error {
	type rawEvent struct {
		SessionID string   `json:"sessionID"`
		Session   *Session `json:"session"`
		Info      *Session `json:"info"`
	}
	var r rawEvent
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	e.SessionID = r.SessionID
	if r.Session != nil {
		e.Session = *r.Session
	} else if r.Info != nil {
		e.Session = *r.Info
	} else {
		var flat Session
		if err := json.Unmarshal(data, &flat); err == nil {
			e.Session = flat
		}
	}
	if e.SessionID == "" && e.Session.ID != "" {
		e.SessionID = e.Session.ID
	}
	if e.Session.ID == "" && e.SessionID != "" {
		e.Session.ID = e.SessionID
	}
	return nil
}

// RevertStagedEvent is emitted for "session.revert.staged".
type RevertStagedEvent struct {
	SessionID string        `json:"sessionID"`
	Revert    SessionRevert `json:"revert"`
}

// RevertClearedEvent is emitted for "session.revert.cleared".
type RevertClearedEvent struct {
	SessionID string `json:"sessionID"`
}

// RevertCommittedEvent is emitted for "session.revert.committed".
type RevertCommittedEvent struct {
	SessionID string `json:"sessionID"`
	MessageID string `json:"messageID"`
}

// InboxEvent is emitted for "session.inbox.enqueued", "session.inbox.delivered",
// "session.inbox.cancelled", and "session.inbox.delivery.changed".
type InboxEvent struct {
	SessionID string        `json:"sessionID"`
	InboxID   string        `json:"inboxID"`
	Delivery  InboxDelivery `json:"delivery,omitempty"`
	Item      *InboxItem    `json:"item,omitempty"`
}

// ShellEvent is emitted for "shell.created", "shell.exited", and "shell.deleted".
type ShellEvent struct {
	ID      string    `json:"id,omitempty"`
	ShellID string    `json:"shellID,omitempty"`
	Status  string    `json:"status,omitempty"`
	Info    ShellItem `json:"info,omitempty"`
	Shell   ShellItem `json:"shell,omitempty"`
}

// FormCreatedEvent is emitted on "form.created".
type FormCreatedEvent struct {
	Form FormInfo `json:"form"`
}

// FormRepliedEvent is emitted on "form.replied".
type FormRepliedEvent struct {
	ID        string         `json:"id"`
	SessionID string         `json:"sessionID"`
	Answer    map[string]any `json:"answer,omitempty"`
}

// FormCancelledEvent is emitted on "form.cancelled".
type FormCancelledEvent struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionID"`
}

// PermissionAskedEvent is emitted on "permission.asked".
type PermissionAskedEvent struct {
	Request PermissionRequest
}

// UnmarshalJSON supports both flat PermissionRequest payloads and nested {"request": {...}} / {"permission": {...}} payloads.
func (e *PermissionAskedEvent) UnmarshalJSON(data []byte) error {
	var wrapper struct {
		Request    *PermissionRequest `json:"request"`
		Permission *PermissionRequest `json:"permission"`
		Info       *PermissionRequest `json:"info"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		return err
	}
	if wrapper.Request != nil && wrapper.Request.ID != "" {
		e.Request = *wrapper.Request
		return nil
	}
	if wrapper.Permission != nil && wrapper.Permission.ID != "" {
		e.Request = *wrapper.Permission
		return nil
	}
	if wrapper.Info != nil && wrapper.Info.ID != "" {
		e.Request = *wrapper.Info
		return nil
	}
	return json.Unmarshal(data, &e.Request)
}

// PermissionRepliedEvent is emitted on "permission.replied".
type PermissionRepliedEvent struct {
	ID        string             `json:"id,omitempty"`
	RequestID string             `json:"requestID"`
	SessionID string             `json:"sessionID"`
	Reply     PermissionDecision `json:"reply,omitempty"`
	Decision  PermissionDecision `json:"decision,omitempty"`
}

// EffectiveRequestID returns the non-empty request ID from the event.
func (e PermissionRepliedEvent) EffectiveRequestID() string {
	if e.RequestID != "" {
		return e.RequestID
	}
	return e.ID
}

// McpChangedEvent is emitted on "mcp.status.changed" and "mcp.resources.changed".
type McpChangedEvent struct {
	Server string `json:"server"`
}

// SessionStatus describes the session status payload inside "session.status".
type SessionStatus struct {
	Type    string `json:"type"`
	Attempt int    `json:"attempt,omitempty"`
	Message string `json:"message,omitempty"`
	Next    int64  `json:"next,omitempty"`
}

// SessionStatusEvent is emitted on "session.status".
type SessionStatusEvent struct {
	SessionID string        `json:"sessionID"`
	Status    SessionStatus `json:"status"`
}

// RetryScheduledEvent is emitted on "session.retry.scheduled".
type RetryScheduledEvent struct {
	SessionID          string `json:"sessionID"`
	AssistantMessageID string `json:"assistantMessageID,omitempty"`
	Attempt            int    `json:"attempt,omitempty"`
	At                 int64  `json:"at,omitempty"`
}

// SessionErrorEvent is emitted on "session.error".
type SessionErrorEvent struct {
	SessionID string `json:"sessionID,omitempty"`
	Error     any    `json:"error,omitempty"`
}
