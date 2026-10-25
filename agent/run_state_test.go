package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"LuminaCode/agentContext"
	"LuminaCode/cluster"
	"LuminaCode/config"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"
	filetool "LuminaCode/tools/file"
)

func phaseFixture(t *testing.T) (*AgentRuntime, context.Context, *AgentState) {
	t.Helper()
	r := NewAgentRuntime(config.Config{APIMaxTokens: 128}, nil)
	ctx, state, err := r.Begin(context.Background(), 128)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.EndRun(state.RunToken()) })
	return r, ctx, state
}
func phaseIs(t *testing.T, r *AgentRuntime, want AgentPhase) {
	t.Helper()
	if got := r.RunSnapshot().Phase; got != want {
		t.Fatalf("phase=%s want=%s", got, want)
	}
}

func TestAgentPhaseTransitionRules(t *testing.T) {
	cases := []struct {
		from, to AgentPhase
		allowed  bool
	}{
		{AgentPhaseIdle, AgentPhasePreparing, true}, {AgentPhaseCompleted, AgentPhasePreparing, true}, {AgentPhaseFailed, AgentPhasePreparing, true}, {AgentPhaseCancelled, AgentPhasePreparing, true},
		{AgentPhasePreparing, AgentPhaseCallingModel, true}, {AgentPhaseCallingModel, AgentPhaseExecutingTools, true}, {AgentPhaseExecutingTools, AgentPhasePreparing, true},
		{AgentPhaseCallingModel, AgentPhaseRecovering, true}, {AgentPhaseRecovering, AgentPhasePreparing, true}, {AgentPhaseCallingModel, AgentPhaseWaitingPermission, true}, {AgentPhaseWaitingPermission, AgentPhaseExecutingTools, true},
		{AgentPhaseWaitingPermission, AgentPhaseCancelling, true}, {AgentPhaseCancelling, AgentPhaseCancelled, true},
		{AgentPhaseIdle, AgentPhaseCallingModel, false}, {AgentPhaseCompleted, AgentPhaseCallingModel, false}, {AgentPhaseCancelling, AgentPhaseCompleted, false}, {AgentPhaseCancelling, AgentPhaseRecovering, false}, {AgentPhaseRecovering, AgentPhaseIdle, false},
	}
	for _, test := range cases {
		if got := CanTransitionAgentPhase(test.from, test.to); got != test.allowed {
			t.Fatalf("%s -> %s allowed=%v want=%v", test.from, test.to, got, test.allowed)
		}
	}
}

func TestRunPhaseLifecycleAndPermissionResume(t *testing.T) {
	r, ctx, state := phaseFixture(t)
	token := state.RunToken()
	phaseIs(t, r, AgentPhasePreparing)
	if err := r.Transition(token, AgentPhaseCallingModel); err != nil {
		t.Fatal(err)
	}
	ticket, event, err := r.BeginPermission(ctx, token, PermissionTool, "read_file", NewStreamEvent("permission_needed", "read_file", nil), false)
	if err != nil {
		t.Fatal(err)
	}
	if PermissionRequestID(event) != ticket.ID {
		t.Fatal("permission event has no matching ticket")
	}
	phaseIs(t, r, AgentPhaseWaitingPermission)
	if err = r.Transition(token, AgentPhaseExecutingTools); err != nil {
		t.Fatal(err)
	}
	phaseIs(t, r, AgentPhaseWaitingPermission)
	if r.RunSnapshot().ResumePhase != AgentPhaseExecutingTools {
		t.Fatal("parallel model completion lost the resumed activity")
	}
	if !r.ResolveRunPermission(PermissionTool, PermissionAnswer{Decision: PermissionOnce}, ticket.ID) {
		t.Fatal("decision rejected")
	}
	answer, err := r.AwaitPermission(ctx, ticket)
	if err != nil || !permissionGranted(answer) {
		t.Fatal("granted permission lost")
	}
	phaseIs(t, r, AgentPhaseExecutingTools)
	if err = r.StartRecovery(token, RecoveryAPI, "retry"); err != nil {
		t.Fatal(err)
	}
	phaseIs(t, r, AgentPhaseRecovering)
	if err = r.Transition(token, AgentPhasePreparing); err != nil {
		t.Fatal(err)
	}
	if err = r.EndRun(token); err != nil {
		t.Fatal(err)
	}
	phaseIs(t, r, AgentPhaseCompleted)
	if err = r.Transition(token, AgentPhaseCallingModel); !errors.Is(err, ErrStaleState) {
		t.Fatal("terminal run was reopened")
	}
}

