package skills

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	coretools "LuminaCode/tools"

	"github.com/google/uuid"
)

const (
	SkillInlineMetaKey = "lumina_skill_context"
)

var EffortThinkingBudgets = map[string]int{"quick": 1024, "standard": 4096}

type SkillExecutionResult struct {
	Mode          string
	Prompt        *string
	ResultText    *string
	SubagentScope *string
	InputTokens   int
	OutputTokens  int
}

type SkillExecutor struct {
	Loader          *SkillLoader
	PromptProcessor *PromptProcessor
	ForkRunner      SkillForkRunner
}

type SkillForkRunner func(ctx context.Context, skill SkillSpec, prompt, subagentScope string, thinkingBudgetTokens *int, baseRegistry *coretools.ToolRegistry, parentState any, extraContext coretools.ExecutionContext) (string, int, int, error)

func NewSkillExecutor(loader *SkillLoader, promptProcessor *PromptProcessor) *SkillExecutor {
	return &SkillExecutor{Loader: loader, PromptProcessor: promptProcessor}
}

func (e *SkillExecutor) ExecuteInline(skill SkillSpec, args, sessionID string, approveProjectShell func(SkillShellPermissionRequest) bool) (SkillExecutionResult, error) {
	loaded := e.Loader.LoadFullContent(skill)
	if sessionID == "" {
		sessionID = uuid.NewString()[:12]
	}
	prompt, err := e.PromptProcessor.Process(loaded, args, sessionID, approveProjectShell)
	if err != nil {
		return SkillExecutionResult{}, err
	}
	return SkillExecutionResult{Mode: "inline", Prompt: &prompt}, nil
}

func (e *SkillExecutor) Execute(ctx context.Context, skill SkillSpec, args, sessionID string, approveProjectShell func(SkillShellPermissionRequest) bool, baseRegistry *coretools.ToolRegistry, parentState any, extraContext coretools.ExecutionContext) (SkillExecutionResult, error) {
	if skill.Frontmatter.Context == "fork" {
		return e.ExecuteFork(ctx, skill, args, sessionID, approveProjectShell, baseRegistry, parentState, extraContext)
	}
	return e.ExecuteInline(skill, args, sessionID, approveProjectShell)
}

func (e *SkillExecutor) ExecuteFork(ctx context.Context, skill SkillSpec, args, sessionID string, approveProjectShell func(SkillShellPermissionRequest) bool, baseRegistry *coretools.ToolRegistry, parentState any, extraContext coretools.ExecutionContext) (SkillExecutionResult, error) {
	loaded := e.Loader.LoadFullContent(skill)
	if sessionID == "" {
		sessionID = uuid.NewString()[:12]
	}
	prompt, err := e.PromptProcessor.Process(loaded, args, sessionID, approveProjectShell)
	if err != nil {
		return SkillExecutionResult{}, err
	}
	prompt = applyForkEffort(prompt, skill.Frontmatter.Effort)
	subagentScope := "subagent:" + uuid.NewString()[:12]
	if extraContext == nil {
		extraContext = coretools.ExecutionContext{}
	}
	extraContext["_skill_agent_scope"] = subagentScope
	if persistence, _ := extraContext["_skill_persistence"].(*SkillPersistence); persistence != nil {
		defer persistence.ClearForScope(subagentScope)
	}
	filtered := buildForkRegistry(baseRegistry, skill.Frontmatter.AllowedTools)
	if e.ForkRunner == nil {
		return SkillExecutionResult{}, fmt.Errorf("fork skill execution is not configured")
	}
	thinkingBudgetTokens := resolveForkThinkingBudget(skill.Frontmatter.Effort)
	resultText, inputTokens, outputTokens, err := e.ForkRunner(ctx, skill, prompt, subagentScope, thinkingBudgetTokens, filtered, parentState, extraContext)
	if err != nil {
		return SkillExecutionResult{}, err
	}
	return SkillExecutionResult{Mode: "fork", Prompt: &prompt, ResultText: &resultText, SubagentScope: &subagentScope, InputTokens: inputTokens, OutputTokens: outputTokens}, nil
}

func (e *SkillExecutor) BuildInlineSkillMessage(skill SkillSpec, prompt string, disableSkillTool bool) map[string]any {
	sourceLabel := "skill"
	switch skill.Source {
	case SkillSourceUser:
		sourceLabel = "user skill"
	case SkillSourceProject:
		sourceLabel = "project skill"
	case SkillSourceBundled:
		sourceLabel = "bundled skill"
	}
	sanitized := regexp.MustCompile(`(?i)</?system[-_]reminder[^>]*>`).ReplaceAllString(prompt, "")
	sanitized = strings.TrimSpace(sanitized)
	text := "<system-reminder>\n" +
		"Skill '" + skill.CanonicalName + "' (" + sourceLabel + ") is active for this turn.\n\n" +
		sanitized + "\n</system-reminder>"
	return map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": text}},
		"isMeta":  true,
		"metadata": map[string]any{
			SkillInlineMetaKey:             true,
			"source":                       SkillInlineSource,
			"skill_name":                   skill.CanonicalName,
			SkillInlineAllowedToolsKey:     skill.Frontmatter.AllowedTools,
			"lumina_skill_model":           optionalValue(skill.Frontmatter.Model),
			"lumina_skill_effort":          skill.Frontmatter.Effort,
			SkillInlineDisableSkillToolKey: disableSkillTool,
		},
	}
}

func buildForkRegistry(base *coretools.ToolRegistry, allowedTools []string) *coretools.ToolRegistry {
	if base == nil {
		return coretools.NewToolRegistry()
	}
	if allowedTools != nil {
		allow := map[string]struct{}{}
		for _, tool := range allowedTools {
			allow[tool] = struct{}{}
		}
		return base.FilteredCopy(allow, nil, false, false)
	}
	return base.FilteredCopy(nil, nil, true, false)
}

func applyForkEffort(prompt string, effort any) string {
	if effort == nil {
		return prompt
	}
	guidance := ""
	switch effort {
	case "quick":
		guidance = "Reasoning effort: quick. Prefer the shortest viable path, keep tool use minimal, and avoid broad exploration."
	case "standard":
		guidance = "Reasoning effort: standard. Use normal depth, verify key assumptions, and favor reliable completion over speed."
	default:
		guidance = fmt.Sprintf("Reasoning effort: %s. Use a deliberate number of steps proportional to this setting when it helps.", pythonEffortString(effort))
	}
	return "<system-reminder>\n" + guidance + "\n</system-reminder>\n\n" + prompt
}

func resolveForkThinkingBudget(effort any) *int {
	switch v := effort.(type) {
	case nil:
		return nil
	case int:
		if v <= 0 {
			return nil
		}
		return &v
	case int64:
		if v <= 0 {
			return nil
		}
		budget := int(v)
		return &budget
	case bool:
		if !v {
			return nil
		}
		budget := 1
		return &budget
	case string:
		if budget, ok := EffortThinkingBudgets[v]; ok {
			return &budget
		}
	}
	return nil
}

func pythonEffortString(effort any) string {
	if v, ok := effort.(bool); ok {
		if v {
			return "True"
		}
		return "False"
	}
	return fmt.Sprint(effort)
}
