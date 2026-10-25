package backend

import (
	"context"
	"sync"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
	luminaui "LuminaCode/ui"
)

type deferralEngine struct{ session *agent.SessionRuntime }

func (e *deferralEngine) PermissionContext(id string) (context.Context, bool) {
	return e.session.PermissionContext(id)
}
func (e *deferralEngine) DeferPermissionRequest(id string) bool {
	return e.session.DeferPermissionRequest(id)
}
func (e *deferralEngine) ResolvePermissionRequest(id, decision, tool string) bool {
	return e.session.ResolvePermissionRequest(id, agent.PermissionAnswer{Decision: decision, ToolName: tool})
}

type deferralFixture struct {
	session  *agent.SessionRuntime
	child    *agent.AgentRuntime
	childCtx context.Context
	ticket   agent.PermissionTicket
	event    agent.StreamEvent
	ui       *luminaui.UiRuntime
	bridge   *WSRendererBridge
	requests chan string
}

func newDeferralFixture(t *testing.T) *deferralFixture {
	t.Helper()
	f := &deferralFixture{session: agent.NewSessionRuntime(), child: agent.NewAgentRuntime(config.Config{}, nil), requests: make(chan string, 8)}
	f.session.Register("child", f.child)
	var state *agent.AgentState
	var err error
	f.childCtx, state, err = f.child.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.child.Abort(); _ = f.child.EndRun(state.RunToken()) })
	f.ticket, f.event, err = f.child.BeginPermission(f.childCtx, state.RunToken(), agent.PermissionSkill, "", agent.NewStreamEvent("permission_needed", "", nil), false)
	if err != nil {
		t.Fatal(err)
	}
	f.bridge = NewWSRendererBridge("local", "deferral", func(push PushEvent) {
		if body, ok := push.Event.(map[string]any); ok && body["type"] == "permission_requested" {
			f.requests <- body["payload"].(map[string]any)["request_id"].(string)
		}
	}, func() int64 { return 1 })
	f.ui = luminaui.NewUiRuntime(&deferralEngine{session: f.session}, f.bridge)
	return f
}
func (f *deferralFixture) present(ctx context.Context) <-chan string {
	done := make(chan string, 1)
	go func() { done <- f.ui.HandlePermissionEventContext(ctx, f.ui.ToUIEvent(f.event)) }()
	return done
}
func (f *deferralFixture) requestID(t *testing.T) string {
	t.Helper()
	select {
	case id := <-f.requests:
		return id
	case <-time.After(time.Second):
		t.Fatal("permission was not presented")
		return ""
	}
}
func awaitPresentation(t *testing.T, done <-chan string) string {
	t.Helper()
	select {
	case decision := <-done:
		return decision
	case <-time.After(time.Second):
		t.Fatal("permission presentation did not finish")
		return ""
	}
}
func (f *deferralFixture) resume() {
	f.session.SetPermissionHandler(func(ctx context.Context, event agent.StreamEvent) {
		answer := luminaui.AskPermissionEvent(ctx, f.bridge, f.ui.ToUIEvent(event))
		f.session.ResolvePermissionRequest(agent.PermissionRequestID(event), agent.PermissionAnswer{Decision: answer})
	})
}

func TestParentCancellationDefersShownChildPermission(t *testing.T) {
	f := newDeferralFixture(t)
	parentCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := f.present(parentCtx)
	oldID := f.requestID(t)
	cancel()
	if answer := awaitPresentation(t, done); answer != "cancelled" {
		t.Fatalf("presentation cancellation became a decision: %s", answer)
	}
	if f.childCtx.Err() != nil || len(f.child.RunSnapshot().PendingPermissions) != 1 || f.child.RunSnapshot().Phase != agent.AgentPhaseWaitingPermission {
		t.Fatal("parent cancellation changed the child run")
	}
	if len(f.ui.Frame.PermissionAudit) != 0 {
		t.Fatal("UI recorded a denial the user did not make")
	}
	if f.bridge.ResolvePermission(oldID, "once") {
		t.Fatal("dismissed modal accepted a reply")
	}
	f.resume()
	newID := f.requestID(t)
	if newID == oldID || !f.bridge.ResolvePermission(newID, "once") {
		t.Fatal("deferred permission did not get a fresh presentation")
	}
	answer, err := f.child.AwaitPermission(f.childCtx, f.ticket)
	if err != nil || answer.Decision != agent.PermissionOnce {
		t.Fatalf("resumed decision lost: %+v %v", answer, err)
	}
}

func TestOwnerCancellationDoesNotRequeuePermission(t *testing.T) {
	for _, reset := range []bool{false, true} {
		t.Run(map[bool]string{false: "abort", true: "reset"}[reset], func(t *testing.T) {
			f := newDeferralFixture(t)
			done := f.present(context.Background())
			id := f.requestID(t)
			if reset {
				f.session.Reset(agent.NewAgentRuntime(config.Config{}, nil))
			} else {
				f.child.Abort()
			}
			if answer := awaitPresentation(t, done); answer != "cancelled" {
				t.Fatal("owner cancellation became a user decision")
			}
			if f.session.DeferPermissionRequest(f.ticket.ID) || f.bridge.ResolvePermission(id, "once") {
				t.Fatal("cancelled permission remained deliverable")
			}
			if len(f.ui.Frame.PermissionAudit) != 0 {
				t.Fatal("owner cancellation was audited as a user decision")
			}
			if _, err := f.child.AwaitPermission(f.childCtx, f.ticket); err == nil {
				t.Fatal("cancelled owner accepted a reply")
			}
		})
	}
}

func TestExplicitDenialStillResolvesChildPermission(t *testing.T) {
	f := newDeferralFixture(t)
	done := f.present(context.Background())
	id := f.requestID(t)
	if !f.bridge.ResolvePermission(id, "deny") {
		t.Fatal("explicit denial rejected")
	}
	if answer := awaitPresentation(t, done); answer != "deny" {
		t.Fatal("explicit denial changed")
	}
	answer, err := f.child.AwaitPermission(f.childCtx, f.ticket)
	if err != nil || answer.Decision != agent.PermissionDeny || len(f.ui.Frame.PermissionAudit) != 1 {
		t.Fatalf("explicit denial was deferred: %+v %v", answer, err)
	}
}

func TestApprovalRacingParentCancellationNeverDeniesChild(t *testing.T) {
	for i := 0; i < 50; i++ {
		f := newDeferralFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		done := f.present(ctx)
		id := f.requestID(t)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); cancel() }()
		go func() { defer wg.Done(); f.bridge.ResolvePermission(id, "once") }()
		wg.Wait()
		awaitPresentation(t, done)
		if len(f.child.RunSnapshot().PendingPermissions) > 0 {
			f.resume()
			next := f.requestID(t)
			if !f.bridge.ResolvePermission(next, "once") {
				t.Fatal("deferred approval rejected")
			}
		}
		answer, err := f.child.AwaitPermission(f.childCtx, f.ticket)
		if err != nil || answer.Decision != agent.PermissionOnce {
			t.Fatalf("parent cancellation synthesized denial: %+v %v", answer, err)
		}
	}
}
