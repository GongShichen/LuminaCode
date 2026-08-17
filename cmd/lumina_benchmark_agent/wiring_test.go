package main

import "testing"

func TestBenchmarkAgentInjector(t *testing.T) {
	if initializeBenchmarkAgentRunner() == nil {
		t.Fatal("benchmark agent injector returned nil")
	}
}
