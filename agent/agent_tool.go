package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"LuminaCode/config"
	coretools "LuminaCode/tools"

	"github.com/google/uuid"
)

const MaxBackgroundWorkersPerScope = 32

type AgentInput struct {
	Description     string `json:"description" jsonschema:"description=A short description of the delegated task"`
	Prompt          string `json:"prompt" jsonschema:"description=Detailed instructions for the sub-agent"`
	SubagentType    string `json:"subagent_type,omitempty" jsonschema:"description=Agent type to spawn. Available types include 'general-purpose' 'Explore' 'Plan' 'Coordinator' and 'docs-lookup'."`
	Model           string `json:"model,omitempty" jsonschema:"description=Optional model override. If omitted inherit from parent config."`
	RunInBackground bool   `json:"run_in_background,omitempty" jsonschema:"description=If true spawn the worker in background and return its task_id immediately."`
	WorkerLabel     string `json:"worker_label,omitempty" jsonschema:"description=Optional human-readable worker label. task_id is still system-generated."`
	Reusable        bool   `json:"reusable,omitempty" jsonschema:"description=If true keep the background worker idle for SendMessage reuse after it finishes."`
	Isolation       string `json:"isolation,omitempty" jsonschema:"description=Filesystem isolation mode: null shared filesystem or 'worktree' git worktree."`
	TimeoutSeconds  any    `json:"timeout_seconds,omitempty" jsonschema:"description=Optional sub-agent work timeout in seconds. Defaults to 300. When reached, the sub-agent is asked to stop using tools and return a final answer from known context."`
}

type AgentTool struct{ coretools.BaseTool }

func NewAgentTool() *AgentTool {
	return &AgentTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:            "Agent",
		Description:     "Launch a new agent to handle complex, multi-step tasks.\n\nAvailable agent types:\n- general-purpose: all tools.\n- Explore: read-only code search.\n- Plan: read-only implementation planning.\n- Coordinator: task orchestration only. Always use run_in_background=true when spawning workers. Never use sync mode. Use TaskWait to wait for workers. Never busy-poll with repeated TaskList calls. Use TaskList and TaskGet for inspection and result retrieval only. After all target workers settle, synthesize their results into one final answer. Workers are automatically isolated via git worktrees.\n- docs-lookup: documentation lookup.",
		InputPrototype:  AgentInput{},
		TimeoutSeconds:  AgentToolDefaultHardTimeoutSeconds,
		ReadOnly:        coretools.BoolPtr(false),
		ConcurrencySafe: coretools.BoolPtr(true),
		Destructive:     coretools.BoolPtr(false),
	}}}
}

func (t *AgentTool) ValidateInput(_ coretools.ExecutionContext, input any) (bool, string) {
	in := derefAgentInput(input)
	if strings.TrimSpace(in.Description) == "" {
		return false, "description must not be empty."
	}
	if strings.TrimSpace(in.Prompt) == "" {
		return false, "prompt must not be empty."
	}
	if _, msg := agentTimeoutSeconds(in); msg != "" {
		return false, msg
	}
	return true, ""
}

func (t *AgentTool) TimeoutForInput(input any) time.Duration {
	seconds, _ := agentTimeoutSeconds(derefAgentInput(input))
	return time.Duration(seconds+SubagentFinalizeSeconds+AgentToolHardTimeoutGraceSecs) * time.Second
}

