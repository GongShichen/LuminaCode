package test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/longmemory"
)

type batchingEmbedder struct {
	mu       sync.Mutex
	calls    int
	maxBatch int
}

func (b *batchingEmbedder) Model() string   { return "test-embedding" }
func (b *batchingEmbedder) Dimensions() int { return 3 }
func (b *batchingEmbedder) Embed(ctx context.Context, texts []string, _ longmemory.EmbeddingKind) ([][]float32, error) {
	b.mu.Lock()
	b.calls++
	if len(texts) > b.maxBatch {
		b.maxBatch = len(texts)
	}
	b.mu.Unlock()
	select {
	case <-time.After(15 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	result := make([][]float32, len(texts))
	for index := range result {
		result[index] = []float32{1, float32(index + 1), 1}
	}
	return result, nil
}

func TestEvidenceAtomsPreserveSourceOffsetsAndStructure(t *testing.T) {
	text := "Release constraints:\n- Database: SQLite.\n- Region: eu-west.\n- Rollback: keep the prior binary.\nThe release date is June 4."
	chunk := longmemory.EvidenceChunk{ChunkID: "chunk-a", MessageID: "message-a", SessionID: "session-a",
		ScopeType: longmemory.ScopeProject, ScopeKey: "project-a", Role: "user", Text: text,
		StartRune: 11, EndRune: 11 + len([]rune(text)), OccurredAt: time.Now().UTC()}
	atoms := longmemory.BuildEvidenceAtoms(chunk, 12, 24)
	if len(atoms) < 4 {
		t.Fatalf("expected sentence/list atoms, got %#v", atoms)
	}
	for _, atom := range atoms {
		start, end := atom.StartRune-chunk.StartRune, atom.EndRune-chunk.StartRune
		if start < 0 || end > len([]rune(text)) || start >= end {
			t.Fatalf("invalid atom offsets: %#v", atom)
		}
		if got := string([]rune(text)[start:end]); got != atom.Text {
			t.Fatalf("atom offset mismatch: got %q want %q", got, atom.Text)
		}
		if atom.EpistemicStatus != "reported" {
			t.Fatalf("user statement must retain reported provenance, got %q", atom.EpistemicStatus)
		}
	}
}

func TestEmbeddingSchedulerBatchesConcurrentQueriesWithoutQueueTimeout(t *testing.T) {
	base := &batchingEmbedder{}
	embedder := longmemory.SharedEmbeddingScheduler(base, longmemory.EmbeddingSchedulerOptions{
		BatchSize: 32, BatchWait: 30 * time.Millisecond, QueryCacheEntries: 128, ExecutionTimeout: time.Second})
	var wait sync.WaitGroup
	errors := make(chan error, 16)
	for index := 0; index < 16; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_, err := embedder.Embed(ctx, []string{"query " + string(rune('a'+index))}, longmemory.EmbeddingQuery)
			errors <- err
		}(index)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	base.mu.Lock()
	calls, maxBatch := base.calls, base.maxBatch
	base.mu.Unlock()
	if calls >= 16 || maxBatch <= 1 {
		t.Fatalf("scheduler did not micro-batch: calls=%d max_batch=%d", calls, maxBatch)
	}
}

func TestEvidenceLedgerCoversComplementaryFactsFromOneSession(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, root+"/memory.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	spans := []longmemory.EvidenceSpan{
		{MessageID: "m-database", Role: "user", Text: "The selected database is SQLite."},
		{MessageID: "m-region", Role: "user", Text: "Production deploys to the eu-west region."},
		{MessageID: "m-date", Role: "user", Text: "The release date is June 4."},
		{MessageID: "m-rollback", Role: "user", Text: "Rollback keeps the prior binary available."},
		{MessageID: "m-suggestion", Role: "assistant", Text: "You could consider Redis, many regions, flexible dates, and several rollback tools."},
	}
	for index := range spans {
		spans[index].OccurredAt = time.Date(2026, 6, index+1, 10, 0, 0, 0, time.UTC)
	}
	if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{
		Episode: &longmemory.Episode{ScopeType: scope.Type, ScopeKey: scope.Key, SessionID: "release-session",
			MessageIDs: []string{"m-database", "m-region", "m-date", "m-rollback", "m-suggestion"},
			Content:    "SQLite eu-west June 4 rollback", OccurredAt: spans[0].OccurredAt},
		EpisodeSpans: spans, AtomTargetTokens: 24, AtomMaxTokens: 48,
	}); err != nil {
		t.Fatal(err)
	}
	expansion := longmemory.QueryExpansion{Queries: []string{
		"selected database SQLite", "production deployment region eu-west", "release date June 4", "rollback prior binary",
	}}
	result, err := store.SearchAllChannels(ctx, longmemory.MemoryQuery{Text: "Summarize the release constraints",
		Timestamp: time.Now().UTC(), Scopes: []longmemory.Scope{scope}}, expansion, nil, longmemory.HybridSearchOptions{
		FTSCandidates: 40, GraphCandidates: 20, GraphMaxHops: 2, RRFK: 60, SessionRetrieval: true,
		SessionCandidates: 12, SessionChunkCandidates: 64, ChunksPerSession: 12,
		AtomMaxSelected: 32, MaxItems: 32, CoverageMaxFacets: 8, CoverageCompletionRounds: 1,
		CoverageRelevanceWeight: .45, CoverageFacetWeight: .25, CoverageProvenanceWeight: .15,
		CoverageSourceWeight: .10, CoverageCoherenceWeight: .05, CoreContextTokens: 0,
		TargetContextTokens: 1200, MaxContextTokens: 1800, EvidencePrimaryBudgetRatio: .7,
		EvidenceCompletionBudgetRatio: .2, EvidenceContextBudgetRatio: .1,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, evidence := range result.Packet.Evidence {
		joined += "\n" + evidence.Text
	}
	for _, expected := range []string{"SQLite", "eu-west", "June 4", "prior binary"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("missing complementary evidence %q in packet:\n%s", expected, joined)
		}
	}
	if len(result.Run.ChannelResults) != 6 {
		t.Fatalf("all six channels must execute, got %d", len(result.Run.ChannelResults))
	}
	if len(result.Run.CoverageLedger.Uncovered) != 0 {
		t.Fatalf("coverage ledger left supported facets uncovered: %#v", result.Run.CoverageLedger)
	}
}

