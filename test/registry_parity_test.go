package test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coretools "LuminaCode/tools"
)

type registryEchoTool struct {
	coretools.BaseTool
}

func newRegistryEchoTool() *registryEchoTool {
	return &registryEchoTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:              "echo_tool",
		Description:       "echo test tool",
		InputPrototype:    map[string]any{},
		Aliases:           []string{"echo_alias"},
		DeprecatedAliases: map[string]string{"legacy_echo": "legacy_echo is deprecated; use echo_tool"},
		ReadOnly:          coretools.BoolPtr(true),
		ConcurrencySafe:   coretools.BoolPtr(true),
		Destructive:       coretools.BoolPtr(false),
	}}}
}

func (t *registryEchoTool) Execute(_ context.Context, _ coretools.ExecutionContext, _ any) (string, error) {
	return "ok", nil
}

type registryTimeoutTool struct {
	coretools.BaseTool
}

func newRegistryTimeoutTool() *registryTimeoutTool {
	return &registryTimeoutTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:            "slow_tool",
		Description:     "slow test tool",
		InputPrototype:  map[string]any{},
		TimeoutSeconds:  0.01,
		ReadOnly:        coretools.BoolPtr(true),
		ConcurrencySafe: coretools.BoolPtr(true),
		Destructive:     coretools.BoolPtr(false),
	}}}
}

func (t *registryTimeoutTool) Execute(_ context.Context, _ coretools.ExecutionContext, _ any) (string, error) {
	time.Sleep(100 * time.Millisecond)
	return "late", nil
}

type registryPanicTool struct {
	coretools.BaseTool
}

func newRegistryPanicTool() *registryPanicTool {
	return &registryPanicTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:            "panic_tool",
		Description:     "panic test tool",
		InputPrototype:  map[string]any{},
		ReadOnly:        coretools.BoolPtr(true),
		ConcurrencySafe: coretools.BoolPtr(true),
		Destructive:     coretools.BoolPtr(false),
	}}}
}

func (t *registryPanicTool) Execute(_ context.Context, _ coretools.ExecutionContext, _ any) (string, error) {
	panic("boom")
}

func TestToolRegistryDeprecatedAliasPrependsPythonWarning(t *testing.T) {
	registry := coretools.NewToolRegistry(newRegistryEchoTool())
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "echo-1", Name: "legacy_echo", Input: map[string]any{},
	}, coretools.ExecutionContext{})
	expected := "[Deprecation warning: legacy_echo is deprecated; use echo_tool]\n\nok"
	if result.IsError || result.Content != expected {
		t.Fatalf("unexpected deprecated alias result: error=%v content=%q", result.IsError, result.Content)
	}
}

func TestToolRegistryTimeoutMessageMatchesPythonShape(t *testing.T) {
	registry := coretools.NewToolRegistry(newRegistryTimeoutTool())
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "slow-1", Name: "slow_tool", Input: map[string]any{},
	}, coretools.ExecutionContext{})
	if !result.IsError {
		t.Fatalf("expected timeout error, got %q", result.Content)
	}
	for _, want := range []string{
		"The operation took too long to complete. Consider:",
		"  - Breaking the task into smaller steps.",
		"  - Using a more targeted query or path.",
		"  - Increasing the timeout if this is expected to be slow.",
	} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("timeout message missing %q: %q", want, result.Content)
		}
	}
}

func TestToolRegistryPanicBecomesUnexpectedErrorLikePythonException(t *testing.T) {
	registry := coretools.NewToolRegistry(newRegistryPanicTool())
	result := registry.Execute(context.Background(), coretools.ToolCall{
		ID: "panic-1", Name: "panic_tool", Input: map[string]any{},
	}, coretools.ExecutionContext{})
	if !result.IsError ||
		!strings.Contains(result.Content, "Unexpected error executing 'panic_tool': boom") ||
		!strings.Contains(result.Content, "Do NOT repeat the exact same call") {
		t.Fatalf("panic should be returned as Python-style tool error, got error=%v content=%q", result.IsError, result.Content)
	}
}

