package agentbench

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"LuminaCode/agent"
	luminaapi "LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/memory"
)

const longMemEvalAnswerPromptIdentity = "memory-qa-grounded-answer-contract-known-state-v60"

const longMemEvalAnswerContractRetries = 3

const longMemEvalAnswerMaxTokens = 4096

const longMemEvalAnswerThinkingBudgetTokens = 3072

const longMemEvalAnswerToolName = "SubmitGroundedAnswer"

const longMemEvalAnswerSystemPrompt = `You are a precise memory question-answering engine. Use only the supplied evidence.
Submit exactly these fields through the provided structured output tool: supports, answer, insufficient.
- supports lists every evidence ID directly used for the answer.
- answer is a concise final answer without evidence IDs, retrieval narration, or hidden reasoning.
- insufficient is true only when the supplied evidence cannot support a defensible response to the request.
Read every evidence section before answering and combine directly relevant facts across sections. A conclusion can be supported even when no single section states it verbatim; do not mark it insufficient merely because its supporting facts appear in separate records, are phrased indirectly, or require straightforward synthesis.
Treat the latest applicable observation at or before the reference time as the current known state unless later evidence changes or contradicts it. Do not require a second observation exactly at the reference time.
When the requested output is not itself a stored fact, produce a useful response grounded in demonstrated interests, preferences, experiences, constraints, and available resources. The evidence need not contain a ready-made response; supported personalization and ordinary practical synthesis are allowed. Do not mark such a response insufficient merely because live external details are unavailable.
Resolve aliases, time, state changes, and any requested calculation from the evidence as a whole. Distinguish observed or completed facts from plans, examples, recommendations, and unrelated quantities. An explicitly negated event did not occur and must be excluded. Before writing JSON, silently inventory every directly relevant observation, remove duplicate mentions, derive the final conclusion, and verify that no relevant evidence section was omitted. The answer field must contain that final conclusion rather than a restatement of intermediate observations. Never substitute a nearby entity or metric and never invent missing facts.`

type dedicatedLongMemEvalAnswerRunner struct{}

type longMemEvalQAContract struct {
	Supports     []string        `json:"supports"`
	Answer       json.RawMessage `json:"answer"`
	Insufficient bool            `json:"insufficient"`
}

type longMemEvalQADiagnostics struct {
	Search         memory.SearchResult   `json:"search"`
	EvidencePacket string                `json:"evidence_packet"`
	EvidenceIDs    []string              `json:"evidence_ids"`
	ResponseText   string                `json:"response_text,omitempty"`
	Contract       longMemEvalQAContract `json:"contract"`
	ParseMode      string                `json:"parse_mode,omitempty"`
	FinalAnswer    string                `json:"final_answer,omitempty"`
	APICalls       int                   `json:"api_calls"`
}

