package apppaths

import (
	"path/filepath"
	"strings"
)

var legacyDefaultSettings = map[string][]string{
	"session_dir":                   {"~/.lumina/sessions", "~/.Lumina/sessions"},
	"long_term_memory_store":        {"~/.lumina/memory/lumina-memory.sqlite", "~/.Lumina/memory/lumina-memory.sqlite"},
	"memory_embedding_model_dir":    {"~/.lumina/models/memory/multilingual-e5-small", "~/.Lumina/models/memory/multilingual-e5-small"},
	"skills_dir":                    {".Lumina/PROJECT_SKILLS"},
	"user_skills_dir":               {"~/.lumina/skills", "~/.Lumina/skills"},
	"bundled_skills_dir":            {".Lumina/SKILLS"},
	"team_dir":                      {"~/.lumina/TEAM", "~/.Lumina/TEAM"},
	"system_prompt_path":            {".Lumina/SYSTEM/system-prompt.md"},
	"memory_extraction_prompt_path": {".Lumina/SYSTEM/extraction_system.md"},
	"worktree_dir":                  {".Lumina/worktrees"},
}

func IsLegacyDefaultSetting(key, value string) bool {
	value = filepath.ToSlash(strings.TrimSpace(value))
	for _, legacy := range legacyDefaultSettings[key] {
		if value == filepath.ToSlash(legacy) {
			return true
		}
	}
	return false
}
