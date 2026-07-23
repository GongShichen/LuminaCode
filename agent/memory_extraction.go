package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"LuminaCode/config"
	"LuminaCode/memory"
)

const extractionResultPreviewChars = 500

// ExtractionConfig controls how conversation evidence is batched into Memory
// Fabric. Semantic compilation cadence is configured on the Fabric itself.
type ExtractionConfig struct {
	ContextMessageCount int
}

func DefaultExtractionConfig() ExtractionConfig {
	return ExtractionConfig{ContextMessageCount: 32}
}

type ExtractionController struct {
	Config           config.Config
	Engine           memory.Engine
	ExtractionConfig ExtractionConfig

	mu                  sync.Mutex
	currentRunning      bool
	currentCancel       context.CancelFunc
	currentRunID        uint64
	pendingContext      *extractionContext
	lastResult          *string
	SourceSessionID     string
	SourceAgentID       string
	SourceTeamName      string
	SourceTeamSessionID string
	SourceTeamAgentID   string
}

type extractionContext struct {
	Messages      []map[string]any
	MessageIDs    []string
	StartIndex    int
	EndIndex      int
	SessionID     string
	ConsumerID    string
	TurnCount     int
	UserTurnCount int
	State         *AgentState
}

func NewExtractionController(cfg config.Config, extractionConfig ...ExtractionConfig) *ExtractionController {
	ec := DefaultExtractionConfig()
	if len(extractionConfig) > 0 {
		ec = extractionConfig[0]
	}
	return &ExtractionController{Config: cfg, ExtractionConfig: ec}
}

func (c *ExtractionController) ShouldExtract(state *AgentState) bool {
	return state != nil && c.Config.LongTermMemoryEnabled && isFabricMemoryBackend(c.Config) &&
		c.Engine != nil && state.MemoryExtractionCursor < len(state.Messages)
}

func (c *ExtractionController) Schedule(_ context.Context, state *AgentState, _ string) bool {
	if !c.ShouldExtract(state) {
		return false
	}
	payload := c.incrementalFabricContext(state)
	if len(payload.Messages) == 0 {
		c.advanceFabricCursor(payload)
		return false
	}

	c.mu.Lock()
	if c.currentRunning {
		c.pendingContext = payload
		c.mu.Unlock()
		return false
	}
	runCtx, cancel := context.WithCancel(context.Background())
	c.currentRunning = true
	c.currentCancel = cancel
	c.currentRunID++
	runID := c.currentRunID
	c.mu.Unlock()
	go c.runExtraction(runCtx, payload, runID)
	return true
}

func (c *ExtractionController) incrementalFabricContext(state *AgentState) *extractionContext {
	messages, messageIDs, startIndex, endIndex, sessionID, consumerID := c.incrementalFabricMessages(state)
	return &extractionContext{
		Messages: messages, MessageIDs: messageIDs, StartIndex: startIndex, EndIndex: endIndex,
		SessionID: sessionID, ConsumerID: consumerID, TurnCount: state.TurnCount,
		UserTurnCount: state.UserTurnCount, State: state,
	}
}

func (c *ExtractionController) incrementalFabricMessages(state *AgentState) ([]map[string]any, []string, int, int, string, string) {
	sessionID := firstNonEmptyString(c.SourceSessionID, state.MemorySessionID)
	if sessionID == "" {
		sessionID = "runtime-" + stableFabricTextID(c.Config.ProjectRoot()) + "-" +
			firstNonEmptyString(c.SourceAgentID, "main")
	}
	consumerID := "memory-fabric-ingest:" + firstNonEmptyString(c.SourceAgentID, "main")
	start := state.MemoryExtractionCursor
	if start < 0 {
		start = 0
	}
	if start >= len(state.Messages) {
		return nil, nil, start, start - 1, sessionID, consumerID
	}

	limit := c.ExtractionConfig.ContextMessageCount
	if limit <= 0 || limit > 32 {
		limit = 32
	}
	messages := make([]map[string]any, 0, limit)
	messageIDs := make([]string, 0, limit)
	end := start - 1
	for index := start; index < len(state.Messages); index++ {
		end = index
		message := state.Messages[index]
		if len(StripTransientContextMessages([]map[string]any{message})) == 0 {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(stringFromAny(message["role"])))
		if role != "user" && role != "assistant" {
			continue
		}
		if visibleMessageText(message["content"]) == "" {
			continue
		}
		messages = append(messages, message)
		messageIDs = append(messageIDs, extractionMessageID(sessionID, index, message))
		if len(messages) >= limit {
			break
		}
	}
	return messages, messageIDs, start, end, sessionID, consumerID
}