func TestAbortPermissionRaceCancelsEveryWaiter(t *testing.T) {
	for i := 0; i < 100; i++ {
		r, ctx, state := phaseFixture(t)
		token := state.RunToken()
		ticket, _, err := r.BeginPermission(ctx, token, PermissionSkill, "", NewStreamEvent("permission_needed", "", nil), true)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			r.ResolveRunPermission(PermissionSkill, PermissionAnswer{Decision: PermissionOnce}, ticket.ID)
		}()
		go func() { defer wg.Done(); <-start; r.Abort() }()
		close(start)
		wg.Wait()
		phaseIs(t, r, AgentPhaseCancelling)
		answer, err := r.AwaitPermission(ctx, ticket)
		if err == nil || permissionGranted(answer) {
			t.Fatal("cancelled run accepted a grant")
		}
		if len(r.RunSnapshot().PendingPermissions) != 0 || len(r.DrainPermissionEvents(token)) != 0 {
			t.Fatal("cancelled waiters leaked")
		}
		if err = r.EndRun(token); err != nil {
			t.Fatal(err)
		}
		phaseIs(t, r, AgentPhaseCancelled)
	}
}

func TestRunIDFencesLateCallbacksWithinSameConversation(t *testing.T) {
	r, ctx, first := phaseFixture(t)
	old := first.RunToken()
	ticket, _, err := r.BeginPermission(ctx, old, PermissionTool, "tool", NewStreamEvent("permission_needed", "", nil), false)
	if err != nil {
		t.Fatal(err)
	}
	r.ResolveRunPermission(PermissionTool, PermissionAnswer{Decision: PermissionOnce}, ticket.ID)
	_, _ = r.AwaitPermission(ctx, ticket)
	if err = r.EndRun(old); err != nil {
		t.Fatal(err)
	}
	nextCtx, next, err := r.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.EndRun(next.RunToken())
	if next.RunToken().Generation != old.Generation || next.RunToken().ID == old.ID {
		t.Fatal("run identity did not separate requests")
	}
	current, _, err := r.BeginPermission(nextCtx, next.RunToken(), PermissionTool, "tool", NewStreamEvent("permission_needed", "", nil), false)
	if err != nil {
		t.Fatal(err)
	}
	if r.ResolveRunPermission(PermissionTool, PermissionAnswer{Decision: PermissionOnce}, ticket.ID) {
		t.Fatal("late answer resolved the next request")
	}
	if err = r.EndRun(old); !errors.Is(err, ErrStaleState) {
		t.Fatal("old cleanup ended a new request")
	}
	if err = r.Transition(old, AgentPhaseCallingModel); !errors.Is(err, ErrStaleState) {
		t.Fatal("old transition changed new request")
	}
	if err = r.Commit(context.Background(), first); !errors.Is(err, ErrStaleState) {
		t.Fatal("old draft published into a later request")
	}
	r.ServicesFor(old).SetReadFileState("old.go", filetool.FileStateEntry{Content: "stale"})
	if _, ok := r.GetReadFileState("old.go"); ok {
		t.Fatal("late tool cache write was accepted")
	}
	phaseIs(t, r, AgentPhaseWaitingPermission)
	r.ResolveRunPermission(PermissionTool, PermissionAnswer{Decision: PermissionDeny}, current.ID)
	_, _ = r.AwaitPermission(nextCtx, current)
}

func TestMultiplePermissionWaitsKeepPhaseUntilAllResolved(t *testing.T) {
	r, ctx, state := phaseFixture(t)
	token := state.RunToken()
	first, _, _ := r.BeginPermission(ctx, token, PermissionSkill, "", NewStreamEvent("permission_needed", "", nil), false)
	second, _, _ := r.BeginPermission(ctx, token, PermissionSkill, "", NewStreamEvent("permission_needed", "", nil), false)
	if r.ResolveRunPermission(PermissionSkill, PermissionAnswer{Decision: PermissionOnce}) {
		t.Fatal("ambiguous legacy answer selected a waiter")
	}
	_ = r.StartRecovery(token, RecoveryContext, "over limit")
	r.ResolveRunPermission(PermissionSkill, PermissionAnswer{Decision: PermissionOnce}, second.ID)
	phaseIs(t, r, AgentPhaseWaitingPermission)
	r.ResolveRunPermission(PermissionSkill, PermissionAnswer{Decision: PermissionDeny}, first.ID)
	phaseIs(t, r, AgentPhaseRecovering)
	_, _ = r.AwaitPermission(ctx, first)
	_, _ = r.AwaitPermission(ctx, second)
}

