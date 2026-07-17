//go:build cgo

package longmemory

import (
	"strings"
	"testing"

	"github.com/sugarme/tokenizer"
)

func TestTokenizeEmbeddingTextRecoversWithWhitespaceNormalization(t *testing.T) {
	var inputs []string
	encode := func(value string) (*tokenizer.Encoding, error) {
		inputs = append(inputs, value)
		if strings.Contains(value, "\n") {
			panic("normalizer alignment failure")
		}
		return &tokenizer.Encoding{Ids: []int{1, 2}, TypeIds: []int{0, 0},
			AttentionMask: []int{1, 1}}, nil
	}
	encoding, err := tokenizeEmbeddingText(encode, "passage: diagram\n  +---+\n  |   |")
	if err != nil {
		t.Fatalf("tokenize with fallback: %v", err)
	}
	if len(encoding.GetIds()) != 2 {
		t.Fatalf("fallback token count = %d, want 2", len(encoding.GetIds()))
	}
	if len(inputs) != 2 {
		t.Fatalf("tokenizer calls = %d, want 2", len(inputs))
	}
	if got, want := inputs[1], "passage: diagram +---+ | |"; got != want {
		t.Fatalf("fallback input = %q, want %q", got, want)
	}
}

func TestTokenizeEmbeddingTextReturnsPanicAsErrorWhenNoFallbackChangesInput(t *testing.T) {
	_, err := tokenizeEmbeddingText(func(string) (*tokenizer.Encoding, error) {
		panic("normalizer alignment failure")
	}, "passage: plain")
	if err == nil || !strings.Contains(err.Error(), "tokenizer panic") {
		t.Fatalf("tokenizer panic error = %v", err)
	}
}
