package test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"LuminaCode/agent"
	"LuminaCode/config"
	coretools "LuminaCode/tools"
)

func TestCoordinatorRecoversFromUnauthorizedToolAndKeepsNotificationsScopedLikePython(t *testing.T) {
	var messageCounter atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		bodyText := string(raw)
		w.Header().Set("Content-Type", "text/event-stream")

		if strings.Contains(bodyText, "alpha task") && !strings.Contains(bodyText, "Spawn workers and summarize") {
			writeOpenAIStream(w, "alpha complete", 2, 2)
			return
		}
		if strings.Contains(bodyText, "beta task") && !strings.Contains(bodyText, "Spawn workers and summarize") {
			writeOpenAIStream(w, "beta complete", 2, 2)
			return
		}

		nextID := func(prefix string) string {
			return fmt.Sprintf("%s-%d", prefix, messageCounter.Add(1))
		}
		if !strings.Contains(bodyText, "Unknown tool: 'read_file'") {
			writeOpenAIToolCalls(w, nextID("msg"), []openAIToolCall{{
				ID: "tool-read", Name: "read_file", Arguments: map[string]any{"file_path": "README.md"},
			}}, 3, 2)
			return
		}

		taskIDs := coordinatorTaskIDs(bodyText)
		if len(taskIDs) == 0 {
			writeOpenAIToolCalls(w, nextID("msg"), []openAIToolCall{
				{ID: "tool-agent-alpha", Name: "Agent", Arguments: map[string]any{
					"description":       "Alpha worker",
					"prompt":            "alpha task",
					"subagent_type":     "general-purpose",
					"run_in_background": true,
				}},
				{ID: "tool-agent-beta", Name: "Agent", Arguments: map[string]any{
					"description":       "Beta worker",
					"prompt":            "beta task",
					"subagent_type":     "general-purpose",
					"run_in_background": true,
				}},
			}, 3, 2)
			return
		}

		if !strings.Contains(bodyText, `"timeout_seconds"`) {
			writeOpenAIToolCalls(w, nextID("msg"), []openAIToolCall{{
				ID: "tool-wait", Name: "TaskWait", Arguments: map[string]any{
					"task_ids": taskIDs, "timeout_seconds": 300,
				},
			}}, 3, 2)
			return
		}

		if !strings.Contains(bodyText, `"name":"TaskGet"`) {
			calls := make([]openAIToolCall, 0, len(taskIDs))
			for i, taskID := range taskIDs {
				calls = append(calls, openAIToolCall{
					ID: fmt.Sprintf("tool-get-%d", i+1), Name: "TaskGet", Arguments: map[string]any{"task_id": taskID},
				})
			}
			writeOpenAIToolCalls(w, nextID("msg"), calls, 3, 2)
			return
		}

		writeOpenAIStream(w, "Recovered after read_file rejection. Final summary: alpha complete; beta complete", 2, 2)
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.CWD = t.TempDir()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "custom-router-model"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 1000
	cfg.AutoMemoryEnabled = false
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false

	runtime := agent.NewAgentTaskRuntime()
	registry := coretools.NewToolRegistry(
		coretools.NewReadFileTool(),
		agent.NewAgentTool(),
		agent.NewTaskListTool(),
		agent.NewTaskGetTool(),
		agent.NewTaskWaitTool(),
		agent.NewTaskStopTool(),
		agent.NewSendMessageTool(),
	)
	parent := agent.NewAgentState()
	result, err := agent.NewAgentTool().Execute(context.Background(), coretools.ExecutionContext{
		"config":          cfg,
		"_registry":       registry,
		"task_runtime":    runtime,
		"scope_id":        "main",
		"current_task_id": "",
		"parent_state":    &parent,
	}, agent.AgentInput{
		Description:  "Coordinate workers",
		Prompt:       "Spawn workers and summarize their results.",
		SubagentType: "Coordinator",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Recovered after read_file rejection.") ||
		!strings.Contains(result, "alpha complete") ||
		!strings.Contains(result, "beta complete") {
		t.Fatalf("coordinator did not recover and summarize workers like Python:\n%s", result)
	}
	if strings.Contains(result, "<task-notification>") || strings.Contains(result, "<persisted-output>") {
		t.Fatalf("coordinator result leaked scoped runtime internals:\n%s", result)
	}
	if drained := runtime.DrainPendingNotifications("main"); len(drained) != 0 {
		t.Fatalf("main scope should not receive coordinator child notifications, got %#v", drained)
	}
	if records := runtime.ListTasks("main"); len(records) != 0 {
		t.Fatalf("foreground coordinator cleanup should remove child task records, got %#v", records)
	}
}

func TestSubagentStubbornUnauthorizedToolExitsAtLowMaxTurnsLikePython(t *testing.T) {
	var messageCounter atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeOpenAIToolCalls(w, fmt.Sprintf("msg-%d", messageCounter.Add(1)), []openAIToolCall{{
			ID: "tool-read", Name: "read_file", Arguments: map[string]any{"file_path": "README.md"},
		}}, 1, 1)
	}))
	defer server.Close()

	cfg := config.NewConfig()
	cfg.CWD = t.TempDir()
	cfg.APIKey = "test-key"
	cfg.APIBaseURL = server.URL
	cfg.APIModel = "custom-router-model"
	cfg.APIType = "openai_compatible"
	cfg.APIMaxTokens = 1000
	cfg.AutoMemoryEnabled = false
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false

	def := agent.GetAgentDefinition("Coordinator")
	def.MaxTurns = 3
	sub := agent.NewSubAgent(cfg, coretools.NewToolRegistry(agent.NewAgentTool()), def, nil, "", "Coordinator", coretools.ExecutionContext{})
	result, err := sub.Run(context.Background(), "Keep coordinating.")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "Sub-agent reached maximum turns (3).") {
		t.Fatalf("expected max-turns termination, got %q", result)
	}
	if strings.Contains(result, "Unknown tool: 'read_file'") {
		t.Fatalf("max-turns final text should not leak repeated unknown-tool errors: %q", result)
	}
}

type openAIToolCall struct {
	ID        string
	Name      string
	Arguments map[string]any
}

func writeOpenAIToolCalls(w http.ResponseWriter, messageID string, calls []openAIToolCall, inputTokens, outputTokens int) {
	toolCalls := make([]map[string]any, 0, len(calls))
	for index, call := range calls {
		args, _ := json.Marshal(call.Arguments)
		toolCalls = append(toolCalls, map[string]any{
			"index": index,
			"id":    call.ID,
			"function": map[string]any{
				"name":      call.Name,
				"arguments": string(args),
			},
		})
	}
	writeOpenAIData(w, map[string]any{
		"id": messageID,
		"choices": []map[string]any{{
			"delta": map[string]any{"tool_calls": toolCalls},
		}},
	})
	writeOpenAIData(w, map[string]any{
		"id": messageID,
		"choices": []map[string]any{{
			"delta":         map[string]any{},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]any{"prompt_tokens": inputTokens, "completion_tokens": outputTokens},
	})
	_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
}

func writeOpenAIData(w http.ResponseWriter, payload map[string]any) {
	data, _ := json.Marshal(payload)
	fmt.Fprintf(w, "data: %s\n\n", data)
}

func coordinatorTaskIDs(text string) []string {
	matches := regexp.MustCompile(`task_id:\s*([A-Za-z0-9_-]+)`).FindAllStringSubmatch(text, -1)
	seen := map[string]struct{}{}
	var ids []string
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		id := match[1]
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}
