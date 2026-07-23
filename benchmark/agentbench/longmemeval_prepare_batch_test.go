package agentbench

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"LuminaCode/memory"
)

func TestSemanticPrewarmTasksIgnoreQuestionAndEvaluationFields(t *testing.T) {
	history := [][]map[string]any{{
		{"role": "user", "content": "I prefer the blue configuration for future work."},
		{"role": "assistant", "content": "Understood."},
	}}
	base := longMemEvalCase{QuestionID: "first", Question: "unrelated prompt", Answer: "hidden-a",
		QuestionType: "type-a", QuestionDate: "2025-01-03", HaystackSessionIDs: []string{"session-1"},
		HaystackDates: []string{"2025-01-02"}, HaystackSessions: history}
	changed := base
	changed.QuestionID = "second"
	changed.Question = "different prompt"
	changed.Answer = map[string]any{"gold": "hidden-b"}
	changed.QuestionType = "type-b"

	work, err := planLongMemEvalSemanticWork(context.Background(), memory.NewLocalSemanticPlanner(nil),
		[]longMemEvalCase{base, changed})
	if err != nil {
		t.Fatal(err)
	}
	tasks := work.Tasks
	if len(tasks) != 1 {
		t.Fatalf("identical history produced %d semantic tasks, want 1", len(tasks))
	}
	if tasks[0].SessionRef != "session-1" || len(tasks[0].Sources) != 1 {
		t.Fatalf("unexpected semantic task: %+v", tasks[0])
	}
	for _, id := range []string{"first", "second"} {
		keys := work.CaseTaskKeys[id]
		if len(keys) != 1 || keys[0] != tasks[0].Key {
			t.Fatalf("case %s dependencies=%v, want shared task %s", id, keys, tasks[0].Key)
		}
	}
}

func TestSemanticDependencyTrackerReleasesCasesIncrementally(t *testing.T) {
	cases := []longMemEvalCase{{QuestionID: "first"}, {QuestionID: "second"}, {QuestionID: "raw-only"}}
	tracker, err := newLongMemEvalSemanticDependencyTracker(cases, map[string][]string{
		"first": {"shared"}, "second": {"shared", "other"},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := tracker.ReleaseReadyCases(); len(got) != 1 || got[0] != 2 {
		t.Fatalf("initial ready cases=%v, want [2]", got)
	}
	if got := tracker.MarkTasksReady([]string{"shared"}); len(got) != 1 || got[0] != 0 {
		t.Fatalf("shared task released=%v, want [0]", got)
	}
	if got := tracker.MarkTasksReady([]string{"shared"}); len(got) != 0 {
		t.Fatalf("duplicate task released cases again: %v", got)
	}
	if got := tracker.MarkTasksReady([]string{"other"}); len(got) != 1 || got[0] != 1 {
		t.Fatalf("other task released=%v, want [1]", got)
	}
	if got := tracker.UnreleasedCaseIDs(); len(got) != 0 {
		t.Fatalf("unreleased cases=%v, want none", got)
	}
}

func TestSemanticTaskPriorityCompletesSmallCasesFirst(t *testing.T) {
	tasks := []longMemEvalSemanticTask{
		{Key: "slow-b", Class: "latin-long-1", SessionRef: "b"},
		{Key: "fast", Class: "latin-short-1", SessionRef: "fast"},
		{Key: "slow-a", Class: "latin-medium-1", SessionRef: "a"},
	}
	cases := []longMemEvalCase{{QuestionID: "slow"}, {QuestionID: "fast"}}
	ordered := prioritizeLongMemEvalSemanticTasks(tasks, cases, map[string][]string{
		"slow": {"slow-b", "slow-a"}, "fast": {"fast"},
	})
	if len(ordered) != len(tasks) {
		t.Fatalf("ordered tasks=%d, want %d", len(ordered), len(tasks))
	}
	if ordered[0].Key != "fast" {
		t.Fatalf("first task=%s, want fast case dependency", ordered[0].Key)
	}
	if ordered[1].Key != "slow-b" || ordered[2].Key != "slow-a" {
		t.Fatalf("slow case class order=%v, want [slow-b slow-a]", []string{ordered[1].Key, ordered[2].Key})
	}
}

func TestMemorySearchAndAnswerContainNoDatasetEntityRules(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve test source path")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	paths := []string{filepath.Join(root, "memory", "search.go"),
		filepath.Join(root, "benchmark", "agentbench", "longmemeval_answer.go")}
	for _, path := range paths {
		encoded, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.ToLower(string(encoded))
		for _, forbidden := range []string{"memory_qa_local", "instagram follower", "subscriber", "pre-1920",
			"coffee limit", "cocktail class", "guitar lesson", "fresh herb", "luxury item"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("%s contains dataset-specific rule %q", path, forbidden)
			}
		}
	}
}

func TestLongMemEvalSemanticPrewarmWorkers(t *testing.T) {
	t.Setenv("LUMINA_LONGMEMEVAL_SEMANTIC_PREWARM_CONCURRENCY", "20")
	if got := longMemEvalSemanticPrewarmWorkers(); got != 20 {
		t.Fatalf("workers=%d, want 20", got)
	}
	t.Setenv("LUMINA_LONGMEMEVAL_SEMANTIC_PREWARM_CONCURRENCY", "invalid")
	if got := longMemEvalSemanticPrewarmWorkers(); got != longMemEvalSemanticPrewarmDefaultWorkers {
		t.Fatalf("invalid workers=%d, want default %d", got, longMemEvalSemanticPrewarmDefaultWorkers)
	}
	t.Setenv("LUMINA_LONGMEMEVAL_SEMANTIC_PREWARM_CONCURRENCY", "1000")
	if got := longMemEvalSemanticPrewarmWorkers(); got != longMemEvalSemanticPrewarmMaxWorkers {
		t.Fatalf("bounded workers=%d, want max %d", got, longMemEvalSemanticPrewarmMaxWorkers)
	}
}