func (t *AgentTool) Execute(ctx context.Context, execCtx coretools.ExecutionContext, input any) (string, error) {
	in := derefAgentInput(input)
	if in.SubagentType == "" {
		in.SubagentType = "general-purpose"
	}
	timeoutSeconds, timeoutErr := agentTimeoutSeconds(in)
	if timeoutErr != "" {
		return "Error: " + timeoutErr, nil
	}
	cfg, ok := configFromContext(execCtx)
	if !ok {
		return "Error: AgentTool requires a Config in the execution context.", nil
	}
	baseRegistry, _ := execCtx["_registry"].(*coretools.ToolRegistry)
	if baseRegistry == nil {
		return "Error: No tool registry available in context.", nil
	}
	taskRuntime, _ := execCtx["task_runtime"].(*AgentTaskRuntime)
	if taskRuntime == nil {
		return "Error: AgentTool requires task runtime support in the execution context.", nil
	}
	parentState, _ := execCtx["parent_state"].(*AgentState)
	definition := GetAgentDefinition(in.SubagentType)
	model := in.Model
	if model == "" {
		model = definition.Model
	}
	if model == "" {
		model = cfg.APIModel
	}
	filteredRegistry := BuildFilteredRegistry(baseRegistry, definition)
	if len(filteredRegistry.ListTools()) == 0 {
		return fmt.Sprintf("Error: No tools available for agent type '%s'. Check the allowlist/denylist.", in.SubagentType), nil
	}
	parentScopeID := stringFromContext(execCtx, "scope_id", "main")
	parentTaskID := stringFromContext(execCtx, "current_task_id", "")
	callerAgentType := stringFromContext(execCtx, "current_agent_type", "")
	workerLabel := in.WorkerLabel
	if workerLabel == "" {
		workerLabel = in.Description
	}
	if callerAgentType == "Coordinator" && !in.RunInBackground {
		return "Error: Coordinator must always spawn workers with run_in_background=true. Sync mode is not allowed.", nil
	}
	isolationMode := in.Isolation
	if isolationMode == "" {
		isolationMode = definition.Isolation
	}
	childCfg := cfg
	worktreePath := ""
	if isolationMode == "worktree" {
		if repoRoot := FindGitRoot(cfg.CWD); repoRoot != "" {
			result, err := CreateWorktree(ctx, repoRoot, cfg.WorktreeBaseRef, in.SubagentType, cfg.WorktreeDir)
			if err == nil && result.WorktreePath != "" {
				worktreePath = result.WorktreePath
			}
		}
	}
	if in.RunInBackground {
		siblings := taskRuntime.ListTasks(parentScopeID)
		active := 0
		for _, record := range siblings {
			if _, terminal := terminalTaskStatuses[record.Status]; !terminal {
				active++
				if record.WorkerLabel == workerLabel {
					return "Error: A worker with the same worker_label already exists in this scope. Choose a unique worker_label.", nil
				}
			}
		}
		if active >= MaxBackgroundWorkersPerScope {
			return fmt.Sprintf("Error: Too many active background workers in this scope. Limit is %d.", MaxBackgroundWorkersPerScope), nil
		}
		extraContext := copyAgentExtraContext(execCtx)
		extraContext["subagent_timeout_seconds"] = timeoutSeconds
		if worktreePath != "" {
			extraContext["worktree_path"] = worktreePath
			extraContext["worktree_cwd"] = worktreePath
		}
		record := taskRuntime.SpawnWorker(ctx, childCfg, filteredRegistry, definition, parentState, in.Description, in.Prompt, in.SubagentType, model, workerLabel, in.Reusable, parentScopeID, parentTaskID, extraContext)
		return fmt.Sprintf("Background worker launched.\ntask_id: %s\nworker_label: %s\nstatus: %s\nreusable: %t", record.TaskID, record.WorkerLabel, record.Status, record.Reusable), nil
	}
	subagentScopeID := "scope-" + uuid.NewString()[:12]
	taskID := "subagent-" + in.SubagentType + "-" + uuid.NewString()[:8]
	record := taskRuntime.RegisterForegroundTask(taskID, parentTaskID, parentScopeID, workerLabel, in.Description, in.SubagentType)
	taskRuntime.EnsureScope(subagentScopeID)
	extraContext := copyAgentExtraContext(execCtx)
	extraContext["subagent_timeout_seconds"] = timeoutSeconds
	extraContext["scope_id"] = subagentScopeID
	extraContext["current_task_id"] = taskID
	extraContext["parent_scope_id"] = parentScopeID
	extraContext["parent_task_id"] = parentTaskID
	extraContext["current_agent_type"] = in.SubagentType
	extraContext["_drain_pending_notifications"] = taskRuntime.DrainPendingNotifications
	if worktreePath != "" {
		extraContext["worktree_cwd"] = worktreePath
	}
	childState := BuildSubagentState(parentState, definition.PermissionMode)
	sub := NewSubAgent(childCfg, filteredRegistry, definition, &childState, model, in.SubagentType, extraContext)
	result, err := sub.Run(ctx, in.Prompt)
	if err != nil {
		taskRuntime.FailForegroundTask(record, "failed")
		taskRuntime.CleanupScope(subagentScopeID)
		taskRuntime.DiscardForegroundTask(taskID)
		if worktreePath != "" {
			_ = RemoveWorktree(ctx, worktreePath)
		}
		return "", err
	}
	taskRuntime.CompleteForegroundTask(record, "completed")
	cleanupReport := taskRuntime.CleanupScope(subagentScopeID)
	taskRuntime.DiscardForegroundTask(taskID)
	if worktreePath != "" {
		_ = RemoveWorktree(ctx, worktreePath)
	}
	if cleanupReport["active_tasks_stopped"] > 0 {
		result = fmt.Sprintf("Sub-agent returned before its background child tasks settled. Stopped and discarded %d background task(s).\n\nPartial sub-agent output:\n%s", cleanupReport["active_tasks_stopped"], result)
	}
	return fmt.Sprintf("[Sub-agent: %s]\n[Task: %s]\n[Tokens: %d in / %d out]\n\n%s", in.SubagentType, in.Description, sub.TotalInputTokens, sub.TotalOutputTokens, result), nil
}

