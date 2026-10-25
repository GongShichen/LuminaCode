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

	"LuminaCode/config"
	"LuminaCode/harness"
	"LuminaCode/memory"
	coretools "LuminaCode/tools"
)

func lifecycleConfig(t *testing.T, endpoint string) config.Config {
	dir := t.TempDir()
	return config.Config{CWD: dir, SessionDir: dir, ProjectRuntimeDir: dir, APIType: "openai_compatible", APIBaseURL: endpoint, APIKey: "test-key", APIModel: "review-model", APIMaxTokens: 128, ContextInputTokenLimit: 16000, MaxToolOutputChars: 50000}
}
func lifecycleServer(t *testing.T, reply string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, reply)
	}))
	t.Cleanup(s.Close)
	return s
}

type lifecycleBlockingMemory struct {
	fabricEngineStub
	entered, release chan struct{}
}

func (m *lifecycleBlockingMemory) Search(ctx context.Context, _ memory.SearchRequest) (memory.SearchResult, error) {
	close(m.entered)
	select {
	case <-m.release:
		return memory.SearchResult{}, nil
	case <-ctx.Done():
		return memory.SearchResult{}, ctx.Err()
	}
}

func TestAbortDuringSubagentPreparationStopsModel(t *testing.T) {
	for _, entry := range []string{"run", "request"} {
		t.Run(entry, func(t *testing.T) {
			var modelCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				modelCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer server.Close()
			cfg := lifecycleConfig(t, server.URL)
			cfg.LongTermMemoryEnabled = true
			cfg.MemoryBackend = "fabric"
			mem := &lifecycleBlockingMemory{entered: make(chan struct{}), release: make(chan struct{})}
			sub := NewSubAgent(cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker", MaxTurns: 1}, nil, "", "worker", coretools.ExecutionContext{"_memory_engine": mem})
			done := make(chan error, 1)
			go func() {
				if entry == "run" {
					_, err := sub.Run(context.Background(), "task")
					done <- err
				} else {
					done <- sub.ExecuteOneRequest(context.Background(), "task", nil).Error
				}
			}()
			select {
			case <-mem.entered:
			case <-time.After(2 * time.Second):
				t.Fatal("preparation did not start")
			}
			sub.runtimeMu.RLock()
			runtime := sub.Runtime
			sub.runtimeMu.RUnlock()
			phaseIs(t, runtime, AgentPhasePreparing)
			if _, err := sub.Run(context.Background(), "another task"); !errors.Is(err, ErrAgentBusy) {
				t.Fatalf("concurrent run admitted during preparation: %v", err)
			}
			sub.Abort()
			close(mem.release)
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation not propagated: %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("run did not finish")
			}
			if modelCalls.Load() != 0 {
				t.Fatal("preparation-stage cancellation was lost")
			}
			phaseIs(t, runtime, AgentPhaseCancelled)
		})
	}
}

type lifecycleFailStore struct {
	harness.EventStore
	calls, failAt int
}

func (*lifecycleFailStore) CreateStream(context.Context, harness.StreamDescriptor) error { return nil }
func (s *lifecycleFailStore) Append(_ context.Context, _ int64, events ...harness.PendingEvent) ([]harness.Event, error) {
	s.calls++
	if s.calls >= s.failAt {
		return nil, errors.New("disk unavailable")
	}
	out := make([]harness.Event, len(events))
	for i := range out {
		out[i].Seq = int64(s.calls*10 + i + 1)
	}
	return out, nil
}

