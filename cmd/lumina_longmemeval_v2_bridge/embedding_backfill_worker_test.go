package main

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"LuminaCode/config"
)

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