func (c *ExtractionController) HasPendingResult() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastResult != nil
}

func (c *ExtractionController) ConsumeResult() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.lastResult == nil {
		return ""
	}
	result := *c.lastResult
	c.lastResult = nil
	return result
}

func (c *ExtractionController) Cancel() {
	c.mu.Lock()
	cancel := c.currentCancel
	c.currentCancel = nil
	c.currentRunning = false
	c.currentRunID++
	c.pendingContext = nil
	c.lastResult = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *ExtractionController) runExtraction(ctx context.Context, payload *extractionContext, runID uint64) {
	defer func() {
		c.mu.Lock()
		pending := c.pendingContext
		c.pendingContext = nil
		if pending != nil {
			runCtx, cancel := context.WithCancel(context.Background())
			c.currentCancel = cancel
			c.currentRunning = true
			c.currentRunID++
			nextRunID := c.currentRunID
			c.mu.Unlock()
			go c.runExtraction(runCtx, pending, nextRunID)
			return
		}
		if c.currentRunID == runID {
			c.currentRunning = false
			c.currentCancel = nil
		}
		c.mu.Unlock()
	}()
	_, _ = c.runFabricExtraction(ctx, payload)
}

func (c *ExtractionController) ExtractNow(ctx context.Context, state *AgentState) (string, error) {
	if state == nil {
		return "", errors.New("agent state is required")
	}
	if !c.Config.LongTermMemoryEnabled || !isFabricMemoryBackend(c.Config) {
		return "", errors.New("Memory Fabric is required for semantic extraction")
	}
	if c.Engine == nil {
		return "", errors.New("memory fabric engine is unavailable")
	}
	payload := c.incrementalFabricContext(state)
	if len(payload.Messages) == 0 {
		c.advanceFabricCursor(payload)
		return "", nil
	}
	return c.runFabricExtraction(ctx, payload)
}

// IngestMessages durably records the next raw evidence batch without invoking
// the Semantic Compiler. It is used while sealing a context and by importers.
func (c *ExtractionController) IngestMessages(ctx context.Context, state *AgentState) (int, error) {
	if state == nil {
		return 0, errors.New("agent state is required")
	}
	if !c.Config.LongTermMemoryEnabled || !isFabricMemoryBackend(c.Config) {
		return 0, errors.New("Memory Fabric is required for evidence ingestion")
	}
	payload := c.incrementalFabricContext(state)
	if len(payload.Messages) == 0 {
		c.advanceFabricCursor(payload)
		return 0, nil
	}
	return c.ingestFabricEvents(ctx, payload)
}

func (c *ExtractionController) ingestFabricEvents(ctx context.Context, payload *extractionContext) (int, error) {
	if c.Engine == nil {
		return 0, errors.New("memory fabric engine is unavailable")
	}
	events := c.fabricEvents(payload)
	if len(events) == 0 {
		c.advanceFabricCursor(payload)
		return 0, nil
	}
	result, err := c.Engine.AppendEvents(ctx, events,
		memory.IngestOptions{SemanticPolicy: memory.SemanticDurableOnly})
	if result.Durable {
		c.advanceFabricCursor(payload)
	}
	if err != nil {
		return len(events), err
	}
	if !result.Durable {
		return 0, errors.New("memory fabric did not confirm durable event ingestion")
	}
	return len(events), nil
}

