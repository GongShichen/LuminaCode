//go:build cgo

package localmodel

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"

	"github.com/sugarme/tokenizer"
	"github.com/sugarme/tokenizer/pretrained"
	ort "github.com/yalue/onnxruntime_go"
	"golang.org/x/sync/semaphore"
)

const (
	bgeDimensions           = BGEEmbeddingDimensions
	bgeMaximumTokens        = 8192
	bgeInferenceBatchLimit  = 64
	bgeInferenceTokenBudget = 8192
	bgeTokenizerSHA256      = "6710678b12670bc442b99edc952c4d996ae309a7020c1fa0096dd245c2faf790"
)

type LocalBGEEncoder struct {
	modelDir         string
	tokenizer        *tokenizer.Tokenizer
	metal            *bgeMetalScheduler
	sessions         []*ort.DynamicAdvancedSession
	available        chan *ort.DynamicAdvancedSession
	colbertWeight    []float32
	colbertBias      []float32
	sparseWeight     []float32
	sparseBias       float32
	revision         string
	provider         string
	inferenceGate    *semaphore.Weighted
	inferenceWork    int64
	tokenizers       chan *tokenizer.Tokenizer
	tokenizerWorkers int
	lifecycleMu      sync.RWMutex
	closed           bool
}

var bgeInstances sync.Map

type bgeInstance struct {
	once    sync.Once
	encoder *LocalBGEEncoder
	err     error
}

func SharedLocalBGEEncoder(modelDir string) (*LocalBGEEncoder, error) {
	modelDir = filepath.Clean(ExpandPath(modelDir))
	raw, _ := bgeInstances.LoadOrStore(modelDir, &bgeInstance{})
	instance := raw.(*bgeInstance)
	instance.once.Do(func() { instance.encoder, instance.err = NewLocalBGEEncoder(modelDir) })
	return instance.encoder, instance.err
}

