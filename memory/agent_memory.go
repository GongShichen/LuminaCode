package memory

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type AgentMemoryScope struct {
	Name string
	Path string
}

func SanitizeAgentTypeForPath(agentType string) string {
	cleaned := strings.ToLower(strings.TrimSpace(strings.ReplaceAll(agentType, ":", "-")))
	cleaned = regexp.MustCompile(`[^a-zA-Z0-9._-]+`).ReplaceAllString(cleaned, "-")
	cleaned = regexp.MustCompile(`-{2,}`).ReplaceAllString(cleaned, "-")
	cleaned = strings.Trim(cleaned, ".-_")
	if cleaned == "" {
		return "general-purpose"
	}
	return cleaned
}

func ResolveAgentMemoryProjectRoot(projectRoot string) string {
	resolved, err := filepath.Abs(projectRoot)
	if err != nil {
		resolved = projectRoot
	}
	if gitRoot := FindCanonicalGitRoot(resolved); gitRoot != "" {
		if abs, err := filepath.Abs(gitRoot); err == nil {
			return abs
		}
		return gitRoot
	}
	return resolved
}

func GetAgentMemoryDirectories(agentType, projectRoot string) []AgentMemoryScope {
	safeType := SanitizeAgentTypeForPath(agentType)
	root := ResolveAgentMemoryProjectRoot(projectRoot)
	home := memoryHomeDir()
	return []AgentMemoryScope{
		{Name: "user", Path: filepath.Join(home, ".Lumina", "agent-memory", safeType)},
		{Name: "project", Path: filepath.Join(root, ".Lumina", "agent-memory", safeType)},
		{Name: "local", Path: filepath.Join(root, ".Lumina", "agent-memory-local", safeType)},
	}
}

func RefreshAgentMemoryIndexes(agentType, projectRoot string) []AgentMemoryScope {
	var refreshed []AgentMemoryScope
	for _, scope := range GetAgentMemoryDirectories(agentType, projectRoot) {
		if _, err := os.Stat(scope.Path); err == nil {
			_, _ = WriteMemoryIndex(scope.Path)
			refreshed = append(refreshed, scope)
		}
	}
	return refreshed
}

func BuildAgentMemoryPrompt(agentType, projectRoot string) string {
	var scopeLines []string
	for _, scope := range GetAgentMemoryDirectories(agentType, projectRoot) {
		if _, err := os.Stat(scope.Path); err != nil {
			continue
		}
		ensureAgentMemoryIndex(scope.Path)
		if LoadMemoryIndex(scope.Path) == "" {
			continue
		}
		scopeLines = append(scopeLines, "- "+scope.Name+" scope: "+scope.Path)
	}
	if len(scopeLines) == 0 {
		return ""
	}
	return "\nAgent-type memory:\n" +
		"These memories are specific to the '" + agentType + "' sub-agent type. " +
		"They describe reusable operating knowledge for this kind of agent, not user preferences or project decisions.\n" +
		"MEMORY.md is the entrypoint index for each agent-memory scope. The current indexes are provided separately as hidden user context. " +
		"Relevant full agent memories may also be recalled automatically as hidden user context when they closely match the current task. Keep the indexes in sync when adding or updating agent memories.\n\n" +
		"Scopes:\n" + strings.Join(scopeLines, "\n") + "\n"
}

func BuildAgentMemoryContextMessages(agentType, projectRoot string) []map[string]any {
	var messages []map[string]any
	for _, scope := range GetAgentMemoryDirectories(agentType, projectRoot) {
		if _, err := os.Stat(scope.Path); err != nil {
			continue
		}
		ensureAgentMemoryIndex(scope.Path)
		indexContent := LoadMemoryIndex(scope.Path)
		if indexContent == "" {
			continue
		}
		indexPath := filepath.Join(scope.Path, IndexFilename)
		text := "<system-reminder>\n" +
			"Contents of " + indexPath + " (" + scope.Name + " agent-memory for '" + agentType + "', persists across conversations):\n\n" +
			indexContent + "\n</system-reminder>"
		messages = append(messages, BuildMetaUserMessage(text, "agent_memory_"+scope.Name))
	}
	return messages
}

func RecallAgentMemoriesForQuery(ctx context.Context, agentType, projectRoot, query string, clientFactory ClientFactory, recentTools []string, alreadySurfaced map[string]struct{}) []MemoryRecall {
	if alreadySurfaced == nil {
		alreadySurfaced = map[string]struct{}{}
	}
	var manifestLines []string
	fileLookup := map[string]struct {
		path     string
		filename string
	}{}
	surfacedWithScope := map[string]struct{}{}
	for item := range alreadySurfaced {
		if strings.HasSuffix(item, ".md") {
			surfacedWithScope[item] = struct{}{}
		}
	}
	for _, scope := range GetAgentMemoryDirectories(agentType, projectRoot) {
		if _, err := os.Stat(scope.Path); err != nil {
			continue
		}
		ensureAgentMemoryIndex(scope.Path)
		indexContent := LoadMemoryIndex(scope.Path)
		if indexContent == "" {
			continue
		}
		for _, entry := range parseAgentScopeIndex(scope.Name, indexContent) {
			scopedName := entry["scoped_name"]
			manifestLines = append(manifestLines, "- [indexed] "+scopedName+": ["+scope.Name+"] "+entry["description"])
			fileLookup[scopedName] = struct {
				path     string
				filename string
			}{path: filepath.Join(scope.Path, entry["filename"]), filename: entry["filename"]}
		}
	}
	if len(manifestLines) == 0 {
		return nil
	}
	selected := SelectRelevantMemories(ctx, query, strings.Join(manifestLines, "\n"), clientFactory, recentTools, surfacedWithScope)
	var results []MemoryRecall
	for _, scopedName := range selected {
		resolved, ok := fileLookup[scopedName]
		if !ok {
			continue
		}
		data, err := os.ReadFile(resolved.path)
		if err != nil {
			continue
		}
		fullText := strings.ToValidUTF8(string(data), "\uFFFD")
		memoryType := MemoryTypeUser
		if parsed, err := ParseMemoryFile(resolved.path); err == nil && parsed != nil {
			memoryType = parsed.MemoryType()
		}
		results = append(results, MemoryRecall{
			Filename: resolved.filename, FilePath: resolved.path, Content: fullText,
			MemoryType: memoryType, RecallID: scopedName,
		})
		TouchMemoryAccess(resolved.path)
	}
	return results
}

func BuildRecalledAgentMemoriesMessage(recalled []MemoryRecall, source string) map[string]any {
	if source == "" {
		source = "agent_memory_recall"
	}
	message := BuildRecalledMemoriesMessage(recalled)
	if message == nil {
		return nil
	}
	if metadata, ok := message["metadata"].(map[string]any); ok {
		metadata["source"] = source
	}
	return message
}

func ensureAgentMemoryIndex(scopePath string) {
	indexPath := filepath.Join(scopePath, IndexFilename)
	if _, err := os.Stat(indexPath); err != nil {
		_, _ = WriteMemoryIndex(scopePath)
	}
}

func parseAgentScopeIndex(scopeName, indexContent string) []map[string]string {
	var parsed []map[string]string
	for _, entry := range ParseMemoryIndex(indexContent) {
		scopedFilename := scopeName + "--" + entry.Filename
		parsed = append(parsed, map[string]string{
			"scope":       scopeName,
			"filename":    entry.Filename,
			"description": entry.Description,
			"scoped_name": scopedFilename,
		})
	}
	return parsed
}
