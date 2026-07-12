package longmemory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type EmbeddingTrace struct {
	QueueWaitMS int64 `json:"queue_wait_ms"`
	ExecutionMS int64 `json:"execution_ms"`
	Batches     int64 `json:"batches"`
	Texts       int64 `json:"texts"`
	CacheHits   int64 `json:"cache_hits"`
	Errors      int64 `json:"errors"`
}

type EmbeddingSchedulerOptions struct {
	BatchSize         int
	BatchWait         time.Duration
	QueryCacheEntries int
	ExecutionTimeout  time.Duration
}

type embeddingRequest struct {
	ctx      context.Context
	texts    []string
	kind     EmbeddingKind
	queuedAt time.Time
	result   chan embeddingResponse
}

type embeddingResponse struct {
	vectors [][]float32
	err     error
}

type ScheduledEmbedder struct {
	base       Embedder
	opts       EmbeddingSchedulerOptions
	queryQueue chan embeddingRequest
	passQueue  chan embeddingRequest
	cacheMu    sync.Mutex
	cache      map[string][][]float32
	cacheOrder []string
	queueWait  atomic.Int64
	execution  atomic.Int64
	batches    atomic.Int64
	texts      atomic.Int64
	cacheHits  atomic.Int64
	errors     atomic.Int64
}

var embeddingSchedulers sync.Map

func SharedEmbeddingScheduler(base Embedder, opts EmbeddingSchedulerOptions) Embedder {
	if base == nil {
		return nil
	}
	opts = normalizeSchedulerOptions(opts)
	key := fmt.Sprintf("%T:%p:%s:%d:%d:%d:%d", base, base, base.Model(), opts.BatchSize,
		opts.BatchWait.Milliseconds(), opts.QueryCacheEntries, opts.ExecutionTimeout.Milliseconds())
	if existing, ok := embeddingSchedulers.Load(key); ok {
		return existing.(*ScheduledEmbedder)
	}
	scheduler := &ScheduledEmbedder{base: base, opts: opts, queryQueue: make(chan embeddingRequest, 256),
		passQueue: make(chan embeddingRequest, 256), cache: map[string][][]float32{}}
	actual, loaded := embeddingSchedulers.LoadOrStore(key, scheduler)
	if loaded {
		return actual.(*ScheduledEmbedder)
	}
	go scheduler.runCoordinator()
	return scheduler
}

func normalizeSchedulerOptions(opts EmbeddingSchedulerOptions) EmbeddingSchedulerOptions {
	if opts.BatchSize <= 0 {
		opts.BatchSize = 32
	}
	if opts.BatchWait <= 0 {
		opts.BatchWait = 20 * time.Millisecond
	}
	if opts.QueryCacheEntries <= 0 {
		opts.QueryCacheEntries = 10000
	}
	if opts.ExecutionTimeout <= 0 {
		opts.ExecutionTimeout = 8 * time.Second
	}
	return opts
}

func (s *ScheduledEmbedder) Model() string   { return s.base.Model() }
func (s *ScheduledEmbedder) Dimensions() int { return s.base.Dimensions() }

func (s *ScheduledEmbedder) Embed(ctx context.Context, texts []string, kind EmbeddingKind) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	vectors := make([][]float32, len(texts))
	missingTexts := make([]string, 0, len(texts))
	missingKeys := make([]string, 0, len(texts))
	missingIndexes := make([][]int, 0, len(texts))
	missingByKey := map[string]int{}
	for index, value := range texts {
		key := embeddingTextCacheKey(kind, value)
		if cached, ok := s.cached(key); ok && len(cached) == 1 {
			s.cacheHits.Add(1)
			vectors[index] = cached[0]
			continue
		}
		if missingIndex, ok := missingByKey[key]; ok {
			missingIndexes[missingIndex] = append(missingIndexes[missingIndex], index)
			continue
		}
		missingByKey[key] = len(missingTexts)
		missingTexts = append(missingTexts, value)
		missingKeys = append(missingKeys, key)
		missingIndexes = append(missingIndexes, []int{index})
	}
	for start := 0; start < len(missingTexts); start += s.opts.BatchSize {
		end := minInt(start+s.opts.BatchSize, len(missingTexts))
		batch, err := s.enqueue(ctx, missingTexts[start:end], kind)
		if err != nil {
			return nil, err
		}
		for offset, vector := range batch {
			missingIndex := start + offset
			s.putCached(missingKeys[missingIndex], [][]float32{vector})
			for _, outputIndex := range missingIndexes[missingIndex] {
				vectors[outputIndex] = append([]float32(nil), vector...)
			}
		}
	}
	return vectors, nil
}

