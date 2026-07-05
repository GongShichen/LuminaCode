package cli

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"LuminaCode/skills"
)

type Completion struct {
	Text          string
	StartPosition int
	Display       string
	DisplayMeta   string
}

func CompleteInput(textBeforeCursor string, registry *skills.SkillRegistry, cwd string) []Completion {
	stripped := strings.TrimLeftFunc(textBeforeCursor, unicode.IsSpace)
	if strings.HasPrefix(stripped, "/") {
		if !SlashCompletionActive(textBeforeCursor) {
			return nil
		}
		return completeSlashCommand(stripped, registry, cwd)
	}
	return CompleteVisiblePaths(textBeforeCursor, cwd)
}

func SlashCompletionActive(textBeforeCursor string) bool {
	_, fragment, ok := SlashCompletionFragment(textBeforeCursor)
	return ok && strings.HasPrefix(fragment, "/")
}

func SlashCompletionFragment(textBeforeCursor string) (leading string, fragment string, ok bool) {
	start := 0
	for start < len(textBeforeCursor) {
		r, size := utf8.DecodeRuneInString(textBeforeCursor[start:])
		if !unicode.IsSpace(r) {
			break
		}
		start += size
	}
	if start >= len(textBeforeCursor) || textBeforeCursor[start] != '/' {
		return "", "", false
	}
	fragment = textBeforeCursor[start:]
	for _, r := range fragment {
		if unicode.IsSpace(r) {
			return textBeforeCursor[:start], fragment, false
		}
	}
	return textBeforeCursor[:start], fragment, true
}

func CompleteVisiblePaths(textBeforeCursor string, cwd string) []Completion {
	base := cwd
	if base == "" {
		if current, err := os.Getwd(); err == nil {
			base = current
		}
	}
	expanded := expandHome(textBeforeCursor)
	dirPart, prefix := splitPathFragment(expanded)
	searchDir := dirPart
	if searchDir == "" {
		searchDir = base
	} else if !filepath.IsAbs(searchDir) {
		searchDir = filepath.Join(base, searchDir)
	}

	entries, err := os.ReadDir(searchDir)
	if err != nil {
		return nil
	}
	completions := make([]Completion, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if name == ".git" || !strings.HasPrefix(name, prefix) {
			continue
		}
		display := name
		if entry.IsDir() {
			display += string(filepath.Separator)
		}
		completions = append(completions, Completion{
			Text:          strings.TrimPrefix(name, prefix),
			StartPosition: 0,
			Display:       display,
			DisplayMeta:   "",
		})
	}
	sort.Slice(completions, func(i, j int) bool {
		return completions[i].Display < completions[j].Display
	})
	return completions
}

func completeSlashCommand(stripped string, registry *skills.SkillRegistry, cwd string) []Completion {
	seen := map[string]struct{}{}
	items := IterCommandCompletionItems(registry, cwd)
	completions := make([]Completion, 0, len(items))
	for _, item := range items {
		if _, ok := seen[item.Name]; ok || !strings.HasPrefix(item.Name, stripped) {
			continue
		}
		seen[item.Name] = struct{}{}
		meta := item.Description
		if commandMeta, ok := CommandMeta[item.Name]; ok {
			meta = commandMeta
		}
		completions = append(completions, Completion{
			Text:          item.Name,
			StartPosition: -len(stripped),
			Display:       item.Name,
			DisplayMeta:   meta,
		})
	}
	return completions
}

func splitPathFragment(fragment string) (string, string) {
	if fragment == "" {
		return "", ""
	}
	cleaned := filepath.Clean(fragment)
	if strings.HasSuffix(fragment, string(filepath.Separator)) {
		return cleaned, ""
	}
	dir := filepath.Dir(fragment)
	if dir == "." {
		dir = ""
	}
	return dir, filepath.Base(fragment)
}

func expandHome(path string) string {
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return path
	}
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
