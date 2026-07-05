package cli

import (
	"strings"

	"LuminaCode/skills"
)

type SlashCommandSpec struct {
	Primary     string
	Description string
	Aliases     []string
}

func (s SlashCommandSpec) Names() []string {
	names := make([]string, 0, 1+len(s.Aliases))
	names = append(names, s.Primary)
	names = append(names, s.Aliases...)
	return names
}

func (s SlashCommandSpec) HelpLabel() string {
	names := s.Names()
	if len(names) == 0 {
		return ""
	}
	label := names[0]
	for _, name := range names[1:] {
		label += ", " + name
	}
	return label
}

var BuiltinCommandSpecs = []SlashCommandSpec{
	{Primary: "/help", Description: "Show this help"},
	{Primary: "/clear", Description: "Start a fresh session"},
	{Primary: "/save", Description: "Save session to disk", Aliases: []string{"/s"}},
	{Primary: "/cost", Description: "Show token usage and API cost breakdown", Aliases: []string{"/tokens"}},
	{Primary: "/compact", Description: "Manually compress conversation context", Aliases: []string{"/compress"}},
	{Primary: "/yolo", Description: "Toggle YOLO mode (skip all permission prompts)"},
	{Primary: "/skill", Description: "Show visible skills for current directory"},
	{Primary: "/mcp", Description: "Show registered MCP tools for current session"},
	{Primary: "/resume", Description: "Resume a previous session by ID"},
	{Primary: "/quit", Description: "Exit LUMINA", Aliases: []string{"/q", "/exit"}},
}

var BuiltinCommands = buildBuiltinCommands()
var CommandMeta = buildCommandMeta()

type CommandCompletionItem struct {
	Name        string
	Description string
}

type CommandHelpRow struct {
	Command     string
	Description string
}

type SlashDispatchKind string

const (
	SlashDispatchNone    SlashDispatchKind = "none"
	SlashDispatchBuiltin SlashDispatchKind = "builtin"
	SlashDispatchExit    SlashDispatchKind = "exit"
	SlashDispatchSkill   SlashDispatchKind = "skill"
	SlashDispatchUnknown SlashDispatchKind = "unknown"
)

type SlashDispatch struct {
	Kind    SlashDispatchKind
	Command string
	Name    string
}

func IterCommandCompletionItems(registry *skills.SkillRegistry, cwd string) []CommandCompletionItem {
	items := make([]CommandCompletionItem, 0, len(BuiltinCommands))
	for _, name := range BuiltinCommands {
		items = append(items, CommandCompletionItem{Name: name, Description: CommandMeta[name]})
	}
	if registry == nil || cwd == "" {
		return items
	}
	for _, skill := range userInvocableSkills(registry, cwd) {
		items = append(items, CommandCompletionItem{
			Name:        "/" + skill.CanonicalName,
			Description: skillHint(skill),
		})
	}
	return items
}

func IterCommandHelpRows(registry *skills.SkillRegistry, cwd string) []CommandHelpRow {
	rows := make([]CommandHelpRow, 0, len(BuiltinCommandSpecs))
	for _, spec := range BuiltinCommandSpecs {
		rows = append(rows, CommandHelpRow{Command: spec.HelpLabel(), Description: spec.Description})
	}
	if registry == nil || cwd == "" {
		return rows
	}
	for _, skill := range userInvocableSkills(registry, cwd) {
		rows = append(rows, CommandHelpRow{
			Command:     "/" + skill.CanonicalName,
			Description: skillHint(skill),
		})
	}
	return rows
}

func IsExitCommand(input string) bool {
	switch input {
	case "/q", "/quit", "/exit":
		return true
	default:
		return false
	}
}

func ClassifyREPLSlashCommand(input string, registry *skills.SkillRegistry, cwd string) SlashDispatch {
	if !strings.HasPrefix(input, "/") {
		return SlashDispatch{Kind: SlashDispatchNone}
	}
	cmd := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(input, "/")))
	cmdName, _, _ := strings.Cut(cmd, " ")
	if cmd == "q" || cmd == "quit" || cmd == "exit" {
		return SlashDispatch{Kind: SlashDispatchExit, Command: cmd, Name: cmdName}
	}
	switch cmd {
	case "help", "clear", "save", "s", "cost", "tokens", "compact", "compress", "skill", "mcp":
		return SlashDispatch{Kind: SlashDispatchBuiltin, Command: cmd, Name: cmdName}
	}
	if cmdName == "yolo" || cmdName == "resume" {
		return SlashDispatch{Kind: SlashDispatchBuiltin, Command: cmd, Name: cmdName}
	}
	if registry != nil {
		if skill := registry.FindVisible(cmdName, cwd); skill != nil && skill.Frontmatter.UserInvocable {
			return SlashDispatch{Kind: SlashDispatchSkill, Command: cmd, Name: cmdName}
		}
	}
	return SlashDispatch{Kind: SlashDispatchUnknown, Command: cmd, Name: cmdName}
}

func buildBuiltinCommands() []string {
	commands := make([]string, 0)
	for _, spec := range BuiltinCommandSpecs {
		commands = append(commands, spec.Names()...)
	}
	return commands
}

func buildCommandMeta() map[string]string {
	meta := map[string]string{}
	for _, spec := range BuiltinCommandSpecs {
		meta[spec.Primary] = spec.Description
		for _, alias := range spec.Aliases {
			meta[alias] = "Alias for " + spec.Primary
		}
	}
	return meta
}

func userInvocableSkills(registry *skills.SkillRegistry, cwd string) []skills.SkillSpec {
	return registry.ListUserInvocable(cwd)
}

func skillHint(skill skills.SkillSpec) string {
	if skill.Frontmatter.ArgumentHint != nil && *skill.Frontmatter.ArgumentHint != "" {
		return *skill.Frontmatter.ArgumentHint
	}
	return skill.Frontmatter.Description
}
