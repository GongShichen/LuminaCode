package backend

import (
	"encoding/json"
	"time"

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
	Type      string `json:"type"`
	SessionID string `json:"session_id,omitempty"`
	Seq       int64  `json:"seq,omitempty"`
	Event     any    `json:"event"`
}

type SessionSnapshot struct {
	SessionID string                `json:"session_id"`
	Frame     luminaui.RenderFrame  `json:"frame"`
	Busy      bool                  `json:"busy"`
	Model     string                `json:"model"`
	CWD       string                `json:"cwd"`
	Teams     []luminateam.Snapshot `json:"teams,omitempty"`
}

func nowRFC3339() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}
