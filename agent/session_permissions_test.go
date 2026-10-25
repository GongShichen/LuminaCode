package agent

import (
	"context"
	"sync"
	"testing"
	"time"

	"LuminaCode/config"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"
)

func TestChildPermissionSurvivesParentStreamClosure(t *testing.T) {
	parent, _, parentState := phaseFixture(t)
	child, _, childState := phaseFixture(t)
	session := NewSessionRuntime()
	session.Register("main", parent)
	session.Register("child", child)
	sub := NewSubAgent(config.Config{}, coretools.NewToolRegistry(), AgentDef{Name: "worker"}, nil, "", "worker", coretools.ExecutionContext{"_session_runtime": session})
	sub.Runtime = child
	ask := sub.buildExecutionContext(childState.RunToken())["_request_skill_shell_permission"].(func(skills.SkillShellPermissionRequest) bool)

	parentOut := make(chan StreamEvent)
	stop := session.forwardPermissions(context.Background(), parentOut)
	done := make(chan bool, 1)
	go func() { done <- ask(skills.SkillShellPermissionRequest{}) }()
	// Wait for publication before detaching the consumer; no receiver is reading
	// parentOut, so forwarding must return the request to the session mailbox.
	deadline := time.After(2 * time.Second)
	for len(child.RunSnapshot().PendingPermissions) == 0 {
		select {
		case <-deadline:
			t.Fatal("child did not request permission")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	stop()
	_ = parent.EndRun(parentState.RunToken())
	close(parentOut)
	phaseIs(t, parent, AgentPhaseCompleted)
	phaseIs(t, child, AgentPhaseWaitingPermission)

	delivered := make(chan StreamEvent, 1)
	session.SetPermissionHandler(func(_ context.Context, event StreamEvent) {
		delivered <- event
		session.ResolvePermissionRequest(PermissionRequestID(event), PermissionAnswer{Decision: PermissionOnce})
	})
	select {
	case event := <-delivered:
		if event.Metadata["run_id"] != childState.RunToken().ID {
			t.Fatal("request retained parent identity")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("idle session lost child permission")
	}
	select {
	case granted := <-done:
		if !granted {
			t.Fatal("child permission rejected")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("child waiter was not released")
	}
	phaseIs(t, parent, AgentPhaseCompleted)
	phaseIs(t, child, AgentPhasePreparing)
}

func TestPermissionForwarderCloseAndChildAbortRace(t *testing.T) {
	for i := 0; i < 100; i++ {
		child, ctx, state := phaseFixture(t)
		session := NewSessionRuntime()
		session.Register("child", child)
		ticket, event, err := child.BeginPermission(ctx, state.RunToken(), PermissionSkill, "", NewStreamEvent("permission_needed", "", nil), false)
		if err != nil {
			t.Fatal(err)
		}
		out := make(chan StreamEvent)
		stop := session.forwardPermissions(context.Background(), out)
		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); <-start; session.PublishPermission(ctx, event) }()
		go func() { defer wg.Done(); <-start; stop(); close(out) }()
		go func() { defer wg.Done(); <-start; child.Abort() }()
		close(start)
		wg.Wait()
		answer, err := child.AwaitPermission(ctx, ticket)
		if err == nil || permissionGranted(answer) {
			t.Fatal("cancelled child was granted permission")
		}
		if session.ResolvePermissionRequest(ticket.ID, PermissionAnswer{Decision: PermissionOnce}) {
			t.Fatal("stale ticket resolved")
		}
		_ = child.EndRun(state.RunToken())
		phaseIs(t, child, AgentPhaseCancelled)
	}
}

func TestSessionResetDiscardsQueuedChildPermission(t *testing.T) {
	child, ctx, state := phaseFixture(t)
	session := NewSessionRuntime()
	session.Register("child", child)
	ticket, event, _ := child.BeginPermission(ctx, state.RunToken(), PermissionSkill, "", NewStreamEvent("permission_needed", "", nil), false)
	if !session.PublishPermission(ctx, event) {
		t.Fatal("publication failed")
	}
	session.Reset(NewAgentRuntime(config.Config{}, nil))
	if session.PublishPermission(ctx, event) || session.ResolvePermissionRequest(ticket.ID, PermissionAnswer{Decision: PermissionOnce}) {
		t.Fatal("reset retained child ownership")
	}
	session.permissionMu.Lock()
	queued := len(session.permissions)
	session.permissionMu.Unlock()
	if queued != 0 {
		t.Fatal("reset retained permission notification")
	}
	child.Abort()
	_, _ = child.AwaitPermission(ctx, ticket)
}
