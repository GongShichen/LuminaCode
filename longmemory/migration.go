package longmemory

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type MigrationResult struct {
	Imported int      `json:"imported"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors,omitempty"`
}

func (s *Store) MigrateLegacyMarkdown(ctx context.Context) (MigrationResult, error) {
	var result MigrationResult
	var status string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM memory_schema WHERE key='legacy_markdown_import'`).Scan(&status); err == nil && strings.HasPrefix(status, "complete") {
		return result, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return result, err
	}
	patterns := []string{
		filepath.Join(home, ".Lumina", "projects", "*", "memory"),
		filepath.Join(home, ".Lumina", "agent-memory", "*"),
		filepath.Join(home, ".lumina", "project", "*", "agent-memory", "*"),
		filepath.Join(home, ".lumina", "project", "*", "agent-memory-local", "*"),
	}
	seen := map[string]struct{}{}
	for _, pattern := range patterns {
		directories, _ := filepath.Glob(pattern)
		for _, directory := range directories {
			walkErr := filepath.WalkDir(directory, func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					result.Errors = append(result.Errors, walkErr.Error())
					return nil
				}
				if entry.IsDir() || !strings.EqualFold(filepath.Ext(path), ".md") {
					return nil
				}
				absolute, _ := filepath.Abs(path)
				if _, ok := seen[absolute]; ok {
					return nil
				}
				seen[absolute] = struct{}{}
				candidate, parseErr := parseLegacyMemory(path, home)
				if parseErr != nil {
					result.Errors = append(result.Errors, path+": "+parseErr.Error())
					return nil
				}
				if strings.TrimSpace(candidate.Content) == "" {
					result.Skipped++
					return nil
				}
				if _, upsertErr := s.Upsert(ctx, candidate); upsertErr != nil {
					result.Errors = append(result.Errors, path+": "+upsertErr.Error())
					return nil
				}
				result.Imported++
				return nil
			})
			if walkErr != nil {
				result.Errors = append(result.Errors, walkErr.Error())
			}
		}
	}
	status = "complete"
	if len(result.Errors) > 0 {
		status = "complete_with_errors"
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO memory_schema(key, value, updated_at) VALUES ('legacy_markdown_import', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, status, formatTime(time.Now().UTC()))
	if logErr := appendMigrationLog(filepath.Join(filepath.Dir(s.Path()), "migration-log.jsonl"), result); err == nil {
		err = logErr
	}
	return result, err
}

func parseLegacyMemory(path, home string) (Candidate, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Candidate{}, err
	}
	metadata := map[string]any{}
	body := string(data)
	if strings.HasPrefix(body, "---\n") || strings.HasPrefix(body, "---\r\n") {
		scanner := bufio.NewScanner(strings.NewReader(body))
		var frontmatter strings.Builder
		lineNumber := 0
		endOffset := 0
		for scanner.Scan() {
			line := scanner.Text()
			lineNumber++
			endOffset += len(line) + 1
			if lineNumber == 1 {
				continue
			}
			if strings.TrimSpace(line) == "---" {
				body = body[minInt(endOffset, len(body)):]
				break
			}
			frontmatter.WriteString(line)
			frontmatter.WriteByte('\n')
		}
		if err := yaml.Unmarshal([]byte(frontmatter.String()), &metadata); err != nil {
			return Candidate{}, fmt.Errorf("parse frontmatter: %w", err)
		}
	}
	title := strings.TrimSpace(stringValue(metadata["title"]))
	if title == "" {
		for _, line := range strings.Split(body, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "# ") {
				title = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "# "))
				break
			}
		}
	}
	if title == "" {
		title = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	scopeType := ScopeType(strings.TrimSpace(stringValue(metadata["scope_type"])))
	scopeKey := strings.TrimSpace(stringValue(metadata["scope_key"]))
	if scopeType == "" || scopeKey == "" {
		scopeType, scopeKey = inferLegacyScope(path, home)
	}
	memoryType := MemoryType(strings.TrimSpace(stringValue(metadata["memory_type"])))
	if memoryType == "" {
		memoryType = inferLegacyMemoryType(path)
	}
	confidence := floatValue(metadata["confidence"], 0.5)
	importance := floatValue(metadata["importance"], 0.5)
	return Candidate{
		MemoryID: strings.TrimSpace(stringValue(metadata["memory_id"])), ScopeType: scopeType, ScopeKey: scopeKey,
		MemoryType: memoryType, Status: StatusActive, Title: title, Content: strings.TrimSpace(body),
		Summary: strings.TrimSpace(stringValue(metadata["summary"])), Tags: stringList(metadata["tags"]),
		Entities: stringList(metadata["entities"]), Confidence: confidence, Importance: importance,
		SourcePaths: []string{path}, ValidFrom: parseFlexibleTime(stringValue(metadata["valid_from"])),
		ValidUntil: parseFlexibleTime(stringValue(metadata["valid_until"])),
	}, nil
}

func inferLegacyScope(path, home string) (ScopeType, string) {
	normalized := filepath.ToSlash(path)
	parts := strings.Split(normalized, "/")
	for index, part := range parts {
		switch part {
		case "agent-memory", "agent-memory-local":
			if index+1 < len(parts) {
				return ScopeAgentType, sanitizeKey(parts[index+1])
			}
		case "projects", "project":
			if index+1 < len(parts) {
				return ScopeProject, sanitizeKey(parts[index+1])
			}
		}
	}
	return ScopeProject, ProjectScopeKey(home)
}

func inferLegacyMemoryType(path string) MemoryType {
	lower := strings.ToLower(filepath.ToSlash(path))
	for _, candidate := range []MemoryType{TypeFeedback, TypePreference, TypeProject, TypeReference, TypeProcedural, TypeEpisodic, TypeSemantic} {
		if strings.Contains(lower, "/"+string(candidate)+"/") {
			return candidate
		}
	}
	return TypeSemantic
}

func appendMigrationLog(path string, result MigrationResult) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	data, _ := json.Marshal(map[string]any{"created_at": time.Now().UTC(), "result": result})
	_, err = file.Write(append(data, '\n'))
	return err
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func floatValue(value any, fallback float64) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	default:
		return fallback
	}
}

func stringList(value any) []string {
	switch typed := value.(type) {
	case []string:
		return normalizeStrings(typed)
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			values = append(values, stringValue(item))
		}
		return normalizeStrings(values)
	case string:
		return normalizeStrings(strings.Split(typed, ","))
	default:
		return nil
	}
}

func parseFlexibleTime(value string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02"} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}
