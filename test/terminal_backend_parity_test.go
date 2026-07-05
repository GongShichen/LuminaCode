package test

import (
	"bytes"
	"strings"
	"testing"

	"LuminaCode/agent"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"
	"LuminaCode/ui"
)

func TestTerminalRendererBackendPermissionPromptsMatchMainRuntimePath(t *testing.T) {
	var out bytes.Buffer
	backend := ui.NewTerminalRendererBackend(strings.NewReader("a\n"), &out, &out)

	answer := backend.AskPermission(map[string]any{
		"name": "mcp-project-trust",
		"input": map[string]any{
			"command": "docs: node server.js",
		},
	}, true)

	if answer != "always" {
		t.Fatalf("expected normalized always answer, got %q", answer)
	}
	if !strings.Contains(out.String(), "Permission needed to trust project MCP servers: docs: node server.js") {
		t.Fatalf("unexpected MCP trust prompt:\n%s", out.String())
	}

	out.Reset()
	backend = ui.NewTerminalRendererBackend(strings.NewReader("y\n"), &out, &out)
	answer = backend.AskPermission(map[string]any{
		"name": "skill-shell:audit",
		"input": map[string]any{
			"command": "npm test",
		},
	}, true)

	if answer != "once" {
		t.Fatalf("expected normalized once answer, got %q", answer)
	}
	if !strings.Contains(out.String(), "Permission needed for skill shell command in audit: npm test") {
		t.Fatalf("unexpected skill shell prompt:\n%s", out.String())
	}

	out.Reset()
	backend = ui.NewTerminalRendererBackend(strings.NewReader("n\n"), &out, &out)
	answer = backend.AskPermission(coretools.ToolCall{
		Name:  "write_file",
		Input: map[string]any{"file_path": "/tmp/demo.txt"},
	}, false)

	if answer != "deny" {
		t.Fatalf("expected normalized deny answer, got %q", answer)
	}
	if !strings.Contains(out.String(), "Permission needed for write_file. Allow once?") {
		t.Fatalf("unexpected tool prompt:\n%s", out.String())
	}
}

func TestTerminalRendererBackendAcceptsSkillShellRequestShapeViaRuntimePrompt(t *testing.T) {
	var out bytes.Buffer
	req := skills.SkillShellPermissionRequest{SkillName: "demo", Command: "make test"}
	rt := ui.NewUiRuntime(nil, ui.NewTerminalRendererBackend(strings.NewReader("y\n"), &out, &out))
	event := ui.UiEvent{
		Type: "permission_requested",
		Metadata: map[string]any{
			"skill_shell_request": req,
			"dangerous":           true,
			"risk":                "high",
		},
	}

	if got := rt.HandlePermissionEvent(event); got != "once" {
		t.Fatalf("expected runtime/backend permission answer once, got %q", got)
	}
	if !strings.Contains(out.String(), "Permission needed for skill shell command in demo: make test") {
		t.Fatalf("runtime should route skill shell prompt through terminal backend, got:\n%s", out.String())
	}
	if len(rt.Frame.PermissionAudit) != 1 || rt.Frame.PermissionAudit[0]["decision"] != "once" {
		t.Fatalf("runtime should still record audit through backend path, got %#v", rt.Frame.PermissionAudit)
	}
}

func TestTerminalRendererBackendUsesRegistryBackfillForToolRenderingLikePython(t *testing.T) {
	var out bytes.Buffer
	registry := coretools.NewToolRegistry(coretools.NewReadFileTool())
	backend := ui.NewTerminalRendererBackend(strings.NewReader(""), &out, &out)
	backend.SetRegistry(registry)
	backend.SetExecContext(coretools.ExecutionContext{"cwd": "/tmp/project"})

	backend.RenderEvent(agent.NewStreamEvent("tool_call", "read_file", map[string]any{
		"id":    "tool-1",
		"input": map[string]any{"file_path": "src/main.go"},
	}))
	if out.Len() != 0 {
		t.Fatalf("tool call should be buffered until a non-tool event, got %q", out.String())
	}
	backend.RenderEvent(agent.NewStreamEvent("tool_result", "", map[string]any{
		"tool_use_id": "tool-1",
		"tool_name":   "read_file",
		"result":      "line1\nline2",
		"is_error":    false,
	}))

	rendered := out.String()
	if !strings.Contains(rendered, "[tool] 📖 Read main.go") {
		t.Fatalf("tool use should render through registry/backfill, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "[tool result] Read 2 lines") {
		t.Fatalf("tool result should render through tool renderer, got:\n%s", rendered)
	}
}

func TestTerminalRendererBackendNonTTYFallbackInputStillMatchesPythonBasicMode(t *testing.T) {
	var out bytes.Buffer
	backend := ui.NewTerminalRendererBackend(strings.NewReader("  hello terminal  \n"), &out, &out)
	text, ok := backend.GetInput(agent.NewAgentState())
	if !ok || text != "hello terminal" {
		t.Fatalf("fallback input should trim and return text, ok=%v text=%q", ok, text)
	}
	if !strings.Contains(out.String(), "> ") {
		t.Fatalf("fallback input should still render normal prompt, got %q", out.String())
	}

	out.Reset()
	state := agent.NewAgentState()
	state.PermissionState.YoloMode = true
	backend = ui.NewTerminalRendererBackend(strings.NewReader("go\n"), &out, &out)
	text, ok = backend.GetInput(&state)
	if !ok || text != "go" {
		t.Fatalf("yolo fallback input should return text, ok=%v text=%q", ok, text)
	}
	if !strings.Contains(out.String(), "! ") {
		t.Fatalf("fallback input should render yolo prompt, got %q", out.String())
	}
}

func TestTerminalRendererBackendPickFromListFallbackMatchesPython(t *testing.T) {
	values := make([][2]string, 0, 25)
	for i := 0; i < 25; i++ {
		values = append(values, [2]string{
			"id" + string(rune('a'+i)),
			"Session " + string(rune('a'+i)),
		})
	}

	var out bytes.Buffer
	backend := ui.NewTerminalRendererBackend(strings.NewReader("22\n"), &out, &out)
	selected := backend.PickFromList("Resume session", values)
	if selected == nil || *selected != "idv" {
		t.Fatalf("fallback picklist should accept any listed number like Python, got %#v", selected)
	}
	if !strings.Contains(out.String(), "Resume session") ||
		!strings.Contains(out.String(), "22. Session v") ||
		!strings.Contains(out.String(), "25. Session y") {
		t.Fatalf("fallback picklist should print the full numbered list, got:\n%s", out.String())
	}

	cancelCases := []struct {
		name  string
		input string
	}{
		{name: "empty", input: "\n"},
		{name: "non-numeric", input: "abc\n"},
		{name: "out-of-range", input: "99\n"},
	}
	for _, tc := range cancelCases {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			backend := ui.NewTerminalRendererBackend(strings.NewReader(tc.input), &output, &output)
			if got := backend.PickFromList("Pick", values[:1]); got != nil {
				t.Fatalf("expected cancel for %s input, got %q", tc.name, *got)
			}
		})
	}

	out.Reset()
	backend = ui.NewTerminalRendererBackend(strings.NewReader("1\n"), &out, &out)
	if got := backend.PickFromList("Empty", nil); got != nil || out.Len() != 0 {
		t.Fatalf("empty picklist should return before rendering, got=%#v out=%q", got, out.String())
	}
}
