package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"LuminaCode/agent"
	"LuminaCode/agentContext"
)

func TestMicroCompactUsesToolResultBlocksAndNames(t *testing.T) {
	messages := []map[string]any{
		{"role": "assistant", "content": []any{
			map[string]any{"type": "tool_use", "id": "read-1", "name": "read_file"},
			map[string]any{"type": "tool_use", "id": "bash-1", "name": "run_shell"},
		}},
		{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "read-1", "content": "old read"},
			map[string]any{"type": "tool_result", "tool_use_id": "bash-1", "content": "fresh shell"},
		}},
	}

	compacted, edits := agentContext.MicroCompactMessages(messages, true, 1)
	if len(edits) != 0 {
		t.Fatalf("expected no cache edits for cold compaction, got %#v", edits)
	}
	if cleared := agentContext.CountClearedToolResult(compacted); cleared != 1 {
		t.Fatalf("expected one cleared tool result, got %d in %#v", cleared, compacted)
	}

	warm, edits := agentContext.MicroCompactMessages(messages, false, 1)
	if agentContext.CountClearedToolResult(warm) != 0 {
		t.Fatalf("warm cache path should not mutate content: %#v", warm)
	}
	if len(edits) != 1 || edits[0].ToolUseID != "read-1" {
		t.Fatalf("expected cache edit for old read result, got %#v", edits)
	}
}

func TestMicroCompactKeepRecentZeroIsClampedToOneLikePython(t *testing.T) {
	messages := []map[string]any{}
	messages = append(messages, microRound("read_file", "read-1", "old read")...)
	messages = append(messages, microRound("run_shell", "shell-1", "recent shell")...)

	compressed, edits := agentContext.MicroCompactMessages(messages, true, 0)
	if len(edits) != 0 {
		t.Fatalf("cold microcompact should not emit cache edits, got %#v", edits)
	}
	contents := toolResultContents(compressed)
	want := []string{"[Old tool result content cleared]", "recent shell"}
	if strings.Join(contents, "\n") != strings.Join(want, "\n") {
		t.Fatalf("keep_recent=0 should clamp to one like Python:\nwant %#v\n got %#v", want, contents)
	}
}

func TestMicroCompactClaudeDisplayAliasesAreNotLuminaBuiltinsLikePython(t *testing.T) {
	messages := []map[string]any{}
	messages = append(messages, microRound("FileRead", "read-1", "old read")...)
	messages = append(messages, microRound("Bash", "shell-1", "old shell")...)

	compressed, edits := agentContext.MicroCompactMessages(messages, true, 1)
	if len(edits) != 0 {
		t.Fatalf("display aliases should not emit cache edits, got %#v", edits)
	}
	contents := toolResultContents(compressed)
	want := []string{"old read", "old shell"}
	if strings.Join(contents, "\n") != strings.Join(want, "\n") {
		t.Fatalf("display aliases should not be treated as builtins:\nwant %#v\n got %#v", want, contents)
	}
}

func microRound(toolName, toolUseID, content string) []map[string]any {
	return []map[string]any{
		{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "id": toolUseID, "name": toolName, "input": map[string]any{}},
		}},
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": toolUseID, "content": content},
		}},
	}
}

func toolResultContents(messages []map[string]any) []string {
	var out []string
	for _, msg := range messages {
		switch blocks := msg["content"].(type) {
		case []map[string]any:
			for _, block := range blocks {
				if block["type"] == "tool_result" {
					out = append(out, block["content"].(string))
				}
			}
		case []any:
			for _, raw := range blocks {
				block, _ := raw.(map[string]any)
				if block["type"] == "tool_result" {
					out = append(out, block["content"].(string))
				}
			}
		}
	}
	return out
}

