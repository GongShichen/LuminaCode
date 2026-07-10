package longmemory

import "time"

type ScopeType string

const (
	ScopeUser      ScopeType = "user"
	ScopeProject   ScopeType = "project"
	ScopeTeam      ScopeType = "team"
	ScopeAgentType ScopeType = "agent_type"
	ScopeTeamAgent ScopeType = "team_agent"
)

type MemoryType string

const (
	TypeSemantic   MemoryType = "semantic"
	TypeEpisodic   MemoryType = "episodic"
	TypeProcedural MemoryType = "procedural"
	TypePreference MemoryType = "preference"
	TypeFeedback   MemoryType = "feedback"
	TypeProject    MemoryType = "project"
	TypeReference  MemoryType = "reference"
)

type Status string

const (
	StatusPending    Status = "pending"
	StatusActive     Status = "active"
	StatusArchived   Status = "archived"
	StatusDeleted    Status = "deleted"
	StatusSuperseded Status = "superseded"
)

type Scope struct {
	Type ScopeType `json:"scope_type"`
	Key  string    `json:"scope_key"`
}

type Entry struct {
	MemoryID            string     `json:"memory_id"`
	ScopeType           ScopeType  `json:"scope_type"`
	ScopeKey            string     `json:"scope_key"`
	MemoryType          MemoryType `json:"memory_type"`
	Title               string     `json:"title"`
	Content             string     `json:"content"`
	Summary             string     `json:"summary"`
	Tags                []string   `json:"tags"`
	Entities            []string   `json:"entities"`
	Importance          float64    `json:"importance"`
	Confidence          float64    `json:"confidence"`
	SourceSessionID     string     `json:"source_session_id"`
	SourceMessageIDs    []string   `json:"source_message_ids"`
	SourceAgentID       string     `json:"source_agent_id"`
	SourceTeamSessionID string     `json:"source_team_session_id"`
	SourcePaths         []string   `json:"source_paths"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
	LastAccessedAt      time.Time  `json:"last_accessed_at"`
	ValidFrom           time.Time  `json:"valid_from"`
	ValidUntil          time.Time  `json:"valid_until"`
	SupersededBy        string     `json:"superseded_by"`
	Status              Status     `json:"status"`
	Score               float64    `json:"score,omitempty"`
	MatchReason         string     `json:"match_reason,omitempty"`
}

type Candidate struct {
	MemoryID            string     `json:"memory_id,omitempty"`
	Action              string     `json:"action,omitempty"`
	TargetMemoryID      string     `json:"target_memory_id,omitempty"`
	ScopeType           ScopeType  `json:"scope_type"`
	ScopeKey            string     `json:"scope_key"`
	MemoryType          MemoryType `json:"memory_type"`
	Status              Status     `json:"status,omitempty"`
	Title               string     `json:"title"`
	Content             string     `json:"content"`
	Summary             string     `json:"summary"`
	Tags                []string   `json:"tags"`
	Entities            []string   `json:"entities"`
	Importance          float64    `json:"importance"`
	Confidence          float64    `json:"confidence"`
	SourceSessionID     string     `json:"source_session_id"`
	SourceMessageIDs    []string   `json:"source_message_ids"`
	SourceAgentID       string     `json:"source_agent_id"`
	SourceTeamSessionID string     `json:"source_team_session_id"`
	SourcePaths         []string   `json:"source_paths"`
	ValidFrom           time.Time  `json:"valid_from"`
	ValidUntil          time.Time  `json:"valid_until"`
}

type SearchOptions struct {
	Query           string
	Scopes          []Scope
	Types           []MemoryType
	Tags            []string
	Limit           int
	MaxCandidates   int
	ContextMaxRunes int
	IncludeInactive bool
	IncludeExpired  bool
	CreatedAfter    time.Time
	CreatedBefore   time.Time
	ExcludeIDs      map[string]struct{}
}

type UsedRecord struct {
	SessionID     string    `json:"session_id"`
	TeamSessionID string    `json:"team_session_id,omitempty"`
	AgentID       string    `json:"agent_id,omitempty"`
	Query         string    `json:"query"`
	MemoryIDs     []string  `json:"memory_ids"`
	CreatedAt     time.Time `json:"created_at"`
}

type RetentionPolicy map[MemoryType]int
