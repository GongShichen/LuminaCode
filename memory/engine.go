package memory

import (
	"context"
	"time"
)

// Engine is the stable boundary between agents and durable long-term memory.
// Implementations must make raw events durable independently of semantic API
// availability and must not perform remote calls from Search.
type Engine interface {
	AppendEvents(context.Context, []RawEvent, IngestOptions) (IngestResult, error)
	Remember(context.Context, MemoryRequest) (MemoryCommitResult, error)
	SealContext(context.Context, ContextRef) (JobRef, error)
	Search(context.Context, SearchRequest) (SearchResult, error)
	PrioritizeConflicts(context.Context, ConflictSelector) (JobRef, error)
	Forget(context.Context, Selector, ForgetMode) error
	Doctor(context.Context) (HealthReport, error)
	Close() error
}

type SemanticPolicy string

const (
	SemanticDeferred      SemanticPolicy = "deferred"
	SemanticRequired      SemanticPolicy = "required"
	SemanticDeterministic SemanticPolicy = "deterministic"
	SemanticDurableOnly   SemanticPolicy = "durable_only"
)

type SemanticStatus string

const (
	SemanticEventDurable      SemanticStatus = "event_durable"
	SemanticRawOnly           SemanticStatus = "raw_only"
	SemanticSkipped           SemanticStatus = "semantic_skipped"
	SemanticProposed          SemanticStatus = "proposed"
	SemanticGrounded          SemanticStatus = "grounded"
	SemanticActive            SemanticStatus = "active"
	SemanticPendingResolution SemanticStatus = "pending_resolution"
	SemanticScoped            SemanticStatus = "scoped"
	SemanticSuperseded        SemanticStatus = "superseded"
	SemanticRejected          SemanticStatus = "rejected"
	SemanticUnresolved        SemanticStatus = "unresolved"
	SemanticQuarantined       SemanticStatus = "quarantined"
	SemanticTombstoned        SemanticStatus = "tombstoned"
)

type NodeKind string

const (
	NodeClaim     NodeKind = "claim"
	NodeEpisode   NodeKind = "episode"
	NodeProcedure NodeKind = "procedure"
)

type ClaimType string

const (
	ClaimFact       ClaimType = "fact"
	ClaimState      ClaimType = "state"
	ClaimPreference ClaimType = "preference"
	ClaimConstraint ClaimType = "constraint"
)

type Facet string

const (
	FacetProfile        Facet = "profile"
	FacetState          Facet = "state"
	FacetPreference     Facet = "preference"
	FacetConstraint     Facet = "constraint"
	FacetConfiguration  Facet = "configuration"
	FacetLocation       Facet = "location"
	FacetRelationship   Facet = "relationship"
	FacetGoal           Facet = "goal"
	FacetProcedureState Facet = "procedure-state"
)

type EvidenceMode string

const (
	EvidenceObserved     EvidenceMode = "observed"
	EvidenceUserDeclared EvidenceMode = "user_declared"
	EvidenceInferred     EvidenceMode = "inferred"
)

type ValueKind string

const (
	ValueText   ValueKind = "text"
	ValueNumber ValueKind = "number"
	ValueTime   ValueKind = "time"
	ValueList   ValueKind = "list"
	ValueBool   ValueKind = "bool"
)

type ClaimValue struct {
	Kind   ValueKind `json:"kind"`
	Text   string    `json:"text,omitempty"`
	Number float64   `json:"number,omitempty"`
	Unit   string    `json:"unit,omitempty"`
	Time   time.Time `json:"time,omitempty"`
	List   []string  `json:"list,omitempty"`
	Bool   *bool     `json:"bool,omitempty"`
}

