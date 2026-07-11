package test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"LuminaCode/longmemory"
)

func TestMemoryLifecycleArchivesOnlyEligibleMemory(t *testing.T) {
	ctx := context.Background()
	store, err := longmemory.Open(ctx, filepath.Join(t.TempDir(), "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	old := now.AddDate(0, 0, -90)
	low, err := store.Upsert(ctx, longmemory.Candidate{ScopeType: longmemory.ScopeProject, ScopeKey: "project-a",
		MemoryType: longmemory.TypeEpisodic, Title: "Low value", Content: "replaceable observation",
		Importance: 0.05, Confidence: 0.05, RetentionExpiresAt: old})
	if err != nil {
		t.Fatal(err)
	}
	high, err := store.Upsert(ctx, longmemory.Candidate{ScopeType: longmemory.ScopeProject, ScopeKey: "project-a",
		MemoryType: longmemory.TypeSemantic, Title: "High value", Content: "important durable fact",
		Importance: 1, Confidence: 1, RetentionExpiresAt: old})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := store.Upsert(ctx, longmemory.Candidate{ScopeType: longmemory.ScopeProject, ScopeKey: "project-a",
		MemoryType: longmemory.TypeEpisodic, Title: "Pinned", Content: "keep this observation",
		Importance: 0.01, Confidence: 0.01, RetentionExpiresAt: old})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Pin(ctx, pinned.MemoryID, true); err != nil {
		t.Fatal(err)
	}
	policy := longmemory.DefaultLifecyclePolicy()
	policy.ArchiveGraceDays = 0
	decisions, err := store.PreviewMaintenance(ctx, policy, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyMaintenance(ctx, decisions); err != nil {
		t.Fatal(err)
	}
	archived, err := store.Get(ctx, low.MemoryID)
	if err != nil || archived.Status != longmemory.StatusArchived || archived.ArchiveReason == "" {
		t.Fatalf("low-value memory was not archived: %#v %v", archived, err)
	}
	keptHigh, _ := store.Get(ctx, high.MemoryID)
	keptPinned, _ := store.Get(ctx, pinned.MemoryID)
	if keptHigh.Status != longmemory.StatusActive || keptPinned.Status != longmemory.StatusActive {
		t.Fatalf("protected memories were archived: high=%#v pinned=%#v", keptHigh, keptPinned)
	}
}

func TestMemoryLifecycleAccessRestoreAndEvents(t *testing.T) {
	ctx := context.Background()
	store, err := longmemory.Open(ctx, filepath.Join(t.TempDir(), "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entry, err := store.Upsert(ctx, longmemory.Candidate{ScopeType: longmemory.ScopeProject, ScopeKey: "project-a",
		Title: "Lifecycle", Content: "lifecycle evidence", Importance: 0.1, Confidence: 0.1})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Archive(ctx, entry.MemoryID, "test_archive"); err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(ctx, entry.MemoryID); err != nil {
		t.Fatal(err)
	}
	if err := store.RecordAccess(ctx, []string{entry.MemoryID}); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Get(ctx, entry.MemoryID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Status != longmemory.StatusActive || restored.Temperature != longmemory.TemperatureHot || restored.AccessCount < 2 {
		t.Fatalf("restore/access did not heat memory: %#v", restored)
	}
	events, err := store.ListLifecycleEvents(ctx, entry.MemoryID, 10)
	if err != nil || len(events) < 2 {
		t.Fatalf("missing lifecycle events: %#v %v", events, err)
	}
}

func TestMemoryLifecyclePreviewIsDeterministic(t *testing.T) {
	ctx := context.Background()
	store, err := longmemory.Open(ctx, filepath.Join(t.TempDir(), "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	entry, err := store.Upsert(ctx, longmemory.Candidate{ScopeType: longmemory.ScopeProject, ScopeKey: "project-a",
		Title: "Deterministic", Content: "deterministic lifecycle", Importance: 0.01, Confidence: 0.01,
		RetentionExpiresAt: now.AddDate(0, 0, -60)})
	if err != nil {
		t.Fatal(err)
	}
	policy := longmemory.DefaultLifecyclePolicy()
	policy.ArchiveGraceDays = 0
	first, err := store.CalculateLifecycle(ctx, entry.MemoryID, policy, now)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CalculateLifecycle(ctx, entry.MemoryID, policy, now)
	if err != nil {
		t.Fatal(err)
	}
	if first.Action != second.Action || first.ProposedState.ValueScore != second.ProposedState.ValueScore {
		t.Fatalf("lifecycle preview changed without state changes: %#v %#v", first, second)
	}
	if _, err := store.ApplyMaintenance(ctx, []longmemory.LifecycleDecision{first}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyMaintenance(ctx, []longmemory.LifecycleDecision{first}); err != nil {
		t.Fatalf("reentrant maintenance should be idempotent: %v", err)
	}
	events, err := store.ListLifecycleEvents(ctx, entry.MemoryID, 10)
	if err != nil {
		t.Fatal(err)
	}
	archives := 0
	for _, event := range events {
		if event.EventType == "archive" {
			archives++
		}
	}
	if archives != 1 {
		t.Fatalf("reentrant maintenance duplicated archive event: %#v", events)
	}
}
