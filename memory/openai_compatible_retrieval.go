package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	openAICompatibleBGEDimensions = 1024
	remoteRetrievalMaxBodyBytes   = 8 << 20
	remoteRetrievalAttempts       = 3
	remoteSplitterRunesPerToken   = 3
)

type OpenAICompatibleModelOptions struct {
	APIKey     string
	BaseURL    string
	Model      string
	Timeout    time.Duration
	HTTPClient *http.Client
}

type OpenAICompatibleRetrievalEncoder struct {
	apiKey        string
	endpoint      string
	model         string
	revision      string
	tokenizerHash string
	client        *http.Client
}

func NewOpenAICompatibleRetrievalEncoder(options OpenAICompatibleModelOptions) (*OpenAICompatibleRetrievalEncoder, error) {
	options, endpoint, err := normalizeRemoteModelOptions(options, "embeddings")
	if err != nil {
		return nil, err
	}
	return &OpenAICompatibleRetrievalEncoder{
		apiKey: options.APIKey, endpoint: endpoint, model: options.Model,
		revision: remoteModelIdentity("bge-m3-dense-v1", options.BaseURL, options.Model),
		tokenizerHash: remoteModelIdentity("unicode-rune-splitter-v1",
			fmt.Sprintf("%d", remoteSplitterRunesPerToken), ""),
		client: remoteModelHTTPClient(options),
	}, nil
}

func (e *OpenAICompatibleRetrievalEncoder) Model() string { return e.model }

func (e *OpenAICompatibleRetrievalEncoder) Revision() string { return e.revision }

func (e *OpenAICompatibleRetrievalEncoder) TokenizerHash() string { return e.tokenizerHash }

func (e *OpenAICompatibleRetrievalEncoder) Encode(ctx context.Context, texts []string,
	_ RetrievalEncodingKind) ([]RetrievalEncoding, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	request := struct {
		Model          string   `json:"model"`
		Input          []string `json:"input"`
		EncodingFormat string   `json:"encoding_format"`
	}{Model: e.model, Input: texts, EncodingFormat: "float"}
	var response struct {
		Data []struct {
			Embedding []float32 `json:"embedding"`
			Index     *int      `json:"index"`
		} `json:"data"`
	}
	if err := postOpenAICompatibleJSON(ctx, e.client, e.endpoint, e.apiKey, request, &response); err != nil {
		return nil, fmt.Errorf("remote BGE-M3 embeddings: %w", err)
	}
	if len(response.Data) != len(texts) {
		return nil, fmt.Errorf("remote BGE-M3 returned %d embeddings, want %d", len(response.Data), len(texts))
	}
	result := make([]RetrievalEncoding, len(texts))
	seen := make([]bool, len(texts))
	for position, item := range response.Data {
		index := position
		if item.Index != nil {
			index = *item.Index
		}
		if index < 0 || index >= len(result) || seen[index] {
			return nil, fmt.Errorf("remote BGE-M3 returned invalid embedding index %d", index)
		}
		if len(item.Embedding) != openAICompatibleBGEDimensions {
			return nil, fmt.Errorf("remote BGE-M3 embedding %d has %d dimensions, want %d",
				index, len(item.Embedding), openAICompatibleBGEDimensions)
		}
		if err := normalizeRemoteEmbedding(item.Embedding); err != nil {
			return nil, fmt.Errorf("remote BGE-M3 embedding %d: %w", index, err)
		}
		result[index] = RetrievalEncoding{Dense: item.Embedding, Sparse: map[int64]float32{}}
		seen[index] = true
	}
	return result, nil
}

func (e *OpenAICompatibleRetrievalEncoder) EncodeChannels(ctx context.Context, texts []string,
	kind RetrievalEncodingKind) ([]RetrievalEncoding, error) {
	return e.Encode(ctx, texts, kind)
}

