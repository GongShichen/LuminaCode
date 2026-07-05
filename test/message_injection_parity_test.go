package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"LuminaCode/agent"
	"LuminaCode/memory"
)

func TestInsertBeforeCurrentUserMessageSkipsToolResultCarrier(t *testing.T) {
	state := agent.NewAgentState()
	state.Messages = []map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "original task"}}},
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "t1", "name": "read_file"}}},
		{"role": "user", "content": []map[string]any{
			{"type": "tool_result", "tool_use_id": "t1", "content": "result"},
			{"type": "text", "text": "<system_hint>hint</system_hint>"},
		}},
	}

	injected := map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "hidden memory"}}, "isMeta": true}
	agent.InsertBeforeCurrentUserMessage(&state, injected)
	if len(state.Messages) != 4 {
		t.Fatalf("expected inserted message, got %#v", state.Messages)
	}
	if state.Messages[0]["isMeta"] != true || state.Messages[0]["role"] != "user" {
		t.Fatalf("expected injection before natural language user message, got %#v", state.Messages)
	}
}

func TestInsertBeforeCurrentUserMessageAppendsWhenOnlyToolResultCarrierExists(t *testing.T) {
	state := agent.NewAgentState()
	state.Messages = []map[string]any{
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "t1", "name": "read_file", "input": map[string]any{}}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": "ok"}}},
	}
	injected := map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "hidden context"}}}

	agent.InsertBeforeCurrentUserMessage(&state, injected)

	if len(state.Messages) != 3 {
		t.Fatalf("expected injection to append when no natural-language user query exists, got %#v", state.Messages)
	}
	blocks, ok := state.Messages[2]["content"].([]map[string]any)
	if !ok || len(blocks) != 1 || blocks[0]["text"] != "hidden context" {
		t.Fatalf("expected injection to append when no natural-language user query exists, got %#v", state.Messages)
	}
}

func TestStripMessageMetadataDoesNotStripContentBlocks(t *testing.T) {
	messages := []map[string]any{{
		"role":     "user",
		"isMeta":   true,
		"metadata": map[string]any{"source": "memory"},
		"content": []map[string]any{
			{"type": "advisor_block", "text": "must remain here"},
			{"type": "text", "text": "hello"},
		},
	}}

	stripped := agent.StripMessageMetadata(messages)
	if _, ok := stripped[0]["isMeta"]; ok {
		t.Fatalf("isMeta should be stripped: %#v", stripped)
	}
	if _, ok := stripped[0]["metadata"]; ok {
		t.Fatalf("metadata should be stripped: %#v", stripped)
	}
	blocks := stripped[0]["content"].([]map[string]any)
	if len(blocks) != 2 || blocks[0]["type"] != "advisor_block" {
		t.Fatalf("content blocks should be preserved exactly, got %#v", blocks)
	}
}

func TestInjectRecalledMemoriesUsesMemoryBuilderFormat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "build-notes.md")
	if err := os.WriteFile(path, []byte("memory body"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := agent.NewAgentState()
	state.Messages = []map[string]any{{"role": "user", "content": "current task"}}

	agent.InjectRecalledMemories(&state, []agent.MemoryRecall{{
		Filename: "build-notes.md",
		FilePath: path,
		Content:  "Run go test ./...",
		RecallID: "agent/build-notes.md",
	}})

	if len(state.Messages) != 2 {
		t.Fatalf("expected memory injection before current task, got %#v", state.Messages)
	}
	injected := state.Messages[0]
	metadata, ok := injected["metadata"].(map[string]any)
	if !ok || metadata[memory.MemoryMetaKey] != true || metadata["source"] != memory.MemoryRecallSource {
		t.Fatalf("unexpected memory metadata: %#v", injected["metadata"])
	}
	if got := metadata["filenames"].([]string); len(got) != 1 || got[0] != "build-notes.md" {
		t.Fatalf("unexpected recalled filenames: %#v", metadata["filenames"])
	}
	if got := metadata["recall_ids"].([]string); len(got) != 1 || got[0] != "agent/build-notes.md" {
		t.Fatalf("unexpected recall ids: %#v", metadata["recall_ids"])
	}
	text := injected["content"].([]map[string]any)[0]["text"].(string)
	if !strings.HasPrefix(text, "<system-reminder>\n") || !strings.Contains(text, "Run go test ./...") || strings.Contains(text, "<recalled-memories>") {
		t.Fatalf("unexpected recalled memory text: %q", text)
	}
	if state.Messages[1]["content"] != "current task" {
		t.Fatalf("memory should be inserted before current user message: %#v", state.Messages)
	}
}

func TestInjectMemoryIndexContextBeforeCurrentUserMessage(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("- [Build Notes](build-notes.md) - Build troubleshooting\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state := agent.NewAgentState()
	state.Messages = []map[string]any{
		{"role": "assistant", "content": []map[string]any{{"type": "tool_use", "id": "t1", "name": "read_file"}}},
		{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "t1", "content": "result"}}},
		{"role": "user", "content": "current task"},
	}

	agent.InjectMemoryIndexContext(&state, dir)

	if len(state.Messages) != 4 {
		t.Fatalf("expected memory index injection, got %#v", state.Messages)
	}
	injected := state.Messages[2]
	metadata, ok := injected["metadata"].(map[string]any)
	if !ok || metadata[memory.MemoryMetaKey] != true || metadata["source"] != memory.MemoryIndexSource {
		t.Fatalf("unexpected memory index metadata: %#v", injected["metadata"])
	}
	text := injected["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(text, "Contents of "+filepath.Join(dir, "MEMORY.md")) || !strings.Contains(text, "Build troubleshooting") {
		t.Fatalf("unexpected memory index text: %q", text)
	}
	if state.Messages[3]["content"] != "current task" {
		t.Fatalf("memory index should be inserted before current user message: %#v", state.Messages)
	}
}
