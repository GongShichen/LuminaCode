package skills

import (
	"path/filepath"
	"strings"
)

var SafeInlineShellExecutables = map[string]bool{
	"bash":           true,
	"cmd":            true,
	"cmd.exe":        true,
	"powershell":     true,
	"powershell.exe": true,
	"pwsh":           true,
	"pwsh.exe":       true,
	"sh":             true,
	"zsh":            true,
}

type SkillShellDecision struct {
	Allowed              bool
	RequiresApproval     bool
	Reason               string
	NormalizedExecutable string
}

func DecideInlineShellExecution(skill SkillSpec, command string) SkillShellDecision {
	if strings.TrimSpace(command) == "" {
		return SkillShellDecision{
			Allowed: false,
			Reason:  "Skill '" + skill.CanonicalName + "' has an empty inline shell command.",
		}
	}
	executable := NormalizeShellExecutable(skill.Frontmatter.Shell)
	if skill.Frontmatter.Shell != nil && !SafeInlineShellExecutables[executable] {
		return SkillShellDecision{
			Allowed:              false,
			Reason:               "Skill '" + skill.CanonicalName + "' shell executable '" + *skill.Frontmatter.Shell + "' is not allowed for inline shell commands.",
			NormalizedExecutable: executable,
		}
	}
	switch skill.Source {
	case SkillSourceBundled:
		decision := SkillShellDecision{Allowed: true, RequiresApproval: skill.Frontmatter.Shell != nil, NormalizedExecutable: executable}
		if decision.RequiresApproval {
			decision.Reason = "Bundled skill requests a custom shell executable."
		}
		return decision
	case SkillSourceUser, SkillSourceProject:
		return SkillShellDecision{
			Allowed:              true,
			RequiresApproval:     true,
			Reason:               strings.Title(string(skill.Source)) + " skills require approval for inline shell commands.",
			NormalizedExecutable: executable,
		}
	default:
		return SkillShellDecision{
			Allowed:              false,
			Reason:               "Skill source '" + string(skill.Source) + "' is not allowed to execute inline shell commands.",
			NormalizedExecutable: executable,
		}
	}
}

func NormalizeShellExecutable(executable *string) string {
	if executable == nil {
		return ""
	}
	normalized := strings.TrimSpace(strings.ToLower(filepath.Base(strings.ReplaceAll(*executable, "\\", "/"))))
	return normalized
}
