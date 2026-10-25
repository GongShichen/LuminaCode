package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"LuminaCode/agentContext"
	"LuminaCode/api"
	"LuminaCode/apppaths"
	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/harness"
	"LuminaCode/mcp"
	"LuminaCode/memory"
	"LuminaCode/security"
	"LuminaCode/sessionmemory"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"
	bashpkg "LuminaCode/tools/bash"

	mapset "github.com/deckarep/golang-set/v2"
)

const (
	MaxAPIErrorRetries = 3
	MaxParentTurns     = 100
)

var fatalAPIErrorKeywords = []string{
	"404",
	"model_not_found",
	"model not found",
	"invalid_api_key",
	"invalid api key",
	"authentication",
	"authorization",
	"not_found",
	"not found",
}

type ModelTurn struct {
	ThinkingContent []map[string]any
	FullText        string
	ToolCalls       []coretools.ToolCall
	MessageID       string
	InputTokens     int
	OutputTokens    int
	OutputTruncated bool
	StreamHadError  bool
	PTLDetected     bool
	PTLErrorMsg     string
}

func ShouldCommitFinalTextAfterStreamError(turn *ModelTurn) bool {
	return turn != nil &&
		turn.StreamHadError &&
		len(turn.ToolCalls) == 0 &&
		strings.TrimSpace(turn.FullText) != ""
}

type RuntimeCacheEditState = agentContext.CacheState

type CoreExecutionEngine struct {
	Config config.Config

	mu sync.Mutex

	Registry       *coretools.ToolRegistry
	AgentRuntime   *AgentRuntime
	SessionRuntime *SessionRuntime

	TaskRuntime            *AgentTaskRuntime
	mcpMu                  sync.Mutex
	mcpInitialized         bool
	mcpContext             coretools.ExecutionContext
	extraction             *ExtractionController
	memoryMu               sync.RWMutex
	memoryEngine           memory.Engine
	memoryEngineErr        error
	memoryEngineRuntimeKey string
	memoryFactory          MemoryFabricFactory
	runtimeIdentity        cluster.RuntimeIdentity
	skillRegistry          *skills.SkillRegistry
	skillDiscovery         *skills.SkillDiscovery
	skillPersistence       *skills.SkillPersistence
	skillRecoveryReady     map[string]bool

	SessionID     string
	AgentID       string
	AgentType     string
	TeamName      string
	TeamSessionID string
	TeamAgentID   string
	sessionMemory *sessionmemory.Manager
	StateObserver func(*AgentState)
	RuntimeEvents *RuntimeEventRecorder
	Runtime       *RuntimeAssembly
}

func NewCoreExecutionEngine(cfg config.Config, identity cluster.RuntimeIdentity, memoryFactory MemoryFabricFactory) *CoreExecutionEngine {
	return newCoreExecutionEngine(cfg, identity, memoryFactory, nil, false)
}

// NewCoreExecutionEngineWithMemoryEngine creates an execution engine with an
// already-open memory engine. Ownership of memoryEngine transfers to the core
// engine, which closes it during shutdown.
func NewCoreExecutionEngineWithMemoryEngine(cfg config.Config, identity cluster.RuntimeIdentity,
	memoryFactory MemoryFabricFactory, memoryEngine memory.Engine) *CoreExecutionEngine {
	return newCoreExecutionEngine(cfg, identity, memoryFactory, memoryEngine, true)
}

func newCoreExecutionEngine(cfg config.Config, identity cluster.RuntimeIdentity, memoryFactory MemoryFabricFactory,
	memoryEngine memory.Engine, injected bool) *CoreExecutionEngine {
	e := &CoreExecutionEngine{
		Config:          cfg,
		AgentRuntime:    NewAgentRuntime(cfg, nil),
		SessionRuntime:  NewSessionRuntime(),
		Registry:        coretools.NewToolRegistry(),
		TaskRuntime:     NewAgentTaskRuntime(),
		sessionMemory:   sessionmemory.NewManager(),
		memoryFactory:   memoryFactory,
		runtimeIdentity: identity,
	}
	e.SessionRuntime.Register("main", e.AgentRuntime)
	e.RegisterDefaultTools()
	e.extraction = NewExtractionController(e.Config)
	if injected {
		e.memoryEngine = memoryEngine
		e.memoryEngineRuntimeKey = fabricMemoryRuntimeKey(e.Config)
		e.extraction.SetEngine(memoryEngine)
	} else {
		e.configureMemoryEngine(context.Background(), e.Config)
	}
	return e
}

func (e *CoreExecutionEngine) RefreshRuntimeConfig(cfg config.Config) {
	if e == nil {
		return
	}
	previousKey := fabricMemoryRuntimeKey(e.Config)
	e.Config = cfg
	if e.extraction != nil {
		e.extraction.Config = cfg
	}
	if previousKey != fabricMemoryRuntimeKey(cfg) || (isFabricMemoryBackend(cfg) && e.memoryEngineSnapshot() == nil) {
		if e.extraction != nil {
			e.extraction.Cancel()
		}
		e.configureMemoryEngine(context.Background(), cfg)
	}
}

func (e *CoreExecutionEngine) RegisterDefaultTools() {
	e.Registry.Register(coretools.NewReadFileTool())
	e.Registry.Register(coretools.NewWriteFileTool())
	e.Registry.Register(coretools.NewEditFileTool())
	e.Registry.Register(coretools.NewNotebookEditTool())
	e.Registry.Register(coretools.NewGrepSearchTool())
	e.Registry.Register(coretools.NewGlobMatchTool())
	e.Registry.Register(coretools.NewWebSearchTool())
	e.Registry.Register(coretools.NewWebFetchTool())
	e.Registry.Register(coretools.NewBashTool())
	e.Registry.Register(NewAgentTool())
	e.Registry.Register(NewTaskListTool())
	e.Registry.Register(NewTaskGetTool())
	e.Registry.Register(NewTaskWaitTool())
	e.Registry.Register(NewTaskStopTool())
	e.Registry.Register(NewSendMessageTool())
	if e.Config.SessionMemoryEnabled {
		e.Registry.Register(NewSessionHistoryListTool())
		e.Registry.Register(NewSessionHistoryGetTool())
	}
	e.Registry.Register(coretools.NewToolSearchTool())
	if e.Config.SkillsEnabled {
		loader := skills.NewSkillLoader(e.Config)
		skillRegistry := skills.NewSkillRegistry(e.Config.CWD)
		for _, skill := range loader.LoadFrontmatterOnly() {
			skillRegistry.Register(skill)
		}
		processor := skills.NewPromptProcessor(e.Config)
		executor := skills.NewSkillExecutor(loader, processor)
		executor.ForkRunner = e.runForkSkill
		e.Registry.Register(skills.NewSkillTool(skillRegistry, executor))
		e.skillRegistry = skillRegistry
		e.skillDiscovery = skills.NewSkillDiscovery(skillRegistry)
		e.skillPersistence = skills.NewSkillPersistence()
		e.skillRecoveryReady = map[string]bool{}
	}
}

func (e *CoreExecutionEngine) runForkSkill(ctx context.Context, skill skills.SkillSpec, prompt, subagentScope string, thinkingBudgetTokens *int, baseRegistry *coretools.ToolRegistry, parentState any, extraContext coretools.ExecutionContext) (string, int, int, error) {
	agentName := "skill-fork"
	if skill.Frontmatter.Agent != nil && *skill.Frontmatter.Agent != "" {
		agentName = *skill.Frontmatter.Agent
	}
	definition := AgentDef{
		Name:           agentName,
		Description:    "Forked skill runner for '" + skill.CanonicalName + "'.",
		MaxTurns:       50,
		PermissionMode: "inherit",
	}
	state := NewAgentState()
	switch parent := parentState.(type) {
	case *security.PermissionState:
		state.PermissionState = ClonePermissionState(parent)
	case *AgentState:
		if parent != nil {
			state.PermissionState = ClonePermissionState(parent.PermissionState)
		}
	default:
		if policy, ok := extraContext["spawn_permissions"].(*security.PermissionState); ok {
			state.PermissionState = ClonePermissionState(policy)
		}
	}
	modelOverride := ""
	if skill.Frontmatter.Model != nil {
		modelOverride = *skill.Frontmatter.Model
	}
	if extraContext == nil {
		extraContext = coretools.ExecutionContext{}
	}
	if _, exists := extraContext["_memory_engine"]; !exists {
		extraContext["_memory_engine"] = e.memoryEngineSnapshot()
	}
	extraContext["_skill_agent_scope"] = subagentScope
	extraContext["scope_id"] = subagentScope
	extraContext["_session_runtime"] = e.SessionRuntime
	if e.RuntimeEvents != nil {
		extraContext["_runtime_store"] = e.RuntimeEvents.EventStore()
	}
	sub := NewSubAgent(e.Config, baseRegistry, definition, &state, modelOverride, agentName, extraContext, thinkingBudgetTokens)
	result, err := sub.Run(ctx, prompt)
	return result, sub.TotalInputTokens, sub.TotalOutputTokens, err
}