func NewLocalBGEEncoder(modelDir string) (*LocalBGEEncoder, error) {
	modelDir = filepath.Clean(ExpandPath(modelDir))
	modelPath := filepath.Join(modelDir, "onnx", "model.onnx")
	tokenizerPath := filepath.Join(modelDir, "onnx", "tokenizer.json")
	for _, path := range []string{tokenizerPath} {
		if info, err := os.Stat(path); err != nil || info.IsDir() {
			return nil, fmt.Errorf("BGE-M3 asset missing: %s", path)
		}
	}
	if err := VerifyBGEHeads(modelDir); err != nil {
		return nil, fmt.Errorf("verify BGE-M3 heads: %w", err)
	}
	runtimeProvider := bgeRuntimeProvider(modelDir)
	if runtimeProvider != "metal" {
		if err := initializeBGERuntime(modelDir); err != nil {
			return nil, err
		}
	}
	tok, err := pretrained.FromFile(tokenizerPath)
	if err != nil {
		return nil, fmt.Errorf("load BGE-M3 tokenizer: %w", err)
	}
	if runtimeProvider == "metal" {
		tokenizers, tokenizerWorkers, poolErr := newBGETokenizerPool(tokenizerPath, tok, runtimeProvider)
		if poolErr != nil {
			return nil, poolErr
		}
		client, clientErr := newBGEMetalClient(modelDir)
		if clientErr != nil {
			return nil, clientErr
		}
		return &LocalBGEEncoder{
			modelDir:         modelDir,
			tokenizer:        tok,
			tokenizers:       tokenizers,
			tokenizerWorkers: tokenizerWorkers,
			metal:            newBGEMetalScheduler(client),
			revision:         bgeModelIdentity(modelDir),
			provider:         runtimeProvider,
		}, nil
	}
	if info, statErr := os.Stat(modelPath); statErr != nil || info.IsDir() {
		return nil, fmt.Errorf("BGE-M3 asset missing: %s", modelPath)
	}
	colbertWeight, err := readFP16File(filepath.Join(modelDir, "heads", "colbert.weight.fp16"), bgeDimensions*bgeDimensions)
	if err != nil {
		return nil, err
	}
	colbertBias, err := readFP16File(filepath.Join(modelDir, "heads", "colbert.bias.fp16"), bgeDimensions)
	if err != nil {
		return nil, err
	}
	sparseWeight, err := readFP16File(filepath.Join(modelDir, "heads", "sparse.weight.fp16"), bgeDimensions)
	if err != nil {
		return nil, err
	}
	sparseBias, err := readFP16File(filepath.Join(modelDir, "heads", "sparse.bias.fp16"), 1)
	if err != nil {
		return nil, err
	}
	provider, firstSession, err := selectBGEExecutionProvider(modelDir, modelPath, tok,
		colbertWeight, colbertBias, sparseWeight, sparseBias[0])
	if err != nil {
		return nil, err
	}
	sessionCount := bgeSessionPoolSizeForProvider(provider)
	sessions := make([]*ort.DynamicAdvancedSession, 0, sessionCount)
	sessions = append(sessions, firstSession)
	for index := 1; index < sessionCount; index++ {
		session, sessionErr := newLocalBGESession(modelPath, bgeSessionThreadBudget(sessionCount), provider)
		if sessionErr != nil {
			for _, opened := range sessions {
				_ = opened.Destroy()
			}
			return nil, sessionErr
		}
		sessions = append(sessions, session)
	}
	tokenizers, tokenizerWorkers, err := newBGETokenizerPool(tokenizerPath, tok, provider)
	if err != nil {
		for _, opened := range sessions {
			_ = opened.Destroy()
		}
		return nil, err
	}
	available := make(chan *ort.DynamicAdvancedSession, len(sessions))
	for _, session := range sessions {
		available <- session
	}
	inferenceWork := bgeInferenceWorkCapacity(provider, sessionCount)
	return &LocalBGEEncoder{modelDir: modelDir, tokenizer: tok, tokenizers: tokenizers,
		tokenizerWorkers: tokenizerWorkers, sessions: sessions, available: available,
		colbertWeight: colbertWeight, colbertBias: colbertBias,
		sparseWeight: sparseWeight, sparseBias: sparseBias[0],
		revision: bgeModelIdentity(modelDir), provider: provider,
		inferenceGate: semaphore.NewWeighted(inferenceWork), inferenceWork: inferenceWork}, nil
}

func newBGETokenizerPool(tokenizerPath string, first *tokenizer.Tokenizer, provider string) (
	chan *tokenizer.Tokenizer, int, error) {
	workers := bgeTokenizerWorkerCount(provider)
	pool := make(chan *tokenizer.Tokenizer, workers)
	pool <- first
	for worker := 1; worker < workers; worker++ {
		tok, err := pretrained.FromFile(tokenizerPath)
		if err != nil {
			return nil, 0, fmt.Errorf("load BGE-M3 tokenizer worker %d: %w", worker+1, err)
		}
		pool <- tok
	}
	return pool, workers, nil
}

func bgeTokenizerWorkerCount(provider string) int {
	if configured, err := strconv.Atoi(strings.TrimSpace(
		os.Getenv("LUMINA_MEMORY_TOKENIZER_WORKERS"))); err == nil && configured > 0 {
		return minIntLocalModel(configured, 32)
	}
	limit := 8
	if normalizeBGEExecutionProvider(provider) == "cpu" && provider != "metal" {
		limit = 2
	}
	workers := runtime.GOMAXPROCS(0)
	if workers > limit {
		workers = limit
	}
	if workers < 1 {
		return 1
	}
	return workers
}

