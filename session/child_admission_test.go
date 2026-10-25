package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/memory"
	coretools "LuminaCode/tools"
)

type admissionRecallGate struct {
	memory.Engine
	entered, release chan struct{}
}

func (g *admissionRecallGate) Search(context.Context, memory.SearchRequest) (memory.SearchResult, error) {
	close(g.entered)
	<-g.release
	return memory.SearchResult{}, nil
}

func TestChildPreparationCannotResurrectClear(t *testing.T) {
	runChildPreparationCannotResurrectClear(t, checkpointFixture)
}
func TestPostgresChildPreparationCannotResurrectClear(t *testing.T) {
	runChildPreparationCannotResurrectClear(t, postgresCheckpointFixture)
}
func runChildPreparationCannotResurrectClear(t *testing.T, fixture func(*testing.T) (RuntimeStore, *agent.AgentRuntime)) {
	t.Helper()
	journal, owner := fixture(t)
	dir := t.TempDir()
	cfg := config.Config{CWD: dir, ProjectRuntimeDir: dir, SessionDir: dir, APIMaxTokens: 128, ContextInputTokenLimit: 16000}
	engine := agent.NewCoreExecutionEngine(cfg, cluster.RuntimeIdentity{TenantID: "local"}, nil)
	engine.AgentRuntime = owner
	if err := engine.SessionRuntime.Register("main", owner); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(engine.Shutdown)
	childCfg := cfg
	childCfg.LongTermMemoryEnabled = true
	childCfg.MemoryBackend = "fabric"
	gate := &admissionRecallGate{entered: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(gate.release) }) }
	defer release()
	sub := agent.NewSubAgent(childCfg, coretools.NewToolRegistry(), agent.AgentDef{Name: "worker"}, nil, "", "worker", coretools.ExecutionContext{"_session_runtime": engine.SessionRuntime, "_runtime_store": journal, "_memory_engine": gate, "scope_id": "preparing-child"})
	done := make(chan error, 1)
	go func() { _, err := sub.Run(context.Background(), "old child request"); done <- err }()
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("child did not reach preparation")
	}
	events, err := journal.LoadStream(context.Background(), "preparing-child", 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatal("fixture must pause before the child's first checkpoint")
	}
	if err = engine.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	clearedHead, err := journal.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, agent.ErrStaleState) {
			t.Fatalf("late child result: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late child did not finish")
	}
	head, err := journal.Head(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if head != clearedHead {
		t.Fatalf("child wrote durable events after Clear: head %d -> %d", clearedHead, head)
	}
	restored := agent.NewSessionRuntime()
	if err = restored.RestoreChildren(context.Background(), journal, cfg, journal.SessionID()); err != nil {
		t.Fatal(err)
	}
	if len(restored.Snapshots()) != 0 {
		t.Fatalf("restart resurrected a retired child: %+v", restored.Snapshots())
	}
	if len(owner.Snapshot().Transcript) != 0 {
		t.Fatal("Clear lost its empty transcript")
	}
}
