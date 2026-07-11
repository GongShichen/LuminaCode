package agentbench

import (
	"context"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
)

const (
	SuiteAiderPolyglotSmoke     = "aider_polyglot_smoke"
	SuiteTerminalBench          = "terminal_bench"
	SuiteTauBench               = "tau_bench"
	SuiteSWEBenchVerified       = "swebench_verified"
	SuiteSWEBenchVerifiedSubset = "swebench_verified_subset"
	SuiteTerminalBenchSmoke     = "terminal_bench_smoke"
	SuiteLongMemEval            = "longmemeval"
	SuiteMemoryArena            = "memoryarena"
	DefaultCaseTimeout          = 900
	ReportPrefix                = "agent-benchmark-"
)

type CaseSpec struct {
	ID               string   `json:"id"`
	Benchmark        string   `json:"benchmark"`
	Repo             string   `json:"repo,omitempty"`
	WorkDir          string   `json:"workdir,omitempty"`
	SetupCommands    []string `json:"setup_commands,omitempty"`
	Prompt           string   `json:"prompt"`
	TestCommands     []string `json:"test_commands,omitempty"`
	TimeoutSeconds   int      `json:"timeout_seconds,omitempty"`
	ExpectedArtifact string   `json:"expected_artifact,omitempty"`
	InstanceID       string   `json:"instance_id,omitempty"`
}

type TimelineEvent struct {
	Name              string         `json:"name"`
	ElapsedMillis     int64          `json:"elapsed_ms"`
	TimestampUnixNano int64          `json:"timestamp_unix_nano"`
	Metadata          map[string]any `json:"metadata,omitempty"`
}

type CommandResult struct {
	Command        string  `json:"command"`
	ExitCode       int     `json:"exit_code"`
	Stdout         string  `json:"stdout,omitempty"`
	Stderr         string  `json:"stderr,omitempty"`
	DurationSecond float64 `json:"duration_seconds"`
	TimedOut       bool    `json:"timed_out"`
	Error          string  `json:"error,omitempty"`
}

type AgentRunResult struct {
	Events          []agent.StreamEvent `json:"events"`
	FinalText       string              `json:"final_text,omitempty"`
	ErrorType       string              `json:"error_type,omitempty"`
	TransientErrors []string            `json:"transient_errors,omitempty"`
	InputTokens     int                 `json:"input_tokens"`
	OutputTokens    int                 `json:"output_tokens"`
	ToolCalls       int                 `json:"tool_calls"`
	TTFTMillis      *float64            `json:"ttft_ms,omitempty"`
	FirstToolCallMS *float64            `json:"first_tool_call_ms,omitempty"`
	Timeline        []TimelineEvent     `json:"timeline,omitempty"`
}

type CaseResult struct {
	Case              CaseSpec        `json:"case"`
	Resolved          bool            `json:"resolved"`
	PatchApplyRate    float64         `json:"patch_apply_rate"`
	TestPassRate      float64         `json:"test_pass_rate"`
	DurationSeconds   float64         `json:"duration_seconds"`
	TTFTMillis        *float64        `json:"ttft_ms,omitempty"`
	FirstToolCallMS   *float64        `json:"first_tool_call_ms,omitempty"`
	FirstTestMS       *float64        `json:"first_test_ms,omitempty"`
	InputTokens       int             `json:"input_tokens"`
	OutputTokens      int             `json:"output_tokens"`
	ToolCalls         int             `json:"tool_calls"`
	ErrorType         string          `json:"error_type,omitempty"`
	FinalPatchPath    string          `json:"final_patch_path,omitempty"`
	TranscriptPath    string          `json:"transcript_path,omitempty"`
	PromptPath        string          `json:"prompt_path,omitempty"`
	TimelinePath      string          `json:"timeline_path,omitempty"`
	TestOutputPath    string          `json:"test_output_path,omitempty"`
	ResultPath        string          `json:"result_path,omitempty"`
	WorkDir           string          `json:"workdir"`
	SetupResults      []CommandResult `json:"setup_results,omitempty"`
	TestResults       []CommandResult `json:"test_results,omitempty"`
	ExpectedArtifact  string          `json:"expected_artifact,omitempty"`
	ExpectedSatisfied bool            `json:"expected_satisfied"`
	Timeline          []TimelineEvent `json:"timeline,omitempty"`
	ExpectedAnswer    any             `json:"expected_answer,omitempty"`
	Hypothesis        string          `json:"hypothesis,omitempty"`
	MemoryHits        []string        `json:"memory_hits,omitempty"`
	MemoryHitDetails  []MemoryHit     `json:"memory_hit_details,omitempty"`
	MemoryMetrics     *MemoryMetrics  `json:"memory_metrics,omitempty"`
	MemoryStorePath   string          `json:"memory_store_path,omitempty"`
	AnswerMatch       bool            `json:"answer_match"`
}

