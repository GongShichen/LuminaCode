//go:build cgo

package longmemory

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
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
	modelName   string
	modelDir    string
	tokenizer   *tokenizer.Tokenizer
	session     *ort.DynamicAdvancedSession
	provider    string
	diagnostics []string
	mu          sync.Mutex
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
	session, provider, diagnostics, err := newEmbeddingSession(modelPath, modelDir)
	if err != nil {
		return nil, fmt.Errorf("load memory embedding model: %w", err)
	}
	return &LocalEmbedder{modelName: modelName, modelDir: modelDir, tokenizer: tok, session: session,
		provider: provider, diagnostics: diagnostics}, nil
}

func (e *LocalEmbedder) Model() string { return e.modelName }

func (e *LocalEmbedder) Dimensions() int { return 384 }

func (e *LocalEmbedder) Provider() string { return e.provider }

func (e *LocalEmbedder) ProviderDiagnostics() []string {
	return append([]string(nil), e.diagnostics...)
}

func newEmbeddingSession(modelPath, modelDir string) (*ort.DynamicAdvancedSession, string, []string, error) {
	providers, diagnostics := embeddingProviderCandidates(modelDir)
	var lastErr error
	for _, provider := range providers {
		options, err := ort.NewSessionOptions()
		if err != nil {
			return nil, "", diagnostics, err
		}
		if err = configureEmbeddingSessionOptions(options, provider); err == nil {
			var session *ort.DynamicAdvancedSession
			session, err = ort.NewDynamicAdvancedSession(modelPath,
				[]string{"input_ids", "attention_mask", "token_type_ids"}, []string{"last_hidden_state"}, options)
			if err == nil {
				_ = options.Destroy()
				return session, provider, diagnostics, nil
			}
		}
		_ = options.Destroy()
		lastErr = err
		diagnostics = append(diagnostics, provider+": "+err.Error())
	}
	return nil, "", diagnostics, lastErr
}

func embeddingProviderCandidates(modelDir string) ([]string, []string) {
	requested := strings.ToLower(strings.TrimSpace(os.Getenv("LUMINA_MEMORY_EMBEDDING_DEVICE")))
	switch requested {
	case "mps", "metal":
		requested = "coreml"
	case "amd":
		requested = "rocm"
	}
	cudaAvailable := fileExists("/dev/nvidiactl") || commandExists("nvidia-smi") ||
		strings.TrimSpace(os.Getenv("CUDA_PATH")) != "" || strings.TrimSpace(os.Getenv("NVIDIA_VISIBLE_DEVICES")) != "" ||
		strings.TrimSpace(os.Getenv("CUDA_VISIBLE_DEVICES")) != ""
	rocmAvailable := fileExists("/dev/kfd") || commandExists("rocminfo") || strings.TrimSpace(os.Getenv("ROCR_VISIBLE_DEVICES")) != ""
	customRuntime := strings.TrimSpace(os.Getenv("LUMINA_ONNXRUNTIME_PATH")) != ""
	installedProvider := ""
	if content, err := os.ReadFile(filepath.Join(modelDir, "runtime", "provider")); err == nil {
		installedProvider = strings.ToLower(strings.TrimSpace(string(content)))
	}
	providers := embeddingProviderCandidatesFor(requested, runtime.GOOS, installedProvider, customRuntime, cudaAvailable, rocmAvailable)
	diagnostics := make([]string, 0, len(providers))
	if (requested == "" || requested == "auto") && installedProvider == "cpu" && (cudaAvailable || rocmAvailable) {
		diagnostics = append(diagnostics, "accelerator hardware detected, but the installed ONNX Runtime only provides CPU execution")
	}
	return providers, diagnostics
}

func embeddingProviderCandidatesFor(requested, goos, installedProvider string, customRuntime, cudaAvailable, rocmAvailable bool) []string {
	if requested != "" && requested != "auto" {
		if requested == "cpu" {
			return []string{"cpu"}
		}
		return []string{requested, "cpu"}
	}
	// Bundled runtimes contain a known provider set. Trust the installer marker
	// instead of attempting a provider that the shared library cannot expose.
	if !customRuntime && installedProvider != "" {
		if installedProvider == "cpu" {
			return []string{"cpu"}
		}
		return []string{installedProvider, "cpu"}
	}
	providers := make([]string, 0, 3)
	switch goos {
	case "darwin":
		providers = append(providers, "coreml")
	case "windows":
		if cudaAvailable {
			providers = append(providers, "cuda")
		} else {
			providers = append(providers, "directml")
		}
	default:
		if cudaAvailable {
			providers = append(providers, "cuda")
		}
		if rocmAvailable {
			providers = append(providers, "rocm")
		}
	}
	return append(providers, "cpu")
}

