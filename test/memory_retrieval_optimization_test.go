package test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
	"LuminaCode/longmemory"
)

func TestSessionChunkSearchConstrainsBeforeTopK(t *testing.T) {
	ctx := context.Background()
	store, err := longmemory.Open(ctx, filepath.Join(t.TempDir(), "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: "session-search"}
	for index := 0; index < 40; index++ {
		text := fmt.Sprintf("needle lighthouse generic distractor repeated repeated repeated %d", index)
		span := longmemory.EvidenceSpan{ScopeType: scope.Type, ScopeKey: scope.Key, SessionID: "distractor-" + string(rune('a'+index)),
			MessageID: "distractor-message-" + string(rune('a'+index)), Role: "assistant", Text: text, OccurredAt: time.Now().UTC()}
		if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{Episode: &longmemory.Episode{ScopeType: scope.Type,
			ScopeKey: scope.Key, SessionID: span.SessionID, MessageIDs: []string{span.MessageID}, Content: text,
			OccurredAt: span.OccurredAt, ObservedAt: span.OccurredAt}, EpisodeSpans: []longmemory.EvidenceSpan{span}}); err != nil {
			t.Fatal(err)
		}
	}
	target := "needle lighthouse chose the cobalt deployment window"
	span := longmemory.EvidenceSpan{ScopeType: scope.Type, ScopeKey: scope.Key, SessionID: "target-session",
		MessageID: "target-message", Role: "user", Text: target, OccurredAt: time.Now().UTC()}
	if err := store.CommitExtraction(ctx, longmemory.ExtractionBatch{Episode: &longmemory.Episode{ScopeType: scope.Type,
		ScopeKey: scope.Key, SessionID: span.SessionID, MessageIDs: []string{span.MessageID}, Content: target,
		OccurredAt: span.OccurredAt, ObservedAt: span.OccurredAt}, EpisodeSpans: []longmemory.EvidenceSpan{span}}); err != nil {
		t.Fatal(err)
	}
	hits, err := store.SearchSessionChunks(ctx, "target-session", "needle lighthouse", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].SourceSessionID != "target-session" || !strings.Contains(hits[0].Content, "cobalt") {
		t.Fatalf("Session query was globally truncated before filtering: %#v", hits)
	}
}

func TestMemoryReferenceTimeIsInjectedIntoHiddenRecall(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.LongTermMemoryStore = filepath.Join(root, "memory.sqlite")
	cfg.MemoryEmbeddingEnabled = false
	cfg.MemoryQueryExpansionEnabled = false
	store, err := longmemory.Open(ctx, cfg.LongTermMemoryStore)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Upsert(ctx, longmemory.Candidate{ScopeType: longmemory.ScopeProject,
		ScopeKey: longmemory.ProjectScopeKey(root), MemoryType: longmemory.TypeProject, Title: "Release",
		Content: "The release happened two days before the checkpoint.", Confidence: 1})
	_ = store.Close()
	state := agent.NewAgentState()
	reference := time.Date(2031, 2, 3, 4, 5, 6, 0, time.UTC)
	state.MemoryQueryTime, state.LastQuery = reference, "When was the release?"
	recalls := agent.RunMemoryRecallWithConfig(ctx, cfg, &state, state.LastQuery)
	if len(recalls) == 0 || !strings.Contains(recalls[0].Content, reference.Format(time.RFC3339)) {
		t.Fatalf("reference time missing from hidden memory context: %#v", recalls)
	}
}

func TestCanonicalMemoryRespectsScopeAndBuildsTimeline(t *testing.T) {
	ctx := context.Background()
	store, err := longmemory.Open(ctx, filepath.Join(t.TempDir(), "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	allowed := longmemory.Scope{Type: longmemory.ScopeProject, Key: "allowed"}
	denied := longmemory.Scope{Type: longmemory.ScopeProject, Key: "denied"}
	for _, scope := range []longmemory.Scope{allowed, denied} {
		entity := longmemory.CanonicalEntity{ScopeType: scope.Type, ScopeKey: scope.Key, Name: "Project Aurora", Confidence: 1}
		if err := store.CommitCanonicalMerge(ctx, longmemory.CanonicalMerge{Entity: &entity, Aliases: []string{"Aurora"}}); err != nil {
			t.Fatal(err)
		}
		event := longmemory.CanonicalEvent{ScopeType: scope.Type, ScopeKey: scope.Key, Title: "Aurora launch",
			Summary: "Aurora launched", OccurredAt: time.Date(2029, 1, 2, 0, 0, 0, 0, time.UTC), Confidence: 1}
		if err := store.CommitCanonicalMerge(ctx, longmemory.CanonicalMerge{Event: &event}); err != nil {
			t.Fatal(err)
		}
	}
	entities, err := store.SearchCanonicalEntities(ctx, "Aurora", []longmemory.Scope{allowed})
	if err != nil || len(entities) != 1 || entities[0].ScopeKey != allowed.Key {
		t.Fatalf("canonical entity scope leak: %#v %v", entities, err)
	}
	events, err := store.SearchCanonicalEvents(ctx, "Aurora", []longmemory.Scope{allowed})
	if err != nil || len(events) != 1 || events[0].ScopeKey != allowed.Key {
		t.Fatalf("canonical event scope leak: %#v %v", events, err)
	}
}

func TestRetrievalCacheInvalidatesAfterMemoryWrite(t *testing.T) {
	ctx := context.Background()
	store, err := longmemory.Open(ctx, filepath.Join(t.TempDir(), "memory.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: "cache"}
	_, _ = store.Upsert(ctx, longmemory.Candidate{ScopeType: scope.Type, ScopeKey: scope.Key, MemoryType: longmemory.TypeProject,
		Title: "Cache fact", Content: "blue cache marker", Confidence: 1})
	query := longmemory.MemoryQuery{Text: "blue cache marker", Timestamp: time.Now().UTC(), Scopes: []longmemory.Scope{scope}}
	opts := longmemory.HybridSearchOptions{FTSCandidates: 10, MaxItems: 4, TargetContextTokens: 600,
		MaxContextTokens: 1000, CacheEnabled: true, CacheTTL: time.Minute}
	if _, err := store.SearchAllChannels(ctx, query, longmemory.QueryExpansion{}, nil, opts); err != nil {
		t.Fatal(err)
	}
	second, err := store.SearchAllChannels(ctx, query, longmemory.QueryExpansion{}, nil, opts)
	if err != nil || !second.Run.CacheHit {
		t.Fatalf("expected scoped cache hit: %#v %v", second.Run, err)
	}
	_, _ = store.Upsert(ctx, longmemory.Candidate{ScopeType: scope.Type, ScopeKey: scope.Key, MemoryType: longmemory.TypeProject,
		Title: "New cache fact", Content: "blue cache marker updated", Confidence: 1})
	third, err := store.SearchAllChannels(ctx, query, longmemory.QueryExpansion{}, nil, opts)
	if err != nil || third.Run.CacheHit {
		t.Fatalf("write did not invalidate retrieval cache: %#v %v", third.Run, err)
	}
}

func TestInvalidMemoryWeightsAreReported(t *testing.T) {
	appRoot := setTestAppRoot(t)
	configDir := filepath.Join(appRoot, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	contents := `{"memory_coverage_relevance_weight":0.8,"memory_coverage_facet_weight":0.2,"memory_coverage_provenance_weight":0.2,"memory_coverage_source_weight":0.1,"memory_coverage_coherence_weight":0.1}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfigForCWD(t.TempDir())
	if err := cfg.ValidateMemoryConfig(); err == nil || !strings.Contains(err.Error(), "must sum to 1") {
		t.Fatalf("invalid weights were silently accepted: errors=%#v err=%v", cfg.MemoryConfigErrors, err)
	}
}

func TestInvalidEvidenceLedgerConfigurationIsReported(t *testing.T) {
	appRoot := setTestAppRoot(t)
	configDir := filepath.Join(appRoot, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	contents := `{
		"memory_query_expansion_timeout_seconds": 1,
		"memory_query_expansion_max_additional_wait_ms": 1500,
		"memory_coverage_support_target": 0.6,
		"memory_coverage_residual_trigger": 0.8,
		"memory_context_max_tokens": 300,
		"memory_atom_structural_context_max_tokens": 384
	}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfigForCWD(t.TempDir())
	err := cfg.ValidateMemoryConfig()
	if err == nil || !strings.Contains(err.Error(), "residual_trigger") ||
		!strings.Contains(err.Error(), "structural_context") || !strings.Contains(err.Error(), "additional_wait") {
		t.Fatalf("invalid Evidence Ledger configuration was accepted: errors=%#v err=%v", cfg.MemoryConfigErrors, err)
	}
}

func TestInvalidMemoryLifecycleConfigurationIsReported(t *testing.T) {
	appRoot := setTestAppRoot(t)
	configDir := filepath.Join(appRoot, "config")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	contents := `{
		"memory_hot_access_days": 120,
		"memory_warm_access_days": 30,
		"memory_auto_hard_delete_enabled": true,
		"memory_value_weights": {
			"importance": 1,
			"confidence": 1,
			"access_recency": 0,
			"access_frequency": 0,
			"reinforcement": 0,
			"provenance_strength": 0,
			"dependency_strength": 0
		}
	}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfigForCWD(t.TempDir())
	err := cfg.ValidateMemoryConfig()
	if err == nil || !strings.Contains(err.Error(), "must not exceed") ||
		!strings.Contains(err.Error(), "must be false") || !strings.Contains(err.Error(), "must sum to 1") {
		t.Fatalf("invalid lifecycle configuration was accepted: errors=%#v err=%v", cfg.MemoryConfigErrors, err)
	}
}