func (*OpenAICompatibleRetrievalEncoder) Split(text string, maxTokens, overlap int) ([]string, error) {
	if maxTokens <= 0 {
		return nil, errors.New("remote BGE-M3 split maxTokens must be positive")
	}
	if overlap < 0 || overlap >= maxTokens {
		return nil, errors.New("remote BGE-M3 split overlap must be non-negative and less than maxTokens")
	}
	runes := []rune(strings.TrimSpace(text))
	if len(runes) == 0 {
		return nil, nil
	}
	window := maxTokens * remoteSplitterRunesPerToken
	overlapRunes := overlap * remoteSplitterRunesPerToken
	if len(runes) <= window {
		return []string{string(runes)}, nil
	}
	step := window - overlapRunes
	result := make([]string, 0, 1+len(runes)/step)
	for start := 0; start < len(runes); start += step {
		end := start + window
		if end > len(runes) {
			end = len(runes)
		}
		chunk := strings.TrimSpace(string(runes[start:end]))
		if chunk != "" {
			result = append(result, chunk)
		}
		if end == len(runes) {
			break
		}
	}
	return result, nil
}

func (e *OpenAICompatibleRetrievalEncoder) SplitMany(text string,
	specs []RetrievalSplitSpec) ([][]string, error) {
	result := make([][]string, len(specs))
	for index, spec := range specs {
		chunks, err := e.Split(text, spec.MaxTokens, spec.Overlap)
		if err != nil {
			return nil, err
		}
		result[index] = chunks
	}
	return result, nil
}

type OpenAICompatibleReranker struct {
	apiKey   string
	endpoint string
	model    string
	revision string
	client   *http.Client
}

func NewOpenAICompatibleReranker(options OpenAICompatibleModelOptions) (*OpenAICompatibleReranker, error) {
	options, endpoint, err := normalizeRemoteModelOptions(options, "rerank")
	if err != nil {
		return nil, err
	}
	return &OpenAICompatibleReranker{apiKey: options.APIKey, endpoint: endpoint, model: options.Model,
		revision: remoteModelIdentity("rerank-v1", options.BaseURL, options.Model),
		client:   remoteModelHTTPClient(options)}, nil
}

func (r *OpenAICompatibleReranker) Model() string { return r.model }

func (r *OpenAICompatibleReranker) Revision() string { return r.revision }

func (r *OpenAICompatibleReranker) Rerank(ctx context.Context, query string,
	documents []string) ([]float64, error) {
	if len(documents) == 0 {
		return nil, nil
	}
	request := struct {
		Model           string   `json:"model"`
		Query           string   `json:"query"`
		Documents       []string `json:"documents"`
		TopN            int      `json:"top_n"`
		ReturnDocuments bool     `json:"return_documents"`
	}{Model: r.model, Query: query, Documents: documents, TopN: len(documents)}
	type rerankItem struct {
		Index          int      `json:"index"`
		RelevanceScore *float64 `json:"relevance_score"`
		Score          *float64 `json:"score"`
	}
	var response struct {
		Results []rerankItem `json:"results"`
		Data    []rerankItem `json:"data"`
	}
	if err := postOpenAICompatibleJSON(ctx, r.client, r.endpoint, r.apiKey, request, &response); err != nil {
		return nil, fmt.Errorf("remote reranker: %w", err)
	}
	items := response.Results
	if len(items) == 0 {
		items = response.Data
	}
	if len(items) != len(documents) {
		return nil, fmt.Errorf("remote reranker returned %d scores, want %d", len(items), len(documents))
	}
	scores := make([]float64, len(documents))
	seen := make([]bool, len(documents))
	for _, item := range items {
		if item.Index < 0 || item.Index >= len(documents) || seen[item.Index] {
			return nil, fmt.Errorf("remote reranker returned invalid document index %d", item.Index)
		}
		var score float64
		switch {
		case item.RelevanceScore != nil:
			score = *item.RelevanceScore
		case item.Score != nil:
			score = *item.Score
		default:
			return nil, fmt.Errorf("remote reranker omitted score for document %d", item.Index)
		}
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return nil, fmt.Errorf("remote reranker returned non-finite score for document %d", item.Index)
		}
		scores[item.Index] = score
		seen[item.Index] = true
	}
	return scores, nil
}