type MemoryHit struct {
	Rank            int      `json:"rank"`
	MemoryID        string   `json:"memory_id"`
	DocumentKind    string   `json:"document_kind,omitempty"`
	ParentID        string   `json:"parent_id,omitempty"`
	MessageID       string   `json:"message_id,omitempty"`
	Title           string   `json:"title"`
	SourceSessionID string   `json:"source_session_id,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	Evidence        bool     `json:"evidence"`
	Stale           bool     `json:"stale"`
}

type MemoryMetrics struct {
	RetrievedCount            int      `json:"retrieved_count"`
	EvidenceTotal             int      `json:"evidence_total,omitempty"`
	EvidenceHitCount          int      `json:"evidence_hit_count,omitempty"`
	EvidenceHit               bool     `json:"evidence_hit"`
	EvidenceRecallAtK         float64  `json:"evidence_recall_at_k,omitempty"`
	FirstEvidenceRank         *int     `json:"first_evidence_rank,omitempty"`
	EvidenceMRR               float64  `json:"evidence_mrr,omitempty"`
	GoldSourceSessionCount    int      `json:"gold_source_session_count,omitempty"`
	GoldSourceSessionHitCount int      `json:"gold_source_session_hit_count,omitempty"`
	SourceSessionRecall       float64  `json:"source_session_recall,omitempty"`
	GoldMessageCount          int      `json:"gold_message_count,omitempty"`
	GoldMessageHitCount       int      `json:"gold_message_hit_count,omitempty"`
	GoldMessageRecall         float64  `json:"gold_message_recall,omitempty"`
	GoldChunkCount            int      `json:"gold_chunk_count,omitempty"`
	GoldChunkHitCount         int      `json:"gold_chunk_hit_count,omitempty"`
	InjectedChunkRecall       float64  `json:"injected_chunk_recall,omitempty"`
	InjectedTextCoverage      float64  `json:"injected_text_coverage,omitempty"`
	MemoryTokenEstimate       int      `json:"memory_token_estimate,omitempty"`
	MemoryTokenRatio          float64  `json:"memory_token_ratio,omitempty"`
	RetrievalRuns             int      `json:"retrieval_runs,omitempty"`
	RetrievalDurationMS       int64    `json:"retrieval_duration_ms,omitempty"`
	StaleUseCount             int      `json:"stale_use_count,omitempty"`
	StaleUseRate              float64  `json:"stale_use_rate,omitempty"`
	RetrievalErrorType        string   `json:"retrieval_error_type,omitempty"`
	SubtaskTotal              int      `json:"subtask_total,omitempty"`
	SubtaskAnswered           int      `json:"subtask_answered,omitempty"`
	SubtaskAnswerRate         float64  `json:"subtask_answer_rate,omitempty"`
	MemoryUpdateCount         int      `json:"memory_update_count,omitempty"`
	MemoryUpdateSuccessRate   float64  `json:"memory_update_success_rate,omitempty"`
	PreviousSubtaskHitCount   int      `json:"previous_subtask_hit_count,omitempty"`
	PreviousSubtaskHitRate    float64  `json:"previous_subtask_hit_rate,omitempty"`
	RecallWarnings            []string `json:"recall_warnings,omitempty"`
}

type LatencySummary struct {
	P50 *float64 `json:"p50,omitempty"`
	P90 *float64 `json:"p90,omitempty"`
	P95 *float64 `json:"p95,omitempty"`
}

type SuiteSummary struct {
	TotalCases              int                `json:"total_cases"`
	ResolvedCases           int                `json:"resolved_cases"`
	PassRate                float64            `json:"pass_rate"`
	AverageDurationSeconds  float64            `json:"average_duration_seconds"`
	DurationSeconds         LatencySummary     `json:"duration_seconds"`
	TTFTMillis              LatencySummary     `json:"ttft_ms"`
	FirstToolCallMillis     LatencySummary     `json:"first_tool_call_ms"`
	FirstTestMillis         LatencySummary     `json:"first_test_ms"`
	AverageInputTokens      float64            `json:"average_input_tokens"`
	AverageOutputTokens     float64            `json:"average_output_tokens"`
	InputTokens             LatencySummary     `json:"input_tokens"`
	OutputTokens            LatencySummary     `json:"output_tokens"`
	FailureCategories       map[string]int     `json:"failure_categories,omitempty"`
	TopFailingCases         []TopFailingCase   `json:"top_failing_cases,omitempty"`
	AveragePatchApplyRate   float64            `json:"average_patch_apply_rate"`
	AverageTestPassRate     float64            `json:"average_test_pass_rate"`
	TotalToolCalls          int                `json:"total_tool_calls"`
	BenchmarkSuiteBreakdown map[string]float64 `json:"benchmark_suite_breakdown,omitempty"`
	Memory                  MemorySummary      `json:"memory,omitempty"`
}

type MemorySummary struct {
	EvidenceCaseCount              int            `json:"evidence_case_count,omitempty"`
	EvidenceHitCases               int            `json:"evidence_hit_cases,omitempty"`
	AverageRetrievedCount          float64        `json:"average_retrieved_count,omitempty"`
	EvidenceHitRate                float64        `json:"evidence_hit_rate,omitempty"`
	AverageEvidenceRecallAtK       float64        `json:"average_evidence_recall_at_k,omitempty"`
	AverageEvidenceMRR             float64        `json:"average_evidence_mrr,omitempty"`
	AverageSourceSessionRecall     float64        `json:"average_source_session_recall,omitempty"`
	AverageGoldMessageRecall       float64        `json:"average_gold_message_recall,omitempty"`
	AverageInjectedChunkRecall     float64        `json:"average_injected_chunk_recall,omitempty"`
	AverageInjectedTextCoverage    float64        `json:"average_injected_text_coverage,omitempty"`
	AverageMemoryTokenEstimate     float64        `json:"average_memory_token_estimate,omitempty"`
	AverageMemoryTokenRatio        float64        `json:"average_memory_token_ratio,omitempty"`
	AverageRetrievalDurationMS     float64        `json:"average_retrieval_duration_ms,omitempty"`
	AverageStaleUseRate            float64        `json:"average_stale_use_rate,omitempty"`
	AverageSubtaskAnswerRate       float64        `json:"average_subtask_answer_rate,omitempty"`
	AverageMemoryUpdateSuccessRate float64        `json:"average_memory_update_success_rate,omitempty"`
	AveragePreviousSubtaskHitRate  float64        `json:"average_previous_subtask_hit_rate,omitempty"`
	RetrievalErrorCategories       map[string]int `json:"retrieval_error_categories,omitempty"`
}

type TopFailingCase struct {
	ID        string  `json:"id"`
	ErrorType string  `json:"error_type,omitempty"`
	Duration  float64 `json:"duration_seconds"`
}

type HarnessArtifactCheck struct {
	Path      string `json:"path"`
	Concrete  bool   `json:"concrete"`
	Exists    bool   `json:"exists"`
	SizeBytes *int64 `json:"size_bytes,omitempty"`
}

type HarnessRepairDiagnostic struct {
	Triggered           bool     `json:"triggered"`
	ExitStatus          *int     `json:"exit_status,omitempty"`
	MissingBeforeRepair []string `json:"missing_before_repair,omitempty"`
}

type HarnessDiagnostic struct {
	TaskID                   string                  `json:"task_id,omitempty"`
	Path                     string                  `json:"path"`
	InstructionPath          string                  `json:"instruction_path,omitempty"`
	AgentExitStatus          *int                    `json:"agent_exit_status,omitempty"`
	FinalAgentExitStatus     *int                    `json:"final_agent_exit_status,omitempty"`
	ExplicitArtifactChecks   []HarnessArtifactCheck  `json:"explicit_artifact_checks,omitempty"`
	ExplicitMissingArtifacts []string                `json:"explicit_missing_artifacts,omitempty"`
	PostFlightRepair         HarnessRepairDiagnostic `json:"post_flight_repair,omitempty"`
	ProcessSnapshotPath      string                  `json:"process_snapshot_path,omitempty"`
	HighCPUProcesses         []string                `json:"high_cpu_processes,omitempty"`
	FailureCategory          string                  `json:"failure_category,omitempty"`
	Raw                      map[string]any          `json:"raw,omitempty"`
}

type Report struct {
	Suite                string              `json:"suite"`
	GeneratedAt          string              `json:"generated_at"`
	DebugRun             bool                `json:"debug_run"`
	RootDir              string              `json:"root_dir"`
	OutputDir            string              `json:"output_dir"`
	WorkDir              string              `json:"work_dir"`
	BenchmarkDir         string              `json:"benchmark_dir,omitempty"`
	Model                string              `json:"model,omitempty"`
	Summary              SuiteSummary        `json:"summary"`
	Results              []CaseResult        `json:"results"`
	PredictionsPath      string              `json:"predictions_path,omitempty"`
	HarnessOutputPath    string              `json:"harness_output_path,omitempty"`
	HarnessCommand       string              `json:"harness_command,omitempty"`
	HarnessExitCode      *int                `json:"harness_exit_code,omitempty"`
	HarnessParsedStats   map[string]any      `json:"harness_parsed_stats,omitempty"`
	OfficialMetrics      map[string]any      `json:"official_metrics,omitempty"`
	LuminaDiagnostics    []HarnessDiagnostic `json:"lumina_diagnostics,omitempty"`
	UpstreamStatusBefore string              `json:"upstream_status_before,omitempty"`
	UpstreamStatusAfter  string              `json:"upstream_status_after,omitempty"`
	UpstreamDirtyAfter   bool                `json:"upstream_dirty_after"`
}

type AgentRunner interface {
	Run(ctx context.Context, cfg config.Config, prompt string, sessionID string) AgentRunResult
}

type RunnerOptions struct {
	Suite              string
	CasesPath          string
	CaseID             string
	Limit              int
	RootDir            string
	OutputDir          string
	WorkDir            string
	ArtifactsDir       string
	BenchmarkDir       string
	TimeoutSeconds     int
	CaseParallel       int
	NoResume           bool
	Config             config.Config
	AgentRunner        AgentRunner
	HarnessCmd         string
	SWEBenchHarnessCmd string
	PreparedEnv        bool
	Now                func() time.Time
}
