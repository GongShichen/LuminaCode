//go:build cgo

package localmodel

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	bgeMetalBatchWindow = 2 * time.Millisecond
	bgeMetalQueueSize   = 128
)

type bgeMetalTransport interface {
	Encode(context.Context, []bgeInput, bool) ([]BGEEmbedding, error)
	Close() error
}

type bgeMetalCall struct {
	ctx          context.Context
	inputs       []bgeInput
	includeMulti bool
	result       chan bgeMetalCallResult
}

type bgeMetalCallResult struct {
	embeddings []BGEEmbedding
	err        error
}

type bgeMetalScheduler struct {
	transport bgeMetalTransport
	window    time.Duration
	queue     chan *bgeMetalCall
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once

	activeMu     sync.Mutex
	activeCancel context.CancelFunc
}

func newBGEMetalScheduler(transport bgeMetalTransport) *bgeMetalScheduler {
	return newBGEMetalSchedulerWithWindow(transport, bgeMetalBatchWindow)
}

func newBGEMetalSchedulerWithWindow(transport bgeMetalTransport, window time.Duration) *bgeMetalScheduler {
	scheduler := &bgeMetalScheduler{
		transport: transport,
		window:    window,
		queue:     make(chan *bgeMetalCall, bgeMetalQueueSize),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}
	go scheduler.run()
	return scheduler
}

func (s *bgeMetalScheduler) Encode(ctx context.Context, inputs []bgeInput,
	includeMulti bool) ([]BGEEmbedding, error) {
	if s == nil {
		return nil, errors.New("BGE-M3 Metal scheduler is not initialized")
	}
	if len(inputs) == 0 {
		return nil, nil
	}
	call := &bgeMetalCall{
		ctx:          ctx,
		inputs:       inputs,
		includeMulti: includeMulti,
		result:       make(chan bgeMetalCallResult, 1),
	}
	select {
	case s.queue <- call:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, errors.New("BGE-M3 Metal scheduler is closed")
	}
	select {
	case result := <-call.result:
		return result.embeddings, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, errors.New("BGE-M3 Metal scheduler is closed")
	}
}

func (s *bgeMetalScheduler) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		close(s.stop)
		s.activeMu.Lock()
		if s.activeCancel != nil {
			s.activeCancel()
		}
		s.activeMu.Unlock()
	})
	<-s.done
	return s.transport.Close()
}

func (s *bgeMetalScheduler) run() {
	defer close(s.done)
	var pending []*bgeMetalCall
	for {
		select {
		case <-s.stop:
			s.failPending(pending, errors.New("BGE-M3 Metal scheduler is closed"))
			return
		default:
		}
		first, remaining, ok := s.nextCall(pending)
		pending = remaining
		if !ok {
			s.failPending(pending, errors.New("BGE-M3 Metal scheduler is closed"))
			return
		}
		if err := first.ctx.Err(); err != nil {
			first.result <- bgeMetalCallResult{err: err}
			continue
		}
		batch := []*bgeMetalCall{first}
		totalInputs, maxTokens := metalCallShape(first)
		if totalInputs < bgeInferenceBatchLimit &&
			totalInputs*maxTokens < bgeInferenceTokenBudget {
			batch, pending = s.collectBatch(batch, pending, totalInputs, maxTokens)
		}
		select {
		case <-s.stop:
			s.failPending(append(batch, pending...), errors.New("BGE-M3 Metal scheduler is closed"))
			return
		default:
		}
		s.dispatch(batch)
	}
}

func (s *bgeMetalScheduler) nextCall(pending []*bgeMetalCall) (
	*bgeMetalCall, []*bgeMetalCall, bool) {
	if len(pending) > 0 {
		return pending[0], pending[1:], true
	}
	select {
	case call := <-s.queue:
		return call, pending, true
	case <-s.stop:
		return nil, pending, false
	}
}