func (dedicatedLongMemEvalAnswerRunner) RunAnswer(ctx context.Context, cfg config.Config, question,
	sessionID string, queryTime time.Time) AgentRunResult {
	started := time.Now()
	if !cfg.UsesMemoryFabric() {
		return AgentRunResult{ErrorType: "answer_memory_backend_error: Fabric is required"}
	}
	cfg.MemoryRemoteProcessing = "off"
	cfg.MemoryContextTargetTokens = minPositive(cfg.MemoryContextTargetTokens, 2400)
	cfg.MemoryContextMaxTokens = minPositive(cfg.MemoryContextMaxTokens, 2800)

	state := agent.NewAgentState()
	state.MemorySessionID = sessionID
	state.MemoryAgentID = "longmemeval-memory-qa"
	state.MemoryAgentType = "main"
	state.MemoryQueryTime = longMemEvalRetrievalReferenceTime(queryTime)
	state.MemoryQueryTimeExplicit = !queryTime.IsZero()
	fabric, err := agent.OpenConfiguredMemoryFabric(ctx, cfg, false)
	if err != nil {
		return AgentRunResult{ErrorType: "answer_memory_open_error: " + err.Error()}
	}
	if fabric == nil {
		return AgentRunResult{ErrorType: "answer_memory_open_error: Fabric is disabled"}
	}
	searchResult, searchReference, searchErr := agent.SearchMemoryWithEngine(ctx, cfg, &state, question, fabric)
	if err := fabric.Close(); err != nil {
		return AgentRunResult{ErrorType: "answer_memory_close_error: " + err.Error()}
	}
	if searchErr != nil {
		return AgentRunResult{ErrorType: "answer_memory_search_error: " + searchErr.Error()}
	}
	evidence, evidenceIDs := compactLongMemEvalSearchEvidence(searchResult, searchReference, 9000)
	orderedEvidenceIDs := make([]string, 0, len(evidenceIDs))
	for id := range evidenceIDs {
		orderedEvidenceIDs = append(orderedEvidenceIDs, id)
	}
	sort.Strings(orderedEvidenceIDs)
	diagnostics := &longMemEvalQADiagnostics{Search: searchResult, EvidencePacket: evidence,
		EvidenceIDs: orderedEvidenceIDs}
	searchTimeline := newTimelineEvent(started, time.Now(), "memory_search", map[string]any{
		"route": searchResult.Route, "evidence": len(searchResult.Evidence),
		"insufficient": searchResult.Insufficient, "duration_ms": searchResult.Diagnostics.Duration.Milliseconds(),
	})

	retry := luminaapi.DefaultRetryConfigPtr()
	retry.BaseDelay = 10 * time.Second
	retry.MaxDelay = 60 * time.Second
	retry.BackoffFactor = 3
	retry.MaxRetries = 3
	retry.Jitter = false
	thinkingBudget := longMemEvalAnswerThinkingBudgetTokens
	client, err := agent.CreateConfiguredLLMClient(cfg, cfg.APIModel, longMemEvalAnswerMaxTokens, &thinkingBudget, retry)
	if err != nil {
		return AgentRunResult{ErrorType: "answer_client_error: " + err.Error(), Diagnostics: diagnostics}
	}
	userPrompt := fmt.Sprintf("Reference time: %s\nQuestion: %s\n\nEvidence packet:\n%s",
		formatLongMemEvalReferenceTime(queryTime), strings.TrimSpace(question), evidence)
	messages := []map[string]any{{"role": "user", "content": userPrompt}}

	response, err := completeLongMemEvalQA(ctx, client, longMemEvalAnswerSystemPrompt, messages)
	diagnostics.ResponseText = longMemEvalQAResponseText(response)
	diagnostics.APICalls = 1
	if err != nil {
		errType := longMemEvalAnswerAPIError(err)
		return AgentRunResult{ErrorType: errType, InputTokens: response.InputTokens, Diagnostics: diagnostics,
			CacheReadInputTokens:     response.CacheReadInputTokens,
			CacheCreationInputTokens: response.CacheCreationInputTokens, OutputTokens: response.OutputTokens}
	}
	answer, contract, parseMode := parseLongMemEvalQACompletion(response)
	apiCalls := 1
	for retryIndex := 0; retryIndex < longMemEvalAnswerContractRetries &&
		longMemEvalQAResponseNeedsRetry(answer, contract, parseMode, evidenceIDs, evidence); retryIndex++ {
		repairMessages := append([]map[string]any(nil), messages...)
		repairMessages = append(repairMessages,
			map[string]any{"role": "assistant", "content": longMemEvalQAResponseText(response)},
			map[string]any{"role": "user", "content": longMemEvalQARepairInstruction(parseMode, contract, evidenceIDs, evidence)})
		repaired, repairErr := completeLongMemEvalQA(ctx, client, longMemEvalAnswerSystemPrompt, repairMessages)
		apiCalls++
		diagnostics.APICalls = apiCalls
		mergeLongMemEvalResponseUsage(&response, repaired)
		if repairErr != nil {
			return AgentRunResult{ErrorType: longMemEvalAnswerAPIError(repairErr), InputTokens: response.InputTokens,
				Diagnostics:              diagnostics,
				CacheReadInputTokens:     response.CacheReadInputTokens,
				CacheCreationInputTokens: response.CacheCreationInputTokens, OutputTokens: response.OutputTokens,
				Timeline: []TimelineEvent{searchTimeline, newTimelineEvent(started, time.Now(), "memory_qa_response",
					map[string]any{"parse_mode": parseMode, "api_calls": apiCalls})}}
		}
		diagnostics.ResponseText = longMemEvalQAResponseText(repaired)
		answer, contract, parseMode = parseLongMemEvalQACompletion(repaired)
	}
	answer = finalizeLongMemEvalQAAnswer(answer, contract, evidenceIDs, evidence)
	diagnostics.Contract = contract
	diagnostics.ParseMode = parseMode
	diagnostics.FinalAnswer = answer
	if answer == "" {
		return AgentRunResult{ErrorType: "answer_contract_error", InputTokens: response.InputTokens,
			Diagnostics:              diagnostics,
			CacheReadInputTokens:     response.CacheReadInputTokens,
			CacheCreationInputTokens: response.CacheCreationInputTokens,
			OutputTokens:             response.OutputTokens, Timeline: []TimelineEvent{searchTimeline, newTimelineEvent(started, time.Now(),
				"memory_qa_response", map[string]any{"parse_mode": parseMode, "api_calls": apiCalls})}}
	}
	return AgentRunResult{FinalText: answer, InputTokens: response.InputTokens, Diagnostics: diagnostics,
		CacheReadInputTokens:     response.CacheReadInputTokens,
		CacheCreationInputTokens: response.CacheCreationInputTokens, OutputTokens: response.OutputTokens,
		Timeline: []TimelineEvent{searchTimeline, newTimelineEvent(started, time.Now(), "memory_qa_response", map[string]any{
			"parse_mode": parseMode, "supports": len(contract.Supports),
			"api_calls": apiCalls,
		})}}
}

