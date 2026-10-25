package agent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"LuminaCode/cluster"
	"LuminaCode/harness"
	coretools "LuminaCode/tools"
)

func admissionServer(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	calls := new(atomic.Int32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(server.Close)
	return server, calls
}

func TestAgentToolRejectsOldRunAfterReset(t *testing.T) {
	for _, background := range []bool{false, true} {
		for _, detach := range []bool{false, true} {
			t.Run(fmt.Sprintf("background=%v/detached_context=%v", background, detach), func(t *testing.T) {
				server, calls := admissionServer(t)
				cfg := lifecycleConfig(t, server.URL)
				engine := NewCoreExecutionEngine(cfg, cluster.RuntimeIdentity{TenantID: "local"}, nil)
				t.Cleanup(engine.Shutdown)
				oldCtx, draft, err := engine.AgentRuntime.Begin(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				oldExec := coretools.ExecutionContext{"config": cfg, "_registry": coretools.NewToolRegistry(coretools.NewReadFileTool()), "task_runtime": engine.TaskRuntime, "_session_runtime": engine.SessionRuntime, "scope_id": "main", "agent_services": engine.AgentRuntime.ServicesFor(draft.RunToken())}
				if err = engine.Reset(context.Background()); err != nil {
					t.Fatal(err)
				}
				if detach {
					oldCtx = context.WithoutCancel(oldCtx)
				}
				_, err = NewAgentTool().Execute(oldCtx, oldExec, AgentInput{Description: "stale tool", Prompt: "task", SubagentType: "Explore", RunInBackground: background})
				if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrStaleState) {
					t.Fatalf("stale tool admitted: %v", err)
				}
				if len(engine.TaskRuntime.ListTasks("main")) != 0 || len(engine.SessionRuntime.Snapshots()) != 1 || calls.Load() != 0 {
					t.Fatal("rejected tool left a task, child runtime, or model request")
				}
			})
		}
	}
}

func TestSpawnWorkerRejectsCancelledCaller(t *testing.T) {
	cfg := lifecycleConfig(t, "")
	tasks := NewAgentTaskRuntime()
	defer tasks.Shutdown()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	record, err := tasks.SpawnWorker(ctx, cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, "test", "task", "worker", "", "worker", false, "main", "", nil)
	if !errors.Is(err, context.Canceled) || record != nil || len(tasks.ListTasks("main")) != 0 {
		t.Fatalf("cancelled spawn accepted: record=%+v error=%v", record, err)
	}
}

func TestSpawnPreparationCannotRefreshExpiredSessionLease(t *testing.T) {
	cfg := lifecycleConfig(t, "")
	tasks := NewAgentTaskRuntime()
	defer tasks.Shutdown()
	session := NewSessionRuntime()
	old := session.lease()
	session.Reset(NewAgentRuntime(cfg, nil))
	record, err := tasks.SpawnWorker(context.Background(), cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, "test", "task", "worker", "", "worker", false, "main", "", coretools.ExecutionContext{"_session_runtime": session, "_session_lease": old})
	if !errors.Is(err, ErrStaleState) || record != nil {
		t.Fatalf("old session admitted a child: record=%+v error=%v", record, err)
	}
	if len(tasks.ListTasks("main")) != 0 {
		t.Fatal("rejected admission published a task")
	}
}

func TestConcurrentSpawnAndSessionReset(t *testing.T) {
	cfg := lifecycleConfig(t, "")
	cfg.APIKey = ""
	for i := 0; i < 50; i++ {
		main := NewAgentRuntime(cfg, nil)
		ctx, state, err := main.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		session := NewSessionRuntime()
		session.Register("main", main)
		tasks := NewAgentTaskRuntime()
		extra := coretools.ExecutionContext{"_session_runtime": session, "_session_lease": session.lease(), "agent_services": main.ServicesFor(state.RunToken())}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var record *AgentTaskRecord
		var spawnErr, resetErr error
		go func() {
			defer wg.Done()
			<-start
			record, spawnErr = tasks.SpawnWorker(ctx, cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, "test", "task", "worker", "", "worker", false, "main", "", extra)
		}()
		go func() {
			defer wg.Done()
			<-start
			resetErr = main.Reset(context.Background())
			session.Reset(main)
			tasks.Shutdown()
		}()
		close(start)
		wg.Wait()
		if resetErr != nil {
			t.Fatal(resetErr)
		}
		if spawnErr != nil && !errors.Is(spawnErr, context.Canceled) && !errors.Is(spawnErr, ErrStaleState) {
			t.Fatal(spawnErr)
		}
		if snapshots := session.Snapshots(); len(snapshots) != 1 {
			t.Fatalf("late child repopulated reset session: %+v", snapshots)
		}
		if record != nil {
			row := tasks.GetTask(record.TaskID, "main")
			if row == nil || (row.Status != "killed" && row.Status != "failed") {
				t.Fatalf("reset left worker active: %+v", row)
			}
		}
		waitAdmissionWorkers(t, tasks)
		tasks.Shutdown()
	}
}

func waitAdmissionWorkers(t *testing.T, tasks *AgentTaskRuntime) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		tasks.mu.Lock()
		n := len(tasks.workers)
		tasks.mu.Unlock()
		if n == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("worker cleanup did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestAdmittedChildSurvivesParentCompletion(t *testing.T) {
	server, calls := admissionServer(t)
	cfg := lifecycleConfig(t, server.URL)
	cfg.LongTermMemoryEnabled = true
	cfg.MemoryBackend = "fabric"
	parent, ctx, state := phaseFixture(t)
	session := NewSessionRuntime()
	session.Register("main", parent)
	tasks := NewAgentTaskRuntime()
	defer tasks.Shutdown()
	mem := &lifecycleBlockingMemory{entered: make(chan struct{}), release: make(chan struct{})}
	record, err := tasks.SpawnWorker(ctx, cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, "test", "task", "worker", "", "worker", true, "main", "", coretools.ExecutionContext{"_memory_engine": mem, "_session_runtime": session, "agent_services": parent.ServicesFor(state.RunToken())})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-mem.entered:
	case <-time.After(time.Second):
		t.Fatal("child preparation did not start")
	}
	if err = parent.EndRun(state.RunToken()); err != nil {
		t.Fatal(err)
	}
	close(mem.release)
	deadline, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tasks.WaitForTasks(deadline, []string{record.TaskID}, "main", 2)
	if row := tasks.GetTask(record.TaskID, "main"); row == nil || row.Status != "idle" || calls.Load() != 1 {
		t.Fatalf("parent completion cancelled admitted child: %+v calls=%d", row, calls.Load())
	}
	// Reuse must retain the session lease without inheriting the completed parent's run guard.
	if _, ok := tasks.SendMessage(record.TaskID, "main", "another task").(*AgentTaskRecord); !ok {
		t.Fatal("reusable child rejected a follow-up")
	}
	tasks.WaitForTasks(deadline, []string{record.TaskID}, "main", 2)
	if row := tasks.GetTask(record.TaskID, "main"); row.Status != "idle" || calls.Load() != 2 {
		t.Fatalf("reused child inherited parent lifetime: %+v calls=%d", row, calls.Load())
	}
}

