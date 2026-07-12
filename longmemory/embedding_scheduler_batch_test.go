package longmemory

import (
	"context"
	"testing"
)

type batchSizeTestEmbedder struct{}

func (batchSizeTestEmbedder) Model() string   { return "test" }
func (batchSizeTestEmbedder) Dimensions() int { return 1 }
func (batchSizeTestEmbedder) Embed(context.Context, []string, EmbeddingKind) ([][]float32, error) {
	return nil, nil
}

func TestEmbeddingBatchSize(t *testing.T) {
	base := batchSizeTestEmbedder{}
	if got := EmbeddingBatchSize(base); got != 32 {
		t.Fatalf("plain embedder batch size = %d, want 32", got)
	}
	scheduled := SharedEmbeddingScheduler(base, EmbeddingSchedulerOptions{BatchSize: 11})
	if got := EmbeddingBatchSize(scheduled); got != 11 {
		t.Fatalf("scheduled embedder batch size = %d, want 11", got)
	}
}
