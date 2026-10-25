package agent

import (
	"LuminaCode/agentContext"
	"LuminaCode/api"
	"context"
)

func (e *CoreExecutionEngine) StateSnapshot() *AgentState {
	s := e.AgentRuntime.Snapshot().LegacyState()
	return &s
}

func (e *CoreExecutionEngine) publishDraft(ctx context.Context, state *AgentState) error {
	if state == nil || state.owner == nil {
		return nil
	}
	return state.owner.Commit(ctx, state)
}

func (e *CoreExecutionEngine) prepareContext(ctx context.Context, state *AgentState, tools []map[string]any, mode agentContext.CompactMode, model string) (agentContext.CompiledContext, error) {
	result, err := e.prepareContextResult(ctx, state, tools, mode, model)
	return result.Compiled, err
}

func (e *CoreExecutionEngine) prepareContextResult(ctx context.Context, state *AgentState, tools []map[string]any, mode agentContext.CompactMode, model string) (agentContext.CompactionResult, error) {
	if err := e.publishDraft(ctx, state); err != nil {
		return agentContext.CompactionResult{}, err
	}
	cfg := e.Config
	cfg.APIModel = model
	manager := e.AgentRuntime.Context
	manager.Configure(cfg)
	snapshot := e.AgentRuntime.Snapshot()
	files := e.AgentRuntime.RecentFiles()
	input := agentContext.BuildInput{SystemPrompt: state.SystemPrompt, ToolSchemas: tools, Evidence: snapshot.Transcript, RestoreFiles: files,
		Normalize: func(messages []map[string]any) []map[string]any {
			return StripMessageMetadata(NormalizeMessages(RepairOrphanTools(messages), model, state.RecentApiErrors))
		},
	}
	if mode != agentContext.CompactAuto || agentContext.RequestBudget(input.SystemPrompt, agentContext.Render(snapshot.Context), tools, cfg.CompressionContextLimit()).Total > cfg.CompressionTriggerTokens() {
		_ = e.AgentRuntime.StartRecovery(state.RunToken(), RecoveryContext, "context compression")
		e.RecordSessionMemory(ctx, state, false)
	}
	result, err := manager.Compact(ctx, mode, input)
	if result.HistoryReplaced {
		e.rebuildSystemPromptAfterHistoryReplace(state)
		result.Next.SystemPrompt = state.SystemPrompt
		result.Compiled.SystemPrompt = state.SystemPrompt
		validated, validateErr := manager.Validate(result.Compiled)
		if validateErr != nil {
			kept := result.Next.Attachments[:0]
			for _, a := range result.Next.Attachments {
				if a.Source != "post_compact_files" {
					kept = append(kept, a)
				}
			}
			result.Next.Attachments = kept
			result.Compiled.Messages = input.Normalize(agentContext.Render(result.Next))
			validated, validateErr = manager.Validate(result.Compiled)
		}
		result.Compiled = validated
		result.Next.Budget = validated.Budget
		result.Stats.TokensAfter = validated.Budget.Total
		if validateErr != nil {
			err = validateErr
		}
	}
	if ctx.Err() != nil {
		return result, ctx.Err()
	}
	if commitErr := e.AgentRuntime.CommitContext(ctx, result, state); commitErr != nil {
		return result, commitErr
	}
	if result.Succeeded {
		e.MarkSkillHistoryCompacted("main")
	}
	if result.HistoryReplaced {
		e.clearRuntimeCompressionStateAfterHistoryReplace(state)
	}
	return result, err
}

// QueryLoop runs against the runtime bound to this engine.
func (e *CoreExecutionEngine) QueryLoop(ctx context.Context) <-chan StreamEvent {
	out := make(chan StreamEvent)
	go func() {
		defer close(out)
		runCtx, state, err := e.beginExecution(ctx)
		if err != nil {
			sendStream(ctx, out, NewStreamEvent("error", err.Error(), nil))
			return
		}
		defer e.finishExecution(runCtx, state)
		defer e.SessionRuntime.forwardPermissions(runCtx, out)()
		for event := range e.queryDraftLoop(runCtx, state) {
			sendStream(runCtx, out, event)
		}
	}()
	return out
}

// The WithState adapters accept detached legacy DTOs. They never install caller
// pointers as runtime state and are not used by the production UI or backend.
func (e *CoreExecutionEngine) QueryLoopWithState(ctx context.Context, state *AgentState) <-chan StreamEvent {
	out := make(chan StreamEvent)
	go func() {
		defer close(out)
		if state != nil {
			if err := e.AgentRuntime.Restore(SnapshotFromState(state)); err != nil {
				sendStream(ctx, out, NewStreamEvent("error", err.Error(), nil))
				return
			}
		}
		for event := range e.QueryLoop(ctx) {
			sendStream(ctx, out, event)
		}
		if state != nil {
			*state = e.AgentRuntime.Snapshot().LegacyState()
		}
	}()
	return out
}

func (q *QueryEngine) SubmitMessageWithState(ctx context.Context, prompt string, state *AgentState, identity ...string) <-chan StreamEvent {
	out := make(chan StreamEvent)
	go func() {
		defer close(out)
		if len(identity) > 0 {
			q.CoreEngine.SessionID = identity[0]
		}
		if len(identity) > 1 {
			q.CoreEngine.AgentID = identity[1]
		}
		if state != nil {
			if err := q.Restore(state); err != nil {
				sendStream(ctx, out, NewStreamEvent("error", err.Error(), nil))
				return
			}
		}
		for event := range q.SubmitMessage(ctx, prompt) {
			sendStream(ctx, out, event)
		}
		if state != nil {
			*state = q.Snapshot().LegacyState()
		}
	}()
	return out
}

func (q *QueryEngine) CompactWithState(state *AgentState) (AgentState, agentContext.CompressionStats) {
	if state != nil {
		_ = q.Restore(state)
	}
	result, _ := q.Compact(context.Background(), agentContext.CompactManual)
	next := q.Snapshot().LegacyState()
	if state != nil {
		*state = next
	}
	return next, result.Stats
}

func (r *AgentRuntime) ConsumeRequestCache(token RunToken, ids map[string]struct{}, model string) *api.LLMRequestOptions {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.runActiveLocked(token) || r.run.Phase == AgentPhaseCancelling {
		return &api.LLMRequestOptions{}
	}
	return r.Context.ConsumeCacheEditsForGeneration(ids, model, token.Generation)
}
func (r *AgentRuntime) FinishRequestCache(token RunToken, success bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.runActiveLocked(token) {
		return
	}
	if success {
		if r.run.Phase != AgentPhaseCancelling {
			r.Context.PinConsumedCacheEdits(token.Generation)
		}
	} else {
		r.Context.RestoreInFlightCacheEdits(token.Generation)
	}
}

func (e *CoreExecutionEngine) beginExecution(ctx context.Context) (context.Context, *AgentState, error) {
	id := ""
	if e.RuntimeEvents != nil {
		id = e.RuntimeEvents.ReservedRunID()
	}
	return e.AgentRuntime.BeginWithRunID(ctx, id, e.Config.APIMaxTokens)
}
func (e *CoreExecutionEngine) finishExecution(ctx context.Context, state *AgentState) {
	if err := e.publishDraft(context.WithoutCancel(ctx), state); err != nil {
		e.AgentRuntime.FailRun(state.RunToken(), err.Error())
	}
	_ = e.AgentRuntime.EndRun(state.RunToken())
}