func TestSemanticEnrichmentUpdatesAtomProvenanceBySourceMessage(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, root+"/memory.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{
		Episode: &longmemory.Episode{ScopeType: scope.Type, ScopeKey: scope.Key, SessionID: "provenance-session",
			MessageIDs: []string{"m-observation"}, Content: "build verified", OccurredAt: time.Now().UTC()},
		EpisodeSpans: []longmemory.EvidenceSpan{{MessageID: "m-observation", Role: "assistant",
			Text: "The build completed successfully after running the test suite.", OccurredAt: time.Now().UTC()}},
		Memories: []longmemory.Candidate{{ScopeType: scope.Type, ScopeKey: scope.Key, MemoryType: longmemory.TypeProject,
			Title: "Verified build", Summary: "The build passed", Content: "The build and tests passed.",
			SourceMessageIDs: []string{"m-observation"}, EpistemicStatus: "observed", Confidence: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	atoms, err := store.SearchAtomsKeyword(ctx, []string{"build completed successfully"}, []longmemory.Scope{scope}, "", 10)
	if err != nil || len(atoms) == 0 {
		t.Fatalf("search enriched atoms: %#v %v", atoms, err)
	}
	for _, atom := range atoms {
		if atom.MessageID == "m-observation" && atom.EpistemicStatus != "observed" {
			t.Fatalf("semantic provenance was not committed: %#v", atom)
		}
	}
}

type jsonExpansionClient struct{}

func (jsonExpansionClient) StreamChat(context.Context, string, []map[string]any, []map[string]any, *api.LLMRequestOptions) <-chan api.EventResult {
	out := make(chan api.EventResult, 1)
	out <- api.EventResult{Event: map[string]any{"type": "text_delta", "text": `{"queries":["release constraints"],"entities":["Project Aurora"],"provenance_hints":["user decision"],"channel":"vector"}`}}
	close(out)
	return out
}
func (jsonExpansionClient) Complete(context.Context, string, []map[string]any, api.CompleteOptions) (string, error) {
	return "", nil
}

func TestMemoryExpansionAcceptsStrictJSONContentAndDropsRoutingFields(t *testing.T) {
	cfg := config.NewConfig()
	cfg.MemoryQueryExpansionEnabled = true
	cfg.MemoryQueryExpansionTimeoutSeconds = 2
	expansion, _, expansionErr := agent.ExpandMemoryQuery(context.Background(), cfg,
		longmemory.MemoryQuery{Text: "What are the release constraints?", Timestamp: time.Now().UTC()},
		longmemory.MemoryCatalog{}, func(context.Context, string) (api.LLMClient, error) { return jsonExpansionClient{}, nil })
	if expansionErr != "" {
		t.Fatal(expansionErr)
	}
	if expansion.ParseMode != "json_content" || len(expansion.Queries) != 1 || len(expansion.ProvenanceHints) != 1 {
		t.Fatalf("JSON expansion was not retained: %#v", expansion)
	}
}

func TestEvidenceLedgerDefaultsArePublishedInExampleConfig(t *testing.T) {
	contents, err := os.ReadFile(filepath.Join("..", ".Lumina", "CONFIG", "defaults.json.example"))
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := json.Unmarshal(contents, &values); err != nil {
		t.Fatal(err)
	}
	expected := map[string]float64{
		"memory_atom_target_tokens": 96, "memory_atom_max_tokens": 160, "memory_atom_max_selected": 32,
		"memory_coverage_max_facets": 8, "memory_coverage_completion_rounds": 1,
		"memory_coverage_relevance_weight": .45, "memory_coverage_facet_weight": .25,
		"memory_coverage_provenance_weight": .15, "memory_coverage_source_weight": .10,
		"memory_coverage_coherence_weight": .05, "memory_evidence_primary_budget_ratio": .70,
		"memory_evidence_completion_budget_ratio": .20, "memory_evidence_context_budget_ratio": .10,
		"memory_embedding_batch_size": 32, "memory_embedding_batch_wait_ms": 20,
		"memory_embedding_query_cache_entries": 10000, "memory_embedding_execution_timeout_seconds": 8,
		"memory_query_expansion_timeout_seconds": 4,
	}
	for key, want := range expected {
		got, ok := values[key].(float64)
		if !ok || got != want {
			t.Fatalf("example config %s=%v, want %v", key, values[key], want)
		}
	}
}
