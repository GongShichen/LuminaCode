//go:build cgo

package localmodel

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"testing"
)

type bgeOfficialFixture struct {
	ModelRevision string                       `json:"model_revision"`
	Query         string                       `json:"query"`
	Document      string                       `json:"document"`
	QueryOutput   bgeOfficialFixtureEmbedding  `json:"query_output"`
	DocOutput     bgeOfficialFixtureEmbedding  `json:"document_output"`
	Scores        bgeOfficialFixtureScores     `json:"scores"`
	Tolerance     bgeOfficialFixtureTolerances `json:"tolerance"`
}

type bgeOfficialFixtureEmbedding struct {
	Dense   []float32                       `json:"dense"`
	Sparse  map[string]float32              `json:"sparse"`
	ColBERT []bgeOfficialFixtureTokenVector `json:"colbert"`
}

type bgeOfficialFixtureTokenVector struct {
	TokenID  int64     `json:"token_id"`
	Position int       `json:"position"`
	Values   []float32 `json:"values"`
}

type bgeOfficialFixtureScores struct {
	Dense   float64 `json:"dense"`
	Sparse  float64 `json:"sparse"`
	ColBERT float64 `json:"colbert"`
	Fused   float64 `json:"fused"`
}

type bgeOfficialFixtureTolerances struct {
	Dense   float64 `json:"dense"`
	Sparse  float64 `json:"sparse"`
	ColBERT float64 `json:"colbert"`
	Scores  float64 `json:"scores"`
}

func TestBGEEncodingMatchesOfficialFlagEmbeddingFixture(t *testing.T) {
	modelDir := os.Getenv("LUMINA_TEST_BGE_MODEL_DIR")
	fixturePath := os.Getenv("LUMINA_TEST_BGE_OFFICIAL_FIXTURE")
	if modelDir == "" || fixturePath == "" {
		t.Skip("set LUMINA_TEST_BGE_MODEL_DIR and LUMINA_TEST_BGE_OFFICIAL_FIXTURE to compare with FlagEmbedding")
	}
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatal(err)
	}
	var fixture bgeOfficialFixture
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.ModelRevision != BGEModelRevision {
		t.Fatalf("fixture revision = %q, want %q", fixture.ModelRevision, BGEModelRevision)
	}
	applyBGEFixtureDefaultTolerances(&fixture.Tolerance)
	encoder, err := NewLocalBGEEncoder(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	query, err := encoder.Encode(context.Background(), []string{fixture.Query}, BGEQuery)
	if err != nil {
		t.Fatal(err)
	}
	document, err := encoder.Encode(context.Background(), []string{fixture.Document}, BGEDocument)
	if err != nil {
		t.Fatal(err)
	}
	if len(query) != 1 || len(document) != 1 {
		t.Fatalf("encoding counts = %d/%d, want 1/1", len(query), len(document))
	}
	compareBGEFixtureEmbedding(t, "query", query[0], fixture.QueryOutput, fixture.Tolerance)
	compareBGEFixtureEmbedding(t, "document", document[0], fixture.DocOutput, fixture.Tolerance)

	scores := bgeOfficialFixtureScores{
		Dense:   bgeProbeDense(query[0].Dense, document[0].Dense),
		Sparse:  bgeProbeSparse(query[0].Sparse, document[0].Sparse),
		ColBERT: bgeProbeMaxSim(query[0].Multi, document[0].Multi),
	}
	scores.Fused = (scores.Dense + .3*scores.Sparse + scores.ColBERT) / 2.3
	for name, pair := range map[string][2]float64{
		"dense": {scores.Dense, fixture.Scores.Dense}, "sparse": {scores.Sparse, fixture.Scores.Sparse},
		"colbert": {scores.ColBERT, fixture.Scores.ColBERT}, "fused": {scores.Fused, fixture.Scores.Fused},
	} {
		if math.Abs(pair[0]-pair[1]) > fixture.Tolerance.Scores {
			t.Errorf("%s score = %.8f, official %.8f", name, pair[0], pair[1])
		}
	}
}

func applyBGEFixtureDefaultTolerances(value *bgeOfficialFixtureTolerances) {
	if value.Dense == 0 {
		value.Dense = 5e-4
	}
	if value.Sparse == 0 {
		value.Sparse = 5e-4
	}
	if value.ColBERT == 0 {
		value.ColBERT = 1e-3
	}
	if value.Scores == 0 {
		value.Scores = 1e-3
	}
}

func compareBGEFixtureEmbedding(t *testing.T, name string, got BGEEmbedding,
	want bgeOfficialFixtureEmbedding, tolerance bgeOfficialFixtureTolerances) {
	t.Helper()
	compareBGEFixtureVector(t, name+" dense", got.Dense, want.Dense, tolerance.Dense)
	if len(got.Sparse) != len(want.Sparse) {
		t.Errorf("%s sparse entries = %d, official %d", name, len(got.Sparse), len(want.Sparse))
	}
	for rawID, expected := range want.Sparse {
		tokenID, err := strconv.ParseInt(rawID, 10, 64)
		if err != nil {
			t.Fatalf("invalid official sparse token ID %q: %v", rawID, err)
		}
		if difference := math.Abs(float64(got.Sparse[tokenID] - expected)); difference > tolerance.Sparse {
			t.Errorf("%s sparse[%d] differs by %.8f", name, tokenID, difference)
		}
	}
	if len(got.Multi) != len(want.ColBERT) {
		t.Fatalf("%s ColBERT token count = %d, official %d", name, len(got.Multi), len(want.ColBERT))
	}
	for index, expected := range want.ColBERT {
		actual := got.Multi[index]
		if actual.TokenID != expected.TokenID || actual.Position != expected.Position {
			t.Errorf("%s ColBERT token %d metadata = %d/%d, official %d/%d", name, index,
				actual.TokenID, actual.Position, expected.TokenID, expected.Position)
		}
		compareBGEFixtureVector(t, fmt.Sprintf("%s ColBERT token %d", name, index),
			actual.Values, expected.Values, tolerance.ColBERT)
	}
}

func compareBGEFixtureVector(t *testing.T, name string, got, want []float32, tolerance float64) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s dimensions = %d, official %d", name, len(got), len(want))
	}
	for index := range want {
		if difference := math.Abs(float64(got[index] - want[index])); difference > tolerance {
			t.Errorf("%s[%d] differs by %.8f", name, index, difference)
			return
		}
	}
}
