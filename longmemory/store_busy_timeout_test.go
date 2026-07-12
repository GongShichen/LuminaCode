package longmemory

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenWithBusyTimeoutConfiguresStoreConnections(t *testing.T) {
	store, err := OpenWithBusyTimeout(context.Background(), filepath.Join(t.TempDir(), "memory.sqlite"), 1234*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var milliseconds int64
	if err := store.db.QueryRow(`PRAGMA busy_timeout`).Scan(&milliseconds); err != nil {
		t.Fatal(err)
	}
	if milliseconds != 1234 {
		t.Fatalf("busy timeout = %dms, want 1234ms", milliseconds)
	}
}
