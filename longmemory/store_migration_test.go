package longmemory

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
)

func TestConcurrentStoreOpenSharesCompletedMigration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.sqlite")
	const workers = 8
	var group sync.WaitGroup
	errors := make(chan error, workers)
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			store, err := Open(context.Background(), path)
			if err == nil {
				err = store.Close()
			}
			errors <- err
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	stateValue, ok := storeMigrations.Load(filepath.Clean(path))
	if !ok || !stateValue.(*storeMigrationState).complete {
		t.Fatal("store migration was not recorded as complete")
	}
}
