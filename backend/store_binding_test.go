package backend

import (
	"context"
	"errors"
	"testing"

	"LuminaCode/agent"
	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/harness"
	"LuminaCode/session"
)

type failingBindingStore struct {
	session.RuntimeStore
	err error
}

func (s *failingBindingStore) CreateStream(context.Context, harness.StreamDescriptor) error {
	return s.err
}

func TestControllerRejectsFailedStoreBinding(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Config{CWD: dir, ProjectRuntimeDir: dir, SessionDir: dir}
	engine := agent.NewQueryEngine(cfg, agent.NewCoreExecutionEngine(cfg, cluster.RuntimeIdentity{}, nil))
	defer engine.Shutdown()
	failure := errors.New("stream creation failed")
	controller, err := NewSessionController(context.Background(), cluster.RuntimeIdentity{}, "failed-binding", cfg, engine, nil, nil, &failingBindingStore{err: failure}, nil)
	if !errors.Is(err, failure) || controller != nil {
		t.Fatalf("controller silently started without persistence: controller=%v error=%v", controller, err)
	}
	if engine.CoreEngine.AgentRuntime.HasDurableStore() {
		t.Fatal("failed store binding was published")
	}
}
