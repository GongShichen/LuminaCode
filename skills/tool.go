package skills

import (
	"context"
	"fmt"

	coretools "LuminaCode/tools"

	"github.com/google/uuid"
)

type SkillToolInput struct {
	Skill string `json:"skill" jsonschema:"description=Canonical skill name to invoke."`
	Args  string `json:"args,omitempty" jsonschema:"description=Optional raw argument string for the skill."`
}

type SkillTool struct {
	coretools.BaseTool
	Registry *SkillRegistry
	Executor *SkillExecutor
}

func NewSkillTool(registry *SkillRegistry, executor *SkillExecutor) *SkillTool {
	return &SkillTool{
		BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
			Name:           "Skill",
			Description:    "Invoke a local skill by canonical name. Use this when a listed skill matches the task better than continuing unaided.",
			InputPrototype: SkillToolInput{},
			ReadOnly:       coretools.BoolPtr(false), ConcurrencySafe: coretools.BoolPtr(true), Destructive: coretools.BoolPtr(false),
		}},
		Registry: registry,
		Executor: executor,
	}
}

func (t *SkillTool) ValidateInput(execCtx coretools.ExecutionContext, input any) (bool, string) {
	in := derefSkillToolInput(input)
	if in.Skill == "" {
		return false, "skill must not be empty."
	}
	cwd := "."
	if raw, ok := execCtx["cwd"].(string); ok && raw != "" {
		cwd = raw
	}
	skill := t.Registry.FindVisible(in.Skill, cwd)
	if skill == nil {
		return false, "Skill not found or not visible from the current path: " + in.Skill
	}
	if skill.Frontmatter.DisableModelInvocation {
		return false, "Skill '" + skill.CanonicalName + "' cannot be invoked automatically by the model."
	}
	return true, ""
}

func (t *SkillTool) Execute(ctx context.Context, execCtx coretools.ExecutionContext, input any) (string, error) {
	in := derefSkillToolInput(input)
	cwd := "."
	if raw, ok := execCtx["cwd"].(string); ok && raw != "" {
		cwd = raw
	}
	skill := t.Registry.FindVisible(in.Skill, cwd)
	if skill == nil {
		return "Skill not found or not visible from the current path: " + in.Skill, nil
	}
	if skill.Frontmatter.DisableModelInvocation {
		return "Skill '" + skill.CanonicalName + "' cannot be invoked automatically by the model.", nil
	}
	approve := func(req SkillShellPermissionRequest) bool {
		requester, _ := execCtx["_request_skill_shell_permission"].(func(SkillShellPermissionRequest) bool)
		if requester == nil {
			return false
		}
		return requester(req)
	}
	sessionID, _ := execCtx["_session_id"].(string)
	if sessionID == "" {
		sessionID = uuid.NewString()[:12]
	}
	execution, err := t.Executor.Execute(ctx, *skill, in.Args, sessionID, approve, registryFromContext(execCtx), execCtx["parent_state"], execCtx)
	if err != nil {
		return "", err
	}
	prompt := ""
	if execution.Prompt != nil {
		prompt = *execution.Prompt
	}
	if persistence, _ := execCtx["_skill_persistence"].(*SkillPersistence); persistence != nil && prompt != "" {
		scope, _ := execCtx["_skill_agent_scope"].(string)
		if scope == "" {
			scope = "main"
		}
		turnCount, _ := execCtx["_turn_count"].(int)
		persistence.RecordInvocation(scope, skill.CanonicalName, skillPath(*skill), prompt, turnCount)
	}
	if execution.Mode == "inline" {
		pending, _ := execCtx["_pending_skill_messages"].([]map[string]any)
		pending = append(pending, t.Executor.BuildInlineSkillMessage(*skill, prompt, false))
		execCtx["_pending_skill_messages"] = pending
		return fmt.Sprintf("Skill '%s' injected into the conversation. Continue using the new skill context.", skill.CanonicalName), nil
	}
	if execution.ResultText != nil {
		return *execution.ResultText, nil
	}
	return fmt.Sprintf("Skill '%s' completed.", skill.CanonicalName), nil
}

func (t *SkillTool) NeedsPermission(any) bool { return false }

func derefSkillToolInput(input any) SkillToolInput {
	switch v := input.(type) {
	case SkillToolInput:
		return v
	case *SkillToolInput:
		if v != nil {
			return *v
		}
	}
	return SkillToolInput{}
}

func registryFromContext(execCtx coretools.ExecutionContext) *coretools.ToolRegistry {
	if registry, ok := execCtx["_registry"].(*coretools.ToolRegistry); ok {
		return registry
	}
	return nil
}

func skillPath(skill SkillSpec) string {
	if skill.SkillFile != "" {
		return skill.SkillFile
	}
	return skill.Directory
}
