package agentbench

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	luminaapi "LuminaCode/api"
	"LuminaCode/memory"
)

func TestLongMemEvalAnswerPromptUsesGenericKnownStateAndSynthesisRules(t *testing.T) {
	for _, required := range []string{
		"latest applicable observation at or before the reference time",
		"current known state",
		"demonstrated interests, preferences, experiences, constraints, and available resources",
		"supported personalization and ordinary practical synthesis are allowed",
		"cannot support a defensible response",
	} {
		if !strings.Contains(longMemEvalAnswerSystemPrompt, required) {
			t.Fatalf("answer prompt missing generic grounding rule %q", required)
		}
	}
	for _, forbidden := range []string{"question_type", "gold", "longmemeval", "benchmark"} {
		if strings.Contains(strings.ToLower(longMemEvalAnswerSystemPrompt), forbidden) {
			t.Fatalf("answer prompt contains forbidden benchmark coupling %q", forbidden)
		}
	}
}

func TestLongMemEvalQAResponseUsesMinimalGroundedContract(t *testing.T) {
	contractJSON, err := json.Marshal(map[string]any{
		"supports":     []string{"s1"},
		"answer":       "concise answer",
		"insufficient": false,
	})
	if err != nil {
		t.Fatal(err)
	}
	answer, contract, mode := parseLongMemEvalQAResponse(string(contractJSON))
	if answer != "concise answer" || mode != "json_contract" || len(contract.Supports) != 1 {
		t.Fatalf("answer=%q mode=%q contract=%+v", answer, mode, contract)
	}
	answer, _, mode = parseLongMemEvalQAResponse("plain answer from the same response")
	if answer != "plain answer from the same response" || mode != "plain_same_response" {
		t.Fatalf("plain answer=%q mode=%q", answer, mode)
	}
	answer, _, mode = parseLongMemEvalQAResponse(`{"supports":[`)
	if answer != "" || mode != "malformed_contract" {
		t.Fatalf("malformed contract leaked as an answer: answer=%q mode=%q", answer, mode)
	}
}

func TestLongMemEvalQACompletionUsesRequiredStructuredTool(t *testing.T) {
	response := luminaapi.Response{Text: "ignored", ToolCalls: []map[string]any{{
		"name": longMemEvalAnswerToolName,
		"input": map[string]any{
			"supports": []any{"e01", "e02"}, "answer": "grounded answer", "insufficient": false,
		},
	}}}
	answer, contract, mode := parseLongMemEvalQACompletion(response)
	if answer != "grounded answer" || mode != "tool_contract" || len(contract.Supports) != 2 {
		t.Fatalf("answer=%q mode=%q contract=%+v", answer, mode, contract)
	}
	if longMemEvalQAResponseNeedsRetry(answer, contract, mode,
		map[string]struct{}{"e01": {}, "e02": {}}, "") {
		t.Fatal("valid structured tool response was marked for repair")
	}
	if got := longMemEvalQAResponseText(response); !strings.Contains(got, `"grounded answer"`) {
		t.Fatalf("structured response was not persisted as JSON: %q", got)
	}
}

func TestLongMemEvalAnswerToolHasOnlyGroundedAnswerFields(t *testing.T) {
	tool := longMemEvalAnswerTool()
	if tool["name"] != longMemEvalAnswerToolName {
		t.Fatalf("tool name=%v", tool["name"])
	}
	schema, ok := tool["input_schema"].(map[string]any)
	if !ok {
		t.Fatalf("input schema=%#v", tool["input_schema"])
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties=%#v", schema["properties"])
	}
	if len(properties) != 3 {
		t.Fatalf("unexpected structured answer fields: %#v", properties)
	}
	for _, field := range []string{"supports", "answer", "insufficient"} {
		if _, ok := properties[field]; !ok {
			t.Fatalf("missing structured answer field %q", field)
		}
	}
}

func TestLongMemEvalQAResponseDiscardsPartiallyDecodedContract(t *testing.T) {
	raw := `{"supports":["s1"],"answer":"Johnson","insufficient":false,}`
	answer, contract, mode := parseLongMemEvalQAResponse(raw)
	if answer != "Johnson" || mode != "recovered_answer_field" {
		t.Fatalf("answer=%q mode=%q", answer, mode)
	}
	if len(contract.Supports) != 0 {
		t.Fatalf("partial contract survived parse failure: %+v", contract)
	}
	if !longMemEvalQAResponseNeedsRetry(answer, contract, mode, map[string]struct{}{"s1": {}}, "") {
		t.Fatal("malformed generated contract was not marked for repair")
	}
}

func TestLongMemEvalQAResponseRetriesUnanchoredSupportID(t *testing.T) {
	evidenceIDs := map[string]struct{}{"session-123": {}}
	contract := longMemEvalQAContract{
		Supports: []string{"session-132"}, Answer: json.RawMessage(`"answer"`),
	}
	if !longMemEvalQAResponseNeedsRetry("answer", contract, "json_contract", evidenceIDs, "") {
		t.Fatal("near-miss support ID was not marked for model repair")
	}
	instruction := longMemEvalQARepairInstruction("json_contract", contract, evidenceIDs, "")
	if !strings.Contains(instruction, "session-132") || !strings.Contains(instruction, "copy every support ID exactly") {
		t.Fatalf("repair instruction did not identify the invalid support: %q", instruction)
	}
	contract.Supports = []string{"session-123"}
	if longMemEvalQAResponseNeedsRetry("answer", contract, "json_contract", evidenceIDs, "") {
		t.Fatal("valid grounded support ID was marked for repair")
	}
	contract.Supports = []string{"1"}
	if !longMemEvalQAResponseNeedsRetry("answer", contract, "json_contract", evidenceIDs,
		"[1] id=session-123") {
		t.Fatal("numeric section label was accepted as an evidence ID")
	}
}