type RawEvent struct {
	ID         string            `json:"id,omitempty"`
	Space      string            `json:"space"`
	ContextID  string            `json:"context_id,omitempty"`
	SessionID  string            `json:"session_id,omitempty"`
	Actor      string            `json:"actor"`
	SourceKind string            `json:"source_kind,omitempty"`
	Content    string            `json:"content"`
	OccurredAt time.Time         `json:"occurred_at,omitempty"`
	SourceRef  string            `json:"source_ref,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type ContextRef struct {
	ID       string    `json:"id"`
	Space    string    `json:"space"`
	ParentID string    `json:"parent_id,omitempty"`
	Type     string    `json:"type,omitempty"`
	Label    string    `json:"label,omitempty"`
	OpenedAt time.Time `json:"opened_at,omitempty"`
	ClosedAt time.Time `json:"closed_at,omitempty"`
}

type IngestOptions struct {
	SemanticPolicy SemanticPolicy `json:"semantic_policy,omitempty"`
	SealContext    bool           `json:"seal_context,omitempty"`
}

type IngestResult struct {
	Durable        bool           `json:"durable"`
	EventIDs       []string       `json:"event_ids,omitempty"`
	IndexedThrough int64          `json:"indexed_through,omitempty"`
	SemanticStatus SemanticStatus `json:"semantic_status"`
	PendingJobID   string         `json:"pending_job_id,omitempty"`
}

type MemoryWriteMode string

const (
	WriteNormal         MemoryWriteMode = "normal"
	WriteExplicit       MemoryWriteMode = "explicit"
	WriteCorrection     MemoryWriteMode = "correction"
	WritePreference     MemoryWriteMode = "preference"
	WriteConstraint     MemoryWriteMode = "constraint"
	WriteCriticalResult MemoryWriteMode = "critical_result"
	WriteImport         MemoryWriteMode = "import"
)

type MemoryRequest struct {
	Space           string          `json:"space"`
	ContextID       string          `json:"context_id,omitempty"`
	Events          []RawEvent      `json:"events,omitempty"`
	SourceEventIDs  []string        `json:"source_event_ids,omitempty"`
	CompileSources  []CompileSource `json:"-"`
	Drafts          []MemoryDraft   `json:"drafts,omitempty"`
	Mode            MemoryWriteMode `json:"mode,omitempty"`
	RequireSemantic bool            `json:"require_semantic,omitempty"`
	Instructions    string          `json:"instructions,omitempty"`
}

type MemoryCommitResult struct {
	Durable        bool           `json:"durable"`
	SemanticStatus SemanticStatus `json:"semantic_status"`
	EventIDs       []string       `json:"event_ids,omitempty"`
	MemoryIDs      []string       `json:"memory_ids,omitempty"`
	ConflictIDs    []string       `json:"conflict_ids,omitempty"`
	ResolutionIDs  []string       `json:"resolution_ids,omitempty"`
	PendingJobID   string         `json:"pending_job_id,omitempty"`
	Usage          APIUsage       `json:"usage,omitempty"`
}

type SourceSpan struct {
	EventID   string `json:"event_id"`
	SourceRef string `json:"source_ref,omitempty"`
	StartRune int    `json:"start_rune"`
	EndRune   int    `json:"end_rune"`
	Role      string `json:"role,omitempty"`
	// Text is an exact source quote in compiler output and a bounded excerpt in
	// adjudicator input. Compiler-provided offsets are never trusted; the
	// grounding layer derives durable rune offsets from this quote locally.
	Text string `json:"text,omitempty"`
}

type Scope struct {
	Project     string `json:"project,omitempty"`
	Environment string `json:"environment,omitempty"`
	Actor       string `json:"actor,omitempty"`
	Condition   string `json:"condition,omitempty"`
}

type MemoryDraft struct {
	Kind          NodeKind       `json:"kind"`
	ClaimType     ClaimType      `json:"claim_type,omitempty"`
	Statement     string         `json:"statement"`
	Subject       string         `json:"subject,omitempty"`
	SubjectType   string         `json:"subject_type,omitempty"`
	Facet         Facet          `json:"facet,omitempty"`
	AttributeKey  string         `json:"attribute_key,omitempty"`
	Scope         Scope          `json:"scope,omitempty"`
	Value         ClaimValue     `json:"value,omitempty"`
	ValidFrom     time.Time      `json:"valid_from,omitempty"`
	ValidUntil    time.Time      `json:"valid_until,omitempty"`
	EvidenceMode  EvidenceMode   `json:"evidence_mode,omitempty"`
	Sources       []SourceSpan   `json:"sources"`
	Keys          []string       `json:"keys,omitempty"`
	RetrievalCues []string       `json:"retrieval_cues,omitempty"`
	Payload       map[string]any `json:"payload,omitempty"`
}

type IdentityAliasProposal struct {
	Canonical string       `json:"canonical"`
	Type      string       `json:"type,omitempty"`
	Aliases   []string     `json:"aliases"`
	Sources   []SourceSpan `json:"sources"`
}

type APIUsage struct {
	Calls                    int    `json:"calls,omitempty"`
	InputTokens              int    `json:"input_tokens,omitempty"`
	CacheReadInputTokens     int    `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens int    `json:"cache_creation_input_tokens,omitempty"`
	OutputTokens             int    `json:"output_tokens,omitempty"`
	Model                    string `json:"model,omitempty"`
}

