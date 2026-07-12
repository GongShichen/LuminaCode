package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"LuminaCode/config"
)

func TestTrajectoryIngestionPoolRunsPreparationConcurrently(t *testing.T) {
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var active atomic.Int64
	var maximum atomic.Int64
	ingest := func(context.Context, config.Config, map[string]any) (trajectoryIngestionResult, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			observed := maximum.Load()
			if current <= observed || maximum.CompareAndSwap(observed, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		return trajectoryIngestionResult{Messages: 3, Ingested: 3}, nil
	}
	pool := newTrajectoryIngestionPool(context.Background(), config.Config{}, nil, 4, ingest)
	defer pool.Close()
	for index := 0; index < 4; index++ {
		if err := pool.Enqueue(context.Background(), map[string]any{"id": string(rune('a' + index))}); err != nil {
			t.Fatal(err)
		}
	}
	for index := 0; index < 4; index++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("workers did not start concurrently")
		}
	}
	if maximum.Load() < 2 {
		t.Fatalf("maximum concurrent workers = %d, want at least 2", maximum.Load())
	}
	close(release)
	stats, err := pool.Drain(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Submitted != 4 || stats.Completed != 4 || stats.Messages != 12 || stats.Ingested != 12 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestTrajectoryIngestionPoolDrainReportsWorkerFailure(t *testing.T) {
	pool := newTrajectoryIngestionPool(context.Background(), config.Config{}, nil, 2,
		func(context.Context, config.Config, map[string]any) (trajectoryIngestionResult, error) {
			return trajectoryIngestionResult{}, errors.New("prepare failed")
		})
	defer pool.Close()
	if err := pool.Enqueue(context.Background(), map[string]any{"id": "broken"}); err != nil {
		t.Fatal(err)
	}
	stats, err := pool.Drain(context.Background())
	if err == nil || stats.Submitted != 1 || stats.Completed != 0 {
		t.Fatalf("Drain() = (%+v, %v), want one submitted failure", stats, err)
	}
}
