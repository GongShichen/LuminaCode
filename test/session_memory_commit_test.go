package test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/config"
	"LuminaCode/sessionmemory"
)

func TestSessionMemoryCommitsOneSummaryPerInterval(t *testing.T) {
	cfg := sessionMemoryTestConfig(t)
	cfg.SessionMemoryTurnInterval = 2
	store, err := sessionmemory.Open(context.Background(), cfg, "sess", fakeSummary("interval"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.IngestMessages(context.Background(), sessionMemoryMessages(4)); err != nil {
		t.Fatal(err)
	}
	if err := store.MaybeCommit(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListCommits(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("expected one commit per two-turn interval, got %#v", items)
	}
	if items[0].StartTurnCount != 3 || items[0].EndTurnCount != 4 ||
		items[1].StartTurnCount != 1 || items[1].EndTurnCount != 2 {
		t.Fatalf("unexpected interval boundaries: %#v", items)
	}
}

func TestSessionMemoryManagerQueuesIntervalsWhileSummaryRuns(t *testing.T) {
	cfg := sessionMemoryTestConfig(t)
	cfg.SessionMemoryTurnInterval = 2
	started := make(chan string, 2)
	release := make(chan struct{})
	manager := sessionmemory.NewManagerWithSummaryFunc(func(ctx context.Context, systemPrompt string, messages []map[string]any, maxTokens int) (string, error) {
		text := fmt.Sprint(messages[0]["content"])
		started <- text
		<-release
		return `{"title":"queued","summary":"queued summary","tags":["queue"]}`, nil
	})

	if err := manager.Observe(context.Background(), cfg, "sess", sessionMemoryMessages(2), false); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first interval summary did not start")
	}
	done := make(chan error, 1)
	go func() {
		done <- manager.Observe(context.Background(), cfg, "sess", sessionMemoryMessages(4), false)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Observe should enqueue the second interval without waiting for the first summary")
	}
	release <- struct{}{}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("second interval summary was not queued after the first completed")
	}
	release <- struct{}{}
	waitForSessionCommits(t, cfg, "sess", 2)
}

func TestSessionMemoryLRUEvictsOldCommits(t *testing.T) {
	cfg := sessionMemoryTestConfig(t)
	cfg.SessionMemoryTurnInterval = 1
	cfg.SessionMemoryMaxCommits = 2
	store, err := sessionmemory.Open(context.Background(), cfg, "sess", fakeSummary("lru"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.IngestMessages(context.Background(), sessionMemoryMessages(4)); err != nil {
		t.Fatal(err)
	}
	if err := store.MaybeCommit(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if err := store.Evict(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, err := store.ListCommits(context.Background(), "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].CommitNo != 4 || items[1].CommitNo != 3 {
		t.Fatalf("expected newest two commits to remain, got %#v", items)
	}
}

func TestSessionHistoryGetMessageLimit(t *testing.T) {
	cfg := sessionMemoryTestConfig(t)
	cfg.SessionMemoryTurnInterval = 3
	cfg.SessionHistoryGetMessageLimit = 2
	store, err := sessionmemory.Open(context.Background(), cfg, "sess", fakeSummary("limit"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.IngestMessages(context.Background(), sessionMemoryMessages(3)); err != nil {
		t.Fatal(err)
	}
	if err := store.MaybeCommit(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	detail, err := store.GetCommit(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Messages) != 2 || detail.OmittedMessages == 0 {
		t.Fatalf("expected get message limit to trim raw snippets, got %#v", detail)
	}
}

func TestSessionMemorySkipsTransientMessages(t *testing.T) {
	cfg := sessionMemoryTestConfig(t)
	cfg.SessionMemoryTurnInterval = 1
	store, err := sessionmemory.Open(context.Background(), cfg, "sess", fakeSummary("transient"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	messages := append([]map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "memory index"}}, "metadata": map[string]any{"source": "memory_index", "lumina_memory_context": true}},
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "[Compaction handoff summary]\nold"}}},
	}, sessionMemoryMessages(1)...)
	if err := store.IngestMessages(context.Background(), messages); err != nil {
		t.Fatal(err)
	}
	if err := store.MaybeCommit(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	detail, err := store.GetCommit(context.Background(), 1, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range detail.Messages {
		if strings.Contains(msg.TextPreview, "memory index") || strings.Contains(msg.TextPreview, "handoff") {
			t.Fatalf("transient/compaction message leaked into session memory: %#v", detail.Messages)
		}
	}
}

func sessionMemoryTestConfig(t *testing.T) config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg := config.NewConfigForCWD(dir)
	cfg.SessionDir = filepath.Join(dir, "sessions")
	cfg.SessionMemoryEnabled = true
	cfg.SessionMemorySummaryModel = "fake-model"
	cfg.SessionMemorySummaryMaxTokens = 128
	cfg.SessionHistoryGetMessageLimit = 20
	cfg.SessionMemoryMaxCommits = 200
	cfg.SessionMemoryMaxMessages = 4000
	return cfg
}

func sessionMemoryMessages(turns int) []map[string]any {
	var messages []map[string]any
	for i := 1; i <= turns; i++ {
		messages = append(messages, map[string]any{
			"role":    "user",
			"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("user turn %d", i)}},
			"metadata": map[string]any{
				"session_user_turn": i,
			},
		})
		messages = append(messages, map[string]any{
			"role":    "assistant",
			"content": []map[string]any{{"type": "text", "text": fmt.Sprintf("assistant turn %d", i)}},
			"metadata": map[string]any{
				"session_user_turn": i,
			},
		})
	}
	return messages
}

func fakeSummary(label string) sessionmemory.SummaryFunc {
	return func(ctx context.Context, systemPrompt string, messages []map[string]any, maxTokens int) (string, error) {
		return fmt.Sprintf(`{"title":"%s title","summary":"%s summary","tags":["%s"]}`, label, label, label), nil
	}
}

func waitForSessionCommits(t *testing.T, cfg config.Config, sessionID string, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		store, err := sessionmemory.Open(context.Background(), cfg, sessionID, fakeSummary("poll"))
		if err == nil {
			items, listErr := store.ListCommits(context.Background(), "", 10)
			_ = store.Close()
			if listErr == nil && len(items) == want {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d session commits", want)
}
