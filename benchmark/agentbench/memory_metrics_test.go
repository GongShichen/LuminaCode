package agentbench

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"LuminaCode/longmemory"
)

func TestOfflineMemoryMetricsResolveInjectedAtoms(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "memory.sqlite")
	store, err := longmemory.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: "metrics-project"}
	messageText := "The deployment database is SQLite and the release region is eu-west."
	if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{
		Episode: &longmemory.Episode{ScopeType: scope.Type, ScopeKey: scope.Key, SessionID: "source-session",
			MessageIDs: []string{"source-message"}, Content: messageText, OccurredAt: time.Now().UTC()},
		EpisodeSpans: []longmemory.EvidenceSpan{{MessageID: "source-message", Role: "user", Text: messageText,
			OccurredAt: time.Now().UTC()}}, AtomTargetTokens: 8, AtomMaxTokens: 16,
	}); err != nil {
		t.Fatal(err)
	}
	atoms, err := store.SearchAtomsKeyword(ctx, []string{"deployment database SQLite"}, []longmemory.Scope{scope}, "", 4)
	if err != nil || len(atoms) == 0 {
		t.Fatalf("find atom: %#v %v", atoms, err)
	}
	chunks, err := store.ChunksByMessageIDs(ctx, []longmemory.Scope{scope}, []string{"source-message"})
	if err != nil || len(chunks) == 0 {
		t.Fatalf("find parent chunk: %#v %v", chunks, err)
	}
	if err := store.RecordUsed(ctx, longmemory.UsedRecord{SessionID: "query-session", Query: "deployment database",
		MemoryIDs: []string{atoms[0].MemoryID}}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, metrics := collectMemoryMetrics(ctx, path, memoryMetricInput{Suite: SuiteLongMemEval,
		GoldChunkIDs: []string{chunks[0].ChunkID}, GoldMessageTexts: map[string]string{"source-message": messageText},
		GoldSourceSessions: []string{"source-session"}}, 1000, false, "")
	if metrics.RetrievedCount != 1 || metrics.GoldChunkHitCount != 1 || metrics.GoldMessageHitCount != 1 || metrics.SourceSessionRecall != 1 {
		t.Fatalf("atom metrics were not resolved through provenance: %#v", metrics)
	}
	if metrics.InjectedTextCoverage <= 0 {
		t.Fatalf("atom offsets did not contribute text coverage: %#v", metrics)
	}
}
