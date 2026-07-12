package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"LuminaCode/agent"
	adapter "LuminaCode/benchmark/longmemevalv2"
	"LuminaCode/config"
	coretools "LuminaCode/tools"
)

type trajectoryIngestionFunc func(context.Context, config.Config, map[string]any) (trajectoryIngestionResult, error)

type trajectoryIngestionResult struct {
	Messages int
	Ingested int
}

type trajectoryIngestionStats struct {
	Submitted int64
	Completed int64
	Messages  int64
	Ingested  int64
}

type trajectoryIngestionTask struct {
	trajectory map[string]any
}

// trajectoryIngestionPool parallelizes CPU-heavy evidence preparation across
// independent sessions. CommitExtraction retains a single writer per store,
// while the embedding worker consumes each committed batch concurrently.
type trajectoryIngestionPool struct {
	ctx       context.Context
	cancel    context.CancelFunc
	cfg       config.Config
	backfill  *embeddingBackfillWorker
	ingest    trajectoryIngestionFunc
	tasks     chan trajectoryIngestionTask
	pending   sync.WaitGroup
	workers   sync.WaitGroup
	close     sync.Once
	errMu     sync.Mutex
	errors    []error
	submitted atomic.Int64
	completed atomic.Int64
	messages  atomic.Int64
	ingested  atomic.Int64
}

func configuredIngestionWorkers() int {
	workers := runtime.NumCPU()
	if workers > 8 {
		workers = 8
	}
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LUMINA_MEMORY_INGESTION_WORKERS"))); err == nil && value > 0 {
		workers = value
	}
	if workers > 32 {
		workers = 32
	}
	if workers < 1 {
		workers = 1
	}
	return workers
}

func newTrajectoryIngestionPool(parent context.Context, cfg config.Config, backfill *embeddingBackfillWorker, workerCount int, ingest trajectoryIngestionFunc) *trajectoryIngestionPool {
	ctx, cancel := context.WithCancel(parent)
	if workerCount < 1 {
		workerCount = 1
	}
	pool := &trajectoryIngestionPool{ctx: ctx, cancel: cancel, cfg: cfg, backfill: backfill, ingest: ingest,
		tasks: make(chan trajectoryIngestionTask, workerCount*2)}
	pool.workers.Add(workerCount)
	for range workerCount {
		go pool.runWorker()
	}
	return pool
}

func (p *trajectoryIngestionPool) Enqueue(ctx context.Context, trajectory map[string]any) error {
	if p == nil {
		return errors.New("trajectory ingestion pool is unavailable")
	}
	p.pending.Add(1)
	select {
	case p.tasks <- trajectoryIngestionTask{trajectory: trajectory}:
		p.submitted.Add(1)
		return nil
	case <-ctx.Done():
		p.pending.Done()
		return ctx.Err()
	case <-p.ctx.Done():
		p.pending.Done()
		return errors.New("trajectory ingestion pool is closed")
	}
}

func (p *trajectoryIngestionPool) Drain(ctx context.Context) (trajectoryIngestionStats, error) {
	if p == nil {
		return trajectoryIngestionStats{}, nil
	}
	done := make(chan struct{})
	go func() {
		p.pending.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		return p.Stats(), ctx.Err()
	case <-p.ctx.Done():
		return p.Stats(), errors.New("trajectory ingestion pool closed before drain completed")
	}
	p.errMu.Lock()
	failures := append([]error(nil), p.errors...)
	p.errMu.Unlock()
	return p.Stats(), errors.Join(failures...)
}

func (p *trajectoryIngestionPool) Stats() trajectoryIngestionStats {
	return trajectoryIngestionStats{Submitted: p.submitted.Load(), Completed: p.completed.Load(),
		Messages: p.messages.Load(), Ingested: p.ingested.Load()}
}

func (p *trajectoryIngestionPool) Close() {
	if p == nil {
		return
	}
	p.close.Do(func() {
		p.cancel()
		close(p.tasks)
		p.workers.Wait()
	})
}

func (p *trajectoryIngestionPool) runWorker() {
	defer p.workers.Done()
	for task := range p.tasks {
		result, err := p.ingestWithRetry(task.trajectory)
		if err != nil {
			trajectoryID := strings.TrimSpace(stringField(task.trajectory, "id"))
			p.errMu.Lock()
			p.errors = append(p.errors, fmt.Errorf("trajectory %s: %w", trajectoryID, err))
			p.errMu.Unlock()
		} else {
			p.completed.Add(1)
			p.messages.Add(int64(result.Messages))
			p.ingested.Add(int64(result.Ingested))
			p.backfill.Notify()
		}
		p.pending.Done()
	}
}

func (p *trajectoryIngestionPool) ingestWithRetry(trajectory map[string]any) (trajectoryIngestionResult, error) {
	for {
		result, err := p.ingest(p.ctx, p.cfg, trajectory)
		if !retryableEmbeddingBackfillError(err) {
			return result, err
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-p.ctx.Done():
			timer.Stop()
			return trajectoryIngestionResult{}, p.ctx.Err()
		case <-timer.C:
		}
	}
}

func ingestTrajectory(ctx context.Context, cfg config.Config, trajectory map[string]any) (trajectoryIngestionResult, error) {
	trajectoryID := strings.TrimSpace(stringField(trajectory, "id"))
	if trajectoryID == "" {
		return trajectoryIngestionResult{}, errors.New("trajectory id is required")
	}
	state := agent.NewAgentState()
	state.MemorySessionID = trajectoryID
	state.MemoryAgentID = "trajectory-replay"
	state.MemoryAgentType = "trajectory-replay"
	state.Messages = adapter.MessagesFromTrajectory(trajectory)
	for _, message := range state.Messages {
		if message["role"] == "user" {
			state.UserTurnCount++
		}
	}
	controller := agent.NewExtractionController(cfg, coretools.NewToolRegistry())
	controller.SourceSessionID = trajectoryID
	controller.SourceAgentID = "trajectory-replay"
	controller.StoreBusyTimeout = 15 * time.Minute
	total := 0
	for {
		count, err := controller.IngestMessages(ctx, &state)
		if err != nil {
			return trajectoryIngestionResult{}, err
		}
		total += count
		if count == 0 {
			break
		}
	}
	return trajectoryIngestionResult{Messages: len(state.Messages), Ingested: total}, nil
}
