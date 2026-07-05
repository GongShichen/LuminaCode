package memory

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
)

const (
	IndexFilename      = "MEMORY.md"
	lineBudget         = 150
	MaxEntrypointLines = 200
	MaxEntrypointBytes = 25_000
)

type EntrypointTruncation struct {
	Content          string
	LineCount        int
	ByteCount        int
	WasLineTruncated bool
	WasByteTruncated bool
}

type MemoryIndexEntry struct {
	Title       string
	Filename    string
	Description string
}

var typeOrder = map[MemoryType]int{
	MemoryTypeUser:      0,
	MemoryTypeProject:   1,
	MemoryTypeFeedback:  2,
	MemoryTypeReference: 3,
}

func GenerateMemoryIndex(memoryDir string) string {
	store := NewMemoryStore(memoryDir)
	entries := store.ListEntries()
	if len(entries) == 0 {
		return ""
	}
	sort.Slice(entries, func(i, j int) bool {
		oi := typeOrder[entries[i].MemoryType()]
		oj := typeOrder[entries[j].MemoryType()]
		if oi == oj {
			return entries[i].Filename() < entries[j].Filename()
		}
		return oi < oj
	})
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		lines = append(lines, makeIndexLine(entry))
	}
	return TruncateEntrypointContent(strings.Join(lines, "\n") + "\n").Content
}

func TruncateEntrypointContent(raw string) EntrypointTruncation {
	trimmed := strings.TrimRightFunc(raw, unicode.IsSpace)
	lines := strings.Split(trimmed, "\n")
	if trimmed == "" {
		lines = []string{""}
	}
	lineCount := len(lines)
	byteCount := len([]byte(trimmed))
	wasLineTruncated := lineCount > MaxEntrypointLines
	wasByteTruncated := false
	truncated := trimmed
	var reasons []string
	if wasLineTruncated {
		truncated = strings.Join(lines[:MaxEntrypointLines], "\n")
		reasons = append(reasons, "too many entries")
	}
	data := []byte(truncated)
	if len(data) > MaxEntrypointBytes {
		chunk := data[:MaxEntrypointBytes]
		if idx := bytes.LastIndexByte(chunk, '\n'); idx > 0 {
			truncated = string(chunk[:idx])
		} else {
			for i := len(chunk); i > 0; i-- {
				if utf8Valid(chunk[:i]) {
					truncated = string(chunk[:i])
					break
				}
			}
		}
		wasByteTruncated = true
		reasons = append(reasons, "too large")
	}
	if wasLineTruncated || wasByteTruncated {
		truncated += "\n\n> WARNING: MEMORY.md is " + strings.Join(reasons, " and ") + ". Only part of it was loaded."
	}
	return EntrypointTruncation{
		Content: truncated, LineCount: lineCount, ByteCount: byteCount,
		WasLineTruncated: wasLineTruncated, WasByteTruncated: wasByteTruncated,
	}
}

func UpdateIndexEntry(memoryDir string, entry MemoryEntry) string {
	indexPath := filepath.Join(memoryDir, IndexFilename)
	newLine := makeIndexLine(entry)
	target := entry.Filename()
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return GenerateMemoryIndex(memoryDir)
	}
	lines := splitLinesLikePython(string(data))
	for _, line := range lines {
		if strings.HasPrefix(line, "> WARNING: MEMORY.md") {
			return GenerateMemoryIndex(memoryDir)
		}
	}
	updated := false
	var newLines []string
	for _, line := range lines {
		parsed := ParseMemoryIndex(line)
		if len(parsed) > 0 && parsed[0].Filename == target {
			newLines = append(newLines, newLine)
			updated = true
		} else {
			newLines = append(newLines, line)
		}
	}
	if !updated {
		newLines = append(newLines, newLine)
	}
	return TruncateEntrypointContent(strings.Join(newLines, "\n") + "\n").Content
}

func RemoveIndexEntry(memoryDir, filename string) string {
	indexPath := filepath.Join(memoryDir, IndexFilename)
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return GenerateMemoryIndex(memoryDir)
	}
	var newLines []string
	for _, line := range splitLinesLikePython(string(data)) {
		parsed := ParseMemoryIndex(line)
		if len(parsed) > 0 && parsed[0].Filename == filename {
			continue
		}
		newLines = append(newLines, line)
	}
	if len(newLines) == 0 {
		return ""
	}
	return TruncateEntrypointContent(strings.Join(newLines, "\n") + "\n").Content
}

func WriteMemoryIndex(memoryDir string) (string, error) {
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		return "", err
	}
	content := GenerateMemoryIndex(memoryDir)
	indexPath := filepath.Join(memoryDir, IndexFilename)
	return indexPath, atomicWriteText(indexPath, content)
}

func LoadMemoryIndex(memoryDir string) string {
	data, err := os.ReadFile(filepath.Join(memoryDir, IndexFilename))
	if err != nil {
		return ""
	}
	return TruncateEntrypointContent(string(data)).Content
}

func ParseMemoryIndex(content string) []MemoryIndexEntry {
	var entries []MemoryIndexEntry
	seen := map[string]bool{}
	lineRE := regexp.MustCompile(`^\s*[-*]\s+\[(?P<title>[^\]]+)\]\((?P<file>[^)]+\.md)\)\s*(?P<tail>.*)$`)
	for _, line := range strings.Split(content, "\n") {
		match := lineRE.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		title := strings.TrimSpace(match[1])
		filename := filepath.Base(match[2])
		if filename == IndexFilename || seen[filename] {
			continue
		}
		description := regexp.MustCompile(`^(?:[-:]\s*)+`).ReplaceAllString(strings.TrimSpace(match[3]), "")
		entries = append(entries, MemoryIndexEntry{Title: title, Filename: filename, Description: description})
		seen[filename] = true
	}
	return entries
}

func makeIndexLine(entry MemoryEntry) string {
	title := titleCaseSlug(entry.Name)
	prefix := fmt.Sprintf("- [%s](%s) - ", title, entry.Filename())
	available := lineBudget - len([]rune(prefix))
	if available < 10 {
		available = 10
	}
	hook := entry.Description
	hookRunes := []rune(hook)
	if len(hookRunes) > available {
		hook = string(hookRunes[:max(available-3, 1)]) + "..."
	}
	return prefix + hook
}

func titleCaseSlug(slug string) string {
	parts := strings.Split(strings.ReplaceAll(slug, "_", "-"), "-")
	for i, part := range parts {
		if part == "" {
			continue
		}
		runes := []rune(strings.ToLower(part))
		runes[0] = unicode.ToUpper(runes[0])
		parts[i] = string(runes)
	}
	return strings.Join(parts, " ")
}

func splitLinesLikePython(text string) []string {
	if text == "" {
		return []string{}
	}
	if strings.HasSuffix(text, "\n") {
		text = strings.TrimSuffix(text, "\n")
	}
	if strings.HasSuffix(text, "\r") {
		text = strings.TrimSuffix(text, "\r")
	}
	return strings.Split(text, "\n")
}

func atomicWriteText(path, content string) error {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(parent, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		if err := os.Rename(tmpName, path); err == nil {
			return nil
		} else {
			lastErr = err
			time.Sleep(time.Duration(20*(attempt+1)) * time.Millisecond)
		}
	}
	return lastErr
}

func utf8Valid(data []byte) bool {
	return strings.ToValidUTF8(string(data), "") == string(data)
}