func TestCancelledToolWaitRemovesQueuedNotification(t *testing.T) {
	r, ctx, state := phaseFixture(t)
	_ = r.Transition(state.RunToken(), AgentPhaseCallingModel)
	waitCtx, cancel := context.WithCancel(ctx)
	ticket, _, err := r.BeginPermission(waitCtx, state.RunToken(), PermissionSkill, "", NewStreamEvent("permission_needed", "", nil), true)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	_, err = r.AwaitPermission(waitCtx, ticket)
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	phaseIs(t, r, AgentPhaseCallingModel)
	if len(r.DrainPermissionEvents(state.RunToken())) != 0 {
		t.Fatal("expired permission was still sent to the UI")
	}
}

func TestRecoveryStateIsPerRunAndReadOnly(t *testing.T) {
	r, _, state := phaseFixture(t)
	token := state.RunToken()
	_ = r.StartRecovery(token, RecoveryAPI, "temporary")
	r.SetAPIErrors(token, 2)
	result := r.HandleOutputTruncation(token, state, &ModelTurn{OutputTruncated: true})
	if !result.ShouldContinue() || r.RunSnapshot().Recovery.Output.CurrentMaxTokens != EscalatedMaxTokens {
		t.Fatal("output recovery was not stored in the run")
	}
	snapshot := r.RunSnapshot()
	snapshot.Recovery.APIErrors = 99
	if r.RunSnapshot().Recovery.APIErrors != 2 {
		t.Fatal("run snapshot shares mutable recovery state")
	}
	r.FailRun(token, "fatal provider error")
	_ = r.EndRun(token)
	phaseIs(t, r, AgentPhaseFailed)
	_, next, err := r.Begin(context.Background(), 256)
	if err != nil {
		t.Fatal(err)
	}
	defer r.EndRun(next.RunToken())
	if r.RunSnapshot().Recovery.APIErrors != 0 || r.OutputRecovery(next.RunToken()).CurrentMaxTokens != 256 {
		t.Fatal("recovery counters leaked across runs")
	}
}

func TestRunControlStateIsNotRestoredFromCheckpoint(t *testing.T) {
	r, ctx, state := phaseFixture(t)
	_, _, _ = r.BeginPermission(ctx, state.RunToken(), PermissionMCP, "", NewStreamEvent("permission_needed", "", nil), false)
	live := r.Snapshot()
	if live.Run == nil || live.Run.Phase != AgentPhaseWaitingPermission {
		t.Fatal("live snapshot omits the run phase")
	}
	r.mu.RLock()
	raw, err := json.Marshal(r.snapshot)
	r.mu.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "pending_permissions") || strings.Contains(string(raw), "waiting_permission") {
		t.Fatal("ephemeral waiters entered persistent state")
	}
	restored := NewAgentRuntime(config.Config{}, nil)
	if err = restored.Restore(live); err != nil {
		t.Fatal(err)
	}
	phaseIs(t, restored, AgentPhaseIdle)
	if len(restored.RunSnapshot().PendingPermissions) != 0 {
		t.Fatal("checkpoint resurrected a permission channel")
	}
}

func TestOnlyOneConcurrentBeginOwnsTheRun(t *testing.T) {
	r := NewAgentRuntime(config.Config{}, nil)
	var won atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := r.Begin(context.Background())
			if err == nil {
				won.Add(1)
			} else if !errors.Is(err, ErrAgentBusy) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("concurrent owners=%d", won.Load())
	}
	_ = r.EndRun(r.RunSnapshot().Token)
}

func TestChildPermissionUsesItsOwnRunAndParentReplyRouter(t *testing.T) {
	parent, pctx, pstate := phaseFixture(t)
	_ = pctx
	registry := NewSessionRuntime()
	registry.Register("main", parent)
	child, cctx, cstate := phaseFixture(t)
	registry.Register("child", child)
	_ = parent.Transition(pstate.RunToken(), AgentPhaseExecutingTools)
	sub := NewSubAgent(config.Config{}, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, "", "worker", coretools.ExecutionContext{"scope_id": "child", "_session_runtime": registry})
	sub.Runtime = child
	var sent StreamEvent
	sub.ExtraContext["_run_permission_emitter"] = func(event StreamEvent) bool {
		sent = event
		return registry.ResolvePermissionRequest(PermissionRequestID(event), PermissionAnswer{Decision: PermissionOnce})
	}
	exec := sub.buildExecutionContext(cstate.RunToken())
	ask := exec["_request_skill_shell_permission"].(func(skills.SkillShellPermissionRequest) bool)
	if !ask(skills.SkillShellPermissionRequest{}) {
		t.Fatal("child approval was not routed")
	}
	if sent.Metadata["run_id"] != cstate.RunToken().ID {
		t.Fatal("child permission used the parent run")
	}
	phaseIs(t, parent, AgentPhaseExecutingTools)
	phaseIs(t, child, AgentPhasePreparing)
	_ = cctx
}