type admissionBlockingStore struct {
	harness.EventStore
	entered     chan context.Context
	release     chan struct{}
	cooperative bool
	failure     error
	appends     atomic.Int32
}

func (s *admissionBlockingStore) CreateStream(ctx context.Context, _ harness.StreamDescriptor) error {
	s.entered <- ctx
	if s.failure != nil {
		return s.failure
	}
	if s.cooperative {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.release:
		}
	} else {
		<-s.release
	}
	return nil
}
func (s *admissionBlockingStore) Append(_ context.Context, _ int64, events ...harness.PendingEvent) ([]harness.Event, error) {
	s.appends.Add(1)
	out := make([]harness.Event, len(events))
	for i := range out {
		out[i].Seq = int64(i + 1)
	}
	return out, nil
}

func TestAbortDuringStoreBindingCancelsPreparation(t *testing.T) {
	for _, cooperative := range []bool{false, true} {
		t.Run(fmt.Sprint(cooperative), func(t *testing.T) {
			server, calls := admissionServer(t)
			cfg := lifecycleConfig(t, server.URL)
			store := &admissionBlockingStore{entered: make(chan context.Context, 1), release: make(chan struct{}), cooperative: cooperative}
			var once sync.Once
			release := func() { once.Do(func() { close(store.release) }) }
			defer release()
			sub := NewSubAgent(cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, "", "worker", coretools.ExecutionContext{"_runtime_store": store})
			done := make(chan error, 1)
			go func() { _, err := sub.Run(context.Background(), "task"); done <- err }()
			var storeCtx context.Context
			select {
			case storeCtx = <-store.entered:
			case <-time.After(time.Second):
				t.Fatal("store initialization did not start")
			}
			aborted := make(chan struct{})
			go func() { sub.Abort(); close(aborted) }()
			select {
			case <-aborted:
			case <-time.After(time.Second):
				release()
				t.Fatal("Abort blocked on storage")
			}
			if storeCtx.Err() == nil {
				t.Fatal("storage did not receive the run's cancellation")
			}
			if !cooperative {
				release()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancelled store binding did not finish")
			}
			phaseIs(t, sub.Runtime, AgentPhaseCancelled)
			if calls.Load() != 0 || sub.Runtime.HasDurableStore() || store.appends.Load() != 0 {
				t.Fatal("cancelled binding published a store, journal event, or model request")
			}
		})
	}
}

