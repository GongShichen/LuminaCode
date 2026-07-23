package agent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"LuminaCode/memory"
)

type recordingArtifactEmbedder struct {
	mu         sync.Mutex
	calls      int
	texts      int
	dimensions int
	err        error
}

func (e *recordingArtifactEmbedder) Model() string { return "artifact-test-model" }

func (e *recordingArtifactEmbedder) Dimensions() int { return e.dimensions }

func (e *recordingArtifactEmbedder) EmbedDense(_ context.Context, texts []string,
	_ memory.VectorPurpose) ([][]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls++
	e.texts += len(texts)
	if e.err != nil {
		return nil, e.err
	}
	result := make([][]float32, len(texts))
	for index, text := range texts {
		result[index] = make([]float32, e.dimensions)
		for dimension := range result[index] {
			result[index][dimension] = float32(len(text) + dimension + 1)
		}
	}
	return result, nil
}

func TestFabricVectorizerReusesContentArtifactsAcrossInstances(t *testing.T) {
	cacheDir := t.TempDir()
	firstEmbedder := &recordingArtifactEmbedder{dimensions: 4}
	first := newFabricVectorizer(firstEmbedder, cacheDir)
	want, err := first.Embed(context.Background(), []string{"alpha", "beta", "alpha"}, memory.VectorContent)
	if err != nil {
		t.Fatal(err)
	}
	if firstEmbedder.calls != 1 || firstEmbedder.texts != 2 {
		t.Fatalf("first embedding calls=%d texts=%d, want one call for two unique texts",
			firstEmbedder.calls, firstEmbedder.texts)
	}

	secondEmbedder := &recordingArtifactEmbedder{dimensions: 4, err: errors.New("cache miss")}
	second := newFabricVectorizer(secondEmbedder, cacheDir)
	got, err := second.Embed(context.Background(), []string{"alpha", "beta", "alpha"}, memory.VectorContent)
	if err != nil {
		t.Fatalf("load shared embedding artifacts: %v", err)
	}
	if secondEmbedder.calls != 0 {
		t.Fatalf("second embedder calls=%d, want zero", secondEmbedder.calls)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cached vectors = %v, want %v", got, want)
	}
}

func TestFabricVectorizerDoesNotPersistQueryEmbeddings(t *testing.T) {
	cacheDir := t.TempDir()
	embedder := &recordingArtifactEmbedder{dimensions: 3}
	vectorizer := newFabricVectorizer(embedder, cacheDir)
	for iteration := 0; iteration < 2; iteration++ {
		if _, err := vectorizer.Embed(context.Background(), []string{"same query"}, memory.VectorQuery); err != nil {
			t.Fatal(err)
		}
	}
	if embedder.calls != 2 {
		t.Fatalf("query embedder calls=%d, want two uncached calls", embedder.calls)
	}
}
