//go:build !cgo

package longmemory

import (
	"context"
	"fmt"
)

type EmbeddingKind string

const (
	EmbeddingQuery   EmbeddingKind = "query"
	EmbeddingPassage EmbeddingKind = "passage"
)

type Embedder interface {
	Model() string
	Dimensions() int
	Embed(ctx context.Context, texts []string, kind EmbeddingKind) ([][]float32, error)
}

type LocalEmbedder struct{}

func NewLocalEmbedder(modelName, modelDir string) (*LocalEmbedder, error) {
	return nil, fmt.Errorf("local memory embeddings require a cgo-enabled LuminaCode build")
}

func SharedLocalEmbedder(modelName, modelDir string) (*LocalEmbedder, error) {
	return NewLocalEmbedder(modelName, modelDir)
}

func (e *LocalEmbedder) Model() string { return "" }

func (e *LocalEmbedder) Dimensions() int { return 0 }

func (e *LocalEmbedder) Provider() string { return "unavailable" }

func (e *LocalEmbedder) ProviderDiagnostics() []string {
	return []string{"local memory embeddings require a cgo-enabled LuminaCode build"}
}

func (e *LocalEmbedder) Embed(context.Context, []string, EmbeddingKind) ([][]float32, error) {
	return nil, fmt.Errorf("local memory embeddings require a cgo-enabled LuminaCode build")
}

func (e *LocalEmbedder) Close() error { return nil }
