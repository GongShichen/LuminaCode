package cluster

import (
	"context"
	"encoding/json"
	"time"
)

type Principal struct {
	TenantID string
	Subject  string
	Scopes   map[string]struct{}
	TokenID  string
}

func LocalPrincipal() Principal {
	return Principal{TenantID: "local", Subject: "local", Scopes: map[string]struct{}{
		"lumina:admin": {}, "lumina:session:read": {}, "lumina:session:write": {},
	}}
}

func (p Principal) HasScope(scope string) bool {
	_, ok := p.Scopes[scope]
	return ok
}

type RuntimeIdentity struct {
	TenantID   string
	Subject    string
	InstanceID string
	SessionID  string
	ProjectID  string
	FenceToken int64
}

type InstanceInfo struct {
	InstanceID    string    `json:"instance_id"`
	AdvertiseAddr string    `json:"advertise_addr,omitempty"`
	Draining      bool      `json:"draining"`
	StartedAt     time.Time `json:"started_at"`
}

type Owner struct {
	InstanceID string `json:"instance_id"`
	LeaseID    string `json:"lease_id"`
	FenceToken int64  `json:"fence_token"`
}

type SessionLease interface {
	Owner() Owner
	Lost() <-chan struct{}
	Release(context.Context) error
}

type SessionCoordinator interface {
	RegisterInstance(context.Context, InstanceInfo, time.Duration) error
	UnregisterInstance(context.Context, string) error
	Lookup(context.Context, string, string) (*Owner, error)
	Acquire(context.Context, string, string, string, time.Duration, time.Duration) (SessionLease, error)
	Close() error
}

type CommandEnvelope struct {
	RequestID         string          `json:"request_id"`
	CommandID         string          `json:"command_id"`
	GatewayInstanceID string          `json:"gateway_instance_id"`
	OwnerInstanceID   string          `json:"owner_instance_id"`
	TenantID          string          `json:"tenant_id"`
	Subject           string          `json:"subject,omitempty"`
	SessionID         string          `json:"session_id"`
	TeamSessionID     string          `json:"team_session_id,omitempty"`
	A2AAgentID        string          `json:"a2a_agent_id,omitempty"`
	Method            string          `json:"method"`
	Params            json.RawMessage `json:"params,omitempty"`
	Deadline          time.Time       `json:"deadline"`
	FenceToken        int64           `json:"fence_token"`
	StreamID          string          `json:"-"`
}

type CommandResponse struct {
	RequestID string          `json:"request_id"`
	OK        bool            `json:"ok"`
	Result    json.RawMessage `json:"result,omitempty"`
	ErrorCode string          `json:"error_code,omitempty"`
	Error     string          `json:"error,omitempty"`
}

type ClusterCommandBus interface {
	Forward(context.Context, CommandEnvelope) (CommandResponse, error)
	Read(context.Context, string, string, int64, time.Duration) ([]CommandEnvelope, error)
	Respond(context.Context, string, CommandResponse) error
	Ack(context.Context, string, string) error
	Close() error
}

type EventNotification struct {
	TenantID       string          `json:"tenant_id"`
	SessionID      string          `json:"session_id"`
	OriginInstance string          `json:"origin_instance_id,omitempty"`
	EventID        string          `json:"event_id,omitempty"`
	Seq            int64           `json:"seq,omitempty"`
	Payload        json.RawMessage `json:"payload"`
}

type EventNotifier interface {
	Publish(context.Context, EventNotification) error
	Subscribe(context.Context, string, string) (<-chan EventNotification, func(), error)
	Close() error
}