const (
	APIStageSemanticCompile      = "semantic_compile"
	APIStageConflictAdjudication = "conflict_adjudication"
)

type APIUsageEvent struct {
	Stage      string    `json:"stage"`
	Space      string    `json:"space,omitempty"`
	ContextID  string    `json:"context_id,omitempty"`
	ResourceID string    `json:"resource_id,omitempty"`
	Usage      APIUsage  `json:"usage"`
	Error      string    `json:"error,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

type APIUsageObserver func(context.Context, APIUsageEvent) error

type CompileRequest struct {
	Mode            MemoryWriteMode `json:"mode"`
	Instructions    string          `json:"instructions,omitempty"`
	Sources         []CompileSource `json:"sources"`
	MaxInputTokens  int             `json:"max_input_tokens"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	MaxNodes        int             `json:"max_nodes"`
}

type CompileResponse struct {
	Nodes    []MemoryDraft           `json:"nodes"`
	Aliases  []IdentityAliasProposal `json:"aliases,omitempty"`
	Usage    APIUsage                `json:"usage,omitempty"`
	Audit    string                  `json:"audit,omitempty"`
	CacheHit bool                    `json:"-"`
}

// CompileContractError marks structured output that remains invalid after the
// compiler's local normalization and bounded model-repair attempt. Job-level
// retries must not replay it blindly; raw evidence remains durable.
type CompileContractError struct {
	Reason string
}

func (e *CompileContractError) Error() string {
	if e == nil || e.Reason == "" {
		return "semantic compiler contract failed"
	}
	return "semantic compiler contract failed: " + e.Reason
}

type CompileSource struct {
	SourceRef  string    `json:"source_ref"`
	SessionRef string    `json:"session_ref,omitempty"`
	Actor      string    `json:"actor"`
	Text       string    `json:"text"`
	OccurredAt time.Time `json:"occurred_at,omitempty"`
}

type PlanningOptions struct {
	Mode                 MemoryWriteMode
	MaxSources           int
	MaxSourcesPerSession int
	MaxSourceRunes       int
}

type SemanticCandidate struct {
	EventID string
	Source  CompileSource
	Score   float64
}

type SemanticPlan struct {
	Candidates      []SemanticCandidate
	SkippedEventIDs []string
}

type SemanticPlanner interface {
	Plan(context.Context, []RawEvent, PlanningOptions) (SemanticPlan, error)
}

// SemanticPlanningBatch describes one independent planning scope. Batch
// planners may share local inference work across scopes, but must preserve the
// same per-scope selection semantics as SemanticPlanner.Plan.
type SemanticPlanningBatch struct {
	Events  []RawEvent
	Options PlanningOptions
}

type BatchSemanticPlanner interface {
	SemanticPlanner
	PlanBatch(context.Context, []SemanticPlanningBatch) ([]SemanticPlan, error)
}

type SemanticCompiler interface {
	Compile(context.Context, CompileRequest) (CompileResponse, error)
}

// CompileRequestSizer lets the orchestration layer reject or split an
// oversized request before any remote API call is made. Implementations must
// include their system prompt and structured-output schema in the estimate.
type CompileRequestSizer interface {
	EstimateInputTokens(CompileRequest) (int, error)
}

// CompileArtifactStore exposes completed semantic artifacts to orchestration
// code without exposing a compiler's on-disk representation. A completed
// empty artifact is a deliberate semantic skip and is safe to reuse.
type CompileArtifactStore interface {
	CompileArtifactCached([]CompileSource) (bool, error)
	MarkCompileArtifactSkipped([]CompileSource) error
}

type ConflictDecision string

const (
	DecisionSupersedes   ConflictDecision = "supersedes"
	DecisionCoexists     ConflictDecision = "coexists"
	DecisionScoped       ConflictDecision = "scoped"
	DecisionDuplicate    ConflictDecision = "duplicate"
	DecisionUncertain    ConflictDecision = "uncertain"
	DecisionNeedsCompile ConflictDecision = "needs_recompile"
)

