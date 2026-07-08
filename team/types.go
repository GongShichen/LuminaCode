package team

import (
	"time"
)

type TeamSpec struct {
	Name             string               `json:"name" yaml:"name"`
	DisplayName      string               `json:"display_name" yaml:"display_name"`
	Description      string               `json:"description" yaml:"description"`
	EntryAgent       string               `json:"entry_agent" yaml:"entry_agent"`
	Loop             TeamLoopSpec         `json:"loop" yaml:"loop"`
	Gates            TeamGateSpec         `json:"gates" yaml:"gates"`
	Output           TeamOutputSpec       `json:"output" yaml:"output"`
	TaskPolicies     []TeamTaskPolicySpec `json:"task_policies" yaml:"task_policies"`
	Transcript       TranscriptSpec       `json:"transcript" yaml:"transcript"`
	Agents           []string             `json:"agents" yaml:"agents"`
	RootDir          string               `json:"root_dir" yaml:"-"`
	TeamSystemPath   string               `json:"team_system_path" yaml:"-"`
	SharedPromptPath string               `json:"shared_prompt_path" yaml:"-"`
	CompletionPolicy string               `json:"completion_policy_path" yaml:"-"`
	AgentSpecs       []TeamAgentSpec      `json:"agent_specs" yaml:"-"`
	AgentMap         map[string]int       `json:"-" yaml:"-"`
	LoadedAt         time.Time            `json:"loaded_at" yaml:"-"`
}

type TeamLoopSpec struct {
	MaxIterations                        int    `json:"max_iterations" yaml:"max_iterations"`
	MaxParallelAgents                    int    `json:"max_parallel_agents" yaml:"max_parallel_agents"`
	A2ADefaultTimeoutSeconds             int    `json:"a2a_default_timeout_seconds" yaml:"a2a_default_timeout_seconds"`
	MinA2ATimeoutSeconds                 int    `json:"min_a2a_timeout_seconds" yaml:"min_a2a_timeout_seconds"`
	RequireContractProjectRootRuntimeDir bool   `json:"require_contract_project_root_runtime_dir" yaml:"require_contract_project_root_runtime_dir"`
	WaitForPendingA2ABeforeNextIteration *bool  `json:"wait_for_pending_a2a_before_next_iteration,omitempty" yaml:"wait_for_pending_a2a_before_next_iteration"`
	CompletionPolicy                     string `json:"completion_policy" yaml:"completion_policy"`
	RequireFinalArtifact                 bool   `json:"require_final_artifact" yaml:"require_final_artifact"`
	StopPolicy                           string `json:"stop_policy" yaml:"stop_policy"`
}

type TeamGateSpec struct {
	RequireContract        bool                `json:"require_contract" yaml:"require_contract"`
	NonblockingFindings    string              `json:"nonblocking_findings" yaml:"nonblocking_findings"`
	DeferralRequiresReason bool                `json:"deferral_requires_reason" yaml:"deferral_requires_reason"`
	Checks                 []TeamGateCheckSpec `json:"checks" yaml:"checks"`
}

type TeamGateCheckSpec struct {
	Name                     string   `json:"name" yaml:"name"`
	Agent                    string   `json:"agent" yaml:"agent"`
	PassStatuses             []string `json:"pass_statuses" yaml:"pass_statuses"`
	AllowedStatuses          []string `json:"allowed_statuses" yaml:"allowed_statuses"`
	EvidenceRequiredStatuses []string `json:"evidence_required_statuses" yaml:"evidence_required_statuses"`
	FindingsRequiredStatuses []string `json:"findings_required_statuses" yaml:"findings_required_statuses"`
	BlockingFindingsFail     bool     `json:"blocking_findings_fail" yaml:"blocking_findings_fail"`
}

type TeamOutputSpec struct {
	ExportToWorkdir bool     `json:"export_to_workdir" yaml:"export_to_workdir"`
	Directory       string   `json:"directory" yaml:"directory"`
	Artifacts       []string `json:"artifacts" yaml:"artifacts"`
}

type TeamTaskPolicySpec struct {
	Name                              string   `json:"name" yaml:"name"`
	Description                       string   `json:"description" yaml:"description"`
	Targets                           []string `json:"targets" yaml:"targets"`
	TaskTypes                         []string `json:"task_types" yaml:"task_types"`
	BeforeContract                    bool     `json:"before_contract" yaml:"before_contract"`
	RequiresContract                  bool     `json:"requires_contract" yaml:"requires_contract"`
	AuditWrites                       bool     `json:"audit_writes" yaml:"audit_writes"`
	ExclusiveWorkspace                bool     `json:"exclusive_workspace" yaml:"exclusive_workspace"`
	RestrictWritesToExpectedArtifacts bool     `json:"restrict_writes_to_expected_artifacts" yaml:"restrict_writes_to_expected_artifacts"`
	AllowedWriteGlobs                 []string `json:"allowed_write_globs" yaml:"allowed_write_globs"`
	DeniedWriteGlobs                  []string `json:"denied_write_globs" yaml:"denied_write_globs"`
}

type TeamTemplateResult struct {
	TeamName     string   `json:"team_name"`
	DisplayName  string   `json:"display_name"`
	Path         string   `json:"path"`
	CreatedFiles []string `json:"created_files"`
	AgentCount   int      `json:"agent_count"`
}

