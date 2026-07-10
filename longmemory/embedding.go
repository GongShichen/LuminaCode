//go:build cgo

package longmemory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/sugarme/tokenizer"
	"github.com/sugarme/tokenizer/pretrained"
	ort "github.com/yalue/onnxruntime_go"
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

type LocalEmbedder struct {
	modelName string
	modelDir  string
	tokenizer *tokenizer.Tokenizer
	session   *ort.DynamicAdvancedSession
	mu        sync.Mutex
}

var embeddingRuntime struct {
	sync.Mutex
	initialized bool
	path        string
}

var embeddingInstances sync.Map

type embeddingInstance struct {
	once     sync.Once
	embedder *LocalEmbedder
	err      error
}

func SharedLocalEmbedder(modelName, modelDir string) (*LocalEmbedder, error) {
	key := strings.TrimSpace(modelName) + "\x00" + filepath.Clean(ExpandPath(modelDir))
	raw, _ := embeddingInstances.LoadOrStore(key, &embeddingInstance{})
	instance := raw.(*embeddingInstance)
	instance.once.Do(func() {
		instance.embedder, instance.err = NewLocalEmbedder(modelName, modelDir)
	})
	return instance.embedder, instance.err
}

func NewLocalEmbedder(modelName, modelDir string) (*LocalEmbedder, error) {
	modelName = strings.TrimSpace(modelName)
	if modelName == "" {
		modelName = "multilingual-e5-small"
	}
	modelDir = ExpandPath(modelDir)
	if modelDir == "" {
		modelDir = filepath.Join(filepath.Dir(DefaultStorePath()), "..", "models", "memory", modelName)
	}
	modelDir = filepath.Clean(modelDir)
	modelPath := filepath.Join(modelDir, "model.onnx")
	tokenizerPath := filepath.Join(modelDir, "tokenizer.json")
	for _, path := range []string{modelPath, tokenizerPath} {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			return nil, fmt.Errorf("memory embedding asset missing: %s", path)
		}
	}
	if err := initializeEmbeddingRuntime(modelDir); err != nil {
		return nil, err
	}
	tok, err := pretrained.FromFile(tokenizerPath)
	if err != nil {
		return nil, fmt.Errorf("load memory tokenizer: %w", err)
	}
	session, err := ort.NewDynamicAdvancedSession(modelPath,
		[]string{"input_ids", "attention_mask", "token_type_ids"}, []string{"last_hidden_state"}, nil)
	if err != nil {
		return nil, fmt.Errorf("load memory embedding model: %w", err)
	}
	return &LocalEmbedder{modelName: modelName, modelDir: modelDir, tokenizer: tok, session: session}, nil
}

func (e *LocalEmbedder) Model() string { return e.modelName }

func (e *LocalEmbedder) Dimensions() int { return 384 }

func (e *LocalEmbedder) Close() error {
	if e == nil || e.session == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	err := e.session.Destroy()
	e.session = nil
	return err
}

