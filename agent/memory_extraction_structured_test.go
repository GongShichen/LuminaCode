package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"LuminaCode/api"
)

type structuredMemoryExtractionStub struct {
	response api.Response
	err      error
	opts     api.StructuredCompletionOptions
}

func (stub *structuredMemoryExtractionStub) CompleteStructured(_ context.Context, _ string, _ []map[string]any,
	opts api.StructuredCompletionOptions) (api.Response, error) {
	stub.opts = opts
	return stub.response, stub.err
}

func TestRunStructuredMemoryExtractionAcceptsRequiredTool(t *testing.T) {
	stub := &structuredMemoryExtractionStub{response: api.Response{ToolCalls: []map[string]any{{
		"name": "ExtractMemoryBatch",
		"input": map[string]any{"memories": []any{map[string]any{
			"action": "create", "scope_type": "project", "memory_type": "preference",
			"title": "Editor preference", "summary": "Uses Vim", "content": "The user prefers Vim.",
			"tags": []any{"editor"}, "entities": []any{"Vim"}, "importance": 0.7, "confidence": 0.9,
			"source_message_ids": []any{"message-1"},
		}},
		}}}}}

	encoded, err := runStructuredMemoryExtraction(context.Background(), stub, "prompt", "system", 2048)
	if err != nil {
		t.Fatal(err)
	}
	batch := ParseLongTermExtractionBatch(encoded)
	if len(batch.Memories) != 1 || batch.Memories[0].Content != "The user prefers Vim." {
		t.Fatalf("unexpected extraction batch: %#v", batch)
	}
	if stub.opts.RequiredTool != "ExtractMemoryBatch" || len(stub.opts.Tools) != 1 || !stub.opts.DisableThinking {
		t.Fatalf("structured extraction did not force its schema: %#v", stub.opts)
	}
}

func TestMemoryExtractionCompletionBudgetIsIndependentOfContextWindow(t *testing.T) {
	stub := &structuredMemoryExtractionStub{response: api.Response{ToolCalls: []map[string]any{{
		"name": "ExtractMemoryBatch", "input": map[string]any{"memories": []any{}},
	}}}}
	if _, err := runStructuredMemoryExtraction(context.Background(), stub, "prompt", "system",
		memoryExtractionCompletionMaxTokens); err != nil {
		t.Fatal(err)
	}
	if stub.opts.MaxTokens != 16*1024 {
		t.Fatalf("extraction completion max tokens = %d, want %d", stub.opts.MaxTokens, 16*1024)
	}
}

func TestRunStructuredMemoryExtractionAcceptsStrictJSONFallback(t *testing.T) {
	stub := &structuredMemoryExtractionStub{response: api.Response{Text: "```json\n{\"memories\":[]}\n```"}}
	encoded, err := runStructuredMemoryExtraction(context.Background(), stub, "prompt", "system", 0)
	if err != nil {
		t.Fatal(err)
	}
	if batch := ParseLongTermExtractionBatch(encoded); len(batch.Memories) != 0 {
		t.Fatalf("unexpected extraction batch: %#v", batch)
	}
	if stub.opts.MaxTokens != 4096 {
		t.Fatalf("default max tokens = %d, want 4096", stub.opts.MaxTokens)
	}
}

func TestRunStructuredMemoryExtractionRejectsUnstructuredText(t *testing.T) {
	stub := &structuredMemoryExtractionStub{response: api.Response{Text: "I found no durable memories."}}
	_, err := runStructuredMemoryExtraction(context.Background(), stub, "prompt", "system", 1024)
	if err == nil || !strings.Contains(err.Error(), "strict JSON object") {
		t.Fatalf("expected strict structured-output error, got %v", err)
	}

	stub.err = errors.New("provider unavailable")
	_, err = runStructuredMemoryExtraction(context.Background(), stub, "prompt", "system", 1024)
	if err == nil || err.Error() != "provider unavailable" {
		t.Fatalf("provider error was not preserved: %v", err)
	}
}
