package agentbench

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/longmemory"
)

func TestCompleteMemoryExtractionRetriesUntilCursorIsComplete(t *testing.T) {
	state := agent.NewAgentState()
	state.Messages = []map[string]any{{"role": "user"}, {"role": "assistant"}}
	calls := 0
	err := completeMemoryExtraction(context.Background(), &state, func(context.Context) error {
		calls++
		if calls == 1 {
			return errors.New("transient structured response failure")
		}
		state.MemoryExtractionCursor++
		return nil
	}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 3 || state.MemoryExtractionCursor != len(state.Messages) {
		t.Fatalf("calls=%d cursor=%d, want calls=3 cursor=%d", calls, state.MemoryExtractionCursor, len(state.Messages))
	}
}

func TestCompleteMemoryExtractionStopsWhenContextIsCanceled(t *testing.T) {
	state := agent.NewAgentState()
	state.Messages = []map[string]any{{"role": "user"}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := completeMemoryExtraction(ctx, &state, func(context.Context) error {
		t.Fatal("extract called after context cancellation")
		return nil
	}, time.Second, time.Second)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v, want context canceled", err)
	}
}

func TestCompletedLongMemEvalCheckpointRequiresUsableAnswer(t *testing.T) {
	complete := CaseResult{Case: CaseSpec{ID: "case"}, Hypothesis: "answer"}
	if !completedLongMemEvalCheckpoint(complete) {
		t.Fatal("usable checkpoint was not accepted")
	}
	complete.ErrorType = "memory_seed_failed"
	if completedLongMemEvalCheckpoint(complete) {
		t.Fatal("failed checkpoint was accepted")
	}
	complete.ErrorType = ""
	complete.Hypothesis = ""
	if completedLongMemEvalCheckpoint(complete) {
		t.Fatal("empty checkpoint answer was accepted")
	}
}

func TestLongMemEvalSessionIdentityDeduplicatesIdenticalSourceRecords(t *testing.T) {
	t.Parallel()
	session := func(content string, hasAnswer bool) []map[string]any {
		return []map[string]any{{"role": "user", "content": content, "has_answer": hasAnswer}}
	}
	c := longMemEvalCase{
		HaystackSessionIDs: []string{"same", "other", "same", "same", "same"},
		HaystackSessions: [][]map[string]any{
			session("first", false),
			session("other", false),
			session("first", true),
			session("second", false),
			session("second", true),
		},
	}
	wantIDs := []string{"same", "other", "same", "same#2", "same#2"}
	wantDuplicates := []bool{false, false, true, false, true}
	for index, expected := range wantIDs {
		got, duplicate := longMemEvalSessionIdentityAt(c, index)
		if got != expected || duplicate != wantDuplicates[index] {
			t.Fatalf("session %d: got (%q, %t), want (%q, %t)",
				index, got, duplicate, expected, wantDuplicates[index])
		}
	}
	if got := longMemEvalSessionIDAt(c, 2); got != "same" {
		t.Fatalf("canonical duplicate ID = %q, want same", got)
	}
}

func TestSyncLongMemEvalExtractionCursorResumesPersistedSession(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	storePath := filepath.Join(t.TempDir(), "memory.sqlite")
	store, err := longmemory.Open(ctx, storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetCursor(ctx, "long-term-extraction:history-replay", "session", "message", 7); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	state := agent.NewAgentState()
	if err := syncLongMemEvalExtractionCursor(ctx, storePath, "session", &state); err != nil {
		t.Fatal(err)
	}
	if state.MemoryExtractionCursor != 8 {
		t.Fatalf("cursor = %d, want 8", state.MemoryExtractionCursor)
	}

	missing := agent.NewAgentState()
	if err := syncLongMemEvalExtractionCursor(ctx, storePath, "missing", &missing); err != nil {
		t.Fatal(err)
	}
	if missing.MemoryExtractionCursor != 0 {
		t.Fatalf("missing cursor = %d, want 0", missing.MemoryExtractionCursor)
	}
}

func TestResetMemoryCaseDirectoryPreservesOnlyMemoryOnResume(t *testing.T) {
	t.Parallel()
	caseDir := filepath.Join(t.TempDir(), "case")
	memoryDir := filepath.Join(caseDir, "data", "memory")
	if err := os.MkdirAll(memoryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(memoryDir, "lumina-memory.sqlite")
	walPath := storePath + "-wal"
	if err := os.WriteFile(storePath, []byte("database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(walPath, []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(caseDir, "scratch.txt"), []byte("discard"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := resetMemoryCaseDirectory(caseDir, true); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{storePath: "database", walPath: "wal"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("preserved %s = %q, %v; want %q", path, got, err, want)
		}
	}
	if _, err := os.Stat(filepath.Join(caseDir, "scratch.txt")); !os.IsNotExist(err) {
		t.Fatalf("scratch file survived resume reset: %v", err)
	}

	if err := resetMemoryCaseDirectory(caseDir, false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(caseDir); !os.IsNotExist(err) {
		t.Fatalf("non-resume reset kept case directory: %v", err)
	}
}
