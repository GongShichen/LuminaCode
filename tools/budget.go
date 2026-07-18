package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

const AbsoluteToolResultChars = 400_000

func ApplyToolResultBudget(rawOutput, toolUseID, sessionDir string, maxChars int) (string, error) {
	return ApplyToolResultBudgetWithPreview(rawOutput, toolUseID, sessionDir, maxChars, 2000)
}

func ApplyToolResultBudgetWithPreview(rawOutput, toolUseID, sessionDir string, maxChars, previewChars int) (string, error) {
	return applyToolResultBudgetInDir(rawOutput, toolUseID, filepath.Join(sessionDir, "tool-results"), maxChars, previewChars)
}

func ApplyToolResultBudgetInDir(rawOutput, toolUseID, resultsDir string, maxChars int) (string, error) {
	return applyToolResultBudgetInDir(rawOutput, toolUseID, resultsDir, maxChars, 2000)
}

func applyToolResultBudgetInDir(rawOutput, toolUseID, resultsDir string, maxChars, previewChars int) (string, error) {
	if charLen(rawOutput) <= maxChars {
		return rawOutput, nil
	}

	filePath := filepath.Join(resultsDir, toolUseID+".txt")
	sizeLabel := formatSize(len([]byte(rawOutput)))

	preview := firstChars(rawOutput, previewChars)
	if charLen(rawOutput) > previewChars {
		preview += "\n..."
	}

	if err := os.MkdirAll(resultsDir, 0o700); err != nil {
		truncated := hardTruncate(rawOutput, maxChars)
		return truncated + fmt.Sprintf("\n\n... [Warning: Failed to persist full output to disk due to error: %s]", err), nil
	}
	if err := os.WriteFile(filePath, []byte(rawOutput), 0o600); err != nil {
		truncated := hardTruncate(rawOutput, maxChars)
		return truncated + fmt.Sprintf("\n\n... [Warning: Failed to persist full output to disk due to error: %s]", err), nil
	}

	previewCount := previewChars
	if total := charLen(rawOutput); total < previewCount {
		previewCount = total
	}
	return fmt.Sprintf(
		"<persisted-output>\nOutput too large (%s). Full output saved to: %s\n\nPreview (first %d characters):\n%s\n</persisted-output>",
		sizeLabel,
		filePath,
		previewCount,
		preview,
	), nil
}

func ClampToAbsoluteMax(content string, absoluteMax int) string {
	if charLen(content) <= absoluteMax {
		return content
	}

	half := absoluteMax / 2
	head := firstChars(content, half)
	tail := lastChars(content, half)
	if absoluteMax == 0 {
		tail = content
	}
	removed := charLen(content) - absoluteMax
	return fmt.Sprintf(
		"%s\n\n[OUTPUT TRUNCATED: %s, %d characters removed — exceeds absolute limit of %s]\n\n%s",
		head,
		formatSize(len([]byte(content))),
		removed,
		formatSize(absoluteMax),
		tail,
	)
}

func ApplyAggregateResultBudget(results []map[string]any, maxChars int) []map[string]any {
	if maxChars <= 0 || len(results) == 0 {
		return results
	}
	total := totalResultContentChars(results)
	if total <= maxChars {
		return results
	}

	type indexedResult struct {
		index int
		size  int
	}
	indexed := make([]indexedResult, 0, len(results))
	for i, result := range results {
		indexed = append(indexed, indexedResult{index: i, size: charLen(stringFromMap(result, "content"))})
	}
	sort.SliceStable(indexed, func(i, j int) bool {
		return indexed[i].size > indexed[j].size
	})

	output := make([]map[string]any, 0, len(results))
	for _, result := range results {
		cp := map[string]any{}
		for key, value := range result {
			cp[key] = value
		}
		output = append(output, cp)
	}

	for _, item := range indexed {
		if total <= maxChars {
			break
		}
		content := stringFromMap(output[item.index], "content")
		if content == "" {
			continue
		}
		budgetForThis := max(0, maxChars-(total-charLen(content)))
		output[item.index]["content"] = truncateAggregateContent(content, budgetForThis, len(results))
		total = totalResultContentChars(output)
	}

	return output
}

func formatSize(sizeBytes int) string {
	if sizeBytes < 1024 {
		return fmt.Sprintf("%d B", sizeBytes)
	}
	if sizeBytes < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(sizeBytes)/1024)
	}
	if sizeBytes < 1024*1024*1024 {
		return fmt.Sprintf("%.1f MB", float64(sizeBytes)/(1024*1024))
	}
	return fmt.Sprintf("%.2f GB", float64(sizeBytes)/(1024*1024*1024))
}

func hardTruncate(content string, maxChars int) string {
	if charLen(content) <= maxChars {
		return content
	}
	half := maxChars / 2
	head := firstChars(content, half)
	tail := lastChars(content, half)
	if maxChars == 0 {
		tail = content
	}
	removed := charLen(content) - maxChars
	return fmt.Sprintf("%s\n\n... [%d characters truncated] ...\n\n%s", head, removed, tail)
}

func truncateAggregateContent(content string, budget, resultCount int) string {
	if budget <= 0 {
		return ""
	}
	if charLen(content) <= budget {
		return content
	}
	notice := fmt.Sprintf(
		"\n\n... [Aggregate budget: %d characters truncated across %d concurrent results. If a <persisted-output> path appears above, use read_file to retrieve the full content.]",
		charLen(content)-budget,
		resultCount,
	)
	if charLen(notice) >= budget {
		return firstChars(content, budget)
	}
	return firstChars(content, budget-charLen(notice)) + notice
}

func totalResultContentChars(results []map[string]any) int {
	total := 0
	for _, result := range results {
		total += charLen(stringFromMap(result, "content"))
	}
	return total
}

func stringFromMap(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}

func charLen(s string) int {
	return len([]rune(s))
}

func firstChars(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}

func lastChars(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[len(runes)-n:])
}