type Conflict struct {
	ID         string         `json:"id"`
	Space      string         `json:"space"`
	SlotID     string         `json:"slot_id"`
	Generation string         `json:"generation"`
	Status     SemanticStatus `json:"status"`
	Members    []MemoryNode   `json:"members"`
	CreatedAt  time.Time      `json:"created_at"`
}

type AdjudicationRequest struct {
	Conflict         Conflict     `json:"conflict"`
	AuthorityPolicy  string       `json:"authority_policy"`
	PriorResolutions []Resolution `json:"prior_resolutions,omitempty"`
}

type AdjudicationResponse struct {
	ConflictID string           `json:"conflict_id,omitempty"`
	Decision   ConflictDecision `json:"decision"`
	WinnerIDs  []string         `json:"winner_ids,omitempty"`
	LoserIDs   []string         `json:"loser_ids,omitempty"`
	Conditions string           `json:"conditions,omitempty"`
	ValidFrom  time.Time        `json:"valid_from,omitempty"`
	ValidUntil time.Time        `json:"valid_until,omitempty"`
	SupportIDs []string         `json:"support_ids,omitempty"`
	Reason     string           `json:"reason,omitempty"`
	Usage      APIUsage         `json:"usage,omitempty"`
}

type ConflictAdjudicator interface {
	Adjudicate(context.Context, AdjudicationRequest) (AdjudicationResponse, error)
}

type AdjudicationBatchRequest struct {
	Items []AdjudicationRequest `json:"items"`
}

type AdjudicationBatchResponse struct {
	Results []AdjudicationResponse `json:"results"`
	Usage   APIUsage               `json:"usage,omitempty"`
}

type BatchConflictAdjudicator interface {
	AdjudicateBatch(context.Context, AdjudicationBatchRequest) (AdjudicationBatchResponse, error)
}

type Resolution struct {
	ID         string           `json:"id"`
	ConflictID string           `json:"conflict_id"`
	Generation string           `json:"generation"`
	Decision   ConflictDecision `json:"decision"`
	WinnerIDs  []string         `json:"winner_ids,omitempty"`
	LoserIDs   []string         `json:"loser_ids,omitempty"`
	Conditions string           `json:"conditions,omitempty"`
	ValidFrom  time.Time        `json:"valid_from,omitempty"`
	ValidUntil time.Time        `json:"valid_until,omitempty"`
	SupportIDs []string         `json:"support_ids,omitempty"`
	Reason     string           `json:"reason,omitempty"`
	PolicyID   string           `json:"policy_id"`
	CreatedAt  time.Time        `json:"created_at"`
}

