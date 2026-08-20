package main

import "testing"

func TestBenchmarkRuntimeInjector(t *testing.T) {
	runtime := initializeBenchmarkRuntime()
	if runtime == nil || runtime.AgentRunner == nil || runtime.LongMemEvalAnswerRunner == nil || runtime.MemoryFactory == nil {
		t.Fatalf("incomplete benchmark runtime: %#v", runtime)
	}
}