func (c *ExtractionController) runFabricExtraction(ctx context.Context, payload *extractionContext) (string, error) {
	if c.Engine == nil {
		return "", errors.New("memory fabric engine is unavailable")
	}
	events := c.fabricEvents(payload)
	if len(events) == 0 {
		c.advanceFabricCursor(payload)
		return "", nil
	}

	mode, sourceIDs := fabricSynchronousMemoryRequest(events)
	policy := memory.SemanticDeferred
	if len(sourceIDs) > 0 {
		policy = memory.SemanticDurableOnly
	}
	ingested, ingestErr := c.Engine.AppendEvents(ctx, events, memory.IngestOptions{SemanticPolicy: policy})
	if !ingested.Durable {
		if ingestErr != nil {
			return "", ingestErr
		}
		return "", errors.New("memory fabric did not confirm durable event ingestion")
	}
	c.advanceFabricCursor(payload)

	semanticStatus := ingested.SemanticStatus
	var semanticErr error
	if len(sourceIDs) > 0 {
		committed, err := c.Engine.Remember(ctx, memory.MemoryRequest{
			Space: fabricMemorySpace(c.Config), ContextID: payload.SessionID, SourceEventIDs: sourceIDs,
			Mode: mode, RequireSemantic: true,
			Instructions: "The user explicitly requested durable semantic memory; preserve scope and correction intent.",
		})
		semanticStatus = committed.SemanticStatus
		semanticErr = err
	}

	summary := fmt.Sprintf("Memory Fabric stored %d raw event(s); semantic status: %s.", len(events), semanticStatus)
	formatted := FormatExtractionResult(summary)
	c.mu.Lock()
	c.lastResult = &formatted
	c.mu.Unlock()
	if semanticErr != nil {
		return formatted, semanticErr
	}
	return formatted, ingestErr
}

func (c *ExtractionController) fabricEvents(payload *extractionContext) []memory.RawEvent {
	if payload == nil {
		return nil
	}
	space := fabricMemorySpace(c.Config)
	fallbackTime := time.Now().UTC()
	events := make([]memory.RawEvent, 0, len(payload.Messages))
	for index, message := range payload.Messages {
		if index >= len(payload.MessageIDs) {
			break
		}
		role := strings.ToLower(strings.TrimSpace(stringFromAny(message["role"])))
		if role != "user" && role != "assistant" {
			continue
		}
		content := strings.TrimSpace(visibleMessageText(message["content"]))
		if content == "" {
			continue
		}
		messageID := payload.MessageIDs[index]
		metadata := map[string]string{
			"project": c.Config.ProjectRoot(), "agent": firstNonEmptyString(c.SourceAgentID, "main"),
			"team": c.SourceTeamName, "role": role,
		}
		if rawMetadata, ok := message["metadata"].(map[string]any); ok {
			if turn := strings.TrimSpace(stringFromAny(rawMetadata["session_user_turn"])); turn != "" {
				metadata["session_user_turn"] = turn
			}
		}
		events = append(events, memory.RawEvent{
			ID: fabricEventID(space, payload.SessionID, messageID), Space: space,
			ContextID: payload.SessionID, SessionID: payload.SessionID, Actor: role,
			SourceKind: "conversation", Content: content,
			OccurredAt: extractionMessageOccurredAt(message, fallbackTime),
			SourceRef:  messageID, Metadata: metadata,
		})
	}
	return events
}

func fabricEventID(space, sessionID, messageID string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{"fabric", space, sessionID, messageID}, "\x00")))
	return "evt_" + hex.EncodeToString(digest[:16])
}

func fabricSynchronousMemoryRequest(events []memory.RawEvent) (memory.MemoryWriteMode, []string) {
	mode := memory.WriteNormal
	priority := 0
	var ids []string
	for _, event := range events {
		if event.Actor != "user" {
			continue
		}
		candidate, candidatePriority, ok := classifyFabricMemoryWrite(event.Content)
		if !ok {
			continue
		}
		ids = append(ids, event.ID)
		if candidatePriority > priority {
			mode, priority = candidate, candidatePriority
		}
	}
	return mode, uniqueMemoryRecallIDs(ids)
}

