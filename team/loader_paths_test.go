package team

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"LuminaCode/apppaths"
	"LuminaCode/config"
)

func TestTeamResourcePrecedence(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	user := filepath.Join(root, "user")
	bundled := filepath.Join(root, "bundled")
	writeTeamFixture(t, bundled, "sample", "bundled")
	writeTeamFixture(t, user, "sample", "user")
	writeTeamFixture(t, apppaths.ProjectTeamsDir(project), "sample", "project")

	loader := NewLoader(config.Config{
		CWD:            filepath.Join(project, "nested"),
		TeamDir:        user,
		BundledTeamDir: bundled,
	})
	spec, err := loader.Load("sample")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Description != "project" {
		t.Fatalf("project team should win, got %q", spec.Description)
	}

	if err := os.RemoveAll(apppaths.ProjectTeamsDir(project)); err != nil {
		t.Fatal(err)
	}
	spec, err = loader.Load("sample")
	if err != nil {
		t.Fatal(err)
	}
	if spec.Description != "user" {
		t.Fatalf("user team should override bundled team, got %q", spec.Description)
	}
}

func writeTeamFixture(t *testing.T, root, name, description string) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(dir, "leader"), 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		TeamConfigFile:                                 fmt.Sprintf("name: %s\ndescription: %s\nentry_agent: leader\nagents:\n  - leader\n", name, description),
		TeamSystemFile:                                 "team system",
		CompletionPolicyFile:                           "complete",
		filepath.Join("leader", AgentConfigFile):       "name: leader\n",
		filepath.Join("leader", AgentSystemPromptFile): "leader prompt",
	}
	for rel, content := range files {
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestJournalCheckpointRestoresTeamWithoutSidecars(t *testing.T) {
	root := t.TempDir()
	teamDir := filepath.Join(root, "teams")
	writeTeamFixture(t, teamDir, "checkpoint-team", "checkpoint")
	cfg := config.NewConfigForCWD(filepath.Join(root, "work"))
	cfg.TeamDir = teamDir
	cfg.SessionDir = filepath.Join(root, "sessions")
	manager := NewManager(cfg, nil, nil)
	manager.UseJournalPersistence(true)
	session, err := manager.Start("parent", "checkpoint-team", cfg.CWD)
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	session.dialogue = append(session.dialogue, DialogueEntry{FromAgent: "leader", Content: "durable"})
	session.loopIteration = 3
	session.mu.Unlock()
	checkpoint := session.ExportRuntimeCheckpoint()
	if _, err := os.Stat(filepath.Join(session.rootDir, "team.json")); !os.IsNotExist(err) {
		t.Fatalf("journal mode wrote a team sidecar: %v", err)
	}
	restoredManager := NewManager(cfg, nil, nil)
	restoredManager.UseJournalPersistence(true)
	snapshots := restoredManager.RestoreRuntimeCheckpoints("parent", cfg.CWD, []RuntimeCheckpoint{checkpoint})
	if len(snapshots) != 1 || snapshots[0].LoopIteration != 3 || len(snapshots[0].Dialogue) != 1 || snapshots[0].Dialogue[0].Content != "durable" {
		t.Fatalf("unexpected restored checkpoint: %#v", snapshots)
	}
	if _, err := restoredManager.Get(checkpoint.Snapshot.TeamSessionID); err != nil {
		t.Fatal(err)
	}
}
