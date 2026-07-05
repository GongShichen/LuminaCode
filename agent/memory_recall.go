package agent

import (
	"context"
	"path/filepath"
	"strings"
	"sync"

	"LuminaCode/config"
	"LuminaCode/memory"
	coretools "LuminaCode/tools"
)

type MemoryRecall = memory.MemoryRecall
type MemoryClientFactory = memory.ClientFactory

func CollectToolObservations(executor *StreamingToolExecutor, toolResults []map[string]any) []map[string]any {
	var observations []map[string]any
	for _, result := range toolResults {
		if result["type"] != "tool_result" {
			continue
		}
		tid := stringFromAny(result["tool_use_id"])
		if tid == "" {
			continue
		}
		slot := executor.GetSlot(tid)
		if slot == nil {
			continue
		}
		content := slot.Truncated
		if content == "" {
			content = stringFromAny(result["content"])
		}
		observations = append(observations, map[string]any{
			"tool_use_id": tid,
			"tool_name":   slot.TC.Name,
			"input":       slot.TC.Input,
			"call":        slot.TC,
			"tool":        executor.Registry.Get(slot.TC.Name),
			"content":     content,
			"is_error":    slot.IsError,
		})
	}
	return observations
}

func RunMemoryRecallWithQuery(ctx context.Context, state *AgentState, query string) []MemoryRecall {
	return RunMemoryRecallWithConfig(ctx, config.GetConfig(), state, query, nil, nil)
}

func RunMemoryRecallWithConfig(ctx context.Context, cfg config.Config, state *AgentState, query string, clientFactory MemoryClientFactory, recentTools []string) []MemoryRecall {
	if state == nil || query == "" || clientFactory == nil {
		return nil
	}
	if !cfg.AutoMemoryEnabled || cfg.AutoMemoryDirectory == nil {
		return nil
	}
	memoryDir := *cfg.AutoMemoryDirectory
	if memoryDir == "" {
		return nil
	}
	if !filepath.IsAbs(memoryDir) {
		memoryDir = filepath.Join(cfg.CWD, memoryDir)
	}
	alreadySurfaced := memory.RecalledMemoryIDs(state.Messages)
	var mainResults []MemoryRecall
	var agentResults []MemoryRecall
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		mainResults = memory.RecallMemoriesForQuery(ctx, query, memoryDir, clientFactory, recentTools, alreadySurfaced)
	}()
	go func() {
		defer wg.Done()
		agentResults = memory.RecallAgentMemoriesForQuery(ctx, "main", cfg.CWD, query, clientFactory, recentTools, alreadySurfaced)
	}()
	wg.Wait()
	return append(mainResults, agentResults...)
}

func AppendFreshRecalledMemories(ctx context.Context, state *AgentState, toolObservations []map[string]any) {
	AppendFreshRecalledMemoriesWithConfig(ctx, config.GetConfig(), state, toolObservations, nil)
}

func AppendFreshRecalledMemoriesWithConfig(ctx context.Context, cfg config.Config, state *AgentState, toolObservations []map[string]any, clientFactory MemoryClientFactory) {
	if state == nil || clientFactory == nil {
		return
	}
	if !cfg.AutoMemoryEnabled || cfg.AutoMemoryDirectory == nil || state.LastQuery == "" {
		return
	}
	if !ShouldTriggerFollowupRecall(toolObservations) {
		return
	}
	recallQuery := BuildFollowupRecallQuery(state.LastQuery, toolObservations)
	if recallQuery == "" {
		return
	}
	recalled := RunMemoryRecallWithConfig(ctx, cfg, state, recallQuery, clientFactory, GetRecentToolNames(state.Messages))
	if len(recalled) == 0 {
		return
	}
	message := memory.BuildRecalledMemoriesMessage(recalled)
	if message != nil {
		state.Messages = append(state.Messages, message)
	}
}

func BuildFollowupRecallQuery(taskQuery string, toolObservations []map[string]any) string {
	var errors []string
	var observations []string
	for _, observation := range toolObservations {
		call, ok := observation["call"].(coretools.ToolCall)
		if !ok {
			call = coretools.ToolCall{
				Name:  stringFromAny(observation["tool_name"]),
				Input: mapFromAny(observation["input"]),
			}
		}
		details := FormatToolInputForRecall(call.Input)
		label := call.Name
		if details != "" {
			label += " (" + details + ")"
		}
		line := "- " + label + ": " + ClipRecallText(stringFromAny(observation["content"]))
		if isTruthy(observation["is_error"]) {
			errors = append(errors, line)
		} else if IsReadLikeObservation(observation) {
			observations = append(observations, line)
		}
	}
	parts := []string{"Task: " + taskQuery}
	if len(errors) > 0 {
		parts = append(parts, "", "Recent tool errors:")
		parts = append(parts, firstStrings(errors, 3)...)
	} else if len(observations) > 0 {
		parts = append(parts, "", "Recent observations:")
		parts = append(parts, firstStrings(observations, 3)...)
	}
	return strings.Join(parts, "\n")
}

func firstStrings(values []string, limit int) []string {
	if len(values) <= limit {
		return values
	}
	return values[:limit]
}

func mapFromAny(raw any) map[string]any {
	switch v := raw.(type) {
	case map[string]any:
		return v
	case map[string]string:
		out := make(map[string]any, len(v))
		for key, value := range v {
			out[key] = value
		}
		return out
	default:
		return map[string]any{}
	}
}
