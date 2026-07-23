package memory

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

type deterministicRetrievalEncoder struct {
	failQuery          bool
	documentBatchSizes *[]int
	splitCalls         *[][2]int
}

type retrievalFailureSwitch struct {
	failDocuments         bool
	failAfterDocumentCall int
	documentCalls         int
}

type switchableRetrievalEncoder struct {
	deterministicRetrievalEncoder
	failure *retrievalFailureSwitch
}

type revisionCompatibleRetrievalEncoder struct {
	deterministicRetrievalEncoder
	revision   string
	compatible string
}

type multiSplitRetrievalEncoder struct {
	deterministicRetrievalEncoder
	singleCalls int
	multiCalls  int
}

func (e *multiSplitRetrievalEncoder) Split(text string, maxTokens, overlap int) ([]string, error) {
	e.singleCalls++
	return e.deterministicRetrievalEncoder.Split(text, maxTokens, overlap)
}

func (e *multiSplitRetrievalEncoder) SplitMany(text string, specs []RetrievalSplitSpec) ([][]string, error) {
	e.multiCalls++
	result := make([][]string, len(specs))
	for index, spec := range specs {
		split, err := e.deterministicRetrievalEncoder.Split(text, spec.MaxTokens, spec.Overlap)
		if err != nil {
			return nil, err
		}
		result[index] = split
	}
	return result, nil
}

func (e revisionCompatibleRetrievalEncoder) Revision() string {
	return e.revision
}

func (e revisionCompatibleRetrievalEncoder) CompatibleRevision(other string) bool {
	return other == e.compatible
}

func (e switchableRetrievalEncoder) Encode(ctx context.Context, texts []string,
	kind RetrievalEncodingKind) ([]RetrievalEncoding, error) {
	if kind == RetrievalDocument && e.failure != nil {
		e.failure.documentCalls++
		if e.failure.failDocuments ||
			(e.failure.failAfterDocumentCall > 0 && e.failure.documentCalls > e.failure.failAfterDocumentCall) {
			return nil, errors.New("fixture document encoding failed")
		}
	}
	return e.deterministicRetrievalEncoder.Encode(ctx, texts, kind)
}

func (deterministicRetrievalEncoder) Model() string         { return "deterministic-bge-fixture" }
func (deterministicRetrievalEncoder) Revision() string      { return "fixture-revision" }
func (deterministicRetrievalEncoder) TokenizerHash() string { return "fixture-tokenizer" }

func (e deterministicRetrievalEncoder) Encode(_ context.Context, texts []string,
	kind RetrievalEncodingKind) ([]RetrievalEncoding, error) {
	if kind == RetrievalQuery && e.failQuery {
		return nil, errors.New("fixture query encoding failed")
	}
	if kind == RetrievalDocument && e.documentBatchSizes != nil {
		*e.documentBatchSizes = append(*e.documentBatchSizes, len(texts))
	}
	result := make([]RetrievalEncoding, len(texts))
	for index, text := range texts {
		tokens := queryTokens(text, 512)
		dense := make([]float32, 8)
		sparse := map[int64]float32{}
		multi := make([]RetrievalTokenVector, 0, len(tokens))
		for position, token := range tokens {
			id := fixtureTokenID(token)
			dimension := int(uint64(id) % uint64(len(dense)))
			dense[dimension]++
			sparse[id]++
			values := make([]float32, len(dense))
			values[dimension] = 1
			multi = append(multi, RetrievalTokenVector{TokenID: id, Position: position,
				Weight: sparse[id], Values: values})
		}
		var norm float64
		for _, value := range dense {
			norm += float64(value * value)
		}
		if norm = math.Sqrt(norm); norm > 0 {
			for dimension := range dense {
				dense[dimension] /= float32(norm)
			}
		}
		result[index] = RetrievalEncoding{Dense: dense, Sparse: sparse, Multi: multi}
	}
	return result, nil
}