func (e *LocalBGEEncoder) acquireTokenizer(ctx context.Context) (*tokenizer.Tokenizer, error) {
	if e == nil || e.tokenizers == nil {
		return nil, errors.New("BGE-M3 tokenizer pool is not initialized")
	}
	select {
	case tok := <-e.tokenizers:
		return tok, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (e *LocalBGEEncoder) releaseTokenizer(tok *tokenizer.Tokenizer) {
	if e != nil && tok != nil && e.tokenizers != nil {
		e.tokenizers <- tok
	}
}

func newLocalBGESession(modelPath string, cpuThreads int, provider string) (*ort.DynamicAdvancedSession, error) {
	options, err := ort.NewSessionOptions()
	if err != nil {
		return nil, err
	}
	defer options.Destroy()
	if err := configureBGESessionOptions(options, cpuThreads); err != nil {
		return nil, err
	}
	if err := configureBGEExecutionProvider(options, modelPath, provider); err != nil {
		return nil, err
	}
	session, err := ort.NewDynamicAdvancedSession(modelPath, []string{"input_ids", "attention_mask"},
		[]string{"sentence_embedding", "token_embeddings"}, options)
	if err != nil {
		return nil, fmt.Errorf("load BGE-M3 ONNX model: %w", err)
	}
	return session, nil
}

func (*LocalBGEEncoder) Model() string { return BGEModelName }
func (e *LocalBGEEncoder) Revision() string {
	if e == nil || strings.TrimSpace(e.revision) == "" {
		return BGEModelRevision
	}
	return e.revision
}
func (*LocalBGEEncoder) TokenizerHash() string { return bgeTokenizerSHA256 }

func (e *LocalBGEEncoder) Close() error {
	if e == nil {
		return nil
	}
	e.lifecycleMu.Lock()
	defer e.lifecycleMu.Unlock()
	if e.closed {
		return nil
	}
	e.closed = true
	var errs []error
	if e.metal != nil {
		errs = append(errs, e.metal.Close())
		e.metal = nil
	}
	for _, session := range e.sessions {
		errs = append(errs, session.Destroy())
	}
	e.sessions = nil
	return errors.Join(errs...)
}

type bgeInput struct {
	ids         []int64
	attention   []int64
	specialMask []int
}

func tokenizeEmbeddingText(encode func(string) (*tokenizer.Encoding, error), value string) (*tokenizer.Encoding, error) {
	encoded, err, panicValue := tryTokenizeEmbeddingText(encode, value)
	if panicValue == nil {
		return encoded, err
	}
	// Some tokenizer builds panic on dense ASCII-art backslash runs. Retrying
	// with equivalent slash separators keeps the original ledger text intact
	// while making the derived representation deterministic and recoverable.
	sanitized := strings.Map(func(r rune) rune {
		if r == '\\' {
			return '/'
		}
		if r < 0x20 && r != '\n' && r != '\r' && r != '\t' {
			return ' '
		}
		return r
	}, value)
	encoded, err, panicValue = tryTokenizeEmbeddingText(encode, sanitized)
	if panicValue == nil {
		return encoded, err
	}
	// Tokenizer offset tracking can also fail on symbol-dense diagrams even
	// after slash normalization. Keep the durable source text untouched and
	// derive a conservative lexical representation for model input only.
	conservative := conservativeTokenizerText(value)
	encoded, err, finalPanic := tryTokenizeEmbeddingText(encode, conservative)
	if finalPanic != nil {
		return nil, fmt.Errorf("BGE-M3 tokenizer panic: %v", finalPanic)
	}
	return encoded, err
}

func conservativeTokenizerText(value string) string {
	var result strings.Builder
	result.Grow(len(value))
	pendingSpace := false
	writeSpace := func() {
		if pendingSpace && result.Len() > 0 {
			result.WriteByte(' ')
		}
		pendingSpace = false
	}
	for _, r := range value {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			writeSpace()
			result.WriteRune(r)
		case unicode.IsSpace(r):
			pendingSpace = true
		case strings.ContainsRune(".,!?;:'\"()-", r):
			writeSpace()
			result.WriteRune(r)
		default:
			pendingSpace = true
		}
	}
	return strings.TrimSpace(result.String())
}

func tryTokenizeEmbeddingText(encode func(string) (*tokenizer.Encoding, error), value string) (
	encoded *tokenizer.Encoding, err error, panicValue any) {
	defer func() {
		if recovered := recover(); recovered != nil {
			encoded = nil
			err = nil
			panicValue = recovered
		}
	}()
	encoded, err = encode(value)
	return encoded, err, nil
}

func prepareBGEInput(tok *tokenizer.Tokenizer, text string, maximum int) (bgeInput, error) {
	encoding, err := tokenizeEmbeddingText(func(value string) (*tokenizer.Encoding, error) {
		return tok.EncodeSingle(value, true)
	}, strings.TrimSpace(text))
	if err != nil {
		return bgeInput{}, err
	}
	ids := encoding.GetIds()
	attention := encoding.GetAttentionMask()
	special := encoding.GetSpecialTokenMask()
	if maximum <= 0 || maximum > bgeMaximumTokens {
		maximum = bgeMaximumTokens
	}
	if len(ids) > maximum {
		ids = ids[:maximum]
	}
	if len(attention) < len(ids) {
		attention = make([]int, len(ids))
		for index := range attention {
			attention[index] = 1
		}
	} else {
		attention = attention[:len(ids)]
	}
	if len(special) < len(ids) {
		special = make([]int, len(ids))
	} else {
		special = special[:len(ids)]
	}
	return bgeInput{ids: intsToInt64(ids), attention: intsToInt64(attention), specialMask: special}, nil
}

func (e *LocalBGEEncoder) prepare(text string, maximum int) (bgeInput, error) {
	tok, err := e.acquireTokenizer(context.Background())
	if err != nil {
		return bgeInput{}, err
	}
	defer e.releaseTokenizer(tok)
	return prepareBGEInput(tok, text, maximum)
}

func (e *LocalBGEEncoder) Encode(ctx context.Context, texts []string, kind BGEInputKind) ([]BGEEmbedding, error) {
	return e.encode(ctx, texts, kind, true)
}

func (e *LocalBGEEncoder) EncodeChannels(ctx context.Context, texts []string, kind BGEInputKind) ([]BGEEmbedding, error) {
	return e.encode(ctx, texts, kind, false)
}

func (e *LocalBGEEncoder) encode(ctx context.Context, texts []string, kind BGEInputKind, includeMulti bool) ([]BGEEmbedding, error) {
	if e == nil {
		return nil, errors.New("BGE-M3 encoder is not initialized")
	}
	if len(texts) == 0 {
		return nil, nil
	}
	e.lifecycleMu.RLock()
	defer e.lifecycleMu.RUnlock()
	if e.closed || e.tokenizer == nil || (e.metal == nil && len(e.sessions) == 0) {
		return nil, errors.New("BGE-M3 encoder is not initialized")
	}
	tok, err := e.acquireTokenizer(ctx)
	if err != nil {
		return nil, err
	}
	defer e.releaseTokenizer(tok)
	inputs := make([]bgeInput, len(texts))
	for index, text := range texts {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		input, err := prepareBGEInput(tok, text, bgeMaximumTokens)
		if err != nil {
			return nil, err
		}
		inputs[index] = input
	}
	if e.metal != nil {
		return e.metal.Encode(ctx, inputs, includeMulti)
	}
	var session *ort.DynamicAdvancedSession
	select {
	case session = <-e.available:
		defer func() { e.available <- session }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	result := make([]BGEEmbedding, len(inputs))
	for _, indexes := range planBGEInferenceBatches(inputs) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		batchInputs := make([]bgeInput, len(indexes))
		for index, original := range indexes {
			batchInputs[index] = inputs[original]
		}
		work := bgeInferenceWorkForBatch(batchInputs)
		if work > e.inferenceWork {
			work = e.inferenceWork
		}
		if e.inferenceGate != nil {
			if err := e.inferenceGate.Acquire(ctx, work); err != nil {
				return nil, err
			}
		}
		batch, err := e.encodePreparedBatch(session, batchInputs, includeMulti)
		if e.inferenceGate != nil {
			e.inferenceGate.Release(work)
		}
		if err != nil {
			return nil, err
		}
		for index, original := range indexes {
			result[original] = batch[index]
		}
	}
	return result, nil
}

func bgeInferenceWorkForBatch(inputs []bgeInput) int64 {
	var maxTokens int64
	for _, input := range inputs {
		if length := int64(len(input.ids)); length > maxTokens {
			maxTokens = length
		}
	}
	work := int64(len(inputs)) * maxTokens * maxTokens
	if work < 1 {
		return 1
	}
	return work
}

func planBGEInferenceBatches(inputs []bgeInput) [][]int {
	order := make([]int, len(inputs))
	for index := range order {
		order[index] = index
	}
	sort.SliceStable(order, func(left, right int) bool {
		return len(inputs[order[left]].ids) < len(inputs[order[right]].ids)
	})
	result := make([][]int, 0, (len(order)+bgeInferenceBatchLimit-1)/bgeInferenceBatchLimit)
	for start := 0; start < len(order); {
		end := start
		maxTokens := 0
		for end < len(order) && end-start < bgeInferenceBatchLimit {
			candidateMax := maxTokens
			if length := len(inputs[order[end]].ids); length > candidateMax {
				candidateMax = length
			}
			if end > start && candidateMax*(end-start+1) > bgeInferenceTokenBudget {
				break
			}
			maxTokens = candidateMax
			end++
		}
		result = append(result, append([]int(nil), order[start:end]...))
		start = end
	}
	return result
}

func (e *LocalBGEEncoder) encodePreparedBatch(session *ort.DynamicAdvancedSession,
	inputs []bgeInput, includeMulti bool) ([]BGEEmbedding, error) {
	batchSize := len(inputs)
	maxTokens := 0
	for _, input := range inputs {
		if len(input.ids) > maxTokens {
			maxTokens = len(input.ids)
		}
	}
	ids := make([]int64, batchSize*maxTokens)
	mask := make([]int64, batchSize*maxTokens)
	for index, input := range inputs {
		offset := index * maxTokens
		copy(ids[offset:], input.ids)
		copy(mask[offset:], input.attention)
	}
	shape := ort.NewShape(int64(batchSize), int64(maxTokens))
	idsTensor, err := ort.NewTensor(shape, ids)
	if err != nil {
		return nil, err
	}
	defer idsTensor.Destroy()
	maskTensor, err := ort.NewTensor(shape, mask)
	if err != nil {
		return nil, err
	}
	defer maskTensor.Destroy()
	outputs := []ort.Value{nil, nil}
	if err := session.Run([]ort.Value{idsTensor, maskTensor}, outputs); err != nil {
		return nil, fmt.Errorf("run BGE-M3: %w", err)
	}
	for _, output := range outputs {
		if output != nil {
			defer output.Destroy()
		}
	}
	denseTensor, denseOK := outputs[0].(*ort.Tensor[float32])
	tokenTensor, tokenOK := outputs[1].(*ort.Tensor[float32])
	if !denseOK || !tokenOK {
		return nil, fmt.Errorf("unexpected BGE-M3 outputs %T/%T", outputs[0], outputs[1])
	}
	if shape := denseTensor.GetShape(); len(shape) != 2 || int(shape[0]) != batchSize || int(shape[1]) != bgeDimensions {
		return nil, fmt.Errorf("unexpected BGE-M3 dense shape %v", shape)
	}
	if shape := tokenTensor.GetShape(); len(shape) != 3 || int(shape[0]) != batchSize || int(shape[1]) != maxTokens || int(shape[2]) != bgeDimensions {
		return nil, fmt.Errorf("unexpected BGE-M3 token shape %v", shape)
	}
	denseData, tokenData := denseTensor.GetData(), tokenTensor.GetData()
	result := make([]BGEEmbedding, batchSize)
	for batchIndex, input := range inputs {
		dense := append([]float32(nil), denseData[batchIndex*bgeDimensions:(batchIndex+1)*bgeDimensions]...)
		normalizeFloat32(dense)
		type encodedToken struct {
			index  int
			id     int64
			weight float32
		}
		tokens := make([]encodedToken, 0, len(input.ids))
		sparse := make(map[int64]float32)
		base := batchIndex * maxTokens * bgeDimensions
		for tokenIndex, tokenID := range input.ids {
			if input.attention[tokenIndex] == 0 || input.specialMask[tokenIndex] != 0 {
				continue
			}
			hidden := tokenData[base+tokenIndex*bgeDimensions : base+(tokenIndex+1)*bgeDimensions]
			weight := e.sparseBias
			for dimension, value := range hidden {
				weight += value * e.sparseWeight[dimension]
			}
			if weight > 0 && weight > sparse[tokenID] {
				sparse[tokenID] = weight
			}
			// BGE-M3's lexical and ColBERT heads are independent. Sparse weights
			// select learned lexical postings, but they must not prune the token
			// vectors used by late interaction.
			if weight < 0 {
				weight = 0
			}
			if includeMulti {
				tokens = append(tokens, encodedToken{index: tokenIndex, id: tokenID, weight: weight})
			}
		}
		hiddenBatch := make([][]float32, len(tokens))
		for index, token := range tokens {
			hiddenBatch[index] = tokenData[base+token.index*bgeDimensions : base+(token.index+1)*bgeDimensions]
		}
		projectedBatch := projectBGETokens(hiddenBatch, e.colbertWeight, e.colbertBias)
		multi := make([]BGETokenVector, 0, len(tokens))
		for index, token := range tokens {
			multi = append(multi, BGETokenVector{TokenID: token.id, Position: token.index,
				Weight: token.weight, Values: projectedBatch[index]})
		}
		result[batchIndex] = BGEEmbedding{Dense: dense, Sparse: sparse, Multi: multi}
	}
	return result, nil
}

func (e *LocalBGEEncoder) Split(text string, maxTokens, overlap int) ([]string, error) {
	result, err := e.SplitMany(text, []BGESplitSpec{{MaxTokens: maxTokens, Overlap: overlap}})
	if err != nil {
		return nil, err
	}
	if len(result) != 1 {
		return nil, errors.New("BGE-M3 split returned an invalid result count")
	}
	return result[0], nil
}

type bgeTokenizedSentence struct {
	text string
	ids  []int
}

func (e *LocalBGEEncoder) SplitMany(text string, specs []BGESplitSpec) ([][]string, error) {
	if e == nil {
		return nil, errors.New("BGE-M3 encoder is not initialized")
	}
	e.lifecycleMu.RLock()
	defer e.lifecycleMu.RUnlock()
	if e.closed || e.tokenizer == nil {
		return nil, errors.New("BGE-M3 encoder is not initialized")
	}
	tok, err := e.acquireTokenizer(context.Background())
	if err != nil {
		return nil, err
	}
	defer e.releaseTokenizer(tok)
	result := make([][]string, len(specs))
	if len(specs) == 0 {
		return result, nil
	}
	normalized := make([]BGESplitSpec, len(specs))
	for index, spec := range specs {
		normalized[index] = normalizeBGESplitSpec(spec)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return result, nil
	}
	total, err := tokenizeEmbeddingText(func(value string) (*tokenizer.Encoding, error) {
		return tok.EncodeSingle(value, false)
	}, text)
	if err != nil {
		return nil, err
	}
	totalTokens := len(total.GetIds())
	needsSentences := false
	for index, spec := range normalized {
		if totalTokens <= spec.MaxTokens {
			result[index] = []string{text}
		} else {
			needsSentences = true
		}
	}
	if !needsSentences {
		return result, nil
	}

	sentences := make([]bgeTokenizedSentence, 0)
	for _, sentence := range splitBGETextSentences(text) {
		encoded, encodeErr := tokenizeEmbeddingText(func(value string) (*tokenizer.Encoding, error) {
			return tok.EncodeSingle(value, false)
		}, sentence)
		if encodeErr != nil {
			return nil, encodeErr
		}
		sentences = append(sentences, bgeTokenizedSentence{text: sentence, ids: encoded.GetIds()})
	}
	for index, spec := range normalized {
		if result[index] != nil {
			continue
		}
		result[index] = splitBGETokenizedSentences(tok, sentences, spec)
	}
	return result, nil
}

func normalizeBGESplitSpec(spec BGESplitSpec) BGESplitSpec {
	if spec.MaxTokens <= 0 {
		spec.MaxTokens = 192
	}
	if spec.Overlap < 0 {
		spec.Overlap = 0
	}
	if spec.Overlap >= spec.MaxTokens {
		spec.Overlap = spec.MaxTokens / 4
	}
	return spec
}

func splitBGETokenizedSentences(tok *tokenizer.Tokenizer, sentences []bgeTokenizedSentence,
	spec BGESplitSpec) []string {
	type piece struct {
		text   string
		tokens int
	}
	var pieces []piece
	for _, sentence := range sentences {
		if len(sentence.ids) <= spec.MaxTokens {
			pieces = append(pieces, piece{text: sentence.text, tokens: len(sentence.ids)})
			continue
		}
		step := spec.MaxTokens - spec.Overlap
		for start := 0; start < len(sentence.ids); start += step {
			end := minIntLocalModel(len(sentence.ids), start+spec.MaxTokens)
			decoded := strings.TrimSpace(tok.Decode(sentence.ids[start:end], true))
			if decoded != "" {
				pieces = append(pieces, piece{text: decoded, tokens: end - start})
			}
			if end == len(sentence.ids) {
				break
			}
		}
	}
	var spans []string
	for start := 0; start < len(pieces); {
		end, tokens := start, 0
		for end < len(pieces) && tokens+pieces[end].tokens <= spec.MaxTokens {
			tokens += pieces[end].tokens
			end++
		}
		if end == start {
			end++
		}
		parts := make([]string, 0, end-start)
		for _, item := range pieces[start:end] {
			parts = append(parts, item.text)
		}
		spans = append(spans, strings.TrimSpace(strings.Join(parts, " ")))
		if end == len(pieces) {
			break
		}
		next, carried := end, 0
		for next > start && carried+pieces[next-1].tokens <= spec.Overlap {
			next--
			carried += pieces[next].tokens
		}
		if next <= start {
			next = end
		}
		start = next
	}
	return spans
}

func splitBGETextSentences(text string) []string {
	runes := []rune(text)
	start := 0
	var result []string
	flush := func(end int) {
		if value := strings.TrimSpace(string(runes[start:end])); value != "" {
			result = append(result, value)
		}
		start = end
	}
	for index, value := range runes {
		switch value {
		case '.', '!', '?', '\n', '\r', '\u3002', '\uff01', '\uff1f':
			flush(index + 1)
		}
	}
	flush(len(runes))
	return result
}

func minIntLocalModel(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func normalizeFloat32(values []float32) {
	var norm float64
	for _, value := range values {
		norm += float64(value * value)
	}
	if norm = math.Sqrt(norm); norm == 0 {
		return
	}
	for index := range values {
		values[index] = float32(float64(values[index]) / norm)
	}
}

func readFP16File(path string, count int) ([]float32, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(content) != count*2 {
		return nil, fmt.Errorf("%s has %d bytes, want %d", path, len(content), count*2)
	}
	result := make([]float32, count)
	for index := range result {
		result[index] = float16ToFloat32(binary.LittleEndian.Uint16(content[index*2:]))
	}
	return result, nil
}

func float16ToFloat32(value uint16) float32 {
	sign := uint32(value&0x8000) << 16
	exponent := uint32(value>>10) & 0x1f
	fraction := uint32(value & 0x03ff)
	switch exponent {
	case 0:
		if fraction == 0 {
			return math.Float32frombits(sign)
		}
		exponent = 1
		for fraction&0x0400 == 0 {
			fraction <<= 1
			exponent--
		}
		fraction &= 0x03ff
		exponent += 127 - 15
	case 31:
		exponent = 255
	default:
		exponent += 127 - 15
	}
	return math.Float32frombits(sign | exponent<<23 | fraction<<13)
}

func fileSHA256(path string) (string, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:]), nil
}