type phaseBlockingTool struct {
	coretools.BaseTool
	started, stopped chan struct{}
}

func (b *phaseBlockingTool) Execute(ctx context.Context, _ coretools.ExecutionContext, _ any) (string, error) {
	close(b.started)
	<-ctx.Done()
	close(b.stopped)
	return "cancelled", ctx.Err()
}
func TestAbortCancelsStreamingToolContext(t *testing.T) {
	r, _, state := phaseFixture(t)
	tool := &phaseBlockingTool{BaseTool: coretools.BaseTool{Spec: coretools.ToolSpec{Name: "blocking", ReadOnly: coretools.BoolPtr(true), ConcurrencySafe: coretools.BoolPtr(true), InputPrototype: struct{}{}}}, started: make(chan struct{}), stopped: make(chan struct{})}
	registry := coretools.NewToolRegistry()
	registry.Register(tool)
	executor := NewStreamingToolExecutor(registry, config.Config{}, state, coretools.ExecutionContext{})
	executor.AddTool(coretools.ToolCall{ID: "call", Name: "blocking", Input: map[string]any{}})
	select {
	case <-tool.started:
	case <-time.After(time.Second):
		t.Fatal("tool did not start")
	}
	r.Abort()
	select {
	case <-tool.stopped:
	case <-time.After(time.Second):
		t.Fatal("run cancellation did not reach the tool")
	}
}

func TestLateCacheCompletionCannotPinNextRun(t *testing.T) {
	cfg := config.Config{APIModel: "claude-test", AnthropicCacheEditsEnabled: true}
	r := NewAgentRuntime(cfg, nil)
	_, old, _ := r.Begin(context.Background())
	_ = r.EndRun(old.RunToken())
	_, current, _ := r.Begin(context.Background())
	defer r.EndRun(current.RunToken())
	r.Context.QueueCacheEdits([]agentContext.CacheEdit{agentContext.NewCacheEdit("new", nil)})
	r.ConsumeRequestCache(current.RunToken(), map[string]struct{}{"new": {}}, cfg.APIModel)
	r.FinishRequestCache(old.RunToken(), true)
	if len(r.Context.CacheEditStateSnapshot().ConsumedInFlight) != 1 {
		t.Fatal("late cache callback consumed a later request")
	}
}

func TestParentCancellationCannotCancelTheNextRun(t *testing.T) {
	r := NewAgentRuntime(config.Config{}, nil)
	for i := 0; i < 30; i++ {
		parent, cancel := context.WithCancel(context.Background())
		ctx, old, err := r.Begin(parent)
		if err != nil {
			t.Fatal(err)
		}
		cancel()
		<-ctx.Done()
		if err = r.EndRun(old.RunToken()); err != nil {
			t.Fatal(err)
		}
		phaseIs(t, r, AgentPhaseCancelled)
		_, next, err := r.Begin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		// A cancellation callback can arrive after its context's Done was observed.
		r.cancelRun(old.RunToken(), "late parent cancellation")
		phaseIs(t, r, AgentPhasePreparing)
		_ = r.EndRun(next.RunToken())
	}
}

