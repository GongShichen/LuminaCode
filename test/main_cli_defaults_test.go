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
)

func TestMainCLIOmitsRequestMaxTokensWhenFlagOmitted(t *testing.T) {
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read request body: %v", err)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Errorf("invalid request body: %v body=%s", err, bodyBytes)
			return
		}
		select {
		case requests <- body:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	testDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Dir(testDir)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(
		ctx,
		"go",
		"run",
		".",
		"-p", "hello",
		"-bare",
		"-api-key", "test-key",
		"-base-url", server.URL,
		"-api-type", "openai_compatible",
		"-model", "custom-router-model",
	)
	cmd.Dir = rootDir
	cmd.Env = isolatedLuminaHomeEnv(t)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go run failed: %v\n%s", err, output)
	}

	select {
	case body := <-requests:
		if _, ok := body["max_tokens"]; ok {
			t.Fatalf("request should omit max_tokens, got %#v", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive a request")
	}
}

func TestMainCLIRunsFromArbitraryDirectoryWithBundledDefaults(t *testing.T) {
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("failed to read request body: %v", err)
			return
		}
		var body map[string]any
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Errorf("invalid request body: %v body=%s", err, bodyBytes)
			return
		}
		select {
		case requests <- body:
		default:
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"msg-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	testDir, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	rootDir := filepath.Dir(testDir)
	bin := testBinaryPath(filepath.Join(t.TempDir(), "lumina-test"))
	buildCtx, buildCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer buildCancel()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", bin, ".")
	build.Dir = rootDir
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, output)
	}

	runCtx, runCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer runCancel()
	cmd := exec.CommandContext(
		runCtx,
		bin,
		"-p", "hello",
		"-bare",
		"-api-key", "test-key",
		"-base-url", server.URL,
		"-api-type", "openai_compatible",
		"-model", "custom-router-model",
	)
	cmd.Dir = t.TempDir()
	cmd.Env = isolatedLuminaHomeEnv(t)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lumina binary failed from arbitrary dir: %v\n%s", err, output)
	}

	select {
	case body := <-requests:
		if _, ok := body["max_tokens"]; ok {
			t.Fatalf("request should omit max_tokens, got %#v", body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not receive a request")
	}
}

func isolatedLuminaHomeEnv(t *testing.T) []string {
	t.Helper()
	env := append(os.Environ(), "HOME="+t.TempDir(), "LUMINA_RESOURCE_ROOT=", "LUMINA_HOME=")
	for _, key := range []string{"GOMODCACHE", "GOCACHE"} {
		out, err := exec.Command("go", "env", key).Output()
		if err != nil {
			continue
		}
		value := strings.TrimSpace(string(out))
		if value != "" {
			env = append(env, key+"="+value)
		}
	}
	return env
}
