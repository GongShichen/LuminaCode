package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"LuminaCode/config"
	"LuminaCode/longmemory"
	coretools "LuminaCode/tools"
)

const extractionResultPreviewChars = 500

var ExtractionAgentDef = AgentDef{
	Name:           "auto-memory-extract",
	Description:    "Background agent that extracts persistent memories from conversation context",
	ToolsAllowlist: stringSet("ExtractMemoryBatch"),
	MaxTurns:       3,
	PermissionMode: "inherit",
}

type ExtractionConfig struct {
	TurnsBetweenExtractions int
	MaxExtractionTurns      int
	ContextMessageCount     int
	CustomPromptPath        string
}

func DefaultExtractionConfig() ExtractionConfig {
	return ExtractionConfig{TurnsBetweenExtractions: 5, MaxExtractionTurns: 5, ContextMessageCount: 8}
}

type ExtractionRunner func(ctx context.Context, prompt, systemPrompt string, filteredRegistry *coretools.ToolRegistry, extraContext coretools.ExecutionContext) (string, error)

type ExtractionController struct {
	Config           config.Config
	BaseRegistry     *coretools.ToolRegistry
	ExtractionConfig ExtractionConfig
	Runner           ExtractionRunner

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
	StorePath     string
	TurnCount     int
	UserTurnCount int
	State         *AgentState
	MemoryDir     string
}

func NewExtractionController(cfg config.Config, baseRegistry *coretools.ToolRegistry, extractionConfig ...ExtractionConfig) *ExtractionController {
	ec := DefaultExtractionConfig()
	if len(extractionConfig) > 0 {
		ec = extractionConfig[0]
	}
	return &ExtractionController{Config: cfg, BaseRegistry: baseRegistry, ExtractionConfig: ec}
}

func (c *ExtractionController) ShouldExtract(state *AgentState) bool {
	if state == nil {
		return false
	}
	if c.Config.LongTermMemoryEnabled && !c.Config.MemoryBackgroundExtractionEnabled {
		return false
	}
	interval := c.ExtractionConfig.TurnsBetweenExtractions
	if c.Config.LongTermMemoryEnabled && c.Config.MemoryBackgroundExtractionInterval > 0 {
		interval = c.Config.MemoryBackgroundExtractionInterval
	}
	turnsSince := state.UserTurnCount - state.LastExtractionUserTurn
	if turnsSince < interval {
		return false
	}
	if state.MemoryWritesSinceExtraction {
		return false
	}
	return true
}