func configureEmbeddingSessionOptions(options *ort.SessionOptions, provider string) error {
	if err := options.SetGraphOptimizationLevel(ort.GraphOptimizationLevelEnableAll); err != nil {
		return err
	}
	switch provider {
	case "cuda":
		cudaOptions, err := ort.NewCUDAProviderOptions()
		if err != nil {
			return err
		}
		defer cudaOptions.Destroy()
		if err := cudaOptions.Update(map[string]string{"device_id": "0"}); err != nil {
			return err
		}
		return options.AppendExecutionProviderCUDA(cudaOptions)
	case "coreml":
		return options.AppendExecutionProviderCoreMLV2(map[string]string{"MLComputeUnits": "ALL"})
	case "rocm":
		return options.AppendExecutionProvider("ROCMExecutionProvider", map[string]string{"device_id": "0"})
	case "directml":
		return options.AppendExecutionProviderDirectML(0)
	case "cpu":
		threads := minInt(runtime.NumCPU(), 16)
		if configured, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LUMINA_MEMORY_EMBEDDING_THREADS"))); err == nil && configured > 0 {
			threads = configured
		}
		if err := options.SetIntraOpNumThreads(threads); err != nil {
			return err
		}
		return options.SetInterOpNumThreads(1)
	default:
		return fmt.Errorf("unsupported memory embedding device %q", provider)
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

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
	inputs := make([]embeddingInput, 0, len(texts))
	maxTokens := 0
	for _, text := range texts {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		input, err := e.prepareEmbeddingInput(strings.TrimSpace(text), kind)
		if err != nil {
			return nil, err
		}
		inputs = append(inputs, input)
		if len(input.ids) > maxTokens {
			maxTokens = len(input.ids)
		}
	}
	return e.embedBatch(inputs, maxTokens)
}

type embeddingInput struct {
	ids       []int64
	attention []int64
	typeIDs   []int64
}

func (e *LocalEmbedder) prepareEmbeddingInput(text string, kind EmbeddingKind) (embeddingInput, error) {
	prefix := "passage: "
	if kind == EmbeddingQuery {
		prefix = "query: "
	}
	encoding, err := e.tokenizer.EncodeSingle(prefix+text, true)
	if err != nil {
		return embeddingInput{}, fmt.Errorf("tokenize memory text: %w", err)
	}
	ids := encoding.GetIds()
	attention := encoding.GetAttentionMask()
	typeIDs := encoding.GetTypeIds()
	if len(ids) == 0 {
		return embeddingInput{}, errors.New("memory tokenizer returned no tokens")
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
	return embeddingInput{ids: intsToInt64(ids), attention: intsToInt64(attention), typeIDs: intsToInt64(typeIDs)}, nil
}

func (e *LocalEmbedder) embedBatch(inputs []embeddingInput, maxTokens int) ([][]float32, error) {
	if len(inputs) == 0 || maxTokens <= 0 {
		return nil, nil
	}
	batchSize := len(inputs)
	inputIDs := make([]int64, batchSize*maxTokens)
	attentionMask := make([]int64, batchSize*maxTokens)
	tokenTypeIDs := make([]int64, batchSize*maxTokens)
	for batchIndex, input := range inputs {
		offset := batchIndex * maxTokens
		copy(inputIDs[offset:offset+len(input.ids)], input.ids)
		copy(attentionMask[offset:offset+len(input.attention)], input.attention)
		copy(tokenTypeIDs[offset:offset+len(input.typeIDs)], input.typeIDs)
	}
	shape := ort.NewShape(int64(batchSize), int64(maxTokens))
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
	if len(outputShape) != 3 || int(outputShape[0]) != batchSize || int(outputShape[1]) != maxTokens || outputShape[2] <= 0 {
		return nil, fmt.Errorf("unexpected memory embedding shape %v", outputShape)
	}
	dimensions := int(outputShape[2])
	data := output.GetData()
	result := make([][]float32, batchSize)
	for batchIndex := range inputs {
		pooled := make([]float32, dimensions)
		var weight float32
		maskOffset := batchIndex * maxTokens
		outputOffset := batchIndex * maxTokens * dimensions
		for tokenIndex := 0; tokenIndex < maxTokens; tokenIndex++ {
			if attentionMask[maskOffset+tokenIndex] == 0 {
				continue
			}
			weight++
			tokenOffset := outputOffset + tokenIndex*dimensions
			for dimension := 0; dimension < dimensions; dimension++ {
				pooled[dimension] += data[tokenOffset+dimension]
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
		result[batchIndex] = pooled
	}
	return result, nil
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
