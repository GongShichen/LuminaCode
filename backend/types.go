package backend

import (
	"encoding/json"
	"time"

	"LuminaCode/harness"
	luminateam "LuminaCode/team"
	luminaui "LuminaCode/ui"
)

type EndpointInfo struct {
	PID       int    `json:"pid"`
	Host      string `json:"host"`
	Port      int    `json:"port"`
	AuthToken string `json:"auth_token"`
	StartedAt string `json:"started_at"`
	URL       string `json:"url"`
}

type RPCRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type RPCResponse struct {
	ID     string    `json:"id"`
	OK     bool      `json:"ok"`
	Result any       `json:"result,omitempty"`
	Error  *RPCError `json:"error,omitempty"`
}

type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type PushEvent struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocol_version,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	StreamID        string `json:"stream_id,omitempty"`
	Seq             int64  `json:"seq,omitempty"`
	AfterSeq        int64  `json:"after_seq,omitempty"`
	EventID         string `json:"event_id,omitempty"`
	EventType       string `json:"event_type,omitempty"`
	SchemaVersion   int    `json:"schema_version,omitempty"`
	Durable         bool   `json:"durable,omitempty"`
	Timestamp       string `json:"timestamp,omitempty"`
	Payload         any    `json:"payload,omitempty"`
	Event           any    `json:"event,omitempty"`
}

type SessionSnapshot struct {
	SessionID         string                `json:"session_id"`
	Frame             luminaui.RenderFrame  `json:"frame"`
	Busy              bool                  `json:"busy"`
	Model             string                `json:"model"`
	CWD               string                `json:"cwd"`
	Teams             []luminateam.Snapshot `json:"teams,omitempty"`
	LastSeq           int64                 `json:"last_seq"`
	ProjectionVersion int                   `json:"projection_version"`
}

type EventPage struct {
	Events       []harness.Event `json:"events"`
	NextAfterSeq int64           `json:"next_after_seq"`
	HasMore      bool            `json:"has_more"`
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