func completeLongMemEvalQA(ctx context.Context, client luminaapi.LLMClient, systemPrompt string,
	messages []map[string]any) (luminaapi.Response, error) {
	if structured, ok := client.(luminaapi.StructuredCompletionClient); ok {
		temperature := 0.0
		return structured.CompleteStructured(ctx, systemPrompt, messages, luminaapi.StructuredCompletionOptions{
			MaxTokens: longMemEvalAnswerMaxTokens, Tools: []map[string]any{longMemEvalAnswerTool()},
			RequiredTool: longMemEvalAnswerToolName, Temperature: &temperature,
		})
	}
	temperature := 0.0
	text, err := client.Complete(ctx, systemPrompt, messages, luminaapi.CompleteOptions{
		MaxTokens: longMemEvalAnswerMaxTokens, Temperature: &temperature,
	})
	return luminaapi.Response{Text: text}, err
}

func longMemEvalAnswerTool() map[string]any {
	return map[string]any{
		"name":        longMemEvalAnswerToolName,
		"description": "Submit a concise answer grounded only in the supplied evidence.",
		"input_schema": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"supports", "answer", "insufficient"},
			"properties": map[string]any{
				"supports": map[string]any{
					"type":        "array",
					"description": "Every evidence ID directly used for the answer.",
					"items":       map[string]any{"type": "string"},
				},
				"answer": map[string]any{
					"type":        "string",
					"description": "The concise final answer without evidence IDs or reasoning.",
				},
				"insufficient": map[string]any{
					"type":        "boolean",
					"description": "True only when the evidence cannot support a defensible response.",
				},
			},
		},
	}
}