func (s *ScheduledEmbedder) enqueue(ctx context.Context, texts []string, kind EmbeddingKind) ([][]float32, error) {
	request := embeddingRequest{ctx: ctx, texts: append([]string(nil), texts...), kind: kind,
		queuedAt: time.Now(), result: make(chan embeddingResponse, 1)}
	queue := s.passQueue
	if kind == EmbeddingQuery {
		queue = s.queryQueue
	}
	select {
	case queue <- request:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	select {
	case response := <-request.result:
		return response.vectors, response.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *ScheduledEmbedder) runCoordinator() {
	queryStreak := 0
	for {
		var first embeddingRequest
		var kind EmbeddingKind
		if queryStreak >= 4 {
			select {
			case first = <-s.passQueue:
				kind, queryStreak = EmbeddingPassage, 0
			default:
			}
		}
		if first.result == nil {
			select {
			case first = <-s.queryQueue:
				kind, queryStreak = EmbeddingQuery, queryStreak+1
			default:
				select {
				case first = <-s.queryQueue:
					kind, queryStreak = EmbeddingQuery, queryStreak+1
				case first = <-s.passQueue:
					kind, queryStreak = EmbeddingPassage, 0
				}
			}
		}
		queue := s.passQueue
		if kind == EmbeddingQuery {
			queue = s.queryQueue
		}
		s.collectAndExecute(first, queue, kind)
	}
}

func (s *ScheduledEmbedder) collectAndExecute(first embeddingRequest, queue <-chan embeddingRequest, kind EmbeddingKind) {
	batch := []embeddingRequest{first}
	textCount := len(first.texts)
	timer := time.NewTimer(s.opts.BatchWait)
collect:
	for textCount < s.opts.BatchSize {
		select {
		case request := <-queue:
			batch = append(batch, request)
			textCount += len(request.texts)
		case <-timer.C:
			break collect
		}
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	s.executeBatch(batch, kind)
}

func (s *ScheduledEmbedder) executeBatch(batch []embeddingRequest, kind EmbeddingKind) {
	active := make([]embeddingRequest, 0, len(batch))
	var texts []string
	for _, request := range batch {
		if request.ctx.Err() != nil {
			request.result <- embeddingResponse{err: request.ctx.Err()}
			continue
		}
		s.queueWait.Add(time.Since(request.queuedAt).Milliseconds())
		active = append(active, request)
		texts = append(texts, request.texts...)
	}
	if len(active) == 0 {
		return
	}
	started := time.Now()
	execCtx, cancel := context.WithTimeout(context.Background(), s.opts.ExecutionTimeout)
	vectors, err := s.base.Embed(execCtx, texts, kind)
	cancel()
	s.execution.Add(time.Since(started).Milliseconds())
	s.batches.Add(1)
	s.texts.Add(int64(len(texts)))
	if err != nil || len(vectors) != len(texts) {
		if err == nil {
			err = fmt.Errorf("embedding count mismatch: got %d, want %d", len(vectors), len(texts))
		}
		s.errors.Add(1)
		for _, request := range active {
			request.result <- embeddingResponse{err: err}
		}
		return
	}
	offset := 0
	for _, request := range active {
		end := offset + len(request.texts)
		request.result <- embeddingResponse{vectors: cloneVectors(vectors[offset:end])}
		offset = end
	}
}

func (s *ScheduledEmbedder) Stats() EmbeddingTrace {
	return EmbeddingTrace{QueueWaitMS: s.queueWait.Load(), ExecutionMS: s.execution.Load(),
		Batches: s.batches.Load(), Texts: s.texts.Load(), CacheHits: s.cacheHits.Load(), Errors: s.errors.Load()}
}

func EmbeddingStats(embedder Embedder) EmbeddingTrace {
	if scheduler, ok := embedder.(*ScheduledEmbedder); ok {
		return scheduler.Stats()
	}
	return EmbeddingTrace{}
}

// EmbeddingBatchSize returns the scheduler's atomic execution unit. Callers
// that persist generated vectors should commit after each unit so a later
// execution failure does not discard already completed work.
func EmbeddingBatchSize(embedder Embedder) int {
	if scheduler, ok := embedder.(*ScheduledEmbedder); ok {
		return scheduler.opts.BatchSize
	}
	return 32
}

func EmbeddingStatsDelta(before, after EmbeddingTrace) EmbeddingTrace {
	return EmbeddingTrace{QueueWaitMS: after.QueueWaitMS - before.QueueWaitMS,
		ExecutionMS: after.ExecutionMS - before.ExecutionMS, Batches: after.Batches - before.Batches,
		Texts: after.Texts - before.Texts, CacheHits: after.CacheHits - before.CacheHits,
		Errors: after.Errors - before.Errors}
}

func embeddingCacheKey(kind EmbeddingKind, texts []string) string {
	normalized := make([]string, len(texts))
	for index, text := range texts {
		normalized[index] = strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(text))), " ")
	}
	return string(kind) + "\x00" + strings.Join(normalized, "\x00")
}

func embeddingTextCacheKey(kind EmbeddingKind, value string) string {
	return embeddingCacheKey(kind, []string{value})
}

func (s *ScheduledEmbedder) cached(key string) ([][]float32, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	vectors, ok := s.cache[key]
	return cloneVectors(vectors), ok
}

func (s *ScheduledEmbedder) putCached(key string, vectors [][]float32) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if _, exists := s.cache[key]; !exists {
		s.cacheOrder = append(s.cacheOrder, key)
	}
	s.cache[key] = cloneVectors(vectors)
	for len(s.cacheOrder) > s.opts.QueryCacheEntries {
		oldest := s.cacheOrder[0]
		s.cacheOrder = s.cacheOrder[1:]
		delete(s.cache, oldest)
	}
}

func cloneVectors(vectors [][]float32) [][]float32 {
	if vectors == nil {
		return nil
	}
	result := make([][]float32, len(vectors))
	for index := range vectors {
		result[index] = append([]float32(nil), vectors[index]...)
	}
	return result
}