func (s *bgeMetalScheduler) collectBatch(batch, pending []*bgeMetalCall,
	totalInputs, maxTokens int) ([]*bgeMetalCall, []*bgeMetalCall) {
	timer := time.NewTimer(s.window)
	defer timer.Stop()
	for {
		select {
		case call := <-s.queue:
			if err := call.ctx.Err(); err != nil {
				call.result <- bgeMetalCallResult{err: err}
				continue
			}
			if metalCallsCompatible(batch[0], call, totalInputs, maxTokens) {
				batch = append(batch, call)
				callInputs, callTokens := metalCallShape(call)
				totalInputs += callInputs
				if callTokens > maxTokens {
					maxTokens = callTokens
				}
				if totalInputs >= bgeInferenceBatchLimit ||
					totalInputs*maxTokens >= bgeInferenceTokenBudget {
					return batch, pending
				}
				continue
			}
			pending = append(pending, call)
		case <-timer.C:
			return batch, pending
		case <-s.stop:
			return batch, pending
		}
	}
}

func metalCallsCompatible(first, next *bgeMetalCall, totalInputs, maxTokens int) bool {
	if first.includeMulti != next.includeMulti {
		return false
	}
	nextInputs, nextTokens := metalCallShape(next)
	if totalInputs+nextInputs > bgeInferenceBatchLimit {
		return false
	}
	if nextTokens > maxTokens {
		maxTokens = nextTokens
	}
	return (totalInputs+nextInputs)*maxTokens <= bgeInferenceTokenBudget
}

func metalCallShape(call *bgeMetalCall) (int, int) {
	maxTokens := 0
	for _, input := range call.inputs {
		if len(input.ids) > maxTokens {
			maxTokens = len(input.ids)
		}
	}
	return len(call.inputs), maxTokens
}

func (s *bgeMetalScheduler) dispatch(batch []*bgeMetalCall) {
	active := make([]*bgeMetalCall, 0, len(batch))
	combined := make([]bgeInput, 0)
	for _, call := range batch {
		if call.ctx.Err() == nil {
			active = append(active, call)
			combined = append(combined, call.inputs...)
		} else {
			call.result <- bgeMetalCallResult{err: call.ctx.Err()}
		}
	}
	if len(active) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.activeMu.Lock()
	s.activeCancel = cancel
	s.activeMu.Unlock()
	monitorDone := make(chan struct{})
	go cancelWhenAllMetalCallsFinish(ctx, cancel, active, monitorDone)
	embeddings, err := s.transport.Encode(ctx, combined, active[0].includeMulti)
	close(monitorDone)
	cancel()
	s.activeMu.Lock()
	s.activeCancel = nil
	s.activeMu.Unlock()
	if err == nil && len(embeddings) != len(combined) {
		err = errors.New("BGE-M3 Metal batch result count does not match the request")
	}

	offset := 0
	for _, call := range active {
		count := len(call.inputs)
		switch {
		case err != nil:
			call.result <- bgeMetalCallResult{err: err}
		case offset+count > len(embeddings):
			call.result <- bgeMetalCallResult{err: errors.New("BGE-M3 Metal batch result is incomplete")}
		default:
			call.result <- bgeMetalCallResult{
				embeddings: append([]BGEEmbedding(nil), embeddings[offset:offset+count]...),
			}
		}
		offset += count
	}
}

func cancelWhenAllMetalCallsFinish(ctx context.Context, cancel context.CancelFunc,
	calls []*bgeMetalCall, done <-chan struct{}) {
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		allCanceled := true
		for _, call := range calls {
			if call.ctx.Err() == nil {
				allCanceled = false
				break
			}
		}
		if allCanceled {
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		case <-ticker.C:
		}
	}
}

func (s *bgeMetalScheduler) failPending(pending []*bgeMetalCall, err error) {
	for _, call := range pending {
		call.result <- bgeMetalCallResult{err: err}
	}
	for {
		select {
		case call := <-s.queue:
			call.result <- bgeMetalCallResult{err: err}
		default:
			return
		}
	}
}
