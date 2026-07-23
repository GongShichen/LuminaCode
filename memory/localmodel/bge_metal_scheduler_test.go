//go:build cgo

package localmodel

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

type fakeBGEMetalTransport struct {
	mu             sync.Mutex
	batches        []int
	includeMulti   []bool
	waitForCancel  bool
	cancelObserved chan struct{}
}

func (f *fakeBGEMetalTransport) Encode(ctx context.Context, inputs []bgeInput,
	includeMulti bool) ([]BGEEmbedding, error) {
	f.mu.Lock()
	f.batches = append(f.batches, len(inputs))
	f.includeMulti = append(f.includeMulti, includeMulti)
	f.mu.Unlock()
	if f.waitForCancel {
		<-ctx.Done()
		select {
		case <-f.cancelObserved:
		default:
			close(f.cancelObserved)
		}
		return nil, ctx.Err()
	}
	result := make([]BGEEmbedding, len(inputs))
	for index, input := range inputs {
		marker := float32(0)
		if len(input.ids) > 0 {
			marker = float32(input.ids[0])
		}
		result[index] = BGEEmbedding{Dense: []float32{marker}}
	}
	return result, nil
}

func (*fakeBGEMetalTransport) Close() error { return nil }

func TestBGEMetalSchedulerBatchesIndependentCaseCalls(t *testing.T) {
	transport := &fakeBGEMetalTransport{}
	scheduler := newBGEMetalSchedulerWithWindow(transport, 20*time.Millisecond)
	defer scheduler.Close()

	const cases = 4
	start := make(chan struct{})
	results := make(chan error, cases)
	var wait sync.WaitGroup
	for caseIndex := 0; caseIndex < cases; caseIndex++ {
		caseIndex := caseIndex
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			encoded, err := scheduler.Encode(context.Background(), []bgeInput{{
				ids: []int64{int64(caseIndex + 1), 2},
			}}, false)
			if err == nil && (len(encoded) != 1 || len(encoded[0].Dense) != 1 ||
				encoded[0].Dense[0] != float32(caseIndex+1)) {
				err = fmt.Errorf("case %d received %+v", caseIndex, encoded)
			}
			results <- err
		}()
	}
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.batches) != 1 || transport.batches[0] != cases {
		t.Fatalf("Metal batches = %v, want one batch of %d independent cases",
			transport.batches, cases)
	}
}

func TestBGEMetalSchedulerKeepsOutputContractsSeparate(t *testing.T) {
	transport := &fakeBGEMetalTransport{}
	scheduler := newBGEMetalSchedulerWithWindow(transport, 10*time.Millisecond)
	defer scheduler.Close()

	start := make(chan struct{})
	var wait sync.WaitGroup
	for _, includeMulti := range []bool{false, true} {
		includeMulti := includeMulti
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			if _, err := scheduler.Encode(context.Background(), []bgeInput{{ids: []int64{1}}},
				includeMulti); err != nil {
				t.Errorf("Encode: %v", err)
			}
		}()
	}
	close(start)
	wait.Wait()
	transport.mu.Lock()
	defer transport.mu.Unlock()
	if len(transport.batches) != 2 {
		t.Fatalf("Metal batches = %v, want separate channel contracts", transport.batches)
	}
	if transport.includeMulti[0] == transport.includeMulti[1] {
		t.Fatalf("Metal include_multi calls were not separated: %v", transport.includeMulti)
	}
}

func TestBGEMetalSchedulerCancelsTransportWhenEveryCaseCancels(t *testing.T) {
	transport := &fakeBGEMetalTransport{
		waitForCancel:  true,
		cancelObserved: make(chan struct{}),
	}
	scheduler := newBGEMetalSchedulerWithWindow(transport, time.Millisecond)
	defer scheduler.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := scheduler.Encode(ctx, []bgeInput{{ids: []int64{1}}}, false); err == nil {
		t.Fatal("Encode unexpectedly succeeded")
	}
	select {
	case <-transport.cancelObserved:
	case <-time.After(time.Second):
		t.Fatal("Metal transport did not observe cancellation")
	}
}
