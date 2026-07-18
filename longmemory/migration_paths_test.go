package longmemory

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"LuminaCode/apppaths"
)

func TestLegacyMarkdownIsArchivedAfterSQLiteImport(t *testing.T) {
	root := t.TempDir()
	t.Setenv("LUMINA_APP_ROOT", root)
	paths, err := apppaths.ResolveCurrent()
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(paths.LegacyDataDir, "projects", "demo", "memory", "fact.md")
	if err := os.MkdirAll(filepath.Dir(source), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("# Durable fact\n\nRemember this."), 0o600); err != nil {
		t.Fatal(err)
	}

	store, err := Open(context.Background(), paths.MemoryDB)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entries, err := store.List(context.Background(), SearchOptions{Limit: 10, IncludeInactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Title != "Durable fact" {
		t.Fatalf("legacy memory was not imported: %#v", entries)
	}
	archived := filepath.Join(paths.LegacyDataDir, "memory", "markdown", "projects", "demo", "memory", "fact.md")
	if _, err := os.Stat(archived); err != nil {
		t.Fatalf("legacy Markdown was not archived: %v", err)
	}
	if _, err := os.Stat(source); !os.IsNotExist(err) {
		t.Fatalf("legacy Markdown source still exists: %v", err)
	}
}
