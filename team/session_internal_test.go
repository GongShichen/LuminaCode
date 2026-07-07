package team

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"LuminaCode/config"
)

func TestSendA2AWaitTimeoutDoesNotMarkTargetInterruptedByUser(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}

	runCtx, cancel := context.WithCancel(context.Background())
	session.mu.Lock()
	session.runCtx = runCtx
	session.mu.Unlock()
	defer cancel()

	target := session.agents["backend"]
	target.mu.Lock()
	target.busy = true
	target.mu.Unlock()
	defer func() {
		target.mu.Lock()
		target.busy = false
		target.mu.Unlock()
	}()

	result := session.SendA2AMessage(context.Background(), "team-leader", A2AMessageInput{
		To:             []string{"backend"},
		TaskType:       "analysis",
		Message:        "wait briefly",
		TimeoutSeconds: 1,
	})
	results, _ := result["results"].([]map[string]any)
	if len(results) != 1 || results[0]["status"] != "task_pending" {
		t.Fatalf("expected task_pending result, got %#v", result)
	}

	session.mu.Lock()
	row := session.activity["backend"]
	dialogue := append([]DialogueEntry(nil), session.dialogue...)
	session.mu.Unlock()
	if row.Status == "interrupted" || row.Summary == "interrupted by user" {
		t.Fatalf("A2A wait timeout should not be shown as user interrupt: %#v", row)
	}
	if row.Summary != "continuing after A2A wait timeout" {
		t.Fatalf("unexpected timeout activity summary: %#v", row)
	}
	foundTimeoutDialogue := false
	for _, entry := range dialogue {
		if entry.FromAgent == "backend" && entry.Kind == "timeout" {
			foundTimeoutDialogue = true
			break
		}
	}
	if !foundTimeoutDialogue {
		t.Fatalf("expected readable timeout dialogue entry, got %#v", dialogue)
	}

	cancel()
}

func TestTeamAgentRuntimeSessionDirIsNestedUnderTeamSession(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	sessionRoot := t.TempDir()
	cfg := config.NewConfigForCWD(workdir)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = sessionRoot
	session, err := NewManager(cfg, nil, nil).Start("parent-session", "product-development", workdir)
	if err != nil {
		t.Fatal(err)
	}
	backend := session.agents["backend"]
	want := filepath.Join(session.rootDir, "agents")
	if backend.Engine.Config.SessionDir != want {
		t.Fatalf("team agent session dir = %s, want %s", backend.Engine.Config.SessionDir, want)
	}
	if _, err := os.Stat(filepath.Join(sessionRoot, session.ID+"-backend")); !os.IsNotExist(err) {
		t.Fatalf("team agent must not create top-level role session dir, stat err=%v", err)
	}
}
