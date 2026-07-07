package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"LuminaCode/config"
	luminateam "LuminaCode/team"
)

func TestRestorePersistedTeamDoesNotReviveStaleRunningState(t *testing.T) {
	root := repoRoot(t)
	parentSessionID := "parent-session"
	teamSessionID := "team-stale-running"
	sessionDir := t.TempDir()
	teamRoot := filepath.Join(sessionDir, parentSessionID, "teams", teamSessionID)
	if err := os.MkdirAll(teamRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	persisted := map[string]any{
		"id":                teamSessionID,
		"parent_session_id": parentSessionID,
		"team":              "product-development",
		"snapshot": map[string]any{
			"team_mode":             true,
			"team_session_id":       teamSessionID,
			"active_team_id":        "product-development",
			"active_team_name":      "Product Development Team",
			"team_loop_iteration":   3,
			"running":               true,
			"waiting_for_user":      true,
			"team_dialogue_entries": []any{},
			"team_activity_rows": []any{
				map[string]any{
					"agent_id":     "qa",
					"display_name": "QA",
					"status":       "running",
					"summary":      "using run_shell",
					"task_id":      "task-1",
				},
			},
			"team_gate_status": map[string]any{},
			"input_enabled":    false,
		},
	}
	data, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamRoot, "team.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.NewConfigForCWD(root)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = sessionDir
	manager := luminateam.NewManager(cfg, nil, nil)
	snapshots := manager.RestorePersistedForParent(parentSessionID, root)
	if len(snapshots) != 1 {
		t.Fatalf("expected one restored team snapshot, got %d", len(snapshots))
	}
	snapshot := snapshots[0]
	if snapshot.Running {
		t.Fatal("restored team must not be marked running without an active backend goroutine")
	}
	if snapshot.WaitingForUser {
		t.Fatal("restored team must not keep a stale waiting-for-user state")
	}
	if !snapshot.InputEnabled {
		t.Fatal("restored stale team should allow input")
	}
	if len(snapshot.ActivityRows) == 0 {
		t.Fatal("expected restored activity rows")
	}
	for _, row := range snapshot.ActivityRows {
		if row.AgentID == "qa" {
			if row.Status == "running" {
				t.Fatal("stale running activity row was not normalized")
			}
			if row.Status != "interrupted" {
				t.Fatalf("expected qa row status interrupted, got %q", row.Status)
			}
			return
		}
	}
	t.Fatal("expected qa activity row")
}