func classifyFabricMemoryWrite(text string) (memory.MemoryWriteMode, int, bool) {
	lower := strings.ToLower(strings.TrimSpace(text))
	if lower == "" {
		return memory.WriteNormal, 0, false
	}
	containsAny := func(values ...string) bool {
		for _, value := range values {
			if strings.Contains(lower, value) {
				return true
			}
		}
		return false
	}
	if containsAny("更正", "纠正", "改成", "不是之前", "actually", "correction", "i meant") {
		return memory.WriteCorrection, 5, true
	}
	if containsAny("必须", "禁止", "永远不要", "不要再", "约束", "must ", "must not", "never ", "always ") {
		return memory.WriteConstraint, 4, true
	}
	if containsAny("我的偏好", "我更喜欢", "我喜欢", "我希望", "偏好", "i prefer", "my preference", "i like") {
		return memory.WritePreference, 3, true
	}
	if containsAny("记住", "记得", "请保存", "remember", "keep this in mind", "save this") {
		return memory.WriteExplicit, 2, true
	}
	return memory.WriteNormal, 0, false
}

func (c *ExtractionController) advanceFabricCursor(payload *extractionContext) {
	if payload == nil || payload.State == nil {
		return
	}
	payload.State.LastExtractionTurn = payload.State.TurnCount
	payload.State.LastExtractionUserTurn = payload.State.UserTurnCount
	payload.State.MemoryWritesSinceExtraction = false
	if payload.EndIndex >= payload.StartIndex {
		payload.State.MemoryExtractionCursor = payload.EndIndex + 1
	}
}

func FormatExtractionResult(agentResult string) string {
	summary := truncateExtractionRunes(strings.TrimSpace(agentResult), extractionResultPreviewChars)
	if summary == "" {
		return ""
	}
	return "<system-reminder note=\"auto-memory\">\nBackground memory extraction completed:\n" + summary + "\n</system-reminder>"
}

func extractionMessageID(sessionID string, index int, message map[string]any) string {
	if id := strings.TrimSpace(stringFromAny(message["id"])); id != "" {
		return id
	}
	material := strings.Join([]string{sessionID, fmt.Sprintf("%d", index),
		stringFromAny(message["role"]), formatExtractionMessageContent(message["content"])}, "\x00")
	return "msg_" + stableFabricTextID(material)
}

func stableFabricTextID(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:16])
}

func minIntAgent(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func extractionMessageOccurredAt(message map[string]any, fallback time.Time) time.Time {
	for _, key := range []string{"created_at", "timestamp", "occurred_at"} {
		if value := strings.TrimSpace(stringFromAny(message[key])); value != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				return parsed.UTC()
			}
		}
	}
	if metadata, ok := message["metadata"].(map[string]any); ok {
		for _, key := range []string{"created_at", "timestamp", "occurred_at"} {
			if value := strings.TrimSpace(stringFromAny(metadata[key])); value != "" {
				if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
					return parsed.UTC()
				}
			}
		}
	}
	return fallback
}

func formatExtractionMessageContent(raw any) string {
	switch content := raw.(type) {
	case []map[string]any:
		return formatExtractionBlocks(content)
	case []any:
		blocks := make([]map[string]any, 0, len(content))
		for _, item := range content {
			if block, ok := item.(map[string]any); ok {
				blocks = append(blocks, block)
			}
		}
		return formatExtractionBlocks(blocks)
	case string:
		return content
	default:
		return fmt.Sprint(raw)
	}
}

func formatExtractionBlocks(blocks []map[string]any) string {
	var parts []string
	for _, block := range blocks {
		switch block["type"] {
		case "text":
			parts = append(parts, stringFromAny(block["text"]))
		case "tool_use":
			parts = append(parts, fmt.Sprintf("[tool: %s id=%s]", stringFromAny(block["name"]), stringFromAny(block["id"])))
		case "tool_result":
			content := stringFromAny(block["content"])
			if len([]rune(content)) > 200 {
				content = truncateExtractionRunes(content, 200) + "...(truncated)"
			}
			parts = append(parts, "[tool_result: "+content+"]")
		}
	}
	return strings.Join(parts, "\n")
}

func truncateExtractionRunes(text string, limit int) string {
	runes := []rune(text)
	if limit < 0 || len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
