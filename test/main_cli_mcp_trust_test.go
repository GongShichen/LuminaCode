package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/mcp"
)

func TestMainSingleShotResolvesMCPTrustLikePythonRuntime(t *testing.T) {
	project := t.TempDir()
	home := t.TempDir()
	paths := initializeTestAppRoot(t, home)
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
	text := string(output)
	for _, forbidden := range []string{"\x1b[?1049h", "LuminaCode", "输入已锁定"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("single-shot CLI should run headless, but output contained %q:\n%s", forbidden, output)
		}
	}
	for _, want := range []string{"Permission needed for mcp-project-trust", "ok"} {
		if !strings.Contains(text, want) {
			t.Fatalf("single-shot CLI should use headless permission flow, missing %q output:\n%s", want, output)
		}
	}

	projectPaths, err := paths.ForProject(project)
	if err != nil {
		t.Fatal(err)
	}
	trustedPath := projectPaths.MCPTrustFile
	data, err := os.ReadFile(trustedPath)
	if err != nil {
		t.Fatalf("expected trusted MCP file after CLI approval: %v\noutput:\n%s", err, output)
	}
	if _, err := os.Stat(filepath.Join(project, ".Lumina", "CONFIG", "trusted_mcp.json")); !os.IsNotExist(err) {
		t.Fatalf("trusted MCP runtime file should not be written under project root, stat err=%v", err)
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

func TestMainInteractiveGoTUIIsDisabled(t *testing.T) {
	home := t.TempDir()
	repoRoot := filepath.Dir(mustGetwd(t))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "run", ".", "--api-key", "test-key", "--base-url", "http://127.0.0.1", "--api-type", "openai_compatible", "--model", "custom-model", "--bare")
	cmd.Dir = repoRoot
	cmd.Env = mainCLITestEnv(t, home)
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("go run timed out; output:\n%s", output)
	}
	if err == nil {
		t.Fatalf("interactive Go TUI should be disabled, output:\n%s", output)
	}
	if !strings.Contains(string(output), "interactive Go TUI has been removed") ||
		!strings.Contains(string(output), "TypeScript frontend command 'lumina'") {
		t.Fatalf("disabled interactive path should point users to TS frontend, output:\n%s", output)
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
	paths := initializeTestAppRoot(t, home)
	return append(os.Environ(),
		"HOME="+home,
		"LUMINA_APP_ROOT="+paths.Root,
		"GOMODCACHE="+goEnv(t, "GOMODCACHE"),
		"GOCACHE="+goEnv(t, "GOCACHE"),
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