func agentTimeoutSeconds(input AgentInput) (int, string) {
	return parseAgentTimeoutSeconds(input.TimeoutSeconds)
}

func parseAgentTimeoutSeconds(raw any) (int, string) {
	if raw == nil {
		return DefaultSubagentTimeoutSeconds, ""
	}
	switch v := raw.(type) {
	case int:
		return validateAgentTimeoutSeconds(v)
	case int32:
		return validateAgentTimeoutSeconds(int(v))
	case int64:
		return validateAgentTimeoutSeconds(int(v))
	case float32:
		return validateAgentTimeoutSeconds(int(v))
	case float64:
		return validateAgentTimeoutSeconds(int(v))
	case json.Number:
		n, err := v.Int64()
		if err != nil {
			return 0, "timeout_seconds must be an integer number of seconds."
		}
		return validateAgentTimeoutSeconds(int(n))
	case string:
		text := strings.TrimSpace(strings.TrimSuffix(v, "s"))
		if text == "" {
			return 0, "timeout_seconds must not be empty."
		}
		n, err := strconv.Atoi(text)
		if err != nil {
			return 0, "timeout_seconds must be an integer number of seconds."
		}
		return validateAgentTimeoutSeconds(n)
	default:
		return 0, fmt.Sprintf("timeout_seconds must be an integer number of seconds, got %T.", raw)
	}
}

func validateAgentTimeoutSeconds(seconds int) (int, string) {
	if seconds <= 0 {
		return 0, "timeout_seconds must be greater than 0."
	}
	return seconds, ""
}

func derefAgentInput(input any) AgentInput {
	switch v := input.(type) {
	case AgentInput:
		return v
	case *AgentInput:
		if v != nil {
			return *v
		}
	}
	return AgentInput{}
}

func configFromContext(execCtx coretools.ExecutionContext) (config.Config, bool) {
	if cfg, ok := execCtx["config"].(config.Config); ok {
		return cfg, true
	}
	if cfg, ok := execCtx["config"].(*config.Config); ok && cfg != nil {
		return *cfg, true
	}
	return config.Config{}, false
}

