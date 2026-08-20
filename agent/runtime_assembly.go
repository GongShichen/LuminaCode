package agent

import (
	"context"

	"LuminaCode/harness"
	"LuminaCode/memory"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"
)

const (
	CapabilityEventStore       harness.CapabilityKey = "event_store"
	CapabilityToolRegistry     harness.CapabilityKey = "tool_registry"
	CapabilityPermissionPolicy harness.CapabilityKey = "permission_policy"
	CapabilityContextCompiler  harness.CapabilityKey = "context_compiler"
	CapabilityMemory           harness.CapabilityKey = "memory"
	CapabilitySkill            harness.CapabilityKey = "skill"
	CapabilityMCP              harness.CapabilityKey = "mcp"
	CapabilitySubagent         harness.CapabilityKey = "subagent"
	CapabilitySandbox          harness.CapabilityKey = "sandbox"
)

type RunHookContext struct {
	SessionID string
	Message   map[string]any
	State     *AgentState
}

type StepHookContext struct {
	SessionID string
	State     *AgentState
	StepNo    int
}

type ContextBuildHookContext struct {
	SessionID   string
	State       *AgentState
	Messages    []map[string]any
	ToolSchemas []map[string]any
}

type ModelResponseHookContext struct {
	SessionID string
	State     *AgentState
	Turn      *ModelTurn
}

type MemoryPreparingHookContext struct {
	SessionID string
	State     *AgentState
}

type SkillPreparingHookContext struct {
	SessionID     string
	State         *AgentState
	InlineRuntime skills.InlineSkillRuntime
}

type MCPPreparingHookContext struct {
	SessionID        string
	ExecutionContext coretools.ExecutionContext
}

type RuntimeHooks struct {
	RunStarting           harness.HookPoint[RunHookContext]
	StepPreparing         harness.HookPoint[StepHookContext]
	ContextBuilding       harness.HookPoint[ContextBuildHookContext]
	ModelResponseReceived harness.HookPoint[ModelResponseHookContext]
	MemoryPreparing       harness.HookPoint[MemoryPreparingHookContext]
	SkillPreparing        harness.HookPoint[SkillPreparingHookContext]
	MCPPreparing          harness.HookPoint[MCPPreparingHookContext]
}

// AttachRuntime connects engine-owned capabilities and the default phase
// hooks. Integrations can replace or extend behavior through ordered hooks
// without changing the main loop.
func (e *CoreExecutionEngine) AttachRuntime(assembly *RuntimeAssembly) error {
	if e == nil || assembly == nil {
		return nil
	}
	e.Runtime = assembly
	providers := []struct {
		key   harness.CapabilityKey
		value any
	}{
		{CapabilityPermissionPolicy, e}, {CapabilityContextCompiler, e},
		{CapabilityMemory, e}, {CapabilityMCP, e},
		{CapabilitySubagent, e.TaskRuntime},
		{CapabilitySandbox, map[string]any{"cwd": e.Config.CWD, "runtime_dir": e.Config.ProjectRuntimeDir}},
	}
	if e.skillRegistry != nil {
		providers = append(providers, struct {
			key   harness.CapabilityKey
			value any
		}{CapabilitySkill, e.skillRegistry})
	}
	for _, provider := range providers {
		if _, err := assembly.Session.Provide(provider.key, provider.value, 0); err != nil {
			return err
		}
	}
	assembly.Hooks.MemoryPreparing.Register(0, func(_ context.Context, input MemoryPreparingHookContext) (harness.HookResult[MemoryPreparingHookContext], error) {
		e.applyMemoryRuntimeIdentity(input.State)
		input.State.Messages = memory.StripMemoryContextMessages(input.State.Messages, "")
		if e.extraction != nil && e.extraction.HasPendingResult() {
			if result := e.extraction.ConsumeResult(); result != "" && len(input.State.Messages) > 0 {
				insertBeforeCurrentUserMessage(input.State, map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": result}}, "isMeta": true})
			}
		}
		return harness.HookResult[MemoryPreparingHookContext]{Value: input}, nil
	})
	assembly.Hooks.SkillPreparing.Register(0, func(ctx context.Context, input SkillPreparingHookContext) (harness.HookResult[SkillPreparingHookContext], error) {
		e.stripSkillMessages(input.State, map[string]struct{}{skills.SkillListingSource: {}, skills.SkillRecoverySource: {}})
		e.maybeCompressContext(ctx, input.State)
		e.injectSkillRuntimeAttachments(input.State)
		e.injectPendingTaskNotifications(input.State)
		input.InlineRuntime = skills.CollectInlineSkillRuntime(input.State.Messages)
		return harness.HookResult[SkillPreparingHookContext]{Value: input}, nil
	})
	assembly.Hooks.MCPPreparing.Register(0, func(_ context.Context, input MCPPreparingHookContext) (harness.HookResult[MCPPreparingHookContext], error) {
		if e.Config.MCPEnabled {
			e.ensureMCPTools(input.ExecutionContext)
		}
		return harness.HookResult[MCPPreparingHookContext]{Value: input}, nil
	})
	return nil
}

type RuntimeAssembly struct {
	Global  *harness.RuntimeScope
	Session *harness.RuntimeScope
	Hooks   RuntimeHooks
}

func NewRuntimeAssembly(sessionID string, store harness.EventStore, registry *coretools.ToolRegistry) (*RuntimeAssembly, error) {
	global := harness.NewScope("global", harness.ScopeGlobal, nil)
	sessionScope, err := global.Child(sessionID, harness.ScopeSession)
	if err != nil {
		return nil, err
	}
	if store != nil {
		if _, err := sessionScope.Provide(CapabilityEventStore, store, 0); err != nil {
			return nil, err
		}
	}
	if registry != nil {
		if _, err := sessionScope.Provide(CapabilityToolRegistry, registry, 0); err != nil {
			return nil, err
		}
	}
	return &RuntimeAssembly{Global: global, Session: sessionScope}, nil
}

func (a *RuntimeAssembly) AppendHookEvents(ctx context.Context, recorder *RuntimeEventRecorder, events []harness.PendingEvent) error {
	if a == nil || recorder == nil || len(events) == 0 {
		return nil
	}
	return recorder.AppendPending(ctx, events...)
}

func (a *RuntimeAssembly) Describe() map[string]any {
	if a == nil || a.Global == nil {
		return map[string]any{"available": false}
	}
	return map[string]any{
		"available": true,
		"scopes":    a.Global.DescribeTree(),
		"hooks": map[string]int{
			"run_starting":            a.Hooks.RunStarting.Len(),
			"step_preparing":          a.Hooks.StepPreparing.Len(),
			"context_building":        a.Hooks.ContextBuilding.Len(),
			"model_response_received": a.Hooks.ModelResponseReceived.Len(),
			"memory_preparing":        a.Hooks.MemoryPreparing.Len(),
			"skill_preparing":         a.Hooks.SkillPreparing.Len(),
			"mcp_preparing":           a.Hooks.MCPPreparing.Len(),
		},
	}
}

func (a *RuntimeAssembly) Close() error {
	if a == nil || a.Global == nil {
		return nil
	}
	return a.Global.Close()
}
