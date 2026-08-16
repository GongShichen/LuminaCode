package harness

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestHookPointOrderingWaterfallAndDispose(t *testing.T) {
	var calls []string
	var point HookPoint[string]
	disposeLate := point.Register(20, func(_ context.Context, value string) (HookResult[string], error) {
		calls = append(calls, "late")
		return HookResult[string]{Value: value + "b"}, nil
	})
	point.Register(10, func(_ context.Context, value string) (HookResult[string], error) {
		calls = append(calls, "early")
		return HookResult[string]{Value: value + "a"}, nil
	})
	result, err := point.Run(context.Background(), "")
	if err != nil || result.Value != "ab" || !reflect.DeepEqual(calls, []string{"early", "late"}) {
		t.Fatalf("unexpected waterfall: result=%#v calls=%v err=%v", result, calls, err)
	}
	disposeLate()
	disposeLate()
	result, err = point.Run(context.Background(), "")
	if err != nil || result.Value != "a" {
		t.Fatalf("dispose must be idempotent: result=%#v err=%v", result, err)
	}
}

func TestHookPointStopsOnError(t *testing.T) {
	var point HookPoint[int]
	point.Register(0, func(_ context.Context, value int) (HookResult[int], error) {
		return HookResult[int]{Value: value + 1}, errors.New("stop")
	})
	result, err := point.Run(context.Background(), 1)
	if err == nil || result.Value != 1 {
		t.Fatalf("failed hook must return last committed waterfall value: %#v %v", result, err)
	}
}
