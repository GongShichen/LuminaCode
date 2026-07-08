package memory

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const MaxRecalledMemories = 5

const SelectMemoriesSystemPrompt = `You are selecting memories that will be useful to a general-purpose local agent as it
processes a user's query. Return a JSON array of filenames for memories that
are clearly useful (up to 5).

- Be selective and discerning; only pick memories with high relevance.
- Prefer recent memories when the index or filename suggests recency.
- If recently-used tools are provided, do NOT select usage reference docs for
  those tools. DO still select warnings, gotchas, or known issues about them.
- Return ONLY the JSON array, nothing else.

Format: ["filename.md", ...]`

type MemoryRecall struct {
	Filename   string
	FilePath   string
	Content    string
	MemoryType MemoryType
	RecallID   string
	Score      float64
}

type CompletionClient interface {
	Complete(ctx context.Context, systemPrompt string, messages []map[string]any, maxTokens int) (string, error)
}

type ClientFactory func(context.Context) (CompletionClient, error)

func ManifestFromMemoryIndex(indexContent string) string {
	entries := ParseMemoryIndex(indexContent)
	if len(entries) == 0 {
		return "(no memories available)"
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, "- [indexed] "+entry.Filename+": "+entry.Description)
	}
	return strings.Join(lines, "\n")
}

func ParseSelectionResponse(text string) []string {
	if match := regexp.MustCompile("(?s)```(?:json)?\\s*(\\[.*?\\])\\s*```").FindStringSubmatch(text); match != nil {
		text = match[1]
	}
	match := regexp.MustCompile(`(?s)\[.*?\]`).FindString(text)
	if match == "" {
		return nil
	}
	var result []string
	if err := json.Unmarshal([]byte(match), &result); err != nil {
		return nil
	}
	return result
}

func SelectRelevantMemories(ctx context.Context, query, manifest string, clientFactory ClientFactory, recentTools []string, alreadySurfaced map[string]struct{}) []string {
	if alreadySurfaced == nil {
		alreadySurfaced = map[string]struct{}{}
	}
	userParts := []string{"Query: " + query, "", "Available memories:", manifest}
	if len(recentTools) > 0 {
		userParts = append(userParts, "", "Recently used tools: "+strings.Join(recentTools, ", "))
	}
	var surfaced []string
	for name := range alreadySurfaced {
		if strings.HasSuffix(name, ".md") {
			surfaced = append(surfaced, name)
		}
	}
	if len(surfaced) > 0 {
		sort.Strings(surfaced)
		userParts = append(userParts, "", "Already shown (do NOT select these): "+strings.Join(surfaced, ", "))
	}
	client, err := clientFactory(ctx)
	if err != nil || client == nil {
		return nil
	}
	text, err := client.Complete(ctx, SelectMemoriesSystemPrompt, []map[string]any{
		{"role": "user", "content": strings.Join(userParts, "\n")},
	}, 256)
	if err != nil {
		return nil
	}
	valid := validNamesFromManifest(manifest)
	selected := ParseSelectionResponse(text)
	var result []string
	for _, name := range selected {
		if _, ok := valid[name]; ok {
			result = append(result, name)
			if len(result) >= MaxRecalledMemories {
				break
			}
		}
	}
	return result
}

func RecallMemoriesForQuery(ctx context.Context, query, memoryDir string, clientFactory ClientFactory, recentTools []string, alreadySurfaced map[string]struct{}) []MemoryRecall {
	if alreadySurfaced == nil {
		alreadySurfaced = map[string]struct{}{}
	}
	indexContent := LoadMemoryIndex(memoryDir)
	indexedEntries := ParseMemoryIndex(indexContent)
	if len(indexedEntries) == 0 {
		return nil
	}
	indexedNames := map[string]struct{}{}
	for _, entry := range indexedEntries {
		indexedNames[entry.Filename] = struct{}{}
	}
	manifest := ManifestFromMemoryIndex(indexContent)
	selected := SelectRelevantMemories(ctx, query, manifest, clientFactory, recentTools, alreadySurfaced)
	if len(selected) == 0 {
		return nil
	}
	var results []MemoryRecall
	for _, filename := range selected {
		if _, surfaced := alreadySurfaced[filename]; surfaced {
			continue
		}
		if _, indexed := indexedNames[filename]; !indexed {
			continue
		}
		path := filepath.Join(memoryDir, filename)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		fullText := strings.ToValidUTF8(string(data), "\uFFFD")
		memoryType := MemoryTypeUser
		if parsed, err := ParseMemoryFile(path); err == nil && parsed != nil {
			memoryType = parsed.MemoryType()
		}
		results = append(results, MemoryRecall{
			Filename: filename, FilePath: path, Content: fullText,
			MemoryType: memoryType, RecallID: filename,
		})
		TouchMemoryAccess(path)
	}
	return results
}

func validNamesFromManifest(manifest string) map[string]struct{} {
	valid := map[string]struct{}{}
	re := regexp.MustCompile(`- \[[^]]*\]\s+(?P<filename>[^\s(:]+\.md)`)
	for _, line := range strings.Split(manifest, "\n") {
		if !strings.HasPrefix(line, "- [") {
			continue
		}
		if match := re.FindStringSubmatch(line); match != nil {
			valid[strings.TrimSpace(match[1])] = struct{}{}
		}
	}
	return valid
}