func normalizeRemoteModelOptions(options OpenAICompatibleModelOptions, operation string) (
	OpenAICompatibleModelOptions, string, error) {
	options.APIKey = strings.TrimSpace(options.APIKey)
	options.BaseURL = strings.TrimSpace(options.BaseURL)
	options.Model = strings.TrimSpace(options.Model)
	if options.APIKey == "" || options.BaseURL == "" || options.Model == "" {
		return options, "", errors.New("API key, base URL, and model are required")
	}
	endpoint, err := openAICompatibleModelEndpoint(options.BaseURL, operation)
	if err != nil {
		return options, "", err
	}
	return options, endpoint, nil
}

func remoteModelHTTPClient(options OpenAICompatibleModelOptions) *http.Client {
	if options.HTTPClient != nil {
		return options.HTTPClient
	}
	timeout := options.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &http.Client{Timeout: timeout}
}

func openAICompatibleModelEndpoint(baseURL, operation string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", fmt.Errorf("invalid OpenAI-compatible base URL %q", baseURL)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", fmt.Errorf("OpenAI-compatible base URL must use http or https")
	}
	path := strings.TrimRight(parsed.Path, "/")
	if operation == "rerank" && strings.HasSuffix(path, "/compatible-mode/v1") {
		parsed.Path = strings.TrimSuffix(path, "/compatible-mode/v1") + "/compatible-api/v1/reranks"
	} else if operation == "rerank" && strings.HasSuffix(path, "/compatible-api/v1") {
		parsed.Path = path + "/reranks"
	} else if strings.HasSuffix(path, "/"+operation) ||
		(operation == "rerank" && strings.HasSuffix(path, "/reranks")) {
		parsed.Path = path
	} else if path == "" {
		parsed.Path = "/v1/" + operation
	} else {
		parsed.Path = path + "/" + operation
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func remoteModelIdentity(kind, baseURL, model string) string {
	hash := sha256.Sum256([]byte(kind + "\x00" + strings.TrimRight(strings.TrimSpace(baseURL), "/") +
		"\x00" + strings.TrimSpace(model)))
	return "openai-compatible:" + kind + "@" + hex.EncodeToString(hash[:12])
}

func normalizeRemoteEmbedding(vector []float32) error {
	var norm float64
	for _, value := range vector {
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return errors.New("embedding contains a non-finite value")
		}
		norm += float64(value * value)
	}
	if norm = math.Sqrt(norm); norm == 0 {
		return errors.New("embedding has zero norm")
	}
	for index := range vector {
		vector[index] /= float32(norm)
	}
	return nil
}

func postOpenAICompatibleJSON(ctx context.Context, client *http.Client, endpoint, apiKey string,
	requestBody, responseBody any) error {
	payload, err := json.Marshal(requestBody)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < remoteRetrievalAttempts; attempt++ {
		req, reqErr := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
		if reqErr != nil {
			return reqErr
		}
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("Content-Type", "application/json")
		resp, doErr := client.Do(req)
		if doErr != nil {
			if attempt+1 < remoteRetrievalAttempts && ctx.Err() == nil {
				if err := waitRemoteModelRetry(ctx, attempt); err != nil {
					return err
				}
				continue
			}
			return doErr
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, remoteRetrievalMaxBodyBytes))
		_ = resp.Body.Close()
		if readErr != nil {
			return readErr
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if err := json.Unmarshal(body, responseBody); err != nil {
				return fmt.Errorf("decode response: %w", err)
			}
			return nil
		}
		statusErr := fmt.Errorf("HTTP %d: %s", resp.StatusCode,
			strings.TrimSpace(string(body[:minIntMemory(len(body), 4096)])))
		if remoteModelQuotaFailure(resp.StatusCode, body) {
			return statusErr
		}
		if (resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500) &&
			attempt+1 < remoteRetrievalAttempts {
			if err := waitRemoteModelRetry(ctx, attempt); err != nil {
				return err
			}
			continue
		}
		return statusErr
	}
	return errors.New("remote model retry budget exhausted")
}

func remoteModelQuotaFailure(statusCode int, body []byte) bool {
	if statusCode == http.StatusPaymentRequired {
		return true
	}
	if statusCode != http.StatusTooManyRequests {
		return false
	}
	message := strings.ToLower(string(body))
	for _, marker := range []string{"quota exhausted", "insufficient balance", "payment required"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func waitRemoteModelRetry(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(attempt+1) * 200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
