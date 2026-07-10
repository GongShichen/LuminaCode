package agent

import (
	"context"
	"fmt"

	"LuminaCode/security"
	coretools "LuminaCode/tools"
	bashpkg "LuminaCode/tools/bash"
)

const (
	PermissionOnce   = "once"
	PermissionAlways = "always"
	PermissionDeny   = "deny"
)

type PermissionCheck func(coretools.ToolCall, coretools.Tool, *AgentState) bool
type PreToolHook func(context.Context, coretools.ToolCall)
type PermissionPrompt func(context.Context, StreamEvent) (string, string)

type PermissionResolver struct {
	Registry        *coretools.ToolRegistry
	CheckPermission PermissionCheck
	PreToolHook     PreToolHook
	RequestDecision PermissionPrompt
	EnableYolo      func(*AgentState)
}

func DeniedToolResultContent(state *AgentState, tc coretools.ToolCall) string {
	state.DeniedToolCalls[tc.Name] = state.DeniedToolCalls[tc.Name] + 1
	denials := state.DeniedToolCalls[tc.Name]
	if denials < 3 {
		return "User denied this action."
	}
	return fmt.Sprintf("User denied this action.\n\n<system_hint>\nThe user has explicitly denied '%s' %d times in a row. This action will NOT be approved on retry. DO NOT request it again. You MUST pivot to a completely different approach or explain to the user why this action is necessary and ask them to approve it directly.\n</system_hint>", tc.Name, denials)
}

func (r *PermissionResolver) Resolve(ctx context.Context, toolCalls []coretools.ToolCall, executor *StreamingToolExecutor, state *AgentState, isAborted func() bool) []StreamEvent {
	var events []StreamEvent
	for _, tc := range toolCalls {
		if isAborted != nil && isAborted() {
			if executor.IsQueued(tc.ID) {
				executor.DenyTool(tc.ID)
			}
			continue
		}
		if r.PreToolHook != nil {
			r.PreToolHook(ctx, tc)
		}
		if !executor.IsQueued(tc.ID) {
			continue
		}
		tool := r.Registry.Get(tc.Name)
		if tool == nil {
			executor.TryStartQueued(tc.ID)
			continue
		}
		validated, _ := tool.DecodeInput(tc.Input)
		if requirement, ok := sandboxUnavailableRequirement(tool, tc, state); ok {
			event := NewStreamEvent("permission_needed", tc.Name, map[string]any{
				"tool_call":           tc,
				"risk":                "high",
				"dangerous":           true,
				"sandbox_unavailable": true,
				"sandbox_platform":    requirement.platform,
				"sandbox_backend":     requirement.backend,
				"enables_yolo":        true,
				"command":             stringFromAny(tc.Input["command"]),
				"reason":              requirement.reason,
			})
			decision := PermissionDeny
			if r.RequestDecision != nil {
				decision, _ = r.RequestDecision(ctx, event)
			} else {
				events = append(events, event)
			}
			if decision != PermissionOnce && decision != PermissionAlways && decision != "true" {
				denyContent := DeniedToolResultContent(state, tc)
				executor.DenyTool(tc.ID)
				events = append(events, NewStreamEvent("tool_result", denyContent, map[string]any{"tool_use_id": tc.ID, "denied": true}))
				continue
			}
			if state.PermissionState == nil {
				state.PermissionState = security.DefaultPermissionState()
			}
			state.PermissionState.YoloMode = true
			if r.EnableYolo != nil {
				r.EnableYolo(state)
			}
			delete(state.DeniedToolCalls, tc.Name)
			delete(state.ToolErrors, tc.Name)
			events = append(events, NewStreamEvent("text", "\n[system] YOLO mode enabled because the OS sandbox backend is unavailable. This and subsequent shell commands will run without OS sandbox isolation.\n", map[string]any{
				"yolo_enabled": true,
				"reason":       "sandbox_unavailable",
			}))
			executor.TryStartQueued(tc.ID)
			continue
		}
		if tool.IsReadOnly(validated) {
			executor.TryStartQueued(tc.ID)
			continue
		}
		needsPermission := true
		if r.CheckPermission != nil {
			needsPermission = r.CheckPermission(tc, tool, state)
		}
		risk := "normal"
		if needsPermission && tool.HasCommandClassifier() {
			command := stringFromAny(tc.Input["command"])
			if command != "" {
				switch bashpkg.ClassifyCommand(command).CommandClass {
				case bashpkg.CommandClassSafe:
					needsPermission = false
				case bashpkg.CommandClassDangerous:
					risk = "high"
				}
			}
		}
		if risk == "normal" && tool.IsDestructive(validated) {
			risk = "high"
		}
		if needsPermission {
			event := NewStreamEvent("permission_needed", tc.Name, map[string]any{
				"tool_call": tc,
				"risk":      risk,
				"dangerous": risk == "high",
			})
			decision := PermissionDeny
			if r.RequestDecision != nil {
				decision, _ = r.RequestDecision(ctx, event)
			} else {
				events = append(events, event)
			}
			if decision == PermissionOnce || decision == PermissionAlways || decision == "true" {
				if decision == PermissionAlways {
					persistAlwaysGrant(state, tc, tool)
				}
				delete(state.DeniedToolCalls, tc.Name)
				delete(state.ToolErrors, tc.Name)
			} else {
				denyContent := DeniedToolResultContent(state, tc)
				executor.DenyTool(tc.ID)
				events = append(events, NewStreamEvent("tool_result", denyContent, map[string]any{"tool_use_id": tc.ID, "denied": true}))
				continue
			}
		}
		executor.TryStartQueued(tc.ID)
	}
	return events
}

type sandboxRequirement struct {
	platform string
	backend  string
	reason   string
}

func sandboxUnavailableRequirement(tool coretools.Tool, tc coretools.ToolCall, state *AgentState) (sandboxRequirement, bool) {
	if tc.Name != "run_shell" || (state != nil && state.YoloEnabled()) {
		return sandboxRequirement{}, false
	}
	provider, ok := tool.(interface {
		SandboxStatus() (available bool, platform string, backend string)
	})
	if !ok {
		return sandboxRequirement{}, false
	}
	available, platform, backend := provider.SandboxStatus()
	if available {
		return sandboxRequirement{}, false
	}
	if backend == "" {
		backend = "supported OS sandbox"
	}
	reason := fmt.Sprintf("The %s sandbox backend (%s) is unavailable. Allowing this command will enable YOLO mode for the current session and run this and subsequent shell commands without OS sandbox isolation.", platform, backend)
	return sandboxRequirement{platform: platform, backend: backend, reason: reason}, true
}

func persistAlwaysGrant(state *AgentState, tc coretools.ToolCall, tool coretools.Tool) {
	if state == nil || state.PermissionState == nil {
		return
	}
	if tc.Name == "run_shell" {
		if command := stringFromAny(tc.Input["command"]); command != "" {
			if prefix := bashpkg.GetSimpleCommandPrefix(command); prefix != "" {
				state.PermissionState.ConfirmCommandPrefix(prefix)
			}
		}
		return
	}
	if tool.ConfirmsFilePaths() {
		if filePath := stringFromAny(tc.Input["file_path"]); filePath != "" {
			state.PermissionState.ConfirmPath(filePath)
		}
		return
	}
	state.PermissionState.ConfirmTool(tc.Name)
}
