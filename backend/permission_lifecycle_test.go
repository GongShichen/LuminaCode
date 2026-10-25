package backend

import (
	"LuminaCode/agent"
	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/session"
	luminaui "LuminaCode/ui"
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type lifecyclePermissionEngine struct{}

func (*lifecyclePermissionEngine) SubmitMessage(ctx context.Context, _ string) <-chan agent.StreamEvent {
	out := make(chan agent.StreamEvent)
	go func() {
		defer close(out)
		select {
		case out <- agent.NewStreamEvent("permission_needed", "run_shell", nil):
		case <-ctx.Done():
			return
		}
		<-ctx.Done()
	}()
	return out
}

type lifecycleOwnedPermissionEngine struct {
	runtime *agent.AgentRuntime
	session *agent.SessionRuntime
	ctx     context.Context
	state   *agent.AgentState
}

func (e *lifecycleOwnedPermissionEngine) PermissionContext(id string) (context.Context, bool) {
	return e.session.PermissionContext(id)
}

func (e *lifecycleOwnedPermissionEngine) SubmitMessage(_ context.Context, _ string) <-chan agent.StreamEvent {
	out := make(chan agent.StreamEvent)
	go func() {
		defer close(out)
		defer e.runtime.EndRun(e.state.RunToken())
		ticket, event, err := e.runtime.BeginPermission(e.ctx, e.state.RunToken(), agent.PermissionTool, "run_shell", agent.NewStreamEvent("permission_needed", "", nil), false)
		if err != nil {
			return
		}
		out <- event
		_, _ = e.runtime.AwaitPermission(e.ctx, ticket)
	}()
	return out
}

func TestRuntimeAbortUnblocksUIWithLiveSubmitContext(t *testing.T) {
	requests := make(chan string, 1)
	bridge := NewWSRendererBridge("local", "runtime-abort", func(event PushEvent) {
		if body, ok := event.Event.(map[string]any); ok && body["type"] == "permission_requested" {
			requests <- body["payload"].(map[string]any)["request_id"].(string)
		}
	}, func() int64 { return 1 })
	runtime := agent.NewAgentRuntime(config.Config{}, nil)
	ctx, state, err := runtime.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	registry := agent.NewSessionRuntime()
	registry.Register("main", runtime)
	ui := luminaui.NewUiRuntime(&lifecycleOwnedPermissionEngine{runtime: runtime, session: registry, ctx: ctx, state: state}, bridge)
	done := make(chan struct{})
	go func() { ui.RunSubmitMessage(context.Background(), "task", nil, "runtime-abort"); close(done) }()
	var id string
	select {
	case id = <-requests:
	case <-time.After(time.Second):
		t.Fatal("modal did not open")
	}
	runtime.Abort()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("UI ignored its permission owner's cancellation")
	}
	if bridge.ResolvePermission(id, "once") {
		t.Fatal("late reply was accepted")
	}
}
func TestUIAbortUnblocksPermissionModal(t *testing.T) {
	requests := make(chan string, 1)
	bridge := NewWSRendererBridge("local", "review", func(event PushEvent) {
		if body, ok := event.Event.(map[string]any); ok && body["type"] == "permission_requested" {
			requests <- body["payload"].(map[string]any)["request_id"].(string)
		}
	}, func() int64 { return 1 })
	ui := luminaui.NewUiRuntime(&lifecyclePermissionEngine{}, bridge)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { ui.RunSubmitMessage(ctx, "task", nil, "review"); close(done) }()
	var id string
	select {
	case id = <-requests:
	case <-time.After(time.Second):
		t.Fatal("modal was not displayed")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("UI submit stayed blocked in AskPermission after cancellation")
	}
	if bridge.ResolvePermission(id, "once") {
		t.Fatal("cancelled bridge request accepted a late reply")
	}
}

func lifecycleController(t *testing.T, emit EventEmitter) *SessionController {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Config{CWD: dir, ProjectRuntimeDir: dir, SessionDir: filepath.Join(dir, "sessions"), APIMaxTokens: 128, ContextInputTokenLimit: 16000}
	repo := session.NewLocalRepository(session.NewStore(cfg.SessionDir))
	loaded, err := repo.OpenRuntime(context.Background(), session.LocalTenantID, "permission-session", session.RuntimeOpenOptions{CreateIfMissing: true, CWD: dir})
	if err != nil {
		t.Fatal(err)
	}
	engine := agent.NewQueryEngine(cfg, agent.NewCoreExecutionEngine(cfg, cluster.RuntimeIdentity{TenantID: session.LocalTenantID}, nil))
	controller := NewSessionController(cluster.RuntimeIdentity{}, "permission-session", cfg, engine, loaded.State, repo, loaded.Journal, emit)
	t.Cleanup(controller.Shutdown)
	return controller
}

func TestClearReleasesPermissionModalAndSubmit(t *testing.T) {
	requests := make(chan string, 1)
	c := lifecycleController(t, func(event PushEvent) {
		if body, ok := event.Event.(map[string]any); ok && body["type"] == "permission_requested" {
			requests <- body["payload"].(map[string]any)["request_id"].(string)
		}
	})
	c.ui.Engine = &lifecyclePermissionEngine{}
	if _, err := c.submit(context.Background(), "task", nil); err != nil {
		t.Fatal(err)
	}
	var id string
	select {
	case id = <-requests:
	case <-time.After(2 * time.Second):
		t.Fatal("modal did not open")
	}
	done := make(chan error, 1)
	go func() { done <- c.Clear() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Clear blocked on the permission modal")
	}
	if c.busy.Load() {
		t.Fatal("submit remained busy after Clear")
	}
	if c.bridge.ResolvePermission(id, "once") {
		t.Fatal("old modal survived Clear")
	}
	if c.engine.Snapshot().Run.Phase != agent.AgentPhaseIdle {
		t.Fatal("Clear did not reset run state")
	}
}