func TestResetWhileChildStoreBindingRejectsLateSuccess(t *testing.T) {
	server, calls := admissionServer(t)
	cfg := lifecycleConfig(t, server.URL)
	session := NewSessionRuntime()
	main := NewAgentRuntime(cfg, nil)
	session.Register("main", main)
	store := &admissionBlockingStore{entered: make(chan context.Context, 1), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(store.release) }) }
	defer release()
	sub := NewSubAgent(cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, "", "worker", coretools.ExecutionContext{"_runtime_store": store, "_session_runtime": session})
	done := make(chan error, 1)
	go func() { _, err := sub.Run(context.Background(), "task"); done <- err }()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("store initialization did not start")
	}
	reset := make(chan struct{})
	go func() { session.Reset(main); close(reset) }()
	select {
	case <-reset:
	case <-time.After(time.Second):
		release()
		t.Fatal("session Reset blocked on child storage")
	}
	release()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrStaleState) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("late initialization did not finish")
	}
	if len(session.Snapshots()) != 1 || calls.Load() != 0 || sub.Runtime.HasDurableStore() || store.appends.Load() != 0 {
		t.Fatal("late initialization crossed Reset")
	}
}

func TestStoreInitializationFailureFailsRun(t *testing.T) {
	server, calls := admissionServer(t)
	cfg := lifecycleConfig(t, server.URL)
	failure := errors.New("storage unavailable")
	store := &admissionBlockingStore{entered: make(chan context.Context, 1), failure: failure}
	sub := NewSubAgent(cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, "", "worker", coretools.ExecutionContext{"_runtime_store": store})
	_, err := sub.Run(context.Background(), "task")
	if !errors.Is(err, failure) {
		t.Fatalf("initialization error lost: %v", err)
	}
	phaseIs(t, sub.Runtime, AgentPhaseFailed)
	if calls.Load() != 0 || sub.Runtime.HasDurableStore() || store.appends.Load() != 0 {
		t.Fatal("failed initialization continued execution")
	}
}

type admissionFilterGate struct {
	coretools.BaseTool
	entered, release chan struct{}
}

