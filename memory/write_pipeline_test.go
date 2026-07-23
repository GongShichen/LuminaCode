package memory

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/viant/sqlite-vec/vector"
)

type boundedCompiler struct {
	mu            sync.Mutex
	calls         int
	active        int
	maxConcurrent int
	requests      []CompileRequest
	delay         time.Duration
}

func (c *boundedCompiler) EstimateInputTokens(request CompileRequest) (int, error) {
	return 3_000 + len(request.Sources)*1_000, nil
}

func (c *boundedCompiler) Compile(ctx context.Context, request CompileRequest) (CompileResponse, error) {
	c.mu.Lock()
	c.calls++
	c.active++
	if c.active > c.maxConcurrent {
		c.maxConcurrent = c.active
	}
	c.requests = append(c.requests, request)
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		c.mu.Lock()
		c.active--
		c.mu.Unlock()
		return CompileResponse{}, ctx.Err()
	case <-time.After(c.delay):
	}
	c.mu.Lock()
	c.active--
	c.mu.Unlock()
	return CompileResponse{Usage: APIUsage{Calls: 1, InputTokens: 100, OutputTokens: 4}}, nil
}

func TestFabricFlushSplitsInputBeforeAPIAndBoundsConcurrency(t *testing.T) {
	compiler := &boundedCompiler{delay: 30 * time.Millisecond}
	options := DefaultFabricOptions(t.TempDir())
	options.Compiler = compiler
	options.StartWorkers = false
	options.CompileBatchTokens = 5_500
	options.WorkerCount = 3
	options.CompileConcurrency = 2
	fabric, err := OpenFabric(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer fabric.Close()

	start := time.Now().UTC()
	events := make([]RawEvent, 8)
	refs := make([]ContextRef, 0, len(events))
	for index := range events {
		events[index] = testEvent(fmt.Sprintf("budget-event-%d", index),
			fmt.Sprintf("The user prefers durable workspace setting number %d.", index), start.Add(time.Duration(index)*time.Second))
		events[index].SessionID = fmt.Sprintf("session-%d", index)
		events[index].ContextID = fmt.Sprintf("context-%d", index)
		refs = append(refs, ContextRef{ID: events[index].ContextID, Space: "test", ClosedAt: events[index].OccurredAt})
	}
	if _, err := fabric.AppendEvents(context.Background(), events,
		IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.SealImport(context.Background(), refs, ImportPlanningOptions{
		MaxCompilerCalls: 4, MaxSources: 32, MaxSourcesPerSession: 2, MaxSourceRunes: 900,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	var compileJobs int
	if err := fabric.ledger.QueryRow(`SELECT COUNT(*) FROM jobs WHERE job_kind=?`, jobCompileEvents).Scan(&compileJobs); err != nil {
		t.Fatal(err)
	}
	if compileJobs != 4 {
		t.Fatalf("compile job count = %d, want final four jobs with no recursive parents", compileJobs)
	}

	compiler.mu.Lock()
	defer compiler.mu.Unlock()
	if compiler.calls != 4 {
		t.Fatalf("compiler calls = %d, want four bounded two-event batches", compiler.calls)
	}
	if compiler.maxConcurrent != 2 {
		t.Fatalf("compiler concurrency = %d, want 2", compiler.maxConcurrent)
	}
	for _, request := range compiler.requests {
		estimated, _ := compiler.EstimateInputTokens(request)
		if estimated > request.MaxInputTokens {
			t.Fatalf("oversized request reached compiler: estimated=%d request=%+v", estimated, request)
		}
		if request.MaxOutputTokens > options.CompileOutputTokens || request.MaxNodes > options.CompileMaxNodes {
			t.Fatalf("compiler output was not bounded: %+v", request)
		}
	}
}

func TestFabricImportCapsSourcesPerCompilerCall(t *testing.T) {
	compiler := &boundedCompiler{delay: time.Millisecond}
	options := DefaultFabricOptions(t.TempDir())
	options.Compiler = compiler
	options.StartWorkers = false
	options.CompileBatchTokens = 100_000
	options.CompileSourcesPerCall = 8
	options.WorkerCount = 3
	options.CompileConcurrency = 2
	fabric, err := OpenFabric(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer fabric.Close()

	start := time.Now().UTC()
	events := make([]RawEvent, 20)
	refs := make([]ContextRef, 20)
	for index := range events {
		events[index] = testEvent(fmt.Sprintf("source-cap-%d", index),
			fmt.Sprintf("I prefer durable workspace profile %d.", index), start.Add(time.Duration(index)*time.Second))
		events[index].SessionID = fmt.Sprintf("session-%d", index)
		events[index].ContextID = fmt.Sprintf("context-%d", index)
		refs[index] = ContextRef{ID: events[index].ContextID, Space: "test", ClosedAt: events[index].OccurredAt}
	}
	if _, err := fabric.AppendEvents(context.Background(), events,
		IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.SealImport(context.Background(), refs, ImportPlanningOptions{
		MaxCompilerCalls: 4, MaxSources: 32, MaxSourcesPerSession: 2, MaxSourceRunes: 900,
	}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	compiler.mu.Lock()
	defer compiler.mu.Unlock()
	if compiler.calls != 3 {
		t.Fatalf("compiler calls = %d, want batches 8+8+4", compiler.calls)
	}
	for _, request := range compiler.requests {
		if len(request.Sources) > 8 {
			t.Fatalf("compiler received %d sources, maximum is 8", len(request.Sources))
		}
	}
}

func TestFabricPrunesVectorsWithoutCurrentDocuments(t *testing.T) {
	options := DefaultFabricOptions(t.TempDir())
	options.StartWorkers = false
	fabric, err := OpenFabric(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer fabric.Close()
	embedding, err := vector.EncodeEmbedding([]float32{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.index.Exec(`INSERT INTO _vec_memory_vectors(dataset_id,id,content,meta,embedding)
		VALUES (?,?,?,?,?)`, vectorDataset("test", "content"), "orphan", "stale", `{}`, embedding); err != nil {
		t.Fatal(err)
	}
	if err := fabric.pruneOrphanVectors(context.Background()); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := fabric.index.QueryRow(`SELECT COUNT(*) FROM _vec_memory_vectors WHERE id='orphan'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("orphan vector count=%d, want zero", count)
	}
}

type contractFailureCompiler struct{}

func (contractFailureCompiler) Compile(context.Context, CompileRequest) (CompileResponse, error) {
	return CompileResponse{}, &CompileContractError{Reason: "truncated output"}
}

type emptyBudgetCompiler struct{}

func (emptyBudgetCompiler) Compile(context.Context, CompileRequest) (CompileResponse, error) {
	return CompileResponse{}, &CompileContractError{Reason: "model exhausted the output budget without a semantic result"}
}

func TestBindCompilerSourcesDropsOnlyUnknownCandidates(t *testing.T) {
	nodes := []MemoryDraft{
		{Statement: "valid", Sources: []SourceSpan{{SourceRef: "source-a"}}},
		{Statement: "invalid", Sources: []SourceSpan{{SourceRef: "source-missing"}}},
	}
	aliases := []IdentityAliasProposal{
		{Canonical: "valid", Sources: []SourceSpan{{SourceRef: "source-b"}}},
		{Canonical: "invalid", Sources: []SourceSpan{{SourceRef: "source-missing"}}},
	}
	boundNodes, boundAliases, dropped, err := bindCompilerSources(nodes, aliases,
		[]CompileSource{{SourceRef: "source-a"}, {SourceRef: "source-b"}}, []string{"event-a", "event-b"})
	if err != nil {
		t.Fatal(err)
	}
	if dropped != 2 || len(boundNodes) != 1 || len(boundAliases) != 1 {
		t.Fatalf("unexpected binding result: nodes=%d aliases=%d dropped=%d", len(boundNodes), len(boundAliases), dropped)
	}
	if boundNodes[0].Sources[0].EventID != "event-a" || boundAliases[0].Sources[0].EventID != "event-b" {
		t.Fatalf("stable sources were not rebound: node=%+v alias=%+v", boundNodes[0], boundAliases[0])
	}
}

func TestFabricDoesNotRetryDeterministicCompilerContractFailure(t *testing.T) {
	options := DefaultFabricOptions(t.TempDir())
	options.Compiler = contractFailureCompiler{}
	options.StartWorkers = false
	fabric, err := OpenFabric(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer fabric.Close()

	event := testEvent("contract-failure", "I prefer the quartz workspace profile.", time.Now().UTC())
	if _, err := fabric.AppendEvents(context.Background(), []RawEvent{event},
		IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.SealContext(context.Background(), ContextRef{ID: event.ContextID, Space: event.Space}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(context.Background()); err == nil {
		t.Fatal("expected deterministic compiler contract failure")
	}
	var status string
	var attempts int
	if err := fabric.ledger.QueryRow(`SELECT status, attempts FROM jobs WHERE job_kind=?`, jobCompileEvents).
		Scan(&status, &attempts); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || attempts != 1 {
		t.Fatalf("contract failure job status=%s attempts=%d, want failed/1", status, attempts)
	}
}

func TestFabricDeferredCompileMarksEmptyBudgetResponseSkipped(t *testing.T) {
	options := DefaultFabricOptions(t.TempDir())
	options.Compiler = emptyBudgetCompiler{}
	options.StartWorkers = false
	fabric, err := OpenFabric(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer fabric.Close()

	event := testEvent("empty-budget", "I prefer a custom workspace layout.", time.Now().UTC())
	if _, err := fabric.AppendEvents(context.Background(), []RawEvent{event},
		IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.SealContext(context.Background(), ContextRef{ID: event.ContextID, Space: event.Space}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(context.Background()); err != nil {
		t.Fatalf("deferred empty compiler response stopped raw indexing: %v", err)
	}
	var jobStatus string
	var eventStatus SemanticStatus
	if err := fabric.ledger.QueryRow(`SELECT status FROM jobs WHERE job_kind=?`, jobCompileEvents).Scan(&jobStatus); err != nil {
		t.Fatal(err)
	}
	if err := fabric.ledger.QueryRow(`SELECT semantic_status FROM events WHERE event_id=?`, event.ID).Scan(&eventStatus); err != nil {
		t.Fatal(err)
	}
	if jobStatus != "complete" || eventStatus != SemanticSkipped {
		t.Fatalf("job=%s event=%s, want complete/semantic_skipped", jobStatus, eventStatus)
	}
}

func TestFabricExplicitCompileDoesNotHideEmptyBudgetResponse(t *testing.T) {
	options := DefaultFabricOptions(t.TempDir())
	options.Compiler = emptyBudgetCompiler{}
	options.StartWorkers = false
	fabric, err := OpenFabric(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer fabric.Close()

	event := testEvent("explicit-empty-budget", "Remember that the workspace profile uses a custom layout.", time.Now().UTC())
	result, err := fabric.Remember(context.Background(), MemoryRequest{Space: event.Space, ContextID: event.ContextID,
		Events: []RawEvent{event}, Mode: WriteExplicit, RequireSemantic: true})
	if err == nil || !result.Durable {
		t.Fatalf("explicit semantic failure was hidden: result=%+v err=%v", result, err)
	}
}

func TestContextPendingEventsIncludeProposedForResume(t *testing.T) {
	fabric := openTestFabric(t, nil, nil, nil)
	event := testEvent("event-proposed-resume", "Remember this stable preference.", time.Now().UTC())
	if _, err := fabric.AppendEvents(context.Background(), []RawEvent{event},
		IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.markEventSemanticStatus(context.Background(), []string{event.ID}, SemanticProposed,
		SemanticEventDurable); err != nil {
		t.Fatal(err)
	}
	ids, err := fabric.contextEventIDs(context.Background(), event.Space, event.ContextID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0] != event.ID {
		t.Fatalf("pending context events = %v, want proposed event %q", ids, event.ID)
	}
}

func TestSemanticCandidateFilterDropsOnlyBoilerplateAndExactDuplicates(t *testing.T) {
	start := time.Now().UTC()
	events := []RawEvent{
		testEvent("thanks", "Thanks!", start),
		testEvent("first", "The user prefers the quartz deployment profile.", start.Add(time.Second)),
		testEvent("latest", "The user prefers the quartz deployment profile.", start.Add(2*time.Second)),
		testEvent("assistant", "The migration completed successfully and all tests passed.", start.Add(3*time.Second)),
	}
	selected := semanticCandidateEvents(events)
	if len(selected) != 2 {
		t.Fatalf("selected events = %+v", selected)
	}
	if selected[0].ID != "latest" || selected[1].ID != "assistant" {
		t.Fatalf("candidate filter removed durable evidence: %+v", selected)
	}
}

type batchRecordingVectorizer struct {
	mu        sync.Mutex
	calls     int
	maxBatch  int
	dimension int
}

func (v *batchRecordingVectorizer) Model() string   { return "batch-local" }
func (v *batchRecordingVectorizer) Dimensions() int { return v.dimension }
func (v *batchRecordingVectorizer) Embed(_ context.Context, texts []string, _ VectorPurpose) ([][]float32, error) {
	v.mu.Lock()
	v.calls++
	if len(texts) > v.maxBatch {
		v.maxBatch = len(texts)
	}
	v.mu.Unlock()
	result := make([][]float32, len(texts))
	for index := range result {
		result[index] = make([]float32, v.dimension)
		result[index][0] = 1
	}
	return result, nil
}

func TestFabricBatchesDocumentEmbeddings(t *testing.T) {
	vectorizer := &batchRecordingVectorizer{dimension: 2}
	options := DefaultFabricOptions(t.TempDir())
	options.Vectorizer = vectorizer
	options.StartWorkers = false
	options.WorkerCount = 2
	options.EmbeddingBatchSize = 16
	fabric, err := OpenFabric(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	defer fabric.Close()

	start := time.Now().UTC()
	events := make([]RawEvent, 40)
	for index := range events {
		events[index] = testEvent(fmt.Sprintf("embed-event-%d", index),
			fmt.Sprintf("Durable local evidence %d.", index), start.Add(time.Duration(index)*time.Second))
	}
	if _, err := fabric.AppendEvents(context.Background(), events,
		IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.SealContext(context.Background(), ContextRef{ID: "ctx", Space: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	vectorizer.mu.Lock()
	calls, maxBatch := vectorizer.calls, vectorizer.maxBatch
	vectorizer.mu.Unlock()
	if calls > 4 || maxBatch < 1 {
		t.Fatalf("embeddings were not batched: calls=%d max_batch=%d", calls, maxBatch)
	}
	var rows int
	if err := fabric.index.QueryRow(`SELECT COUNT(*) FROM _vec_memory_vectors`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows < 1 || rows > 14 {
		t.Fatalf("vector rows = %d, want compact session chunks only", rows)
	}
}