func TestRetrievalSidecarBatchesDocumentEncoding(t *testing.T) {
	ctx := context.Background()
	var batchSizes []int
	fabric := openRetrievalTestFabric(t, deterministicRetrievalEncoder{documentBatchSizes: &batchSizes})
	events := make([]RawEvent, 70)
	for index := range events {
		events[index] = RawEvent{ID: fmt.Sprintf("event-%03d", index), Space: "test",
			ContextID: fmt.Sprintf("context-%03d", index/2), Content: fmt.Sprintf("fact number %d", index),
			OccurredAt: time.Now().UTC().Add(time.Duration(index) * time.Second)}
	}
	if _, err := fabric.AppendEvents(ctx, events, IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.SyncRetrievalSidecar(ctx); err != nil {
		t.Fatal(err)
	}
	wantBatches := (len(events) + retrievalSidecarEncodeBatch - 1) / retrievalSidecarEncodeBatch
	if len(batchSizes) != wantBatches {
		t.Fatalf("document encoding calls = %v, want %d event-channel batches", batchSizes, wantBatches)
	}
	total := 0
	for _, size := range batchSizes {
		if size <= 0 || size > retrievalSidecarEncodeBatch {
			t.Fatalf("invalid document batch sizes: %v", batchSizes)
		}
		total += size
	}
	if total != len(events) {
		t.Fatalf("encoded event channels = %d, want %d", total, len(events))
	}
}

func TestRetrievalSidecarResumesPartialStagingCheckpoint(t *testing.T) {
	ctx := context.Background()
	failure := &retrievalFailureSwitch{failAfterDocumentCall: 1}
	fabric := openRetrievalTestFabric(t, switchableRetrievalEncoder{failure: failure})
	events := make([]RawEvent, 70)
	for index := range events {
		events[index] = RawEvent{ID: fmt.Sprintf("resume-event-%03d", index), Space: "test",
			ContextID: fmt.Sprintf("resume-context-%03d", index/2), Content: fmt.Sprintf("resume fact %d", index),
			OccurredAt: time.Now().UTC().Add(time.Duration(index) * time.Second)}
	}
	if _, err := fabric.AppendEvents(ctx, events, IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.SyncRetrievalSidecar(ctx); err == nil {
		t.Fatal("partial sidecar build unexpectedly succeeded")
	}
	stagingPath := fabric.options.RetrievalSidecarPath + ".staging"
	staging, err := openFabricDB(stagingPath, false)
	if err != nil {
		t.Fatal(err)
	}
	var checkpointed int
	if err := staging.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_state`).Scan(&checkpointed); err != nil {
		_ = staging.Close()
		t.Fatal(err)
	}
	if err := staging.Close(); err != nil {
		t.Fatal(err)
	}
	if checkpointed != retrievalSidecarEncodeBatch {
		t.Fatalf("checkpointed events = %d, want %d", checkpointed, retrievalSidecarEncodeBatch)
	}

	failure.failAfterDocumentCall = 0
	failure.documentCalls = 0
	if err := fabric.SyncRetrievalSidecar(ctx); err != nil {
		t.Fatal(err)
	}
	remaining := len(events) - retrievalSidecarEncodeBatch
	wantResumeCalls := (remaining + retrievalSidecarEncodeBatch - 1) / retrievalSidecarEncodeBatch
	if failure.documentCalls != wantResumeCalls {
		t.Fatalf("resume document calls = %d, want %d event-channel calls for the remaining %d events",
			failure.documentCalls, wantResumeCalls, remaining)
	}
	if _, err := os.Stat(stagingPath); !os.IsNotExist(err) {
		t.Fatalf("published staging checkpoint still exists: %v", err)
	}
}

func (e deterministicRetrievalEncoder) Split(text string, maxTokens, overlap int) ([]string, error) {
	if e.splitCalls != nil {
		*e.splitCalls = append(*e.splitCalls, [2]int{maxTokens, overlap})
	}
	words := strings.Fields(text)
	if len(words) <= maxTokens {
		return []string{strings.TrimSpace(text)}, nil
	}
	step := maxTokens - overlap
	var result []string
	for start := 0; start < len(words); start += step {
		end := minIntMemory(len(words), start+maxTokens)
		result = append(result, strings.Join(words[start:end], " "))
		if end == len(words) {
			break
		}
	}
	return result, nil
}

func TestRetrievalSidecarUsesBoundedEventEncodingWindows(t *testing.T) {
	ctx := context.Background()
	var splitCalls [][2]int
	fabric := openRetrievalTestFabric(t, deterministicRetrievalEncoder{splitCalls: &splitCalls})
	words := make([]string, 1200)
	for index := range words {
		words[index] = fmt.Sprintf("token-%04d", index)
	}
	if _, err := fabric.AppendEvents(ctx, []RawEvent{{
		ID: "long-event", Space: "test", ContextID: "long-context",
		Content: strings.Join(words, " "), OccurredAt: time.Now().UTC(),
	}}, IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.SyncRetrievalSidecar(ctx); err != nil {
		t.Fatal(err)
	}
	var windowCount int
	if err := fabric.sidecar.QueryRowContext(ctx,
		`SELECT window_count FROM event_vectors WHERE event_id='long-event'`).Scan(&windowCount); err != nil {
		t.Fatal(err)
	}
	if windowCount != 3 {
		t.Fatalf("event encoding windows = %d, want 3", windowCount)
	}
	want := map[[2]int]bool{{192, 32}: false, {
		retrievalSidecarEventWindowTokens, retrievalSidecarEventWindowOverlap}: false}
	for _, call := range splitCalls {
		if _, ok := want[call]; ok {
			want[call] = true
		}
	}
	for call, seen := range want {
		if !seen {
			t.Fatalf("missing split call max=%d overlap=%d in %v", call[0], call[1], splitCalls)
		}
	}
}

func TestRetrievalSidecarSplitsEachEventOnceWhenEncoderSupportsMultiplePlans(t *testing.T) {
	encoder := &multiSplitRetrievalEncoder{}
	result, err := splitRetrievalEvent(encoder,
		"one two three four five six seven eight nine ten eleven twelve")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || len(result[0]) == 0 || len(result[1]) == 0 {
		t.Fatalf("unexpected split result: %#v", result)
	}
	if encoder.multiCalls != 1 || encoder.singleCalls != 0 {
		t.Fatalf("multi/single split calls = %d/%d, want 1/0", encoder.multiCalls, encoder.singleCalls)
	}
}

func fixtureTokenID(value string) int64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(value))
	return int64(hash.Sum64() & math.MaxInt64)
}

func openRetrievalTestFabric(t *testing.T, encoder RetrievalEncoder) *Fabric {
	t.Helper()
	options := DefaultFabricOptions(t.TempDir())
	options.StartWorkers = false
	options.RemoteProcessing = RemoteProcessingOff
	options.RetrievalEncoder = encoder
	options.MaxEvidence = 4
	options.TargetContextTokens = 96
	options.MaxContextTokens = 128
	fabric, err := OpenFabric(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fabric.Close() })
	return fabric
}

func TestRetrievalEncoderRevisionCompatibility(t *testing.T) {
	encoder := revisionCompatibleRetrievalEncoder{
		revision:   "metal-revision",
		compatible: "cpu-revision",
	}
	if !retrievalEncoderRevisionCompatible(encoder, "metal-revision") {
		t.Fatal("exact revision did not match")
	}
	if !retrievalEncoderRevisionCompatible(encoder, "cpu-revision") {
		t.Fatal("declared compatible revision did not match")
	}
	if retrievalEncoderRevisionCompatible(encoder, "unknown-revision") {
		t.Fatal("unknown revision unexpectedly matched")
	}
	if retrievalEncoderRevisionCompatible(nil, "cpu-revision") {
		t.Fatal("nil encoder unexpectedly matched")
	}
}

func TestRetrievalSidecarReadyAcceptsDeclaredCompatibleRevision(t *testing.T) {
	ctx := context.Background()
	original := revisionCompatibleRetrievalEncoder{revision: "cpu-revision"}
	fabric := openRetrievalTestFabric(t, original)
	if _, err := fabric.AppendEvents(ctx, []RawEvent{{
		ID: "compatible-event", Space: "test", ContextID: "compatible-context",
		Content:    "A portable event representation remains valid across equivalent runtimes.",
		OccurredAt: time.Now().UTC(),
	}}, IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.SyncRetrievalSidecar(ctx); err != nil {
		t.Fatal(err)
	}
	events, checksum, err := fabric.loadSidecarEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("ledger events = %d, want 1", len(events))
	}

	fabric.options.RetrievalEncoder = revisionCompatibleRetrievalEncoder{
		revision:   "metal-revision",
		compatible: "cpu-revision",
	}
	if ready, reason := fabric.retrievalSidecarReady(ctx, checksum); !ready {
		t.Fatalf("compatible sidecar was not ready: %s", reason)
	}

	fabric.options.RetrievalEncoder = revisionCompatibleRetrievalEncoder{
		revision: "different-revision",
	}
	if ready, _ := fabric.retrievalSidecarReady(ctx, checksum); ready {
		t.Fatal("incompatible sidecar unexpectedly reported ready")
	}
}

func TestRetrievalSidecarHybridSearchAndIncrementalSync(t *testing.T) {
	ctx := context.Background()
	fabric := openRetrievalTestFabric(t, deterministicRetrievalEncoder{})
	now := time.Date(2026, time.July, 22, 9, 0, 0, 0, time.UTC)
	events := []RawEvent{
		{ID: "event-aurora-window", Space: "test", ContextID: "context-launch", SessionID: "launch",
			Actor: "user", Content: "The Aurora launch window opens on Tuesday.", OccurredAt: now.Add(-3 * time.Hour)},
		{ID: "event-general-plan", Space: "test", ContextID: "context-launch", SessionID: "launch",
			Actor: "assistant", Content: "The project plan includes several ordinary review steps.", OccurredAt: now.Add(-2 * time.Hour)},
		{ID: "event-aurora-calibration", Space: "test", ContextID: "context-calibration", SessionID: "calibration",
			Actor: "user", Content: "Aurora telescope calibration uses the cobalt reference card.", OccurredAt: now.Add(-time.Hour)},
		{ID: "event-future-calibration", Space: "test", ContextID: "context-future", SessionID: "future",
			Actor: "user", Content: "Aurora telescope calibration changed to the silver reference card.", OccurredAt: now.Add(time.Hour)},
	}
	if _, err := fabric.AppendEvents(ctx, events, IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if ready, _ := fabric.retrievalSidecarReady(ctx, "not-the-ledger-checksum"); ready {
		t.Fatal("new ledger content unexpectedly matched the empty sidecar")
	}
	result, err := fabric.Search(ctx, SearchRequest{Space: "test", Query: "Aurora telescope cobalt calibration",
		ReferenceTime: now, MaxEvidence: 3, MaxContextTokens: 72, IncludeDiagnostics: true})
	if err != nil {
		t.Fatal(err)
	}
	if !containsString(result.Route, "bge-m3-hybrid") || result.Diagnostics.FallbackReason != "" {
		t.Fatalf("hybrid retrieval did not run: route=%v fallback=%q", result.Route, result.Diagnostics.FallbackReason)
	}
	if result.Diagnostics.BGEFTSCandidates == 0 || result.Diagnostics.BGEDenseCandidates == 0 ||
		result.Diagnostics.BGESparseCandidates == 0 || result.Diagnostics.ExactScoredEvents == 0 ||
		result.Diagnostics.PPRSeedEvents == 0 {
		t.Fatalf("incomplete hybrid diagnostics: %+v", result.Diagnostics)
	}
	if result.Diagnostics.RetrievalModelRevision != "fixture-revision" {
		t.Fatalf("model revision = %q", result.Diagnostics.RetrievalModelRevision)
	}
	if result.Diagnostics.EvidenceTokens > 72 {
		t.Fatalf("evidence tokens = %d, budget 72", result.Diagnostics.EvidenceTokens)
	}
	if len(result.Evidence) == 0 || result.Evidence[0].ResourceID != "event-aurora-calibration" {
		t.Fatalf("top evidence = %+v", result.Evidence)
	}
	for _, evidence := range result.Evidence {
		if evidence.ResourceID == "event-future-calibration" {
			t.Fatalf("future evidence leaked through reference-time filter: %+v", evidence)
		}
	}
	var buildState string
	if err := fabric.sidecar.QueryRow(`SELECT value FROM meta WHERE key='build_state'`).Scan(&buildState); err != nil {
		t.Fatal(err)
	}
	if buildState != "ready" {
		t.Fatalf("published sidecar state = %q, want ready", buildState)
	}

	additional := RawEvent{ID: "event-aurora-optics", Space: "test", ContextID: "context-optics", SessionID: "optics",
		Actor: "assistant", Content: "Aurora optics validation uses a prism fixture.", OccurredAt: now.Add(-30 * time.Minute)}
	if _, err := fabric.AppendEvents(ctx, []RawEvent{additional}, IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.Search(ctx, SearchRequest{Space: "test", Query: "Aurora prism optics",
		ReferenceTime: now, MaxEvidence: 3, MaxContextTokens: 72}); err != nil {
		t.Fatal(err)
	}
	var indexed int
	if err := fabric.sidecar.QueryRow(`SELECT COUNT(*) FROM event_state`).Scan(&indexed); err != nil {
		t.Fatal(err)
	}
	if indexed != len(events)+1 {
		t.Fatalf("incrementally indexed events = %d, want %d", indexed, len(events)+1)
	}
}

func TestRetrievalSidecarUsesUnifiedPipelineForIdentifierLikeQueries(t *testing.T) {
	ctx := context.Background()
	fabric := openRetrievalTestFabric(t, deterministicRetrievalEncoder{})
	now := time.Now().UTC()
	event := RawEvent{ID: "event-unified-route", Space: "test", ContextID: "context-unified",
		Actor: "user", Content: "The stored record mentions SIAC_GEE, pre-1920 items, and the date 7/22.",
		OccurredAt: now.Add(-time.Minute)}
	if _, err := fabric.AppendEvents(ctx, []RawEvent{event}, IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		"Which model does SIAC_GEE use?",
		"How many pre-1920 items are stored?",
		"What happened before 7/22?",
		"task_12345",
		"/var/lib/lumina/item.json",
	} {
		result, err := fabric.Search(ctx, SearchRequest{Space: "test", Query: query,
			ReferenceTime: now, IncludeDiagnostics: true})
		if err != nil {
			t.Fatalf("query %q: %v", query, err)
		}
		if !containsString(result.Route, "bge-m3-hybrid") {
			t.Fatalf("query %q bypassed BGE-M3: route=%v", query, result.Route)
		}
	}
}

func TestRetrievalSidecarFailsClosedWhenQueryEncodingFails(t *testing.T) {
	ctx := context.Background()
	fabric := openRetrievalTestFabric(t, deterministicRetrievalEncoder{failQuery: true})
	now := time.Now().UTC()
	event := RawEvent{ID: "event-local-fallback", Space: "test", ContextID: "context-fallback",
		Actor: "user", Content: "The local fallback stores the indigo notebook location.", OccurredAt: now.Add(-time.Minute)}
	if _, err := fabric.AppendEvents(ctx, []RawEvent{event}, IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	result, err := fabric.Search(ctx, SearchRequest{Space: "test", Query: "indigo notebook location",
		ReferenceTime: now, IncludeDiagnostics: true})
	if err == nil || !strings.Contains(err.Error(), "BGE-M3 retrieval failed") {
		t.Fatalf("query encoding error = %v, result=%+v", err, result)
	}
	if len(result.Evidence) != 0 {
		t.Fatalf("failed BGE query returned fallback evidence: %+v", result.Evidence)
	}
}

func TestRetrievalSidecarFailedStagingBuildKeepsPublishedDatabase(t *testing.T) {
	ctx := context.Background()
	failure := &retrievalFailureSwitch{}
	fabric := openRetrievalTestFabric(t, switchableRetrievalEncoder{failure: failure})
	now := time.Now().UTC()
	first := RawEvent{ID: "event-published", Space: "test", ContextID: "context-published",
		Actor: "user", Content: "The published record contains the cobalt marker.", OccurredAt: now.Add(-time.Minute)}
	if _, err := fabric.AppendEvents(ctx, []RawEvent{first}, IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if _, err := fabric.Search(ctx, SearchRequest{Space: "test", Query: "cobalt marker", ReferenceTime: now}); err != nil {
		t.Fatal(err)
	}
	var beforeCount int
	if err := fabric.sidecar.QueryRow(`SELECT COUNT(*) FROM event_vectors`).Scan(&beforeCount); err != nil {
		t.Fatal(err)
	}
	failure.failDocuments = true
	second := RawEvent{ID: "event-unpublished", Space: "test", ContextID: "context-unpublished",
		Actor: "assistant", Content: "The new record contains the silver marker.", OccurredAt: now}
	if _, err := fabric.AppendEvents(ctx, []RawEvent{second}, IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	result, err := fabric.Search(ctx, SearchRequest{Space: "test", Query: "silver marker",
		ReferenceTime: now.Add(time.Second), IncludeDiagnostics: true})
	if err == nil || !strings.Contains(err.Error(), "BGE-M3 retrieval failed") {
		t.Fatalf("staging build error = %v, result=%+v", err, result)
	}
	if len(result.Evidence) != 0 {
		t.Fatalf("failed staging build returned fallback evidence: %+v", result.Evidence)
	}
	var afterFailureCount int
	if err := fabric.sidecar.QueryRow(`SELECT COUNT(*) FROM event_vectors`).Scan(&afterFailureCount); err != nil {
		t.Fatal(err)
	}
	if afterFailureCount != beforeCount {
		t.Fatalf("failed staging build changed published events: before=%d after=%d", beforeCount, afterFailureCount)
	}
	stagingPath := fabric.options.RetrievalSidecarPath + ".staging"
	if _, err := os.Stat(stagingPath); err != nil {
		t.Fatalf("failed staging checkpoint was not retained: %v", err)
	}
	failure.failDocuments = false
	if _, err := fabric.Search(ctx, SearchRequest{Space: "test", Query: "silver marker",
		ReferenceTime: now.Add(time.Second)}); err != nil {
		t.Fatal(err)
	}
	var recoveredCount int
	if err := fabric.sidecar.QueryRow(`SELECT COUNT(*) FROM event_vectors`).Scan(&recoveredCount); err != nil {
		t.Fatal(err)
	}
	if recoveredCount != 2 {
		t.Fatalf("recovered published events = %d, want 2", recoveredCount)
	}
	if _, err := os.Stat(stagingPath); !os.IsNotExist(err) {
		t.Fatalf("published staging checkpoint still exists: %v", err)
	}
}

func TestFastEventScoringSelectsRelevantSpanWithoutQueryTimeDocumentEncoding(t *testing.T) {
	ctx := context.Background()
	var documentBatchSizes []int
	fabric := openRetrievalTestFabric(t, deterministicRetrievalEncoder{documentBatchSizes: &documentBatchSizes})
	words := make([]string, 7000)
	for index := range words {
		words[index] = "background"
	}
	words[4000] = "needleword"
	now := time.Now().UTC()
	event := RawEvent{ID: "event-long", Space: "test", ContextID: "context-long", Actor: "user",
		Content: strings.Join(words, " "), OccurredAt: now.Add(-time.Minute)}
	if _, err := fabric.AppendEvents(ctx, []RawEvent{event}, IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		t.Fatal(err)
	}
	if err := fabric.SyncRetrievalSidecar(ctx); err != nil {
		t.Fatal(err)
	}
	documentCallsAfterBuild := len(documentBatchSizes)
	encoded, err := fabric.options.RetrievalEncoder.Encode(ctx, []string{"find needleword detail"}, RetrievalQuery)
	if err != nil || len(encoded) != 1 {
		t.Fatalf("encode query: count=%d err=%v", len(encoded), err)
	}
	dense, err := fabric.sidecarDense(ctx, "test", encoded[0].Dense, now, bgeChannelLimit)
	if err != nil || len(dense) != 1 {
		t.Fatalf("event dense channel: count=%d err=%v", len(dense), err)
	}
	result, err := fabric.Search(ctx, SearchRequest{Space: "test", Query: "find needleword detail",
		ReferenceTime: now, MaxEvidence: 2, MaxContextTokens: 96, IncludeDiagnostics: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Diagnostics.ExactScoredSpans == 0 || result.Diagnostics.DocumentEncodeBatches != 0 {
		t.Fatalf("exact span diagnostics = spans %d batches %d route=%v fallback=%q candidates=%d/%d/%d",
			result.Diagnostics.ExactScoredSpans, result.Diagnostics.DocumentEncodeBatches,
			result.Route, result.Diagnostics.FallbackReason, result.Diagnostics.BGEFTSCandidates,
			result.Diagnostics.BGEDenseCandidates, result.Diagnostics.BGESparseCandidates)
	}
	if len(result.Evidence) == 0 || !strings.Contains(result.Evidence[0].Content, "needleword") {
		t.Fatalf("best exact span did not reach evidence: %+v", result.Evidence)
	}
	if len(documentBatchSizes) != documentCallsAfterBuild {
		t.Fatalf("search encoded documents again: build_calls=%d after_search=%d",
			documentCallsAfterBuild, len(documentBatchSizes))
	}
}

func TestFullTokenMaxSimPreservesPosition(t *testing.T) {
	document := []RetrievalTokenVector{
		{TokenID: 1, Position: 4, Weight: .7, Values: []float32{1, 0}},
		{TokenID: 2, Position: 37, Weight: .9, Values: []float32{0, 1}},
	}
	query := []RetrievalTokenVector{
		{Values: []float32{0, 1}},
		{Values: []float32{.6, .8}},
	}
	score, position, tokenScores := multiVectorMaxSimDetails(query, document)
	if math.Abs(score-.9) > 1e-6 || position != 37 {
		t.Fatalf("MaxSim score/position = %.4f/%d", score, position)
	}
	if len(tokenScores) != 2 || tokenScores[0] < .99 || math.Abs(tokenScores[1]-.8) > 1e-6 {
		t.Fatalf("MaxSim token scores = %v", tokenScores)
	}
}

func TestSubmodularEvidenceKeepsLearnedRelevancePrimary(t *testing.T) {
	analysis := analyzeMemoryQuery("unmatched query")
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "a", ResourceKind: "event", ContextID: "ctx",
			Content: "first independent record", SourceEventIDs: []string{"a"}},
			score: .95, queryTokenScores: []float64{.9, .2}},
		{document: searchDocument{ID: "b", ResourceKind: "event", ContextID: "ctx",
			Content: "second separate note", SourceEventIDs: []string{"b"}},
			score: .94, queryTokenScores: []float64{.85, .1}},
		{document: searchDocument{ID: "c", ResourceKind: "event", ContextID: "ctx",
			Content: "third distinct fact", SourceEventIDs: []string{"c"}},
			score: .92, queryTokenScores: []float64{.1, .9}},
	}
	evidence, _, _ := selectSubmodularEvidence(candidates, analysis, 2, 64)
	var selected []string
	for _, item := range evidence {
		selected = append(selected, item.SourceEventIDs...)
	}
	if len(evidence) != 2 || evidence[0].ID != "a" || !containsString(selected, "b") {
		t.Fatalf("lower-relevance diversity displaced ranked evidence: %+v", evidence)
	}
}

func TestSubmodularEvidenceDoesNotGateSemanticParaphrasesOnLexicalCoverage(t *testing.T) {
	analysis := analyzeMemoryQuery("types of delivery services")
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "semantic", ResourceKind: "event", ContextID: "ctx-semantic",
			Content: "Uber Eats was convenient on weekends", SourceEventIDs: []string{"semantic"}},
			score: .80, coverage: 0, queryTokenScores: []float64{.8, .7}},
		{document: searchDocument{ID: "lexical-noise", ResourceKind: "event", ContextID: "ctx-noise",
			Content: "a generic report about types of delivery services", SourceEventIDs: []string{"lexical-noise"}},
			score: .68, coverage: .9, queryTokenScores: []float64{.8, .7}},
	}
	evidence, _, _ := selectSubmodularEvidence(candidates, analysis, 1, 64)
	if len(evidence) != 1 || evidence[0].ID != "semantic" {
		t.Fatalf("lexical coverage gated the stronger semantic paraphrase: %+v", evidence)
	}
}

func TestSemanticFacilityGainUsesLearnedQueryImportance(t *testing.T) {
	gain := semanticFacilityGain([]float64{.9, .9}, []float64{.8, .2}, []float64{.1, .9})
	if math.Abs(gain-.64) > 1e-9 {
		t.Fatalf("weighted semantic facility gain = %.6f, want .64", gain)
	}
	uniform := semanticFacilityGain([]float64{.9, .9}, []float64{.8, .2}, nil)
	if math.Abs(uniform-.4) > 1e-9 {
		t.Fatalf("uniform semantic facility gain = %.6f, want .4", uniform)
	}
}

func TestSubmodularEvidenceDoesNotLetTermCoverageOverrideRelevance(t *testing.T) {
	analysis := queryAnalysis{tokens: []string{"common", "rare"}, weights: map[string]float64{
		"common": 1,
		"rare":   9,
	}}
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "common", ResourceKind: "event", ContextID: "ctx",
			Content: "common detail", SourceEventIDs: []string{"common"}}, score: .80},
		{document: searchDocument{ID: "rare", ResourceKind: "event", ContextID: "ctx",
			Content: "rare detail", SourceEventIDs: []string{"rare"}}, score: .76},
	}
	evidence, _, _ := selectSubmodularEvidence(candidates, analysis, 1, 32)
	if len(evidence) != 1 || !containsString(evidence[0].SourceEventIDs, "common") {
		t.Fatalf("query-term gain overrode learned relevance: %+v", evidence)
	}
}

func TestSubmodularEvidenceBalancesContextSupportAndDiversityDeterministically(t *testing.T) {
	analysis := analyzeMemoryQuery("aurora telescope")
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "a", ResourceID: "a", ResourceKind: "event", ContextID: "ctx-1",
			Content: "aurora telescope primary detail", SourceEventIDs: []string{"a"}}, score: .95, graphComponent: "graph-1"},
		{document: searchDocument{ID: "b", ResourceID: "b", ResourceKind: "event", ContextID: "ctx-1",
			Content: "aurora telescope primary detail repeated", SourceEventIDs: []string{"b"}}, score: .94, graphComponent: "graph-1"},
		{document: searchDocument{ID: "c", ResourceID: "c", ResourceKind: "event", ContextID: "ctx-2",
			Content: "aurora telescope secondary fact", SourceEventIDs: []string{"c"}}, score: .80, graphComponent: "graph-2"},
	}
	first, _, _ := selectSubmodularEvidence(candidates, analysis, 2, 24)
	second, _, _ := selectSubmodularEvidence(candidates, analysis, 2, 24)
	if len(first) != 2 || len(second) != 2 || first[0].ID != second[0].ID || first[1].ID != second[1].ID {
		t.Fatalf("non-deterministic evidence: first=%+v second=%+v", first, second)
	}
	if first[0].ID != "a" || first[1].ID != "c" {
		t.Fatalf("near-duplicate evidence was not removed: %+v", first)
	}
	var tokens int
	for _, item := range first {
		tokens += maxIntMemory(1, estimateTokens(item.Content))
	}
	if tokens > 24 {
		t.Fatalf("selected %d tokens, budget 24", tokens)
	}
}

func TestSubmodularEvidenceKeepsComplementaryEventsFromStrongContext(t *testing.T) {
	analysis := analyzeMemoryQuery("aurora telescope calibration")
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "a", ResourceID: "a", ResourceKind: "event", ContextID: "ctx-1",
			Content: "aurora telescope optical alignment", SourceEventIDs: []string{"a"}}, score: .96},
		{document: searchDocument{ID: "b", ResourceID: "b", ResourceKind: "event", ContextID: "ctx-1",
			Content: "calibration fixture measurement result", SourceEventIDs: []string{"b"}}, score: .91},
		{document: searchDocument{ID: "c", ResourceID: "c", ResourceKind: "event", ContextID: "ctx-1",
			Content: "prism stability observation", SourceEventIDs: []string{"c"}}, score: .88},
		{document: searchDocument{ID: "d", ResourceID: "d", ResourceKind: "event", ContextID: "ctx-2",
			Content: "aurora telescope overview", SourceEventIDs: []string{"d"}}, score: .84},
		{document: searchDocument{ID: "e", ResourceID: "e", ResourceKind: "event", ContextID: "ctx-3",
			Content: "unrelated telescope catalog", SourceEventIDs: []string{"e"}}, score: .82},
	}
	evidence, _, _ := selectSubmodularEvidence(candidates, analysis, 4, 96)
	var strongContext int
	for _, item := range evidence {
		if item.ContextID == "ctx-1" {
			strongContext++
		}
	}
	if strongContext < 2 {
		t.Fatalf("strong context retained %d complementary events, want at least 2: %+v", strongContext, evidence)
	}
}

func TestSubmodularEvidenceDoesNotPenalizeComplementaryEventsByContextCount(t *testing.T) {
	analysis := analyzeMemoryQuery("aurora telescope calibration")
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "a", ResourceKind: "event", ContextID: "ctx-1",
			Content: "aurora telescope alignment", SourceEventIDs: []string{"a"}}, score: .95},
		{document: searchDocument{ID: "b", ResourceKind: "event", ContextID: "ctx-1",
			Content: "calibration fixture detail", SourceEventIDs: []string{"b"}}, score: .94},
		{document: searchDocument{ID: "c", ResourceKind: "event", ContextID: "ctx-1",
			Content: "prism stability measurement", SourceEventIDs: []string{"c"}}, score: .93},
		{document: searchDocument{ID: "d", ResourceKind: "event", ContextID: "ctx-1",
			Content: "mount vibration result", SourceEventIDs: []string{"d"}}, score: .92},
		{document: searchDocument{ID: "e", ResourceKind: "event", ContextID: "ctx-2",
			Content: "aurora telescope observation", SourceEventIDs: []string{"e"}}, score: .915},
	}
	evidence, _, _ := selectSubmodularEvidence(candidates, analysis, 4, 96)
	if len(evidence) != 4 || evidence[0].ID != "a" || evidence[1].ID != "b" ||
		evidence[2].ID != "c" || evidence[3].ID != "d" {
		t.Fatalf("context count displaced higher-relevance complementary evidence: %+v", evidence)
	}
}

func TestSubmodularEvidenceUsesNewContextOnlyAsNearTieBreaker(t *testing.T) {
	analysis := analyzeMemoryQuery("aurora telescope calibration")
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "anchor", ResourceKind: "event", ContextID: "ctx-1",
			Content: "aurora telescope alignment", SourceEventIDs: []string{"anchor"}}, score: .95},
		{document: searchDocument{ID: "companion", ResourceKind: "event", ContextID: "ctx-1",
			Content: "calibration fixture measurement", SourceEventIDs: []string{"companion"}}, score: .378},
		{document: searchDocument{ID: "novel", ResourceKind: "event", ContextID: "ctx-2",
			Content: "unrelated catalog entry", SourceEventIDs: []string{"novel"}}, score: .365},
	}
	evidence, _, _ := selectSubmodularEvidence(candidates, analysis, 2, 64)
	if len(evidence) != 2 || evidence[0].ID != "anchor" || evidence[1].ID != "companion" {
		t.Fatalf("new-context gain displaced materially stronger evidence: %+v", evidence)
	}
}

func TestSubmodularEvidenceAllowsAdditionalComplementaryEventsWithoutContextCap(t *testing.T) {
	analysis := analyzeMemoryQuery("aurora telescope calibration")
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "a", ResourceKind: "event", ContextID: "ctx-1",
			Content: "aurora optical alignment", SourceEventIDs: []string{"a"}}, score: .99},
		{document: searchDocument{ID: "b", ResourceKind: "event", ContextID: "ctx-1",
			Content: "telescope mirror measurement", SourceEventIDs: []string{"b"}}, score: .98},
		{document: searchDocument{ID: "c", ResourceKind: "event", ContextID: "ctx-1",
			Content: "calibration fixture result", SourceEventIDs: []string{"c"}}, score: .97},
		{document: searchDocument{ID: "d", ResourceKind: "event", ContextID: "ctx-1",
			Content: "prism stability observation", SourceEventIDs: []string{"d"}}, score: .96},
		{document: searchDocument{ID: "e", ResourceKind: "event", ContextID: "ctx-2",
			Content: "unrelated catalog entry", SourceEventIDs: []string{"e"}}, score: .10},
	}
	evidence, _, _ := selectSubmodularEvidence(candidates, analysis, 4, 96)
	if len(evidence) != 4 {
		t.Fatalf("evidence count = %d, want 4: %+v", len(evidence), evidence)
	}
	for _, item := range evidence {
		if item.ContextID != "ctx-1" {
			t.Fatalf("low-value context displaced complementary evidence: %+v", evidence)
		}
	}
}

func TestSubmodularEvidencePreservesAtomicEventBoundary(t *testing.T) {
	analysis := analyzeMemoryQuery("aurora telescope calibration")
	base := time.Date(2026, time.July, 23, 1, 0, 0, 0, time.UTC)
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "anchor", ResourceID: "anchor", ResourceKind: "event", ContextID: "ctx-1",
			Actor: "user", OccurredAt: base, Content: "aurora telescope alignment result",
			SourceEventIDs: []string{"source-anchor"}}, score: .99, coverage: 1},
		{document: searchDocument{ID: "companion", ResourceID: "companion", ResourceKind: "event", ContextID: "ctx-1",
			Actor: "assistant", OccurredAt: base.Add(time.Minute), Content: "calibration used the cobalt reference fixture",
			SourceEventIDs: []string{"source-companion"}}, score: .91, coverage: .5},
		{document: searchDocument{ID: "other", ResourceID: "other", ResourceKind: "event", ContextID: "ctx-2",
			Actor: "user", OccurredAt: base, Content: "aurora telescope catalog overview",
			SourceEventIDs: []string{"source-other"}}, score: .98, coverage: .5},
	}
	evidence, _, _ := selectSubmodularEvidence(candidates, analysis, 1, 80)
	if len(evidence) != 1 {
		t.Fatalf("evidence count = %d, want 1: %+v", len(evidence), evidence)
	}
	if !reflect.DeepEqual(evidence[0].SourceEventIDs, []string{"source-anchor"}) {
		t.Fatalf("atomic evidence sources = %v", evidence[0].SourceEventIDs)
	}
	if strings.Contains(evidence[0].Content, "cobalt reference") ||
		strings.Contains(evidence[0].Content, "catalog overview") {
		t.Fatalf("another event was appended to atomic evidence: %s", evidence[0].Content)
	}
}

func TestSubmodularEvidenceDoesNotHideUnseenContextInAnotherEvidence(t *testing.T) {
	analysis := analyzeMemoryQuery("aurora calibration")
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "anchor", ResourceID: "anchor", ResourceKind: "event", ContextID: "ctx-a",
			Content: "aurora calibration anchor", SourceEventIDs: []string{"source-anchor"}}, score: .99},
		{document: searchDocument{ID: "cross", ResourceID: "cross", ResourceKind: "event", ContextID: "ctx-b",
			Content: "independent calibration observation", SourceEventIDs: []string{"source-cross"}}, score: .84},
	}
	evidence, _, _ := selectSubmodularEvidence(candidates, analysis, 1, 80)
	if len(evidence) != 1 {
		t.Fatalf("evidence count = %d, want 1: %+v", len(evidence), evidence)
	}
	if containsString(evidence[0].SourceEventIDs, "source-cross") ||
		strings.Contains(evidence[0].Content, "independent calibration") {
		t.Fatalf("cross-context event was hidden in atomic evidence: %+v", evidence[0])
	}
	contexts := selectedEvidenceContextIDs(evidence, candidates)
	if !reflect.DeepEqual(contexts, []string{"ctx-a"}) {
		t.Fatalf("selected contexts = %v", contexts)
	}
}

func TestSubmodularEvidenceTreatsLexicalCoverageAsMarginalGain(t *testing.T) {
	analysis := analyzeMemoryQuery("aurora calibration result")
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "semantic-only", ResourceKind: "event", ContextID: "ctx-a",
			Content: "generic neighboring discussion", SourceEventIDs: []string{"semantic-only"}},
			score: .45, coverage: 0},
		{document: searchDocument{ID: "anchored", ResourceKind: "event", ContextID: "ctx-b",
			Content: "aurora calibration observation", SourceEventIDs: []string{"anchored"}},
			score: .34, coverage: .40},
	}
	evidence, _, _ := selectSubmodularEvidence(candidates, analysis, 1, 24)
	if len(evidence) != 1 || evidence[0].ID != "semantic-only" {
		t.Fatalf("lexical coverage overrode the stronger BGE score: %+v", evidence)
	}
}

func TestSubmodularEvidenceAtomicEventsRespectTotalTokenBudget(t *testing.T) {
	analysis := analyzeMemoryQuery("aurora calibration")
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "anchor", ResourceKind: "event", ContextID: "ctx",
			Content: "aurora calibration anchor", SourceEventIDs: []string{"anchor"}}, score: .99},
		{document: searchDocument{ID: "one", ResourceKind: "event", ContextID: "ctx",
			Content: strings.Repeat("calibration detail ", 40), SourceEventIDs: []string{"one"}}, score: .90},
		{document: searchDocument{ID: "two", ResourceKind: "event", ContextID: "ctx",
			Content: strings.Repeat("aurora measurement ", 40), SourceEventIDs: []string{"two"}}, score: .89},
	}
	evidence, _, _ := selectSubmodularEvidence(candidates, analysis, 1, 24)
	if len(evidence) != 1 {
		t.Fatalf("evidence count = %d, want 1", len(evidence))
	}
	if got := estimateTokens(evidence[0].Content); got > 24 {
		t.Fatalf("atomic evidence uses %d tokens, budget 24: %s", got, evidence[0].Content)
	}
}

func TestSubmodularEvidenceCarriesUnusedPayloadBudgetForward(t *testing.T) {
	analysis := analyzeMemoryQuery("aurora calibration measured value")
	candidates := []*rankedCandidate{
		{document: searchDocument{ID: "short", ResourceKind: "event", ContextID: "ctx-a",
			Content: "aurora calibration index", SourceEventIDs: []string{"short"}}, score: .99},
		{document: searchDocument{ID: "long", ResourceKind: "event", ContextID: "ctx-b",
			Content: "A report concerns the aurora calibration procedure. " +
				"The result was reviewed independently by two technicians. " +
				"Its final measured value was 84 credits.",
			SourceEventIDs: []string{"long"}}, score: .98},
	}
	evidence, _, _ := selectSubmodularEvidence(candidates, analysis, 2, 60)
	if len(evidence) != 2 {
		t.Fatalf("evidence count = %d, want 2: %+v", len(evidence), evidence)
	}
	if !strings.Contains(evidence[1].Content, "84 credits") {
		t.Fatalf("unused payload budget did not reach the longer fact: %s", evidence[1].Content)
	}
	total := estimateTokens(evidence[0].Content) + estimateTokens(evidence[1].Content)
	if total > 60 {
		t.Fatalf("evidence uses %d tokens, budget 60: %+v", total, evidence)
	}
}

func TestBGEExactScoreUsesFrozenOfficialWeights(t *testing.T) {
	got := fusedBGEExactScore(.46, .69, .92)
	want := (.46 + .3*.69 + .92) / 2.3
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("fused BGE score = %.12f, want %.12f", got, want)
	}
}

func TestBGEEventScoreUsesFrozenDenseSparseWeights(t *testing.T) {
	got := fusedBGEEventScore(.52, .78)
	want := (.52 + .3*.78) / 1.3
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("fused BGE event score = %.12f, want %.12f", got, want)
	}
}

func TestTopSidecarContextIDsPreservesScoreOrderAndUniqueness(t *testing.T) {
	candidates := []*sidecarCandidate{
		{eventID: "a-1", contextID: "a"},
		{eventID: "a-2", contextID: "a"},
		{eventID: "b-1", contextID: "b"},
		{eventID: "c-1", contextID: "c"},
	}
	if got, want := topSidecarContextIDs(candidates, 2), []string{"a", "b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("top contexts = %v, want %v", got, want)
	}
}

func TestBGECandidateReasonsMarksContextExpansion(t *testing.T) {
	candidate := &sidecarCandidate{rawScores: map[string]float64{"bge_context_expansion": 1}}
	if got := bgeCandidateReasons(candidate); !containsString(got, "context-event-expansion") {
		t.Fatalf("context expansion reason missing: %v", got)
	}
}

func TestTopUnseenPPREventIDsAppliesLimitAfterExcludingShortlist(t *testing.T) {
	rank := map[string]float64{
		"event:seed-a": .99,
		"event:seed-b": .98,
		"context:one":  .97,
		"event:new-a":  .80,
		"event:new-b":  .70,
		"event:new-c":  .60,
	}
	seen := map[string]struct{}{"seed-a": {}, "seed-b": {}}
	got := topUnseenPPREventIDs(rank, seen, 2)
	want := []string{"new-a", "new-b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unseen PPR events = %v, want %v", got, want)
	}
}

func TestSparseDotProductMatchesOnlyIdenticalTokenIDs(t *testing.T) {
	got := sparseDotProduct(map[int64]float32{1: .5, 2: .25}, map[int64]float32{1: .4, 3: 9})
	if math.Abs(got-.2) > 1e-6 {
		t.Fatalf("sparse score = %.6f, want .2", got)
	}
}

func TestEvidenceOrderingGroupsContextsThenUsesChronology(t *testing.T) {
	base := time.Date(2026, time.July, 22, 12, 0, 0, 0, time.UTC)
	evidence := []Evidence{
		{ID: "b-late", ContextID: "b", Score: .7, OccurredAt: base.Add(time.Hour)},
		{ID: "a-late", ContextID: "a", Score: .9, OccurredAt: base.Add(2 * time.Hour)},
		{ID: "a-early", ContextID: "a", Score: .8, OccurredAt: base},
		{ID: "b-early", ContextID: "b", Score: .6, OccurredAt: base.Add(-time.Hour)},
	}
	ordered := orderEvidenceByContextAndTime(evidence)
	want := []string{"a-early", "a-late", "b-early", "b-late"}
	for index, id := range want {
		if ordered[index].ID != id {
			t.Fatalf("ordered evidence = %+v, want IDs %v", ordered, want)
		}
	}
}
