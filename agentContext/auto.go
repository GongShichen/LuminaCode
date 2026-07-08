package agentContext

import (
	"LuminaCode/api"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"reflect"
	"regexp"
	"strings"

	"github.com/mohae/deepcopy"
)

const (
	MaxConsecutiveFailure        = 3
	MaxPtlRetries                = 3
	PostCompactMaxFilesToRestore = 5
	PostCompactMaxTokenPerFile   = 5000
)

var ptlErrorKeywords = map[string]struct{}{
	"too_long":            {},
	"token limit":         {},
	"context length":      {},
	"prompt is too long":  {},
	"maximum context":     {},
	"exceeds the maximum": {},
	"max_tokens":          {},
	"input length":        {},
	"reduce the length":   {},
}

type LLMClientFunc func(context.Context, []map[string]any, string) (string, error)

type AutoCompactResult struct {
	Summary        string `json:"summary"`
	OriginalTokens int    `json:"original_tokens"`
	SummaryTokens  int    `json:"summary_tokens"`
	Success        bool   `json:"success"`
	Error          string `json:"error"`
}

func GetString(m map[string]any, key string, defaultStr any) any {
	if m == nil {
		return defaultStr
	}
	v, ok := m[key].(string)
	if !ok {
		return defaultStr
	}
	return v
}

func buildSummaryPrompt(messages []map[string]any) string {
	parts := []string{"Summarize this coding session:\n\n"}
	for _, message := range messages {
		role := GetString(message, "role", "unknown")
		content := message["content"]
		switch c := content.(type) {
		case []any:
			texts := []string{}
			tools := []string{}
			for _, rawItem := range c {
				item, ok := rawItem.(map[string]any)
				if !ok {
					continue
				}
				itemType, ok := item["type"]
				if !ok {
					continue
				}
				if itemType == "text" {
					text, ok := GetString(item, "text", "").(string)
					if !ok {
						continue
					}
					if strings.TrimSpace(text) != "" {
						texts = append(texts, TruncateRunes(text, 300))
					}
				}
				if itemType == "tool_use" {
					tool, ok := GetString(item, "name", "unknown").(string)
					if !ok {
						continue
					}
					tools = append(tools, tool)
				}
			}
			for _, text := range texts {
				parts = append(parts, fmt.Sprintf("[%s]: %s\n", role, text))
			}
			if len(tools) > 0 {
				parts = append(parts, fmt.Sprintf("[%s used tools: %s]\n", role, strings.Join(tools, ", ")))
			}
		case string:
			parts = append(parts, fmt.Sprintf("[%s]: %s\n", role, TruncateRunes(c, 300)))
		}
	}
	return strings.Join(parts, "")
}

func TruncateRunes(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen])
}

type AutoCompactOptions struct {
	APIKey           string
	APIBaseURL       string
	APIModel         string
	APIType          string
	MaxSummaryTokens int
}

var analysisBlockPattern = regexp.MustCompile(`(?s)<analysis>.*?</analysis>`)

