package main

import (
	"context"
	"fmt"
	"os"
	"sync"

	"LuminaCode/config"
)

type embeddingBackfillFunc func(context.Context, config.Config) (int, error)

type embeddingBackfillRequest struct {
	result chan embeddingBackfillResult
}

type embeddingBackfillResult struct {
	processed int
	err       error
}

// embeddingBackfillWorker overlaps committed raw-memory ingestion with local
// embedding backfill. A query submits a barrier request and waits until all
// jobs committed before that request have been consumed.
type embeddingBackfillWorker struct {
	cfg      config.Config
	backfill embeddingBackfillFunc
	ctx      context.Context
	cancel   context.CancelFunc
	requests chan embeddingBackfillRequest
	done     chan struct{}
	close    sync.Once
}

func newEmbeddingBackfillWorker(parent context.Context, cfg config.Config, backfill embeddingBackfillFunc) *embeddingBackfillWorker {
	ctx, cancel := context.WithCancel(parent)
	worker := &embeddingBackfillWorker{
		cfg: cfg, backfill: backfill, ctx: ctx, cancel: cancel,
		requests: make(chan embeddingBackfillRequest, 256), done: make(chan struct{}),
	}
	go worker.run()
	return worker
}

func (w *embeddingBackfillWorker) Notify() {
	if w == nil || !w.cfg.MemoryEmbeddingEnabled {
		return
	}
	select {
	case w.requests <- embeddingBackfillRequest{}:
	default:
		// A queued request already guarantees another complete backlog scan.
	}
}

func (w *embeddingBackfillWorker) Drain(ctx context.Context) (int, error) {
	if w == nil || !w.cfg.MemoryEmbeddingEnabled {
		return 0, nil
	}
	result := make(chan embeddingBackfillResult, 1)
	request := embeddingBackfillRequest{result: result}
	select {
	case w.requests <- request:
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-w.done:
		return 0, fmt.Errorf("embedding backfill worker is closed")
	}
	select {
	case outcome := <-result:
		return outcome.processed, outcome.err
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-w.done:
		return 0, fmt.Errorf("embedding backfill worker closed before the drain completed")
	}
}

func (w *embeddingBackfillWorker) Close() {
	if w == nil {
		return
	}
	w.close.Do(func() {
		w.cancel()
		<-w.done
	})
}

func (w *embeddingBackfillWorker) run() {
	defer close(w.done)
	for {
		var first embeddingBackfillRequest
		select {
		case <-w.ctx.Done():
			return
		case first = <-w.requests:
		}
		waiters := make([]chan embeddingBackfillResult, 0, 1)
		if first.result != nil {
			waiters = append(waiters, first.result)
		}
		// Coalesce notifications that accumulated while ingestion was committing
		// the next trajectory. One complete scan services all of them.
	drainRequests:
		for {
			select {
			case request := <-w.requests:
				if request.result != nil {
					waiters = append(waiters, request.result)
				}
			default:
				break drainRequests
			}
		}
		processed, err := w.backfill(w.ctx, w.cfg)
		if err != nil && len(waiters) == 0 {
			fmt.Fprintf(os.Stderr, "background memory embedding backfill: %v\n", err)
		}
		outcome := embeddingBackfillResult{processed: processed, err: err}
		for _, waiter := range waiters {
			waiter <- outcome
		}
	}
}
