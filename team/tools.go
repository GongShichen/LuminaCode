package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	coretools "LuminaCode/tools"
)

type A2AMessageInput struct {
	To                []string `json:"to" jsonschema:"description=One or more target team agent ids"`
	Message           string   `json:"message" jsonschema:"description=Readable message or task instructions for the target agent(s)"`
	TaskType          string   `json:"task_type,omitempty" jsonschema:"description=Task category, for example analysis implementation review qa"`
	ExpectedArtifacts []string `json:"expected_artifacts,omitempty" jsonschema:"description=Artifacts expected from the target agent(s)"`
	AwaitResponse     *bool    `json:"await_response,omitempty" jsonschema:"description=If true wait for responses up to timeout_seconds. Defaults to true when omitted."`
	TimeoutSeconds    int      `json:"timeout_seconds,omitempty" jsonschema:"description=Wait window in seconds. Timeout is not a stop condition; it returns pending status."`
}

type CompleteTeamTaskInput struct {
	FinalAnswer       string            `json:"final_answer" jsonschema:"description=Final answer to show the user"`
	QAStatus          string            `json:"qa_status,omitempty" jsonschema:"description=QA gate status when this team configures a QA gate: pass or not_applicable"`
	ReviewerStatus    string            `json:"reviewer_status,omitempty" jsonschema:"description=Reviewer gate status when this team configures a Reviewer gate: pass or accepted_with_notes"`
	RequiredArtifacts []string          `json:"required_artifacts,omitempty" jsonschema:"description=Required artifact names that must exist before completion"`
	DeferralReasons   map[string]string `json:"deferral_reasons,omitempty" jsonschema:"description=Reasons for deferring configured nonblocking findings when the team policy allows deferral"`
	Summary           string            `json:"summary,omitempty" jsonschema:"description=Short completion summary"`
}

type RecordTeamContractTool struct {
	coretools.BaseTool
	Runtime *Session
	From    string
}

func NewRecordTeamContractTool(runtime *Session, from string) *RecordTeamContractTool {
	return &RecordTeamContractTool{
		BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
			Name:            "RecordTeamContract",
			Description:     "Record or update the Team Acceptance Contract before dispatching implementation, QA, or review work. Required for Product Development Team completion.",
			InputPrototype:  AcceptanceContract{},
			ReadOnly:        coretools.BoolPtr(false),
			ConcurrencySafe: coretools.BoolPtr(false),
			Destructive:     coretools.BoolPtr(false),
			TimeoutSeconds:  30,
		}},
		Runtime: runtime,
		From:    from,
	}
}

func (t *RecordTeamContractTool) ValidateInput(_ coretools.ExecutionContext, input any) (bool, string) {
	contract := derefContract(input)
	if strings.TrimSpace(contract.ProjectRoot) == "" {
		return false, "project_root must not be empty."
	}
	if len(nonEmptyStrings(contract.UserRequirements)) == 0 {
		return false, "user_requirements must include at least one requirement."
	}
	if len(nonEmptyStrings(contract.RequiredArtifacts)) == 0 {
		return false, "required_artifacts must include at least one artifact."
	}
	if len(requiredChecks(contract.RequiredCommands))+len(requiredChecks(contract.IntegrationSmokes)) == 0 {
		return false, "required_commands or integration_smokes must include at least one required check."
	}
	if strings.TrimSpace(contract.IntegrationContract) == "" {
		return false, "integration_contract must state how components integrate."
	}
	return true, ""
}

func (t *RecordTeamContractTool) Execute(_ context.Context, _ coretools.ExecutionContext, input any) (string, error) {
	if t.Runtime == nil {
		return "<tool_use_error>\nTeam runtime is not available.\n</tool_use_error>", nil
	}
	return t.Runtime.RecordContract(t.From, derefContract(input)), nil
}

type SubmitGateVerdictTool struct {
	coretools.BaseTool
	Runtime *Session
	From    string
}

