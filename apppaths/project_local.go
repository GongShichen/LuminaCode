package apppaths

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	ProjectLocalDirName      = ".Lumina"
	ProjectLocalDirNameLower = ".lumina"
	LegacyConfigDirName      = "CONFIG"
	LegacySystemDirName      = "SYSTEM"
	LegacySkillsDirName      = "SKILLS"
	LegacyTeamsDirName       = "TEAM"
	ProjectSkillsDirName     = "PROJECT_SKILLS"
	ProjectWorktreesDirName  = "worktrees"
	ProjectDefaultsFileName  = "defaults.json"
	ProjectMCPFileName       = "mcp.json"
	SystemPromptFileName     = "system-prompt.md"
)

func ProjectLocalRoot(projectRoot string) string {
	return filepath.Join(projectRoot, ProjectLocalDirName)
}

func ProjectDefaultsFile(projectRoot string) string {
	return filepath.Join(ProjectLocalRoot(projectRoot), LegacyConfigDirName, ProjectDefaultsFileName)
}

func ProjectMCPFile(projectRoot string) string {
	return filepath.Join(ProjectLocalRoot(projectRoot), LegacyConfigDirName, ProjectMCPFileName)
}

func ProjectSystemPrompt(projectRoot string) string {
	return filepath.Join(ProjectLocalRoot(projectRoot), LegacySystemDirName, SystemPromptFileName)
}

func ProjectBundledSkillsDir(projectRoot string) string {
	return filepath.Join(ProjectLocalRoot(projectRoot), LegacySkillsDirName)
}

func ProjectTeamsDir(projectRoot string) string {
	return filepath.Join(ProjectLocalRoot(projectRoot), LegacyTeamsDirName)
}

func ProjectSkillsDir(projectRoot string) string {
	return filepath.Join(ProjectLocalRoot(projectRoot), ProjectSkillsDirName)
}

func ProjectWorktreesDir(projectRoot string) string {
	return filepath.Join(ProjectLocalRoot(projectRoot), ProjectWorktreesDirName)
}

func LegacyDefaultRoot(home string) string {
	return filepath.Join(home, ".lumina")
}

func LegacyEndpointFile(root string) string {
	return filepath.Join(root, "run", "backend.json")
}

func DiscoverProjectRoot(start string, markers []string) (string, error) {
	current, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	if info, statErr := os.Stat(current); statErr == nil && !info.IsDir() {
		current = filepath.Dir(current)
	}
	markers = append(append([]string(nil), markers...), ProjectLocalDirName)
	for candidate := current; ; candidate = filepath.Dir(candidate) {
		for _, marker := range markers {
			marker = strings.TrimSpace(marker)
			if marker == "" {
				continue
			}
			if _, statErr := os.Lstat(filepath.Join(candidate, marker)); statErr == nil {
				return candidate, nil
			}
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			break
		}
	}
	return current, nil
}
