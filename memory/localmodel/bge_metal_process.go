//go:build cgo

package localmodel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type bgeMetalClient struct {
	modelDir string
	binary   string

	mu     sync.Mutex
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	encode *json.Encoder
	decode *json.Decoder
	stderr bytes.Buffer
	nextID int
	closed bool
}

type bgeMetalRequest struct {
	ID            int       `json:"id"`
	InputIDs      [][]int32 `json:"input_ids"`
	AttentionMask [][]int32 `json:"attention_mask"`
	SpecialMask   [][]int32 `json:"special_mask"`
	IncludeMulti  bool      `json:"include_multi"`
}

type bgeMetalSparseToken struct {
	TokenID  int64   `json:"token_id"`
	Position int     `json:"position"`
	Weight   float32 `json:"weight"`
}

type bgeMetalTokenVector struct {
	TokenID  int64     `json:"token_id"`
	Position int       `json:"position"`
	Weight   float32   `json:"weight"`
	Values   []float32 `json:"values"`
}

type bgeMetalEmbedding struct {
	Dense  []float32             `json:"dense"`
	Sparse []bgeMetalSparseToken `json:"sparse"`
	Multi  []bgeMetalTokenVector `json:"multi"`
}

type bgeMetalResponse struct {
	ID         int                 `json:"id"`
	Embeddings []bgeMetalEmbedding `json:"embeddings"`
	Error      string              `json:"error"`
}

func bgeRuntimeProvider(modelDir string) string {
	content, err := os.ReadFile(filepath.Join(modelDir, "runtime", "provider"))
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(string(content)))
}

func newBGEMetalClient(modelDir string) (*bgeMetalClient, error) {
	binary := filepath.Join(modelDir, "runtime", "bin", "lumina-bge-metal")
	for _, path := range []string{
		binary,
		filepath.Join(modelDir, "metal", "config.json"),
		filepath.Join(modelDir, "metal", "model.safetensors"),
		filepath.Join(modelDir, "metal", "model.safetensors.index.json"),
		filepath.Join(modelDir, "runtime", "bin", "mlx-swift_Cmlx.bundle",
			"Contents", "Resources", "default.metallib"),
	} {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return nil, fmt.Errorf("BGE-M3 Metal asset missing: %s", path)
		}
	}
	return &bgeMetalClient{modelDir: modelDir, binary: binary}, nil
}

func (c *bgeMetalClient) Encode(ctx context.Context, inputs []bgeInput,
	includeMulti bool) ([]BGEEmbedding, error) {
	if c == nil {
		return nil, errors.New("BGE-M3 Metal runtime is not initialized")
	}
	request := prepareBGEMetalRequest(inputs, includeMulti)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("BGE-M3 Metal runtime is closed")
	}
	if err := c.startLocked(); err != nil {
		return nil, err
	}
	c.nextID++
	request.ID = c.nextID

	type roundTripResult struct {
		response bgeMetalResponse
		err      error
	}
	completed := make(chan roundTripResult, 1)
	go func() {
		var response bgeMetalResponse
		if err := c.encode.Encode(request); err != nil {
			completed <- roundTripResult{err: err}
			return
		}
		err := c.decode.Decode(&response)
		completed <- roundTripResult{response: response, err: err}
	}()

	select {
	case result := <-completed:
		if result.err != nil {
			stderr := c.stopLocked()
			return nil, fmt.Errorf("BGE-M3 Metal transport failed: %w%s", result.err, stderrSuffix(stderr))
		}
		if result.response.ID != request.ID {
			stderr := c.stopLocked()
			return nil, fmt.Errorf("BGE-M3 Metal response id %d, want %d%s",
				result.response.ID, request.ID, stderrSuffix(stderr))
		}
		if strings.TrimSpace(result.response.Error) != "" {
			return nil, fmt.Errorf("BGE-M3 Metal inference: %s", result.response.Error)
		}
		if len(result.response.Embeddings) != len(inputs) {
			return nil, fmt.Errorf("BGE-M3 Metal returned %d embeddings, want %d",
				len(result.response.Embeddings), len(inputs))
		}
		return convertBGEMetalEmbeddings(result.response.Embeddings), nil
	case <-ctx.Done():
		stderr := c.stopLocked()
		<-completed
		return nil, fmt.Errorf("%w%s", ctx.Err(), stderrSuffix(stderr))
	}
}

func (c *bgeMetalClient) startLocked() error {
	if c.cmd != nil {
		return nil
	}
	c.stderr.Reset()
	cmd := exec.Command(c.binary, "serve", c.modelDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return err
	}
	cmd.Stderr = &c.stderr
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("start BGE-M3 Metal runtime: %w", err)
	}
	c.cmd = cmd
	c.stdin = stdin
	c.encode = json.NewEncoder(stdin)
	c.decode = json.NewDecoder(stdout)
	return nil
}

func (c *bgeMetalClient) stopLocked() string {
	if c.cmd == nil {
		return strings.TrimSpace(c.stderr.String())
	}
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
	c.cmd = nil
	c.stdin = nil
	c.encode = nil
	c.decode = nil
	return strings.TrimSpace(c.stderr.String())
}

func (c *bgeMetalClient) Close() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	c.stopLocked()
	return nil
}

func prepareBGEMetalRequest(inputs []bgeInput, includeMulti bool) bgeMetalRequest {
	maxTokens := 0
	for _, input := range inputs {
		if len(input.ids) > maxTokens {
			maxTokens = len(input.ids)
		}
	}
	request := bgeMetalRequest{
		InputIDs:      make([][]int32, len(inputs)),
		AttentionMask: make([][]int32, len(inputs)),
		SpecialMask:   make([][]int32, len(inputs)),
		IncludeMulti:  includeMulti,
	}
	for index, input := range inputs {
		request.InputIDs[index] = make([]int32, maxTokens)
		request.AttentionMask[index] = make([]int32, maxTokens)
		request.SpecialMask[index] = make([]int32, maxTokens)
		for token := range input.ids {
			request.InputIDs[index][token] = int32(input.ids[token])
			request.AttentionMask[index][token] = int32(input.attention[token])
			request.SpecialMask[index][token] = int32(input.specialMask[token])
		}
	}
	return request
}

func convertBGEMetalEmbeddings(values []bgeMetalEmbedding) []BGEEmbedding {
	result := make([]BGEEmbedding, len(values))
	for index, value := range values {
		sparse := make(map[int64]float32, len(value.Sparse))
		for _, token := range value.Sparse {
			if token.Weight > sparse[token.TokenID] {
				sparse[token.TokenID] = token.Weight
			}
		}
		multi := make([]BGETokenVector, len(value.Multi))
		for tokenIndex, token := range value.Multi {
			multi[tokenIndex] = BGETokenVector{
				TokenID:  token.TokenID,
				Position: token.Position,
				Weight:   token.Weight,
				Values:   token.Values,
			}
		}
		result[index] = BGEEmbedding{Dense: value.Dense, Sparse: sparse, Multi: multi}
	}
	return result
}

func stderrSuffix(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	return ": " + value
}