func AutoCompact(ctx context.Context, messages []map[string]any, systemPrompt string, opt AutoCompactOptions) AutoCompactResult {
	maxSummaryTokens := opt.MaxSummaryTokens
	if maxSummaryTokens == 0 {
		maxSummaryTokens = 500
	}
	prompt := buildSummaryPrompt(messages)
	compactSystem := "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools. " +
		"Tool calls will be REJECTED and will waste your only turn.\n\n" +
		"You are a summarization agent. You must compress the conversation " +
		"using a two-stage process:\n" +
		"1. <analysis>: Write a chronological scratchpad of the user's " +
		"intent, methods tried, and errors.\n" +
		"2. <summary>: Provide the final summary. It MUST include these " +
		"sections:\n" +
		"   - Primary Request\n" +
		"   - Files and Code (include absolute paths)\n" +
		"   - Errors and Fixes\n" +
		"   - Current Work\n" +
		"   - Pending Tasks"
	llmClient, err := api.CreateLLMClient(opt.APIKey, opt.APIBaseURL, opt.APIModel, maxSummaryTokens, nil, nil, opt.APIType)
	if err != nil {
		slog.Warn("L4 AutoCompact failed", "error", err)
		return AutoCompactResult{
			Summary:        "",
			OriginalTokens: 0,
			SummaryTokens:  0,
			Success:        false,
			Error:          err.Error(),
		}
	}

	summaryMessages := []map[string]any{
		{
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "text",
					"text": prompt,
				},
			},
		},
	}
	var fullText strings.Builder
	for item := range llmClient.StreamChat(ctx, compactSystem, summaryMessages, []map[string]any{}, nil) {
		if item.Err != nil {
			slog.Warn("L4 AutoCompact failed", "error", item.Err)
			return AutoCompactResult{
				Summary:        "",
				OriginalTokens: 0,
				SummaryTokens:  0,
				Success:        false,
				Error:          item.Err.Error(),
			}
		}
		eventType, ok := item.Event["type"].(string)
		if !ok {
			slog.Warn("Unknown event type")
			continue
		}
		switch eventType {
		case "text_delta":
			text, _ := item.Event["text"].(string)
			fullText.WriteString(text)
		case "error":
			msg, _ := item.Event["message"].(string)
			if msg == "" {
				msg = "Unknown"
			}
			slog.Warn("L4 AutoCompact failed", "message", msg)
			return AutoCompactResult{
				Summary:        "",
				OriginalTokens: 0,
				SummaryTokens:  0,
				Success:        false,
				Error:          msg,
			}
		}
	}
	summary := strings.TrimSpace(fullText.String())
	summary = analysisBlockPattern.ReplaceAllString(summary, "")
	summary = strings.TrimSpace(summary)
	originalEst := 0
	for _, msg := range messages {
		originalEst += len([]rune(fmt.Sprintf("%v", msg))) / 4
	}
	return AutoCompactResult{
		Summary:        summary,
		OriginalTokens: originalEst,
		SummaryTokens:  len([]rune(summary)) / 4,
		Success:        summary != "",
	}
}

func InjectSummary(ctx context.Context, messages []map[string]any, summary *string, keepRecent int) []map[string]any {
	if summary == nil || *summary == "" || len(messages) <= keepRecent*2 {
		return messages
	}
	summaryMessage := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{
				"type": "text",
				"text": fmt.Sprintf("[Session summary — earlier conversation compressed]\n\n%s", *summary),
			},
		},
	}
	start := len(messages) - keepRecent*2
	if start < 0 {
		start = 0
	}
	finalMessages := []map[string]any{messages[0], summaryMessage}
	finalMessages = append(finalMessages, messages[start:]...)
	return finalMessages
}

func AutocompactTriggerThreshold(contextLimit int, threshold float64) int {
	if contextLimit <= 0 {
		return 0
	}
	if threshold <= 0 {
		threshold = 0.8
	}
	return int(math.Floor(float64(contextLimit) * threshold))
}

func ShouldAutoCompact(ctx context.Context, currentTokens, snipTokensFreed, contextLimit, consecutiveFailure int, l3Suppressed bool) bool {
	return ShouldAutoCompactAtThreshold(ctx, currentTokens, snipTokensFreed, AutocompactTriggerThreshold(contextLimit, 0.8), consecutiveFailure, l3Suppressed)
}

func ShouldAutoCompactAtThreshold(ctx context.Context, currentTokens, snipTokensFreed, triggerThreshold, consecutiveFailure int, l3Suppressed bool) bool {
	if consecutiveFailure >= MaxConsecutiveFailure {
		slog.Warn(fmt.Sprintf("AutoCompact circuit breaker triggered after %d consecutive failures; skipping compression", consecutiveFailure))
		return false
	}
	if l3Suppressed {
		return false
	}
	adjustedTokens := currentTokens - snipTokensFreed
	return adjustedTokens >= triggerThreshold
}

func isPtlError(e error) bool {
	msg := strings.ToLower(e.Error())
	for k, _ := range ptlErrorKeywords {
		if strings.Contains(msg, k) {
			return true
		}
	}
	return false
}