func TestResetRejectsOldPermissionAndRecoveryCallbacks(t *testing.T) {
	r, ctx, old := phaseFixture(t)
	ticket, _, _ := r.BeginPermission(ctx, old.RunToken(), PermissionSkill, "", NewStreamEvent("permission_needed", "", nil), false)
	if err := r.Reset(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, next, err := r.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer r.EndRun(next.RunToken())
	if r.ResolveRunPermission(PermissionSkill, PermissionAnswer{Decision: PermissionOnce}, ticket.ID) {
		t.Fatal("reset accepted an old permission reply")
	}
	if err = r.StartRecovery(old.RunToken(), RecoveryAPI, "old error"); !errors.Is(err, ErrStaleState) {
		t.Fatal("reset accepted old recovery")
	}
	r.SetAPIErrors(old.RunToken(), 99)
	if r.RunSnapshot().Recovery.APIErrors != 0 {
		t.Fatal("old recovery changed a new generation")
	}
	phaseIs(t, r, AgentPhasePreparing)
}

func TestRunSnapshotIsConsistentDuringPermissionUpdates(t *testing.T) {
	r, ctx, state := phaseFixture(t)
	token := state.RunToken()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			ticket, _, err := r.BeginPermission(ctx, token, PermissionSkill, "", NewStreamEvent("permission_needed", "", nil), false)
			if err != nil {
				t.Error(err)
				return
			}
			r.ResolveRunPermission(PermissionSkill, PermissionAnswer{Decision: PermissionOnce}, ticket.ID)
			_, _ = r.AwaitPermission(ctx, ticket)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			snapshot := r.RunSnapshot()
			if snapshot.Phase == AgentPhaseWaitingPermission && len(snapshot.PendingPermissions) == 0 {
				t.Error("permission phase published without a waiter")
			}
			if len(snapshot.PendingPermissions) > 0 && snapshot.Phase != AgentPhaseWaitingPermission {
				t.Error("waiters published outside the permission phase")
			}
			if len(snapshot.PendingPermissions) > 0 {
				snapshot.PendingPermissions[0].ID = "mutated snapshot"
			}
		}
	}()
	wg.Wait()
	phaseIs(t, r, AgentPhasePreparing)
}

func TestModelCallPhaseAndAbortAreObservable(t *testing.T) {
	entered := make(chan struct{})
	left := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(entered)
		<-request.Context().Done()
		close(left)
	}))
	defer server.Close()
	cfg := config.Config{CWD: t.TempDir(), APIType: "openai_compatible", APIBaseURL: server.URL, APIKey: "test-key", APIModel: "model", APIMaxTokens: 128, ContextInputTokenLimit: 10000}
	engine := NewCoreExecutionEngine(cfg, cluster.RuntimeIdentity{TenantID: "local"}, nil)
	defer engine.Shutdown()
	engine.Registry = coretools.NewToolRegistry()
	initial := NewAgentState()
	initial.SystemPrompt = "system"
	initial.Messages = []map[string]any{{"role": "user", "content": "task"}}
	if err := engine.AgentRuntime.Restore(SnapshotFromState(&initial)); err != nil {
		t.Fatal(err)
	}
	events := engine.QueryLoop(context.Background())
	finished := make(chan struct{})
	go func() {
		for range events {
		}
		close(finished)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("model request did not start")
	}
	phaseIs(t, engine.AgentRuntime, AgentPhaseCallingModel)
	engine.Abort()
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("aborted model call did not finish")
	}
	phaseIs(t, engine.AgentRuntime, AgentPhaseCancelled)
	select {
	case <-left:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP request was not cancelled")
	}
}

func TestAcceptedRunIDMatchesLiveAndDurableExecution(t *testing.T) {
	engine := NewCoreExecutionEngine(config.Config{APIMaxTokens: 128}, cluster.RuntimeIdentity{TenantID: "local"}, nil)
	defer engine.Shutdown()
	recorder := NewRuntimeEventRecorder(nil, "session")
	recorder.BindRuntime(engine.AgentRuntime)
	engine.RuntimeEvents = recorder
	recorder.ReserveRunID("accepted-command-run")
	_, state, err := engine.beginExecution(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer engine.finishExecution(context.Background(), state)
	if state.RunToken().ID != "accepted-command-run" || engine.AgentRuntime.RunSnapshot().Token.ID != "accepted-command-run" {
		t.Fatal("accepted command and live execution IDs diverged")
	}
}

func TestRunCannotCompleteWhilePermissionIsUnresolved(t *testing.T) {
	r, ctx, state := phaseFixture(t)
	ticket, _, err := r.BeginPermission(ctx, state.RunToken(), PermissionTool, "tool", NewStreamEvent("permission_needed", "", nil), false)
	if err != nil {
		t.Fatal(err)
	}
	if CanTransitionAgentPhase(AgentPhaseWaitingPermission, AgentPhaseCompleted) {
		t.Fatal("unresolved permission allowed a successful terminal")
	}
	if err = r.EndRun(state.RunToken()); err != nil {
		t.Fatal(err)
	}
	phaseIs(t, r, AgentPhaseFailed)
	if r.RunSnapshot().Error == "" {
		t.Fatal("unresolved permission failed without a reason")
	}
	if _, err = r.AwaitPermission(ctx, ticket); err == nil {
		t.Fatal("ended permission request did not unblock")
	}
}
