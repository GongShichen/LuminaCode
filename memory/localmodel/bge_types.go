package localmodel

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	BGEModelName           = "bge-m3"
	BGEModelRevision       = "e44369c5623cc146f016da906583db4ee0e3488d"
	BGEEmbeddingDimensions = 1024
	bgeCPUINT8Identity     = BGEModelRevision + ":cpu-int8@3fa3a927e7bc973ae751a8add34455b52d915ac0"
	bgeMetalINT8Identity   = BGEModelRevision + ":metal-int8@8273f354c222397b0ffeb5f053d3e810aa485ce9"
)

type bgeModelManifest struct {
	Revision          string `json:"revision"`
	Variant           string `json:"variant"`
	VariantRepository string `json:"variant_repository"`
	VariantRevision   string `json:"variant_revision"`
}

func bgeModelIdentity(modelDir string) string {
	content, err := os.ReadFile(filepath.Join(modelDir, "manifest.json"))
	if err != nil {
		return BGEModelRevision
	}
	var manifest bgeModelManifest
	if json.Unmarshal(content, &manifest) != nil {
		return BGEModelRevision
	}
	baseRevision := strings.TrimSpace(manifest.Revision)
	if baseRevision == "" {
		baseRevision = BGEModelRevision
	}
	variant := strings.TrimSpace(manifest.Variant)
	variantRevision := strings.TrimSpace(manifest.VariantRevision)
	if variant == "" || variantRevision == "" {
		return baseRevision
	}
	return baseRevision + ":" + variant + "@" + variantRevision
}

type BGEInputKind string

const (
	BGEQuery    BGEInputKind = "query"
	BGEDocument BGEInputKind = "document"
)

type BGETokenVector struct {
	TokenID  int64
	Position int
	Weight   float32
	Values   []float32
}

type BGEEmbedding struct {
	Dense  []float32
	Sparse map[int64]float32
	Multi  []BGETokenVector
}

type BGESplitSpec struct {
	MaxTokens int
	Overlap   int
}

type BGEEncoder interface {
	Model() string
	Revision() string
	TokenizerHash() string
	Encode(context.Context, []string, BGEInputKind) ([]BGEEmbedding, error)
	EncodeChannels(context.Context, []string, BGEInputKind) ([]BGEEmbedding, error)
	Split(text string, maxTokens, overlap int) ([]string, error)
	SplitMany(text string, specs []BGESplitSpec) ([][]string, error)
}

func (e *LocalBGEEncoder) CompatibleRevision(other string) bool {
	if e == nil {
		return false
	}
	current := e.Revision()
	if current == other {
		return true
	}
	return (current == bgeCPUINT8Identity && other == bgeMetalINT8Identity) ||
		(current == bgeMetalINT8Identity && other == bgeCPUINT8Identity)
}

func ProbeBGE(modelDir string) error {
	encoder, err := NewLocalBGEEncoder(modelDir)
	if err != nil {
		return err
	}
	defer encoder.Close()
	query, err := encoder.Encode(context.Background(), []string{"local memory retrieval probe"}, BGEQuery)
	if err != nil {
		return err
	}
	document, err := encoder.Encode(context.Background(), []string{"local memory retrieval probe document"}, BGEDocument)
	if err != nil {
		return err
	}
	if len(query) != 1 || len(document) != 1 {
		return fmt.Errorf("BGE probe returned query/document counts %d/%d, want 1/1", len(query), len(document))
	}
	for index, item := range []BGEEmbedding{query[0], document[0]} {
		if len(item.Dense) != BGEEmbeddingDimensions || len(item.Sparse) == 0 || len(item.Multi) == 0 {
			return fmt.Errorf("BGE probe output %d is incomplete: dense=%d sparse=%d multi=%d",
				index, len(item.Dense), len(item.Sparse), len(item.Multi))
		}
		for _, value := range item.Dense {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return fmt.Errorf("BGE probe output %d contains a non-finite dense value", index)
			}
		}
	}
	dense := bgeProbeDense(query[0].Dense, document[0].Dense)
	sparse := bgeProbeSparse(query[0].Sparse, document[0].Sparse)
	colbert := bgeProbeMaxSim(query[0].Multi, document[0].Multi)
	fused := (dense + .3*sparse + colbert) / 2.3
	for name, value := range map[string]float64{"dense": dense, "sparse": sparse, "colbert": colbert, "fused": fused} {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("BGE probe %s score is not finite", name)
		}
	}
	return nil
}

func bgeProbeDense(left, right []float32) float64 {
	var result float64
	for index, value := range left {
		result += float64(value * right[index])
	}
	return result
}

func bgeProbeSparse(left, right map[int64]float32) float64 {
	if len(left) > len(right) {
		left, right = right, left
	}
	var result float64
	for tokenID, weight := range left {
		result += float64(weight * right[tokenID])
	}
	return result
}

func bgeProbeMaxSim(query, document []BGETokenVector) float64 {
	if len(query) == 0 || len(document) == 0 {
		return 0
	}
	var total float64
	for _, queryToken := range query {
		best := -math.MaxFloat64
		for _, documentToken := range document {
			if len(queryToken.Values) != len(documentToken.Values) {
				continue
			}
			score := bgeProbeDense(queryToken.Values, documentToken.Values)
			if score > best {
				best = score
			}
		}
		if best > -math.MaxFloat64 {
			total += best
		}
	}
	return total / float64(len(query))
}