func (e *CoreExecutionEngine) ensureMCPTools(execCtx coretools.ExecutionContext) {
	e.mcpMu.Lock()
	defer e.mcpMu.Unlock()
	if e.mcpInitialized {
		mergeExecutionContext(execCtx, e.mcpContext)
		return
	}
	mcpCtx := coretools.ExecutionContext{}
	if e.mcpContext != nil {
		if approvals, ok := e.mcpContext["trusted_mcp_servers"]; ok {
			mcpCtx["trusted_mcp_servers"] = approvals
		}
	}
	_ = coretools.RegisterMCPTools(e.Registry, e.Config.CWD, mcpCtx)
	e.mcpContext = mcpCtx
	e.mcpInitialized = true
	mergeExecutionContext(execCtx, e.mcpContext)
}

func (e *CoreExecutionEngine) maybePromptMCPTrust(ctx context.Context, out chan<- StreamEvent) {
	e.mcpMu.Lock()
	pending, _ := e.mcpContext["pending_mcp_trust"].([]map[string]any)
	e.mcpMu.Unlock()
	token, ok := runTokenFromContext(ctx)
	if !ok {
		return
	}
	if len(pending) == 0 {
		return
	}
	if e.AgentRuntime.YoloEnabled() {
		_ = e.AgentRuntime.withRun(token, func() { e.applyMCPTrustDecision(pending, true) })
		return
	}
	event := NewStreamEvent("permission_needed", "mcp_project_trust", map[string]any{"mcp_trust_request": pending, "dangerous": true, "risk": "high"})
	ticket, event, err := e.AgentRuntime.BeginPermission(ctx, token, PermissionMCP, "", event, false)
	if err != nil {
		return
	}
	if !sendStream(ctx, out, event) {
		e.AgentRuntime.cancelPermission(ticket)
		return
	}
	answer, err := e.AgentRuntime.AwaitPermission(ctx, ticket)
	if err != nil {
		return
	}
	granted := permissionGranted(answer)
	_ = e.AgentRuntime.withRun(token, func() { e.applyMCPTrustDecision(pending, granted) })
}

func (e *CoreExecutionEngine) applyMCPTrustDecision(pending []map[string]any, granted bool) {
	e.mcpMu.Lock()
	defer e.mcpMu.Unlock()
	if e.mcpContext == nil {
		return
	}
	if !granted {
		e.mcpContext["pending_mcp_trust"] = []map[string]any{}
		return
	}
	approvals := map[string]string{}
	for _, item := range pending {
		name := stringFromAny(item["name"])
		fingerprint := stringFromAny(item["fingerprint"])
		if name != "" && fingerprint != "" {
			approvals[name] = fingerprint
		}
	}
	e.mcpContext["trusted_mcp_servers"] = approvals
	e.mcpContext["pending_mcp_trust"] = []map[string]any{}
	e.mcpInitialized = false
}

func (e *CoreExecutionEngine) disconnectMCP(ctx context.Context) {
	e.mcpMu.Lock()
	defer e.mcpMu.Unlock()
	clients, _ := e.mcpContext["mcp_clients"].(map[string]*mcp.McpClient)
	for _, client := range clients {
		client.Disconnect(ctx)
	}
	e.mcpContext = nil
	e.mcpInitialized = false
}

func mergeExecutionContext(dst, src coretools.ExecutionContext) {
	for key, value := range src {
		dst[key] = value
	}
}

func (e *CoreExecutionEngine) Abort() { e.AgentRuntime.Abort() }