func NewSubmitGateVerdictTool(runtime *Session, from string) *SubmitGateVerdictTool {
	return &SubmitGateVerdictTool{
		BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
			Name:            "SubmitGateVerdict",
			Description:     "Submit a structured QA or Reviewer gate verdict with evidence and findings. Required before Team completion.",
			InputPrototype:  GateVerdict{},
			ReadOnly:        coretools.BoolPtr(false),
			ConcurrencySafe: coretools.BoolPtr(false),
			Destructive:     coretools.BoolPtr(false),
			TimeoutSeconds:  30,
		}},
		Runtime: runtime,
		From:    from,
	}
}

func (t *SubmitGateVerdictTool) ValidateInput(_ coretools.ExecutionContext, input any) (bool, string) {
	verdict := derefGateVerdict(input)
	role := strings.TrimSpace(verdict.Role)
	if role == "" {
		role = t.From
	}
	if role != "qa" && role != "reviewer" {
		return false, "role must be qa or reviewer."
	}
	if role == "qa" && !validQAStatus(verdict.Status) {
		return false, "QA status must be pass or not_applicable."
	}
	if role == "reviewer" && !validReviewerStatus(verdict.Status) && strings.TrimSpace(verdict.Status) != "reject" {
		return false, "Reviewer status must be pass, accepted_with_notes, or reject."
	}
	if strings.TrimSpace(verdict.Summary) == "" {
		return false, "summary must not be empty."
	}
	if role == "qa" && strings.TrimSpace(verdict.Status) == "pass" && len(verdict.Evidence) == 0 {
		return false, "QA pass requires evidence."
	}
	if role == "reviewer" && strings.TrimSpace(verdict.Status) != "pass" && len(verdict.Findings) == 0 {
		return false, "Reviewer non-pass verdict requires findings."
	}
	return true, ""
}

func (t *SubmitGateVerdictTool) Execute(_ context.Context, _ coretools.ExecutionContext, input any) (string, error) {
	if t.Runtime == nil {
		return "<tool_use_error>\nTeam runtime is not available.\n</tool_use_error>", nil
	}
	return t.Runtime.SubmitGateVerdict(t.From, derefGateVerdict(input)), nil
}

type SendA2AMessageTool struct {
	coretools.BaseTool
	Runtime *Session
	From    string
}

func NewSendA2AMessageTool(runtime *Session, from string) *SendA2AMessageTool {
	return &SendA2AMessageTool{
		BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
			Name:            "SendA2AMessage",
			Description:     "Send an A2A message or task to one or more agents in the current team. Member-to-member messages are visible in Team mode transcript. Timeout is only a wait window; the team loop keeps running.",
			InputPrototype:  A2AMessageInput{},
			ReadOnly:        coretools.BoolPtr(false),
			ConcurrencySafe: coretools.BoolPtr(true),
			Destructive:     coretools.BoolPtr(false),
			TimeoutSeconds:  3600,
		}},
		Runtime: runtime,
		From:    from,
	}
}

func (t *SendA2AMessageTool) ValidateInput(_ coretools.ExecutionContext, input any) (bool, string) {
	in := derefA2A(input)
	if len(in.To) == 0 {
		return false, "to must include at least one target agent id."
	}
	if strings.TrimSpace(in.Message) == "" {
		return false, "message must not be empty."
	}
	return true, ""
}

func (t *SendA2AMessageTool) TimeoutForInput(input any) time.Duration {
	in := derefA2A(input)
	wait := in.TimeoutSeconds
	if wait <= 0 {
		wait = 300
	}
	return time.Duration(wait+30) * time.Second
}

func (t *SendA2AMessageTool) Execute(ctx context.Context, _ coretools.ExecutionContext, input any) (string, error) {
	if t.Runtime == nil {
		return "<tool_use_error>\nTeam runtime is not available.\n</tool_use_error>", nil
	}
	result := t.Runtime.SendA2AMessage(ctx, t.From, derefA2A(input))
	data, _ := json.MarshalIndent(result, "", "  ")
	return string(data), nil
}