func (c *ExtractionController) Schedule(_ context.Context, state *AgentState, memoryDir string) bool {
	if !c.ShouldExtract(state) {
		return false
	}
	storePath := c.Config.LongTermMemoryStore
	if strings.TrimSpace(memoryDir) != "" && filepath.Clean(longmemory.ExpandPath(storePath)) == filepath.Clean(longmemory.DefaultStorePath()) {
		storePath = filepath.Join(memoryDir, "lumina-memory.sqlite")
	}
	messages, messageIDs, startIndex, endIndex, sessionID, consumerID := c.incrementalMessages(state, storePath)
	if len(messages) == 0 {
		state.LastExtractionTurn = state.TurnCount
		state.LastExtractionUserTurn = state.UserTurnCount
		return false
	}
	payload := &extractionContext{
		Messages:      messages,
		MessageIDs:    messageIDs,
		StartIndex:    startIndex,
		EndIndex:      endIndex,
		SessionID:     sessionID,
		ConsumerID:    consumerID,
		StorePath:     storePath,
		TurnCount:     state.TurnCount,
		UserTurnCount: state.UserTurnCount,
		State:         state,
		MemoryDir:     memoryDir,
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

func (c *ExtractionController) incrementalMessages(state *AgentState, storePath string) ([]map[string]any, []string, int, int, string, string) {
	sessionID := firstNonEmptyString(c.SourceSessionID, state.MemorySessionID)
	if sessionID == "" {
		sessionID = "runtime-" + longmemory.ProjectScopeKey(c.Config.CWD) + "-" + firstNonEmptyString(c.SourceAgentID, "main")
	}
	consumerID := "long-term-extraction:" + firstNonEmptyString(c.SourceAgentID, "main")
	start := state.MemoryExtractionCursor
	if store, err := longmemory.Open(context.Background(), storePath); err == nil {
		_, index, cursorErr := store.GetCursor(context.Background(), consumerID, sessionID)
		_ = store.Close()
		if cursorErr == nil && index+1 > start {
			start = index + 1
		} else if cursorErr != nil && !errors.Is(cursorErr, sql.ErrNoRows) {
			start = state.MemoryExtractionCursor
		}
	}
	if start < 0 {
		start = 0
	}
	if start >= len(state.Messages) {
		return nil, nil, start, start - 1, sessionID, consumerID
	}
	end := len(state.Messages)
	const maxMessagesPerExtraction = 32
	if end-start > maxMessagesPerExtraction {
		end = start + maxMessagesPerExtraction
	}
	messages := append([]map[string]any(nil), state.Messages[start:end]...)
	messageIDs := make([]string, len(messages))
	for offset, message := range messages {
		messageIDs[offset] = extractionMessageID(sessionID, start+offset, message)
	}
	return messages, messageIDs, start, end - 1, sessionID, consumerID
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
	defer c.mu.Unlock()
	c.pendingContext = nil
	c.lastResult = nil
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
	if c.Config.LongTermMemoryEnabled {
		_, _ = c.runLongTermExtraction(ctx, payload)
	}
}

func (c *ExtractionController) ExtractNow(ctx context.Context, state *AgentState) (string, error) {
	if state == nil {
		return "", fmt.Errorf("agent state is required")
	}
	messages, messageIDs, startIndex, endIndex, sessionID, consumerID := c.incrementalMessages(state, c.Config.LongTermMemoryStore)
	if len(messages) == 0 {
		return "", nil
	}
	return c.runLongTermExtraction(ctx, &extractionContext{Messages: messages, MessageIDs: messageIDs,
		StartIndex: startIndex, EndIndex: endIndex, SessionID: sessionID, ConsumerID: consumerID,
		StorePath: c.Config.LongTermMemoryStore, TurnCount: state.TurnCount, UserTurnCount: state.UserTurnCount, State: state})
}

// IngestMessages persists the next raw message batch and its searchable chunks
// without waiting for semantic enrichment. It advances only the ingestion
// cursor, so extraction can retry independently.
func (c *ExtractionController) IngestMessages(ctx context.Context, state *AgentState) (int, error) {
	if state == nil {
		return 0, fmt.Errorf("agent state is required")
	}
	storePath := c.Config.LongTermMemoryStore
	sessionID := firstNonEmptyString(c.SourceSessionID, state.MemorySessionID)
	if sessionID == "" {
		sessionID = "runtime-" + longmemory.ProjectScopeKey(c.Config.CWD) + "-" + firstNonEmptyString(c.SourceAgentID, "main")
	}
	consumerID := "long-term-extraction:" + firstNonEmptyString(c.SourceAgentID, "main")
	start := 0
	store, err := longmemory.Open(ctx, storePath)
	if err != nil {
		return 0, err
	}
	defer store.Close()
	if _, index, cursorErr := store.GetCursor(ctx, consumerID+":ingestion", sessionID); cursorErr == nil {
		start = index + 1
	} else if !errors.Is(cursorErr, sql.ErrNoRows) {
		return 0, cursorErr
	}
	if start >= len(state.Messages) {
		return 0, nil
	}
	end := minIntAgent(start+32, len(state.Messages))
	messages := append([]map[string]any(nil), state.Messages[start:end]...)
	messageIDs := make([]string, len(messages))
	for offset, message := range messages {
		messageIDs[offset] = extractionMessageID(sessionID, start+offset, message)
	}
	payload := &extractionContext{Messages: messages, MessageIDs: messageIDs, StartIndex: start, EndIndex: end - 1,
		SessionID: sessionID, ConsumerID: consumerID, StorePath: storePath, TurnCount: state.TurnCount,
		UserTurnCount: state.UserTurnCount, State: state}
	batch := c.buildRawIngestionBatch(ctx, payload, firstNonEmptyString(c.SourceAgentID, "main"))
	if err := store.CommitExtraction(ctx, batch); err != nil {
		return 0, fmt.Errorf("commit raw memory ingestion: %w", err)
	}
	return len(messages), nil
}

func minIntAgent(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func (c *ExtractionController) runLongTermExtraction(ctx context.Context, payload *extractionContext) (summary string, runErr error) {
	storePath := firstNonEmptyString(payload.StorePath, c.Config.LongTermMemoryStore)
	store, err := longmemory.Open(ctx, storePath)
	if err != nil {
		if payload.State != nil {
			payload.State.MemoryExtractionCursor = payload.StartIndex
		}
		return "", err
	}
	defer store.Close()
	jobMessages, jobMessageIDs := persistentExtractionMessages(payload.Messages, payload.MessageIDs)
	jobPayload, _ := json.Marshal(map[string]any{
		"session_id": payload.SessionID, "consumer_id": payload.ConsumerID,
		"start_message_index": payload.StartIndex, "end_message_index": payload.EndIndex,
		"message_ids": jobMessageIDs, "messages": jobMessages, "cwd": c.Config.CWD,
		"source_agent_id": c.SourceAgentID, "source_team_name": c.SourceTeamName,
		"source_team_agent_id": c.SourceTeamAgentID, "source_team_session_id": c.SourceTeamSessionID,
	})
	job := longmemory.Job{Kind: "extraction", ScopeType: longmemory.ScopeProject,
		ScopeKey: longmemory.ProjectScopeKey(c.Config.CWD), Payload: string(jobPayload)}
	if err := store.EnqueueJob(ctx, job); err != nil {
		return "", fmt.Errorf("enqueue memory extraction: %w", err)
	}
	job.JobID = longmemory.StableID(job.ScopeType, job.ScopeKey, job.Kind, job.Payload)
	if err := store.StartJob(ctx, job.JobID); err != nil {
		return "", fmt.Errorf("start memory extraction job: %w", err)
	}
	defer func() {
		jobCtx := context.WithoutCancel(ctx)
		if runErr != nil {
			_ = store.RetryJob(jobCtx, job.JobID, runErr, time.Minute)
			return
		}
		_ = store.CompleteJob(jobCtx, job.JobID)
	}()
	agentID := firstNonEmptyString(c.SourceAgentID, "main")
	scopes := longmemory.RuntimeScopes(c.Config.CWD, agentID, c.SourceTeamName, c.SourceTeamAgentID)
	rawBatch := c.buildRawIngestionBatch(ctx, payload, agentID)
	if err := store.CommitExtraction(ctx, rawBatch); err != nil {
		return "", fmt.Errorf("commit raw memory ingestion: %w", err)
	}
	existing, _ := store.Search(ctx, longmemory.SearchOptions{
		Query:         extractionSearchText(payload.Messages),
		Scopes:        scopes,
		Limit:         12,
		MaxCandidates: 30,
	})
	prompt := BuildLongTermExtractionPromptWithIDs(payload.Messages, payload.MessageIDs, existing)
	systemPrompt := longTermExtractionSystemPrompt()
	runner := c.Runner
	if runner == nil {
		runner = c.defaultRunner(payload.State)
	}
	result, err := runner(ctx, prompt, systemPrompt, coretools.NewToolRegistry(), coretools.ExecutionContext{
		"system_prompt_override": systemPrompt,
	})
	if err != nil {
		return "", fmt.Errorf("extract semantic memory: %w", err)
	}
	batch := ParseLongTermExtractionBatch(result)
	var candidates []longmemory.Candidate
	acceptedMemoryIndexes := map[int]int{}
	validMessageIDs := map[string]struct{}{}
	for _, messageID := range payload.MessageIDs {
		validMessageIDs[messageID] = struct{}{}
	}
	for memoryIndex, candidate := range batch.Memories {
		action := normalizeMemoryAction(candidate.Action)
		if action == "ignore" {
			continue
		}
		candidate.SourceSessionID = firstNonEmptyString(candidate.SourceSessionID, c.SourceSessionID)
		candidate.SourceAgentID = firstNonEmptyString(candidate.SourceAgentID, agentID)
		candidate.SourceTeamSessionID = firstNonEmptyString(candidate.SourceTeamSessionID, c.SourceTeamSessionID)
		if action == "update" && candidate.MemoryID == "" {
			candidate.MemoryID = candidate.TargetMemoryID
		}
		if candidate.ScopeType == "" {
			candidate.ScopeType = longmemory.ScopeProject
			candidate.ScopeKey = longmemory.ProjectScopeKey(c.Config.CWD)
		}
		if candidate.ScopeKey == "" {
			switch candidate.ScopeType {
			case longmemory.ScopeUser:
				candidate.ScopeKey = longmemory.UserScopeKey()
			case longmemory.ScopeAgentType:
				candidate.ScopeKey = longmemory.AgentTypeScopeKey(c.Config.CWD, candidate.SourceAgentID)
			case longmemory.ScopeTeam:
				candidate.ScopeKey = longmemory.TeamScopeKey(c.SourceTeamName)
			case longmemory.ScopeTeamAgent:
				candidate.ScopeKey = longmemory.TeamAgentScopeKey(c.SourceTeamName, firstNonEmptyString(c.SourceTeamAgentID, candidate.SourceAgentID))
			default:
				candidate.ScopeKey = longmemory.ProjectScopeKey(c.Config.CWD)
			}
		}
		candidate = longmemory.ApplyRetention(candidate, retentionPolicyFromConfig(c.Config), time.Now().UTC())
		if candidate.Status == "" {
			candidate.Status = longmemory.StatusActive
		}
		if requiresMemoryConfirmation(c.Config, candidate) {
			candidate.Status = longmemory.StatusPending
		}
		sources := append([]string(nil), candidate.SourceMessageIDs...)
		for _, span := range batch.Spans {
			if span.MemoryIndex == memoryIndex {
				sources = append(sources, span.MessageID)
			}
		}
		candidate.SourceMessageIDs = candidate.SourceMessageIDs[:0]
		for _, source := range sources {
			if _, ok := validMessageIDs[source]; ok && !containsStringAgent(candidate.SourceMessageIDs, source) {
				candidate.SourceMessageIDs = append(candidate.SourceMessageIDs, source)
			}
		}
		if len(candidate.SourceMessageIDs) == 0 {
			continue
		}
		acceptedMemoryIndexes[memoryIndex] = len(candidates)
		candidates = append(candidates, candidate)
	}
	batch.Memories = candidates
	batch.Facts, batch.Spans, batch.Edges = remapAcceptedExtractionReferences(batch.Facts, batch.Spans, batch.Edges, acceptedMemoryIndexes)
	now := time.Now().UTC()
	batch.Episode = &longmemory.Episode{
		ScopeType: longmemory.ScopeProject, ScopeKey: longmemory.ProjectScopeKey(c.Config.CWD),
		SessionID: payload.SessionID, TeamSessionID: c.SourceTeamSessionID, AgentID: agentID,
		MessageIDs: append([]string(nil), payload.MessageIDs...), Kind: "conversation",
		Content: extractionSearchText(payload.Messages), OccurredAt: extractionOccurredAt(payload.Messages, now), ObservedAt: now,
	}
	for index, message := range payload.Messages {
		role := strings.ToLower(strings.TrimSpace(stringFromAny(message["role"])))
		if role != "user" && role != "assistant" {
			continue
		}
		text := visibleMessageText(message["content"])
		if text == "" || index >= len(payload.MessageIDs) {
			continue
		}
		batch.EpisodeSpans = append(batch.EpisodeSpans, longmemory.EvidenceSpan{
			MessageID: payload.MessageIDs[index], Role: role, Text: text,
			OccurredAt: extractionMessageOccurredAt(message, batch.Episode.OccurredAt),
		})
	}
	sessionIndexID := longmemory.StableID(batch.Episode.ScopeType, batch.Episode.ScopeKey, "session-index", batch.Episode.SessionID)
	for _, span := range batch.EpisodeSpans {
		span.MemoryID = sessionIndexID
		span.ScopeType = batch.Episode.ScopeType
		span.ScopeKey = batch.Episode.ScopeKey
		span.SessionID = batch.Episode.SessionID
		batch.Chunks = append(batch.Chunks, longmemory.BuildEvidenceChunks(span)...)
	}
	for index := range batch.Facts {
		memoryIndex := batch.Facts[index].MemoryIndex
		if memoryIndex < 0 || memoryIndex >= len(batch.Memories) {
			continue
		}
		batch.Facts[index].ScopeType = batch.Memories[memoryIndex].ScopeType
		batch.Facts[index].ScopeKey = batch.Memories[memoryIndex].ScopeKey
		if batch.Facts[index].ObservedAt.IsZero() {
			batch.Facts[index].ObservedAt = now
		}
	}
	batch.Spans = validateExtractionSpans(batch.Spans, batch.Memories, payload)
	batch = retainMemoriesWithEvidence(batch)
	allowedCoreScopes := map[string]struct{}{}
	for _, candidate := range batch.Memories {
		if candidate.Status == longmemory.StatusActive && (candidate.MemoryType == longmemory.TypePreference || candidate.MemoryType == longmemory.TypeFeedback || candidate.MemoryType == longmemory.TypeProject || candidate.MemoryType == longmemory.TypeProcedural) {
			allowedCoreScopes[string(candidate.ScopeType)+"\x00"+candidate.ScopeKey] = struct{}{}
		}
	}
	filteredCoreBlocks := batch.CoreBlocks[:0]
	for index := range batch.CoreBlocks {
		if batch.CoreBlocks[index].ScopeType == "" {
			batch.CoreBlocks[index].ScopeType = longmemory.ScopeProject
			batch.CoreBlocks[index].ScopeKey = longmemory.ProjectScopeKey(c.Config.CWD)
		}
		if _, ok := allowedCoreScopes[string(batch.CoreBlocks[index].ScopeType)+"\x00"+batch.CoreBlocks[index].ScopeKey]; ok {
			filteredCoreBlocks = append(filteredCoreBlocks, batch.CoreBlocks[index])
		}
	}
	batch.CoreBlocks = filteredCoreBlocks
	if c.Config.MemoryEmbeddingEnabled && len(batch.Memories) > 0 {
		embeddingPrepared := false
		if embedder := configuredMemoryEmbedder(c.Config); embedder != nil {
			texts := make([]string, len(batch.Memories))
			for index, candidate := range batch.Memories {
				texts[index] = candidate.Title + "\n" + candidate.Summary + "\n" + candidate.Content
			}
			if vectors, embedErr := embedder.Embed(ctx, texts, longmemory.EmbeddingPassage); embedErr == nil {
				for index, vector := range vectors {
					batch.Embeddings = append(batch.Embeddings, longmemory.MemoryEmbedding{MemoryIndex: index,
						Model: embedder.Model(), ContentHash: longmemory.StableID(longmemory.ScopeProject, "embedding", "content", texts[index]), Vector: vector})
				}
				embeddingPrepared = len(batch.Embeddings) == len(batch.Memories)
			}
			if batch.Episode != nil {
				sessionText := batch.Episode.Content
				if vectors, sessionErr := embedder.Embed(ctx, []string{sessionText}, longmemory.EmbeddingPassage); sessionErr == nil && len(vectors) == 1 {
					batch.SessionEmbedding = &longmemory.MemoryEmbedding{Model: embedder.Model(),
						ContentHash: longmemory.StableID(longmemory.ScopeProject, "embedding", "session", sessionText), Vector: vectors[0]}
				}
			}
		}
		if !embeddingPrepared {
			batch.Jobs = append(batch.Jobs, longmemory.Job{Kind: "embedding_backfill", ScopeType: longmemory.ScopeProject,
				ScopeKey: longmemory.ProjectScopeKey(c.Config.CWD), Payload: fmt.Sprintf(`{"session_id":%q}`, payload.SessionID)})
		}
	}
	if c.Config.MemoryEmbeddingEnabled && len(batch.Chunks) > 0 {
		if embedder := configuredMemoryEmbedder(c.Config); embedder != nil {
			texts := make([]string, len(batch.Chunks))
			for index := range batch.Chunks {
				texts[index] = batch.Chunks[index].Text
			}
			if vectors, embedErr := embedder.Embed(ctx, texts, longmemory.EmbeddingPassage); embedErr == nil {
				for index, vector := range vectors {
					if index >= len(batch.Chunks) {
						break
					}
					batch.ChunkEmbeddings = append(batch.ChunkEmbeddings, longmemory.MemoryEmbedding{
						MemoryID: batch.Chunks[index].ChunkID, Model: embedder.Model(),
						ContentHash: batch.Chunks[index].ContentHash, Vector: vector,
					})
				}
			}
		}
	}
	if c.Config.MemoryEmbeddingEnabled && batch.Episode != nil && batch.SessionEmbedding == nil {
		if embedder := configuredMemoryEmbedder(c.Config); embedder != nil {
			if vectors, embedErr := embedder.Embed(ctx, []string{batch.Episode.Content}, longmemory.EmbeddingPassage); embedErr == nil && len(vectors) == 1 {
				batch.SessionEmbedding = &longmemory.MemoryEmbedding{Model: embedder.Model(),
					ContentHash: longmemory.StableID(longmemory.ScopeProject, "embedding", "session", batch.Episode.Content), Vector: vectors[0]}
			}
		}
	}
	batch.ConsumerID = payload.ConsumerID
	batch.SessionID = payload.SessionID
	batch.LastMessageIndex = payload.EndIndex
	batch.AtomTargetTokens = c.Config.MemoryAtomTargetTokens
	batch.AtomMaxTokens = c.Config.MemoryAtomMaxTokens
	if len(payload.MessageIDs) > 0 {
		batch.LastMessageID = payload.MessageIDs[len(payload.MessageIDs)-1]
	}
	if err := store.CommitExtraction(ctx, batch); err != nil {
		return "", err
	}
	var saved []string
	for _, candidate := range batch.Memories {
		memoryID := candidate.MemoryID
		if memoryID == "" {
			memoryID = longmemory.StableID(candidate.ScopeType, candidate.ScopeKey, candidate.Title, candidate.Content)
		}
		saved = append(saved, memoryID)
	}
	summary = "saved nothing"
	if len(saved) > 0 {
		summary = "saved long-term memories: " + strings.Join(saved, ", ")
	}
	formatted := FormatExtractionResult(summary)
	if payload.State != nil {
		payload.State.LastExtractionTurn = payload.State.TurnCount
		payload.State.LastExtractionUserTurn = payload.State.UserTurnCount
		payload.State.MemoryWritesSinceExtraction = false
		payload.State.MemoryExtractionCursor = payload.EndIndex + 1
	}
	c.mu.Lock()
	c.lastResult = &formatted
	c.mu.Unlock()
	return formatted, nil
}

func persistentExtractionMessages(messages []map[string]any, messageIDs []string) ([]map[string]any, []string) {
	result := make([]map[string]any, 0, len(messages))
	ids := make([]string, 0, len(messages))
	for index, message := range messages {
		if index >= len(messageIDs) {
			break
		}
		role := strings.ToLower(strings.TrimSpace(stringFromAny(message["role"])))
		if role != "user" && role != "assistant" {
			continue
		}
		text := visibleMessageText(message["content"])
		if text == "" {
			continue
		}
		stored := map[string]any{"role": role, "content": text, "id": messageIDs[index]}
		if timestamp := stringFromAny(message["timestamp"]); timestamp != "" {
			stored["timestamp"] = timestamp
		}
		result = append(result, stored)
		ids = append(ids, messageIDs[index])
	}
	return result, ids
}

func (c *ExtractionController) ProcessExtractionJob(ctx context.Context, job longmemory.Job) error {
	if job.Kind != "extraction" {
		return fmt.Errorf("unsupported memory job kind %q", job.Kind)
	}
	var payload struct {
		SessionID           string           `json:"session_id"`
		ConsumerID          string           `json:"consumer_id"`
		StartMessageIndex   int              `json:"start_message_index"`
		EndMessageIndex     int              `json:"end_message_index"`
		MessageIDs          []string         `json:"message_ids"`
		Messages            []map[string]any `json:"messages"`
		CWD                 string           `json:"cwd"`
		SourceAgentID       string           `json:"source_agent_id"`
		SourceTeamName      string           `json:"source_team_name"`
		SourceTeamAgentID   string           `json:"source_team_agent_id"`
		SourceTeamSessionID string           `json:"source_team_session_id"`
	}
	if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
		return fmt.Errorf("decode memory extraction job: %w", err)
	}
	controller := NewExtractionController(c.Config, c.BaseRegistry, c.ExtractionConfig)
	controller.Runner = c.Runner
	if strings.TrimSpace(payload.CWD) != "" {
		controller.Config.CWD = payload.CWD
	}
	controller.SourceAgentID = payload.SourceAgentID
	controller.SourceTeamName = payload.SourceTeamName
	controller.SourceTeamAgentID = payload.SourceTeamAgentID
	controller.SourceTeamSessionID = payload.SourceTeamSessionID
	controller.SourceSessionID = payload.SessionID
	if len(payload.Messages) == 0 && len(payload.MessageIDs) > 0 {
		store, err := longmemory.Open(ctx, controller.Config.LongTermMemoryStore)
		if err != nil {
			return err
		}
		spans, loadErr := store.EvidenceSpansByMessageIDs(ctx, payload.SessionID, payload.MessageIDs)
		_ = store.Close()
		if loadErr != nil {
			return loadErr
		}
		payload.Messages = make([]map[string]any, 0, len(spans))
		payload.MessageIDs = payload.MessageIDs[:0]
		for _, span := range spans {
			role := span.Role
			if role == "" {
				role = "unknown"
			}
			payload.Messages = append(payload.Messages, map[string]any{"role": role, "content": span.Text,
				"id": span.MessageID, "timestamp": span.OccurredAt.Format(time.RFC3339)})
			payload.MessageIDs = append(payload.MessageIDs, span.MessageID)
		}
	}
	if len(payload.Messages) == 0 || len(payload.Messages) != len(payload.MessageIDs) {
		return errors.New("memory extraction job does not contain a complete visible message window")
	}
	_, err := controller.runLongTermExtraction(ctx, &extractionContext{Messages: payload.Messages,
		MessageIDs: payload.MessageIDs, StartIndex: payload.StartMessageIndex, EndIndex: payload.EndMessageIndex,
		SessionID: payload.SessionID, ConsumerID: payload.ConsumerID, StorePath: controller.Config.LongTermMemoryStore})
	return err
}

func remapAcceptedExtractionReferences(facts []longmemory.Fact, spans []longmemory.EvidenceSpan, edges []longmemory.Edge,
	accepted map[int]int) ([]longmemory.Fact, []longmemory.EvidenceSpan, []longmemory.Edge) {
	filteredFacts := facts[:0]
	for _, fact := range facts {
		if index, ok := accepted[fact.MemoryIndex]; ok {
			fact.MemoryIndex = index
			filteredFacts = append(filteredFacts, fact)
		}
	}
	filteredSpans := spans[:0]
	for _, span := range spans {
		if index, ok := accepted[span.MemoryIndex]; ok {
			span.MemoryIndex = index
			filteredSpans = append(filteredSpans, span)
		}
	}
	filteredEdges := edges[:0]
	for _, edge := range edges {
		from, fromOK := accepted[edge.FromMemoryIndex]
		to, toOK := accepted[edge.ToMemoryIndex]
		if fromOK && toOK {
			edge.FromMemoryIndex = from
			edge.ToMemoryIndex = to
			filteredEdges = append(filteredEdges, edge)
		}
	}
	return filteredFacts, filteredSpans, filteredEdges
}

func (c *ExtractionController) defaultRunner(parentState *AgentState) ExtractionRunner {
	return func(ctx context.Context, prompt, systemPrompt string, _ *coretools.ToolRegistry, extraContext coretools.ExecutionContext) (string, error) {
		model := c.Config.APIModel
		if c.Config.ExtractionModel != nil && *c.Config.ExtractionModel != "" {
			model = *c.Config.ExtractionModel
		}
		extractionTool := newExtractMemoryBatchTool()
		registry := coretools.NewToolRegistry(extractionTool)
		sub := NewSubAgent(c.Config, registry, ExtractionAgentDef, parentState, model, "auto-memory-extract", extraContext)
		if _, err := sub.Run(ctx, prompt); err != nil {
			return "", err
		}
		result, ok, err := extractionTool.batchJSON()
		if err != nil {
			return "", fmt.Errorf("encode extracted memory batch: %w", err)
		}
		if !ok {
			return "", errors.New("memory extraction model did not call ExtractMemoryBatch")
		}
		return result, nil
	}
}

func (c *ExtractionController) buildRawIngestionBatch(ctx context.Context, payload *extractionContext, agentID string) longmemory.ExtractionBatch {
	now := time.Now().UTC()
	episode := &longmemory.Episode{
		ScopeType: longmemory.ScopeProject, ScopeKey: longmemory.ProjectScopeKey(c.Config.CWD),
		SessionID: payload.SessionID, TeamSessionID: c.SourceTeamSessionID, AgentID: agentID,
		MessageIDs: append([]string(nil), payload.MessageIDs...), Kind: "conversation",
		Content: extractionSearchText(payload.Messages), OccurredAt: extractionOccurredAt(payload.Messages, now), ObservedAt: now,
	}
	batch := longmemory.ExtractionBatch{Episode: episode, ConsumerID: payload.ConsumerID + ":ingestion",
		SessionID: payload.SessionID, LastMessageIndex: payload.EndIndex,
		AtomTargetTokens: c.Config.MemoryAtomTargetTokens, AtomMaxTokens: c.Config.MemoryAtomMaxTokens}
	if len(payload.MessageIDs) > 0 {
		batch.LastMessageID = payload.MessageIDs[len(payload.MessageIDs)-1]
	}
	for index, message := range payload.Messages {
		if index >= len(payload.MessageIDs) {
			break
		}
		role := strings.ToLower(strings.TrimSpace(stringFromAny(message["role"])))
		if role != "user" && role != "assistant" {
			continue
		}
		text := visibleMessageText(message["content"])
		if text == "" {
			continue
		}
		batch.EpisodeSpans = append(batch.EpisodeSpans, longmemory.EvidenceSpan{MessageID: payload.MessageIDs[index],
			Role: role, Text: text, OccurredAt: extractionMessageOccurredAt(message, episode.OccurredAt)})
	}
	sessionIndexID := longmemory.StableID(episode.ScopeType, episode.ScopeKey, "session-index", episode.SessionID)
	for _, span := range batch.EpisodeSpans {
		span.MemoryID = sessionIndexID
		span.ScopeType = episode.ScopeType
		span.ScopeKey = episode.ScopeKey
		span.SessionID = episode.SessionID
		batch.Chunks = append(batch.Chunks, longmemory.BuildEvidenceChunks(span)...)
	}
	if c.Config.MemoryEmbeddingEnabled {
		if embedder := configuredMemoryEmbedder(c.Config); embedder != nil {
			if len(batch.Chunks) > 0 {
				texts := make([]string, len(batch.Chunks))
				for index := range batch.Chunks {
					texts[index] = batch.Chunks[index].Text
				}
				if vectors, err := embedder.Embed(ctx, texts, longmemory.EmbeddingPassage); err == nil {
					for index, vector := range vectors {
						if index >= len(batch.Chunks) {
							break
						}
						batch.ChunkEmbeddings = append(batch.ChunkEmbeddings, longmemory.MemoryEmbedding{
							MemoryID: batch.Chunks[index].ChunkID, Model: embedder.Model(),
							ContentHash: batch.Chunks[index].ContentHash, Vector: vector})
					}
				}
			}
			if vectors, err := embedder.Embed(ctx, []string{episode.Content}, longmemory.EmbeddingPassage); err == nil && len(vectors) == 1 {
				batch.SessionEmbedding = &longmemory.MemoryEmbedding{Model: embedder.Model(),
					ContentHash: longmemory.StableID(episode.ScopeType, episode.ScopeKey, episode.SessionID, episode.Content), Vector: vectors[0]}
			}
		}
		if len(batch.ChunkEmbeddings) != len(batch.Chunks) || batch.SessionEmbedding == nil {
			batch.Jobs = append(batch.Jobs, longmemory.Job{Kind: "embedding_backfill",
				ScopeType: longmemory.ScopeProject, ScopeKey: longmemory.ProjectScopeKey(c.Config.CWD),
				Payload: fmt.Sprintf(`{"session_id":%q,"source":"raw_ingestion"}`, payload.SessionID)})
		}
	}
	return batch
}

func BuildLongTermExtractionPrompt(messagesSlice []map[string]any, existing []longmemory.Entry) string {
	return BuildLongTermExtractionPromptWithIDs(messagesSlice, nil, existing)
}

func BuildLongTermExtractionPromptWithIDs(messagesSlice []map[string]any, messageIDs []string, existing []longmemory.Entry) string {
	var msgLines []string
	for index, msg := range messagesSlice {
		role := stringFromAny(msg["role"])
		if role == "" {
			role = "unknown"
		}
		messageID := ""
		if index < len(messageIDs) {
			messageID = messageIDs[index]
		}
		msgLines = append(msgLines, "## "+role+" [message_id="+messageID+"]\n"+formatExtractionMessageContent(msg["content"]))
	}
	var existingLines []string
	for _, entry := range existing {
		existingLines = append(existingLines, "- "+entry.MemoryID+" ["+string(entry.ScopeType)+"/"+string(entry.MemoryType)+"] "+entry.Title+": "+entry.Summary)
	}
	if len(existingLines) == 0 {
		existingLines = append(existingLines, "(none)")
	}
	return `## Existing related long-term memories

` + strings.Join(existingLines, "\n") + `

## Recent conversation

` + strings.Join(msgLines, "\n\n") + `

## Task

Extract only durable cross-session memories. Call ExtractMemoryBatch exactly once with this shape:

{
  "memories": [
    {
      "action": "create|update|supersede|ignore",
      "target_memory_id": "existing memory_id for update/supersede, otherwise empty",
      "memory_id": "existing memory_id when action=update, otherwise empty",
      "scope_type": "user|project|team|agent_type|team_agent",
      "scope_key": "",
      "memory_type": "semantic|episodic|procedural|preference|feedback|project|reference",
      "title": "short title",
      "summary": "one-line summary",
      "content": "specific reusable memory",
      "tags": ["tag"],
      "entities": ["entity"],
      "importance": 0.0,
      "confidence": 0.0,
	  "epistemic_status": "reported|observed|derived|suggested|hypothetical|questioned",
	  "source_message_ids": ["exact source message_id"],
      "source_paths": ["optional path"]
    }
  ],
  "facts": [
    {
      "memory_index": 0,
      "subject": "normalized entity",
      "predicate": "stable relation name",
      "object": "fact value",
      "qualifiers": {},
      "confidence": 0.0,
      "valid_from": "RFC3339 when known, otherwise empty",
      "valid_until": "RFC3339 when known, otherwise empty"
    }
  ],
  "edges": [
    {"from_memory_index": 0, "to_memory_index": 1, "edge_type": "related_to|supports|contradicts|derived_from|next_event", "weight": 0.0, "confidence": 0.0}
  ],
  "core_blocks": [
    {"scope_type": "user|project|team|agent_type|team_agent", "scope_key": "", "label": "short stable label", "description": "", "content": "small always-useful content"}
  ]
}

Rules:
- Save information when it can help a future session understand the user's history, preferences, commitments, environment, decisions, or prior work.
- Treat explicit autobiographical events, dated activities, purchases, ownership, relationships, completed tasks, plans, and user feedback as durable episodic evidence when the user may refer to them later.
- Do not discard a specific user event merely because it happened once; use episodic memory and preserve its date and exact evidence.
- Do not save secrets, API keys, credentials, or incidental one-off details.
- Do not save generic advice or generated boilerplate unless the user says they adopted it or it records a reusable decision.
- Prefer project scope for project decisions and user scope only for durable user preferences.
- Use procedural for durable behavior rules.
- Compare against existing memories. Use create for new knowledge, update to revise the same durable memory in place, supersede when a new memory replaces an outdated one, and ignore for duplicates or weak candidates.
- For update and supersede, set target_memory_id to the relevant existing memory_id.
- Every saved memory must include at least one source_message_ids entry copied exactly from the recent conversation. The runtime attaches the original message text; do not reproduce or rewrite evidence text.
- Label epistemic_status from the cited source: reported for direct reports, observed for tool or system observations, derived for conclusions supported by evidence, suggested for advice, hypothetical for unconfirmed scenarios, and questioned for questions rather than assertions.
- Facts must use memory_index to reference the zero-based memories array. Extract valid_from/valid_until from the conversation when present; do not use the extraction time as event time.
- Core blocks are only for compact, repeatedly useful preferences, project invariants, or team policies. Do not copy ordinary episodic details into core blocks.
- Return {"memories":[]} when nothing should be remembered.`
}

func ParseLongTermExtractionBatch(text string) longmemory.ExtractionBatch {
	text = strings.TrimSpace(text)
	if matchStart := strings.Index(text, "{"); matchStart >= 0 {
		if matchEnd := strings.LastIndex(text, "}"); matchEnd >= matchStart {
			text = text[matchStart : matchEnd+1]
		}
	}
	var batch longmemory.ExtractionBatch
	_ = json.Unmarshal([]byte(text), &batch)
	remap := map[int]int{}
	filtered := batch.Memories[:0]
	for oldIndex, candidate := range batch.Memories {
		if normalizeMemoryAction(candidate.Action) == "ignore" {
			continue
		}
		if strings.TrimSpace(candidate.Title) == "" && strings.TrimSpace(candidate.Content) == "" && strings.TrimSpace(candidate.Summary) == "" {
			continue
		}
		remap[oldIndex] = len(filtered)
		filtered = append(filtered, candidate)
	}
	batch.Memories = filtered
	filteredFacts := batch.Facts[:0]
	for _, fact := range batch.Facts {
		if newIndex, ok := remap[fact.MemoryIndex]; ok {
			fact.MemoryIndex = newIndex
			filteredFacts = append(filteredFacts, fact)
		}
	}
	batch.Facts = filteredFacts
	filteredSpans := batch.Spans[:0]
	for _, span := range batch.Spans {
		if newIndex, ok := remap[span.MemoryIndex]; ok {
			span.MemoryIndex = newIndex
			filteredSpans = append(filteredSpans, span)
		}
	}
	batch.Spans = filteredSpans
	filteredEdges := batch.Edges[:0]
	for _, edge := range batch.Edges {
		fromIndex, fromOK := remap[edge.FromMemoryIndex]
		toIndex, toOK := remap[edge.ToMemoryIndex]
		if fromOK && toOK {
			edge.FromMemoryIndex = fromIndex
			edge.ToMemoryIndex = toIndex
			filteredEdges = append(filteredEdges, edge)
		}
	}
	batch.Edges = filteredEdges
	return batch
}

func ParseLongTermMemoryCandidates(text string) []longmemory.Candidate {
	return ParseLongTermExtractionBatch(text).Memories
}

func normalizeMemoryAction(action string) string {
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "", "create":
		return "create"
	case "update", "supersede", "ignore":
		return strings.ToLower(strings.TrimSpace(action))
	default:
		return "create"
	}
}

func longTermExtractionSystemPrompt() string {
	return `You are LuminaCode's long-term memory extraction engine.
You must submit structured memory candidates by calling ExtractMemoryBatch exactly once.
You never write files.
You never include secrets.
You separate session history from cross-session long-term memory.
You are conservative: durable, reusable, sourced memories only.`
}

func extractionSearchText(messagesSlice []map[string]any) string {
	var parts []string
	for _, msg := range messagesSlice {
		parts = append(parts, formatExtractionMessageContent(msg["content"]))
	}
	return strings.Join(parts, "\n")
}

func containsStringAgent(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func requiresMemoryConfirmation(cfg config.Config, candidate longmemory.Candidate) bool {
	if candidate.ScopeType == longmemory.ScopeUser && cfg.MemoryWriteConfirmUserScope {
		return true
	}
	if candidate.MemoryType == longmemory.TypeProcedural && cfg.MemoryWriteConfirmProcedural {
		return true
	}
	return false
}

func retentionPolicyFromConfig(cfg config.Config) longmemory.RetentionPolicy {
	if len(cfg.MemoryRetentionDays) == 0 {
		return nil
	}
	policy := longmemory.RetentionPolicy{}
	for key, days := range cfg.MemoryRetentionDays {
		policy[longmemory.MemoryType(key)] = days
	}
	return policy
}

func FormatExtractionResult(agentResult string) string {
	summary := strings.TrimSpace(agentResult)
	summary = truncateExtractionRunes(summary, extractionResultPreviewChars)
	if summary == "" {
		return ""
	}
	return "<system-reminder note=\"auto-memory\">\nBackground memory extraction completed:\n" + summary + "\n</system-reminder>"
}

func extractionMessageID(sessionID string, index int, message map[string]any) string {
	if id := strings.TrimSpace(stringFromAny(message["id"])); id != "" {
		return id
	}
	role := stringFromAny(message["role"])
	content := formatExtractionMessageContent(message["content"])
	return longmemory.StableID(longmemory.ScopeProject, sessionID, fmt.Sprintf("message-%d-%s", index, role), content)
}

func extractionOccurredAt(messages []map[string]any, fallback time.Time) time.Time {
	for _, message := range messages {
		for _, key := range []string{"created_at", "timestamp", "occurred_at"} {
			if value := strings.TrimSpace(stringFromAny(message[key])); value != "" {
				if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
					return parsed.UTC()
				}
			}
		}
	}
	return fallback
}

func extractionMessageOccurredAt(message map[string]any, fallback time.Time) time.Time {
	for _, key := range []string{"created_at", "timestamp", "occurred_at"} {
		if value := strings.TrimSpace(stringFromAny(message[key])); value != "" {
			if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil {
				return parsed.UTC()
			}
		}
	}
	return fallback
}

func validateExtractionSpans(spans []longmemory.EvidenceSpan, candidates []longmemory.Candidate, payload *extractionContext) []longmemory.EvidenceSpan {
	type sourceMessage struct {
		text       string
		role       string
		occurredAt time.Time
	}
	byMessage := map[string]sourceMessage{}
	for index, messageID := range payload.MessageIDs {
		if index < len(payload.Messages) {
			byMessage[messageID] = sourceMessage{text: formatExtractionMessageContent(payload.Messages[index]["content"]),
				role:       strings.ToLower(strings.TrimSpace(stringFromAny(payload.Messages[index]["role"]))),
				occurredAt: extractionMessageOccurredAt(payload.Messages[index], time.Now().UTC())}
		}
	}
	var valid []longmemory.EvidenceSpan
	for memoryIndex, candidate := range candidates {
		for _, messageID := range candidate.SourceMessageIDs {
			source, ok := byMessage[messageID]
			if !ok || strings.TrimSpace(source.text) == "" {
				continue
			}
			valid = append(valid, longmemory.EvidenceSpan{MemoryIndex: memoryIndex,
				ScopeType: candidate.ScopeType, ScopeKey: candidate.ScopeKey, SessionID: payload.SessionID,
				MessageID: messageID, Role: source.role, Text: source.text, StartRune: 0,
				EndRune: len([]rune(source.text)), OccurredAt: source.occurredAt})
		}
	}
	return valid
}

func retainMemoriesWithEvidence(batch longmemory.ExtractionBatch) longmemory.ExtractionBatch {
	hasEvidence := map[int]struct{}{}
	for _, span := range batch.Spans {
		hasEvidence[span.MemoryIndex] = struct{}{}
	}
	remap := map[int]int{}
	memories := make([]longmemory.Candidate, 0, len(batch.Memories))
	for oldIndex, candidate := range batch.Memories {
		if _, ok := hasEvidence[oldIndex]; !ok {
			continue
		}
		remap[oldIndex] = len(memories)
		memories = append(memories, candidate)
	}
	batch.Memories = memories
	facts := batch.Facts[:0]
	for _, fact := range batch.Facts {
		newIndex, ok := remap[fact.MemoryIndex]
		if !ok {
			continue
		}
		fact.MemoryIndex = newIndex
		facts = append(facts, fact)
	}
	batch.Facts = facts
	spans := batch.Spans[:0]
	for _, span := range batch.Spans {
		newIndex, ok := remap[span.MemoryIndex]
		if !ok {
			continue
		}
		span.MemoryIndex = newIndex
		spans = append(spans, span)
	}
	batch.Spans = spans
	edges := batch.Edges[:0]
	for _, edge := range batch.Edges {
		fromIndex, fromOK := remap[edge.FromMemoryIndex]
		toIndex, toOK := remap[edge.ToMemoryIndex]
		if !fromOK || !toOK {
			continue
		}
		edge.FromMemoryIndex = fromIndex
		edge.ToMemoryIndex = toIndex
		edges = append(edges, edge)
	}
	batch.Edges = edges
	return batch
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
