package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"LuminaCode/config"
	"LuminaCode/sessionmemory"
	coretools "LuminaCode/tools"
)

type SessionHistoryListInput struct {
	Query string `json:"query,omitempty" jsonschema_description:"Optional search query for this agent's session history commit summaries"`
	Limit int    `json:"limit,omitempty" jsonschema:"default=20" jsonschema_description:"Maximum number of commits to return"`
}

type SessionHistoryGetInput struct {
	CommitNo        int   `json:"commit_no" jsonschema_description:"Commit number returned by session_history_list"`
	IncludeMessages *bool `json:"include_messages,omitempty" jsonschema:"nullable,default=true" jsonschema_description:"Whether to include original message snippets for this commit"`
}

type SessionHistoryListTool struct{ coretools.BaseTool }

func NewSessionHistoryListTool() coretools.Tool {
	return &SessionHistoryListTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:            "session_history_list",
		Description:     "List local session-memory commits for this LuminaCode session. Use this when earlier conversation details may have been compressed or forgotten; inspect the list first, then call session_history_get for details.",
		InputPrototype:  SessionHistoryListInput{},
		ReadOnly:        coretools.BoolPtr(true),
		ConcurrencySafe: coretools.BoolPtr(true),
		Destructive:     coretools.BoolPtr(false),
		MaxOutputChars:  20_000,
	}}}
}

func (t *SessionHistoryListTool) Execute(ctx context.Context, execCtx coretools.ExecutionContext, input any) (string, error) {
	in := derefSessionHistoryList(input)
	cfg, sessionID, agentID, err := sessionHistoryContext(execCtx)
	if err != nil {
		return err.Error(), nil
	}
	store, err := sessionmemory.Open(ctx, cfg, sessionID, nil)
	if err != nil {
		return fmt.Sprintf("Error opening session history: %s", err), nil
	}
	defer store.Close()
	items, err := store.ListCommits(ctx, in.Query, in.Limit)
	if err != nil {
		return fmt.Sprintf("Error listing session history: %s", err), nil
	}
	payload := map[string]any{
		"session_id": sessionID,
		"agent_id":   agentID,
		"query":      in.Query,
		"commits":    items,
	}
	return marshalSessionHistoryJSON(payload), nil
}

type SessionHistoryGetTool struct{ coretools.BaseTool }

func NewSessionHistoryGetTool() coretools.Tool {
	return &SessionHistoryGetTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{
		Name:            "session_history_get",
		Description:     "Read one local session-memory commit summary and its original message snippets. Use after session_history_list when you need exact earlier conversation details.",
		InputPrototype:  SessionHistoryGetInput{},
		ReadOnly:        coretools.BoolPtr(true),
		ConcurrencySafe: coretools.BoolPtr(true),
		Destructive:     coretools.BoolPtr(false),
		MaxOutputChars:  50_000,
	}}}
}

func (t *SessionHistoryGetTool) Execute(ctx context.Context, execCtx coretools.ExecutionContext, input any) (string, error) {
	in := derefSessionHistoryGet(input)
	cfg, sessionID, agentID, err := sessionHistoryContext(execCtx)
	if err != nil {
		return err.Error(), nil
	}
	if in.CommitNo <= 0 {
		return "Error: commit_no must be a positive integer.", nil
	}
	store, err := sessionmemory.Open(ctx, cfg, sessionID, nil)
	if err != nil {
		return fmt.Sprintf("Error opening session history: %s", err), nil
	}
	defer store.Close()
	detail, err := store.GetCommit(ctx, in.CommitNo, boolDefault(in.IncludeMessages, true))
	if err != nil {
		return fmt.Sprintf("Error reading session history commit %d: %s", in.CommitNo, err), nil
	}
	payload := map[string]any{
		"session_id": sessionID,
		"agent_id":   agentID,
		"commit":     detail,
	}
	if detail.OmittedMessages > 0 {
		payload["note"] = fmt.Sprintf("%d original messages were omitted because session_history_get_message_limit=%d. Use the summary first, then ask for narrower follow-up context if needed.", detail.OmittedMessages, cfg.SessionHistoryGetMessageLimit)
	}
	return marshalSessionHistoryJSON(payload), nil
}

func sessionHistoryContext(execCtx coretools.ExecutionContext) (config.Config, string, string, error) {
	cfg, _ := execCtx["config"].(config.Config)
	if !cfg.SessionMemoryEnabled {
		return cfg, "", "", fmt.Errorf("session memory is disabled")
	}
	sessionID := strings.TrimSpace(fmt.Sprint(execCtx["_session_id"]))
	if sessionID == "" || sessionID == "<nil>" {
		return cfg, "", "", fmt.Errorf("session id is not available")
	}
	agentID := strings.TrimSpace(fmt.Sprint(execCtx["_agent_id"]))
	if agentID == "" || agentID == "<nil>" {
		agentID = "main"
	}
	cfg.SessionMemoryAgentID = agentID
	return cfg, sessionID, agentID, nil
}

func derefSessionHistoryList(input any) SessionHistoryListInput {
	switch v := input.(type) {
	case *SessionHistoryListInput:
		if v != nil {
			return *v
		}
	case SessionHistoryListInput:
		return v
	}
	return SessionHistoryListInput{}
}

func derefSessionHistoryGet(input any) SessionHistoryGetInput {
	out := SessionHistoryGetInput{}
	switch v := input.(type) {
	case *SessionHistoryGetInput:
		if v != nil {
			out = *v
		}
	case SessionHistoryGetInput:
		out = v
	}
	return out
}

func boolDefault(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}

func marshalSessionHistoryJSON(value any) string {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(data)
}
