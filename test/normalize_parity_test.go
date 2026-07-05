package test

import (
	"testing"

	"LuminaCode/agent"
	"LuminaCode/config"

	mapset "github.com/deckarep/golang-set/v2"
)

func TestNormalizeMessagesMatchesPythonPipeline(t *testing.T) {
	messages := []map[string]any{
		{
			"role": "user",
			"content": []map[string]any{
				{"type": "text", "text": "see this"},
				{"type": "image", "source": "large"},
				{"type": "file", "name": "keep.txt"},
				{"type": "thinking", "thinking": "user private thought"},
				{"type": "advisor_block", "text": "internal"},
			},
		},
		{
			"role":      "user",
			"isVirtual": true,
			"content":   []map[string]any{{"type": "text", "text": "ui-only"}},
		},
		{
			"role": "assistant",
			"id":   "msg-1",
			"content": []map[string]any{
				{"type": "thinking", "thinking": "keep for thinking model"},
				{"type": "text", "text": "part 1"},
			},
		},
		{
			"role": "assistant",
			"id":   "msg-1",
			"content": []map[string]any{
				{"type": "text", "text": "part 2"},
			},
		},
		{
			"role":    "assistant",
			"id":      "msg-2",
			"content": []map[string]any{{"type": "text", "text": "separate"}},
		},
	}

	normalized := agent.NormalizeMessages(messages, "deepseek-v4-pro", []string{"image too large"})
	if len(normalized) != 3 {
		t.Fatalf("expected virtual and split messages normalized to 3 messages, got %#v", normalized)
	}

	firstBlocks := normalized[0]["content"].([]map[string]any)
	if firstBlocks[0]["type"] != "file" || firstBlocks[1]["type"] != "text" {
		t.Fatalf("expected attachment ordering and image stripping, got %#v", firstBlocks)
	}
	for _, block := range firstBlocks {
		if block["type"] == "thinking" || block["type"] == "advisor_block" || block["type"] == "image" {
			t.Fatalf("unexpected stripped block survived: %#v", firstBlocks)
		}
	}

	mergedBlocks := normalized[1]["content"].([]map[string]any)
	if len(mergedBlocks) != 3 || mergedBlocks[0]["type"] != "thinking" || mergedBlocks[2]["text"] != "part 2" {
		t.Fatalf("expected same-id assistant chunks merged with thinking preserved, got %#v", mergedBlocks)
	}
	if normalized[2]["id"] != "msg-2" {
		t.Fatalf("assistant messages with different ids must not be merged: %#v", normalized[2])
	}
}

func TestNormalizeMessagesStripsUnsupportedThinkingAndRepairsOrphanResult(t *testing.T) {
	messages := []map[string]any{
		{
			"role": "user",
			"content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "lost-tool", "content": "orphan result"},
			},
		},
		{
			"role": "assistant",
			"id":   "msg-plain",
			"content": []map[string]any{
				{"type": "thinking", "thinking": "strip me"},
				{"type": "signature", "signature": "strip me too"},
				{"type": "text", "text": "visible"},
			},
		},
	}

	normalized := agent.NormalizeMessages(messages, "gpt-4.1", nil)
	if len(normalized) != 3 {
		t.Fatalf("expected synthetic assistant plus original messages, got %#v", normalized)
	}
	synthetic := normalized[0]["content"].([]map[string]any)[0]
	if synthetic["type"] != "tool_use" || synthetic["id"] != "lost-tool" || synthetic["name"] != "unknown" {
		t.Fatalf("expected synthetic unknown tool_use before orphan result, got %#v", synthetic)
	}
	assistantBlocks := normalized[2]["content"].([]map[string]any)
	if len(assistantBlocks) != 1 || assistantBlocks[0]["type"] != "text" {
		t.Fatalf("expected unsupported model thinking/signature stripped, got %#v", assistantBlocks)
	}
}

func TestBuildMessagesStripsMetadataAndAppliesClaudeRollingCache(t *testing.T) {
	cfg := config.NewConfig()
	cfg.APIModel = "claude-sonnet-4-6"
	engine := agent.NewCoreExecutionEngine(&cfg)
	state := agent.NewAgentState()
	for i := 0; i < 12; i++ {
		state.Messages = append(state.Messages, map[string]any{
			"role":     "user",
			"isMeta":   i == 0,
			"metadata": map[string]any{"idx": i},
			"content":  []map[string]any{{"type": "text", "text": "message"}},
		})
	}

	built := engine.BuildMessages(&state)
	if len(built) != 12 {
		t.Fatalf("unexpected message count: %#v", built)
	}
	if state.CacheBreakPoints.Cardinality() != 1 || !state.CacheBreakPoints.Contains(4) {
		t.Fatalf("expected Python rolling breakpoint at n/3, got %#v", state.CacheBreakPoints.ToSlice())
	}
	if _, ok := built[0]["metadata"]; ok {
		t.Fatalf("BuildMessages must strip message metadata: %#v", built[0])
	}
	block := built[4]["content"].([]map[string]any)[0]
	if cc, ok := block["cache_control"].(map[string]any); !ok || cc["type"] != "ephemeral" {
		t.Fatalf("expected cache_control on breakpoint block, got %#v", block)
	}

	cfg.APIModel = "gpt-5"
	nonClaude := agent.NewCoreExecutionEngine(&cfg)
	state.CacheBreakPoints = mapset.NewSet[int](4)
	_ = nonClaude.BuildMessages(&state)
	if state.CacheBreakPoints.Cardinality() != 0 {
		t.Fatalf("non-Claude models should clear rolling cache breakpoints, got %#v", state.CacheBreakPoints.ToSlice())
	}
}
