package test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/backend"
	"LuminaCode/config"
	"LuminaCode/longmemory"
)

func TestLongMemoryStoreSearchScopeAndLifecycle(t *testing.T) {
	ctx := context.Background()
	projectRoot := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(projectRoot, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	projectScope := longmemory.ProjectScopeKey(projectRoot)
	active, err := store.Upsert(ctx, longmemory.Candidate{
		ScopeType:  longmemory.ScopeProject,
		ScopeKey:   projectScope,
		MemoryType: longmemory.TypeProject,
		Title:      "Build Command",
		Content:    "Run go test ./... before committing runtime changes.",
		Tags:       []string{"build", "go"},
		Entities:   []string{"go test"},
		Importance: 0.9,
		Confidence: 0.95,
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := store.Upsert(ctx, longmemory.Candidate{
		ScopeType:  longmemory.ScopeUser,
		ScopeKey:   longmemory.UserScopeKey(),
		MemoryType: longmemory.TypePreference,
		Status:     longmemory.StatusPending,
		Title:      "Pending Preference",
		Content:    "This preference must not be recalled before approval.",
	})
	if err != nil {
		t.Fatal(err)
	}

	found, err := store.Search(ctx, longmemory.SearchOptions{
		Query:  "go test runtime",
		Scopes: []longmemory.Scope{{Type: longmemory.ScopeProject, Key: projectScope}},
		Limit:  5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].MemoryID != active.MemoryID {
		t.Fatalf("expected active project memory only, got %#v", found)
	}

	all, err := store.List(ctx, longmemory.SearchOptions{IncludeInactive: true, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("expected active + pending in management list, got %#v", all)
	}

	if err := store.SetStatus(ctx, pending.MemoryID, longmemory.StatusActive); err != nil {
		t.Fatal(err)
	}
	found, err = store.Search(ctx, longmemory.SearchOptions{Query: "preference approval", Scopes: []longmemory.Scope{{Type: longmemory.ScopeUser, Key: longmemory.UserScopeKey()}}, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].MemoryID != pending.MemoryID {
		t.Fatalf("approved pending memory should become searchable, got %#v", found)
	}

	if err := store.Delete(ctx, active.MemoryID, false); err != nil {
		t.Fatal(err)
	}
	found, err = store.Search(ctx, longmemory.SearchOptions{Query: "go test runtime", Scopes: []longmemory.Scope{{Type: longmemory.ScopeProject, Key: projectScope}}, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("soft-deleted memory should not be recalled, got %#v", found)
	}
	if err := store.Delete(ctx, active.MemoryID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(ctx, active.MemoryID); !longmemory.IsNotFound(err) {
		t.Fatalf("hard-deleted memory should be gone, err=%v", err)
	}
}

func TestLongMemoryGovernanceAndExpiry(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	expired := longmemory.ApplyRetention(longmemory.Candidate{
		ScopeType:  longmemory.ScopeProject,
		ScopeKey:   longmemory.ProjectScopeKey(root),
		MemoryType: longmemory.TypeEpisodic,
		Title:      "Expired Incident",
		Content:    "An old incident that should not be recalled.",
	}, longmemory.RetentionPolicy{longmemory.TypeEpisodic: 1}, time.Now().AddDate(0, 0, -3))
	entry, err := store.Upsert(ctx, expired)
	if err != nil {
		t.Fatal(err)
	}
	found, err := store.Search(ctx, longmemory.SearchOptions{Query: "old incident", Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("expired memory should not be recalled by default: %#v", found)
	}
	found, err = store.Search(ctx, longmemory.SearchOptions{Query: "old incident", Limit: 5, IncludeExpired: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].MemoryID != entry.MemoryID {
		t.Fatalf("include expired should expose expired memory: %#v", found)
	}
	found, err = store.Search(ctx, longmemory.SearchOptions{Query: "old incident", Limit: 5, IncludeExpired: true, CreatedAfter: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("created_after should filter future window, got %#v", found)
	}
	if err := store.Deprioritize(ctx, entry.MemoryID); err != nil {
		t.Fatal(err)
	}
	updated, err := store.Get(ctx, entry.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Importance != 0 {
		t.Fatalf("deprioritize should set importance to 0, got %.2f", updated.Importance)
	}
	replacement, err := store.Upsert(ctx, longmemory.Candidate{
		ScopeType:  longmemory.ScopeProject,
		ScopeKey:   longmemory.ProjectScopeKey(root),
		MemoryType: longmemory.TypeEpisodic,
		Title:      "Replacement Incident",
		Content:    "Replacement memory.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Supersede(ctx, entry.MemoryID, replacement.MemoryID); err != nil {
		t.Fatal(err)
	}
	old, err := store.Get(ctx, entry.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != longmemory.StatusSuperseded || old.SupersededBy != replacement.MemoryID {
		t.Fatalf("old memory not superseded correctly: %#v", old)
	}
}

func TestLongMemoryRecallDoesNotUseCompletionRerank(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, item := range []struct {
		title   string
		content string
	}{
		{"First", "alpha shared topic but less useful"},
		{"Second", "alpha shared topic and exact user preference"},
		{"Third", "alpha shared topic and unrelated note"},
	} {
		_, err := store.Upsert(ctx, longmemory.Candidate{
			ScopeType:  longmemory.ScopeProject,
			ScopeKey:   longmemory.ProjectScopeKey(root),
			MemoryType: longmemory.TypeProject,
			Title:      item.title,
			Content:    item.content,
			Importance: 0.5,
			Confidence: 0.9,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.NewConfigForCWD(root)
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = store.Path()
	cfg.MemoryRecallMaxItems = 1
	state := agent.NewAgentState()
	state.LastQuery = "alpha preference"
	state.Messages = []map[string]any{{"role": "user", "content": "alpha preference"}}
	recalls := agent.RunMemoryRecallWithConfig(ctx, cfg, &state, state.LastQuery)
	if len(recalls) != 1 || !strings.Contains(recalls[0].Content, "alpha shared topic") {
		t.Fatalf("expected locally fused evidence, got %#v", recalls)
	}
}

func TestLongMemoryMissingEmbeddingKeepsOtherChannelCandidates(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		if _, err := store.Upsert(ctx, longmemory.Candidate{ScopeType: longmemory.ScopeProject,
			ScopeKey: longmemory.ProjectScopeKey(root), MemoryType: longmemory.TypeProject,
			Title: "Release rule", Content: "release rule candidate " + string(rune('a'+index)), Confidence: 0.9}); err != nil {
			t.Fatal(err)
		}
	}
	_ = store.Close()
	cfg := config.NewConfigForCWD(root)
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = filepath.Join(root, "memory.sqlite")
	cfg.MemoryRecallMaxItems = 1
	cfg.MemoryEmbeddingEnabled = true
	cfg.MemoryEmbeddingModelDir = filepath.Join(root, "missing-model")
	state := agent.NewAgentState()
	state.Messages = []map[string]any{{"role": "user", "content": "release rule"}}
	recalls := agent.RunMemoryRecallWithConfig(ctx, cfg, &state, "release rule")
	if len(recalls) != 1 || !strings.Contains(recalls[0].Content, "release rule candidate") {
		t.Fatalf("missing vector channel discarded local candidates: %#v", recalls)
	}
}

func TestLongMemoryExportImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := longmemory.Open(ctx, filepath.Join(root, "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Upsert(ctx, longmemory.Candidate{
		ScopeType:  longmemory.ScopeProject,
		ScopeKey:   longmemory.ProjectScopeKey(root),
		MemoryType: longmemory.TypeReference,
		Title:      "Reference Link",
		Content:    "Use the internal release checklist for deployment.",
		Tags:       []string{"release"},
	}); err != nil {
		t.Fatal(err)
	}
	exportDir, err := longmemory.ExportMarkdown(ctx, store, filepath.Join(root, "export"))
	if err != nil {
		t.Fatal(err)
	}

	imported, err := longmemory.Open(ctx, filepath.Join(root, "imported.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer imported.Close()
	count, err := backend.ImportMemoryCandidates(ctx, imported, exportDir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("expected one imported memory, got %d", count)
	}
	found, err := imported.Search(ctx, longmemory.SearchOptions{Query: "release checklist", Limit: 5, IncludeInactive: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Title != "Reference Link" {
		t.Fatalf("unexpected imported memories: %#v", found)
	}
}