func TestTimeoutFinalizerOutcomes(t *testing.T) {
	success := "data: {\"choices\":[{\"delta\":{\"content\":\"final answer\"},\"finish_reason\":\"stop\"}]}\n\ndata: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n"
	for _, tc := range []struct {
		name, reply   string
		status        int
		setup, budget bool
		failAt        int
		want          AgentPhase
	}{
		{name: "success", reply: success, want: AgentPhaseCompleted},
		{name: "setup", setup: true, want: AgentPhaseFailed},
		{name: "empty", reply: "data: [DONE]\n\n", want: AgentPhaseFailed},
		{name: "stream_event", reply: "data: {\"error\":{\"message\":\"invalid API key\",\"type\":\"invalid_api_key\"}}\n\ndata: [DONE]\n\n", want: AgentPhaseFailed},
		{name: "transport", status: http.StatusUnauthorized, reply: "invalid API key", want: AgentPhaseFailed},
		{name: "budget", budget: true, want: AgentPhaseFailed},
		{name: "initial_commit", reply: success, failAt: 1, want: AgentPhaseFailed},
		{name: "final_commit", reply: success, failAt: 4, want: AgentPhaseFailed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if tc.status != 0 {
					w.WriteHeader(tc.status)
				}
				fmt.Fprint(w, tc.reply)
			}))
			defer server.Close()
			cfg := lifecycleConfig(t, server.URL)
			if tc.setup {
				cfg.APIKey = ""
			}
			if tc.budget {
				cfg.ContextInputTokenLimit = 1
			}
			initial := NewAgentState()
			initial.SystemPrompt = "system"
			initial.Messages = []map[string]any{{"role": "user", "content": "task"}}
			state := &SubAgentSessionState{Runtime: NewAgentRuntime(cfg, &initial), SystemPrompt: "system", Messages: initial.Messages}
			if tc.failAt > 0 {
				state.Runtime.BindStore(&lifecycleFailStore{failAt: tc.failAt}, "child")
			}
			sub := NewSubAgent(cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, "", "worker", coretools.ExecutionContext{})
			result := sub.finalizeTimedOutRun(context.Background(), "task", state, "partial output")
			phaseIs(t, state.Runtime, tc.want)
			if tc.want == AgentPhaseFailed {
				if result.Error == nil || state.Runtime.RunSnapshot().Error == "" {
					t.Fatal("finalizer failure was not recorded")
				}
				if tc.name != "final_commit" && result.FinalText != "partial output" {
					t.Fatal("failed recovery lost partial progress")
				}
			} else {
				if result.Error != nil || result.FinalText != "final answer" {
					t.Fatalf("finalizer failed: %+v", result)
				}
				if result.TotalInputTokens != 7 || result.TotalOutputTokens != 3 {
					t.Fatalf("finalizer usage lost: %+v", result)
				}
			}
		})
	}
}

func TestAbortDuringTimeoutFinalizer(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
	}))
	defer server.Close()
	cfg := lifecycleConfig(t, server.URL)
	initial := NewAgentState()
	initial.SystemPrompt = "system"
	initial.Messages = []map[string]any{{"role": "user", "content": "task"}}
	state := &SubAgentSessionState{Runtime: NewAgentRuntime(cfg, &initial), SystemPrompt: "system", Messages: initial.Messages}
	sub := NewSubAgent(cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, "", "worker", coretools.ExecutionContext{})
	done := make(chan SubAgentRequestResult, 1)
	go func() { done <- sub.finalizeTimedOutRun(context.Background(), "task", state, "partial") }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("finalizer did not start")
	}
	sub.Abort()
	select {
	case result := <-done:
		if !errors.Is(result.Error, context.Canceled) {
			t.Fatalf("cancelled finalizer lost error: %v", result.Error)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("finalizer ignored abort")
	}
	phaseIs(t, state.Runtime, AgentPhaseCancelled)
}

func TestAbortBetweenTimeoutAndFinalizerPreventsRestart(t *testing.T) {
	cfg := lifecycleConfig(t, "http://127.0.0.1:1")
	runtime := NewAgentRuntime(cfg, nil)
	_, draft, err := runtime.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	runtime.cancelRun(draft.RunToken(), context.DeadlineExceeded.Error())
	_ = runtime.EndRun(draft.RunToken())
	sub := NewSubAgent(cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, "", "worker", coretools.ExecutionContext{})
	sub.Runtime = runtime
	sub.Abort()
	state := &SubAgentSessionState{Runtime: runtime, draft: draft, Messages: []map[string]any{{"role": "user", "content": "task"}}}
	result := sub.finalizeTimedOutRun(context.Background(), "task", state, "partial")
	if !errors.Is(result.Error, context.Canceled) {
		t.Fatalf("recovery restarted after user abort: %v", result.Error)
	}
	if runtime.RunSnapshot().Token != draft.RunToken() {
		t.Fatal("aborted request accepted a new recovery lease")
	}
	phaseIs(t, runtime, AgentPhaseCancelled)
}

func TestAbortFencesTimeoutRecoveryAdmission(t *testing.T) {
	for i := 0; i < 100; i++ {
		r, _, draft := phaseFixture(t)
		r.cancelRun(draft.RunToken(), context.DeadlineExceeded.Error())
		_ = r.EndRun(draft.RunToken())
		var next *AgentState
		var beginErr error
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; r.Abort() }()
		go func() {
			defer wg.Done()
			<-start
			_, next, beginErr = r.beginRecoveryRun(context.Background(), draft.RunToken(), 128)
		}()
		close(start)
		wg.Wait()
		if beginErr != nil && !errors.Is(beginErr, context.Canceled) {
			t.Fatal(beginErr)
		}
		if next != nil {
			if r.RunSnapshot().Phase != AgentPhaseCancelling {
				t.Fatal("recovery escaped cancellation")
			}
			_ = r.EndRun(next.RunToken())
		}
		phaseIs(t, r, AgentPhaseCancelled)
		_, fresh, err := r.Begin(context.Background())
		if err != nil {
			t.Fatal("old cancellation blocked a fresh request", err)
		}
		if _, _, err = r.beginRecoveryRun(context.Background(), draft.RunToken(), 128); !errors.Is(err, ErrStaleState) {
			t.Fatal("old recovery replaced a fresh request")
		}
		_ = r.EndRun(fresh.RunToken())
	}
}