func TestContextCompactionHandlesNativeGoContentBlocks(t *testing.T) {
	longOld := strings.Repeat("x", 2500)
	messages := []map[string]any{
		{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "id": "read-1", "name": "read_file"},
		}},
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "read-1", "content": longOld},
		}},
		{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "id": "bash-1", "name": "run_shell"},
		}},
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "bash-1", "content": "fresh shell"},
		}},
	}

	if tokens := agentContext.TokenCountWithEstimation(messages); tokens == 0 {
		t.Fatal("native []map content should contribute to token estimation")
	}
	snipped, freed := agentContext.SnipCompactIfNeeded(messages, 1, 2000)
	if freed <= 0 {
		t.Fatalf("expected snip compaction to see native []map content, got freed=%d", freed)
	}
	firstResult := snipped[1]["content"].([]map[string]any)[0]["content"].(string)
	if !strings.Contains(firstResult, "<snip>") {
		t.Fatalf("expected old native tool result snipped, got %q", firstResult)
	}
	micro, edits := agentContext.MicroCompactMessages(messages, false, 1)
	if agentContext.CountClearedToolResult(micro) != 0 || len(edits) != 1 || edits[0].ToolUseID != "read-1" {
		t.Fatalf("expected warm microcompact edit for native content, edits=%#v micro=%#v", edits, micro)
	}
}

