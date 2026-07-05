package agent

import (
	"context"
	"fmt"

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