func TestSubagentRunPropagatesStructuredFailure(t *testing.T) {
	cfg := lifecycleConfig(t, "http://127.0.0.1:1")
	cfg.APIKey = ""
	sub := NewSubAgent(cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker", MaxTurns: 1}, nil, "", "worker", coretools.ExecutionContext{})
	result, err := sub.Run(context.Background(), "task")
	t.Logf("phase=%s returned_error=%v output=%s", sub.Runtime.RunSnapshot().Phase, err, result)
	if err == nil {
		t.Fatal("Run discarded the failed child execution's error")
	}
}

func TestFailedWorkerDoesNotReportCompleted(t *testing.T) {
	for _, reusable := range []bool{false, true} {
		t.Run(fmt.Sprint(reusable), func(t *testing.T) {
			cfg := lifecycleConfig(t, "http://127.0.0.1:1")
			cfg.APIKey = ""
			tasks := NewAgentTaskRuntime()
			defer tasks.Shutdown()
			registry := NewSessionRuntime()
			record, spawnErr := tasks.SpawnWorker(context.Background(), cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker", MaxTurns: 1}, nil, "test worker", "task", "worker", "", "worker", reusable, "main", "", coretools.ExecutionContext{"_session_runtime": registry})
			if spawnErr != nil {
				t.Fatal(spawnErr)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			tasks.WaitForTasks(ctx, []string{record.TaskID}, "main", 3)
			rows := tasks.ListTasks("main")
			if len(rows) != 1 || rows[0].Status != "failed" || rows[0].ErrorText == "" {
				t.Fatalf("failed child reported as successful: %+v", rows)
			}
			child := registry.Snapshots()[record.TaskID]
			if child.Run == nil || child.Run.Phase != AgentPhaseFailed {
				t.Fatalf("unexpected child phase: %+v", child.Run)
			}
			if reusable {
				if _, ok := tasks.SendMessage(record.TaskID, "main", "retry").(string); !ok {
					t.Fatal("failed worker was left reusable")
				}
			}
		})
	}
}

func TestStoppedWorkerCannotPublishLateCompletion(t *testing.T) {
	cfg := lifecycleConfig(t, "http://127.0.0.1:1")
	cfg.LongTermMemoryEnabled = true
	cfg.MemoryBackend = "fabric"
	mem := &lifecycleBlockingMemory{entered: make(chan struct{}), release: make(chan struct{})}
	tasks := NewAgentTaskRuntime()
	defer tasks.Shutdown()
	registry := NewSessionRuntime()
	parent, _, state := phaseFixture(t)
	registry.Register("main", parent)
	for _, reusable := range []bool{false, true} {
		if reusable {
			mem = &lifecycleBlockingMemory{entered: make(chan struct{}), release: make(chan struct{})}
		}
		record, spawnErr := tasks.SpawnWorker(context.Background(), cfg, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, fmt.Sprint(reusable), "task", "worker", "", "worker", reusable, "main", "", coretools.ExecutionContext{"_memory_engine": mem, "_session_runtime": registry})
		if spawnErr != nil {
			t.Fatal(spawnErr)
		}
		select {
		case <-mem.entered:
		case <-time.After(2 * time.Second):
			t.Fatal("worker did not prepare")
		}
		tasks.StopTask(record.TaskID, "main")
		close(mem.release)
		deadline := time.Now().Add(2 * time.Second)
		for {
			tasks.mu.Lock()
			_, active := tasks.workers[record.TaskID]
			tasks.mu.Unlock()
			if !active {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("stopped worker did not finish")
			}
			time.Sleep(time.Millisecond)
		}
		for _, row := range tasks.ListTasks("main") {
			if row.TaskID == record.TaskID && (row.Status != "killed" || row.TerminationReason != "stopped") {
				t.Fatalf("late execution replaced stop: %+v", row)
			}
		}
		child := registry.Snapshots()[record.TaskID]
		if child.Run == nil || child.Run.Phase != AgentPhaseCancelled {
			t.Fatalf("stopped child's run was not cancelled: %+v", child.Run)
		}
	}
	phaseIs(t, parent, AgentPhasePreparing)
	if parent.RunSnapshot().Token != state.RunToken() {
		t.Fatal("child stop changed parent lease")
	}
}
