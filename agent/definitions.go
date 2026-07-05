package agent

import coretools "LuminaCode/tools"

type AgentDef struct {
	Name           string
	Description    string
	ToolsAllowlist map[string]struct{}
	ToolsDenylist  map[string]struct{}
	Model          string
	IsReadOnly     bool
	MaxTurns       int
	PermissionMode string
	Isolation      string
}

var AgentDefinitions = map[string]AgentDef{
	"general-purpose": {
		Name:           "general-purpose",
		Description:    "General-purpose agent for researching complex questions, searching for code, and executing multi-step tasks.",
		MaxTurns:       50,
		PermissionMode: "inherit",
	},
	"Explore": {
		Name:           "Explore",
		Description:    "Fast read-only search agent for locating code. Use it to find files by pattern, grep for symbols or keywords, or answer 'where is X defined / which files reference Y'.",
		ToolsAllowlist: stringSet("read_file", "grep_search", "glob_match"),
		IsReadOnly:     true,
		MaxTurns:       30,
		PermissionMode: "bypass",
	},
	"Plan": {
		Name:           "Plan",
		Description:    "Software architect agent for designing implementation plans. Use this when you need to plan the implementation strategy for a task. Returns step-by-step plans and identifies critical files.",
		ToolsAllowlist: stringSet("read_file", "grep_search", "glob_match"),
		IsReadOnly:     true,
		MaxTurns:       40,
		PermissionMode: "bypass",
	},
	"Coordinator": {
		Name:           "Coordinator",
		Description:    "Coordinator agent for orchestrating background workers. Always use run_in_background=true when spawning workers. Never use sync mode. Use TaskWait to wait for workers. Never busy-poll with repeated TaskList calls. Use TaskList and TaskGet for inspection and result retrieval only. After all target workers settle, synthesize their results into one final answer.",
		ToolsAllowlist: stringSet("Agent", "TaskList", "TaskGet", "TaskWait", "TaskStop", "SendMessage"),
		MaxTurns:       100,
		PermissionMode: "bypass",
		Isolation:      "worktree",
	},
	"docs-lookup": {
		Name:           "docs-lookup",
		Description:    "Use this agent when the user asks questions about documentation, reference material, hooks, slash commands, MCP servers, settings, IDE integrations, or SDK usage.",
		ToolsAllowlist: stringSet("read_file", "grep_search", "glob_match"),
		IsReadOnly:     true,
		MaxTurns:       20,
		PermissionMode: "inherit",
	},
}

func GetAgentDefinition(subagentType string) AgentDef {
	if def, ok := AgentDefinitions[subagentType]; ok {
		return def
	}
	return AgentDefinitions["general-purpose"]
}

func BuildFilteredRegistry(base *coretools.ToolRegistry, definition AgentDef) *coretools.ToolRegistry {
	if base == nil {
		return coretools.NewToolRegistry()
	}
	return base.FilteredCopy(definition.ToolsAllowlist, definition.ToolsDenylist, definition.IsReadOnly, false)
}

func stringSet(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}
