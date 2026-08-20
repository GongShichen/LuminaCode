package session

import (
	"os"
	"testing"
)

func TestLocalMigrationMarkerMakesSessionReadOnly(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save("session-1", []map[string]any{{"role": "user", "content": "before"}}, 1); err != nil {
		t.Fatal(err)
	}
	if err := MarkLocalRuntimeMigrated(dir, ClusterMigrationReport{MigrationID: "migration-1",
		TenantID: "tenant-a", SessionID: "session-1", EventCount: 1, Checksum: "checksum"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save("session-1", []map[string]any{{"role": "user", "content": "after"}}, 2); err == nil {
		t.Fatal("migrated local session accepted a write")
	}
	store.Delete("session-1")
	if _, err := os.Stat(LocalMigrationMarker(dir, "session-1")); err != nil {
		t.Fatalf("migrated source was deleted: %v", err)
	}
}
