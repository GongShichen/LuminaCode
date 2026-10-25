package team

import (
	"LuminaCode/agent"
	"LuminaCode/cluster"
	"LuminaCode/config"
	"context"
	"encoding/json"
	"sync"
	"testing"
)

func newStateTestSession(t *testing.T) (*Session, *agent.QueryEngine, config.Config) {
	t.Helper()
	cfg := config.Config{ContextInputTokenLimit: 16000, SessionDir: t.TempDir()}
	s := NewSession("parent", cfg, TeamSpec{}, nil, nil, nil)
	s.persistEnabled = false
	engine := agent.NewQueryEngine(cfg, agent.NewCoreExecutionEngine(cfg, cluster.RuntimeIdentity{TenantID: "local"}, nil))
	t.Cleanup(engine.Shutdown)
	s.agents["worker"] = &AgentRuntime{Engine: engine}
	return s, engine, cfg
}

func TestTeamCheckpointAndPermissionUpdatesUseOwnedSnapshots(t *testing.T) {
	s, engine, cfg := newStateTestSession(t)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			c := cfg
			c.Yolo = i%2 == 0
			s.ApplyRuntimeConfig(c)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			if _, err := json.Marshal(s.ExportRuntimeCheckpoint()); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	cfg.Yolo = true
	s.ApplyRuntimeConfig(cfg)
	checkpoint := s.ExportRuntimeCheckpoint()
	if !engine.Snapshot().Permissions.Policy.YoloMode || !checkpoint.AgentSnapshots["worker"].Permissions.Policy.YoloMode {
		t.Fatal("permission updates did not reach the owned state")
	}
	legacy := checkpoint.AgentStates["worker"]
	legacy.PermissionState.YoloMode = false
	if !engine.Snapshot().Permissions.Policy.YoloMode {
		t.Fatal("checkpoint exposes a mutable permission policy")
	}
}

func TestTeamCheckpointIncludesAsyncMemoryProgress(t *testing.T) {
	s, engine, _ := newStateTestSession(t)
	initial := s.ExportRuntimeCheckpoint()
	snapshot := engine.Snapshot()
	engine.CoreEngine.AgentRuntime.AdvanceMemory(snapshot.Generation, 99)
	checkpoint := s.ExportRuntimeCheckpoint()
	if checkpoint.AgentSnapshots["worker"].Memory.ExtractionSeq != 99 {
		t.Fatal("checkpoint used a stale external state copy")
	}
	if engine.Snapshot().Generation != snapshot.Generation {
		t.Fatal("reading a checkpoint restored the agent")
	}
	if initial.AgentSnapshots["worker"].Memory.ExtractionSeq != 0 {
		t.Fatal("published snapshot changed in place")
	}
	// Policy changes are commands; they must not restore the older checkpoint.
	if err := engine.CoreEngine.AgentRuntime.SetYolo(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if engine.Snapshot().Memory.ExtractionSeq != 99 {
		t.Fatal("permission update rolled back asynchronous progress")
	}
}