func stringFromContext(ctx coretools.ExecutionContext, key, fallback string) string {
	if s, ok := ctx[key].(string); ok && s != "" {
		return s
	}
	return fallback
}

func copyAgentExtraContext(execCtx coretools.ExecutionContext) coretools.ExecutionContext {
	out := coretools.ExecutionContext{}
	for key, value := range execCtx {
		if key == "parent_state" || key == "_registry" {
			continue
		}
		out[key] = value
	}
	return out
}

type TaskListInput struct{}

type TaskGetInput struct {
	TaskID string `json:"task_id"`
}

type TaskWaitInput struct {
	TaskIDs       []string `json:"task_ids"`
	TimeoutSecond int      `json:"timeout_seconds"`
}

type TaskStopInput struct {
	TaskID string `json:"task_id"`
}

type SendMessageInput struct {
	TaskID string `json:"task_id"`
	Prompt string `json:"prompt"`
}

const taskTextPreviewChars = 4000

type taskTool struct {
	coretools.BaseTool
	exec func(context.Context, coretools.ExecutionContext, any) (string, error)
}

func (t *taskTool) NeedsPermission(any) bool { return false }

func (t *taskTool) Execute(ctx context.Context, execCtx coretools.ExecutionContext, input any) (string, error) {
	return t.exec(ctx, execCtx, input)
}

func (t *taskTool) FormatLargeResult(_ context.Context, content string, _ int, _, _ string) (string, error) {
	return content, nil
}

func NewTaskListTool() coretools.Tool {
	return newTaskTool("TaskList", "List all direct child tasks spawned by the current agent scope, including their status, usage, and result metadata.", TaskListInput{}, func(_ context.Context, execCtx coretools.ExecutionContext, _ any) (string, error) {
		rt, scope, err := runtimeAndScope(execCtx)
		if err != "" {
			return err, nil
		}
		records := rt.ListTasks(scope)
		tasks := make([]map[string]any, 0, len(records))
		for _, record := range records {
			tasks = append(tasks, serializeTaskPayload(record.ToMap()))
		}
		payload := map[string]any{"tasks": tasks}
		return jsonString(payload), nil
	})
}

func NewTaskGetTool() coretools.Tool {
	return newTaskTool("TaskGet", "Get the latest details for one direct child task by task_id, including status, result text, error text, usage, and lifecycle metadata.", TaskGetInput{}, func(_ context.Context, execCtx coretools.ExecutionContext, input any) (string, error) {
		rt, scope, err := runtimeAndScope(execCtx)
		if err != "" {
			return err, nil
		}
		in := derefTaskGet(input)
		record := rt.GetTask(in.TaskID, scope)
		if record == nil {
			return fmt.Sprintf("Error: Task '%s' was not found in the current scope.", in.TaskID), nil
		}
		return jsonString(serializeTaskPayload(record.ToMap())), nil
	})
}

func NewTaskWaitTool() coretools.Tool {
	return newTaskTool("TaskWait", "Block without busy-polling until all specified direct child tasks reach a stable state, or until timeout_seconds elapses.", TaskWaitInput{}, func(ctx context.Context, execCtx coretools.ExecutionContext, input any) (string, error) {
		rt, scope, err := runtimeAndScope(execCtx)
		if err != "" {
			return err, nil
		}
		in := derefTaskWait(input)
		return jsonString(serializeTaskResult(rt.WaitForTasks(ctx, in.TaskIDs, scope, in.TimeoutSecond))), nil
	})
}

func NewTaskStopTool() coretools.Tool {
	return newTaskTool("TaskStop", "Stop one direct child task. Queued workers are killed immediately; running workers are cancelled and converge to killed.", TaskStopInput{}, func(_ context.Context, execCtx coretools.ExecutionContext, input any) (string, error) {
		rt, scope, err := runtimeAndScope(execCtx)
		if err != "" {
			return err, nil
		}
		in := derefTaskStop(input)
		result := rt.StopTask(in.TaskID, scope)
		if text, ok := result.(string); ok {
			return text, nil
		}
		return jsonString(serializeTaskResult(result)), nil
	})
}

