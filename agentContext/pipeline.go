package agentContext

import (
	"LuminaCode/api"
	"LuminaCode/config"
	"context"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	"github.com/mohae/deepcopy"
)

type CompressionStats struct {
	LevelReached         int               `json:"level_reached"`
	TokensBefore         int               `json:"tokens_before"`
	TokensAfter          int               `json:"tokens_after"`
	SnipTokensFreed      int               `json:"snip_tokens_freed"`
	MicroTokensFreed     int               `json:"micro_tokens_freed"`
	CollapsedTokensFreed int               `json:"collapsed_tokens_freed"`
	AutoTokensFreed      int               `json:"auto_tokens_freed"`
	SnipRemoved          int               `json:"snip_removed"`
	MicroTruncated       int               `json:"micro_truncated"`
	MicroCleared         int               `json:"micro_cleared"`
	CollapseCount        int               `json:"collapse_count"`
	AutoTriggered        bool              `json:"auto_triggered"`
	CollapsedRegions     []CollapsedRegion `json:"collapsed_regions"`
}

type CompactionReplacementHistory struct {
	UserRequests []string         `json:"user_requests"`
	Summary      string           `json:"summary"`
	Recent       []map[string]any `json:"recent"`
}

func DefaultCompressionStats() *CompressionStats {
	return &CompressionStats{
		LevelReached:         0,
		TokensBefore:         0,
		TokensAfter:          0,
		SnipTokensFreed:      0,
		MicroTokensFreed:     0,
		CollapsedTokensFreed: 0,
		AutoTokensFreed:      0,
		SnipRemoved:          0,
		MicroTruncated:       0,
		MicroCleared:         0,
		CollapseCount:        0,
		AutoTriggered:        false,
		CollapsedRegions:     []CollapsedRegion{},
	}
}

type ContextPipeline struct {
	Config                         config.Config
	AutoCompactor                  any
	ConsecutiveAutoCompactFailures int
}

func DefaultContextPipeline() *ContextPipeline {
	return &ContextPipeline{
		Config:                         config.GetConfig(),
		ConsecutiveAutoCompactFailures: 0,
	}
}