func TestSnipCompactUsesRoleAndPreservesRecentToolResults(t *testing.T) {
	longOld := strings.Repeat("x", 2100)
	longFresh := strings.Repeat("y", 2100)
	messages := []map[string]any{
		{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": longOld}}},
		{"role": "assistant", "content": "ok"},
		{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": longFresh}}},
	}
	compacted, freed := agentContext.SnipCompactIfNeeded(messages, 1, 2000)
	if freed <= 0 {
		t.Fatalf("expected snip tokens freed, got %d", freed)
	}
	firstContent := compacted[0]["content"].([]any)[0].(map[string]any)["content"].(string)
	lastContent := compacted[2]["content"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(firstContent, "<snip>") || lastContent != longFresh {
		t.Fatalf("unexpected snip compact result: first=%q last-len=%d", firstContent, len(lastContent))
	}
}

func TestSnipCompactHonorsExplicitZeroPreserveTurns(t *testing.T) {
	longOld := strings.Repeat("x", 2100)
	longFresh := strings.Repeat("y", 2100)
	messages := []map[string]any{
		{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": longOld}}},
		{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": longFresh}}},
	}

	compacted, freed := agentContext.SnipCompactIfNeeded(messages, 0, 2000)
	if freed <= 0 {
		t.Fatalf("expected explicit zero preserve count to free tokens, got %d", freed)
	}
	firstContent := compacted[0]["content"].([]any)[0].(map[string]any)["content"].(string)
	lastContent := compacted[1]["content"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(firstContent, "<snip>") || !strings.Contains(lastContent, "<snip>") {
		t.Fatalf("explicit zero preserve should snip every stale tool result: first=%q last=%q", firstContent, lastContent)
	}
}

func TestSnipCompactUsesPythonCharacterThreshold(t *testing.T) {
	old := "你好世界"
	fresh := strings.Repeat("fresh ", 20)
	messages := []map[string]any{
		{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": old}}},
		{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": fresh}}},
	}

	compacted, freed := agentContext.SnipCompactIfNeeded(messages, 1, 4)
	if freed != 0 {
		t.Fatalf("content length equal to threshold should not be snipped, freed=%d", freed)
	}
	firstContent := compacted[0]["content"].([]any)[0].(map[string]any)["content"].(string)
	if firstContent != old {
		t.Fatalf("snip threshold should count Python characters, got %q", firstContent)
	}

	compacted, freed = agentContext.SnipCompactIfNeeded(messages, 1, 3)
	if freed == 0 {
		t.Fatalf("content over character threshold should report token delta, got %d", freed)
	}
	firstContent = compacted[0]["content"].([]any)[0].(map[string]any)["content"].(string)
	if !strings.Contains(firstContent, "<snip>") {
		t.Fatalf("expected snip tag when character threshold exceeded, got %q", firstContent)
	}
}

func TestSnipToolResultKeepsPipSuccessSeparator(t *testing.T) {
	input := "before\nSuccessfully installed alpha beta\nline two\n\nkept"
	got := agentContext.SnipToolResult(input)
	want := "before\n[pip packages installed]\n\nkept"
	if got != want {
		t.Fatalf("unexpected pip success snip:\nwant %q\n got %q", want, got)
	}
}

func TestContextPipelineSnipRemovedUsesPythonCharacters(t *testing.T) {
	content := "Successfully installed " + strings.Repeat("界", 10) + "\n\nkept"
	messages := []map[string]any{
		{"role": "user", "content": []any{map[string]any{"type": "tool_result", "content": content}}},
	}
	pipeline := agentContext.DefaultContextPipeline()
	_, stats := pipeline.Compress(messages, 10, "sys", 1, 0.5, nil, nil, false)
	if stats.SnipRemoved != 9 {
		t.Fatalf("snip removed should use Python character counts, got %#v", stats)
	}
}

func TestContextPipelineL2ReportsExactClearedBlockCountLikePython(t *testing.T) {
	var messages []map[string]any
	messages = append(messages, microRound("read_file", "tool-1", strings.Repeat("A", 1800))...)
	messages = append(messages, microRound("run_shell", "tool-2", strings.Repeat("B", 1800))...)
	messages = append(messages, microRound("grep_search", "tool-3", strings.Repeat("C", 1800))...)

	pipeline := agentContext.DefaultContextPipeline()
	compressed, stats := pipeline.Compress(messages, 0, "system", 200, 0.4, nil, nil, false)

	if stats.MicroCleared != 2 || stats.MicroTruncated != 2 || stats.MicroTokensFreed <= 0 {
		t.Fatalf("expected exact Python L2 contribution stats, got %#v", stats)
	}
	contents := toolResultContents(compressed)
	if len(contents) != 3 || contents[0] != "[Old tool result content cleared]" ||
		contents[1] != "[Old tool result content cleared]" ||
		contents[2] != strings.Repeat("C", 1800) {
		t.Fatalf("expected only recent tool result preserved after L2, got %#v", contents)
	}
}

func TestMicroCompactResultUsesPythonCharacterSlices(t *testing.T) {
	content := "你好abcdef再见"
	got := agentContext.MicroCompactResult(content, 6)
	want := "你好a\n\n... [4 characters truncated] ...\n\nf再见"
	if got != want {
		t.Fatalf("unexpected microcompact result:\nwant %q\n got %q", want, got)
	}
	zero := agentContext.MicroCompactResult("abcd", 0)
	if zero != "\n\n... [4 characters truncated] ...\n\nabcd" {
		t.Fatalf("max_chars=0 should mirror Python slicing, got %q", zero)
	}
}

func TestAutoCompactHelpersMatchPythonBehavior(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": "first"},
		{"role": "assistant", "content": "answer"},
		{"role": "user", "content": "second"},
		{"role": "assistant", "content": "answer2"},
		{"role": "user", "content": "third"},
	}
	summary := "Primary Request\nPending Tasks"
	injected := agentContext.InjectSummary(context.Background(), messages, &summary, 1)
	if len(injected) != 4 || !strings.Contains(injected[1]["content"].([]any)[0].(map[string]any)["text"].(string), "earlier conversation compressed") {
		t.Fatalf("unexpected injected summary: %#v", injected)
	}
	injectedText := injected[1]["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.HasPrefix(injectedText, "[Session summary — earlier conversation compressed]\n\n") {
		t.Fatalf("summary injection header should match Python exactly, got %q", injectedText)
	}

	truncated := agentContext.TruncateHeadForPtlRetry(context.Background(), messages)
	if len(truncated) != 4 || truncated[0]["content"] != "first" || truncated[1]["content"] != "second" {
		t.Fatalf("unexpected PTL truncation: %#v", truncated)
	}

	if agentContext.ShouldAutoCompact(context.Background(), 159999, 0, 200000, 0, false) {
		t.Fatal("159999 should be below LUMINA's 80% trigger threshold")
	}
	if !agentContext.ShouldAutoCompact(context.Background(), 160000, 0, 200000, 0, false) {
		t.Fatal("expected autocompact at APIMaxTokens/contextLimit 80% trigger threshold")
	}

	attempts := 0
	summaryText, err := agentContext.ExecuteAutoCompactWithRetry(context.Background(), messages, "sys", func(ctx context.Context, msgs []map[string]any, system string) (string, error) {
		attempts++
		if attempts == 1 {
			return "", errors.New("prompt is too long")
		}
		if len(msgs) != 4 || msgs[1]["content"] != "second" {
			t.Fatalf("retry did not use truncated current messages: %#v", msgs)
		}
		return "ok", nil
	})
	if err != nil || summaryText != "ok" || attempts != 2 {
		t.Fatalf("unexpected retry result summary=%q attempts=%d err=%v", summaryText, attempts, err)
	}
}

func TestAutoCompactUsesPythonPromptAndCharacterTokenEstimate(t *testing.T) {
	var requestBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(bodyBytes, &requestBody); err != nil {
			t.Fatalf("invalid request body: %v body=%s", err, bodyBytes)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"<analysis>scratch</analysis>总结内容\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	result := agentContext.AutoCompact(context.Background(), []map[string]any{
		{"role": "user", "content": "请总结"},
	}, "sys", agentContext.AutoCompactOptions{
		APIKey:           "test-key",
		APIBaseURL:       server.URL,
		APIModel:         "custom-model",
		APIType:          "openai_compatible",
		MaxSummaryTokens: 500,
	})
	if !result.Success || result.Summary != "总结内容" || result.SummaryTokens != 1 {
		t.Fatalf("unexpected autocompact result: %#v", result)
	}
	messages, _ := requestBody["messages"].([]any)
	if len(messages) == 0 {
		t.Fatalf("expected OpenAI-compatible messages in request: %#v", requestBody)
	}
	system, _ := messages[0].(map[string]any)
	wantSystem := "CRITICAL: Respond with TEXT ONLY. Do NOT call any tools. Tool calls will be REJECTED and will waste your only turn.\n\n" +
		"You are a summarization agent. You must compress the conversation using a two-stage process:\n" +
		"1. <analysis>: Write a chronological scratchpad of the user's intent, methods tried, and errors.\n" +
		"2. <summary>: Provide the final summary. It MUST include these sections:\n" +
		"   - Primary Request\n" +
		"   - Files and Code (include absolute paths)\n" +
		"   - Errors and Fixes\n" +
		"   - Current Work\n" +
		"   - Pending Tasks"
	if system["content"] != wantSystem {
		t.Fatalf("autocompact system prompt diverged from Python:\nwant %q\n got %q", wantSystem, system["content"])
	}
}

func TestContextPipelineAutoCompactClientCreationFailureIncrementsCircuitLikePython(t *testing.T) {
	state := agent.NewAgentState()
	pipeline := agentContext.DefaultContextPipeline()
	pipeline.Config.APIKey = ""
	pipeline.Config.APIBaseURL = "http://127.0.0.1:1"
	pipeline.Config.APIModel = "gpt-5"
	pipeline.Config.APIType = "openai_compatible"
	messages := []map[string]any{{
		"role":    "user",
		"content": strings.Repeat("large context ", 2000),
	}}

	_, stats := pipeline.Compress(messages, 0, "sys", 100, 0.1, &state, nil, true)
	if !stats.AutoTriggered || stats.LevelReached != 4 {
		t.Fatalf("expected forced L4 autocompact attempt, got %#v", stats)
	}
	if state.ConsecutiveAutoCompactFailures != 1 || pipeline.ConsecutiveAutoCompactFailures != 1 {
		t.Fatalf("client creation failure should increment Python circuit counters, state=%d pipeline=%d", state.ConsecutiveAutoCompactFailures, pipeline.ConsecutiveAutoCompactFailures)
	}
}

func TestCompactionReplacementHistoryKeepsRealUserRequestsOnly(t *testing.T) {
	messages := []map[string]any{
		{"role": "user", "content": "first real request"},
		{"role": "user", "content": "memory index", "isMeta": true, "metadata": map[string]any{"source": "memory_index", "lumina_memory_context": true}},
		{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "thinking done"}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "call-1", "content": "tool output"}}},
		{"role": "user", "content": "second real request"},
		{"role": "assistant", "content": "recent assistant"},
		{"role": "user", "content": "recent user"},
	}

	replacement := agentContext.BuildCompactionReplacementHistory(messages, "handoff summary", 1)
	if strings.Join(replacement.UserRequests, "|") != "first real request|second real request" {
		t.Fatalf("unexpected user request summary: %#v", replacement.UserRequests)
	}
	if replacement.Summary != "handoff summary" {
		t.Fatalf("unexpected summary: %q", replacement.Summary)
	}
	if len(replacement.Recent) != 2 ||
		replacement.Recent[0]["content"] != "recent assistant" ||
		replacement.Recent[1]["content"] != "recent user" {
		t.Fatalf("unexpected recent history: %#v", replacement.Recent)
	}
}

func TestContextPipelineAutocompactFailuresPersistOnAgentStateAcrossInstancesLikePython(t *testing.T) {
	state := agent.NewAgentState()
	messages := []map[string]any{{
		"role":    "user",
		"content": strings.Repeat("large context ", 2000),
	}}

	for i := 0; i < 2; i++ {
		pipeline := agentContext.DefaultContextPipeline()
		pipeline.Config.APIKey = ""
		pipeline.Config.APIBaseURL = "http://127.0.0.1:1"
		pipeline.Config.APIModel = "gpt-5"
		pipeline.Config.APIType = "openai_compatible"

		_, stats := pipeline.Compress(messages, 0, "sys", 100, 0.1, &state, nil, true)
		if !stats.AutoTriggered || stats.LevelReached != 4 {
			t.Fatalf("expected forced L4 autocompact attempt on pass %d, got %#v", i+1, stats)
		}
	}

	if state.ConsecutiveAutoCompactFailures != 2 {
		t.Fatalf("autocompact failures should persist on shared agent state like Python, got %d", state.ConsecutiveAutoCompactFailures)
	}
}

func TestContextPipelineUsesStateZeroAutocompactFailuresOverPipelineCounterLikePython(t *testing.T) {
	state := agent.NewAgentState()
	state.ConsecutiveAutoCompactFailures = 0
	pipeline := agentContext.DefaultContextPipeline()
	pipeline.ConsecutiveAutoCompactFailures = 3
	pipeline.Config.APIKey = ""
	pipeline.Config.APIBaseURL = "http://127.0.0.1:1"
	pipeline.Config.APIModel = "gpt-5"
	pipeline.Config.APIType = "openai_compatible"
	messages := []map[string]any{{
		"role":    "user",
		"content": strings.Repeat("large context ", 2000),
	}}

	_, stats := pipeline.Compress(messages, 0, "sys", 100, 0.1, &state, nil, true)
	if !stats.AutoTriggered || stats.LevelReached != 4 {
		t.Fatalf("state failure count 0 should override pipeline circuit like Python, got %#v", stats)
	}
	if state.ConsecutiveAutoCompactFailures != 1 || pipeline.ConsecutiveAutoCompactFailures != 1 {
		t.Fatalf("failed L4 attempt should increment from state zero, state=%d pipeline=%d", state.ConsecutiveAutoCompactFailures, pipeline.ConsecutiveAutoCompactFailures)
	}
}

func TestTokenEstimationUsesPythonCharacterLength(t *testing.T) {
	if got := agentContext.RoughEstimate("你好世界abc"); got != 1 {
		t.Fatalf("rough string estimate should use Python character length, got %d", got)
	}
	messages := []map[string]any{{"role": "user", "content": "你好世界abc"}}
	if got := agentContext.TokenCountWithEstimation(messages); got != 1 {
		t.Fatalf("message token estimate should use Python character length, got %d", got)
	}
}

func TestTokenEstimationUsesPythonDictReprForContentBlocks(t *testing.T) {
	messages := []map[string]any{{
		"role": "assistant",
		"content": []map[string]any{{
			"type": "text",
			"text": "hello",
		}},
	}}
	if got := agentContext.RoughEstimate(messages); got != 8 {
		t.Fatalf("content block estimate should match Python str(dict)//4, got %d", got)
	}
	messages = []map[string]any{{
		"role": "assistant",
		"content": []map[string]any{{
			"type": "tool_use",
			"id":   "1",
			"name": "read_file",
			"input": map[string]any{
				"file_path": "a.go",
			},
		}},
	}}
	if got := agentContext.RoughEstimate(messages); got != 21 {
		t.Fatalf("nested content block estimate should match Python repr length, got %d", got)
	}
}

func TestRunPostCompactCleanupFormattingAndDedup(t *testing.T) {
	state := agent.NewAgentState()
	state.Messages = []map[string]any{{"role": "user", "content": "hello"}}
	agentContext.RunPostCompactCleanUp(context.Background(), &state, []map[string]string{
		{"path": "/tmp/a.go", "content": "package main"},
	})
	if len(state.Messages) != 2 {
		t.Fatalf("expected recovery message appended, got %#v", state.Messages)
	}
	text := state.Messages[1]["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "--- File: /tmp/a.go ---\npackage main") || strings.Contains(text, "{/tmp/a.go}") {
		t.Fatalf("unexpected recovery text: %q", text)
	}
	agentContext.RunPostCompactCleanUp(context.Background(), &state, []map[string]string{
		{"path": "/tmp/a.go", "content": "package main"},
	})
	if len(state.Messages) != 2 {
		t.Fatalf("expected duplicate cleanup block to be skipped, got %#v", state.Messages)
	}
}

func TestRunPostCompactCleanupTruncatesByPythonCharacters(t *testing.T) {
	state := agent.NewAgentState()
	state.Messages = []map[string]any{{"role": "user", "content": "hello"}}
	content := strings.Repeat("界", agentContext.PostCompactMaxTokenPerFile*4+1)
	agentContext.RunPostCompactCleanUp(context.Background(), &state, []map[string]string{
		{"path": "/tmp/unicode.txt", "content": content},
	})
	text := state.Messages[1]["content"].([]any)[0].(map[string]any)["text"].(string)
	restored := strings.TrimPrefix(text, "[System: Post-compact memory restoration]\n\n--- File: /tmp/unicode.txt ---\n")
	if len([]rune(restored)) != agentContext.PostCompactMaxTokenPerFile*4 {
		t.Fatalf("restored content should be truncated by Python characters, got %d", len([]rune(restored)))
	}
}

func TestL3CollapseThresholdIsBoundedBeforeLuminaEightyPercentL4(t *testing.T) {
	contextLimit := 200000
	softLimit := int(float64(contextLimit) * 0.85)
	l4Threshold := softLimit
	threshold := agentContext.GetL3CollapseThreshold(contextLimit, softLimit)
	want := minIntForTest(90000, minIntForTest(softLimit, l4Threshold-1))
	if threshold != want || threshold > softLimit || threshold >= l4Threshold {
		t.Fatalf("unexpected L3 threshold: got=%d want=%d soft=%d l4=%d", threshold, want, softLimit, l4Threshold)
	}
	if got := agentContext.GetL3CollapseThreshold(200000, 170000); got != 90000 {
		t.Fatalf("L3 threshold should keep Python's 90k upper bound, got %d", got)
	}
}

func TestCollapseCreatesAndProjectsRegions(t *testing.T) {
	var messages []map[string]any
	for i := 0; i < 14; i++ {
		messages = append(messages, map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "text",
				"text": "This is an older debugging exchange with enough words.",
			}},
		})
	}
	suppress, regions := agentContext.ApplyCollapseIfNeed(messages, 100000, 90000, nil)
	if !suppress || len(regions) != 1 {
		t.Fatalf("expected one collapsed region, suppress=%v regions=%#v", suppress, regions)
	}
	if regions[0].StartIdx != 0 || regions[0].EndIdx != 4 {
		t.Fatalf("expected oldest span before recent tail to collapse, got %#v", regions[0])
	}
	projected := agentContext.ProjectCollapsedView(messages, regions)
	if len(projected) != 11 {
		t.Fatalf("expected 4 messages replaced by one summary, got len=%d", len(projected))
	}
	text := projected[0]["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(text, "[Earlier conversation — summarized]") || !strings.Contains(text, "Turn 1") || strings.Contains(text, "Turn 0") {
		t.Fatalf("unexpected projection summary: %q", text)
	}

	var alternating []map[string]any
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		alternating = append(alternating, map[string]any{
			"role": role,
			"content": []any{map[string]any{
				"type": "text",
				"text": "This is an older debugging exchange with enough words.",
			}},
		})
	}
	collapsed := agentContext.CollapseMessages(alternating, 5)
	if len(collapsed) >= len(messages) || !strings.Contains(collapsed[0]["content"].([]map[string]any)[0]["text"].(string), "Earlier conversation") {
		t.Fatalf("expected legacy collapse to summarize older exchanges: %#v", collapsed)
	}

	zeroKeep := agentContext.CollapseMessages(alternating, 0)
	if len(zeroKeep) != len(alternating) {
		t.Fatalf("Python keep_recent=0 preserves all exchanges because -0 slices from the head: %#v", zeroKeep)
	}
}

