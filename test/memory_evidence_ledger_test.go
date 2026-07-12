package test

import (
	"context"
	"encoding/json"
	"fmt"
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

type recordingEmbedder struct {
	mu    sync.Mutex
	texts []string
}

func (*recordingEmbedder) Model() string   { return "recording-embedding" }
func (*recordingEmbedder) Dimensions() int { return 3 }
func (embedder *recordingEmbedder) Embed(_ context.Context, texts []string, _ longmemory.EmbeddingKind) ([][]float32, error) {
	embedder.mu.Lock()
	embedder.texts = append(embedder.texts, texts...)
	embedder.mu.Unlock()
	result := make([][]float32, len(texts))
	for index := range result {
		result[index] = []float32{1, 0, 0}
	}
	return result, nil
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
	rebuilt := longmemory.BuildEvidenceAtoms(chunk, 12, 24)
	if len(atoms) < 4 {
		t.Fatalf("expected sentence/list atoms, got %#v", atoms)
	}
	listOrdinals := map[int]bool{}
	for index, atom := range atoms {
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
		if rebuilt[index].AtomID != atom.AtomID {
			t.Fatalf("structure rebuild changed stable atom ID: %q != %q", rebuilt[index].AtomID, atom.AtomID)
		}
		if atom.ContainerKind == "list_item" {
			listOrdinals[atom.ContainerOrdinal] = true
		}
	}
	for _, ordinal := range []int{1, 2, 3} {
		if !listOrdinals[ordinal] {
			t.Fatalf("missing list ordinal %d in %#v", ordinal, atoms)
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

func TestEmbeddingSchedulerEnforcesBatchSizeForLargePassageRequest(t *testing.T) {
	base := &batchingEmbedder{}
	embedder := longmemory.SharedEmbeddingScheduler(base, longmemory.EmbeddingSchedulerOptions{
		BatchSize: 32, BatchWait: time.Millisecond, QueryCacheEntries: 8, ExecutionTimeout: time.Second})
	texts := make([]string, 65)
	for index := range texts {
		texts[index] = "passage " + string(rune('a'+index%26))
	}
	vectors, err := embedder.Embed(context.Background(), texts, longmemory.EmbeddingPassage)
	if err != nil || len(vectors) != len(texts) {
		t.Fatalf("large passage embedding failed: vectors=%d error=%v", len(vectors), err)
	}
	base.mu.Lock()
	maxBatch := base.maxBatch
	base.mu.Unlock()
	if maxBatch > 32 {
		t.Fatalf("scheduler exceeded configured micro-batch size: %d", maxBatch)
	}
}

func TestIncrementalExpansionDoesNotRepeatOriginalQuery(t *testing.T) {
	ctx := context.Background()
	store, err := longmemory.Open(ctx, filepath.Join(t.TempDir(), "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	future := make(chan longmemory.ExpansionResult, 1)
	future <- longmemory.ExpansionResult{Expansion: longmemory.QueryExpansion{
		Queries: []string{"Aurora deployment constraints"}, ParseMode: "structured_tool"}}
	close(future)
	embedder := &recordingEmbedder{}
	result, err := store.SearchAllChannels(ctx, longmemory.MemoryQuery{Text: "Summarize Aurora", Timestamp: time.Now().UTC()},
		longmemory.QueryExpansion{}, embedder, longmemory.HybridSearchOptions{ExpansionFuture: future,
			ExpansionAdditionalWait: time.Second, TargetContextTokens: 400, MaxContextTokens: 600,
			CoreContextTokens: 1, AtomMaxSelected: 8})
	if err != nil {
		t.Fatal(err)
	}
	embedder.mu.Lock()
	texts := append([]string(nil), embedder.texts...)
	embedder.mu.Unlock()
	counts := map[string]int{}
	for _, text := range texts {
		counts[text]++
	}
	if counts["Summarize Aurora"] != 1 || counts["Aurora deployment constraints"] != 1 {
		t.Fatalf("incremental retrieval repeated original or lost delta query: %#v", texts)
	}
	if len(result.Run.ChannelResults) != 6 {
		t.Fatalf("incremental retrieval did not retain one logical result per channel: %#v", result.Run.ChannelResults)
	}
}

func TestIncrementalExpansionUsesItsTotalDeadline(t *testing.T) {
	ctx := context.Background()
	store, err := longmemory.Open(ctx, filepath.Join(t.TempDir(), "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	future := make(chan longmemory.ExpansionResult, 1)
	go func() {
		time.Sleep(80 * time.Millisecond)
		future <- longmemory.ExpansionResult{Expansion: longmemory.QueryExpansion{
			Queries: []string{"Aurora deployment constraints"}, ParseMode: "structured_tool"}, DurationMS: 80}
		close(future)
	}()
	result, err := store.SearchAllChannels(ctx, longmemory.MemoryQuery{Text: "Summarize Aurora", Timestamp: time.Now().UTC()},
		longmemory.QueryExpansion{}, &recordingEmbedder{}, longmemory.HybridSearchOptions{ExpansionFuture: future,
			ExpansionAdditionalWait: 5 * time.Millisecond, ExpansionDeadline: time.Now().Add(250 * time.Millisecond),
			TargetContextTokens: 400, MaxContextTokens: 600, CoreContextTokens: 1, AtomMaxSelected: 8})
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.QueryExpansionParseMode != "structured_tool" || result.Run.ExpansionError != "" {
		t.Fatalf("healthy expansion was discarded before its total deadline: %#v", result.Run)
	}
}

func TestQueryExpansionWithoutDeadlineWaitsForRecallQuality(t *testing.T) {
	ctx := context.Background()
	store, err := longmemory.Open(ctx, filepath.Join(t.TempDir(), "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	future := make(chan longmemory.ExpansionResult, 1)
	go func() {
		time.Sleep(80 * time.Millisecond)
		future <- longmemory.ExpansionResult{Expansion: longmemory.QueryExpansion{
			Facets: []longmemory.FacetDraft{{Text: "Aurora rollback constraint"}}, ParseMode: "structured_tool"}, DurationMS: 80}
		close(future)
	}()
	result, err := store.SearchAllChannels(ctx, longmemory.MemoryQuery{Text: "Summarize Aurora", Timestamp: time.Now().UTC()},
		longmemory.QueryExpansion{}, &recordingEmbedder{}, longmemory.HybridSearchOptions{ExpansionFuture: future,
			ExpansionAdditionalWait: time.Millisecond, TargetContextTokens: 400, MaxContextTokens: 600,
			CoreContextTokens: 1, AtomMaxSelected: 8})
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.QueryExpansionParseMode != "structured_tool" || result.Run.ExpansionError != "" {
		t.Fatalf("unlimited expansion was discarded by an incremental wait: %#v", result.Run)
	}
}

func TestEvidenceAtomsAreBuiltOnceAcrossOverlappingChunks(t *testing.T) {
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: "atom-overlap"}
	text := strings.Repeat("Background context about the vehicle and its maintenance history. ", 14) +
		"I recently had an issue with my car's GPS system on 3/22, and I took it back to the dealership. " +
		"They replaced the entire system, and now it works flawlessly."
	span := longmemory.EvidenceSpan{SpanID: "span", MemoryID: "session-index", ScopeType: scope.Type,
		ScopeKey: scope.Key, SessionID: "session", MessageID: "message", Role: "user", Text: text,
		OccurredAt: time.Now().UTC()}
	chunks := longmemory.BuildEvidenceChunks(span)
	if len(chunks) < 2 {
		t.Fatalf("fixture did not produce overlapping chunks: %d", len(chunks))
	}
	atoms := longmemory.BuildEvidenceAtomsForSpan(span, chunks, 96, 160)
	if len(atoms) == 0 {
		t.Fatal("no atoms generated")
	}
	for index, atom := range atoms {
		if atom.ChunkID == "" {
			t.Fatalf("atom %d was not assigned to a parent chunk", index)
		}
		if index > 0 && atom.StartRune < atoms[index-1].EndRune {
			t.Fatalf("atoms overlap: %#v then %#v", atoms[index-1], atom)
		}
		if strings.TrimSpace(atom.Text) == "5." {
			t.Fatalf("list ordinal became a standalone atom: %#v", atom)
		}
	}
	joined := ""
	for _, atom := range atoms {
		joined += " " + atom.Text
	}
	for _, expected := range []string{"GPS system on 3/22", "replaced the entire system", "works flawlessly"} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("atomized message lost %q: %s", expected, joined)
		}
	}
}

func TestEvidenceAtomsSeparateReportedFactsFromTrailingQuestion(t *testing.T) {
	span := longmemory.EvidenceSpan{SpanID: "span", ScopeType: longmemory.ScopeProject, ScopeKey: "project",
		SessionID: "session", MessageID: "message", Role: "user",
		Text: "The deployment failed after the health check. We rolled back to the prior release. Have you seen this failure before?"}
	atoms := longmemory.BuildEvidenceAtomsForSpan(span, longmemory.BuildEvidenceChunks(span), 96, 160)
	if len(atoms) != 2 {
		t.Fatalf("expected declarative evidence and question to be split, got %#v", atoms)
	}
	if atoms[0].EpistemicStatus != "reported" || atoms[1].EpistemicStatus != "questioned" {
		t.Fatalf("mixed speech acts were misclassified: %#v", atoms)
	}
	if !strings.Contains(atoms[0].Text, "rolled back") || !strings.HasPrefix(atoms[1].Text, "Have you") {
		t.Fatalf("unexpected speech-act boundaries: %#v", atoms)
	}
}

func TestQueryRewritesDoNotBecomeIndependentCoverageFacets(t *testing.T) {
	plan := longmemory.QueryPlan{Query: "Summarize the release constraints"}
	expansion := longmemory.QueryExpansion{Queries: []string{"release requirements", "deployment constraints"},
		Facets: []longmemory.FacetDraft{{Text: "database choice"}, {Text: "deployment region"}}}
	facets := longmemory.BuildCoverageFacets(plan, expansion, 8)
	if len(facets) != 3 || facets[0].Required {
		t.Fatalf("composite query was not retained as an optional root facet: %#v", facets)
	}
	for _, facet := range facets {
		if facet.Text == "release requirements" || facet.Text == "deployment constraints" {
			t.Fatalf("query rewrite leaked into coverage ledger: %#v", facets)
		}
	}
	if !facets[1].Required || !facets[2].Required {
		t.Fatalf("leaf facets were not marked as coverage obligations: %#v", facets)
	}
}

func TestCoverageFacetsFallBackToOriginalQueryWithoutDrafts(t *testing.T) {
	plan := longmemory.QueryPlan{Query: "Summarize the release constraints"}
	facets := longmemory.BuildCoverageFacets(plan, longmemory.QueryExpansion{Queries: []string{"release requirements"}}, 8)
	if len(facets) != 1 || facets[0].Text != plan.Query {
		t.Fatalf("missing expansion drafts did not preserve the original information need: %#v", facets)
	}
}

func TestCoverageLeafFacetsDoNotInheritGlobalAnchors(t *testing.T) {
	expansion := longmemory.QueryExpansion{Entities: []string{"Aurora", "Atlas"}, RelationTerms: []string{"compared with"},
		Facets: []longmemory.FacetDraft{
			{Text: "Aurora release date", Entities: []string{"Aurora"}, Relations: []string{"released"}},
			{Text: "Atlas deployment region", Entities: []string{"Atlas"}, Relations: []string{"deployed"}},
		}}
	facets := longmemory.BuildCoverageFacets(longmemory.QueryPlan{Query: "Compare Aurora and Atlas"}, expansion, 8)
	if len(facets) != 3 || facets[0].Required {
		t.Fatalf("unexpected facet hierarchy: %#v", facets)
	}
	if strings.Join(facets[1].Entities, ",") != "Aurora" || strings.Join(facets[1].Relations, ",") != "released" ||
		strings.Join(facets[2].Entities, ",") != "Atlas" || strings.Join(facets[2].Relations, ",") != "deployed" {
		t.Fatalf("global anchors contaminated independent leaf facets: %#v", facets)
	}
}

func TestCoverageFacetsIgnoreUnparsedTemporalHints(t *testing.T) {
	expansion := longmemory.QueryExpansion{TemporalConstraints: []longmemory.TemporalConstraint{{FromText: "first service"}},
		Facets: []longmemory.FacetDraft{{Text: "issue after service",
			TemporalConstraints: []longmemory.TemporalConstraint{{AtText: "the prior deployment"}}}}}
	facets := longmemory.BuildCoverageFacets(longmemory.QueryPlan{Query: "What happened after service?"}, expansion, 8)
	for _, facet := range facets {
		if len(facet.TemporalHints) != 0 {
			t.Fatalf("unparsed natural-language time became universal temporal coherence: %#v", facets)
		}
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
	}, Facets: []longmemory.FacetDraft{
		{Text: "selected database SQLite"}, {Text: "production deployment region eu-west"},
		{Text: "release date June 4"}, {Text: "rollback prior binary"},
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
	if len(result.Run.CoverageLedger.Selected) < 4 {
		t.Fatalf("coverage ledger stopped before complementary constraints were selected: %#v", result.Run.CoverageLedger)
	}
}

func TestEvidenceLedgerKeepsDirectUserAssertionOverTopicalNoise(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	commit := func(session string, at time.Time, spans []longmemory.EvidenceSpan) {
		t.Helper()
		ids := make([]string, len(spans))
		for index := range spans {
			ids[index] = spans[index].MessageID
			spans[index].OccurredAt = at.Add(time.Duration(index) * time.Second)
		}
		if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{Episode: &longmemory.Episode{
			ScopeType: scope.Type, ScopeKey: scope.Key, SessionID: session, MessageIDs: ids,
			Content: strings.Join(ids, " "), OccurredAt: at}, EpisodeSpans: spans,
			AtomTargetTokens: 96, AtomMaxTokens: 160}); err != nil {
			t.Fatal(err)
		}
	}
	commit("service", time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC), []longmemory.EvidenceSpan{
		{MessageID: "service-user", Role: "user", Text: "My new car had its first service today, and the appointment went well."},
		{MessageID: "service-assistant", Role: "assistant", Text: "Regular detailing, paint care, accessories, and fuel tracking can help maintain a new car."},
	})
	issueSpans := []longmemory.EvidenceSpan{{MessageID: "issue-user", Role: "user",
		Text: "The first problem after service was the GPS system not functioning correctly. The dealership replaced the system."}}
	for index := 0; index < 12; index++ {
		issueSpans = append(issueSpans, longmemory.EvidenceSpan{MessageID: fmt.Sprintf("issue-noise-%d", index), Role: "assistant",
			Text: fmt.Sprintf("General car service issue discussion %d: inspect paint, accessories, fuel, tires, and maintenance.", index)})
	}
	commit("issue", time.Date(2026, 3, 22, 10, 0, 0, 0, time.UTC), issueSpans)
	expansion := longmemory.QueryExpansion{Queries: []string{"first vehicle problem after service"},
		Entities: []string{"first service", "new car", "issue", "problem"}, RelationTerms: []string{"after service"},
		Facets: []longmemory.FacetDraft{{Text: "problem that occurred after the service", Entities: []string{"issue", "problem"}}}}
	result, err := store.SearchAllChannels(ctx, longmemory.MemoryQuery{Text: "What was the first problem after the car's service?",
		Timestamp: time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC), Scopes: []longmemory.Scope{scope}}, expansion, nil,
		longmemory.HybridSearchOptions{FTSCandidates: 40, GraphCandidates: 20, GraphMaxHops: 2, RRFK: 60,
			SessionRetrieval: true, SessionCandidates: 12, SessionChunkCandidates: 64, ChunksPerSession: 8,
			AtomMaxSelected: 32, CoverageMaxFacets: 8, CoverageCompletionRounds: 1,
			CoverageRelevanceWeight: .45, CoverageFacetWeight: .25, CoverageProvenanceWeight: .15,
			CoverageSourceWeight: .10, CoverageCoherenceWeight: .05, CoverageSupportTarget: .82,
			CoverageResidualTrigger: .65, CoverageMinMarginalGain: .015, TargetContextTokens: 1200,
			MaxContextTokens: 1800, EvidencePrimaryBudgetRatio: .7, EvidenceCompletionBudgetRatio: .2,
			EvidenceContextBudgetRatio: .1})
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, evidence := range result.Packet.Evidence {
		joined += "\n" + evidence.Text
	}
	if !strings.Contains(joined, "GPS system not functioning correctly") {
		t.Fatalf("direct user assertion was displaced by topical evidence:\n%s\nledger=%#v", joined, result.Run.CoverageLedger)
	}
}

func TestEvidenceLedgerIsolatesTwentyConcurrentRetrievalRuns(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	text := "Aurora uses SQLite. Production region is eu-west. Release date is June 4. Rollback keeps the prior binary."
	if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{
		Episode: &longmemory.Episode{ScopeType: scope.Type, ScopeKey: scope.Key, SessionID: "release",
			MessageIDs: []string{"release-message"}, Content: text, OccurredAt: time.Now().UTC()},
		EpisodeSpans: []longmemory.EvidenceSpan{{MessageID: "release-message", Role: "user", Text: text,
			OccurredAt: time.Now().UTC()}}, AtomTargetTokens: 12, AtomMaxTokens: 24,
	}); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	errors := make(chan error, 20)
	for index := 0; index < 20; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			result, runErr := store.SearchAllChannels(ctx, longmemory.MemoryQuery{
				Text: "Summarize Aurora release constraints", Timestamp: time.Now().UTC(), Scopes: []longmemory.Scope{scope},
				SessionID: "query-" + string(rune('a'+index)),
			}, longmemory.QueryExpansion{Facets: []longmemory.FacetDraft{
				{Text: "database"}, {Text: "production region"}, {Text: "release date"}, {Text: "rollback"},
			}}, nil, longmemory.HybridSearchOptions{FTSCandidates: 40, VectorCandidates: 40, GraphCandidates: 20,
				SessionRetrieval: true, SessionCandidates: 12, SessionChunkCandidates: 64, ChunksPerSession: 6,
				AtomMaxSelected: 32, CoverageMaxFacets: 8, CoverageCompletionRounds: 1,
				TargetContextTokens: 1200, MaxContextTokens: 1800, SuppressTrace: true})
			if runErr != nil {
				errors <- runErr
				return
			}
			if len(result.Run.ChannelResults) != 6 || result.Run.Query.SessionID != "query-"+string(rune('a'+index)) {
				errors <- fmt.Errorf("retrieval run crossed state: %#v", result.Run)
				return
			}
			joined := ""
			for _, evidence := range result.Packet.Evidence {
				joined += " " + evidence.Text
			}
			for _, expected := range []string{"SQLite", "eu-west", "June 4", "prior binary"} {
				if !strings.Contains(joined, expected) {
					errors <- fmt.Errorf("run %d missing %s in %q", index, expected, joined)
					return
				}
			}
		}(index)
	}
	wait.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
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
	if len(expansion.Diagnostics) != 1 || !strings.Contains(expansion.Diagnostics[0], "channel") {
		t.Fatalf("ignored routing field was not diagnosed: %#v", expansion.Diagnostics)
	}
}

type structuredExpansionClient struct {
	streamCalls     int
	structuredCalls int
}

func (client *structuredExpansionClient) StreamChat(context.Context, string, []map[string]any, []map[string]any, *api.LLMRequestOptions) <-chan api.EventResult {
	client.streamCalls++
	result := make(chan api.EventResult)
	close(result)
	return result
}

func (*structuredExpansionClient) Complete(context.Context, string, []map[string]any, api.CompleteOptions) (string, error) {
	return "", nil
}

func (client *structuredExpansionClient) CompleteStructured(context.Context, string, []map[string]any, api.StructuredCompletionOptions) (api.Response, error) {
	client.structuredCalls++
	return api.Response{ToolCalls: []map[string]any{{"name": "ExpandMemoryQuery", "input": map[string]any{
		"queries": []any{"Aurora release constraints"},
		"facets":  []any{map[string]any{"text": "deployment region", "entities": []any{"Aurora"}}},
	}}}}, nil
}

func TestMemoryExpansionPrefersStructuredCompletion(t *testing.T) {
	cfg := config.NewConfig()
	cfg.MemoryQueryExpansionEnabled = true
	client := &structuredExpansionClient{}
	expansion, _, expansionErr := agent.ExpandMemoryQuery(context.Background(), cfg,
		longmemory.MemoryQuery{Text: "Summarize Aurora", Timestamp: time.Now().UTC()}, longmemory.MemoryCatalog{},
		func(context.Context, string) (api.LLMClient, error) { return client, nil })
	if expansionErr != "" || expansion.ParseMode != "structured_tool" || len(expansion.Facets) != 1 {
		t.Fatalf("structured expansion failed: expansion=%#v error=%q", expansion, expansionErr)
	}
	if client.structuredCalls != 1 || client.streamCalls != 0 {
		t.Fatalf("structured path was not preferred: structured=%d stream=%d", client.structuredCalls, client.streamCalls)
	}
}

func TestCoverageLedgerDoesNotMultiplySameMessageSupport(t *testing.T) {
	facet := longmemory.CoverageFacet{FacetID: "facet", Text: "Aurora deployment region", Entities: []string{"Aurora"}}
	candidates := []longmemory.CandidateScore{
		{MemoryID: "atom-a", FusedScore: 1, Entry: longmemory.Entry{MemoryID: "atom-a", DocumentKind: "atom",
			Content: "Aurora deploys to eu-west.", SourceSessionID: "session", MessageID: "message", EpistemicStatus: "reported"},
			Contributions: []longmemory.SignalContribution{{Channel: "bm25", SignalFamily: "lexical", Rank: 1, Native: true}}},
		{MemoryID: "atom-b", FusedScore: .95, Entry: longmemory.Entry{MemoryID: "atom-b", DocumentKind: "atom",
			Content: "Aurora deploys to eu-west.", SourceSessionID: "session", MessageID: "message", EpistemicStatus: "reported"},
			Contributions: []longmemory.SignalContribution{{Channel: "vector", SignalFamily: "semantic", Rank: 1, Native: true}}},
	}
	opts := longmemory.HybridSearchOptions{AtomMaxSelected: 32, CoverageSupportTarget: .99,
		CoverageMinMarginalGain: .015, CoverageRelevanceWeight: .45, CoverageFacetWeight: .25,
		CoverageProvenanceWeight: .15, CoverageSourceWeight: .10, CoverageCoherenceWeight: .05}
	selected, ledger := longmemory.BuildCoverageLedger(candidates, []longmemory.CoverageFacet{facet}, opts, 1000)
	state := ledger.FacetStates[facet.FacetID]
	if len(state.Sources) != 1 {
		t.Fatalf("same message created multiple independent support sources: %#v", state)
	}
	if len(selected) >= opts.AtomMaxSelected {
		t.Fatalf("ledger mechanically selected the atom safety cap: %d", len(selected))
	}
}

func TestCoverageLedgerDoesNotMultiplyRepeatedAssertionAcrossMessages(t *testing.T) {
	facet := longmemory.CoverageFacet{FacetID: "facet", Text: "Aurora deployment region", Entities: []string{"Aurora"}}
	candidates := []longmemory.CandidateScore{
		{MemoryID: "first", FusedScore: 1, Entry: longmemory.Entry{MemoryID: "first", DocumentKind: "atom",
			Content: "Aurora is ready for deployment.", SourceSessionID: "s1", MessageID: "m1", EpistemicStatus: "reported"}},
		{MemoryID: "repeat", FusedScore: .9, Entry: longmemory.Entry{MemoryID: "repeat", DocumentKind: "atom",
			Content: "Aurora deployment is ready.", SourceSessionID: "s2", MessageID: "m2", EpistemicStatus: "reported"}},
	}
	selected, ledger := longmemory.BuildCoverageLedger(candidates, []longmemory.CoverageFacet{facet},
		longmemory.HybridSearchOptions{AtomMaxSelected: 32, CoverageSupportTarget: .99,
			CoverageRelevanceWeight: .45, CoverageFacetWeight: .25, CoverageProvenanceWeight: .15,
			CoverageSourceWeight: .10, CoverageCoherenceWeight: .05}, 1000)
	state := ledger.FacetStates[facet.FacetID]
	if len(state.Sources) != 1 || len(selected) != 1 {
		t.Fatalf("repeated assertion accumulated as independent evidence: selected=%#v state=%#v", selected, state)
	}
}

func TestCoverageLedgerNormalizesOnlyObservableAnchors(t *testing.T) {
	facet := longmemory.CoverageFacet{FacetID: "facet", Text: "Aurora deployment", Entities: []string{"Aurora", "unseen-system"}}
	candidate := longmemory.CandidateScore{MemoryID: "direct", FusedScore: 1, Entry: longmemory.Entry{MemoryID: "direct",
		DocumentKind: "atom", Content: "Aurora is deployed.", SourceSessionID: "s1", MessageID: "m1", EpistemicStatus: "observed"}}
	_, ledger := longmemory.BuildCoverageLedger([]longmemory.CandidateScore{candidate}, []longmemory.CoverageFacet{facet},
		longmemory.HybridSearchOptions{AtomMaxSelected: 32, CoverageSupportTarget: .99}, 1000)
	state := ledger.FacetStates[facet.FacetID]
	if len(state.ObservableAnchors) != 1 || state.ObservableAnchors[0] != "aurora" {
		t.Fatalf("unobservable anchors distorted the coverage denominator: %#v", state)
	}
}

func TestCoverageLedgerStopsAfterDirectAssertionInsteadOfFillingWithTopicNoise(t *testing.T) {
	facet := longmemory.CoverageFacet{FacetID: "booking", Text: "Airbnb booking date",
		Entities: []string{"Airbnb"}, Relations: []string{"booked"}}
	candidates := []longmemory.CandidateScore{{MemoryID: "booking", FusedScore: 1,
		Entry: longmemory.Entry{MemoryID: "booking", DocumentKind: "atom",
			Content: "I booked the Airbnb 3 months in advance.", SourceSessionID: "trip", MessageID: "booking-message",
			EpistemicStatus: "reported"}}}
	for index := 0; index < 20; index++ {
		candidates = append(candidates, longmemory.CandidateScore{MemoryID: fmt.Sprintf("noise-%d", index), FusedScore: .8,
			Entry: longmemory.Entry{MemoryID: fmt.Sprintf("noise-%d", index), DocumentKind: "atom",
				Content:         fmt.Sprintf("San Francisco restaurant recommendation number %d.", index),
				SourceSessionID: "trip", MessageID: fmt.Sprintf("noise-message-%d", index), EpistemicStatus: "suggested"}})
	}
	selected, ledger := longmemory.BuildCoverageLedger(candidates, []longmemory.CoverageFacet{facet},
		longmemory.HybridSearchOptions{AtomMaxSelected: 32, AtomTargetTokens: 96, CoverageSupportTarget: .82,
			CoverageRelevanceWeight: .45, CoverageFacetWeight: .25, CoverageProvenanceWeight: .15,
			CoverageSourceWeight: .10, CoverageCoherenceWeight: .05}, 2400)
	if len(selected) != 1 || selected[0].MemoryID != "booking" || ledger.StopReason != "support_target_reached" {
		t.Fatalf("topic noise filled the evidence budget after direct support: selected=%#v ledger=%#v", selected, ledger)
	}
}

func TestCoverageLedgerPrefersNewSessionEvidenceOverSameSessionTopicEcho(t *testing.T) {
	facets := []longmemory.CoverageFacet{
		{FacetID: "query", Text: "first issue with the new car after its first service",
			Entities: []string{"new car", "first service", "issue"}, Relations: []string{"after service"}},
		{FacetID: "issue", Text: "car issue after servicing", Entities: []string{"car", "issue"}},
	}
	contributions := []longmemory.SignalContribution{
		{Channel: "bm25", SignalFamily: "lexical", Rank: 1, Native: true},
		{Channel: "vector", SignalFamily: "semantic", Rank: 2, Native: true},
	}
	candidates := []longmemory.CandidateScore{
		{MemoryID: "service", FusedScore: 1, Entry: longmemory.Entry{MemoryID: "service", DocumentKind: "atom",
			Content: "My new car had its first service today.", SourceSessionID: "service-session", MessageID: "service-message",
			EpistemicStatus: "reported"}, Contributions: contributions},
		{MemoryID: "accessories", FusedScore: .97, Entry: longmemory.Entry{MemoryID: "accessories", DocumentKind: "atom",
			Content: "I like the accessories in my new car.", SourceSessionID: "service-session", MessageID: "accessory-message",
			EpistemicStatus: "reported"}, Contributions: contributions},
		{MemoryID: "gps", FusedScore: .96, Entry: longmemory.Entry{MemoryID: "gps", DocumentKind: "atom",
			Content: "The first issue was the GPS system not functioning correctly.", SourceSessionID: "issue-session",
			MessageID: "issue-message", EpistemicStatus: "reported"}, Contributions: contributions},
	}
	opts := longmemory.HybridSearchOptions{AtomMaxSelected: 32, AtomTargetTokens: 96,
		CoverageSupportTarget: .82, CoverageMinMarginalGain: .015, CoverageRelevanceWeight: .45,
		CoverageFacetWeight: .25, CoverageProvenanceWeight: .15, CoverageSourceWeight: .10,
		CoverageCoherenceWeight: .05}
	selected, _ := longmemory.BuildCoverageLedger(candidates, facets, opts, 1000)
	positions := map[string]int{}
	for index, candidate := range selected {
		positions[candidate.MemoryID] = index + 1
	}
	if positions["gps"] == 0 {
		t.Fatalf("independent issue Session was not selected: %#v", selected)
	}
	if positions["accessories"] > 0 && positions["accessories"] < positions["gps"] {
		t.Fatalf("same-Session topical echo displaced independent evidence: %#v", selected)
	}
}

func TestCoverageLedgerDoesNotTreatTopicalEchoAsDirectFacetSupport(t *testing.T) {
	facets := []longmemory.CoverageFacet{
		{FacetID: "deployment", Text: "deployment rollback constraint", Relations: []string{"rollback constraint"}},
		{FacetID: "verification", Text: "post-deployment verification result", Relations: []string{"verification result"}},
	}
	candidates := []longmemory.CandidateScore{
		{MemoryID: "topic-1", FusedScore: 1, Entry: longmemory.Entry{MemoryID: "topic-1", DocumentKind: "atom",
			Content: "deployment discussion and general project status", SourceSessionID: "s1", MessageID: "m1", EpistemicStatus: "reported"}},
		{MemoryID: "topic-2", FusedScore: .95, Entry: longmemory.Entry{MemoryID: "topic-2", DocumentKind: "atom",
			Content: "deployment planning and release notes", SourceSessionID: "s2", MessageID: "m2", EpistemicStatus: "reported"}},
		{MemoryID: "direct", FusedScore: .65, Entry: longmemory.Entry{MemoryID: "direct", DocumentKind: "atom",
			Content: "Post-deployment verification result: all health checks passed.", SourceSessionID: "s3", MessageID: "m3", EpistemicStatus: "reported"}},
	}
	opts := longmemory.HybridSearchOptions{AtomMaxSelected: 32, AtomTargetTokens: 96,
		CoverageSupportTarget: .82, CoverageMinMarginalGain: .015, CoverageRelevanceWeight: .45,
		CoverageFacetWeight: .25, CoverageProvenanceWeight: .15, CoverageSourceWeight: .10,
		CoverageCoherenceWeight: .05}
	selected, ledger := longmemory.BuildCoverageLedger(candidates, facets, opts, 2400)
	found := false
	for _, candidate := range selected {
		found = found || candidate.MemoryID == "direct"
	}
	if !found {
		t.Fatalf("direct evidence was displaced by topical echoes: selected=%+v ledger=%+v", selected, ledger)
	}
}

func TestCoverageLedgerRetainsStrongerProvenanceForSameAnchor(t *testing.T) {
	facet := longmemory.CoverageFacet{FacetID: "constraint", Text: "deployment rollback constraint",
		Relations: []string{"rollback constraint"}}
	candidates := []longmemory.CandidateScore{
		{MemoryID: "suggestion", FusedScore: 1, Entry: longmemory.Entry{MemoryID: "suggestion", DocumentKind: "atom",
			Content: "A rollback constraint might require keeping two releases.", SourceSessionID: "s1",
			MessageID: "m1", EpistemicStatus: "suggested"}},
		{MemoryID: "observation", FusedScore: .4, Entry: longmemory.Entry{MemoryID: "observation", DocumentKind: "atom",
			Content: "Observed rollback constraint: keep three releases available.", SourceSessionID: "s1",
			MessageID: "m2", EpistemicStatus: "observed"}},
	}
	opts := longmemory.HybridSearchOptions{AtomMaxSelected: 32, AtomTargetTokens: 96,
		CoverageSupportTarget: .82, CoverageMinMarginalGain: .015, CoverageRelevanceWeight: .45,
		CoverageFacetWeight: .25, CoverageProvenanceWeight: .15, CoverageSourceWeight: .10,
		CoverageCoherenceWeight: .05}
	selected, _ := longmemory.BuildCoverageLedger(candidates, []longmemory.CoverageFacet{facet}, opts, 1000)
	found := false
	for _, candidate := range selected {
		found = found || candidate.MemoryID == "observation"
	}
	if !found {
		t.Fatalf("stronger provenance did not improve existing anchor support: %#v", selected)
	}
}

func TestCoverageLedgerSkipsHighUtilityCandidateWithExhaustedMarginalGain(t *testing.T) {
	facets := []longmemory.CoverageFacet{
		{FacetID: "service", Text: "first service", Relations: []string{"service"}},
		{FacetID: "issue", Text: "issue after service", Relations: []string{"issue", "problem"}},
	}
	candidates := []longmemory.CandidateScore{
		{MemoryID: "service", FusedScore: 1, Entry: longmemory.Entry{MemoryID: "service", Content: "The first service went well.",
			DocumentKind: "atom", SourceSessionID: "s1", MessageID: "m1", EpistemicStatus: "reported"}},
		{MemoryID: "service-echo", FusedScore: .99, Entry: longmemory.Entry{MemoryID: "service-echo", Content: "The service appointment was routine.",
			DocumentKind: "atom", SourceSessionID: "s1", MessageID: "m2", EpistemicStatus: "reported"}},
		{MemoryID: "issue", FusedScore: .55, Entry: longmemory.Entry{MemoryID: "issue", Content: "The issue after service was a failed health check.",
			DocumentKind: "atom", SourceSessionID: "s2", MessageID: "m3", EpistemicStatus: "observed"}},
	}
	opts := longmemory.HybridSearchOptions{AtomMaxSelected: 32, AtomTargetTokens: 96,
		CoverageSupportTarget: .95, CoverageMinMarginalGain: .05, CoverageRelevanceWeight: .45,
		CoverageFacetWeight: .25, CoverageProvenanceWeight: .15, CoverageSourceWeight: .10,
		CoverageCoherenceWeight: .05}
	selected, _ := longmemory.BuildCoverageLedger(candidates, facets, opts, 1000)
	found := false
	for _, candidate := range selected {
		found = found || candidate.MemoryID == "issue"
	}
	if !found {
		t.Fatalf("low-gain topical echo stopped selection before uncovered evidence: %#v", selected)
	}
}

func TestCoverageLedgerDoesNotUseDeprecatedMarginalGainGate(t *testing.T) {
	facet := longmemory.CoverageFacet{FacetID: "deployment", Text: "Aurora deployment region", Entities: []string{"Aurora"}}
	candidates := []longmemory.CandidateScore{
		{MemoryID: "first", FusedScore: 1, Entry: longmemory.Entry{MemoryID: "first", Content: "Aurora deployment phase 1 is being prepared.",
			DocumentKind: "atom", SourceSessionID: "s1", MessageID: "m1", EpistemicStatus: "reported"}},
		{MemoryID: "second", FusedScore: .5, Entry: longmemory.Entry{MemoryID: "second", Content: "Aurora deployment phase 2 uses eu-west.",
			DocumentKind: "atom", SourceSessionID: "s2", MessageID: "m2", EpistemicStatus: "observed"}},
	}
	opts := longmemory.HybridSearchOptions{AtomMaxSelected: 32, AtomTargetTokens: 96,
		CoverageSupportTarget: .999, CoverageMinMarginalGain: .99, CoverageRelevanceWeight: .45,
		CoverageFacetWeight: .25, CoverageProvenanceWeight: .15, CoverageSourceWeight: .10,
		CoverageCoherenceWeight: .05}
	selected, ledger := longmemory.BuildCoverageLedger(candidates, []longmemory.CoverageFacet{facet}, opts, 1000)
	if len(selected) != 2 {
		t.Fatalf("deprecated marginal gate still discarded independent evidence: %#v", selected)
	}
	if ledger.StopReason != "candidate_exhausted" {
		t.Fatalf("unexpected stop reason after selecting all candidates: %q", ledger.StopReason)
	}
}

func TestCoverageLedgerReportsTokenBudgetPrecisely(t *testing.T) {
	facet := longmemory.CoverageFacet{FacetID: "deployment", Text: "Aurora deployment"}
	candidates := []longmemory.CandidateScore{{MemoryID: "large", FusedScore: 1,
		Entry: longmemory.Entry{MemoryID: "large", Content: strings.Repeat("deployment evidence ", 20),
			DocumentKind: "atom", SourceSessionID: "s1", MessageID: "m1", EpistemicStatus: "reported"}}}
	selected, ledger := longmemory.BuildCoverageLedger(candidates, []longmemory.CoverageFacet{facet},
		longmemory.HybridSearchOptions{AtomMaxSelected: 32, CoverageSupportTarget: .82}, 1)
	if len(selected) != 0 || ledger.StopReason != "token_budget_reached" {
		t.Fatalf("token budget was not diagnosed precisely: selected=%#v ledger=%#v", selected, ledger)
	}
}

func TestResidualSweepRunsForAnyFacetBelowSupportTarget(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(root)}
	span := longmemory.EvidenceSpan{MessageID: "deployment", Role: "user", Text: "Aurora deploys in eu-west.",
		OccurredAt: time.Now().UTC()}
	if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{Episode: &longmemory.Episode{
		ScopeType: scope.Type, ScopeKey: scope.Key, SessionID: "release", MessageIDs: []string{span.MessageID},
		Content: span.Text, OccurredAt: span.OccurredAt}, EpisodeSpans: []longmemory.EvidenceSpan{span},
		AtomTargetTokens: 96, AtomMaxTokens: 160}); err != nil {
		t.Fatal(err)
	}
	result, err := store.SearchAllChannels(ctx, longmemory.MemoryQuery{Text: "Where does Aurora deploy?",
		Timestamp: time.Now().UTC(), Scopes: []longmemory.Scope{scope}}, longmemory.QueryExpansion{
		Facets: []longmemory.FacetDraft{{Text: "Aurora deployment region", Entities: []string{"Aurora"}}}}, nil,
		longmemory.HybridSearchOptions{FTSCandidates: 40, GraphCandidates: 20, GraphMaxHops: 2, RRFK: 60,
			SessionRetrieval: true, SessionCandidates: 12, SessionChunkCandidates: 64, ChunksPerSession: 6,
			AtomMaxSelected: 32, CoverageMaxFacets: 8, CoverageCompletionRounds: 1,
			CoverageSupportTarget: .999, CoverageResidualTrigger: .01, TargetContextTokens: 1200,
			MaxContextTokens: 1800, EvidencePrimaryBudgetRatio: .7, EvidenceCompletionBudgetRatio: .2,
			EvidenceContextBudgetRatio: .1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Run.ResidualSweepCandidates == 0 {
		t.Fatalf("an unmet support target did not trigger residual retrieval: %#v", result.Run.CoverageLedger)
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
		"memory_coverage_support_target": .82, "memory_coverage_residual_trigger": .82,
		"memory_coverage_min_marginal_gain": 0,
		"memory_coverage_relevance_weight":  .45, "memory_coverage_facet_weight": .25,
		"memory_coverage_provenance_weight": .15, "memory_coverage_source_weight": .10,
		"memory_coverage_coherence_weight": .05, "memory_evidence_primary_budget_ratio": .70,
		"memory_evidence_completion_budget_ratio": .20, "memory_evidence_context_budget_ratio": .10,
		"memory_embedding_batch_size": 32, "memory_embedding_batch_wait_ms": 20,
		"memory_embedding_query_cache_entries": 10000, "memory_embedding_execution_timeout_seconds": 8,
		"memory_query_expansion_timeout_seconds": 0, "memory_query_expansion_max_additional_wait_ms": 750,
		"memory_atom_structural_context_max_tokens": 384,
	}
	for key, want := range expected {
		got, ok := values[key].(float64)
		if !ok || got != want {
			t.Fatalf("example config %s=%v, want %v", key, values[key], want)
		}
	}
	if enabled, ok := values["memory_atom_structural_context_enabled"].(bool); !ok || !enabled {
		t.Fatalf("example config memory_atom_structural_context_enabled=%v, want true", values["memory_atom_structural_context_enabled"])
	}
}