func parseLongMemEvalQACompletion(response luminaapi.Response) (string, longMemEvalQAContract, string) {
	for _, call := range response.ToolCalls {
		if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(call["name"])), longMemEvalAnswerToolName) {
			continue
		}
		input, ok := call["input"].(map[string]any)
		if !ok || input == nil {
			return "", longMemEvalQAContract{}, "tool_contract_invalid"
		}
		encoded, err := json.Marshal(input)
		if err != nil {
			return "", longMemEvalQAContract{}, "tool_contract_invalid"
		}
		answer, contract, mode := parseLongMemEvalQAResponse(string(encoded))
		if mode == "json_contract" || mode == "json_contract_empty" {
			return answer, contract, "tool_contract"
		}
		return "", longMemEvalQAContract{}, "tool_contract_invalid"
	}
	return parseLongMemEvalQAResponse(response.Text)
}

func longMemEvalQAResponseText(response luminaapi.Response) string {
	for _, call := range response.ToolCalls {
		if !strings.EqualFold(strings.TrimSpace(fmt.Sprint(call["name"])), longMemEvalAnswerToolName) {
			continue
		}
		if input, ok := call["input"].(map[string]any); ok && input != nil {
			if encoded, err := json.Marshal(input); err == nil {
				return string(encoded)
			}
		}
	}
	return response.Text
}

func longMemEvalAnswerAPIError(err error) string {
	if err == nil {
		return ""
	}
	if luminaapi.IsQuotaExhaustedError(err) || luminaapi.IsQuotaExhaustedMessage(err.Error()) {
		return "api_quota_exhausted: " + err.Error()
	}
	return "answer_api_error: " + err.Error()
}

func mergeLongMemEvalResponseUsage(total *luminaapi.Response, addition luminaapi.Response) {
	if total == nil {
		return
	}
	total.InputTokens += addition.InputTokens
	total.CacheReadInputTokens += addition.CacheReadInputTokens
	total.CacheCreationInputTokens += addition.CacheCreationInputTokens
	total.OutputTokens += addition.OutputTokens
}

func longMemEvalQAResponseNeedsRetry(answer string, contract longMemEvalQAContract, parseMode string,
	evidenceIDs map[string]struct{}, evidence string) bool {
	if (parseMode != "json_contract" && parseMode != "tool_contract") || strings.TrimSpace(answer) == "" {
		return true
	}
	if contract.Insufficient {
		return false
	}
	return !supportsAreAnchored(contract, evidenceIDs, evidence)
}

func longMemEvalQARepairInstruction(parseMode string, contract longMemEvalQAContract,
	evidenceIDs map[string]struct{}, evidence string) string {
	reason := "the response did not satisfy the required JSON contract"
	if parseMode == "json_contract_empty" {
		reason = "the JSON answer field was empty"
	} else if parseMode == "json_contract" && !contract.Insufficient && len(contract.Supports) == 0 {
		reason = "the non-insufficient answer had no grounded support IDs"
	} else if parseMode == "json_contract" && !contract.Insufficient {
		invalid := unanchoredLongMemEvalQASupports(contract, evidenceIDs, evidence)
		if len(invalid) > 0 {
			reason = "these support IDs were not present in the supplied evidence: " + strings.Join(invalid, ", ")
		}
	}
	return `Validation failed because ` + reason + `. Re-read the original question and evidence, copy every support ID exactly from an evidence header, then emit one complete valid JSON object only. Preserve the required fields supports, answer, insufficient. Do not include markdown, comments, or trailing text.`
}

func finalizeLongMemEvalQAAnswer(answer string, contract longMemEvalQAContract,
	evidenceIDs map[string]struct{}, evidence string) string {
	if contract.Insufficient {
		return "Insufficient evidence."
	}
	if !supportsAreAnchored(contract, evidenceIDs, evidence) {
		return ""
	}
	return strings.TrimSpace(answer)
}

func minPositive(value, maximum int) int {
	if value <= 0 || value > maximum {
		return maximum
	}
	return value
}

func formatLongMemEvalReferenceTime(value time.Time) string {
	if value.IsZero() {
		return "unknown"
	}
	return value.UTC().Format(time.RFC3339)
}