func TestContextPipelineExistingL3RegionsReportProjectionSavingsLikePython(t *testing.T) {
	var messages []map[string]any
	for i := 0; i < 12; i++ {
		messages = append(messages,
			map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": fmt.Sprintf("user-%d %s", i, strings.Repeat("x", 4000))}}},
			map[string]any{"role": "assistant", "content": []map[string]any{{"type": "text", "text": fmt.Sprintf("assistant-%d %s", i, strings.Repeat("y", 4000))}}},
		)
	}
	original := make([]map[string]any, len(messages))
	copy(original, messages)
	existing := []agentContext.CollapsedRegion{{
		StartIdx: 0,
		EndIdx:   22,
		Summary:  "[Earlier conversation -- summarized]",
	}}

	pipeline := agentContext.DefaultContextPipeline()
	compressed, stats := pipeline.Compress(messages, 0, "system", 20_000, 0.5, &agent.AgentState{SystemPrompt: "system"}, existing, false)

	if fmt.Sprint(compressed) != fmt.Sprint(original) || fmt.Sprint(messages) != fmt.Sprint(original) {
		t.Fatalf("existing L3 projection should report savings without mutating messages")
	}
	if len(stats.CollapsedRegions) != 1 || stats.CollapsedRegions[0] != existing[0] {
		t.Fatalf("expected existing collapsed region to be retained, got %#v", stats.CollapsedRegions)
	}
	if stats.CollapseCount <= 0 || stats.CollapsedTokensFreed <= 0 || stats.AutoTriggered {
		t.Fatalf("expected Python-style projection savings without L4, got %#v", stats)
	}
}

func TestCollapseSummaryThresholdUsesPythonCharacters(t *testing.T) {
	var messages []map[string]any
	for i := 0; i < 7; i++ {
		messages = append(messages, map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type": "text",
				"text": strings.Repeat("界", 10),
			}},
		})
	}
	collapsed := agentContext.CollapseMessages(messages, 2)
	if len(collapsed) != len(messages) {
		t.Fatalf("10 Python characters should not satisfy len(text) > 10 summary threshold, got %#v", collapsed)
	}
}

func minIntForTest(a, b int) int {
	if a < b {
		return a
	}
	return b
}
