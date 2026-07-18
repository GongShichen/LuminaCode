package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"LuminaCode/config"
	"LuminaCode/maintenance"
	"LuminaCode/session"
)

func TestStorageMaintenanceDryRunAndEnforceProtectsActivePinnedAndRecent(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	old := float64(now.Add(-40*24*time.Hour).UnixNano()) / 1e9
	recent := float64(now.Add(-time.Hour).UnixNano()) / 1e9

	writeFakeSession(t, dir, "old-delete", old, false)
	writeFakeSession(t, dir, "old-active", old, false)
	writeFakeSession(t, dir, "old-pinned", old, true)
	writeFakeSession(t, dir, "recent", recent, false)
	if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("orphan"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := config.Config{
		SessionDir:                    dir,
		SessionMaintenanceEnabled:     true,
		SessionRetentionDays:          30,
		SessionMaxEntries:             500,
		SessionArchiveBeforeDelete:    true,
		SessionProtectPinned:          true,
		SessionMaintenanceMode:        "warn",
		SessionHighWaterRatio:         0.8,
		TeamTimelineMaxEntries:        2000,
		TeamDialogueMaxEntries:        1000,
		SessionMemoryMaxCommits:       200,
		SessionMemoryMaxMessages:      4000,
		SessionMemoryTurnInterval:     5,
		SessionHistoryGetMessageLimit: 20,
	}
	active := map[string]struct{}{"old-active": {}}

	dryRun, err := maintenance.Cleanup(cfg, maintenance.Options{Now: now, CurrentSessions: active})
	if err != nil {
		t.Fatal(err)
	}
	if len(dryRun.Actions) != 2 {
		t.Fatalf("expected old session and orphan planned, got %#v", dryRun.Actions)
	}
	if _, err := os.Stat(filepath.Join(dir, "old-delete")); err != nil {
		t.Fatalf("dry-run should not delete old-delete: %v", err)
	}

	enforced, err := maintenance.Cleanup(cfg, maintenance.Options{Now: now, CurrentSessions: active, Enforce: true})
	if err != nil {
		t.Fatal(err)
	}
	if enforced.DeletedCount != 1 {
		t.Fatalf("expected one session deletion, got %#v", enforced)
	}
	if _, err := os.Stat(filepath.Join(dir, "old-delete")); !os.IsNotExist(err) {
		t.Fatalf("old-delete should be removed, stat err=%v", err)
	}
	for _, id := range []string{"old-active", "old-pinned", "recent"} {
		if _, err := os.Stat(filepath.Join(dir, id)); err != nil {
			t.Fatalf("%s should be protected: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(maintenance.ArchiveDir(dir), "old-delete", "summary.md")); err != nil {
		t.Fatalf("expected lightweight archive summary: %v", err)
	}
	if _, err := os.Stat(filepath.Join(maintenance.ArchiveDir(dir), "old-delete", "transcript.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("archive should not copy full transcript, stat err=%v", err)
	}
}

func TestSessionPinUpdatesMetaAndList(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	writeFakeSession(t, dir, "pin-me", float64(time.Now().UnixNano())/1e9, false)
	if _, err := store.Pin("pin-me", true); err != nil {
		t.Fatal(err)
	}
	meta := store.LoadMeta("pin-me")
	if meta == nil || !meta.Pinned {
		t.Fatalf("expected pinned meta, got %#v", meta)
	}
	found := false
	for _, listed := range store.ListSessions() {
		if listed.SessionID == "pin-me" {
			found = listed.Pinned
		}
	}
	if !found {
		t.Fatalf("ListSessions should expose pinned state")
	}
}

func TestMaintenanceConfigDefaultsRejectInvalidValues(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cfgDir := filepath.Join(home, ".lumina", "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := map[string]any{
		"session_maintenance_mode":      "delete-everything",
		"session_retention_days":        -1,
		"session_max_entries":           0,
		"session_max_disk_bytes":        -1,
		"session_high_water_ratio":      2,
		"team_timeline_max_entries":     0,
		"team_dialogue_max_entries":     -10,
		"team_artifact_max_bytes":       -1,
		"session_archive_before_delete": false,
	}
	data, _ := json.Marshal(raw)
	if err := os.WriteFile(filepath.Join(cfgDir, "settings.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfigForCWD(t.TempDir())
	if cfg.SessionMaintenanceMode != "warn" ||
		cfg.SessionRetentionDays != 30 ||
		cfg.SessionMaxEntries != 500 ||
		cfg.SessionMaxDiskBytes != 0 ||
		cfg.SessionHighWaterRatio != 0.8 ||
		cfg.TeamTimelineMaxEntries != 2000 ||
		cfg.TeamDialogueMaxEntries != 1000 ||
		cfg.TeamArtifactMaxBytes != 0 {
		t.Fatalf("invalid maintenance defaults should fall back, got %#v", cfg)
	}
	if cfg.SessionArchiveBeforeDelete {
		t.Fatalf("valid boolean override should be applied")
	}
}

func writeFakeSession(t *testing.T, root, id string, updated float64, pinned bool) {
	t.Helper()
	sessionDir := filepath.Join(root, id)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := map[string]any{
		"session_id":    id,
		"created_at":    updated,
		"last_updated":  updated,
		"message_count": 2,
		"turn_count":    1,
		"pinned":        pinned,
	}
	data, _ := json.Marshal(meta)
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "transcript.jsonl"), []byte(`{"role":"user","content":"hello"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	artifactDir := filepath.Join(sessionDir, "teams", "team-1", "artifacts")
	if err := os.MkdirAll(artifactDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifactDir, "index.json"), []byte(`[{"name":"report.md","path":"report.md"}]`), 0o644); err != nil {
		t.Fatal(err)
	}
}
