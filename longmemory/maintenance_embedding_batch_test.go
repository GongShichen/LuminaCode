package longmemory

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestCommitEmbeddingBatchUsesStoreWriterLock(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "memory.sqlite")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	writer := extractionCommitLock(path)
	writer.Lock()
	done := make(chan error, 1)
	go func() {
		done <- store.commitEmbeddingBatch(ctx, "atom", "test-model", []pendingEmbeddingWrite{
			{id: "atom-a", contentHash: "hash-a", vector: []float32{1, 0}},
			{id: "atom-b", contentHash: "hash-b", vector: []float32{0, 1}},
		})
	}()
	select {
	case err := <-done:
		writer.Unlock()
		t.Fatalf("embedding batch bypassed writer lock: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	writer.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var count int
	if err := store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_atom_embeddings WHERE model='test-model'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("embedding count = %d, want 2", count)
	}
}