func TestLongMemEvalFinalAnswerRequiresGroundedSupports(t *testing.T) {
	contract := longMemEvalQAContract{Supports: []string{"s1"}, Answer: json.RawMessage(`"grounded"`)}
	if got := finalizeLongMemEvalQAAnswer("grounded", contract, map[string]struct{}{"s1": {}}, ""); got != "grounded" {
		t.Fatalf("grounded answer=%q", got)
	}
	if got := finalizeLongMemEvalQAAnswer("ungrounded", contract, map[string]struct{}{"s2": {}}, ""); got != "" {
		t.Fatalf("ungrounded answer was accepted: %q", got)
	}
	contract.Insufficient = true
	if got := finalizeLongMemEvalQAAnswer("nearby value", contract, nil, ""); got != "Insufficient evidence." {
		t.Fatalf("insufficient answer=%q", got)
	}
}

func TestLongMemEvalContractHasNoTaskModeOrOperands(t *testing.T) {
	encoded, err := json.Marshal(longMemEvalQAContract{Supports: []string{"s1"}, Answer: json.RawMessage(`"answer"`)})
	if err != nil {
		t.Fatal(err)
	}
	text := string(encoded)
	for _, forbidden := range []string{`"mode"`, `"operands"`, `"operation"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("answer contract contains task-specific field %s: %s", forbidden, text)
		}
	}
}

func TestLongMemEvalAnswerPromptUsesOneUniversalGroundingPolicy(t *testing.T) {
	prompt := strings.ToLower(longMemEvalAnswerSystemPrompt)
	for _, required := range []string{
		"read every evidence section",
		"combine directly relevant facts across sections",
		"supporting facts appear in separate records",
		"explicitly negated event did not occur",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("answer prompt is missing universal grounding instruction %q", required)
		}
	}
	for _, forbidden := range []string{
		"question_type", "gold answer", "benchmark", "task mode", "operator", "operands",
	} {
		if strings.Contains(prompt, forbidden) {
			t.Fatalf("answer prompt contains disallowed routing concept %q", forbidden)
		}
	}
}

func TestCompactLongMemEvalSearchEvidencePreservesEverySelectedItem(t *testing.T) {
	result := memory.SearchResult{Route: []string{"lexical", "context-expand"}}
	for index := 0; index < 8; index++ {
		id := fmt.Sprintf("context-%d", index)
		content := strings.Repeat(fmt.Sprintf("evidence-%d ", index), 300)
		result.Evidence = append(result.Evidence, memory.Evidence{ID: id, ResourceID: id,
			ResourceKind: "context", Content: content, ContextID: fmt.Sprintf("session-%d", index/2),
			Score:      float64(index),
			Actor:      "user",
			OccurredAt: time.Date(2026, time.July, index+1, 0, 0, 0, 0, time.UTC)})
	}
	packet, ids := compactLongMemEvalSearchEvidence(result, time.Date(2026, time.July, 20, 0, 0, 0, 0, time.UTC), 9000)
	if len([]rune(packet)) > 9000 {
		t.Fatalf("packet exceeded rune budget: %d", len([]rune(packet)))
	}
	for index := 0; index < 8; index++ {
		alias := fmt.Sprintf("e%02d", index+1)
		if _, ok := ids[alias]; !ok || !strings.Contains(packet, "["+alias+"]") ||
			!strings.Contains(packet, fmt.Sprintf("evidence-%d", index)) {
			t.Fatalf("packet omitted selected item alias %s", alias)
		}
	}
	if strings.Contains(packet, "context-0") {
		t.Fatalf("packet leaked redundant internal evidence IDs: %s", packet)
	}
	if strings.Contains(packet, "Relevance order:") {
		t.Fatalf("packet retained the regressive score-ranked evidence index: %s", packet)
	}
	if !strings.Contains(packet, "Context c1:\n[e01] actor=user") ||
		!strings.Contains(packet, "Context c4:\n[e07] actor=user") {
		t.Fatalf("packet omitted compact context and actor provenance: %s", packet)
	}
	for _, contextAlias := range []string{"c1", "c2", "c3", "c4"} {
		if count := strings.Count(packet, "Context "+contextAlias+":"); count != 1 {
			t.Fatalf("packet emitted context %s %d times, want one group header: %s",
				contextAlias, count, packet)
		}
	}
	if !strings.Contains(packet, "Evidence IDs:") || strings.Contains(packet, "Local memory route:") {
		t.Fatalf("packet retained redundant metadata: %s", packet)
	}
	budgets := balancedEvidenceBudgets([]string{strings.Repeat("a", 5000), strings.Repeat("b", 5000),
		strings.Repeat("c", 5000)}, 3000)
	if budgets[0] != 1000 || budgets[1] != 1000 || budgets[2] != 1000 {
		t.Fatalf("evidence budget was not balanced across sections: %v", budgets)
	}
}
