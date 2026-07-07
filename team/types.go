package team

import (
	"time"
)

type TeamSpec struct {
	Name             string          `json:"name" yaml:"name"`
	DisplayName      string          `json:"display_name" yaml:"display_name"`
	Description      string          `json:"description" yaml:"description"`
	EntryAgent       string          `json:"entry_agent" yaml:"entry_agent"`
	Loop             TeamLoopSpec    `json:"loop" yaml:"loop"`
	Gates            TeamGateSpec    `json:"gates" yaml:"gates"`
	Transcript       TranscriptSpec  `json:"transcript" yaml:"transcript"`
	Agents           []string        `json:"agents" yaml:"agents"`
	RootDir          string          `json:"root_dir" yaml:"-"`
	TeamSystemPath   string          `json:"team_system_path" yaml:"-"`
	CompletionPolicy string          `json:"completion_policy_path" yaml:"-"`
	AgentSpecs       []TeamAgentSpec `json:"agent_specs" yaml:"-"`
	AgentMap         map[string]int  `json:"-" yaml:"-"`
	LoadedAt         time.Time       `json:"loaded_at" yaml:"-"`
}

type TeamLoopSpec struct {
	MaxIterations        int    `json:"max_iterations" yaml:"max_iterations"`
	MaxParallelAgents    int    `json:"max_parallel_agents" yaml:"max_parallel_agents"`
	CompletionPolicy     string `json:"completion_policy" yaml:"completion_policy"`
	RequireFinalArtifact bool   `json:"require_final_artifact" yaml:"require_final_artifact"`
	StopPolicy           string `json:"stop_policy" yaml:"stop_policy"`
}

type TeamGateSpec struct {
	RequireContract        bool   `json:"require_contract" yaml:"require_contract"`
	QAAgent                string `json:"qa_agent" yaml:"qa_agent"`
	ReviewerAgent          string `json:"reviewer_agent" yaml:"reviewer_agent"`
	NonblockingFindings    string `json:"nonblocking_findings" yaml:"nonblocking_findings"`
	DeferralRequiresReason bool   `json:"deferral_requires_reason" yaml:"deferral_requires_reason"`
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

type GateStatus struct {
	QA       string `json:"qa"`
	Reviewer string `json:"reviewer"`
}

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
