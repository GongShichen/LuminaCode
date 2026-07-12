package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"LuminaCode/config"
)

type codedBackfillError int

func (e codedBackfillError) Error() string { return fmt.Sprintf("sqlite code %d", int(e)) }
func (e codedBackfillError) Code() int     { return int(e) }

func TestEmbeddingBackfillWorkerOverlapsAndDrains(t *testing.T) {
	cfg := config.Config{MemoryEmbeddingEnabled: true}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var calls atomic.Int64
	worker := newEmbeddingBackfillWorker(context.Background(), cfg, func(context.Context, config.Config) (int, error) {
		calls.Add(1)
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		return 17, nil
	})
	defer worker.Close()

	worker.Notify()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("background backfill did not start after notification")
	}

	drained := make(chan embeddingBackfillResult, 1)
	go func() {
		processed, err := worker.Drain(context.Background())
		drained <- embeddingBackfillResult{processed: processed, err: err}
	}()
	close(release)

	select {
	case result := <-drained:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.processed != 17 {
			t.Fatalf("processed = %d, want 17", result.processed)
		}
	case <-time.After(time.Second):
		t.Fatal("drain barrier did not complete")
	}
	if calls.Load() < 2 {
		t.Fatalf("backfill calls = %d, want background scan plus drain barrier", calls.Load())
	}
}

func TestEmbeddingBackfillWorkerDisabled(t *testing.T) {
	worker := newEmbeddingBackfillWorker(context.Background(), config.Config{}, func(context.Context, config.Config) (int, error) {
		t.Fatal("disabled worker must not invoke backfill")
		return 0, nil
	})
	defer worker.Close()
	worker.Notify()
	processed, err := worker.Drain(context.Background())
	if err != nil || processed != 0 {
		t.Fatalf("Drain() = (%d, %v), want (0, nil)", processed, err)
	}
}

func TestEmbeddingBackfillWorkerRetriesSQLiteContention(t *testing.T) {
	cfg := config.Config{MemoryEmbeddingEnabled: true}
	var calls atomic.Int64
	worker := newEmbeddingBackfillWorker(context.Background(), cfg, func(context.Context, config.Config) (int, error) {
		if calls.Add(1) == 1 {
			return 7, codedBackfillError(5)
		}
		return 11, nil
	})
	defer worker.Close()
	processed, err := worker.Drain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if processed != 18 {
		t.Fatalf("processed = %d, want persisted work from both attempts (18)", processed)
	}
	if calls.Load() != 2 {
		t.Fatalf("backfill calls = %d, want 2", calls.Load())
	}
}

func TestRetryableEmbeddingBackfillErrorUsesSQLitePrimaryCode(t *testing.T) {
	if !retryableEmbeddingBackfillError(fmt.Errorf("wrapped: %w", codedBackfillError(5|2<<8))) {
		t.Fatal("extended SQLITE_BUSY code should be retryable")
	}
	if retryableEmbeddingBackfillError(codedBackfillError(19)) {
		t.Fatal("SQLITE_CONSTRAINT should not be retryable")
	}
}