func (e *CoreExecutionEngine) Reset(contexts ...context.Context) error {
	resetCtx := context.Background()
	if len(contexts) > 0 {
		resetCtx = contexts[0]
	}
	// Retire child publishers before writing the reset event. A child's first
	// checkpoint must not appear after Clear and become a new recovered actor.
	e.AgentRuntime.Abort()
	e.SessionRuntime.Reset(e.AgentRuntime)
	e.TaskRuntime.Shutdown()
	if err := e.AgentRuntime.Reset(resetCtx); err != nil {
		return err
	}
	e.disconnectMCP(context.Background())
	if e.extraction != nil {
		e.extraction.Cancel()
	}
	e.flushAndSealMemory(context.Background())
	if e.skillPersistence != nil {
		e.skillPersistence.ClearAll()
	}
	e.mu.Lock()
	defer e.mu.Unlock()

	e.skillRecoveryReady = map[string]bool{}
	return nil
}
func (e *CoreExecutionEngine) Shutdown() {
	e.AgentRuntime.Abort()
	e.SessionRuntime.close()
	e.disconnectMCP(context.Background())
	if e.TaskRuntime != nil {
		e.TaskRuntime.Shutdown()
	}
	if e.extraction != nil {
		e.extraction.Cancel()
	}
	e.flushAndSealMemory(context.Background())
	e.closeMemoryEngine()
	if e.skillPersistence != nil {
		e.skillPersistence.ClearAll()
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mcpInitialized = false
	e.skillRecoveryReady = map[string]bool{}
	e.TaskRuntime = NewAgentTaskRuntime()
}

func (e *CoreExecutionEngine) ClearMCP() {
	e.disconnectMCP(context.Background())
}

func (e *CoreExecutionEngine) ExportSkillRecoverySnapshot() map[string]any {
	if e.skillPersistence == nil {
		return nil
	}
	return e.skillPersistence.ExportSnapshot()
}

func (e *CoreExecutionEngine) ImportSkillRecoverySnapshot(snapshot map[string]any) {
	if e.skillPersistence == nil {
		return
	}
	e.skillPersistence.ImportSnapshot(snapshot)
}

func (e *CoreExecutionEngine) MarkSkillHistoryCompacted(agentScope string) {
	if agentScope == "" {
		agentScope = "main"
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.skillRecoveryReady == nil {
		e.skillRecoveryReady = map[string]bool{}
	}
	e.skillRecoveryReady[agentScope] = true
}

func (e *CoreExecutionEngine) ResolvePermission(decision, toolName string) {
	e.AgentRuntime.ResolveRunPermission(PermissionTool, PermissionAnswer{Decision: decision, ToolName: toolName})
}
func (e *CoreExecutionEngine) ResolveSkillPermission(granted bool) {
	decision := PermissionDeny
	if granted {
		decision = PermissionOnce
	}
	e.SessionRuntime.ResolvePermissionKind(PermissionSkill, PermissionAnswer{Decision: decision})
}
func (e *CoreExecutionEngine) ResolvePermissionRequest(id, decision, toolName string) bool {
	return e.SessionRuntime.ResolvePermissionRequest(id, PermissionAnswer{Decision: decision, ToolName: toolName})
}

func (e *CoreExecutionEngine) PermissionContext(id string) (context.Context, bool) {
	return e.SessionRuntime.PermissionContext(id)
}

func (e *CoreExecutionEngine) DeferPermissionRequest(id string) bool {
	return e.SessionRuntime.DeferPermissionRequest(id)
}

func (e *CoreExecutionEngine) SkillNames(cwd string) []string {
	if e == nil || e.skillRegistry == nil {
		return nil
	}
	if strings.TrimSpace(cwd) == "" {
		cwd = e.Config.CWD
	}
	visible := e.skillRegistry.ListVisible(cwd)
	names := make([]string, 0, len(visible))
	for _, skill := range visible {
		names = append(names, skill.CanonicalName)
	}
	sort.Strings(names)
	return names
}

func (e *CoreExecutionEngine) ResolveMCPTrust(granted bool) {
	decision := PermissionDeny
	if granted {
		decision = PermissionOnce
	}
	e.AgentRuntime.ResolveRunPermission(PermissionMCP, PermissionAnswer{Decision: decision})
}

func (e *CoreExecutionEngine) queryDraftLoop(ctx context.Context, state *AgentState) <-chan StreamEvent {
	out := make(chan StreamEvent)
	go func() {
		defer close(out)
		raw := make(chan StreamEvent)
		go func() { defer close(raw); e.queryLoop(ctx, state, raw) }()
		for event := range raw {
			e.AgentRuntime.ObserveRunEvent(state.RunToken(), event)
			sendStream(ctx, out, event)
		}
	}()
	return out
}

func (e *CoreExecutionEngine) queryLoop(ctx context.Context, state *AgentState, out chan<- StreamEvent) {
	token := state.RunToken()

	if e.Config.MCPEnabled && !e.mcpInitialized {
		if e.Runtime != nil {
			result, err := e.Runtime.Hooks.MCPPreparing.Run(ctx, MCPPreparingHookContext{SessionID: e.SessionID, ExecutionContext: coretools.ExecutionContext{}})
			if err != nil || e.Runtime.AppendHookEvents(ctx, e.RuntimeEvents, result.Events) != nil {
				sendStream(ctx, out, NewStreamEvent("error", "MCP preparation hook failed", map[string]any{"runtime_hook": true}))
				return
			}
		} else {
			e.ensureMCPTools(coretools.ExecutionContext{})
		}
		e.maybePromptMCPTrust(ctx, out)
		if e.Config.MCPEnabled && !e.mcpInitialized {
			if e.Runtime != nil {
				_, _ = e.Runtime.Hooks.MCPPreparing.Run(ctx, MCPPreparingHookContext{SessionID: e.SessionID, ExecutionContext: coretools.ExecutionContext{}})
			} else {
				e.ensureMCPTools(coretools.ExecutionContext{})
			}
		}
	}

	if e.Runtime != nil {
		result, err := e.Runtime.Hooks.MemoryPreparing.Run(ctx, MemoryPreparingHookContext{SessionID: e.SessionID, State: state})
		if err != nil || e.Runtime.AppendHookEvents(ctx, e.RuntimeEvents, result.Events) != nil {
			sendStream(ctx, out, NewStreamEvent("error", "memory preparation hook failed", map[string]any{"runtime_hook": true}))
			return
		}
		state = result.Value.State
	} else {
		e.applyMemoryRuntimeIdentity(state)
		state.Messages = memory.StripMemoryContextMessages(state.Messages, "")
		if e.extraction != nil && e.extraction.HasPendingResult() {
			if result := e.extraction.ConsumeResult(); result != "" && len(state.Messages) > 0 {
				insertBeforeCurrentUserMessage(state, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": result}}, "isMeta": true})
			}
		}
	}
	cancelRecallPrefetch := e.prefetchMemoryRecall(ctx, state)
	if cancelRecallPrefetch != nil {
		defer cancelRecallPrefetch()
	}

	for {
		if ctx.Err() != nil || e.AgentRuntime.RunSnapshot().Phase == AgentPhaseCancelling {
			_ = e.publishDraft(context.WithoutCancel(ctx), state)
			sendStream(ctx, out, NewStreamEvent("done", "Aborted by user.", nil))
			return
		}
		if err := e.AgentRuntime.Transition(token, AgentPhasePreparing); err != nil {
			return
		}
		maxTurns := e.Config.MaxParentTurns
		if maxTurns <= 0 {
			maxTurns = MaxParentTurns
		}
		if state.TurnCount >= maxTurns {
			_ = e.publishDraft(context.WithoutCancel(ctx), state)
			sendStream(ctx, out, NewStreamEvent("error", fmt.Sprintf("Reached maximum turns (%d). The task may be too complex or stuck in a loop. Try breaking it into smaller steps.", maxTurns), nil))
			sendStream(ctx, out, NewStreamEvent("done", "", nil))
			return
		}
		if e.RuntimeEvents != nil {
			if err := e.RuntimeEvents.BeginStep(ctx); err != nil {
				sendStream(ctx, out, NewStreamEvent("error", err.Error(), map[string]any{"runtime_journal": true}))
				sendStream(ctx, out, NewStreamEvent("done", "", nil))
				return
			}
		}
		if e.Runtime != nil {
			result, err := e.Runtime.Hooks.StepPreparing.Run(ctx, StepHookContext{SessionID: e.SessionID, State: state, StepNo: state.TurnCount + 1})
			if err != nil || e.Runtime.AppendHookEvents(ctx, e.RuntimeEvents, result.Events) != nil {
				if err == nil {
					err = fmt.Errorf("step hook event commit failed")
				}
				sendStream(ctx, out, NewStreamEvent("error", err.Error(), map[string]any{"runtime_hook": true}))
				sendStream(ctx, out, NewStreamEvent("done", "", nil))
				return
			}
			state = result.Value.State
		}

		if err := e.publishDraft(ctx, state); err != nil {
			sendStream(ctx, out, NewStreamEvent("error", err.Error(), nil))
			return
		}
		inlineRuntime := skills.InlineSkillRuntime{}
		if e.Runtime != nil {
			result, err := e.Runtime.Hooks.SkillPreparing.Run(ctx, SkillPreparingHookContext{SessionID: e.SessionID, State: state})
			if err != nil || e.Runtime.AppendHookEvents(ctx, e.RuntimeEvents, result.Events) != nil {
				sendStream(ctx, out, NewStreamEvent("error", "skill preparation hook failed", map[string]any{"runtime_hook": true}))
				sendStream(ctx, out, NewStreamEvent("done", "", nil))
				return
			}
			state = result.Value.State
			inlineRuntime = result.Value.InlineRuntime
		} else {
			e.stripSkillMessages(state, map[string]struct{}{skills.SkillListingSource: {}, skills.SkillRecoverySource: {}})
			e.injectSkillRuntimeAttachments(state)
			e.injectPendingTaskNotifications(state)
			inlineRuntime = skills.CollectInlineSkillRuntime(contextSkillMessages(state))
		}
		activeRegistry := e.Registry
		if e.Config.MCPEnabled {
			mcpCtx := coretools.ExecutionContext{}
			if e.Runtime != nil {
				_, _ = e.Runtime.Hooks.MCPPreparing.Run(ctx, MCPPreparingHookContext{SessionID: e.SessionID, ExecutionContext: mcpCtx})
			} else {
				e.ensureMCPTools(mcpCtx)
			}
		}
		if inlineRuntime.HasAllowedTools {
			allow := map[string]struct{}{}
			for _, name := range inlineRuntime.AllowedToolNames {
				allow[name] = struct{}{}
			}
			activeRegistry = e.Registry.FilteredCopy(allow, nil, false, false)
		}
		if inlineRuntime.DisableSkillTool {
			activeRegistry = activeRegistry.FilteredCopy(nil, map[string]struct{}{"Skill": {}}, false, false)
		}

		turn := &ModelTurn{}
		execCtx := coretools.ExecutionContext{
			"cwd":                 e.Config.CWD,
			"config":              e.Config,
			"runtime_dir":         e.Config.ProjectRuntimeDir,
			"web_search_scope":    e.Config.WebSearchCacheScope,
			"_session_id":         e.SessionID,
			"_agent_id":           e.sessionMemoryAgentID(),
			"allowed_read_roots":  compactToolRoots(e.Config.CWD, e.Config.ProjectRuntimeDir),
			"allowed_write_roots": compactToolRoots(e.Config.CWD, e.Config.ProjectRuntimeDir),
			"_registry":           activeRegistry,
			"agent_services":      e.AgentRuntime.ServicesFor(state.RunToken()),
			"spawn_permissions":   ClonePermissionState(state.PermissionState),
			"task_runtime":        e.TaskRuntime,
			"_session_runtime":    e.SessionRuntime,
			"_session_lease":      e.SessionRuntime.lease(),
			"_runtime_store": func() any {
				if e.RuntimeEvents != nil {
					return e.RuntimeEvents.EventStore()
				}
				return nil
			}(),
			"scope_id":                "main",
			"current_task_id":         nil,
			"parent_task_id":          nil,
			"parent_scope_id":         nil,
			"current_agent_type":      "",
			"_pending_messages":       []map[string]any{},
			"_pending_skill_messages": []map[string]any{},
			"_skill_persistence":      e.skillPersistence,
			"_skill_agent_scope":      "main",
			"_turn_count":             state.TurnCount,
			"_memory_engine":          e.memoryEngineSnapshot(),
		}
		if e.Config.MCPEnabled {
			if e.Runtime != nil {
				result, err := e.Runtime.Hooks.MCPPreparing.Run(ctx, MCPPreparingHookContext{SessionID: e.SessionID, ExecutionContext: execCtx})
				if err != nil || e.Runtime.AppendHookEvents(ctx, e.RuntimeEvents, result.Events) != nil {
					sendStream(ctx, out, NewStreamEvent("error", "MCP preparation hook failed", map[string]any{"runtime_hook": true}))
					return
				}
				execCtx = result.Value.ExecutionContext
			} else {
				e.ensureMCPTools(execCtx)
			}
		}
		executor := NewStreamingToolExecutor(activeRegistry, e.Config, state, execCtx)
		executor.Observer = e.RuntimeEvents
		execCtx["_request_skill_shell_permission"] = func(req skills.SkillShellPermissionRequest) bool {
			return e.requestSkillShellPermission(ctx, executor, req)
		}

		thinkingBudget := resolveInlineThinkingBudget(inlineRuntime.Effort)
		modelOverride := ""
		if inlineRuntime.ModelOverride != nil {
			modelOverride = *inlineRuntime.ModelOverride
		}
		client, err := e.BuildClient(e.AgentRuntime.OutputRecovery(token).CurrentMaxTokens, modelOverride, thinkingBudget)
		if err != nil {
			sendStream(ctx, out, NewStreamEvent("error", err.Error(), nil))
			sendStream(ctx, out, NewStreamEvent("done", "", nil))
			return
		}

		toolSchemas := activeRegistry.GetAPISchemas()
		compiled, err := e.prepareContext(ctx, state, toolSchemas, agentContext.CompactAuto, firstNonEmptyString(modelOverride, e.Config.APIModel))
		if err != nil {
			sendStream(ctx, out, NewStreamEvent("error", err.Error(), nil))
			sendStream(ctx, out, NewStreamEvent("done", "", nil))
			return
		}
		messages := compiled.Messages
		if e.Runtime != nil {
			result, err := e.Runtime.Hooks.ContextBuilding.Run(ctx, ContextBuildHookContext{
				SessionID: e.SessionID, State: state, Messages: messages, ToolSchemas: toolSchemas,
			})
			if err != nil || e.Runtime.AppendHookEvents(ctx, e.RuntimeEvents, result.Events) != nil {
				if err == nil {
					err = fmt.Errorf("context hook event commit failed")
				}
				sendStream(ctx, out, NewStreamEvent("error", err.Error(), map[string]any{"runtime_hook": true}))
				sendStream(ctx, out, NewStreamEvent("done", "", nil))
				return
			}
			messages = result.Value.Messages
			toolSchemas = result.Value.ToolSchemas
		}
		compiled.Messages = StripMessageMetadata(NormalizeMessages(RepairOrphanTools(messages), firstNonEmptyString(modelOverride, e.Config.APIModel), state.RecentApiErrors))
		compiled.ToolSchemas = toolSchemas
		compiled, err = e.AgentRuntime.Context.Validate(compiled)
		if err != nil {
			sendStream(ctx, out, NewStreamEvent("error", err.Error(), nil))
			sendStream(ctx, out, NewStreamEvent("done", "", nil))
			return
		}
		messages = e.applyRollingCache(compiled.Messages, state, firstNonEmptyString(modelOverride, e.Config.APIModel))
		withCache := compiled
		withCache.Messages = messages
		if validated, cacheErr := e.AgentRuntime.Context.Validate(withCache); cacheErr == nil {
			compiled = validated
		} else {
			messages = compiled.Messages
		}
		if err = e.AgentRuntime.RecordCompiled(ctx, compiled, state); err != nil {
			sendStream(ctx, out, NewStreamEvent("error", err.Error(), nil))
			sendStream(ctx, out, NewStreamEvent("done", "", nil))
			return
		}
		if e.RuntimeEvents != nil {
			if err := e.RuntimeEvents.RecordCompiledContext(ctx, state.SystemPrompt, messages, toolSchemas); err != nil {
				sendStream(ctx, out, NewStreamEvent("error", err.Error(), map[string]any{"runtime_journal": true}))
				sendStream(ctx, out, NewStreamEvent("done", "", nil))
				return
			}
			if err := e.RuntimeEvents.RecordModelRequest(ctx, firstNonEmptyString(modelOverride, e.Config.APIModel), len(messages), len(toolSchemas)); err != nil {
				sendStream(ctx, out, NewStreamEvent("error", err.Error(), map[string]any{"runtime_journal": true}))
				sendStream(ctx, out, NewStreamEvent("done", "", nil))
				return
			}
		}
		requestOptions := e.AgentRuntime.ConsumeRequestCache(state.RunToken(), VisibleToolResultIDs(messages), firstNonEmptyString(modelOverride, e.Config.APIModel))
		if len(requestOptions.AnthropicCacheEdits) == 0 {
			requestOptions = nil
		}
		streamCtx := api.ContextWithStreamIdleTimeout(ctx, time.Duration(e.Config.APIStreamIdleTimeoutSeconds*float64(time.Second)))
		if err := e.AgentRuntime.Transition(token, AgentPhaseCallingModel); err != nil {
			return
		}
		for result := range client.StreamChat(streamCtx, state.SystemPrompt, messages, toolSchemas, requestOptions) {
			if result.Err != nil {
				if ctx.Err() != nil || e.AgentRuntime.RunSnapshot().Phase == AgentPhaseCancelling {
					e.AgentRuntime.FinishRequestCache(state.RunToken(), false)
					_ = e.publishDraft(context.WithoutCancel(ctx), state)
					sendStream(ctx, out, NewStreamEvent("done", "Aborted by user.", nil))
					return
				}
				msg := result.Err.Error()
				if IsFatalAPIError(msg) {
					e.AgentRuntime.FinishRequestCache(state.RunToken(), false)
					e.rememberAPIError(state, msg)
					_ = e.publishDraft(context.WithoutCancel(ctx), state)
					sendStream(ctx, out, NewStreamEvent("error", msg, api.ErrorMetadata(result.Err)))
					sendStream(ctx, out, NewStreamEvent("done", "", nil))
					return
				}
				if IsPTLErrorMessage(msg) {
					turn.PTLDetected = true
					turn.PTLErrorMsg = msg
					break
				}
				e.AgentRuntime.SetAPIErrors(token, e.AgentRuntime.APIErrors(token)+1)
				_ = e.AgentRuntime.StartRecovery(token, RecoveryAPI, msg)
				e.rememberAPIError(state, msg)
				if e.AgentRuntime.APIErrors(token) >= MaxAPIErrorRetries {
					e.AgentRuntime.FinishRequestCache(state.RunToken(), false)
					sendStream(ctx, out, e.apiErrorEvent(msg, api.ErrorMetadata(result.Err)))
					_ = e.publishDraft(context.WithoutCancel(ctx), state)
					sendStream(ctx, out, NewStreamEvent("done", "", nil))
					return
				}
				metadata := api.ErrorMetadata(result.Err)
				metadata["recovered"] = true
				sendStream(ctx, out, NewStreamEvent("error", "API error (recovered): "+msg, metadata))
				turn.StreamHadError = true
				break
			}
			if ctx.Err() != nil || e.AgentRuntime.RunSnapshot().Phase == AgentPhaseCancelling {
				e.AgentRuntime.FinishRequestCache(state.RunToken(), false)
				_ = e.publishDraft(context.WithoutCancel(ctx), state)
				sendStream(ctx, out, NewStreamEvent("done", "Aborted by user.", nil))
				return
			}
			action := e.HandleStreamEvent(result.Event, turn, executor, state, e.AgentRuntime.APIErrors(token))
			e.AgentRuntime.SetAPIErrors(token, action.ConsecutiveAPIErrors)
			if action.Event != nil {
				sendStream(ctx, out, *action.Event)
			}
			if action.Return {
				e.AgentRuntime.FinishRequestCache(state.RunToken(), false)
				sendStream(ctx, out, NewStreamEvent("done", "", nil))
				return
			}
			if action.Break {
				break
			}
		}

		if turn.StreamHadError || turn.PTLDetected {
			e.AgentRuntime.FinishRequestCache(state.RunToken(), false)
		} else {
			e.AgentRuntime.FinishRequestCache(state.RunToken(), true)
		}

		if ShouldCommitFinalTextAfterStreamError(turn) {
			turn.StreamHadError = false
			e.AgentRuntime.SetAPIErrors(token, 0)
		}
		if !turn.StreamHadError {
			e.AgentRuntime.SetAPIErrors(token, 0)
		}
		if turn.StreamHadError && len(turn.ToolCalls) == 0 && e.AgentRuntime.APIErrors(token) < MaxAPIErrorRetries {
			_ = e.AgentRuntime.StartRecovery(token, RecoveryAPI, "retrying model request")
			continue
		}
		if turn.PTLDetected {
			if !e.AgentRuntime.TryRecovery(token, RecoveryContext, 3) {
				sendStream(ctx, out, NewStreamEvent("error", "Context recovery retry limit reached", nil))
				return
			}
			_ = e.AgentRuntime.StartRecovery(token, RecoveryContext, turn.PTLErrorMsg)
			action, event := (&PTLRecoveryManager{Config: e.Config}).RecoverContext(ctx, state, turn.PTLErrorMsg)
			if action == "retry" {
				e.clearRuntimeCompressionStateAfterHistoryReplace(state)
				e.AgentRuntime.ResetOutputRecovery(token, e.Config.APIMaxTokens)
				continue
			}
			if event != nil {
				sendStream(ctx, out, *event)
			}
			_ = e.publishDraft(context.WithoutCancel(ctx), state)
			sendStream(ctx, out, NewStreamEvent("done", "", nil))
			return
		}
		if turn.OutputTruncated {
			recovery := e.AgentRuntime.HandleOutputTruncation(token, state, turn)
			if recovery.ShouldContinue() {
				continue
			}
			if recovery.ShouldReturn() {
				if recovery.Event != nil {
					sendStream(ctx, out, *recovery.Event)
				}
				_ = e.publishDraft(context.WithoutCancel(ctx), state)
				sendStream(ctx, out, NewStreamEvent("done", "", nil))
				return
			}
		}

		e.AgentRuntime.ResetOutputRetries(token)
		CommitAssistantTurn(state, turn.ThinkingContent, turn.FullText, turn.ToolCalls, turn.MessageID, turn.InputTokens, turn.OutputTokens)
		if e.Runtime != nil {
			result, err := e.Runtime.Hooks.ModelResponseReceived.Run(ctx, ModelResponseHookContext{SessionID: e.SessionID, State: state, Turn: turn})
			if err != nil || e.Runtime.AppendHookEvents(ctx, e.RuntimeEvents, result.Events) != nil {
				if err == nil {
					err = fmt.Errorf("model response hook event commit failed")
				}
				sendStream(ctx, out, NewStreamEvent("error", err.Error(), map[string]any{"runtime_hook": true}))
				sendStream(ctx, out, NewStreamEvent("done", "", nil))
				return
			}
			state = result.Value.State
		}
		if e.RuntimeEvents != nil && len(state.Messages) > 0 {
			if err := e.RuntimeEvents.RecordAssistant(ctx, state.Messages[len(state.Messages)-1], harness.UsageRecordedPayload{
				InputTokens: turn.InputTokens, OutputTokens: turn.OutputTokens,
			}); err != nil {
				sendStream(ctx, out, NewStreamEvent("error", err.Error(), map[string]any{"runtime_journal": true}))
				sendStream(ctx, out, NewStreamEvent("done", "", nil))
				return
			}
		}
		if len(turn.ToolCalls) == 0 {
			e.RecordSessionMemory(ctx, state, false)
			if e.extraction != nil &&
				e.Config.LongTermMemoryEnabled &&
				isFabricMemoryBackend(e.Config) &&
				state.LastQuery != "" {
				e.extraction.Config = e.Config
				e.extraction.SourceSessionID = e.SessionID
				e.extraction.SourceAgentID = e.memoryAgentType()
				e.extraction.SourceTeamSessionID = e.TeamSessionID
				e.extraction.SourceTeamName = e.TeamName
				e.extraction.SourceTeamAgentID = e.TeamAgentID
				e.extraction.Schedule(ctx, state, "")
			}
			_ = e.publishDraft(context.WithoutCancel(ctx), state)
			sendStream(ctx, out, NewStreamEvent("done", "", nil))
			return
		}

		if err := e.AgentRuntime.Transition(token, AgentPhaseExecutingTools); err != nil {
			return
		}
		resolver := PermissionResolver{
			Registry:        activeRegistry,
			CheckPermission: e.CheckPermissionChain,
			EnableYolo: func(state *AgentState) {
				if state != nil && state.PermissionState != nil {
					state.PermissionState.YoloMode = true
				}
				_ = e.publishDraft(ctx, state)
				if e.StateObserver != nil {
					snapshot := e.AgentRuntime.Snapshot().LegacyState()
					e.StateObserver(&snapshot)
				}
			},
			RequestDecision: func(ctx context.Context, event StreamEvent) (string, string) {
				toolCall, _ := event.Metadata["tool_call"].(coretools.ToolCall)
				if e.RuntimeEvents != nil && toolCall.ID != "" {
					if err := e.RuntimeEvents.RecordPermission(ctx, toolCall, true, ""); err != nil {
						return PermissionDeny, ""
					}
				}
				ticket, published, err := e.AgentRuntime.BeginPermission(ctx, token, PermissionTool, toolCall.Name, event, false)
				if err != nil {
					return PermissionDeny, ""
				}
				if !sendStream(ctx, out, published) {
					e.AgentRuntime.cancelPermission(ticket)
					return PermissionDeny, ""
				}
				decision, err := e.AgentRuntime.AwaitPermission(ctx, ticket)
				if err != nil {
					return PermissionDeny, ""
				}
				if e.RuntimeEvents != nil && toolCall.ID != "" {
					if err = e.RuntimeEvents.RecordPermission(ctx, toolCall, false, decision.Decision); err != nil {
						return PermissionDeny, ""
					}
				}
				return decision.Decision, decision.ToolName
			},
		}
		for _, event := range resolver.Resolve(ctx, turn.ToolCalls, executor, state, func() bool { return ctx.Err() != nil || e.AgentRuntime.RunSnapshot().Phase == AgentPhaseCancelling }) {
			sendStream(ctx, out, event)
		}

		if err := e.publishDraft(ctx, state); err != nil {
			sendStream(ctx, out, NewStreamEvent("error", err.Error(), nil))
			return
		}
		e.executeAndCommitTools(ctx, state, executor, out)
		e.RecordSessionMemory(ctx, state, false)
		state.TurnCount++
	}
}

func (e *CoreExecutionEngine) prefetchMemoryRecall(ctx context.Context, state *AgentState) context.CancelFunc {
	if state == nil || state.LastQuery == "" {
		return nil
	}
	if !e.Config.LongTermMemoryEnabled {
		return nil
	}
	recallCtx, cancel := context.WithCancel(context.Background())
	resultCh := make(chan []MemoryRecall, 1)
	go func() {
		query := state.MemoryQueryText
		if strings.TrimSpace(query) == "" {
			query = state.LastQuery
		}
		resultCh <- RunMemoryRecallWithEngine(recallCtx, e.Config, state, query, e.memoryEngineSnapshot())
	}()
	select {
	case recalled := <-resultCh:
		cancel()
		if len(recalled) > 0 {
			InjectRecalledMemories(state, recalled)
		}
		return nil
	case <-ctx.Done():
		return cancel
	}
}

type StreamAction struct {
	Event                *StreamEvent
	Break                bool
	Return               bool
	ConsecutiveAPIErrors int
}

func (e *CoreExecutionEngine) HandleStreamEvent(event map[string]any, turn *ModelTurn, executor *StreamingToolExecutor, state *AgentState, consecutiveAPIErrors int) StreamAction {
	switch stringFromAny(event["type"]) {
	case "text_delta":
		text := stringFromAny(event["text"])
		turn.FullText += text
		ev := NewStreamEvent("text", text, nil)
		return StreamAction{Event: &ev, ConsecutiveAPIErrors: consecutiveAPIErrors}
	case "message_id":
		turn.MessageID = stringFromAny(event["id"])
	case "thinking":
		text := stringFromAny(event["text"])
		block := map[string]any{"type": "thinking", "thinking": text, "signature": stringFromAny(event["signature"])}
		turn.ThinkingContent = append(turn.ThinkingContent, block)
		ev := NewStreamEvent("thinking", text, map[string]any{"signature": stringFromAny(event["signature"])})
		return StreamAction{Event: &ev, ConsecutiveAPIErrors: consecutiveAPIErrors}
	case "redacted_thinking":
		turn.ThinkingContent = append(turn.ThinkingContent, map[string]any{"type": "redacted_thinking", "data": stringFromAny(event["data"])})
		ev := NewStreamEvent("thinking", "[redacted thinking]", nil)
		return StreamAction{Event: &ev, ConsecutiveAPIErrors: consecutiveAPIErrors}
	case "thinking_delta":
		text := stringFromAny(event["text"])
		if len(turn.ThinkingContent) > 0 && turn.ThinkingContent[len(turn.ThinkingContent)-1]["type"] == "thinking" {
			last := turn.ThinkingContent[len(turn.ThinkingContent)-1]
			last["thinking"] = stringFromAny(last["thinking"]) + text
		}
		ev := NewStreamEvent("thinking", text, nil)
		return StreamAction{Event: &ev, ConsecutiveAPIErrors: consecutiveAPIErrors}
	case "signature_delta":
		if len(turn.ThinkingContent) > 0 && turn.ThinkingContent[len(turn.ThinkingContent)-1]["type"] == "thinking" {
			last := turn.ThinkingContent[len(turn.ThinkingContent)-1]
			last["signature"] = stringFromAny(last["signature"]) + stringFromAny(event["signature"])
		}
	case "tool_use":
		input := map[string]any{}
		if raw, ok := event["input"].(map[string]any); ok {
			input = raw
		}
		tc := coretools.ToolCall{ID: stringFromAny(event["id"]), Name: stringFromAny(event["name"]), Input: input}
		turn.ToolCalls = append(turn.ToolCalls, tc)
		if e.RuntimeEvents != nil {
			if err := e.RuntimeEvents.RecordToolPlanned(context.Background(), tc, len(turn.ToolCalls)-1); err != nil {
				ev := NewStreamEvent("error", err.Error(), map[string]any{"runtime_journal": true})
				return StreamAction{Return: true, Event: &ev, ConsecutiveAPIErrors: consecutiveAPIErrors}
			}
		}
		executor.AddTool(tc)
		ev := NewStreamEvent("tool_call", tc.Name, map[string]any{"id": tc.ID, "input": tc.Input})
		return StreamAction{Event: &ev, ConsecutiveAPIErrors: consecutiveAPIErrors}
	case "usage":
		turn.InputTokens = intFromAny(event["input_tokens"])
		turn.OutputTokens = intFromAny(event["output_tokens"])
	case "stop_reason":
		reason := stringFromAny(event["stop_reason"])
		if reason == "" {
			reason = stringFromAny(event["reason"])
		}
		if IsOutputTruncatedStopReason(reason) {
			turn.OutputTruncated = true
		}
	case "model_fallback":
		metadata := map[string]any{
			"primary_model":  event["primary_model"],
			"fallback_model": event["fallback_model"],
			"reason":         event["reason"],
		}
		ev := NewStreamEvent("model_fallback", stringFromAny(event["fallback_model"]), metadata)
		return StreamAction{Event: &ev, ConsecutiveAPIErrors: consecutiveAPIErrors}
	case "error":
		msg := stringFromAny(event["message"])
		metadata := map[string]any{}
		for _, key := range []string{"error_type", "provider", "status_code", "status", "raw_error", "request_url", "operation"} {
			if value, ok := event[key]; ok {
				metadata[key] = value
			}
		}
		if IsFatalAPIError(msg) {
			e.rememberAPIError(state, msg)
			ev := NewStreamEvent("error", msg, metadata)
			_ = e.publishDraft(context.Background(), state)
			return StreamAction{Return: true, Event: &ev, ConsecutiveAPIErrors: consecutiveAPIErrors}
		}
		if IsPTLErrorMessage(msg) && !IsFatalAPIError(msg) {
			turn.PTLDetected = true
			turn.PTLErrorMsg = msg
			return StreamAction{Break: true, ConsecutiveAPIErrors: consecutiveAPIErrors}
		}
		turn.StreamHadError = true
		consecutiveAPIErrors++
		if ShouldCommitFinalTextAfterStreamError(turn) {
			return StreamAction{Break: true, ConsecutiveAPIErrors: consecutiveAPIErrors}
		}
		e.rememberAPIError(state, msg)
		ev := NewStreamEvent("error", msg, metadata)
		if consecutiveAPIErrors >= MaxAPIErrorRetries {
			_ = e.publishDraft(context.Background(), state)
			return StreamAction{Return: true, Event: &ev, ConsecutiveAPIErrors: consecutiveAPIErrors}
		}
		metadata["recovered"] = true
		ev.Metadata = metadata
		return StreamAction{Break: true, Event: &ev, ConsecutiveAPIErrors: consecutiveAPIErrors}
	}
	return StreamAction{ConsecutiveAPIErrors: consecutiveAPIErrors}
}

func (e *CoreExecutionEngine) executeAndCommitTools(ctx context.Context, state *AgentState, executor *StreamingToolExecutor, out chan<- StreamEvent) {
	var toolResults []map[string]any
	for executor.HasPendingWork() {
		for _, event := range e.drainSkillPermissionEvents() {
			sendStream(ctx, out, event)
		}
		for _, progress := range executor.DrainProgress() {
			sendStream(ctx, out, BuildProgressEvent(progress))
		}
		toolResults = append(toolResults, executor.GetCompletedResults()...)
		if executor.HasPendingWork() {
			executor.WaitForActivity(ctx)
			if executor.HasPendingWork() && !executor.HasRunningWork() {
				break
			}
		}
	}
	for _, event := range e.drainSkillPermissionEvents() {
		sendStream(ctx, out, event)
	}
	toolResults = append(toolResults, executor.GetCompletedResults()...)
	toolResults = append(toolResults, executor.GetRemainingResults(ctx)...)
	for _, progress := range executor.DrainProgress() {
		sendStream(ctx, out, BuildProgressEvent(progress))
	}
	for _, tr := range toolResults {
		tid := stringFromAny(tr["tool_use_id"])
		slot := executor.GetSlot(tid)
		if slot == nil {
			continue
		}
		if slot.IsError {
			if tool := executor.Registry.Get(slot.TC.Name); tool != nil && tool.SupportsSiblingAbort() {
				executor.AbortSiblings(tid)
			}
		} else {
			e.PostToolUse(slot.TC, state)
			delete(state.DeniedToolCalls, slot.TC.Name)
			delete(state.ToolErrors, slot.TC.Name)
		}
		sendStream(ctx, out, NewStreamEvent("tool_result", "", map[string]any{
			"tool_use_id": tid,
			"tool_name":   slot.TC.Name,
			"result":      TruncateResult(slot.Truncated, 500),
			"is_error":    slot.IsError,
		}))
	}
	CommitToolResultsTurn(state, toolResults, executor)
	e.recordFabricToolEvents(ctx, state, toolResults, executor)
	e.injectMemoryRecallAfterTools(ctx, state, toolResults, executor)
	if pending, ok := executor.Context["_pending_skill_messages"].([]map[string]any); ok && len(pending) > 0 {
		state.Messages = append(state.Messages, pending...)
		executor.Context["_pending_skill_messages"] = []map[string]any{}
	}
}

func (e *CoreExecutionEngine) injectMemoryRecallAfterTools(ctx context.Context, state *AgentState,
	toolResults []map[string]any, executor *StreamingToolExecutor) {
	if e == nil || state == nil || !e.Config.LongTermMemoryEnabled || len(toolResults) == 0 {
		return
	}
	query := toolMemoryRecallQuery(state.LastQuery, toolResults, executor)
	if query == "" {
		return
	}
	// Tool follow-up recall is deliberately local. Tool inputs and outputs are
	// useful retrieval anchors but must not become a second remote expansion.
	recalled := RunMemoryRecallWithEngine(ctx, e.Config, state, query, e.memoryEngineSnapshot())
	for _, item := range recalled {
		if len(item.RecallIDs) > 0 {
			InjectRecalledMemories(state, recalled)
			return
		}
	}
}

func toolMemoryRecallQuery(original string, toolResults []map[string]any, executor *StreamingToolExecutor) string {
	parts := []string{strings.TrimSpace(original)}
	for _, result := range toolResults {
		toolUseID := stringFromAny(result["tool_use_id"])
		if executor != nil {
			if slot := executor.GetSlot(toolUseID); slot != nil {
				parts = append(parts, strings.TrimSpace(slot.TC.Name))
				if encoded, err := json.Marshal(slot.TC.Input); err == nil {
					parts = append(parts, string(encoded))
				}
			}
		}
		parts = append(parts, strings.TrimSpace(stringFromAny(result["content"])))
	}
	query := strings.TrimSpace(strings.Join(parts, "\n"))
	runes := []rune(query)
	if len(runes) > 4000 {
		query = string(runes[:4000])
	}
	return query
}

func (e *CoreExecutionEngine) requestSkillShellPermission(ctx context.Context, executor *StreamingToolExecutor, req skills.SkillShellPermissionRequest) bool {
	if e.AgentRuntime.YoloEnabled() {
		return true
	}
	token, ok := runTokenFromContext(ctx)
	if !ok {
		return false
	}
	event := NewStreamEvent("permission_needed", "", map[string]any{"skill_shell_request": req, "dangerous": true, "risk": "high"})
	ticket, _, err := e.AgentRuntime.BeginPermission(ctx, token, PermissionSkill, "", event, true)
	if err != nil {
		return false
	}
	if executor != nil {
		executor.signalActivity()
	}
	answer, err := e.AgentRuntime.AwaitPermission(ctx, ticket)
	return err == nil && permissionGranted(answer)
}
func (e *CoreExecutionEngine) drainSkillPermissionEvents() []StreamEvent {
	return e.AgentRuntime.DrainPermissionEvents(e.AgentRuntime.RunSnapshot().Token)
}

func (e *CoreExecutionEngine) BuildClient(maxTokens int, model string, thinkingBudgetTokens *int) (api.LLMClient, error) {
	if model == "" {
		model = e.Config.APIModel
	}
	return CreateConfiguredLLMClient(e.Config, model, maxTokens, thinkingBudgetTokens, api.DefaultRetryConfigPtr())
}

func (e *CoreExecutionEngine) PostToolUse(tc coretools.ToolCall, state *AgentState) {
}

func (e *CoreExecutionEngine) BuildMessages(state *AgentState) []map[string]any {
	messages := state.Messages
	if state.ContextSnapshot != nil {
		snapshot := *state.ContextSnapshot
		snapshot.Messages = agentContext.MessagesFromMaps("working", state.Messages)
		messages = agentContext.Render(snapshot)
	}
	repaired := RepairOrphanTools(messages)
	normalized := NormalizeMessages(repaired, e.Config.APIModel, state.RecentApiErrors)
	apiReady := StripMessageMetadata(normalized)
	return e.applyRollingCache(apiReady, state)
}

func (e *CoreExecutionEngine) applyRollingCache(messages []map[string]any, state *AgentState, models ...string) []map[string]any {
	if state == nil {
		return messages
	}
	if state.CacheBreakPoints == nil {
		state.CacheBreakPoints = mapset.NewSet[int]()
	}
	model := e.Config.APIModel
	if len(models) > 0 {
		model = models[0]
	}
	if !strings.HasPrefix(strings.ToLower(model), "claude-") {
		state.CacheBreakPoints.Clear()
		return messages
	}
	n := len(messages)
	if n < 8 {
		state.CacheBreakPoints.Clear()
		return messages
	}

	boundary := n - 4
	breakpoints := mapset.NewSet[int]()
	for point := range state.CacheBreakPoints.Iter() {
		if point >= 0 && point < boundary {
			breakpoints.Add(point)
		}
	}
	const maxHistoryBreakpoints = 2
	if breakpoints.Cardinality() < maxHistoryBreakpoints {
		candidates := []int{}
		if n >= 12 {
			candidates = append(candidates, max(n/3, 2))
		}
		if n >= 18 {
			candidates = append(candidates, max((2*n)/3, (n/3)+5))
		}
		for _, candidate := range candidates {
			if breakpoints.Cardinality() >= maxHistoryBreakpoints {
				break
			}
			if candidate >= boundary {
				continue
			}
			tooClose := false
			for existing := range breakpoints.Iter() {
				if absInt(candidate-existing) < 4 {
					tooClose = true
					break
				}
			}
			if !tooClose {
				breakpoints.Add(candidate)
			}
		}
	}
	if breakpoints.Cardinality() == 0 {
		state.CacheBreakPoints.Clear()
		return messages
	}
	state.CacheBreakPoints = breakpoints
	result := make([]map[string]any, 0, len(messages))
	for i, msg := range messages {
		if !breakpoints.Contains(i) {
			result = append(result, msg)
			continue
		}
		blocks := contentBlocks(msg["content"])
		if len(blocks) == 0 {
			result = append(result, msg)
			continue
		}
		newBlocks := append([]map[string]any{}, blocks...)
		last := copyMap(newBlocks[len(newBlocks)-1])
		last["cache_control"] = map[string]any{"type": "ephemeral"}
		newBlocks[len(newBlocks)-1] = last
		cp := copyMap(msg)
		cp["content"] = newBlocks
		result = append(result, cp)
	}
	return result
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

func VisibleToolResultIDs(messages []map[string]any) map[string]struct{} {
	ids := map[string]struct{}{}
	for _, message := range messages {
		blocks := contentBlocks(message["content"])
		for _, block := range blocks {
			if block["type"] != "tool_result" {
				continue
			}
			toolUseID, _ := block["tool_use_id"].(string)
			if toolUseID != "" {
				ids[toolUseID] = struct{}{}
			}
		}
	}
	return ids
}

func (e *CoreExecutionEngine) QueueCacheEdits(edits []agentContext.CacheEdit) {
	e.AgentRuntime.Context.QueueCacheEdits(edits)
}
func (e *CoreExecutionEngine) CacheEditStateSnapshot() RuntimeCacheEditState {
	return e.AgentRuntime.Context.CacheEditStateSnapshot()
}
func (e *CoreExecutionEngine) ConsumeCacheEditsForRequest(ids map[string]struct{}) *api.LLMRequestOptions {
	return e.consumeCacheEditsForRequest(ids)
}
func (e *CoreExecutionEngine) PinConsumedCacheEdits()     { e.pinConsumedCacheEdits() }
func (e *CoreExecutionEngine) RestoreInFlightCacheEdits() { e.restoreInFlightCacheEdits() }
func (e *CoreExecutionEngine) consumeCacheEditsForRequest(ids map[string]struct{}, models ...string) *api.LLMRequestOptions {
	return e.AgentRuntime.Context.ConsumeCacheEditsForRequest(ids, models...)
}
func (e *CoreExecutionEngine) pinConsumedCacheEdits(generations ...uint64) {
	e.AgentRuntime.Context.PinConsumedCacheEdits(generations...)
}
func (e *CoreExecutionEngine) restoreInFlightCacheEdits(generations ...uint64) {
	e.AgentRuntime.Context.RestoreInFlightCacheEdits(generations...)
}

func (e *CoreExecutionEngine) RecordSessionMemory(ctx context.Context, state *AgentState, force bool) {
	if e == nil || state == nil || e.sessionMemory == nil || e.SessionID == "" || !e.Config.SessionMemoryEnabled {
		return
	}
	cfg := e.Config
	cfg.SessionMemoryAgentID = e.sessionMemoryAgentID()
	var err error
	if e.RuntimeEvents != nil && e.RuntimeEvents.EventStore() != nil {
		err = e.sessionMemory.ObserveEvents(ctx, cfg, e.SessionID, e.RuntimeEvents.EventStore(), force)
	} else {
		err = e.sessionMemory.Observe(ctx, cfg, e.SessionID, state.Messages, force)
	}
	if err != nil {
		slog.Warn("session memory observe failed", "session_id", e.SessionID, "error", err)
	}
	if e.StateObserver != nil {
		snapshot := e.AgentRuntime.Snapshot().LegacyState()
		e.StateObserver(&snapshot)
	}
}

func (e *CoreExecutionEngine) sessionMemoryAgentID() string {
	if e == nil || strings.TrimSpace(e.AgentID) == "" {
		return "main"
	}
	return strings.TrimSpace(e.AgentID)
}

func (e *CoreExecutionEngine) memoryAgentType() string {
	if e == nil {
		return "main"
	}
	if strings.TrimSpace(e.AgentType) != "" {
		return strings.TrimSpace(e.AgentType)
	}
	return e.sessionMemoryAgentID()
}

func (e *CoreExecutionEngine) applyMemoryRuntimeIdentity(state *AgentState) {
	if e == nil || state == nil {
		return
	}
	state.MemorySessionID = strings.TrimSpace(e.SessionID)
	state.MemoryAgentID = e.sessionMemoryAgentID()
	state.MemoryAgentType = e.memoryAgentType()
	state.MemoryTeamName = strings.TrimSpace(e.TeamName)
	state.MemoryTeamSessionID = strings.TrimSpace(e.TeamSessionID)
	state.MemoryTeamAgentID = strings.TrimSpace(e.TeamAgentID)
}

func (e *CoreExecutionEngine) rebuildSystemPromptAfterHistoryReplace(state *AgentState) {
	if state == nil {
		return
	}
	memorySection := ""
	if e.Config.LongTermMemoryEnabled {
		memorySection = agentContext.BuildMemorySection(&e.Config)
	}
	if prompt, err := agentContext.BuildSystemPromptWithConfig(e.Config, memorySection); err == nil && strings.TrimSpace(prompt) != "" {
		state.SystemPrompt = appendHarnessSystemPrompt(prompt, e.Config.HarnessMode)
	}
}

func (e *CoreExecutionEngine) clearRuntimeCompressionStateAfterHistoryReplace(state *AgentState) {
	e.AgentRuntime.Context.ResetCache()
	if state != nil && state.CacheBreakPoints != nil {
		state.CacheBreakPoints.Clear()
	}
}

func recentReadFilesForPostCompact(state *AgentState, limit int) []map[string]string {
	if state == nil || limit <= 0 || len(state.ReadFileState) == 0 {
		return nil
	}
	type entry struct {
		timestamp float64
		path      string
		content   string
	}
	var entries []entry
	for path, fileState := range state.ReadFileState {
		if fileState.IsPartialView || fileState.Content == "" {
			continue
		}
		entries = append(entries, entry{timestamp: fileState.TimeStamp, path: path, content: fileState.Content})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].timestamp < entries[j].timestamp })
	if len(entries) > limit {
		entries = entries[len(entries)-limit:]
	}
	out := make([]map[string]string, 0, len(entries))
	for _, item := range entries {
		out = append(out, map[string]string{"path": item.path, "content": item.content})
	}
	return out
}

func (e *CoreExecutionEngine) injectSkillRuntimeAttachments(state *AgentState) {
	if e.skillDiscovery == nil || state == nil {
		return
	}
	cwd := skills.ResolveSkillContextCWD(e.Config.CWD, map[string]any{"cwd": e.Config.CWD})
	if listing := e.skillDiscovery.BuildListingMessage(e.Config.APIMaxTokens, cwd); listing != nil {
		insertBeforeCurrentUserMessage(state, listing)
	}
	if e.skillPersistence != nil && e.isSkillRecoveryReady("main") {
		if recovery := e.skillPersistence.BuildRecoveryMessage("main"); recovery != nil {
			insertBeforeCurrentUserMessage(state, recovery)
		}
	}
}

func (e *CoreExecutionEngine) stripSkillMessages(state *AgentState, sources map[string]struct{}) {
	if state == nil {
		return
	}
	state.Messages = skills.StripSkillContextMessages(state.Messages, sources)
}

func (e *CoreExecutionEngine) injectPendingTaskNotifications(state *AgentState) {
	if state == nil || e.TaskRuntime == nil {
		return
	}
	for _, message := range e.TaskRuntime.DrainPendingNotifications("main") {
		insertBeforeCurrentUserMessage(state, message)
	}
}

func (e *CoreExecutionEngine) isSkillRecoveryReady(agentScope string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.skillRecoveryReady != nil && e.skillRecoveryReady[agentScope]
}

func resolveInlineThinkingBudget(effort any) *int {
	switch v := effort.(type) {
	case nil:
		return nil
	case int:
		if v <= 0 {
			return nil
		}
		return &v
	case int64:
		if v <= 0 {
			return nil
		}
		budget := int(v)
		return &budget
	case bool:
		if !v {
			return nil
		}
		budget := 1
		return &budget
	case string:
		if budget, ok := skills.EffortThinkingBudgets[v]; ok {
			return &budget
		}
	}
	return nil
}

func (e *CoreExecutionEngine) CheckPermissionChain(tc coretools.ToolCall, tool coretools.Tool, state *AgentState) bool {
	if tool == nil {
		return true
	}
	if state.PermissionState != nil && state.PermissionState.YoloMode {
		return false
	}
	validated, err := tool.DecodeInput(tc.Input)
	if err != nil {
		return true
	}
	if tool.IsReadOnly(validated) {
		return false
	}
	if tc.Name == "run_shell" && state.PermissionState != nil {
		command := stringFromAny(tc.Input["command"])
		prefix := bashpkg.GetSimpleCommandPrefix(command)
		if prefix != "" && state.PermissionState.IsCommandPrefixConfirmed(prefix) {
			permission := bashpkg.AnalyzeCommandPermissions(command)
			if !permission.NeedsUserDecision {
				return false
			}
		}
	}
	if state.PermissionState != nil && state.PermissionState.IsToolConfirmed(tc.Name) {
		return false
	}
	if tool.ConfirmsFilePaths() && state.PermissionState != nil {
		if fp := stringFromAny(tc.Input["file_path"]); fp != "" && state.PermissionState.IsPathConfirmed(fp) {
			return false
		}
	}
	return tool.NeedsPermission(validated)
}

func (e *CoreExecutionEngine) rememberAPIError(state *AgentState, msg string) {
	state.RecentApiErrors = append(state.RecentApiErrors, msg)
	if len(state.RecentApiErrors) > 5 {
		state.RecentApiErrors = state.RecentApiErrors[len(state.RecentApiErrors)-5:]
	}
}

func (e *CoreExecutionEngine) apiErrorEvent(msg string, metadata map[string]any) StreamEvent {
	return NewStreamEvent("error", fmt.Sprintf("API error after %d retries: %s. The request may be malformed. Try /clear to start a fresh session.", MaxAPIErrorRetries, msg), metadata)
}

func IsFatalAPIError(msg string) bool {
	low := strings.ToLower(msg)
	for _, kw := range fatalAPIErrorKeywords {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

func BuildSystemPrompt(cwd string) string {
	prompt, err := agentContext.BuildSystemPrompt(cwd, "")
	if err == nil && strings.TrimSpace(prompt) != "" {
		return prompt
	}
	return "You are LuminaCode, a general-purpose local agent. Work in the current directory: " + cwd
}

func insertBeforeCurrentUserMessage(state *AgentState, message map[string]any) {
	if state == nil || len(state.Messages) == 0 {
		return
	}
	insertAt := len(state.Messages)
	last := state.Messages[len(state.Messages)-1]
	if role, _ := last["role"].(string); role == "user" {
		insertAt = len(state.Messages) - 1
	}
	state.Messages = append(state.Messages, nil)
	copy(state.Messages[insertAt+1:], state.Messages[insertAt:])
	state.Messages[insertAt] = message
}

func findLuminaRoot(cwd string) string {
	if cwd == "" {
		var err error
		cwd, err = os.Getwd()
		if err != nil {
			return ""
		}
	}
	current, err := filepath.Abs(cwd)
	if err != nil {
		current = cwd
	}
	if info, err := os.Stat(current); err == nil && !info.IsDir() {
		current = filepath.Dir(current)
	}
	for {
		if _, err := os.Stat(apppaths.ProjectLocalRoot(current)); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

func sendStream(ctx context.Context, out chan<- StreamEvent, event StreamEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case out <- event:
		return true
	}
}

func compactToolRoots(values ...string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if abs, err := filepath.Abs(value); err == nil {
			value = abs
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func ClonePermissionState(state *security.PermissionState) *security.PermissionState {
	if state == nil {
		return security.DefaultPermissionState()
	}
	m, err := state.ToMap()
	if err != nil {
		return security.DefaultPermissionState()
	}
	clone, err := security.GetPermissionStateFromMap(m)
	if err != nil {
		return security.DefaultPermissionState()
	}
	return &clone
}

func BuildSubagentState(parentState *AgentState, _ string) AgentState {
	child := NewAgentState()
	if parentState != nil {
		child.PermissionState = ClonePermissionState(parentState.PermissionState)
	}
	return child
}
