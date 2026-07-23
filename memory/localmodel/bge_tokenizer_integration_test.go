//go:build cgo

package localmodel

import (
	"context"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func TestBGESplitRecoversFromMetaspaceASCIIArtPanic(t *testing.T) {
	modelDir := os.Getenv("LUMINA_TEST_BGE_MODEL_DIR")
	if modelDir == "" {
		t.Skip("set LUMINA_TEST_BGE_MODEL_DIR to run the BGE tokenizer integration test")
	}
	encoder, err := NewLocalBGEEncoder(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	texts := []string{
		"A diagram with an arrow pointing to a wheel:\n```\n   ________\n  /        \\\n /          \\\n/            \\\n|            |\n \\          /\n  \\________/\n       |\n       v\n  ________\n /        \\\n/          \\\n|          |\n \\        /\n  \\______/\n```",
		"A side-view vehicle diagram:\n```\n   ________\n  /        \\\n /          \\\n/            \\\n|            |\n|            |\n|            |\n|            |\n \\          /\n  \\________/\n```",
	}
	for _, text := range texts {
		spans, err := encoder.Split(text, 192, 32)
		if err != nil {
			t.Fatal(err)
		}
		if len(spans) != 1 || spans[0] != text {
			t.Fatalf("unexpected spans: %#v", spans)
		}
	}
}

func TestBGESplitManyMatchesIndependentPlans(t *testing.T) {
	modelDir := os.Getenv("LUMINA_TEST_BGE_MODEL_DIR")
	if modelDir == "" {
		t.Skip("set LUMINA_TEST_BGE_MODEL_DIR to run the BGE tokenizer integration test")
	}
	encoder, err := NewLocalBGEEncoder(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	text := strings.Repeat(
		"The project record preserves a complete event with its original temporal context. ", 120)
	specs := []BGESplitSpec{{MaxTokens: 192, Overlap: 32}, {MaxTokens: 8192, Overlap: 256}}
	combined, err := encoder.SplitMany(text, specs)
	if err != nil {
		t.Fatal(err)
	}
	if len(combined) != len(specs) {
		t.Fatalf("combined split results = %d, want %d", len(combined), len(specs))
	}
	for index, spec := range specs {
		independent, splitErr := encoder.Split(text, spec.MaxTokens, spec.Overlap)
		if splitErr != nil {
			t.Fatal(splitErr)
		}
		if !reflect.DeepEqual(combined[index], independent) {
			t.Fatalf("split plan %d changed output", index)
		}
	}
}

func BenchmarkBGESplitPlans(b *testing.B) {
	modelDir := os.Getenv("LUMINA_TEST_BGE_MODEL_DIR")
	if modelDir == "" {
		b.Skip("set LUMINA_TEST_BGE_MODEL_DIR to run the BGE tokenizer benchmark")
	}
	encoder, err := NewLocalBGEEncoder(modelDir)
	if err != nil {
		b.Fatal(err)
	}
	defer encoder.Close()
	text := strings.Repeat(
		"The project record preserves a complete event with its original temporal context. ", 120)
	specs := []BGESplitSpec{{MaxTokens: 192, Overlap: 32}, {MaxTokens: 8192, Overlap: 256}}
	b.Run("combined", func(b *testing.B) {
		for b.Loop() {
			if _, splitErr := encoder.SplitMany(text, specs); splitErr != nil {
				b.Fatal(splitErr)
			}
		}
	})
	b.Run("independent", func(b *testing.B) {
		for b.Loop() {
			for _, spec := range specs {
				if _, splitErr := encoder.Split(text, spec.MaxTokens, spec.Overlap); splitErr != nil {
					b.Fatal(splitErr)
				}
			}
		}
	})
}

func BenchmarkBGESplitPlansConcurrent(b *testing.B) {
	modelDir := os.Getenv("LUMINA_TEST_BGE_MODEL_DIR")
	if modelDir == "" {
		b.Skip("set LUMINA_TEST_BGE_MODEL_DIR to run the BGE tokenizer benchmark")
	}
	encoder, err := NewLocalBGEEncoder(modelDir)
	if err != nil {
		b.Fatal(err)
	}
	defer encoder.Close()
	text := strings.Repeat(
		"The project record preserves a complete event with its original temporal context. ", 120)
	specs := []BGESplitSpec{{MaxTokens: 192, Overlap: 32}, {MaxTokens: 8192, Overlap: 256}}
	for b.Loop() {
		var group sync.WaitGroup
		errorsByCall := make([]error, 20)
		for index := range errorsByCall {
			group.Add(1)
			go func() {
				defer group.Done()
				_, errorsByCall[index] = encoder.SplitMany(text, specs)
			}()
		}
		group.Wait()
		for _, splitErr := range errorsByCall {
			if splitErr != nil {
				b.Fatal(splitErr)
			}
		}
	}
}

func TestBGEEncodingKeepsEveryNonSpecialTokenForMaxSim(t *testing.T) {
	modelDir := os.Getenv("LUMINA_TEST_BGE_MODEL_DIR")
	if modelDir == "" {
		t.Skip("set LUMINA_TEST_BGE_MODEL_DIR to run the BGE model integration test")
	}
	encoder, err := NewLocalBGEEncoder(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	text := "A low lexical-weight token must still participate in late interaction."
	input, err := encoder.prepare(text, bgeMaximumTokens)
	if err != nil {
		t.Fatal(err)
	}
	want := 0
	for index := range input.ids {
		if input.attention[index] != 0 && input.specialMask[index] == 0 {
			want++
		}
	}
	encoded, err := encoder.Encode(context.Background(), []string{text}, BGEDocument)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != 1 || len(encoded[0].Multi) != want {
		t.Fatalf("ColBERT vectors = %d, want every %d non-special token", len(encoded[0].Multi), want)
	}
	for _, token := range encoded[0].Multi {
		if token.Position < 0 || token.Position >= len(input.ids) || input.specialMask[token.Position] != 0 {
			t.Fatalf("invalid ColBERT token position: %+v", token)
		}
	}
}

func TestBGEEncodingRunsFullShortDocumentBatch(t *testing.T) {
	modelDir := os.Getenv("LUMINA_TEST_BGE_MODEL_DIR")
	if modelDir == "" {
		t.Skip("set LUMINA_TEST_BGE_MODEL_DIR to run the BGE batch integration test")
	}
	encoder, err := NewLocalBGEEncoder(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	texts := make([]string, bgeInferenceBatchLimit)
	for index := range texts {
		texts[index] = fmt.Sprintf("Document %d records a concise local memory fact with stable context.", index)
	}
	encoded, err := encoder.EncodeChannels(context.Background(), texts, BGEDocument)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != len(texts) {
		t.Fatalf("encoded documents = %d, want %d", len(encoded), len(texts))
	}
}

func TestBGEEncodingPoolRunsConcurrentCalls(t *testing.T) {
	modelDir := os.Getenv("LUMINA_TEST_BGE_MODEL_DIR")
	if modelDir == "" {
		t.Skip("set LUMINA_TEST_BGE_MODEL_DIR to run the BGE pool integration test")
	}
	t.Setenv("LUMINA_MEMORY_EMBEDDING_THREADS", "10")
	t.Setenv("LUMINA_MEMORY_EMBEDDING_SESSIONS", "2")
	t.Setenv("LUMINA_MEMORY_TOKENIZER_WORKERS", "2")
	encoder, err := NewLocalBGEEncoder(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	defer encoder.Close()
	if encoder.metal == nil && len(encoder.sessions) != 2 {
		t.Fatalf("BGE sessions = %d, want 2", len(encoder.sessions))
	}
	if encoder.tokenizerWorkers != 2 {
		t.Fatalf("BGE tokenizer workers = %d, want 2", encoder.tokenizerWorkers)
	}
	texts := make([]string, 16)
	for index := range texts {
		texts[index] = fmt.Sprintf("Concurrent document %d records a local memory fact.", index)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	for worker := 0; worker < 2; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			encoded, encodeErr := encoder.EncodeChannels(context.Background(), texts, BGEDocument)
			if encodeErr == nil && len(encoded) != len(texts) {
				encodeErr = fmt.Errorf("encoded documents = %d, want %d", len(encoded), len(texts))
			}
			errs <- encodeErr
		}()
	}
	close(start)
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestBGEQuantizedVariantPreservesReferenceScores(t *testing.T) {
	modelDir := os.Getenv("LUMINA_TEST_BGE_MODEL_DIR")
	referenceDir := os.Getenv("LUMINA_TEST_BGE_REFERENCE_MODEL_DIR")
	if modelDir == "" || referenceDir == "" {
		t.Skip("set candidate and reference BGE model directories to run the quantization gate")
	}
	candidate, err := NewLocalBGEEncoder(modelDir)
	if err != nil {
		t.Fatal(err)
	}
	defer candidate.Close()
	reference, err := NewLocalBGEEncoder(referenceDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Close()

	queryText := "Which submission deadline and advisor were recorded for the project?"
	documentTexts := []string{
		"The project submission deadline was February 1, and Dr. Lee agreed to advise it.",
		"A general explanation of project planning and scheduling.",
		"The user bought groceries and later discussed a weekend trip.",
		"Dr. Lee reviewed the proposal before its February deadline.",
	}
	candidateQuery, err := candidate.Encode(context.Background(), []string{queryText}, BGEQuery)
	if err != nil {
		t.Fatal(err)
	}
	referenceQuery, err := reference.Encode(context.Background(), []string{queryText}, BGEQuery)
	if err != nil {
		t.Fatal(err)
	}
	candidateDocuments, err := candidate.Encode(context.Background(), documentTexts, BGEDocument)
	if err != nil {
		t.Fatal(err)
	}
	referenceDocuments, err := reference.Encode(context.Background(), documentTexts, BGEDocument)
	if err != nil {
		t.Fatal(err)
	}
	if similarity := cosineSimilarity(candidateQuery[0].Dense, referenceQuery[0].Dense); similarity < .98 {
		t.Errorf("quantized query dense cosine = %.6f, want >= .98", similarity)
	}
	candidateScores := make([]float64, len(documentTexts))
	referenceScores := make([]float64, len(documentTexts))
	for index := range documentTexts {
		if similarity := cosineSimilarity(candidateDocuments[index].Dense, referenceDocuments[index].Dense); similarity < .98 {
			t.Errorf("quantized document %d dense cosine = %.6f, want >= .98", index, similarity)
		}
		candidateScores[index] = fusedBGEProbeScore(candidateQuery[0], candidateDocuments[index])
		referenceScores[index] = fusedBGEProbeScore(referenceQuery[0], referenceDocuments[index])
		if difference := math.Abs(candidateScores[index] - referenceScores[index]); difference > .08 {
			t.Errorf("quantized document %d fused score differs by %.6f, want <= .08", index, difference)
		}
	}
	if bestScoreIndex(candidateScores) != bestScoreIndex(referenceScores) {
		t.Errorf("quantized top document = %d, reference = %d",
			bestScoreIndex(candidateScores), bestScoreIndex(referenceScores))
	}
}

func cosineSimilarity(left, right []float32) float64 {
	if len(left) == 0 || len(left) != len(right) {
		return 0
	}
	var dot, leftNorm, rightNorm float64
	for index := range left {
		dot += float64(left[index] * right[index])
		leftNorm += float64(left[index] * left[index])
		rightNorm += float64(right[index] * right[index])
	}
	if leftNorm == 0 || rightNorm == 0 {
		return 0
	}
	return dot / math.Sqrt(leftNorm*rightNorm)
}

func fusedBGEProbeScore(query, document BGEEmbedding) float64 {
	return (bgeProbeDense(query.Dense, document.Dense) +
		.3*bgeProbeSparse(query.Sparse, document.Sparse) +
		bgeProbeMaxSim(query.Multi, document.Multi)) / 2.3
}

func bestScoreIndex(scores []float64) int {
	best := -1
	for index := range scores {
		if best < 0 || scores[index] > scores[best] {
			best = index
		}
	}
	return best
}

func BenchmarkBGEConcurrentEncoding(b *testing.B) {
	modelDir := os.Getenv("LUMINA_TEST_BGE_MODEL_DIR")
	if modelDir == "" {
		b.Skip("set LUMINA_TEST_BGE_MODEL_DIR to run the BGE concurrency benchmark")
	}
	encoder, err := NewLocalBGEEncoder(modelDir)
	if err != nil {
		b.Fatal(err)
	}
	defer encoder.Close()
	texts := make([]string, 64)
	for index := range texts {
		texts[index] = fmt.Sprintf(
			"Document %d records a concise local memory fact and enough surrounding context for retrieval.", index)
	}
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		start := make(chan struct{})
		const workers = 4
		errs := make(chan error, workers)
		var wait sync.WaitGroup
		for worker := 0; worker < workers; worker++ {
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				_, encodeErr := encoder.EncodeChannels(context.Background(), texts, BGEDocument)
				errs <- encodeErr
			}()
		}
		close(start)
		wait.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}

func BenchmarkBGEConcurrentQueries(b *testing.B) {
	modelDir := os.Getenv("LUMINA_TEST_BGE_MODEL_DIR")
	if modelDir == "" {
		b.Skip("set LUMINA_TEST_BGE_MODEL_DIR to run the BGE concurrency benchmark")
	}
	encoder, err := NewLocalBGEEncoder(modelDir)
	if err != nil {
		b.Fatal(err)
	}
	defer encoder.Close()
	const workers = 20
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		start := make(chan struct{})
		errs := make(chan error, workers)
		var wait sync.WaitGroup
		for worker := 0; worker < workers; worker++ {
			worker := worker
			wait.Add(1)
			go func() {
				defer wait.Done()
				<-start
				_, encodeErr := encoder.EncodeChannels(context.Background(), []string{
					fmt.Sprintf("What local memory fact was recorded for item %d?", worker),
				}, BGEQuery)
				errs <- encodeErr
			}()
		}
		close(start)
		wait.Wait()
		close(errs)
		for err := range errs {
			if err != nil {
				b.Fatal(err)
			}
		}
	}
}