func compactLongMemEvalSearchEvidence(result memory.SearchResult, reference time.Time,
	maxRunes int) (string, map[string]struct{}) {
	type evidenceSection struct {
		groupHeader string
		header      string
		content     string
	}
	sections := make([]evidenceSection, 0, len(result.Evidence))
	orderedIDs := make([]string, 0, len(result.Evidence))
	visibleIDs := map[string]struct{}{}
	contextAliases := map[string]string{}
	previousContextAlias := ""
	for _, item := range result.Evidence {
		id := strings.TrimSpace(item.ResourceID)
		if id == "" {
			id = strings.TrimSpace(item.ID)
		}
		if id == "" || strings.TrimSpace(item.Content) == "" {
			continue
		}
		alias := fmt.Sprintf("e%02d", len(sections)+1)
		orderedIDs = append(orderedIDs, alias)
		visibleIDs[alias] = struct{}{}
		header := "[" + alias + "]"
		groupHeader := ""
		if item.ContextID != "" {
			contextAlias := contextAliases[item.ContextID]
			if contextAlias == "" {
				contextAlias = fmt.Sprintf("c%d", len(contextAliases)+1)
				contextAliases[item.ContextID] = contextAlias
			}
			if contextAlias != previousContextAlias {
				groupHeader = "Context " + contextAlias + ":"
			}
			previousContextAlias = contextAlias
		} else {
			previousContextAlias = ""
		}
		if item.Actor != "" {
			header += " actor=" + item.Actor
		}
		if !item.OccurredAt.IsZero() {
			header += " time=" + item.OccurredAt.UTC().Format(time.RFC3339)
		}
		if item.ResourceKind != "" && item.ResourceKind != "event" {
			header += " kind=" + item.ResourceKind
		}
		if item.Status != "" {
			header += " status=" + string(item.Status)
		}
		sections = append(sections, evidenceSection{
			groupHeader: groupHeader,
			header:      header,
			content:     strings.TrimSpace(item.Content),
		})
	}
	header := fmt.Sprintf("Evidence IDs: %s\nReference time: %s\nEvidence:\n",
		strings.Join(orderedIDs, ", "), reference.UTC().Format(time.RFC3339))
	footer := ""
	if result.Insufficient {
		footer += "\nMemory evidence is insufficient for the exact requested target. Do not substitute a nearby entity or guess.\n"
	}
	fixedRunes := len([]rune(header + footer))
	for _, section := range sections {
		fixedRunes += len([]rune(section.header)) + 2
		if section.groupHeader != "" {
			fixedRunes += len([]rune(section.groupHeader)) + 1
		}
	}
	contentBudget := maxRunes - fixedRunes
	if maxRunes <= 0 {
		contentBudget = int(^uint(0) >> 1)
	}
	if contentBudget < 0 {
		contentBudget = 0
	}
	contents := make([]string, len(sections))
	for index := range sections {
		contents[index] = sections[index].content
	}
	budgets := balancedEvidenceBudgets(contents, contentBudget)
	var output strings.Builder
	output.WriteString(header)
	for index, section := range sections {
		if section.groupHeader != "" {
			output.WriteString(section.groupHeader)
			output.WriteByte('\n')
		}
		output.WriteString(section.header)
		output.WriteByte('\n')
		output.WriteString(truncateLongMemEvalRunesBalanced(section.content, budgets[index]))
		output.WriteByte('\n')
	}
	output.WriteString(footer)
	return strings.TrimSpace(output.String()), visibleIDs
}

func balancedEvidenceBudgets(contents []string, total int) []int {
	budgets := make([]int, len(contents))
	if len(contents) == 0 || total <= 0 {
		return budgets
	}
	remaining := total
	active := make([]int, len(contents))
	for index := range contents {
		active[index] = index
	}
	for remaining > 0 && len(active) > 0 {
		share := remaining / len(active)
		if share == 0 {
			share = 1
		}
		next := active[:0]
		for _, index := range active {
			if remaining <= 0 {
				break
			}
			needed := len([]rune(contents[index])) - budgets[index]
			if needed <= 0 {
				continue
			}
			grant := minPositive(needed, minPositive(share, remaining))
			budgets[index] += grant
			remaining -= grant
			if budgets[index] < len([]rune(contents[index])) {
				next = append(next, index)
			}
		}
		if len(next) == len(active) && share == 0 {
			break
		}
		active = next
	}
	return budgets
}

