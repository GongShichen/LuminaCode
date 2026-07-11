package test

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
	"LuminaCode/longmemory"
	coretools "LuminaCode/tools"
)

type blockingMemoryEmbedder struct{}

func (blockingMemoryEmbedder) Model() string   { return "blocking" }
func (blockingMemoryEmbedder) Dimensions() int { return 2 }
func (blockingMemoryEmbedder) Embed(ctx context.Context, _ []string, _ longmemory.EmbeddingKind) ([][]float32, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestRawMemoryIngestionSurvivesSemanticExtractionFailure(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	storePath := filepath.Join(root, "memory.sqlite")
	cfg := config.NewConfigForCWD(root)
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = storePath
	cfg.MemoryEmbeddingEnabled = false
	cfg.MemoryWriteConfirmProcedural = false
	state := agent.NewAgentState()
	state.MemorySessionID = "raw-ingestion-session"
	tail := "zircon-needle-8472 is the durable decision"
	state.Messages = []map[string]any{{"role": "user", "id": "message-tail",
		"timestamp": time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC).Format(time.RFC3339),
		"content":   strings.Repeat("background paragraph. ", 180) + tail}}
	controller := agent.NewExtractionController(cfg, coretools.NewToolRegistry())
	controller.Runner = func(context.Context, string, string, *coretools.ToolRegistry, coretools.ExecutionContext) (string, error) {
		return "", errors.New("semantic extractor unavailable")
	}
	if _, err := controller.ExtractNow(ctx, &state); err == nil {
		t.Fatal("semantic extraction failure must remain retryable")
	}
	if state.MemoryExtractionCursor != 0 {
		t.Fatalf("semantic cursor advanced after failure: %d", state.MemoryExtractionCursor)
	}
	store, err := longmemory.Open(ctx, storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	hits, err := store.SearchChunkBM25(ctx, []string{"zircon needle 8472"}, []longmemory.Scope{scope}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 || !strings.Contains(hits[0].Content, tail) {
		t.Fatalf("tail evidence was not indexed before semantic extraction: %#v", hits)
	}
	_, index, err := store.GetCursor(ctx, "long-term-extraction:main:ingestion", "raw-ingestion-session")
	if err != nil || index != 0 {
		t.Fatalf("raw ingestion cursor was not committed: index=%d err=%v", index, err)
	}
}

func TestSemanticExtractionJobResumesFromPersistedVisibleMessages(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	storePath := filepath.Join(root, "memory.sqlite")
	cfg := config.NewConfigForCWD(root)
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = storePath
	cfg.MemoryEmbeddingEnabled = false
	cfg.MemoryWriteConfirmProcedural = false
	state := agent.NewAgentState()
	state.MemorySessionID = "restart-session"
	state.Messages = []map[string]any{{"role": "user", "id": "restart-source", "content": "Use WAL for restart safety."}}
	failing := agent.NewExtractionController(cfg, coretools.NewToolRegistry())
	failing.Runner = func(context.Context, string, string, *coretools.ToolRegistry, coretools.ExecutionContext) (string, error) {
		return "", errors.New("temporary extraction outage")
	}
	if _, err := failing.ExtractNow(ctx, &state); err == nil {
		t.Fatal("expected retryable extraction failure")
	}
	store, err := longmemory.Open(ctx, storePath)
	if err != nil {
		t.Fatal(err)
	}
	jobs, err := store.ListJobs(ctx, []string{"extraction"}, []string{"retry"}, 10)
	if err != nil || len(jobs) != 1 {
		_ = store.Close()
		t.Fatalf("missing persisted extraction job: jobs=%#v err=%v", jobs, err)
	}
	_ = store.Close()
	retry := agent.NewExtractionController(cfg, coretools.NewToolRegistry())
	retry.Runner = func(context.Context, string, string, *coretools.ToolRegistry, coretools.ExecutionContext) (string, error) {
		return `{"memories":[{"scope_type":"project","memory_type":"procedural","title":"Restart safety","summary":"Use WAL","content":"Use WAL for restart safety.","source_message_ids":["restart-source"],"confidence":1}]}`, nil
	}
	if err := retry.ProcessExtractionJob(ctx, jobs[0]); err != nil {
		t.Fatal(err)
	}
	store, err = longmemory.Open(ctx, storePath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hits, err := store.Search(ctx, longmemory.SearchOptions{Query: "restart safety WAL",
		Scopes: []longmemory.Scope{{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}}, Limit: 10})
	if err != nil || len(hits) == 0 {
		t.Fatalf("persisted extraction job did not enrich memory: hits=%#v err=%v", hits, err)
	}
}

func TestEvidenceChunksParticipateInEveryRetrievalChannel(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	occurredAt := time.Date(2026, 5, 4, 12, 30, 0, 0, time.UTC)
	text := "Project Aurora selected SQLite WAL mode for concurrent local sessions."
	sessionIndexID := longmemory.StableID(scope.Type, scope.Key, "session-index", "session-aurora")
	span := longmemory.EvidenceSpan{MemoryID: sessionIndexID, ScopeType: scope.Type, ScopeKey: scope.Key,
		SessionID: "session-aurora", MessageID: "message-aurora", Role: "user", Text: text, OccurredAt: occurredAt}
	chunks := longmemory.BuildEvidenceChunks(span)
	if len(chunks) != 1 {
		t.Fatalf("unexpected test chunk count: %d", len(chunks))
	}
	if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{
		Episode: &longmemory.Episode{ScopeType: scope.Type, ScopeKey: scope.Key, SessionID: "session-aurora",
			AgentID: "main", MessageIDs: []string{"message-aurora"}, Kind: "conversation", Content: text,
			OccurredAt: occurredAt, ObservedAt: occurredAt},
		EpisodeSpans: []longmemory.EvidenceSpan{span},
		Memories: []longmemory.Candidate{{MemoryID: "aurora-decision", ScopeType: scope.Type, ScopeKey: scope.Key,
			MemoryType: longmemory.TypeProject, Title: "Aurora persistence", Content: text, Summary: "SQLite WAL selected",
			Entities: []string{"Project Aurora", "SQLite WAL"}, SourceSessionID: "session-aurora",
			SourceMessageIDs: []string{"message-aurora"}, Confidence: 1}},
		Spans: []longmemory.EvidenceSpan{{MemoryIndex: 0, ScopeType: scope.Type, ScopeKey: scope.Key,
			SessionID: "session-aurora", MessageID: "message-aurora", Role: "user", Text: text, OccurredAt: occurredAt}},
		ChunkEmbeddings: []longmemory.MemoryEmbedding{{MemoryID: chunks[0].ChunkID, Model: "fixed", ContentHash: chunks[0].ContentHash,
			Vector: []float32{1, 0}}},
		Embeddings: []longmemory.MemoryEmbedding{{MemoryIndex: 0, Model: "fixed", ContentHash: "decision", Vector: []float32{1, 0}}},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := store.SearchAllChannels(ctx, longmemory.MemoryQuery{Text: "How does Aurora persist concurrent sessions?",
		Timestamp: occurredAt.Add(time.Hour), Scopes: []longmemory.Scope{scope}, SessionID: "query-session"},
		longmemory.QueryExpansion{Entities: []string{"Project Aurora", "SQLite WAL"},
			TemporalConstraints: []longmemory.TemporalConstraint{{From: occurredAt.Add(-time.Hour), To: occurredAt.Add(time.Hour)}}},
		fixedMemoryEmbedder{model: "fixed", vector: []float32{1, 0}}, longmemory.HybridSearchOptions{
			FTSCandidates: 20, VectorCandidates: 20, GraphCandidates: 20, MaxItems: 8,
			TargetContextTokens: 1200, MaxContextTokens: 2000, NeighborChunks: 1,
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Run.ChannelResults) != 6 {
		t.Fatalf("expected every retrieval channel, got %#v", result.Run.ChannelResults)
	}
	for _, name := range []string{"bm25", "vector", "entity", "temporal", "session", "graph"} {
		found := false
		for _, channel := range result.Run.ChannelResults {
			if channel.Channel == name {
				found = true
				if name != "graph" && len(channel.Candidates) == 0 {
					t.Fatalf("channel %s did not return indexed evidence: %#v", name, channel)
				}
			}
		}
		if !found {
			t.Fatalf("channel %s did not execute", name)
		}
	}
	if len(result.Packet.Documents) == 0 || result.Packet.Documents[0].Kind != "atom" ||
		!strings.Contains(result.Packet.Documents[0].Text, "SQLite WAL") {
		t.Fatalf("final packet did not contain original chunk evidence: %#v", result.Packet)
	}
}

func TestSlowRetrievalChannelCannotDiscardCompletedChannelEvidence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	text := "The release codename is Silver Meridian."
	if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{
		Episode: &longmemory.Episode{ScopeType: scope.Type, ScopeKey: scope.Key, SessionID: "deadline-session",
			MessageIDs: []string{"deadline-message"}, Content: text, OccurredAt: time.Now().UTC(), ObservedAt: time.Now().UTC()},
		EpisodeSpans: []longmemory.EvidenceSpan{{MessageID: "deadline-message", Role: "user", Text: text, OccurredAt: time.Now().UTC()}},
	}); err != nil {
		t.Fatal(err)
	}
	result, err := store.SearchAllChannels(ctx, longmemory.MemoryQuery{Text: "Silver Meridian",
		Timestamp: time.Now().UTC(), Scopes: []longmemory.Scope{scope}}, longmemory.QueryExpansion{},
		blockingMemoryEmbedder{}, longmemory.HybridSearchOptions{FTSCandidates: 20, VectorCandidates: 20,
			GraphCandidates: 20, MaxItems: 8, TargetContextTokens: 1200, MaxContextTokens: 2000,
			LocalTimeout: 10 * time.Millisecond})
	if err != nil {
		t.Fatalf("one timed-out channel discarded the evidence packet: %v", err)
	}
	if len(result.Packet.Evidence) == 0 || !strings.Contains(result.Packet.Evidence[0].Text, "Silver Meridian") {
		t.Fatalf("completed BM25 evidence was discarded: %#v", result)
	}
	vectorTimedOut := false
	for _, channel := range result.Run.ChannelResults {
		if channel.Channel == "vector" && strings.Contains(channel.Error, "deadline") {
			vectorTimedOut = true
		}
	}
	if !vectorTimedOut {
		t.Fatalf("timed-out vector channel was not diagnosed: %#v", result.Run.ChannelResults)
	}
}

func TestChunkBackfillReusesCanonicalMessageChunks(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "memory.sqlite")
	store, err := longmemory.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	text := strings.Repeat("canonical evidence sentence. ", 80)
	span := longmemory.EvidenceSpan{ScopeType: scope.Type, ScopeKey: scope.Key, SessionID: "same-session",
		MessageID: "same-message", Role: "user", Text: text, OccurredAt: time.Now().UTC()}
	expected := len(longmemory.BuildEvidenceChunks(span))
	if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{
		Episode: &longmemory.Episode{ScopeType: scope.Type, ScopeKey: scope.Key, SessionID: "same-session",
			MessageIDs: []string{"same-message"}, Content: text, OccurredAt: span.OccurredAt, ObservedAt: span.OccurredAt},
		EpisodeSpans: []longmemory.EvidenceSpan{span},
		Memories: []longmemory.Candidate{{MemoryID: "semantic-copy", ScopeType: scope.Type, ScopeKey: scope.Key,
			MemoryType: longmemory.TypeSemantic, Title: "Canonical", Content: text, Summary: "Canonical",
			SourceSessionID: "same-session", SourceMessageIDs: []string{"same-message"}, Confidence: 1}},
		Spans: []longmemory.EvidenceSpan{{MemoryIndex: 0, ScopeType: scope.Type, ScopeKey: scope.Key,
			SessionID: "same-session", MessageID: "same-message", Role: "user", Text: text, OccurredAt: span.OccurredAt}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = longmemory.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	chunks, err := store.ChunksByMessageIDs(ctx, []longmemory.Scope{scope}, []string{"same-message"})
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != expected {
		t.Fatalf("backfill duplicated canonical chunks: got %d want %d", len(chunks), expected)
	}
	if len(chunks) >= 3 {
		middle, err := store.Get(ctx, chunks[len(chunks)/2].ChunkID)
		if err != nil {
			t.Fatal(err)
		}
		packet, err := store.BuildEvidencePacket(ctx, longmemory.QueryPlan{Query: "canonical evidence", TargetContextTokens: 2000},
			[]longmemory.CandidateScore{{MemoryID: middle.MemoryID, Entry: *middle, FusedScore: 1}}, nil,
			longmemory.HybridSearchOptions{MaxItems: 8, TargetContextTokens: 2000, MaxContextTokens: 3000, NeighborChunks: 1})
		if err != nil {
			t.Fatal(err)
		}
		if len(packet.Evidence) != 1 || len(packet.Evidence[0].DocumentIDs) != 3 || len(packet.Documents) != 3 {
			t.Fatalf("neighbor evidence IDs and documents diverged: %#v", packet)
		}
	}
}