func (p *ContextPipeline) Compress(
	messages []map[string]any,
	currentTokens int,
	systemPrompt string,
	contextLimit int,
	threshold float64,
	state any,
	existingL3Region []CollapsedRegion,
	allowAutoCompact bool) ([]map[string]any, CompressionStats) {
	if threshold <= 0.0 {
		threshold = p.Config.ContextCompressThreshold
	}
	softLimit := int(float64(contextLimit) * threshold)
	stats := *DefaultCompressionStats()
	current := deepcopy.Copy(messages).([]map[string]any)
	if currentTokens > 0 {
		stats.TokensBefore = currentTokens
	} else {
		stats.TokensBefore = TokenCountWithEstimation(messages)
	}
	if stats.TokensBefore <= softLimit {
		stats.TokensAfter = stats.TokensBefore
		return current, stats
	}

	// Snip
	slog.Debug(fmt.Sprintf("L1 snip: %d tokens -> %d limit", stats.TokensBefore, softLimit))
	beforeChars := totalResultChars(messages)
	current = SnipMessages(messages)
	afterChars := totalResultChars(current)
	stats.SnipRemoved = MaxInt(0, beforeChars-afterChars)
	stats.LevelReached = 1

	postL1Tokens := TokenCountWithEstimation(current)
	stats.SnipTokensFreed = stats.TokensBefore - postL1Tokens
	stats.TokensAfter = postL1Tokens
	if stats.TokensAfter <= softLimit {
		return current, stats
	}

	// Micro compact
	slog.Debug(fmt.Sprintf("L2 micro: %d tokens -> %d limit", stats.TokensBefore, softLimit))
	beforeCleared := CountClearedToolResult(current)
	postL1Tokens = stats.TokensAfter
	current, _ = MicroCompactMessages(current, true, 1)
	afterCleared := CountClearedToolResult(current)
	stats.MicroCleared = afterCleared - beforeCleared
	stats.MicroTruncated = stats.MicroCleared
	stats.LevelReached = 2

	postL2Tokens := TokenCountWithEstimation(current)
	stats.MicroTokensFreed = postL1Tokens - postL2Tokens
	stats.TokensAfter = postL2Tokens
	if stats.TokensAfter <= softLimit {
		return current, stats
	}

	// Collapse
	slog.Debug(fmt.Sprintf("L3 collapse: %d tokens still over limit", stats.TokensBefore))
	l3Threshold := getL3CollapseThreshold(contextLimit, softLimit)
	var existingRegions []CollapsedRegion
	if existingL3Region != nil {
		existingRegions = existingL3Region
	} else {
		existingRegions = []CollapsedRegion{}
	}
	if existingRegions != nil && len(existingRegions) > 0 {
		projectedExisting := ProjectCollapsedView(current, existingRegions)
		projectedExistingTokens := TokenCountWithEstimation(projectedExisting)
		if projectedExistingTokens <= softLimit {
			stats.LevelReached = 3
			stats.CollapsedRegions = existingRegions
			stats.CollapseCount = MaxInt(0, len(current)-len(projectedExisting))
			stats.CollapsedTokensFreed = postL2Tokens - projectedExistingTokens
			stats.TokensAfter = projectedExistingTokens
			return current, stats
		}
	}
	currentTokens = stats.TokensAfter
	if existingRegions != nil && len(existingRegions) > 0 {
		currentTokens = TokenCountWithEstimation(ProjectCollapsedView(current, existingRegions))
	}
	suppressAuto, regions := ApplyCollapseIfNeed(current, currentTokens, l3Threshold, existingRegions)
	stats.CollapsedRegions = regions
	projectedCurrent := ProjectCollapsedView(current, regions)
	stats.CollapseCount = MaxInt(0, len(current)-len(projectedCurrent))
	if suppressAuto {
		stats.LevelReached = 3
		stats.CollapsedTokensFreed = postL2Tokens - TokenCountWithEstimation(projectedCurrent)
		stats.TokensAfter = TokenCountWithEstimation(projectedCurrent)
		return current, stats
	}
	if !allowAutoCompact {
		projectedTokens := TokenCountWithEstimation(projectedCurrent)
		stats.AutoTokensFreed = 0
		stats.CollapsedTokensFreed = postL2Tokens - projectedTokens
		stats.TokensAfter = projectedTokens
		return current, stats
	}
	postL3Tokens := TokenCountWithEstimation(projectedCurrent)
	stats.CollapsedTokensFreed = postL2Tokens - postL3Tokens
	stats.TokensAfter = postL3Tokens
	if stats.TokensAfter <= softLimit {
		return current, stats
	}

	// Auto compact
	failureCount := p.ConsecutiveAutoCompactFailures
	if stateFailures, ok := getIntFieldOK(state, "ConsecutiveAutoCompactFailures"); ok {
		failureCount = stateFailures
	}
	l1CharTokensFreed := stats.SnipRemoved / 4
	if !ShouldAutoCompactAtThreshold(context.Background(), stats.TokensAfter, l1CharTokensFreed, softLimit, failureCount, false) {
		slog.Debug(fmt.Sprintf("L4 suppressed: tokens after = %d, snip freed = %d"+
			"consecutive failures=%d",
			stats.TokensAfter, l1CharTokensFreed, failureCount))
		stats.AutoTokensFreed = 0
		stats.TokensAfter = postL3Tokens
		return current, stats
	}
	slog.Debug("L4 auto: nuclear option triggered")
	if remaining := getIntPointerField(state, "TaskBudgetRemaining"); remaining != nil {
		finalTokensBeforeNuke := postL3Tokens
		*remaining -= finalTokensBeforeNuke
		slog.Debug(fmt.Sprintf("L4 budget carryover: deducted %d tokens, %d remaining",
			finalTokensBeforeNuke, *remaining))
	}
	stats.AutoTriggered = true
	stats.LevelReached = 4
	summary, err := p.autoCompact(context.Background(), projectedCurrent)
	if err != nil {
		slog.Warn(fmt.Sprintf("L4 auto: failed to auto compact %s", err.Error()))
		nextFailures := getIntField(state, "ConsecutiveAutoCompactFailures", p.ConsecutiveAutoCompactFailures) + 1
		setIntField(state, "ConsecutiveAutoCompactFailures", nextFailures)
		p.ConsecutiveAutoCompactFailures = nextFailures
	} else {
		current = injectSummary(current, summary, 2)
		p.ConsecutiveAutoCompactFailures = 0
		setIntField(state, "ConsecutiveAutoCompactFailures", 0)
	}
	finalTokens := TokenCountWithEstimation(current)
	stats.AutoTokensFreed = postL3Tokens - finalTokens
	stats.TokensAfter = finalTokens
	return current, stats
}

func getIntField(state any, field string, fallback int) int {
	if value, ok := getIntFieldOK(state, field); ok {
		return value
	}
	return fallback
}

func getIntFieldOK(state any, field string) (int, bool) {
	value := reflectValue(state)
	if !value.IsValid() {
		return 0, false
	}
	fieldValue := value.FieldByName(field)
	if !fieldValue.IsValid() {
		return 0, false
	}
	switch fieldValue.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return int(fieldValue.Int()), true
	default:
		return 0, false
	}
}