func TestToolRegistryAPISchemaFiltersMatchPythonOptions(t *testing.T) {
	disabled := newRegistryEchoTool()
	disabled.Spec.Name = "disabled_tool"
	disabled.Spec.Enabled = coretools.BoolPtr(false)
	writeOnly := newRegistryEchoTool()
	writeOnly.Spec.Name = "write_tool"
	writeOnly.Spec.ReadOnly = coretools.BoolPtr(false)

	registry := coretools.NewToolRegistry(newRegistryEchoTool(), disabled, writeOnly)
	defaultSchemas := registry.GetAPISchemas()
	if schemaNames(defaultSchemas)["disabled_tool"] {
		t.Fatalf("default schemas should exclude disabled tools: %#v", defaultSchemas)
	}

	readOnlySchemas := registry.GetAPISchemasFiltered(true, map[string]struct{}{"echo_tool": {}}, true)
	names := schemaNames(readOnlySchemas)
	if names["echo_tool"] || names["write_tool"] || names["disabled_tool"] {
		t.Fatalf("read-only deny filter returned unexpected schemas: %#v", readOnlySchemas)
	}
}

func TestToolRegistryListToolsPreservesInsertionButSchemasSortLikePython(t *testing.T) {
	first := newRegistryEchoTool()
	first.Spec.Name = "z_tool"
	second := newRegistryEchoTool()
	second.Spec.Name = "a_tool"
	registry := coretools.NewToolRegistry(first, second)

	listed := registry.ListTools()
	if len(listed) != 2 || listed[0].Name() != "z_tool" || listed[1].Name() != "a_tool" {
		t.Fatalf("list_tools should preserve Python insertion order, got %#v", registryToolNames(listed))
	}
	schemas := registry.GetAPISchemas()
	if got := schemaNameList(schemas); len(got) != 2 || got[0] != "a_tool" || got[1] != "z_tool" {
		t.Fatalf("get_api_schemas should sort by name like Python, got %#v", got)
	}
}

func TestToolRegistryGetReadOnlyToolsMatchesPythonInsertionOrder(t *testing.T) {
	readA := newRegistryEchoTool()
	readA.Spec.Name = "read_a"
	write := newRegistryEchoTool()
	write.Spec.Name = "write_mid"
	write.Spec.ReadOnly = coretools.BoolPtr(false)
	readB := newRegistryEchoTool()
	readB.Spec.Name = "read_b"
	registry := coretools.NewToolRegistry(readA, write, readB)

	got := registryToolNames(registry.GetReadOnlyTools())
	if strings.Join(got, ",") != "read_a,read_b" {
		t.Fatalf("get_read_only_tools should preserve Python active insertion order and filter read-only tools, got %#v", got)
	}
}

func TestToolRegistryEnrichForRenderUsesBackfillWithoutMutatingInputLikePython(t *testing.T) {
	dir := t.TempDir()
	registry := coretools.NewToolRegistry(coretools.NewReadFileTool())
	input := map[string]any{"file_path": "src/main.go", "offset": 2}
	enriched := registry.EnrichForRender(coretools.ToolCall{
		ID: "read-1", Name: "read_file", Input: input,
	}, coretools.ExecutionContext{"cwd": dir})
	readInput, ok := enriched.(coretools.ReadFileInput)
	if !ok {
		t.Fatalf("expected enriched read input, got %#v", enriched)
	}
	wantPath := filepath.Join(dir, "src", "main.go")
	if readInput.FilePath != wantPath || readInput.Offset != 2 {
		t.Fatalf("unexpected enriched input: %#v want path %s", readInput, wantPath)
	}
	if input["file_path"] != "src/main.go" {
		t.Fatalf("enrich_for_render must not mutate original tool input, got %#v", input)
	}

	fallback := registry.EnrichForRender(coretools.ToolCall{
		ID: "missing-1", Name: "missing", Input: map[string]any{"x": "y"},
	}, coretools.ExecutionContext{})
	if !strings.Contains(fallback.(fmt.Stringer).String(), "x:y") {
		t.Fatalf("unknown tools should return fallback-like input, got %#v", fallback)
	}
}

func schemaNames(schemas []map[string]any) map[string]bool {
	names := map[string]bool{}
	for _, schema := range schemas {
		name, _ := schema["name"].(string)
		names[name] = true
	}
	return names
}

func schemaNameList(schemas []map[string]any) []string {
	names := make([]string, 0, len(schemas))
	for _, schema := range schemas {
		name, _ := schema["name"].(string)
		names = append(names, name)
	}
	return names
}

func registryToolNames(tools []coretools.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		names = append(names, tool.Name())
	}
	return names
}
