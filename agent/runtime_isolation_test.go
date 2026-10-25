package agent

import (
	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestIsolationChildToolIdentity(t *testing.T) {
	inherited := copyAgentExtraContext(coretools.ExecutionContext{"scope_id": "main", "_agent_id": "main", "_skill_agent_scope": "main"})
	inherited["scope_id"] = "child-1"
	sub := NewSubAgent(config.Config{}, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, "model", "worker", inherited)
	got := sub.buildExecutionContext()
	t.Logf("scope=%v history_agent=%v skill_scope=%v", got["scope_id"], got["_agent_id"], got["_skill_agent_scope"])
	if got["_agent_id"] != "child-1" || got["_skill_agent_scope"] != "child-1" {
		t.Fatal("child retained the parent's agent and skill identities")
	}
}

func TestIsolationSubagentInlinePolicyAcrossTurns(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(path, []byte("evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var models []string
	var toolsPerRequest []int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		models = append(models, fmt.Sprint(body["model"]))
		list, _ := body["tools"].([]any)
		toolsPerRequest = append(toolsPerRequest, len(list))
		n := len(models)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			args, _ := json.Marshal(map[string]any{"file_path": path})
			event := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-1", "type": "function", "function": map[string]any{"name": "read_file", "arguments": string(args)}}}}}}}
			raw, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", raw)
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()
	cfg := config.Config{CWD: dir, APIType: "openai_compatible", APIBaseURL: server.URL, APIKey: "test-key", APIModel: "base-model", APIMaxTokens: 128, ContextInputTokenLimit: 10000, MaxToolOutputChars: 50000, ProjectRuntimeDir: dir, SessionDir: dir}
	registry := coretools.NewToolRegistry()
	registry.Register(coretools.NewReadFileTool())
	registry.Register(coretools.NewWriteFileTool())
	sub := NewSubAgent(cfg, registry, AgentDef{Name: "worker", MaxTurns: 3}, nil, "base-model", "worker", coretools.ExecutionContext{})
	inline := map[string]any{"role": "user", "isMeta": true, "content": []map[string]any{{"type": "text", "text": "only use read_file"}}, "metadata": map[string]any{"source": skills.SkillInlineSource, skills.SkillInlineAllowedToolsKey: []string{"read_file"}, "lumina_skill_model": "restricted-model"}}
	state := &SubAgentSessionState{SystemPrompt: "system", ScopeID: "child", Messages: []map[string]any{inline, {"role": "user", "content": "read the evidence"}}}
	result := sub.ExecuteOneRequest(context.Background(), "read the evidence", state)
	mu.Lock()
	defer mu.Unlock()
	t.Logf("models=%v tool_counts=%v final=%s", models, toolsPerRequest, result.FinalText)
	if len(models) != 2 {
		t.Fatalf("expected two requests: %v", models)
	}
	if models[1] != "restricted-model" || toolsPerRequest[1] != 1 {
		t.Fatal("inline model/tool restrictions disappeared after context commit")
	}
}

func TestIsolationForkedSkillPermissionSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	dir := t.TempDir()
	cfg := config.Config{CWD: dir, SessionDir: dir, ProjectRuntimeDir: dir, APIType: "openai_compatible", APIBaseURL: server.URL, APIKey: "test-key", APIModel: "model", APIMaxTokens: 128, ContextInputTokenLimit: 16000}
	engine := NewCoreExecutionEngine(cfg, cluster.RuntimeIdentity{TenantID: "local"}, nil)
	defer engine.Shutdown()
	if err := engine.AgentRuntime.SetYolo(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	policy := engine.AgentRuntime.Snapshot().Permissions.Policy
	// SkillTool now receives spawn_permissions; the removed parent_state key is nil.
	_, _, _, err := engine.runForkSkill(context.Background(), skills.SkillSpec{CanonicalName: "forked"}, "task", "fork-child", nil, coretools.NewToolRegistry(), nil, coretools.ExecutionContext{"spawn_permissions": policy})
	if err != nil {
		t.Fatal(err)
	}
	child := engine.SessionRuntime.Snapshots()["fork-child"]
	t.Logf("parent_yolo=%v child_yolo=%v", policy.YoloMode, child.Permissions.Policy.YoloMode)
	if !child.Permissions.Policy.YoloMode {
		t.Fatal("forked skill lost the parent permission snapshot")
	}
}