func setIntField(state any, field string, value int) {
	target := reflectValue(state)
	if !target.IsValid() {
		return
	}
	fieldValue := target.FieldByName(field)
	if !fieldValue.IsValid() || !fieldValue.CanSet() {
		return
	}
	switch fieldValue.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		fieldValue.SetInt(int64(value))
	}
}

func getIntPointerField(state any, field string) *int {
	value := reflectValue(state)
	if !value.IsValid() {
		return nil
	}
	fieldValue := value.FieldByName(field)
	if !fieldValue.IsValid() || fieldValue.Kind() != reflect.Ptr || fieldValue.IsNil() {
		return nil
	}
	if fieldValue.Type().Elem().Kind() != reflect.Int {
		return nil
	}
	ptr, _ := fieldValue.Interface().(*int)
	return ptr
}

func reflectValue(state any) reflect.Value {
	if state == nil {
		return reflect.Value{}
	}
	value := reflect.ValueOf(state)
	if value.Kind() == reflect.Ptr {
		if value.IsNil() {
			return reflect.Value{}
		}
		value = value.Elem()
	}
	if value.Kind() != reflect.Struct {
		return reflect.Value{}
	}
	return value
}

func EstimateTokens(messages []map[string]any) int {
	return TokenCountWithEstimation(messages)
}

func (p *ContextPipeline) autoCompact(
	ctx context.Context,
	messages []map[string]any,
) (string, error) {
	client, err := api.CreateLLMClient(
		p.Config.APIKey,
		p.Config.APIBaseURL,
		p.Config.APIModel,
		500,
		nil,
		api.DefaultRetryConfigPtr(),
		p.Config.APIType,
	)
	if err != nil {
		slog.Warn("L4 auto compact failed", "error", err)
		return "", err
	}

	summaryPrompt := buildAutoCompactPrompt(messages)

	compactSystem := "You are creating a handoff summary for a general-purpose local agent. " +
		"Preserve the user's goals, constraints, decisions, artifacts touched, tool findings, " +
		"errors, and the current next step. Do not invent facts. Keep it concise."

	msgs := []map[string]any{
		{
			"role": "user",
			"content": []any{
				map[string]any{
					"type": "text",
					"text": summaryPrompt,
				},
			},
		},
	}

	var fullText strings.Builder

	ch := client.StreamChat(ctx, compactSystem, msgs, []map[string]any{}, nil)
	for item := range ch {
		if item.Err != nil {
			slog.Warn("L4 auto compact API error", "error", item.Err)
			break
		}

		eventType, _ := item.Event["type"].(string)

		switch eventType {
		case "text_delta":
			text, _ := item.Event["text"].(string)
			fullText.WriteString(text)

		case "error":
			message, _ := item.Event["message"].(string)
			slog.Warn("L4 auto compact API error", "message", message)
			break
		}
	}

	return strings.TrimSpace(fullText.String()), nil
}

func buildAutoCompactPrompt(messages []map[string]any) string {
	parts := []string{
		"Create a handoff summary for the next model invocation. The summary will replace old conversation history, while the system prompt and project instructions will be rebuilt separately before the next request.\n\n",
		"Include:\n",
		"- Current user goal and explicit constraints\n",
		"- Important implementation decisions already made\n",
		"- Files, commands, and tool results that matter\n",
		"- Known failures or blockers\n",
		"- Concrete next step\n\n",
		"Conversation excerpts:\n\n",
	}
	for _, msg := range messages {
		role := GetString(msg, "role", "system")
		content := msg["content"]
		switch c := content.(type) {
		case []map[string]any:
			var texts []string
			var tools []string
			for _, block := range c {
				blockType := GetString(block, "type", "")
				if blockType == "text" {
					text := GetString(block, "text", "").(string)
					if len(text) > 0 {
						texts = append(texts, text)
					}
				}
				if blockType == "tool_use" {
					name := GetString(block, "name", "").(string)
					if name != "" {
						tools = append(tools, name)
					}
				}
			}
			for _, text := range texts {
				parts = append(parts, fmt.Sprintf("[%s]: %s\n", role, TruncateRunes(text, 500)))
			}
			if len(tools) > 0 {
				parts = append(parts, fmt.Sprintf("[%s called tools: %s]\n", role, strings.Join(tools, ", ")))
			}
		case []any:
			var texts []string
			var tools []string
			for _, rawBlock := range c {
				block, ok := rawBlock.(map[string]any)
				if !ok {
					continue
				}
				blockType := GetString(block, "type", "")
				if blockType == "text" {
					text := GetString(block, "text", "").(string)
					if len(text) > 0 {
						texts = append(texts, text)
					}
				}
				if blockType == "tool_use" {
					name := GetString(block, "name", "").(string)
					if name != "" {
						tools = append(tools, name)
					}
				}
			}
			for _, text := range texts {
				parts = append(parts, fmt.Sprintf("[%s]: %s\n", role, TruncateRunes(text, 500)))
			}
			if len(tools) > 0 {
				parts = append(parts, fmt.Sprintf("[%s called tools: %s]\n", role, strings.Join(tools, ", ")))
			}
		case string:
			parts = append(parts, fmt.Sprintf("[%s]: %s\n", role, TruncateRunes(c, 500)))
		}
	}

	return strings.Join(parts, "")
}