type CompleteTeamTaskTool struct {
	coretools.BaseTool
	Runtime *Session
	From    string
}

func NewCompleteTeamTaskTool(runtime *Session, from string) *CompleteTeamTaskTool {
	return &CompleteTeamTaskTool{
		BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
			Name:            "CompleteTeamTask",
			Description:     "Mark the current Team run complete. Only use after Team Leader has final answer, QA gate is pass/not_applicable, Reviewer gate is pass/accepted_with_notes, required artifacts exist, and no required task remains active.",
			InputPrototype:  CompleteTeamTaskInput{},
			ReadOnly:        coretools.BoolPtr(false),
			ConcurrencySafe: coretools.BoolPtr(false),
			Destructive:     coretools.BoolPtr(false),
			TimeoutSeconds:  30,
		}},
		Runtime: runtime,
		From:    from,
	}
}

func (t *CompleteTeamTaskTool) ValidateInput(_ coretools.ExecutionContext, input any) (bool, string) {
	in := derefComplete(input)
	if strings.TrimSpace(in.FinalAnswer) == "" {
		return false, "final_answer must not be empty."
	}
	if strings.TrimSpace(in.QAStatus) != "" && !validQAStatus(in.QAStatus) {
		return false, "qa_status must be pass or not_applicable."
	}
	if strings.TrimSpace(in.ReviewerStatus) != "" && !validReviewerStatus(in.ReviewerStatus) {
		return false, "reviewer_status must be pass or accepted_with_notes."
	}
	return true, ""
}

func (t *CompleteTeamTaskTool) Execute(_ context.Context, _ coretools.ExecutionContext, input any) (string, error) {
	if t.Runtime == nil {
		return "<tool_use_error>\nTeam runtime is not available.\n</tool_use_error>", nil
	}
	return t.Runtime.CompleteTask(t.From, derefComplete(input)), nil
}

func derefA2A(input any) A2AMessageInput {
	switch v := input.(type) {
	case A2AMessageInput:
		return v
	case *A2AMessageInput:
		if v != nil {
			return *v
		}
	}
	return A2AMessageInput{}
}

func derefComplete(input any) CompleteTeamTaskInput {
	switch v := input.(type) {
	case CompleteTeamTaskInput:
		return v
	case *CompleteTeamTaskInput:
		if v != nil {
			return *v
		}
	}
	return CompleteTeamTaskInput{}
}

func derefContract(input any) AcceptanceContract {
	switch v := input.(type) {
	case AcceptanceContract:
		return v
	case *AcceptanceContract:
		if v != nil {
			return *v
		}
	}
	return AcceptanceContract{}
}

func derefGateVerdict(input any) GateVerdict {
	switch v := input.(type) {
	case GateVerdict:
		return v
	case *GateVerdict:
		if v != nil {
			return *v
		}
	}
	return GateVerdict{}
}

func validQAStatus(status string) bool {
	status = strings.TrimSpace(status)
	return status == "pass" || status == "not_applicable"
}

func validReviewerStatus(status string) bool {
	status = strings.TrimSpace(status)
	return status == "pass" || status == "accepted_with_notes"
}

func nonEmptyStrings(values []string) []string {
	var out []string
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}

func requiredChecks(checks []ContractCheck) []ContractCheck {
	var out []ContractCheck
	for _, check := range checks {
		if check.Required || strings.TrimSpace(check.Name) != "" || strings.TrimSpace(check.Command) != "" {
			out = append(out, check)
		}
	}
	return out
}

func missingArtifacts(required []string, existing []Artifact) []string {
	have := map[string]struct{}{}
	for _, artifact := range existing {
		have[artifact.Name] = struct{}{}
		have[artifact.ID] = struct{}{}
	}
	var missing []string
	for _, name := range required {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := have[name]; !ok {
			missing = append(missing, name)
		}
	}
	return missing
}

func formatArtifactRefs(refs []string) string {
	if len(refs) == 0 {
		return ""
	}
	return fmt.Sprintf("Artifact: %s", strings.Join(refs, ", "))
}