type TranscriptSpec struct {
	ShowMemberDialogue bool `json:"show_member_dialogue" yaml:"show_member_dialogue"`
	ShowToolDetails    bool `json:"show_tool_details" yaml:"show_tool_details"`
	ShowThinking       bool `json:"show_thinking" yaml:"show_thinking"`
}

type TeamAgentSpec struct {
	Name             string   `json:"name" yaml:"name"`
	DisplayName      string   `json:"display_name" yaml:"display_name"`
	Description      string   `json:"description" yaml:"description"`
	CommunicatesWith any      `json:"communicates_with" yaml:"communicates_with"`
	Model            string   `json:"model" yaml:"model"`
	Tools            string   `json:"tools" yaml:"tools"`
	MaxTurnsPerTask  int      `json:"max_turns_per_task" yaml:"max_turns_per_task"`
	PrivateSkills    bool     `json:"private_skills" yaml:"private_skills"`
	AllowedAgents    []string `json:"allowed_agents" yaml:"-"`
	RootDir          string   `json:"root_dir" yaml:"-"`
	SystemPromptPath string   `json:"system_prompt_path" yaml:"-"`
	SkillsDir        string   `json:"skills_dir" yaml:"-"`
}

type TeamListItem struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	AgentCount  int    `json:"agent_count"`
	RootDir     string `json:"root_dir"`
}

type DialogueEntry struct {
	ID           string   `json:"id"`
	FromAgent    string   `json:"from_agent"`
	ToAgent      []string `json:"to_agent"`
	Kind         string   `json:"kind"`
	Summary      string   `json:"summary"`
	Content      string   `json:"content"`
	ArtifactRefs []string `json:"artifact_refs"`
	TaskID       string   `json:"task_id"`
	CreatedAt    string   `json:"created_at"`
}

type ActivityRow struct {
	AgentID     string `json:"agent_id"`
	DisplayName string `json:"display_name"`
	Status      string `json:"status"`
	Summary     string `json:"summary"`
	TaskID      string `json:"task_id"`
}

type Artifact struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Owner     string `json:"owner"`
	Summary   string `json:"summary"`
	Path      string `json:"path"`
	CreatedAt string `json:"created_at"`
}

type GateStatus map[string]string

type ContractCheck struct {
	Name     string `json:"name"`
	Command  string `json:"command,omitempty"`
	CWD      string `json:"cwd,omitempty"`
	Required bool   `json:"required"`
}

type AcceptanceContract struct {
	ProjectRoot         string          `json:"project_root"`
	UserRequirements    []string        `json:"user_requirements"`
	ComponentBoundaries []string        `json:"component_boundaries"`
	IntegrationContract string          `json:"integration_contract"`
	RequiredArtifacts   []string        `json:"required_artifacts"`
	RequiredCommands    []ContractCheck `json:"required_commands"`
	IntegrationSmokes   []ContractCheck `json:"integration_smokes"`
	CompletionCriteria  []string        `json:"completion_criteria"`
	CreatedAt           string          `json:"created_at"`
	UpdatedAt           string          `json:"updated_at"`
}

type GateEvidence struct {
	Name          string `json:"name"`
	Command       string `json:"command,omitempty"`
	CWD           string `json:"cwd,omitempty"`
	Passed        bool   `json:"passed"`
	OutputSummary string `json:"output_summary"`
}

type GateFinding struct {
	Category string `json:"category"`
	Summary  string `json:"summary"`
	Details  string `json:"details,omitempty"`
	Blocking bool   `json:"blocking"`
}

type GateVerdict struct {
	Role      string         `json:"role"`
	AgentID   string         `json:"agent_id"`
	Status    string         `json:"status"`
	Summary   string         `json:"summary"`
	Evidence  []GateEvidence `json:"evidence"`
	Findings  []GateFinding  `json:"findings"`
	CreatedAt string         `json:"created_at"`
}

type Snapshot struct {
	TeamMode         bool                   `json:"team_mode"`
	TeamSessionID    string                 `json:"team_session_id"`
	ActiveTeamID     string                 `json:"active_team_id"`
	ActiveTeamName   string                 `json:"active_team_name"`
	LoopIteration    int                    `json:"team_loop_iteration"`
	Running          bool                   `json:"running"`
	WaitingForUser   bool                   `json:"waiting_for_user"`
	Agents           []TeamAgentSpec        `json:"team_agents"`
	Dialogue         []DialogueEntry        `json:"team_dialogue_entries"`
	ActivityRows     []ActivityRow          `json:"team_activity_rows"`
	Artifacts        []Artifact             `json:"team_artifacts"`
	GateStatus       GateStatus             `json:"team_gate_status"`
	TeamContract     *AcceptanceContract    `json:"team_contract,omitempty"`
	GateVerdicts     map[string]GateVerdict `json:"team_gate_verdicts,omitempty"`
	InputEnabled     bool                   `json:"input_enabled"`
	InputPlaceholder string                 `json:"input_placeholder"`
}

type TimelineEvent struct {
	Type      string `json:"type"`
	CreatedAt string `json:"created_at"`
	Payload   any    `json:"payload"`
}