func NewSendMessageTool() coretools.Tool {
	return newTaskTool("SendMessage", "Send a new prompt to an existing reusable worker. This only succeeds when the target worker is idle and ready for more work.", SendMessageInput{}, func(_ context.Context, execCtx coretools.ExecutionContext, input any) (string, error) {
		rt, scope, err := runtimeAndScope(execCtx)
		if err != "" {
			return err, nil
		}
		in := derefSendMessage(input)
		result := rt.SendMessage(in.TaskID, scope, in.Prompt)
		if text, ok := result.(string); ok {
			return text, nil
		}
		return jsonString(serializeTaskResult(result)), nil
	})
}

func newTaskTool(name, description string, proto any, exec func(context.Context, coretools.ExecutionContext, any) (string, error)) coretools.Tool {
	return &taskTool{
		BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
			Name: name, Description: description, InputPrototype: proto,
			ReadOnly: coretools.BoolPtr(false), ConcurrencySafe: coretools.BoolPtr(true), Destructive: coretools.BoolPtr(false),
		}},
		exec: exec,
	}
}

func runtimeAndScope(execCtx coretools.ExecutionContext) (*AgentTaskRuntime, string, string) {
	rt, _ := execCtx["task_runtime"].(*AgentTaskRuntime)
	if rt == nil {
		return nil, "main", "Error: task runtime is not available in the execution context."
	}
	return rt, stringFromContext(execCtx, "scope_id", "main"), ""
}

func jsonString(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}

func serializeTaskResult(value any) any {
	switch v := value.(type) {
	case AgentTaskRecord:
		return serializeTaskPayload(v.ToMap())
	case *AgentTaskRecord:
		if v == nil {
			return nil
		}
		return serializeTaskPayload(v.ToMap())
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			if key == "tasks" {
				if tasks, ok := item.([]map[string]any); ok {
					serialized := make([]map[string]any, 0, len(tasks))
					for _, task := range tasks {
						serialized = append(serialized, serializeTaskPayload(task))
					}
					out[key] = serialized
					continue
				}
			}
			out[key] = item
		}
		return out
	default:
		return value
	}
}

func serializeTaskPayload(data map[string]any) map[string]any {
	payload := make(map[string]any, len(data)+4)
	for key, value := range data {
		payload[key] = value
	}
	for _, field := range []string{"result_text", "error_text"} {
		value, _ := payload[field].(string)
		runes := []rune(value)
		if len(runes) <= taskTextPreviewChars {
			continue
		}
		payload[field] = string(runes[:taskTextPreviewChars]) + "\n...[truncated]"
		payload[field+"_truncated"] = true
		payload[field+"_chars_total"] = len(runes)
	}
	return payload
}

func derefTaskGet(input any) TaskGetInput {
	if v, ok := input.(*TaskGetInput); ok && v != nil {
		return *v
	}
	if v, ok := input.(TaskGetInput); ok {
		return v
	}
	return TaskGetInput{}
}

func derefTaskWait(input any) TaskWaitInput {
	if v, ok := input.(*TaskWaitInput); ok && v != nil {
		return *v
	}
	if v, ok := input.(TaskWaitInput); ok {
		return v
	}
	return TaskWaitInput{}
}

func derefTaskStop(input any) TaskStopInput {
	if v, ok := input.(*TaskStopInput); ok && v != nil {
		return *v
	}
	if v, ok := input.(TaskStopInput); ok {
		return v
	}
	return TaskStopInput{}
}

func derefSendMessage(input any) SendMessageInput {
	if v, ok := input.(*SendMessageInput); ok && v != nil {
		return *v
	}
	if v, ok := input.(SendMessageInput); ok {
		return v
	}
	return SendMessageInput{}
}
