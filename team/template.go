package team

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func (l Loader) CreateTemplate(displayName string) (TeamTemplateResult, error) {
	displayName = strings.TrimSpace(displayName)
	if displayName == "" {
		return TeamTemplateResult{}, fmt.Errorf("team name is required")
	}
	name := teamTemplateDirName(displayName)
	if name == "" {
		return TeamTemplateResult{}, fmt.Errorf("team name must contain letters or numbers")
	}
	root := filepath.Join(l.TeamDir(), name)
	if _, err := os.Stat(root); err == nil {
		return TeamTemplateResult{}, fmt.Errorf("team %q already exists at %s", name, root)
	} else if !os.IsNotExist(err) {
		return TeamTemplateResult{}, err
	}
	created := []string{
		TeamConfigFile,
		TeamSystemFile,
		CompletionPolicyFile,
		filepath.Join("team-leader", AgentConfigFile),
		filepath.Join("team-leader", AgentSystemPromptFile),
		filepath.Join("team-leader", "skills"),
	}
	for _, dir := range []string{root, filepath.Join(root, "team-leader", "skills")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return TeamTemplateResult{}, err
		}
	}
	files := map[string]string{
		TeamConfigFile: teamTemplateYAML(name, displayName),
		TeamSystemFile: `# Team System

You are an Agent Team. Keep each agent's context isolated, coordinate through readable team dialogue, and continue until the user interrupts or the task is complete.

The Team Leader owns planning, delegation, artifact tracking, and the final answer. This starter team contains only the Team Leader; add more agent directories and list them in team.yaml when you need specialists.
`,
		CompletionPolicyFile: `# Completion Policy

The Team Leader may complete the task when the user request is satisfied, required artifacts are produced, and the final answer explains what changed or what was delivered.

No QA or Reviewer gate is required by default. Add gates.qa_agent or gates.reviewer_agent in team.yaml only after adding those agents.
`,
		filepath.Join("team-leader", AgentConfigFile): `name: team-leader
display_name: Team Leader
description: Coordinates this team and produces final answers.
communicates_with: all
model: inherit
tools: inherit
max_turns_per_task: 0
private_skills: true
`,
		filepath.Join("team-leader", AgentSystemPromptFile): `# Team Leader

You coordinate this custom team.

- Understand the user's request and decide whether specialist agents are needed.
- If this team only has Team Leader configured, complete the work directly.
- When more agents are added, delegate through A2A messages and keep member dialogue concise and useful.
- Track required artifacts and verify they exist before final completion.
- Use private skills from this agent's skills/ directory when the user or task calls for them.
`,
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			return TeamTemplateResult{}, err
		}
	}
	return TeamTemplateResult{
		TeamName:     name,
		DisplayName:  displayName,
		Path:         root,
		CreatedFiles: created,
		AgentCount:   1,
	}, nil
}

func teamTemplateDirName(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	re := regexp.MustCompile(`[^a-z0-9]+`)
	name = re.ReplaceAllString(name, "-")
	return strings.Trim(name, "-")
}

func teamTemplateYAML(name, displayName string) string {
	return fmt.Sprintf(`name: %s
display_name: %s
description: Custom Agent Team
entry_agent: team-leader
loop:
  max_iterations: 0
  max_parallel_agents: 1
  completion_policy: team_leader_only
  require_final_artifact: false
  stop_policy: user_interrupt_or_task_complete_only
gates:
  require_contract: false
  qa_agent: ""
  reviewer_agent: ""
  nonblocking_findings: allow_complete
  deferral_requires_reason: false
transcript:
  show_member_dialogue: true
  show_tool_details: false
  show_thinking: false
agents:
  - team-leader
`, name, quoteYAMLString(displayName))
}

func quoteYAMLString(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, `"`, `\"`)
	return `"` + value + `"`
}