func TestIdleSessionRoutesChildPermissionAndClearCancelsIt(t *testing.T) {
	requests := make(chan string, 4)
	c := lifecycleController(t, func(event PushEvent) {
		if body, ok := event.Event.(map[string]any); ok && body["type"] == "permission_requested" {
			requests <- body["payload"].(map[string]any)["request_id"].(string)
		}
	})
	parent := c.engine.CoreEngine.AgentRuntime
	_, draft, err := parent.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err = parent.EndRun(draft.RunToken()); err != nil {
		t.Fatal(err)
	}
	child := agent.NewAgentRuntime(c.cfg, nil)
	c.engine.CoreEngine.SessionRuntime.Register("child", child)
	ctx, state, err := child.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer child.EndRun(state.RunToken())
	for _, decision := range []string{"once", "cancel"} {
		ticket, event, err := child.BeginPermission(ctx, state.RunToken(), agent.PermissionSkill, "", agent.NewStreamEvent("permission_needed", "", nil), false)
		if err != nil {
			t.Fatal(err)
		}
		if !c.engine.CoreEngine.SessionRuntime.PublishPermission(ctx, event) {
			t.Fatal("publication rejected")
		}
		var id string
		select {
		case id = <-requests:
		case <-time.After(2 * time.Second):
			t.Fatal("idle parent lost child request")
		}
		if decision == "once" {
			if !c.bridge.ResolvePermission(id, "once") {
				t.Fatal("child response rejected")
			}
			answer, err := child.AwaitPermission(ctx, ticket)
			if err != nil || answer.Decision != "once" {
				t.Fatal("reply did not reach the child")
			}
			if parent.RunSnapshot().Phase != agent.AgentPhaseCompleted {
				t.Fatal("child permission changed parent phase")
			}
		} else {
			if err := c.Clear(); err != nil {
				t.Fatal(err)
			}
			if _, err := child.AwaitPermission(ctx, ticket); err == nil {
				t.Fatal("Clear did not cancel the child")
			}
			// The handler wakes on the child's cancellation independently of a submit.
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				c.bridge.mu.Lock()
				n := len(c.bridge.permissions)
				c.bridge.mu.Unlock()
				if n == 0 {
					break
				}
				time.Sleep(time.Millisecond)
			}
			if c.bridge.ResolvePermission(id, "once") {
				t.Fatal("Clear retained the child modal")
			}
		}
	}
}

func TestBridgePermissionResolveCancelRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		requests := make(chan string, 1)
		bridge := NewWSRendererBridge("local", "race", func(event PushEvent) {
			if body, ok := event.Event.(map[string]any); ok && body["type"] == "permission_requested" {
				requests <- body["payload"].(map[string]any)["request_id"].(string)
			}
		}, func() int64 { return 1 })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan string, 1)
		go func() { done <- bridge.AskPermissionContext(ctx, nil, true) }()
		id := <-requests
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cancel() }()
		go func() { defer wg.Done(); bridge.ResolvePermission(id, "once") }()
		wg.Wait()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("permission waiter leaked")
		}
		if bridge.ResolvePermission(id, "once") {
			t.Fatal("late reply resolved a closed waiter")
		}
	}
}

func TestBridgeQueuedPermissionCancellationDoesNotConsumeNextReply(t *testing.T) {
	requests := make(chan string, 3)
	bridge := NewWSRendererBridge("local", "queue", func(event PushEvent) {
		if body, ok := event.Event.(map[string]any); ok && body["type"] == "permission_requested" {
			requests <- body["payload"].(map[string]any)["request_id"].(string)
		}
	}, func() int64 { return 1 })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first := make(chan string, 1)
	go func() { first <- bridge.AskPermissionContext(ctx, nil, true) }()
	var oldID string
	select {
	case oldID = <-requests:
	case <-time.After(time.Second):
		t.Fatal("first permission did not open")
	}
	queuedCtx, cancelQueued := context.WithCancel(context.Background())
	queued := make(chan string, 1)
	go func() { queued <- bridge.AskPermissionContext(queuedCtx, nil, true) }()
	cancelQueued()
	select {
	case answer := <-queued:
		if answer != "deny" {
			t.Fatal("cancelled queue was approved")
		}
	case <-time.After(time.Second):
		t.Fatal("queued cancellation blocked on the first modal")
	}
	cancel()
	select {
	case <-first:
	case <-time.After(time.Second):
		t.Fatal("first cancellation did not release the modal")
	}
	next := make(chan string, 1)
	go func() { next <- bridge.AskPermissionContext(context.Background(), nil, true) }()
	var newID string
	select {
	case newID = <-requests:
	case <-time.After(time.Second):
		t.Fatal("fresh permission did not open")
	}
	if newID == oldID || bridge.ResolvePermission(oldID, "once") {
		t.Fatal("old permission survived cancellation")
	}
	if !bridge.ResolvePermission(newID, "once") {
		t.Fatal("fresh permission rejected")
	}
	select {
	case answer := <-next:
		if answer != "once" {
			t.Fatalf("fresh grant lost: %s", answer)
		}
	case <-time.After(time.Second):
		t.Fatal("fresh grant did not release its waiter")
	}
}
