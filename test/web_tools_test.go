package test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"LuminaCode/config"
	"LuminaCode/tools"
)

func TestWebSearchUsesSearxNGJSON(t *testing.T) {
	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("format"); got != "json" {
			t.Fatalf("format=%q want json", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{
				"title":    "Example",
				"url":      "https://example.com/research",
				"content":  "snippet",
				"engine":   "mock",
				"category": "general",
			}},
		})
	}))
	defer searx.Close()

	cfg := config.NewConfigForCWD(t.TempDir())
	cfg.WebSearchBaseURL = searx.URL
	cfg.ProjectRuntimeDir = t.TempDir()
	tool := tools.NewWebSearchTool()
	out, err := tool.Execute(context.Background(), tools.ExecutionContext{"config": cfg, "runtime_dir": cfg.ProjectRuntimeDir, "_session_id": "s1", "_agent_id": "a1"}, tools.WebSearchInput{Query: "lumina", NumResults: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"provider": "searxng"`) || !strings.Contains(out, `"https://example.com/research"`) {
		t.Fatalf("unexpected WebSearch output:\n%s", out)
	}
}

func TestWebFetchRequiresAndUsesSearxNGVerification(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Research Page</title></head><body><article><h1>Research Page</h1><p>Evidence grounded content for Lumina DeepResearch.</p></article></body></html>`))
	}))
	defer page.Close()

	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{
				"title":   "Research Page",
				"url":     page.URL,
				"content": "snippet",
			}},
		})
	}))
	defer searx.Close()

	cfg := config.NewConfigForCWD(t.TempDir())
	cfg.WebSearchBaseURL = searx.URL
	cfg.ProjectRuntimeDir = t.TempDir()
	cfg.WebFetchRequireSearch = true
	execCtx := tools.ExecutionContext{"config": cfg, "runtime_dir": cfg.ProjectRuntimeDir, "_session_id": "s1", "_agent_id": "a1"}
	if _, err := tools.NewWebSearchTool().Execute(context.Background(), execCtx, tools.WebSearchInput{Query: "research", NumResults: 1}); err != nil {
		t.Fatal(err)
	}
	out, err := tools.NewWebFetchTool().Execute(context.Background(), execCtx, tools.WebFetchInput{URL: page.URL, MaxChars: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"source_verified_by_searxng": true`) || !strings.Contains(out, "Evidence grounded content") {
		t.Fatalf("unexpected WebFetch output:\n%s", out)
	}
}

func TestWebSearchCacheScopeCanBeSharedAcrossTeamAgents(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Shared Source</title></head><body><article><p>Team shared cache evidence.</p></article></body></html>`))
	}))
	defer page.Close()

	searchCalls := 0
	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		searchCalls++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": []map[string]any{{
				"title":   "Shared Source",
				"url":     page.URL,
				"content": "snippet",
			}},
		})
	}))
	defer searx.Close()

	cfg := config.NewConfigForCWD(t.TempDir())
	cfg.WebSearchBaseURL = searx.URL
	cfg.ProjectRuntimeDir = t.TempDir()
	cfg.WebFetchRequireSearch = true
	searchCtx := tools.ExecutionContext{"config": cfg, "runtime_dir": cfg.ProjectRuntimeDir, "_session_id": "team-session", "_agent_id": "search-strategist", "web_search_scope": "team-session"}
	fetchCtx := tools.ExecutionContext{"config": cfg, "runtime_dir": cfg.ProjectRuntimeDir, "_session_id": "team-session", "_agent_id": "source-reader", "web_search_scope": "team-session"}
	if _, err := tools.NewWebSearchTool().Execute(context.Background(), searchCtx, tools.WebSearchInput{Query: "shared source", NumResults: 1}); err != nil {
		t.Fatal(err)
	}
	out, err := tools.NewWebFetchTool().Execute(context.Background(), fetchCtx, tools.WebFetchInput{URL: page.URL, MaxChars: 2000})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Team shared cache evidence") {
		t.Fatalf("shared cache fetch did not read page:\n%s", out)
	}
	if searchCalls != 1 {
		t.Fatalf("fetch should use shared cache without verification search, calls=%d", searchCalls)
	}
}

func TestWebSearchCacheDefaultsToAgentIsolation(t *testing.T) {
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<html><head><title>Private Source</title></head><body><article><p>Agent private cache evidence.</p></article></body></html>`))
	}))
	defer page.Close()

	searchCalls := 0
	searx := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		searchCalls++
		if searchCalls == 1 {
			_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{{"title": "Private Source", "url": page.URL, "content": "snippet"}}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []map[string]any{}})
	}))
	defer searx.Close()

	cfg := config.NewConfigForCWD(t.TempDir())
	cfg.WebSearchBaseURL = searx.URL
	cfg.ProjectRuntimeDir = t.TempDir()
	cfg.WebFetchRequireSearch = true
	searchCtx := tools.ExecutionContext{"config": cfg, "runtime_dir": cfg.ProjectRuntimeDir, "_session_id": "session", "_agent_id": "agent-a"}
	fetchCtx := tools.ExecutionContext{"config": cfg, "runtime_dir": cfg.ProjectRuntimeDir, "_session_id": "session", "_agent_id": "agent-b"}
	if _, err := tools.NewWebSearchTool().Execute(context.Background(), searchCtx, tools.WebSearchInput{Query: "private source", NumResults: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := tools.NewWebFetchTool().Execute(context.Background(), fetchCtx, tools.WebFetchInput{URL: page.URL, MaxChars: 2000})
	if err == nil || !strings.Contains(err.Error(), "SearxNG source verification failed") {
		t.Fatalf("expected agent-isolated cache verification failure, got %v", err)
	}
	if searchCalls != 2 {
		t.Fatalf("fetch should run its own verification search, calls=%d", searchCalls)
	}
}

func TestSearxNGScriptEnablesJSONFormat(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "setup-searxng.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{"formats:", "- json", "127.0.0.1:${SEARXNG_PORT}:8080"} {
		if !strings.Contains(text, want) {
			t.Fatalf("setup-searxng.sh missing %q", want)
		}
	}
}

func TestArxivMCPSetupUsesManagedAsyncRunner(t *testing.T) {
	root := repoRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "scripts", "setup-arxiv-mcp.sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		"run-arxiv-mcp.py",
		"asyncio.run(main())",
		`"args": [os.environ["RUNNER_FILE"]]`,
		"FastMCP description compatibility",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("setup-arxiv-mcp.sh missing %q", want)
		}
	}
}
