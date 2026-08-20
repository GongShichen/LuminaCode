package backend

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/session"
)

type recordingEventClient struct {
	mu     sync.Mutex
	events []any
	closed int
}

func (c *recordingEventClient) write(value any) {
	c.mu.Lock()
	c.events = append(c.events, value)
	c.mu.Unlock()
}

func (c *recordingEventClient) close() {
	c.mu.Lock()
	c.closed++
	c.mu.Unlock()
}

func TestEventHubPublishesConcurrentlyAndClosesOnce(t *testing.T) {
	hub := NewEventHub()
	clients := make([]*recordingEventClient, 16)
	for i := range clients {
		clients[i] = &recordingEventClient{}
		if !hub.Register(clients[i]) {
			t.Fatalf("register client %d", i)
		}
	}
	var wait sync.WaitGroup
	for i := 0; i < 32; i++ {
		wait.Add(1)
		go func(seq int64) {
			defer wait.Done()
			hub.Publish(PushEvent{Type: "event", Seq: seq})
		}(int64(i))
	}
	wait.Wait()
	hub.Close()
	hub.Close()
	for i, client := range clients {
		client.mu.Lock()
		eventCount, closeCount := len(client.events), client.closed
		client.mu.Unlock()
		if eventCount != 32 || closeCount != 1 {
			t.Fatalf("client %d events=%d closes=%d", i, eventCount, closeCount)
		}
	}
	if hub.Register(&recordingEventClient{}) {
		t.Fatal("closed hub accepted a new client")
	}
}

func TestShutdownSignalIsIdempotent(t *testing.T) {
	signal := NewShutdownSignal()
	var wait sync.WaitGroup
	for i := 0; i < 32; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			signal.Request()
		}()
	}
	wait.Wait()
	select {
	case <-signal.Done():
	default:
		t.Fatal("shutdown signal was not closed")
	}
}

type recordingEngineFactory struct {
	inner        agent.QueryEngineFactory
	engine       *agent.QueryEngine
	originalTask *agent.AgentTaskRuntime
}

func (f *recordingEngineFactory) Create(cfg config.Config, identity cluster.RuntimeIdentity) *agent.QueryEngine {
	f.engine = f.inner.Create(cfg, identity)
	f.originalTask = f.engine.CoreEngine.TaskRuntime
	return f.engine
}

func TestSessionRuntimeFactoryCleansPartialAssembly(t *testing.T) {
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	cfg.LongTermMemoryEnabled = false
	cfg.SkillsEnabled = false
	store := session.NewStore(cfg.SessionDir)
	repository := session.NewLocalRepository(store)
	engines := &recordingEngineFactory{inner: agent.NewQueryEngineFactory(agent.NewConfiguredMemoryFabricFactory())}
	factory := NewSessionRuntimeFactory(cfg, repository, engines, func(PushEvent) {})
	var openedJournal session.RuntimeStore
	factory.assemble = func(_ string, journal session.RuntimeStore, _ *agent.QueryEngine) (*agent.RuntimeAssembly, error) {
		openedJournal = journal
		return nil, errors.New("assembly failed")
	}
	if _, err := factory.Create(context.Background(), cluster.RuntimeIdentity{TenantID: session.LocalTenantID},
		"partial-runtime", root, nil, true); err == nil {
		t.Fatal("expected assembly failure")
	}
	if engines.engine == nil || engines.engine.CoreEngine.TaskRuntime == engines.originalTask {
		t.Fatal("query engine was not shut down after partial construction")
	}
	if openedJournal == nil {
		t.Fatal("runtime journal was not opened")
	}
	if _, err := openedJournal.Head(context.Background()); err == nil {
		t.Fatal("runtime journal remained usable after cleanup")
	}
}

func TestDaemonInjectorCleanupClosesListener(t *testing.T) {
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	cfg.LongTermMemoryEnabled = false
	cfg.SkillsEnabled = false
	app, cleanup, err := InitializeDaemonApp(context.Background(), DaemonOptions{
		Host: "127.0.0.1", Config: cfg, EndpointPath: filepath.Join(root, "run", "backend.json"),
	})
	if err != nil {
		t.Fatal(err)
	}
	address := app.listener.Addr().String()
	cleanup()
	conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("listener accepted a connection after Wire cleanup")
	}
}