func (e *LocalEmbedder) Embed(ctx context.Context, texts []string, kind EmbeddingKind) ([][]float32, error) {
	if e == nil || e.session == nil || e.tokenizer == nil {
		return nil, errors.New("memory embedder is not initialized")
	}
	if len(texts) == 0 {
		return nil, nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	result := make([][]float32, 0, len(texts))
	for _, text := range texts {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		vector, err := e.embedOne(strings.TrimSpace(text), kind)
		if err != nil {
			return nil, err
		}
		result = append(result, vector)
	}
	return result, nil
}

func (e *LocalEmbedder) embedOne(text string, kind EmbeddingKind) ([]float32, error) {
	prefix := "passage: "
	if kind == EmbeddingQuery {
		prefix = "query: "
	}
	encoding, err := e.tokenizer.EncodeSingle(prefix+text, true)
	if err != nil {
		return nil, fmt.Errorf("tokenize memory text: %w", err)
	}
	ids := encoding.GetIds()
	attention := encoding.GetAttentionMask()
	typeIDs := encoding.GetTypeIds()
	if len(ids) == 0 {
		return nil, errors.New("memory tokenizer returned no tokens")
	}
	if len(ids) > 512 {
		ids = ids[:512]
	}
	if len(attention) < len(ids) {
		attention = make([]int, len(ids))
		for idx := range attention {
			attention[idx] = 1
		}
	} else {
		attention = attention[:len(ids)]
	}
	if len(typeIDs) < len(ids) {
		typeIDs = make([]int, len(ids))
	} else {
		typeIDs = typeIDs[:len(ids)]
	}
	inputIDs := intsToInt64(ids)
	attentionMask := intsToInt64(attention)
	tokenTypeIDs := intsToInt64(typeIDs)
	shape := ort.NewShape(1, int64(len(ids)))
	idsTensor, err := ort.NewTensor(shape, inputIDs)
	if err != nil {
		return nil, err
	}
	defer idsTensor.Destroy()
	attentionTensor, err := ort.NewTensor(shape, attentionMask)
	if err != nil {
		return nil, err
	}
	defer attentionTensor.Destroy()
	typeTensor, err := ort.NewTensor(shape, tokenTypeIDs)
	if err != nil {
		return nil, err
	}
	defer typeTensor.Destroy()
	outputs := []ort.Value{nil}
	if err := e.session.Run([]ort.Value{idsTensor, attentionTensor, typeTensor}, outputs); err != nil {
		return nil, fmt.Errorf("run memory embedding model: %w", err)
	}
	if outputs[0] == nil {
		return nil, errors.New("memory embedding model returned no output")
	}
	defer outputs[0].Destroy()
	output, ok := outputs[0].(*ort.Tensor[float32])
	if !ok {
		return nil, fmt.Errorf("unexpected memory embedding output %T", outputs[0])
	}
	outputShape := output.GetShape()
	if len(outputShape) != 3 || int(outputShape[1]) != len(ids) || outputShape[2] <= 0 {
		return nil, fmt.Errorf("unexpected memory embedding shape %v", outputShape)
	}
	dimensions := int(outputShape[2])
	pooled := make([]float32, dimensions)
	data := output.GetData()
	var weight float32
	for tokenIndex, active := range attentionMask {
		if active == 0 {
			continue
		}
		weight++
		offset := tokenIndex * dimensions
		for dimension := 0; dimension < dimensions; dimension++ {
			pooled[dimension] += data[offset+dimension]
		}
	}
	if weight == 0 {
		return nil, errors.New("memory embedding attention mask is empty")
	}
	var norm float64
	for idx := range pooled {
		pooled[idx] /= weight
		norm += float64(pooled[idx] * pooled[idx])
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return nil, errors.New("memory embedding has zero norm")
	}
	for idx := range pooled {
		pooled[idx] = float32(float64(pooled[idx]) / norm)
	}
	return pooled, nil
}

func initializeEmbeddingRuntime(modelDir string) error {
	embeddingRuntime.Lock()
	defer embeddingRuntime.Unlock()
	if embeddingRuntime.initialized || ort.IsInitialized() {
		embeddingRuntime.initialized = true
		return nil
	}
	runtimePath, err := findEmbeddingRuntime(modelDir)
	if err != nil {
		return err
	}
	ort.SetSharedLibraryPath(runtimePath)
	if err := ort.InitializeEnvironment(ort.WithLogLevelError()); err != nil {
		return fmt.Errorf("initialize ONNX Runtime for memory embeddings: %w", err)
	}
	embeddingRuntime.initialized = true
	embeddingRuntime.path = runtimePath
	return nil
}

func findEmbeddingRuntime(modelDir string) (string, error) {
	if configured := strings.TrimSpace(os.Getenv("LUMINA_ONNXRUNTIME_PATH")); configured != "" {
		configured = ExpandPath(configured)
		if info, err := os.Stat(configured); err == nil && !info.IsDir() {
			return configured, nil
		}
		return "", fmt.Errorf("LUMINA_ONNXRUNTIME_PATH does not point to a file: %s", configured)
	}
	var names []string
	switch runtime.GOOS {
	case "darwin":
		names = []string{"libonnxruntime.dylib"}
	case "windows":
		names = []string{"onnxruntime.dll"}
	default:
		names = []string{"libonnxruntime.so", "libonnxruntime.so.1"}
	}
	for _, directory := range []string{modelDir, filepath.Join(modelDir, "runtime"), filepath.Join(modelDir, "runtime", "lib"), filepath.Join(modelDir, "lib")} {
		for _, name := range names {
			path := filepath.Join(directory, name)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("ONNX Runtime library not found under %s; run make install or make doctor", modelDir)
}

func intsToInt64(values []int) []int64 {
	result := make([]int64, len(values))
	for idx, value := range values {
		result[idx] = int64(value)
	}
	return result
}
