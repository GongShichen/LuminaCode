package agentbench

import (
	"context"
	"strings"
	"time"

	"LuminaCode/agent"
	"LuminaCode/cluster"
	"LuminaCode/config"
)

type HeadlessAgentRunner struct {
	engineFactory agent.QueryEngineFactory
}

func NewHeadlessAgentRunner(engineFactory agent.QueryEngineFactory) *HeadlessAgentRunner {
	return &HeadlessAgentRunner{engineFactory: engineFactory}
}

func (r *HeadlessAgentRunner) Run(ctx context.Context, cfg config.Config, prompt string, sessionID string) AgentRunResult {
	return r.RunAt(ctx, cfg, prompt, sessionID, time.Time{})
}

func (r *HeadlessAgentRunner) RunAt(ctx context.Context, cfg config.Config, prompt string, sessionID string, queryTime time.Time) AgentRunResult {
	if r == nil || r.engineFactory == nil {
		return AgentRunResult{ErrorType: "runner_dependency_error: query engine factory is required"}
	}
	start := time.Now()
	timeline := []TimelineEvent{
		newTimelineEvent(start, start, "first_model_request", nil),
	}
	cfg.Yolo = true
	previousConfig := config.GetConfig()
	config.SetConfig(cfg)
	defer config.SetConfig(previousConfig)
	engine := r.engineFactory.Create(cfg, cluster.RuntimeIdentity{TenantID: "local", SessionID: sessionID})
	defer engine.Shutdown()
	state := agent.NewAgentState()
	state.MemoryQueryTime = queryTime
	state.MemoryQueryTimeExplicit = true
	if cfg.Yolo && state.PermissionState != nil {
		state.PermissionState.YoloMode = true
	}

	var result AgentRunResult
	var final strings.Builder
	var sawText bool
	var sawTool bool
	for event := range engine.SubmitMessage(ctx, prompt, &state, sessionID) {
		result.Events = append(result.Events, event)
		switch event.Type {
		case "text":
			final.WriteString(event.Content)
			if !sawText {
				sawText = true
				ms := float64(time.Since(start).Milliseconds())
				result.TTFTMillis = &ms
				timeline = append(timeline, newTimelineEvent(start, time.Now(), "first_text_delta", nil))
			}
		case "tool_call":
			result.ToolCalls++
			if !sawTool {
				sawTool = true
				ms := float64(time.Since(start).Milliseconds())
				result.FirstToolCallMS = &ms
				timeline = append(timeline, newTimelineEvent(start, time.Now(), "first_tool_call", map[string]any{
					"tool": event.Content,
				}))
			}
		case "permission_needed":
			engine.ResolveMCPTrust(true)
			engine.ResolveSkillPermission(true)
			engine.ResolvePermission(agent.PermissionOnce, event.Content)
		case "error":
			if recovered, _ := event.Metadata["recovered"].(bool); recovered {
				result.TransientErrors = append(result.TransientErrors, event.Content)
				continue
			}
			result.ErrorType = event.Content
		case "done":
			timeline = append(timeline, newTimelineEvent(start, time.Now(), "final_answer", nil))
		}
	}
	if result.ErrorType == "" && ctx.Err() != nil {
		result.ErrorType = "agent_timeout: " + ctx.Err().Error()
	}
	result.FinalText = final.String()
	result.InputTokens, result.OutputTokens = state.TokenTotals()
	result.Timeline = timeline
	return result
}

func newTimelineEvent(start time.Time, at time.Time, name string, metadata map[string]any) TimelineEvent {
	return TimelineEvent{
		Name:              name,
		ElapsedMillis:     at.Sub(start).Milliseconds(),
		TimestampUnixNano: at.UnixNano(),
		Metadata:          metadata,
	}
}