func TruncateHeadForPtlRetry(ctx context.Context, messages []map[string]any) []map[string]any {
	if len(messages) <= 3 {
		return messages
	}
	result, ok := deepcopy.Copy(messages).([]map[string]any)
	if !ok {
		return messages
	}
	firstUserIdx := -1
	for i, msg := range result {
		role, ok := msg["role"].(string)
		if !ok {
			continue
		}
		if role == "user" {
			firstUserIdx = i
			break
		}
	}
	if firstUserIdx == -1 {
		return result
	}
	cutIdx := -1
	for i := firstUserIdx + 1; i < len(result); i++ {
		role, _ := result[i]["role"].(string)
		if role == "user" {
			cutIdx = i
			break
		}
	}
	if cutIdx == -1 || cutIdx >= len(result) {
		return result
	}
	if cutIdx > 0 {
		res := []map[string]any{result[0]}
		res = append(res, result[cutIdx:]...)
		return res
	}
	return result[cutIdx:]
}

func ExecuteAutoCompactWithRetry(ctx context.Context, messages []map[string]any, systemPrompt string, llmClientFunc LLMClientFunc) (string, error) {
	currentMessages := deepcopy.Copy(messages).([]map[string]any)
	for attempt := 0; attempt <= MaxPtlRetries; attempt++ {
		summary, err := llmClientFunc(ctx, currentMessages, systemPrompt)
		if err == nil {
			return summary, nil
		}
		if !isPtlError(err) {
			return "", err
		}
		if attempt >= MaxPtlRetries {
			slog.Warn(fmt.Sprintf("PTL retry exhausted (%d attempts), giving up.", attempt+1))
			return "", errors.New("PTL retry exhausted")
		}
		slog.Warn(fmt.Sprintf("PTL error on auto compact attempt %d/%d: %s", attempt+1, MaxPtlRetries+1, err.Error()))
		currentMessages = TruncateHeadForPtlRetry(ctx, currentMessages)
	}
	return "", errors.New("unreachable")
}

func RunPostCompactCleanUp(ctx context.Context, state any, recentReadFiles []map[string]string) {
	if recentReadFiles == nil {
		return
	}
	messages := getStateMessages(state)
	start := len(messages) - 2
	if start < 0 {
		start = 0
	}

	for _, m := range messages[start:] {
		mj, err := json.Marshal(m)
		if err != nil {
			if strings.Contains(fmt.Sprintf("%v", m), "[System: Post-compact memory restoration]") {
				return
			}
			continue
		}
		if strings.Contains(string(mj), "[System: Post-compact memory restoration]") {
			return
		}
	}
	s := len(recentReadFiles) - PostCompactMaxFilesToRestore
	if s < 0 {
		s = 0
	}
	filesToRestore := recentReadFiles[s:]
	restoreTextParts := []string{"[System: Post-compact memory restoration]"}

	for _, f := range filesToRestore {
		path, ok := f["path"]
		if !ok {
			path = "unknown"
		}
		content, ok := f["content"]
		if !ok {
			content = ""
		}
		truncateContent := TruncateRunes(content, PostCompactMaxTokenPerFile*4)
		restoreTextParts = append(restoreTextParts, fmt.Sprintf("--- File: %s ---\n%s", path, truncateContent))
	}
	recoverMsg := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{
				"type": "text",
				"text": strings.Join(restoreTextParts, "\n\n"),
			},
		},
	}
	setStateMessages(state, append(messages, recoverMsg))
}

func getStateMessages(state any) []map[string]any {
	value := reflectValue(state)
	if !value.IsValid() {
		return nil
	}
	fieldValue := value.FieldByName("Messages")
	if !fieldValue.IsValid() || fieldValue.IsNil() {
		return nil
	}
	messages, _ := fieldValue.Interface().([]map[string]any)
	return messages
}

func setStateMessages(state any, messages []map[string]any) {
	value := reflectValue(state)
	if !value.IsValid() {
		return
	}
	fieldValue := value.FieldByName("Messages")
	if !fieldValue.IsValid() || !fieldValue.CanSet() {
		return
	}
	fieldValue.Set(reflect.ValueOf(messages))
}

func MinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