func (g *admissionFilterGate) IsReadOnly(any) bool { close(g.entered); <-g.release; return true }

func TestAgentToolRechecksParentAfterPreparation(t *testing.T) {
	cfg := lifecycleConfig(t, "")
	engine := NewCoreExecutionEngine(cfg, cluster.RuntimeIdentity{TenantID: "local"}, nil)
	defer engine.Shutdown()
	ctx, state, err := engine.AgentRuntime.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	gate := &admissionFilterGate{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{Name: "read_file"}}, entered: make(chan struct{}), release: make(chan struct{})}
	extra := coretools.ExecutionContext{"config": cfg, "_registry": coretools.NewToolRegistry(gate), "task_runtime": engine.TaskRuntime, "_session_runtime": engine.SessionRuntime, "agent_services": engine.AgentRuntime.ServicesFor(state.RunToken())}
	done := make(chan error, 1)
	go func() {
		_, err := NewAgentTool().Execute(ctx, extra, AgentInput{Description: "preparing", Prompt: "task", SubagentType: "Explore", RunInBackground: true})
		done <- err
	}()
	select {
	case <-gate.entered:
	case <-time.After(time.Second):
		t.Fatal("tool did not reach preparation")
	}
	engine.AgentRuntime.Abort()
	close(gate.release)
	select {
	case err = <-done:
		if !errors.Is(err, context.Canceled) && !errors.Is(err, ErrStaleState) {
			t.Fatalf("cancelled preparation was admitted: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("preparation did not finish")
	}
	if len(engine.TaskRuntime.ListTasks("main")) != 0 {
		t.Fatal("cancelled tool preparation created a task")
	}
	_ = engine.AgentRuntime.EndRun(state.RunToken())
}

func TestAgentToolRejectsPreviousRunWithinSameGeneration(t *testing.T) {
	cfg := lifecycleConfig(t, "")
	engine := NewCoreExecutionEngine(cfg, cluster.RuntimeIdentity{TenantID: "local"}, nil)
	defer engine.Shutdown()
	oldCtx, old, err := engine.AgentRuntime.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	oldServices := engine.AgentRuntime.ServicesFor(old.RunToken())
	if err = engine.AgentRuntime.EndRun(old.RunToken()); err != nil {
		t.Fatal(err)
	}
	_, fresh, err := engine.AgentRuntime.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.AgentRuntime.EndRun(fresh.RunToken())
	_, err = NewAgentTool().Execute(context.WithoutCancel(oldCtx), coretools.ExecutionContext{"config": cfg, "_registry": coretools.NewToolRegistry(coretools.NewReadFileTool()), "task_runtime": engine.TaskRuntime, "_session_runtime": engine.SessionRuntime, "agent_services": oldServices}, AgentInput{Description: "old run", Prompt: "task", SubagentType: "Explore", RunInBackground: true})
	if !errors.Is(err, ErrStaleState) || len(engine.TaskRuntime.ListTasks("main")) != 0 {
		t.Fatalf("previous run admitted a task: %v", err)
	}
	if engine.AgentRuntime.RunSnapshot().Token != fresh.RunToken() {
		t.Fatal("rejected tool changed the new run")
	}
}

func TestShutdownExpiresChildAdmission(t *testing.T) {
	cfg := lifecycleConfig(t, "")
	engine := NewCoreExecutionEngine(cfg, cluster.RuntimeIdentity{}, nil)
	tasks := engine.TaskRuntime
	extra := coretools.ExecutionContext{"_session_runtime": engine.SessionRuntime, "_session_lease": engine.SessionRuntime.lease()}
	engine.Shutdown()
	record, err := tasks.SpawnWorker(context.Background(), cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, "late", "task", "worker", "", "worker", false, "main", "", extra)
	if !errors.Is(err, ErrStaleState) || record != nil {
		t.Fatalf("closed session admitted a child: %+v %v", record, err)
	}
}
