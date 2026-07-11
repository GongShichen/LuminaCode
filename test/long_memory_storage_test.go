package test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/longmemory"
)

type fixedMemoryEmbedder struct {
	model  string
	vector []float32
}

func (e fixedMemoryEmbedder) Model() string { return e.model }

func (e fixedMemoryEmbedder) Dimensions() int { return len(e.vector) }

func (e fixedMemoryEmbedder) Embed(_ context.Context, texts []string, _ longmemory.EmbeddingKind) ([][]float32, error) {
	result := make([][]float32, len(texts))
	for index := range result {
		result[index] = append([]float32(nil), e.vector...)
	}
	return result, nil
}

func TestMemoryStorageCommitsFactTimelineEvidenceEmbeddingAndCursor(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	firstTime := time.Date(2025, 1, 2, 10, 0, 0, 0, time.UTC)
	first := longmemory.Candidate{ScopeType: scope.Type, ScopeKey: scope.Key, MemoryType: longmemory.TypeProject,
		Title: "Package manager", Content: "The project uses npm.", Summary: "Uses npm", Entities: []string{"package manager"},
		Confidence: 0.9, Importance: 0.8, SourceSessionID: "session-a", SourceMessageIDs: []string{"message-a"}, ValidFrom: firstTime}
	first.MemoryID = longmemory.StableID(first.ScopeType, first.ScopeKey, first.Title, first.Content)
	if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{
		Memories: []longmemory.Candidate{first},
		Facts: []longmemory.Fact{{MemoryIndex: 0, ScopeType: scope.Type, ScopeKey: scope.Key, Subject: "project",
			Predicate: "package_manager", Object: "npm", Confidence: 0.9, ValidFrom: firstTime, ObservedAt: firstTime}},
		Spans: []longmemory.EvidenceSpan{{MemoryIndex: 0, ScopeType: scope.Type, ScopeKey: scope.Key,
			SessionID: "session-a", MessageID: "message-a", Text: "The project uses npm.", OccurredAt: firstTime}},
		Embeddings: []longmemory.MemoryEmbedding{{MemoryIndex: 0, Model: "test-embedding", ContentHash: "hash-a", Vector: []float32{1, 0}}},
		ConsumerID: "extractor", SessionID: "session-a", LastMessageID: "message-a", LastMessageIndex: 3,
	}); err != nil {
		t.Fatal(err)
	}
	secondTime := firstTime.AddDate(0, 1, 0)
	second := first
	second.Title = "Updated package manager"
	second.Content = "The project now uses pnpm."
	second.Summary = "Uses pnpm"
	second.ValidFrom = secondTime
	second.SourceSessionID = "session-b"
	second.SourceMessageIDs = []string{"message-b"}
	second.MemoryID = longmemory.StableID(second.ScopeType, second.ScopeKey, second.Title, second.Content)
	if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{
		Memories: []longmemory.Candidate{second},
		Facts: []longmemory.Fact{{MemoryIndex: 0, ScopeType: scope.Type, ScopeKey: scope.Key, Subject: "project",
			Predicate: "package_manager", Object: "pnpm", Confidence: 0.95, ValidFrom: secondTime, ObservedAt: secondTime}},
		Spans: []longmemory.EvidenceSpan{{MemoryIndex: 0, ScopeType: scope.Type, ScopeKey: scope.Key,
			SessionID: "session-b", MessageID: "message-b", Text: "The project now uses pnpm.", OccurredAt: secondTime}},
		Embeddings: []longmemory.MemoryEmbedding{{MemoryIndex: 0, Model: "test-embedding", ContentHash: "hash-b", Vector: []float32{0.95, 0.05}}},
	}); err != nil {
		t.Fatal(err)
	}
	current, err := store.ResolveFactsAt(ctx, []longmemory.Scope{scope}, []string{"project"}, time.Time{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(current) != 1 || current[0].Object != "pnpm" {
		t.Fatalf("expected current pnpm fact, got %#v", current)
	}
	historical, err := store.ResolveFactsAt(ctx, []longmemory.Scope{scope}, []string{"project"}, firstTime.Add(24*time.Hour), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(historical) != 1 || historical[0].Object != "npm" {
		t.Fatalf("expected historical npm fact, got %#v", historical)
	}
	messageID, index, err := store.GetCursor(ctx, "extractor", "session-a")
	if err != nil || messageID != "message-a" || index != 3 {
		t.Fatalf("unexpected extraction cursor: id=%q index=%d err=%v", messageID, index, err)
	}
	vectorHits, err := store.SearchVector(ctx, []float32{1, 0}, "test-embedding", longmemory.SearchOptions{Scopes: []longmemory.Scope{scope}, MaxCandidates: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectorHits) != 2 || vectorHits[0].MemoryID != first.MemoryID {
		t.Fatalf("unexpected vector ordering: %#v", vectorHits)
	}
	spans, err := store.ListEvidenceSpans(ctx, []string{second.MemoryID})
	if err != nil || len(spans[second.MemoryID]) != 1 || spans[second.MemoryID][0].MessageID != "message-b" {
		t.Fatalf("unexpected evidence spans: %#v err=%v", spans, err)
	}
}

func TestMemoryHybridSearchCombinesChannelsRespectsScopeAndRecordsTrace(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	foreign := longmemory.Scope{Type: longmemory.ScopeProject, Key: "foreign-project"}
	for index, item := range []struct {
		scope   longmemory.Scope
		title   string
		content string
		vector  []float32
	}{
		{scope, "Frontend preference", "The user prefers keyboard-driven terminal interfaces.", []float32{1, 0}},
		{scope, "Build procedure", "Run go test and npm test before release.", []float32{0.8, 0.2}},
		{foreign, "Foreign preference", "The other project uses keyboard navigation.", []float32{1, 0}},
	} {
		candidate := longmemory.Candidate{ScopeType: item.scope.Type, ScopeKey: item.scope.Key,
			MemoryType: longmemory.TypeProject, Title: item.title, Content: item.content, Summary: item.content,
			SourceSessionID: "session-" + string(rune('a'+index)), Confidence: 0.9, Importance: 0.7}
		candidate.MemoryID = longmemory.StableID(candidate.ScopeType, candidate.ScopeKey, candidate.Title, candidate.Content)
		if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{Memories: []longmemory.Candidate{candidate},
			Spans: []longmemory.EvidenceSpan{{MemoryIndex: 0, ScopeType: item.scope.Type, ScopeKey: item.scope.Key,
				SessionID: candidate.SourceSessionID, MessageID: "message", Text: item.content}},
			Embeddings: []longmemory.MemoryEmbedding{{MemoryIndex: 0, Model: "test-embedding", ContentHash: "hash", Vector: item.vector}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	query := longmemory.MemoryQuery{Text: "What terminal interface preference did the user mention?",
		Scopes: []longmemory.Scope{scope}, SessionID: "query-session", Timestamp: time.Now().UTC()}
	result, err := store.SearchAllChannels(ctx, query, longmemory.QueryExpansion{Queries: []string{"terminal interface preference"}},
		fixedMemoryEmbedder{model: "test-embedding", vector: []float32{1, 0}}, longmemory.HybridSearchOptions{
			FTSCandidates: 10, VectorCandidates: 10, GraphCandidates: 10, GraphMaxHops: 2,
			MaxItems: 4, RRFK: 60, MMRLambda: 0.75, TargetContextTokens: 1200, MaxContextTokens: 1600,
			SessionID: "query-session",
		})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Packet.Evidence) == 0 || !strings.Contains(result.Packet.Evidence[0].Text, "keyboard-driven") {
		t.Fatalf("expected relevant evidence first, got %#v", result.Packet.Evidence)
	}
	for _, evidence := range result.Packet.Evidence {
		if evidence.ScopeKey == foreign.Key {
			t.Fatalf("foreign project memory leaked into result: %#v", evidence)
		}
	}
	traces, err := store.ListRetrievalTraces(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(traces) != 1 || traces[0].SessionID != "query-session" || len(traces[0].SelectedIDs) == 0 {
		t.Fatalf("retrieval trace was not persisted: %#v", traces)
	}
	if traces[0].Run == nil || len(traces[0].Run.ChannelResults) != 6 {
		t.Fatalf("expected six-channel trace, got %#v", traces[0].Run)
	}
}

func TestMemorySessionIndexAccumulatesAndCreatesNextEventEdges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	memories := []longmemory.Candidate{
		{MemoryID: "event-one", ScopeType: scope.Type, ScopeKey: scope.Key, MemoryType: longmemory.TypeEpisodic,
			Title: "First decision", Content: "The team selected SQLite for local persistence.", Summary: "selected SQLite",
			SourceSessionID: "shared-session", SourceMessageIDs: []string{"message-one"}, Confidence: 1},
		{MemoryID: "event-two", ScopeType: scope.Type, ScopeKey: scope.Key, MemoryType: longmemory.TypeEpisodic,
			Title: "Second decision", Content: "The team selected WAL mode for concurrent sessions.", Summary: "selected WAL mode",
			SourceSessionID: "shared-session", SourceMessageIDs: []string{"message-two"}, Confidence: 1},
	}
	for index, candidate := range memories {
		messageID := candidate.SourceMessageIDs[0]
		if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{
			Episode: &longmemory.Episode{ScopeType: scope.Type, ScopeKey: scope.Key, SessionID: "shared-session",
				AgentID: "backend", MessageIDs: []string{messageID}, Content: candidate.Content,
				OccurredAt: time.Date(2026, 1, index+1, 10, 0, 0, 0, time.UTC)},
			EpisodeSpans: []longmemory.EvidenceSpan{{MessageID: messageID, Text: candidate.Content,
				OccurredAt: time.Date(2026, 1, index+1, 10, 0, 0, 0, time.UTC)}},
			Memories: []longmemory.Candidate{candidate},
			Spans: []longmemory.EvidenceSpan{{MemoryIndex: 0, ScopeType: scope.Type, ScopeKey: scope.Key,
				SessionID: "shared-session", MessageID: messageID, Text: candidate.Content}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	sessionHits, err := store.SearchSessions(ctx, []string{"SQLite WAL persistence"}, []longmemory.Scope{
		{Type: longmemory.ScopeUser, Key: longmemory.UserScopeKey()}, scope,
	}, 10)
	if err != nil {
		t.Fatal(err)
	}
	var sessionMemoryID string
	for _, entry := range sessionHits {
		if entry.DocumentKind == "session" {
			sessionMemoryID = entry.MemoryID
			if !strings.Contains(entry.Content, "selected SQLite") || !strings.Contains(entry.Content, "selected WAL mode") {
				t.Fatalf("session parent did not accumulate both extraction batches: %#v", entry)
			}
			break
		}
	}
	if sessionMemoryID == "" {
		t.Fatalf("session index was not exposed as episodic evidence: %#v", sessionHits)
	}
	if entry, err := store.Get(ctx, sessionMemoryID); err != nil || entry.SourceSessionID != "shared-session" {
		t.Fatalf("Store.Get did not resolve Session evidence: entry=%#v err=%v", entry, err)
	}
	spans, err := store.ListEvidenceSpans(ctx, []string{sessionMemoryID})
	if err != nil || len(spans[sessionMemoryID]) < 2 {
		t.Fatalf("expected provenance spans across both extraction batches: %#v err=%v", spans, err)
	}
	graph, err := store.ExpandGraph(ctx, []string{"event-one"}, []longmemory.Scope{scope}, 2, 10)
	if err != nil {
		t.Fatal(err)
	}
	if graph["event-two"] <= 0 {
		t.Fatalf("next_event edge was not available to graph retrieval: %#v", graph)
	}
}

func TestMemoryAllTypesRemainEligibleForFullChannelRetrieval(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	types := []longmemory.MemoryType{longmemory.TypeSemantic, longmemory.TypeEpisodic, longmemory.TypeProcedural,
		longmemory.TypePreference, longmemory.TypeFeedback, longmemory.TypeProject, longmemory.TypeReference}
	for index, memoryType := range types {
		if _, err := store.Upsert(ctx, longmemory.Candidate{MemoryID: "type-" + string(memoryType), ScopeType: scope.Type,
			ScopeKey: scope.Key, MemoryType: memoryType, Title: "shared retrieval marker",
			Content: "shared retrieval marker content " + string(rune('a'+index)), Confidence: 1}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.SearchAllChannels(ctx, longmemory.MemoryQuery{Text: "shared retrieval marker",
		Timestamp: time.Now().UTC(), Scopes: []longmemory.Scope{scope}}, longmemory.QueryExpansion{}, nil,
		longmemory.HybridSearchOptions{FTSCandidates: 20, MaxItems: 20, MaxContextTokens: 6000})
	if err != nil {
		t.Fatal(err)
	}
	seenTypes := map[longmemory.MemoryType]bool{}
	for _, candidate := range result.Trace.Candidates {
		seenTypes[candidate.Entry.MemoryType] = true
	}
	for _, memoryType := range types {
		if !seenTypes[memoryType] {
			t.Fatalf("memory type %q was filtered from full-channel candidates: %#v", memoryType, seenTypes)
		}
	}
}

func TestMemoryConsolidationMergesDuplicateProvenanceWithinScope(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	for index, sessionID := range []string{"session-a", "session-b"} {
		candidate := longmemory.Candidate{MemoryID: "duplicate-" + sessionID, ScopeType: scope.Type, ScopeKey: scope.Key,
			MemoryType: longmemory.TypeSemantic, Title: "Build requirement", Content: "Run all tests before release.",
			Summary: "All tests are required.", Entities: []string{"release"}, SourceSessionID: sessionID,
			SourceMessageIDs: []string{"message-" + sessionID}, Importance: 0.7 + float64(index)*0.1, Confidence: 0.9}
		if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{Memories: []longmemory.Candidate{candidate},
			Spans: []longmemory.EvidenceSpan{{MemoryIndex: 0, ScopeType: scope.Type, ScopeKey: scope.Key,
				SessionID: sessionID, MessageID: "message-" + sessionID, Text: candidate.Content}},
			Embeddings: []longmemory.MemoryEmbedding{{MemoryIndex: 0, Model: "test-embedding", ContentHash: sessionID, Vector: []float32{1, 0}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	result, err := store.Consolidate(ctx, "test-embedding", 20)
	if err != nil {
		t.Fatal(err)
	}
	if result.Consolidated != 1 {
		t.Fatalf("expected one duplicate merge, got %#v", result)
	}
	entries, err := store.List(ctx, longmemory.SearchOptions{Scopes: []longmemory.Scope{scope}, IncludeInactive: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	var active *longmemory.Entry
	for index := range entries {
		if entries[index].Status == longmemory.StatusActive {
			active = &entries[index]
		}
	}
	if active == nil {
		t.Fatal("expected an active canonical memory")
	}
	spans, err := store.ListEvidenceSpans(ctx, []string{active.MemoryID})
	if err != nil || len(spans[active.MemoryID]) != 2 {
		t.Fatalf("expected merged provenance spans, got %#v err=%v", spans, err)
	}
}

func TestMemoryMaintenanceJobsPersistRetryState(t *testing.T) {
	ctx := context.Background()
	store, err := longmemory.Open(ctx, filepath.Join(t.TempDir(), "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	job := longmemory.Job{Kind: "extraction", ScopeType: longmemory.ScopeProject, ScopeKey: "project-a", Payload: `{"session_id":"a"}`}
	if err := store.EnqueueJob(ctx, job); err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimJobs(ctx, []string{"extraction"}, 1)
	if err != nil || len(claimed) != 1 || claimed[0].Status != "running" {
		t.Fatalf("unexpected claimed jobs: %#v err=%v", claimed, err)
	}
	if err := store.RetryJob(ctx, claimed[0].JobID, context.DeadlineExceeded, time.Second); err != nil {
		t.Fatal(err)
	}
	jobs, err := store.ListJobs(ctx, []string{"extraction"}, []string{"retry"}, 10)
	if err != nil || len(jobs) != 1 || !strings.Contains(jobs[0].LastError, "deadline") {
		t.Fatalf("retry state was not persisted: %#v err=%v", jobs, err)
	}
}

func TestLegacyMarkdownMemoryMigratesWithoutDeletingSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	legacyPath := filepath.Join(home, ".Lumina", "projects", "sample-project", "memory", "feedback", "rule.md")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\ntitle: Runtime location\nmemory_type: feedback\nconfidence: 0.9\n---\n# Runtime location\n\nDo not write runtime files into the project workspace.\n"
	if err := os.WriteFile(legacyPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := longmemory.Open(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entries, err := store.Search(context.Background(), longmemory.SearchOptions{Query: "runtime project workspace", IncludeInactive: true, Limit: 10})
	if err != nil || len(entries) != 1 || entries[0].MemoryType != longmemory.TypeFeedback {
		t.Fatalf("legacy memory was not migrated: %#v err=%v", entries, err)
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("legacy source file was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".lumina", "memory", "migration-log.jsonl")); err != nil {
		t.Fatalf("migration log was not written: %v", err)
	}
}

func TestReadOnlyCoreBlockCannotBeOverwritten(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{CoreBlocks: []longmemory.CoreBlock{{
		ScopeType: scope.Type, ScopeKey: scope.Key, Label: "safety", Content: "Never commit credentials.", ReadOnly: true,
	}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{CoreBlocks: []longmemory.CoreBlock{{
		ScopeType: scope.Type, ScopeKey: scope.Key, Label: "safety", Content: "Credentials are allowed.",
	}}}); err != nil {
		t.Fatal(err)
	}
	blocks, err := store.ListCoreBlocks(ctx, []longmemory.Scope{scope})
	if err != nil || len(blocks) != 1 || blocks[0].Content != "Never commit credentials." || !blocks[0].ReadOnly {
		t.Fatalf("read-only core block was overwritten: %#v err=%v", blocks, err)
	}
}

func TestHardDeleteCascadesExtendedMemoryRecords(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	candidate := longmemory.Candidate{MemoryID: "delete-me", ScopeType: scope.Type, ScopeKey: scope.Key,
		MemoryType: longmemory.TypeProject, Title: "Temporary fact", Content: "The temporary value is one.",
		SourceSessionID: "session-delete", SourceMessageIDs: []string{"message-delete"}}
	if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{Memories: []longmemory.Candidate{candidate},
		Facts:      []longmemory.Fact{{MemoryIndex: 0, ScopeType: scope.Type, ScopeKey: scope.Key, Subject: "temporary", Predicate: "value", Object: "one"}},
		Spans:      []longmemory.EvidenceSpan{{MemoryIndex: 0, ScopeType: scope.Type, ScopeKey: scope.Key, SessionID: "session-delete", MessageID: "message-delete", Text: candidate.Content}},
		Embeddings: []longmemory.MemoryEmbedding{{MemoryIndex: 0, Model: "test-embedding", ContentHash: "delete", Vector: []float32{1, 0}}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, candidate.MemoryID, true); err != nil {
		t.Fatal(err)
	}
	spans, err := store.ListEvidenceSpans(ctx, []string{candidate.MemoryID})
	if err != nil || len(spans[candidate.MemoryID]) != 0 {
		t.Fatalf("evidence spans survived hard delete: %#v err=%v", spans, err)
	}
	vectors, err := store.SearchVector(ctx, []float32{1, 0}, "test-embedding", longmemory.SearchOptions{Scopes: []longmemory.Scope{scope}, Limit: 10})
	if err != nil || len(vectors) != 0 {
		t.Fatalf("embedding survived hard delete: %#v err=%v", vectors, err)
	}
	facts, err := store.ResolveFactsAt(ctx, []longmemory.Scope{scope}, []string{"temporary"}, time.Time{}, 10)
	if err != nil || len(facts) != 0 {
		t.Fatalf("fact survived hard delete: %#v err=%v", facts, err)
	}
}
