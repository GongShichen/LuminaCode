package longmemory

import "time"

type Episode struct {
	EpisodeID     string    `json:"episode_id"`
	ScopeType     ScopeType `json:"scope_type"`
	ScopeKey      string    `json:"scope_key"`
	SessionID     string    `json:"session_id"`
	TeamSessionID string    `json:"team_session_id,omitempty"`
	AgentID       string    `json:"agent_id,omitempty"`
	MessageIDs    []string  `json:"message_ids"`
	Kind          string    `json:"kind"`
	Content       string    `json:"content"`
	OccurredAt    time.Time `json:"occurred_at"`
	ObservedAt    time.Time `json:"observed_at"`
	ContentHash   string    `json:"content_hash"`
}

type SessionIndex struct {
	IndexID     string    `json:"index_id"`
	ScopeType   ScopeType `json:"scope_type"`
	ScopeKey    string    `json:"scope_key"`
	SessionID   string    `json:"session_id"`
	Summary     string    `json:"summary"`
	Keyphrases  []string  `json:"keyphrases"`
	Entities    []string  `json:"entities"`
	Roles       []string  `json:"roles"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	ContentHash string    `json:"content_hash"`
}

type MemoryEntity struct {
	MemoryID   string    `json:"memory_id"`
	ScopeType  ScopeType `json:"scope_type"`
	ScopeKey   string    `json:"scope_key"`
	Normalized string    `json:"normalized_entity"`
	Original   string    `json:"original_text"`
	EntityType string    `json:"entity_type"`
	Confidence float64   `json:"confidence"`
}

type Fact struct {
	MemoryIndex   int            `json:"memory_index,omitempty"`
	FactID        string         `json:"fact_id"`
	MemoryID      string         `json:"memory_id"`
	ScopeType     ScopeType      `json:"scope_type"`
	ScopeKey      string         `json:"scope_key"`
	Subject       string         `json:"subject"`
	Predicate     string         `json:"predicate"`
	Object        string         `json:"object"`
	Qualifiers    map[string]any `json:"qualifiers"`
	FactKey       string         `json:"fact_key"`
	Confidence    float64        `json:"confidence"`
	ValidFrom     time.Time      `json:"valid_from"`
	ValidUntil    time.Time      `json:"valid_until"`
	ObservedAt    time.Time      `json:"observed_at"`
	InvalidatedAt time.Time      `json:"invalidated_at"`
	SupersededBy  string         `json:"superseded_by"`
	Status        Status         `json:"status"`
}

type EdgeType string

const (
	EdgeRelatedTo   EdgeType = "related_to"
	EdgeSupports    EdgeType = "supports"
	EdgeContradicts EdgeType = "contradicts"
	EdgeSupersedes  EdgeType = "supersedes"
	EdgeDerivedFrom EdgeType = "derived_from"
	EdgeNextEvent   EdgeType = "next_event"
	EdgeContains    EdgeType = "contains"
)

type Edge struct {
	FromMemoryIndex int       `json:"from_memory_index,omitempty"`
	ToMemoryIndex   int       `json:"to_memory_index,omitempty"`
	EdgeID          string    `json:"edge_id"`
	ScopeType       ScopeType `json:"scope_type"`
	ScopeKey        string    `json:"scope_key"`
	FromID          string    `json:"from_id"`
	ToID            string    `json:"to_id"`
	Type            EdgeType  `json:"edge_type"`
	Weight          float64   `json:"weight"`
	Confidence      float64   `json:"confidence"`
	CreatedAt       time.Time `json:"created_at"`
	ValidUntil      time.Time `json:"valid_until"`
}

type EvidenceSpan struct {
	MemoryIndex int       `json:"memory_index,omitempty"`
	SpanID      string    `json:"span_id"`
	MemoryID    string    `json:"memory_id"`
	ScopeType   ScopeType `json:"scope_type"`
	ScopeKey    string    `json:"scope_key"`
	SessionID   string    `json:"session_id"`
	MessageID   string    `json:"message_id"`
	Role        string    `json:"role,omitempty"`
	SourcePath  string    `json:"source_path,omitempty"`
	Text        string    `json:"text"`
	StartRune   int       `json:"start_rune"`
	EndRune     int       `json:"end_rune"`
	OccurredAt  time.Time `json:"occurred_at"`
	ContentHash string    `json:"content_hash"`
}

type EvidenceChunk struct {
	ChunkID        string    `json:"chunk_id"`
	SpanID         string    `json:"span_id"`
	ParentMemoryID string    `json:"parent_memory_id"`
	ScopeType      ScopeType `json:"scope_type"`
	ScopeKey       string    `json:"scope_key"`
	SessionID      string    `json:"session_id"`
	MessageID      string    `json:"message_id"`
	Role           string    `json:"role"`
	Text           string    `json:"text"`
	StartRune      int       `json:"start_rune"`
	EndRune        int       `json:"end_rune"`
	OccurredAt     time.Time `json:"occurred_at"`
	ValidFrom      time.Time `json:"valid_from"`
	ValidUntil     time.Time `json:"valid_until"`
	ContentHash    string    `json:"content_hash"`
}

type CoreBlock struct {
	BlockID          string    `json:"block_id"`
	ScopeType        ScopeType `json:"scope_type"`
	ScopeKey         string    `json:"scope_key"`
	Label            string    `json:"label"`
	Description      string    `json:"description"`
	Content          string    `json:"content"`
	ReadOnly         bool      `json:"read_only"`
	Generation       int       `json:"generation"`
	Confidence       float64   `json:"confidence"`
	SourceSessionID  string    `json:"source_session_id,omitempty"`
	SourceMessageIDs []string  `json:"source_message_ids,omitempty"`
	FirstObservedAt  time.Time `json:"first_observed_at,omitempty"`
	LastConfirmedAt  time.Time `json:"last_confirmed_at,omitempty"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type Job struct {
	JobID       string    `json:"job_id"`
	Kind        string    `json:"kind"`
	ScopeType   ScopeType `json:"scope_type"`
	ScopeKey    string    `json:"scope_key"`
	Payload     string    `json:"payload"`
	Status      string    `json:"status"`
	Attempts    int       `json:"attempts"`
	LastError   string    `json:"last_error"`
	AvailableAt time.Time `json:"available_at"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type QueryPlan struct {
	Query               string   `json:"query"`
	Subqueries          []string `json:"subqueries"`
	Entities            []string `json:"entities"`
	Scopes              []Scope  `json:"required_scopes"`
	TargetContextTokens int      `json:"target_context_tokens"`
}

type MessageExcerpt struct {
	Role      string    `json:"role"`
	Text      string    `json:"text"`
	Timestamp time.Time `json:"timestamp,omitempty"`
}

type MemoryQuery struct {
	Text          string           `json:"text"`
	RecentContext []MessageExcerpt `json:"recent_context,omitempty"`
	Timestamp     time.Time        `json:"timestamp"`
	Scopes        []Scope          `json:"scopes"`
	SessionID     string           `json:"session_id,omitempty"`
	TeamSessionID string           `json:"team_session_id,omitempty"`
	AgentID       string           `json:"agent_id,omitempty"`
}

type TemporalConstraint struct {
	From     time.Time `json:"from,omitempty"`
	To       time.Time `json:"to,omitempty"`
	At       time.Time `json:"at,omitempty"`
	Order    string    `json:"order,omitempty"`
	FromText string    `json:"from_text,omitempty"`
	ToText   string    `json:"to_text,omitempty"`
	AtText   string    `json:"at_text,omitempty"`
}

type QueryExpansion struct {
	Queries             []string             `json:"queries,omitempty"`
	Entities            []string             `json:"entities,omitempty"`
	TemporalConstraints []TemporalConstraint `json:"temporal_constraints,omitempty"`
	RelationTerms       []string             `json:"relation_terms,omitempty"`
}

type RetrievalCandidate struct {
	DocumentID     string             `json:"document_id,omitempty"`
	DocumentKind   string             `json:"document_kind,omitempty"`
	ParentID       string             `json:"parent_id,omitempty"`
	MemoryID       string             `json:"memory_id"`
	Entry          Entry              `json:"entry"`
	SourceSession  string             `json:"source_session_id,omitempty"`
	SourceMessages []string           `json:"source_message_ids,omitempty"`
	Scope          Scope              `json:"scope"`
	OccurredAt     time.Time          `json:"occurred_at,omitempty"`
	ValidFrom      time.Time          `json:"valid_from,omitempty"`
	ValidUntil     time.Time          `json:"valid_until,omitempty"`
	ChannelRanks   map[string]int     `json:"channel_ranks"`
	ChannelScores  map[string]float64 `json:"channel_scores"`
	FusedScore     float64            `json:"fused_score"`
	GraphScore     float64            `json:"graph_score,omitempty"`
	Selected       bool               `json:"selected"`
	DropReason     string             `json:"drop_reason,omitempty"`
}

type RetrievalDocument struct {
	DocumentID string    `json:"document_id"`
	Kind       string    `json:"kind"`
	ParentID   string    `json:"parent_id,omitempty"`
	Scope      Scope     `json:"scope"`
	SessionID  string    `json:"session_id,omitempty"`
	MessageID  string    `json:"message_id,omitempty"`
	Role       string    `json:"role,omitempty"`
	Text       string    `json:"text"`
	OccurredAt time.Time `json:"occurred_at,omitempty"`
	ValidFrom  time.Time `json:"valid_from,omitempty"`
	ValidUntil time.Time `json:"valid_until,omitempty"`
}

type ChannelResult struct {
	Channel    string               `json:"channel"`
	Query      string               `json:"query"`
	Candidates []RetrievalCandidate `json:"candidates,omitempty"`
	DurationMS int64                `json:"duration_ms"`
	Error      string               `json:"error,omitempty"`
}

type RetrievalRun struct {
	RunID                       string                    `json:"run_id"`
	Query                       MemoryQuery               `json:"query"`
	Expansion                   QueryExpansion            `json:"expansion"`
	ExpansionModel              string                    `json:"expansion_model,omitempty"`
	ExpansionError              string                    `json:"expansion_error,omitempty"`
	ChannelResults              []ChannelResult           `json:"channel_results"`
	SelectedIDs                 []string                  `json:"selected_ids,omitempty"`
	InjectedIDs                 []string                  `json:"injected_ids,omitempty"`
	Evidence                    []Evidence                `json:"evidence,omitempty"`
	StopReason                  string                    `json:"stop_reason"`
	EstimatedTokens             int                       `json:"estimated_tokens,omitempty"`
	DurationMS                  int64                     `json:"duration_ms"`
	ReferenceTime               time.Time                 `json:"reference_time,omitempty"`
	SelectedSessions            []string                  `json:"selected_sessions,omitempty"`
	GlobalChannelCandidates     map[string]int            `json:"global_channel_candidates,omitempty"`
	PerSessionChannelCandidates map[string]map[string]int `json:"per_session_channel_candidates,omitempty"`
	CoverageFacets              []string                  `json:"coverage_facets,omitempty"`
	CanonicalEntities           []CanonicalEntity         `json:"canonical_entities,omitempty"`
	CanonicalEvents             []CanonicalEvent          `json:"canonical_events,omitempty"`
	CacheHit                    bool                      `json:"cache_hit,omitempty"`
	CacheKeyScope               string                    `json:"cache_key_scope,omitempty"`
	ExpansionWaitMS             int64                     `json:"expansion_wait_ms,omitempty"`
	CreatedAt                   time.Time                 `json:"created_at"`
}

type CanonicalEntity struct {
	EntityID     string    `json:"entity_id"`
	ScopeType    ScopeType `json:"scope_type"`
	ScopeKey     string    `json:"scope_key"`
	Name         string    `json:"name"`
	EntityType   string    `json:"entity_type,omitempty"`
	Confidence   float64   `json:"confidence"`
	SourceChunks []string  `json:"source_chunks,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type CanonicalEvent struct {
	EventID      string            `json:"event_id"`
	ScopeType    ScopeType         `json:"scope_type"`
	ScopeKey     string            `json:"scope_key"`
	Title        string            `json:"title"`
	Summary      string            `json:"summary"`
	OccurredAt   time.Time         `json:"occurred_at,omitempty"`
	ValidFrom    time.Time         `json:"valid_from,omitempty"`
	ValidUntil   time.Time         `json:"valid_until,omitempty"`
	Participants []CanonicalEntity `json:"participants,omitempty"`
	SourceChunks []string          `json:"source_chunks,omitempty"`
	Confidence   float64           `json:"confidence"`
}

type CanonicalMerge struct {
	Entity       *CanonicalEntity `json:"entity,omitempty"`
	Event        *CanonicalEvent  `json:"event,omitempty"`
	Aliases      []string         `json:"aliases,omitempty"`
	SourceChunks []string         `json:"source_chunks,omitempty"`
}

type TimelineEntry struct {
	EventID    string    `json:"event_id,omitempty"`
	DocumentID string    `json:"document_id,omitempty"`
	SessionID  string    `json:"session_id,omitempty"`
	MessageID  string    `json:"message_id,omitempty"`
	Role       string    `json:"role,omitempty"`
	Text       string    `json:"text"`
	OccurredAt time.Time `json:"occurred_at,omitempty"`
	ValidFrom  time.Time `json:"valid_from,omitempty"`
	ValidUntil time.Time `json:"valid_until,omitempty"`
}

type MemoryCatalog struct {
	TotalMemories int            `json:"total_memories"`
	TotalEpisodes int            `json:"total_episodes"`
	TotalSessions int            `json:"total_sessions"`
	TotalChunks   int            `json:"total_chunks"`
	TotalEntities int            `json:"total_entities"`
	TotalFacts    int            `json:"total_facts"`
	TotalEdges    int            `json:"total_edges"`
	ByType        map[string]int `json:"by_type"`
	ByScope       map[string]int `json:"by_scope"`
	Oldest        time.Time      `json:"oldest,omitempty"`
	Newest        time.Time      `json:"newest,omitempty"`
}

type CandidateScore struct {
	DocumentID    string             `json:"document_id,omitempty"`
	MemoryID      string             `json:"memory_id"`
	Entry         Entry              `json:"entry"`
	ChannelRanks  map[string]int     `json:"channel_ranks"`
	ChannelScores map[string]float64 `json:"channel_scores"`
	FusedScore    float64            `json:"fused_score"`
	GraphScore    float64            `json:"graph_score"`
	Selected      bool               `json:"selected"`
	DropReason    string             `json:"drop_reason,omitempty"`
}

type Evidence struct {
	DocumentID     string         `json:"document_id,omitempty"`
	DocumentKind   string         `json:"document_kind,omitempty"`
	ParentID       string         `json:"parent_id,omitempty"`
	MemoryID       string         `json:"memory_id"`
	DocumentIDs    []string       `json:"document_ids,omitempty"`
	Title          string         `json:"title"`
	Text           string         `json:"text"`
	ScopeType      ScopeType      `json:"scope_type"`
	ScopeKey       string         `json:"scope_key"`
	MemoryType     MemoryType     `json:"memory_type"`
	SourceSession  string         `json:"source_session_id,omitempty"`
	SourceMessages []string       `json:"source_message_ids,omitempty"`
	SourcePaths    []string       `json:"source_paths,omitempty"`
	OccurredAt     time.Time      `json:"occurred_at"`
	ValidFrom      time.Time      `json:"valid_from"`
	ValidUntil     time.Time      `json:"valid_until"`
	Confidence     float64        `json:"confidence"`
	Score          float64        `json:"score"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}

type EvidencePacket struct {
	Plan            QueryPlan           `json:"plan"`
	ReferenceTime   time.Time           `json:"reference_time,omitempty"`
	CoreBlocks      []CoreBlock         `json:"core_blocks"`
	Evidence        []Evidence          `json:"evidence"`
	Timeline        []TimelineEntry     `json:"timeline,omitempty"`
	CanonicalEvents []CanonicalEvent    `json:"canonical_events,omitempty"`
	EstimatedTokens int                 `json:"estimated_tokens"`
	Warnings        []string            `json:"warnings,omitempty"`
	Documents       []RetrievalDocument `json:"documents,omitempty"`
	SourceCoverage  map[string]int      `json:"source_coverage,omitempty"`
}

type RetrievalTrace struct {
	RunID           string           `json:"run_id"`
	SessionID       string           `json:"session_id"`
	TeamSessionID   string           `json:"team_session_id,omitempty"`
	AgentID         string           `json:"agent_id,omitempty"`
	Plan            QueryPlan        `json:"plan"`
	Candidates      []CandidateScore `json:"candidates"`
	SelectedIDs     []string         `json:"selected_ids"`
	EstimatedTokens int              `json:"estimated_tokens"`
	DurationMS      int64            `json:"duration_ms"`
	Error           string           `json:"error,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	Run             *RetrievalRun    `json:"run,omitempty"`
}

type ExtractionBatch struct {
	Episode          *Episode          `json:"episode,omitempty"`
	Memories         []Candidate       `json:"memories"`
	Facts            []Fact            `json:"facts"`
	Edges            []Edge            `json:"edges"`
	Spans            []EvidenceSpan    `json:"evidence_spans"`
	EpisodeSpans     []EvidenceSpan    `json:"episode_spans,omitempty"`
	Chunks           []EvidenceChunk   `json:"chunks,omitempty"`
	ChunkEmbeddings  []MemoryEmbedding `json:"chunk_embeddings,omitempty"`
	CoreBlocks       []CoreBlock       `json:"core_blocks"`
	Embeddings       []MemoryEmbedding `json:"embeddings,omitempty"`
	SessionEmbedding *MemoryEmbedding  `json:"session_embedding,omitempty"`
	Jobs             []Job             `json:"jobs,omitempty"`
	ConsumerID       string            `json:"consumer_id,omitempty"`
	SessionID        string            `json:"session_id,omitempty"`
	LastMessageID    string            `json:"last_message_id,omitempty"`
	LastMessageIndex int               `json:"last_message_index,omitempty"`
}

type MemoryEmbedding struct {
	MemoryIndex int       `json:"memory_index"`
	MemoryID    string    `json:"memory_id,omitempty"`
	Model       string    `json:"model"`
	ContentHash string    `json:"content_hash"`
	Vector      []float32 `json:"vector"`
}