func totalResultChars(messages []map[string]any) int {
	total := 0
	for _, m := range messages {
		content, ok := contentBlocks(m["content"])
		if !ok {
			continue
		}
		for _, item := range content {
			itemType, ok := item["type"].(string)
			if !ok || itemType != "tool_result" {
				continue
			}
			itemContent, ok := item["content"].(string)
			if !ok {
				itemContent = ""
			}
			total += len([]rune(itemContent))
		}
	}
	return total
}

func countToolResults(messages []map[string]any) int {
	count := 0
	for _, m := range messages {
		content, ok := contentBlocks(m["content"])
		if !ok {
			continue
		}
		for _, item := range content {
			itemType, ok := item["type"].(string)
			if ok && itemType == "tool_result" {
				count++
			}
		}
	}
	return count
}

func injectSummary(messages []map[string]any, summary string, keepRecent int) []map[string]any {
	if summary == "" || len(messages) <= keepRecent*2 {
		return messages
	}
	replacement := BuildCompactionReplacementHistory(messages, summary, keepRecent)
	result := []map[string]any{
		buildCompactionHandoffMessage(replacement),
	}
	result = append(result, replacement.Recent...)
	return result
}

func BuildCompactionReplacementHistory(messages []map[string]any, summary string, keepRecent int) CompactionReplacementHistory {
	if keepRecent < 0 {
		keepRecent = 0
	}
	start := len(messages) - keepRecent*2
	if start < 0 {
		start = 0
	}
	return CompactionReplacementHistory{
		UserRequests: collectRealUserRequests(messages[:start], 12),
		Summary:      strings.TrimSpace(summary),
		Recent:       append([]map[string]any(nil), messages[start:]...),
	}
}

func buildCompactionHandoffMessage(replacement CompactionReplacementHistory) map[string]any {
	var parts []string
	if len(replacement.UserRequests) > 0 {
		parts = append(parts, "[Previous user requests]")
		for _, request := range replacement.UserRequests {
			parts = append(parts, "- "+request)
		}
		parts = append(parts, "")
	}
	parts = append(parts, "[Compaction handoff summary]")
	parts = append(parts, replacement.Summary)
	return map[string]any{
		"role": "user",
		"content": []map[string]any{
			{
				"type": "text",
				"text": strings.TrimSpace(strings.Join(parts, "\n")),
			},
		},
	}
}

func collectRealUserRequests(messages []map[string]any, limit int) []string {
	var requests []string
	for _, message := range messages {
		if GetString(message, "role", "") != "user" || isTransientContextMessage(message) || hasToolResultBlock(message) {
			continue
		}
		text := strings.TrimSpace(textFromMessage(message))
		if text == "" {
			continue
		}
		requests = append(requests, TruncateRunes(oneLine(text), 500))
	}
	if limit > 0 && len(requests) > limit {
		requests = requests[len(requests)-limit:]
	}
	return requests
}

func isTransientContextMessage(message map[string]any) bool {
	if isMeta, _ := message["isMeta"].(bool); isMeta {
		return true
	}
	metadata, _ := message["metadata"].(map[string]any)
	if metadata["lumina_memory_context"] == true {
		return true
	}
	switch source, _ := metadata["source"].(string); source {
	case "skill_inline", "skill_listing", "skill_recovery", "memory_index", "memory_recall", "task_notification":
		return true
	default:
		return false
	}
}

func hasToolResultBlock(message map[string]any) bool {
	blocks, ok := contentBlocks(message["content"])
	if !ok {
		return false
	}
	for _, block := range blocks {
		if GetString(block, "type", "") == "tool_result" {
			return true
		}
	}
	return false
}

func textFromMessage(message map[string]any) string {
	content := message["content"]
	switch c := content.(type) {
	case string:
		return c
	default:
		blocks, ok := contentBlocks(content)
		if !ok {
			return ""
		}
		var texts []string
		for _, block := range blocks {
			if GetString(block, "type", "") != "text" {
				continue
			}
			if text, ok := GetString(block, "text", "").(string); ok && text != "" {
				texts = append(texts, text)
			}
		}
		return strings.Join(texts, "\n")
	}
}

func oneLine(text string) string {
	return strings.Join(strings.Fields(text), " ")
}