type Identity struct {
	ID          string `json:"id"`
	Space       string `json:"space"`
	Canonical   string `json:"canonical"`
	Type        string `json:"type,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

type MemoryNode struct {
	ID            string         `json:"id"`
	Space         string         `json:"space"`
	ContextID     string         `json:"context_id,omitempty"`
	Kind          NodeKind       `json:"kind"`
	ClaimType     ClaimType      `json:"claim_type,omitempty"`
	Statement     string         `json:"statement"`
	SubjectID     string         `json:"subject_id,omitempty"`
	Subject       string         `json:"subject,omitempty"`
	Facet         Facet          `json:"facet,omitempty"`
	AttributeKey  string         `json:"attribute_key,omitempty"`
	ScopeKey      string         `json:"scope_key,omitempty"`
	SlotID        string         `json:"slot_id,omitempty"`
	Value         ClaimValue     `json:"value,omitempty"`
	ValidFrom     time.Time      `json:"valid_from,omitempty"`
	ValidUntil    time.Time      `json:"valid_until,omitempty"`
	EvidenceMode  EvidenceMode   `json:"evidence_mode,omitempty"`
	Status        SemanticStatus `json:"status"`
	Sources       []SourceSpan   `json:"sources,omitempty"`
	Keys          []string       `json:"keys,omitempty"`
	RetrievalCues []string       `json:"retrieval_cues,omitempty"`
	Payload       map[string]any `json:"payload,omitempty"`
	CreatedAt     time.Time      `json:"created_at"`
}

type VectorPurpose string

const (
	VectorQuery   VectorPurpose = "query"
	VectorContent VectorPurpose = "content"
	VectorTrigger VectorPurpose = "trigger"
)

type Vectorizer interface {
	Model() string
	Dimensions() int
	Embed(context.Context, []string, VectorPurpose) ([][]float32, error)
}

type RetrievalEncodingKind string

const (
	RetrievalQuery    RetrievalEncodingKind = "query"
	RetrievalDocument RetrievalEncodingKind = "document"
)

type RetrievalTokenVector struct {
	TokenID  int64
	Position int
	Weight   float32
	Values   []float32
}

type RetrievalEncoding struct {
	Dense  []float32
	Sparse map[int64]float32
	Multi  []RetrievalTokenVector
}

type RetrievalEncoder interface {
	Model() string
	Revision() string
	TokenizerHash() string
	Encode(context.Context, []string, RetrievalEncodingKind) ([]RetrievalEncoding, error)
	Split(text string, maxTokens, overlap int) ([]string, error)
}

type RetrievalSplitSpec struct {
	MaxTokens int
	Overlap   int
}

type RetrievalMultiSplitter interface {
	SplitMany(text string, specs []RetrievalSplitSpec) ([][]string, error)
}

// RetrievalChannelEncoder avoids materializing token-level ColBERT vectors
// while building query-independent dense and sparse sidecar channels.
type RetrievalChannelEncoder interface {
	EncodeChannels(context.Context, []string, RetrievalEncodingKind) ([]RetrievalEncoding, error)
}

type SearchRequest struct {
	Space              string    `json:"space"`
	Query              string    `json:"query"`
	ReferenceTime      time.Time `json:"reference_time,omitempty"`
	ContextID          string    `json:"context_id,omitempty"`
	MaxEvidence        int       `json:"max_evidence,omitempty"`
	MaxContextTokens   int       `json:"max_context_tokens,omitempty"`
	IncludeDiagnostics bool      `json:"include_diagnostics,omitempty"`
}

type Evidence struct {
	ID             string         `json:"id"`
	ResourceID     string         `json:"resource_id"`
	ResourceKind   string         `json:"resource_kind"`
	Content        string         `json:"content"`
	Score          float64        `json:"score"`
	OccurredAt     time.Time      `json:"occurred_at,omitempty"`
	ContextID      string         `json:"context_id,omitempty"`
	Actor          string         `json:"actor,omitempty"`
	SlotID         string         `json:"slot_id,omitempty"`
	Status         SemanticStatus `json:"status,omitempty"`
	SourceEventIDs []string       `json:"source_event_ids,omitempty"`
	MatchReasons   []string       `json:"match_reasons,omitempty"`
}

type SearchDiagnostics struct {
	FTSCandidates            int                         `json:"fts_candidates"`
	VectorCandidates         int                         `json:"vector_candidates"`
	SlotCandidates           int                         `json:"slot_candidates"`
	Deduplicated             int                         `json:"deduplicated"`
	IndexLag                 int64                       `json:"index_lag"`
	Duration                 time.Duration               `json:"duration"`
	Route                    []string                    `json:"route,omitempty"`
	QueryTerms               []SearchTermDiagnostic      `json:"query_terms,omitempty"`
	Candidates               []SearchCandidateDiagnostic `json:"candidates,omitempty"`
	BGEFTSCandidates         int                         `json:"bge_fts_candidates,omitempty"`
	BGEDenseCandidates       int                         `json:"bge_dense_candidates,omitempty"`
	BGESparseCandidates      int                         `json:"bge_sparse_candidates,omitempty"`
	MaxSimCandidates         int                         `json:"maxsim_candidates,omitempty"`
	PPRSeedEvents            int                         `json:"ppr_seed_events,omitempty"`
	PPRCandidates            int                         `json:"ppr_candidates,omitempty"`
	RRFCandidates            int                         `json:"rrf_candidates,omitempty"`
	PPRAdditions             int                         `json:"ppr_additions,omitempty"`
	ExactScoredEvents        int                         `json:"exact_scored_events,omitempty"`
	ExactScoredSpans         int                         `json:"exact_scored_spans,omitempty"`
	DocumentEncodeBatches    int                         `json:"document_encode_batches,omitempty"`
	ContextCompanionEvents   int                         `json:"context_companion_events,omitempty"`
	ContextExpansionContexts int                         `json:"context_expansion_contexts,omitempty"`
	ContextExpandedEvents    int                         `json:"context_expanded_events,omitempty"`
	ContextExpandedSpans     int                         `json:"context_expanded_spans,omitempty"`
	SelectedSourceEvents     []string                    `json:"selected_source_events,omitempty"`
	SelectedContextIDs       []string                    `json:"selected_context_ids,omitempty"`
	CandidateSourceEvents    []string                    `json:"candidate_source_events,omitempty"`
	CandidateContextIDs      []string                    `json:"candidate_context_ids,omitempty"`
	ExactSourceEvents        []string                    `json:"exact_source_events,omitempty"`
	ExactContextIDs          []string                    `json:"exact_context_ids,omitempty"`
	EvidenceTokens           int                         `json:"evidence_tokens,omitempty"`
	StageLatency             map[string]time.Duration    `json:"stage_latency,omitempty"`
	RetrievalModelRevision   string                      `json:"retrieval_model_revision,omitempty"`
	RetrievalSidecarSchema   string                      `json:"retrieval_sidecar_schema,omitempty"`
	RetrievalTokenizerHash   string                      `json:"retrieval_tokenizer_hash,omitempty"`
	LedgerEvents             int                         `json:"ledger_events,omitempty"`
	SidecarEvents            int                         `json:"sidecar_events,omitempty"`
	SidecarAligned           bool                        `json:"sidecar_aligned,omitempty"`
	FallbackReason           string                      `json:"fallback_reason,omitempty"`
}

type SearchTermDiagnostic struct {
	Text             string  `json:"text"`
	Weight           float64 `json:"weight"`
	ContextFrequency int     `json:"context_frequency"`
}

type SearchCandidateDiagnostic struct {
	ID             string             `json:"id"`
	ResourceID     string             `json:"resource_id"`
	ResourceKind   string             `json:"resource_kind"`
	ContextID      string             `json:"context_id,omitempty"`
	Status         SemanticStatus     `json:"status,omitempty"`
	Score          float64            `json:"score"`
	ChannelRanks   map[string]int     `json:"channel_ranks,omitempty"`
	ChannelScores  map[string]float64 `json:"channel_scores,omitempty"`
	MaxSimScore    float64            `json:"maxsim_score,omitempty"`
	QueryCoverage  float64            `json:"query_coverage,omitempty"`
	MatchReasons   []string           `json:"match_reasons,omitempty"`
	SourceEventIDs []string           `json:"source_event_ids,omitempty"`
	Content        string             `json:"content"`
	Selected       bool               `json:"selected"`
}

type SearchResult struct {
	Route        []string          `json:"route"`
	Evidence     []Evidence        `json:"evidence"`
	CurrentView  []MemoryNode      `json:"current_view,omitempty"`
	Conflicts    []Conflict        `json:"conflicts,omitempty"`
	Insufficient bool              `json:"insufficient"`
	Diagnostics  SearchDiagnostics `json:"diagnostics,omitempty"`
}

type ConflictSelector struct {
	Space       string   `json:"space"`
	ConflictIDs []string `json:"conflict_ids,omitempty"`
	SlotIDs     []string `json:"slot_ids,omitempty"`
}

type Selector struct {
	Space      string   `json:"space"`
	EventIDs   []string `json:"event_ids,omitempty"`
	MemoryIDs  []string `json:"memory_ids,omitempty"`
	ContextIDs []string `json:"context_ids,omitempty"`
}

type ForgetMode string

const (
	ForgetTombstone ForgetMode = "tombstone"
	ForgetPurge     ForgetMode = "purge"
)

type JobRef struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Status string `json:"status"`
}

type HealthReport struct {
	Healthy          bool     `json:"healthy"`
	LedgerPath       string   `json:"ledger_path"`
	IndexPath        string   `json:"index_path"`
	LedgerQuickCheck string   `json:"ledger_quick_check"`
	IndexQuickCheck  string   `json:"index_quick_check"`
	PendingJobs      int      `json:"pending_jobs"`
	PendingOutbox    int      `json:"pending_outbox"`
	IndexGeneration  int64    `json:"index_generation"`
	IndexedLedgerSeq int64    `json:"indexed_ledger_seq"`
	Warnings         []string `json:"warnings,omitempty"`
}
