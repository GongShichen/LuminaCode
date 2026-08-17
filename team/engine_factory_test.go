package team

import (
	"path/filepath"
	"sync/atomic"
	"testing"

	"LuminaCode/agent"
	"LuminaCode/config"
)

type countingQueryEngineFactory struct {
	inner agent.QueryEngineFactory
	count atomic.Int32
}

func (f *countingQueryEngineFactory) Create(cfg config.Config) *agent.QueryEngine {
	f.count.Add(1)
	return f.inner.Create(cfg)
}

func TestTeamSessionCreatesEveryAgentThroughInjectedFactory(t *testing.T) {
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	cfg.LongTermMemoryEnabled = false
	cfg.SkillsEnabled = false
	factory := &countingQueryEngineFactory{inner: newTestQueryEngineFactory()}
	session := NewSession("parent", cfg, TeamSpec{
		Name: "factory-test", EntryAgent: "lead", AgentSpecs: []TeamAgentSpec{{Name: "lead"}, {Name: "worker"}},
	}, factory, nil, nil)
	defer session.Shutdown()
	if got := factory.count.Load(); got != 2 {
		t.Fatalf("factory calls=%d, want 2", got)
	}
}
