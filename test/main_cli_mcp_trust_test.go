package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/mcp"
	"LuminaCode/session"
)

func TestMainSingleShotResolvesMCPTrustLikePythonRuntime(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, ".mcp.json"), []byte(`{"mcpServers":{"docs":{"command":"fake-mcp","args":["--stdio"]}}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	configs := mcp.LoadProjectMCPConfig(project)
	if len(configs) != 1 {
		t.Fatalf("expected project MCP config, got %#v", configs)
	}
	fingerprint := configs[0].Fingerprint()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected API path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	repoRoot := filepath.Dir(mustGetwd(t))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", ".", "-p", "hello", "--cwd", project, "--api-key", "test-key", "--base-url", server.URL, "--api-type", "openai_compatible", "--model", "custom-model", "--bare")
	cmd.Dir = repoRoot
	cmd.Env = mainCLITestEnv(t, home)
	cmd.Stdin = strings.NewReader("y\n")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("go run timed out; output:\n%s", output)
	}
	if err != nil {
		t.Fatalf("go run failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), "Permission needed to trust project MCP servers") {
		t.Fatalf("single-shot CLI should prompt for MCP trust, output:\n%s", output)
	}

	data, err := os.ReadFile(filepath.Join(project, ".Lumina", "CONFIG", "trusted_mcp.json"))
	if err != nil {
		t.Fatalf("expected trusted MCP file after CLI approval: %v\noutput:\n%s", err, output)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("invalid trusted MCP JSON: %v", err)
	}
	servers, _ := payload["servers"].(map[string]any)
	if servers["docs"] != fingerprint {
		t.Fatalf("trusted MCP fingerprint mismatch: got %#v want %q", payload, fingerprint)
	}
}

func TestMainREPLResumeWithoutIDUsesPickListLikePython(t *testing.T) {
	home := t.TempDir()
	store := session.NewStore(filepath.Join(home, ".Lumina", "sessions"))
	state := agent.NewAgentState()
	state.Messages = []map[string]any{{"role": "user", "content": "hello"}}
	state.TurnCount = 2
	if err := store.SaveStateWithRecovery("sess-one", &state, nil, nil); err != nil {
		t.Fatal(err)
	}

	repoRoot := filepath.Dir(mustGetwd(t))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", ".", "--api-key", "test-key", "--base-url", "http://127.0.0.1", "--api-type", "openai_compatible", "--model", "custom-model", "--bare")
	cmd.Dir = repoRoot
	cmd.Env = mainCLITestEnv(t, home)
	cmd.Stdin = strings.NewReader("/resume\n1\n/exit\n")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("go run timed out; output:\n%s", output)
	}
	if err != nil {
		t.Fatalf("go run failed: %v\n%s", err, output)
	}
	text := string(output)
	if !strings.Contains(text, "Resume Session") ||
		!strings.Contains(text, "sess-one  (1 msgs, 2 turns)") ||
		!strings.Contains(text, "Resumed session sess-one (1 messages, 2 turns)") {
		t.Fatalf("REPL /resume should use picklist and restore selected state, output:\n%s", output)
	}
}

func TestMainREPLStartsWithoutActiveStateLikePython(t *testing.T) {
	home := t.TempDir()
	repoRoot := filepath.Dir(mustGetwd(t))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", ".", "--api-key", "test-key", "--base-url", "http://127.0.0.1", "--api-type", "openai_compatible", "--model", "custom-model", "--bare")
	cmd.Dir = repoRoot
	cmd.Env = mainCLITestEnv(t, home)
	cmd.Stdin = strings.NewReader("/cost\n/save\n/yolo\n/compact\n/exit\n")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("go run timed out; output:\n%s", output)
	}
	if err != nil {
		t.Fatalf("go run failed: %v\n%s", err, output)
	}
	text := string(output)
	if count := strings.Count(text, "No active session"); count != 4 {
		t.Fatalf("fresh REPL slash commands should see no active state before first user turn, count=%d output:\n%s", count, output)
	}
	if strings.Contains(text, "Session Cost") || strings.Contains(text, "YOLO mode:") || strings.Contains(text, "No compression needed.") {
		t.Fatalf("fresh REPL should not create an empty active state for slash commands, output:\n%s", output)
	}
	if !strings.Contains(text, "Goodbye.") {
		t.Fatalf("/exit should print Python-style goodbye, output:\n%s", output)
	}
}

func TestMainREPLMCPCommandListsRegistryToolsLikePython(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	mcpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		msg := mcp.ParseMessage(string(body))
		switch req := msg.(type) {
		case mcp.JSONRPCNotification:
			w.WriteHeader(http.StatusNoContent)
		case mcp.JSONRPCRequest:
			result := map[string]any{}
			switch req.Method {
			case "initialize":
				result = map[string]any{"serverInfo": map[string]any{"name": "docs"}, "capabilities": map[string]any{}}
			case "tools/list":
				result = map[string]any{"tools": []map[string]any{{
					"name":        "Echo.Tool",
					"description": "Echo text",
					"inputSchema": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
				}}}
			default:
				result = map[string]any{}
			}
			_, _ = w.Write([]byte(mcp.SerializeMessage(mcp.JSONRPCResponse{JSONRPC: "2.0", ID: req.ID, Result: result})))
		default:
			t.Fatalf("unexpected MCP message: %#v", msg)
		}
	}))
	defer mcpServer.Close()

	config := `{"mcpServers":{"docs":{"url":"` + mcpServer.URL + `"}}}`
	if err := os.WriteFile(filepath.Join(project, ".mcp.json"), []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	configs := mcp.LoadProjectMCPConfig(project)
	if len(configs) != 1 {
		t.Fatalf("expected project MCP config, got %#v", configs)
	}
	if err := mcp.SaveTrustedMCP(project, map[string]string{"docs": configs[0].Fingerprint()}); err != nil {
		t.Fatal(err)
	}

	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer apiServer.Close()

	repoRoot := filepath.Dir(mustGetwd(t))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", ".", "--cwd", project, "--api-key", "test-key", "--base-url", apiServer.URL, "--api-type", "openai_compatible", "--model", "custom-model", "--bare")
	cmd.Dir = repoRoot
	cmd.Env = mainCLITestEnv(t, home)
	cmd.Stdin = strings.NewReader("hello\n/mcp\n/exit\n")
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("go run timed out; output:\n%s", output)
	}
	if err != nil {
		t.Fatalf("go run failed: %v\n%s", err, output)
	}
	text := string(output)
	for _, want := range []string{
		"Registered MCP Tools",
		"mcp__docs__echo_tool",
		"dynamic",
		"mcp_list_resources",
		"resource",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("/mcp output missing %q:\n%s", want, output)
		}
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

func mainCLITestEnv(t *testing.T, home string) []string {
	t.Helper()
	return append(os.Environ(),
		"HOME="+home,
		"GOMODCACHE="+goEnv(t, "GOMODCACHE"),
		"GOCACHE="+goEnv(t, "GOCACHE"),
		"LUMINA_UI_BACKEND=legacy_terminal",
	)
}

func goEnv(t *testing.T, key string) string {
	t.Helper()
	output, err := exec.Command("go", "env", key).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(output))
}
