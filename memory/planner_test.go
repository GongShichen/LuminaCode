package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type semanticPlannerRecordingVectorizer struct {
	mu     sync.Mutex
	counts map[string]int
}

func (v *semanticPlannerRecordingVectorizer) Model() string   { return "test" }
func (v *semanticPlannerRecordingVectorizer) Dimensions() int { return 2 }

func (v *semanticPlannerRecordingVectorizer) Embed(_ context.Context, texts []string,
	_ VectorPurpose) ([][]float32, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	result := make([][]float32, len(texts))
	for index, text := range texts {
		v.counts[text]++
		if text == genericAssistantPrototype {
			result[index] = []float32{0, 1}
		} else {
			result[index] = []float32{1, 0}
		}
	}
	return result, nil
}

func TestLocalSemanticPlannerUsesSemanticClassificationWithoutPhraseRouting(t *testing.T) {
	start := time.Now().UTC()
	events := []RawEvent{
		{ID: "generic", SessionID: "s1", Actor: "assistant", Content: "Mercury is the closest planet to the Sun.", OccurredAt: start},
		{ID: "preference", SessionID: "s1", Actor: "user", Content: "I prefer concise answers.", OccurredAt: start.Add(time.Second)},
		{ID: "proposal", SessionID: "s2", Actor: "assistant", Content: "Use the quartz deployment profile for this project.", OccurredAt: start.Add(2 * time.Second)},
		{ID: "accepted", SessionID: "s2", Actor: "user", Content: "That works, use that.", OccurredAt: start.Add(3 * time.Second)},
		{ID: "tool", SessionID: "s2", Actor: "tool", SourceKind: "command", Content: "command succeeded", OccurredAt: start.Add(4 * time.Second)},
	}
	planner := NewLocalSemanticPlanner(&semanticPlannerRecordingVectorizer{counts: map[string]int{}})
	plan, err := planner.Plan(context.Background(), events, PlanningOptions{Mode: WriteNormal, MaxSources: 8,
		MaxSourcesPerSession: 2, MaxSourceRunes: 900})
	if err != nil {
		t.Fatal(err)
	}
	selected := map[string]bool{}
	for _, candidate := range plan.Candidates {
		selected[candidate.EventID] = true
	}
	if selected["generic"] || selected["proposal"] || selected["tool"] ||
		!selected["preference"] || !selected["accepted"] {
		t.Fatalf("unexpected planner selection: candidates=%+v skipped=%v", plan.Candidates, plan.SkippedEventIDs)
	}
}

func TestLocalSemanticPlannerEnforcesCaseAndSessionCaps(t *testing.T) {
	start := time.Now().UTC()
	var events []RawEvent
	for session := 0; session < 20; session++ {
		for turn := 0; turn < 4; turn++ {
			events = append(events, RawEvent{ID: fmt.Sprintf("%d-%d", session, turn),
				SessionID: fmt.Sprintf("session-%d", session), Actor: "user",
				Content:    fmt.Sprintf("I prefer profile %d option %d.", session, turn),
				OccurredAt: start.Add(time.Duration(session*10+turn) * time.Second)})
		}
	}
	plan, err := NewLocalSemanticPlanner(&semanticPlannerRecordingVectorizer{counts: map[string]int{}}).
		Plan(context.Background(), events, PlanningOptions{
			Mode: WriteImport, MaxSources: 32, MaxSourcesPerSession: 2, MaxSourceRunes: 900})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Candidates) != 32 {
		t.Fatalf("selected %d sources, want hard cap 32", len(plan.Candidates))
	}
	perSession := map[string]int{}
	for _, candidate := range plan.Candidates {
		perSession[candidate.Source.SessionRef]++
		if perSession[candidate.Source.SessionRef] > 2 {
			t.Fatalf("session %s exceeded cap: %d", candidate.Source.SessionRef, perSession[candidate.Source.SessionRef])
		}
		if len([]rune(candidate.Source.Text)) > 900 {
			t.Fatalf("source exceeded rune cap: %d", len([]rune(candidate.Source.Text)))
		}
	}
}

func TestSemanticPlanningTextBoundsEmbeddingInputAndKeepsBothEnds(t *testing.T) {
	value := "BEGIN-IDENTITY " + strings.Repeat("middle ", 200) + " FINAL-CORRECTION"
	got := semanticPlanningText(value)
	if len([]rune(got)) > semanticPlanningRunes {
		t.Fatalf("planning text has %d runes, want at most %d", len([]rune(got)), semanticPlanningRunes)
	}
	if !strings.Contains(got, "BEGIN-IDENTITY") || !strings.Contains(got, "FINAL-CORRECTION") {
		t.Fatalf("planning text lost an endpoint: %q", got)
	}
}

func TestLocalSemanticPlannerBatchMatchesIndependentPlansAndDeduplicatesEmbedding(t *testing.T) {
	text := "The blue configuration remains available for future sessions."
	start := time.Now().UTC()
	batches := []SemanticPlanningBatch{
		{Events: []RawEvent{{ID: "one", SessionID: "session-one", Actor: "user", Content: text,
			OccurredAt: start}}, Options: PlanningOptions{Mode: WriteImport}},
		{Events: []RawEvent{{ID: "two", SessionID: "session-two", Actor: "user", Content: text,
			OccurredAt: start.Add(time.Second)}}, Options: PlanningOptions{Mode: WriteImport}},
	}
	batchedVectorizer := &semanticPlannerRecordingVectorizer{counts: map[string]int{}}
	planner := NewLocalSemanticPlanner(batchedVectorizer).(BatchSemanticPlanner)
	batched, err := planner.PlanBatch(context.Background(), batches)
	if err != nil {
		t.Fatal(err)
	}
	if batchedVectorizer.counts[text] != 1 {
		t.Fatalf("batch embedded duplicate text %d times, want 1", batchedVectorizer.counts[text])
	}
	for index, batch := range batches {
		independentVectorizer := &semanticPlannerRecordingVectorizer{counts: map[string]int{}}
		independent, err := NewLocalSemanticPlanner(independentVectorizer).Plan(context.Background(),
			batch.Events, batch.Options)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(batched[index].Candidates) != fmt.Sprint(independent.Candidates) ||
			fmt.Sprint(batched[index].SkippedEventIDs) != fmt.Sprint(independent.SkippedEventIDs) {
			t.Fatalf("batch %d changed planning result: batch=%+v independent=%+v",
				index, batched[index], independent)
		}
	}
}