func truncateLongMemEvalRunesBalanced(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len([]rune(value)) <= limit {
		return value
	}
	lines := strings.Split(value, "\n")
	if len(lines) == 1 {
		return truncateLongMemEvalRunesBothEnds(value, limit)
	}
	perLine := limit / len(lines)
	if perLine < 16 {
		perLine = 16
	}
	selected := make([]string, 0, len(lines))
	used := 0
	for _, line := range lines {
		remaining := limit - used
		if remaining <= 0 {
			break
		}
		part := truncateLongMemEvalRunesBothEnds(line, minPositive(perLine, remaining))
		if part == "" {
			continue
		}
		selected = append(selected, part)
		used += len([]rune(part)) + 1
	}
	return strings.Join(selected, "\n")
}

func truncateLongMemEvalRunesBothEnds(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if limit <= 0 {
		return ""
	}
	if len(runes) <= limit {
		return string(runes)
	}
	if limit <= 5 {
		return string(runes[:limit])
	}
	head := (limit - 3) / 2
	tail := limit - 3 - head
	return strings.TrimSpace(string(runes[:head])) + "..." + strings.TrimSpace(string(runes[len(runes)-tail:]))
}

func truncateLongMemEvalRunes(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if limit <= 0 {
		return ""
	}
	if len(runes) <= limit {
		return string(runes)
	}
	if limit <= 3 {
		return string(runes[:limit])
	}
	return strings.TrimSpace(string(runes[:limit-3])) + "..."
}

func parseLongMemEvalQAResponse(raw string) (string, longMemEvalQAContract, string) {
	raw = strings.TrimSpace(raw)
	var contract longMemEvalQAContract
	object := extractJSONObject(raw)
	if json.Unmarshal([]byte(object), &contract) == nil {
		if answer := rawJSONAnswer(contract.Answer); answer != "" {
			return answer, contract, "json_contract"
		}
		return "", contract, "json_contract_empty"
	}
	// json.Unmarshal may populate fields before reporting a later syntax or type
	// error. That partial object is not a valid contract and must never drive a
	// deterministic calculation.
	contract = longMemEvalQAContract{}
	if match := regexp.MustCompile(`(?is)"answer"\s*:\s*"((?:\\.|[^"\\])*)"`).FindStringSubmatch(raw); len(match) == 2 {
		var answer string
		if json.Unmarshal([]byte(`"`+match[1]+`"`), &answer) == nil {
			return strings.TrimSpace(answer), contract, "recovered_answer_field"
		}
	}
	trimmedFence := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "```json"), "```"))
	if strings.HasPrefix(trimmedFence, "{") || strings.HasPrefix(trimmedFence, "[") {
		return "", contract, "malformed_contract"
	}
	plain := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(raw, "```json"), "```"))
	plain = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(plain, "```"), "```"))
	return plain, contract, "plain_same_response"
}

func rawJSONAnswer(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	if decoder.Decode(&value) == nil {
		return strings.TrimSpace(stringifyAny(value))
	}
	return ""
}

func supportsAreAnchored(contract longMemEvalQAContract, evidenceIDs map[string]struct{}, evidence string) bool {
	return len(contract.Supports) > 0 && len(unanchoredLongMemEvalQASupports(contract, evidenceIDs, evidence)) == 0
}

func unanchoredLongMemEvalQASupports(contract longMemEvalQAContract,
	evidenceIDs map[string]struct{}, _ string) []string {
	invalid := make([]string, 0)
	for _, support := range contract.Supports {
		support = strings.TrimSpace(support)
		if support == "" {
			invalid = append(invalid, "<empty>")
			continue
		}
		if _, ok := evidenceIDs[support]; !ok {
			invalid = append(invalid, support)
		}
	}
	return invalid
}
