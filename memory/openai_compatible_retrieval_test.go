package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestOpenAICompatibleRetrievalEncoderUsesEmbeddingContract(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" || r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("unexpected embedding request: path=%q auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request struct {
			Model          string   `json:"model"`
			Input          []string `json:"input"`
			EncodingFormat string   `json:"encoding_format"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != "BAAI/bge-m3" || request.EncodingFormat != "float" || len(request.Input) != 2 {
			t.Fatalf("unexpected embedding body: %+v", request)
		}
		left := make([]float32, openAICompatibleBGEDimensions)
		right := make([]float32, openAICompatibleBGEDimensions)
		left[0], left[1] = 3, 4
		right[2] = 2
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"index": 1, "embedding": right},
			map[string]any{"index": 0, "embedding": left},
		}})
	}))
	defer server.Close()

	encoder, err := NewOpenAICompatibleRetrievalEncoder(OpenAICompatibleModelOptions{
		APIKey: "secret", BaseURL: server.URL, Model: "BAAI/bge-m3", HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := encoder.EncodeChannels(context.Background(), []string{"query", "document"}, RetrievalQuery)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != 2 || len(encoded[0].Dense) != openAICompatibleBGEDimensions ||
		math.Abs(float64(encoded[0].Dense[0]-.6)) > 1e-6 ||
		math.Abs(float64(encoded[0].Dense[1]-.8)) > 1e-6 || encoded[1].Dense[2] != 1 {
		t.Fatalf("unexpected normalized embeddings: first=%v second=%v", encoded[0].Dense[:3], encoded[1].Dense[:3])
	}
	if len(encoded[0].Sparse) != 0 || len(encoded[0].Multi) != 0 {
		t.Fatalf("OpenAI embedding endpoint should provide dense channels only: %+v", encoded[0])
	}
}

func TestOpenAICompatibleRetrievalEncoderBatchesProviderRequests(t *testing.T) {
	var calls atomic.Int32
	var largest atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		calls.Add(1)
		for {
			current := largest.Load()
			if int32(len(request.Input)) <= current || largest.CompareAndSwap(current, int32(len(request.Input))) {
				break
			}
		}
		data := make([]map[string]any, len(request.Input))
		for index := range request.Input {
			vector := make([]float32, openAICompatibleBGEDimensions)
			vector[0] = 1
			data[index] = map[string]any{"index": index, "embedding": vector}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer server.Close()
	encoder, err := NewOpenAICompatibleRetrievalEncoder(OpenAICompatibleModelOptions{
		APIKey: "secret", BaseURL: server.URL, Model: "BAAI/bge-m3", HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	texts := make([]string, remoteRetrievalBatchSize*2+5)
	for index := range texts {
		texts[index] = fmt.Sprintf("document %d", index)
	}
	encoded, err := encoder.EncodeChannels(context.Background(), texts, RetrievalDocument)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) != len(texts) {
		t.Fatalf("encoded=%d, want %d", len(encoded), len(texts))
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("provider calls=%d, want 3", got)
	}
	if got := largest.Load(); got != remoteRetrievalBatchSize {
		t.Fatalf("largest provider batch=%d, want %d", got, remoteRetrievalBatchSize)
	}
}

func TestOpenAICompatibleRetrievalEncoderIdentityAndUnicodeSplit(t *testing.T) {
	first, err := NewOpenAICompatibleRetrievalEncoder(OpenAICompatibleModelOptions{
		APIKey: "first", BaseURL: "https://example.test/v1", Model: "BAAI/bge-m3",
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewOpenAICompatibleRetrievalEncoder(OpenAICompatibleModelOptions{
		APIKey: "second", BaseURL: "https://example.test/v1", Model: "BAAI/bge-m3",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision() != second.Revision() {
		t.Fatal("API key must not change the embedding-space identity")
	}
	chunks, err := first.Split("甲乙丙丁戊己庚辛壬癸", 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(chunks) != 3 || chunks[0] != "甲乙丙丁戊己" || chunks[1] != "丁戊己庚辛壬" ||
		chunks[2] != "庚辛壬癸" {
		t.Fatalf("unexpected Unicode chunks: %#v", chunks)
	}
}

func TestOpenAICompatibleRerankerMapsScoresByDocumentIndex(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/rerank" || r.Header.Get("Authorization") != "Bearer rerank-secret" {
			t.Fatalf("unexpected rerank request: path=%q auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		var request struct {
			Model     string   `json:"model"`
			Query     string   `json:"query"`
			Documents []string `json:"documents"`
			TopN      int      `json:"top_n"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Model != "reranker" || request.Query != "query" || request.TopN != 3 || len(request.Documents) != 3 {
			t.Fatalf("unexpected rerank body: %+v", request)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"results": []any{
			map[string]any{"index": 2, "relevance_score": .7},
			map[string]any{"index": 0, "relevance_score": .9},
			map[string]any{"index": 1, "relevance_score": .2},
		}})
	}))
	defer server.Close()

	reranker, err := NewOpenAICompatibleReranker(OpenAICompatibleModelOptions{
		APIKey: "rerank-secret", BaseURL: server.URL + "/v1/rerank", Model: "reranker",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	scores, err := reranker.Rerank(context.Background(), "query", []string{"a", "b", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(scores) != 3 || scores[0] != .9 || scores[1] != .2 || scores[2] != .7 {
		t.Fatalf("unexpected rerank scores: %#v", scores)
	}
}

func TestOpenAICompatibleRerankerNormalizesAlibabaCompatibleEndpoint(t *testing.T) {
	endpoint, err := openAICompatibleModelEndpoint(
		"https://workspace.cn-beijing.maas.aliyuncs.com/compatible-mode/v1", "rerank")
	if err != nil {
		t.Fatal(err)
	}
	want := "https://workspace.cn-beijing.maas.aliyuncs.com/compatible-api/v1/reranks"
	if endpoint != want {
		t.Fatalf("reranker endpoint=%q, want %q", endpoint, want)
	}
}

func TestOpenAICompatibleRemoteModelRetries429ButNot402(t *testing.T) {
	var retryCalls atomic.Int32
	retryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if retryCalls.Add(1) == 1 {
			http.Error(w, "busy", http.StatusTooManyRequests)
			return
		}
		vector := make([]float32, openAICompatibleBGEDimensions)
		vector[0] = 1
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": vector}}})
	}))
	defer retryServer.Close()
	encoder, err := NewOpenAICompatibleRetrievalEncoder(OpenAICompatibleModelOptions{
		APIKey: "key", BaseURL: retryServer.URL, Model: "model", HTTPClient: retryServer.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := encoder.Encode(ctx, []string{"text"}, RetrievalQuery); err != nil {
		t.Fatal(err)
	}
	if retryCalls.Load() != 2 {
		t.Fatalf("429 attempts=%d, want 2", retryCalls.Load())
	}

	var quotaCalls atomic.Int32
	quotaServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		quotaCalls.Add(1)
		http.Error(w, "payment required", http.StatusPaymentRequired)
	}))
	defer quotaServer.Close()
	reranker, err := NewOpenAICompatibleReranker(OpenAICompatibleModelOptions{
		APIKey: "key", BaseURL: quotaServer.URL, Model: "reranker", HTTPClient: quotaServer.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reranker.Rerank(context.Background(), "query", []string{"doc"}); err == nil {
		t.Fatal("HTTP 402 should fail")
	}
	if quotaCalls.Load() != 1 {
		t.Fatalf("HTTP 402 attempts=%d, want 1", quotaCalls.Load())
	}
}

func TestOpenAICompatibleRemoteModelDoesNotRetryQuota429(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		http.Error(w, `{"error":{"message":"insufficient balance"}}`, http.StatusTooManyRequests)
	}))
	defer server.Close()
	encoder, err := NewOpenAICompatibleRetrievalEncoder(OpenAICompatibleModelOptions{
		APIKey: "key", BaseURL: server.URL, Model: "model", HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := encoder.Encode(context.Background(), []string{"text"}, RetrievalQuery); err == nil {
		t.Fatal("quota exhaustion should fail")
	}
	if calls.Load() != 1 {
		t.Fatalf("quota exhaustion attempts=%d, want 1", calls.Load())
	}
}

type retrievalRerankerStub struct {
	scores []float64
}

func (retrievalRerankerStub) Model() string    { return "stub" }
func (retrievalRerankerStub) Revision() string { return "stub-v1" }
func (s retrievalRerankerStub) Rerank(context.Context, string, []string) ([]float64, error) {
	return append([]float64(nil), s.scores...), nil
}

func TestRerankSidecarCandidatesReordersUnifiedCandidateSet(t *testing.T) {
	candidates := []*sidecarCandidate{
		{eventID: "a", content: "alpha", score: .8},
		{eventID: "b", content: "beta", score: .7},
		{eventID: "c", content: "gamma", score: .6},
	}
	result, err := rerankSidecarCandidates(context.Background(), retrievalRerankerStub{scores: []float64{.1, .9, .4}},
		"query", candidates)
	if err != nil {
		t.Fatal(err)
	}
	if result[0].eventID != "b" || result[1].eventID != "c" || result[2].eventID != "a" {
		t.Fatalf("unexpected reranked order: %s %s %s", result[0].eventID, result[1].eventID, result[2].eventID)
	}
	if result[0].rawScores["bge_before_rerank"] != .7 || result[0].rawScores["reranker"] != .9 {
		t.Fatalf("rerank diagnostics missing: %#v", result[0].rawScores)
	}
}
