package agentbench

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"LuminaCode/memory"
)

func TestLongMemEvalTokenUsageRecordsAndAggregatesStages(t *testing.T) {
	options := RunnerOptions{OutputDir: t.TempDir(), LongMemEvalPhase: LongMemEvalPhasePrepare}
	observer := longMemEvalUsageObserver(options, "case-1")
	for _, event := range []memory.APIUsageEvent{
		{Stage: memory.APIStageSemanticCompile, Usage: memory.APIUsage{
			Calls: 1, InputTokens: 120, OutputTokens: 30, Model: "compiler"}, RecordedAt: time.Now().UTC()},
		{Stage: memory.APIStageConflictAdjudication, Usage: memory.APIUsage{
			Calls: 1, InputTokens: 40, OutputTokens: 8, Model: "judge"}, RecordedAt: time.Now().UTC()},
	} {
		if err := observer(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	usage, err := loadLongMemEvalStageUsage(filepath.Join(options.OutputDir, longMemEvalTokenUsageFilename),
		LongMemEvalPhasePrepare, "case-1", "")
	if err != nil {
		t.Fatal(err)
	}
	if usage[memory.APIStageSemanticCompile].InputTokens != 120 ||
		usage[memory.APIStageConflictAdjudication].OutputTokens != 8 {
		t.Fatalf("unexpected stage usage: %+v", usage)
	}
	if _, ok := usage["evidence_ingest"]; !ok {
		t.Fatalf("zero-token local stage was omitted: %+v", usage)
	}
	result := CaseResult{}
	applyLongMemEvalStageUsage(&result, usage)
	if result.InputTokens != 160 || result.OutputTokens != 38 {
		t.Fatalf("unexpected aggregate tokens: %+v", result)
	}
}

func TestLongMemEvalAnswerTokenUsageIsolatedByFingerprint(t *testing.T) {
	options := RunnerOptions{OutputDir: t.TempDir(), LongMemEvalPhase: LongMemEvalPhaseAnswer}
	first := CaseResult{Case: CaseSpec{ID: "case-1"}, InputTokens: 120, OutputTokens: 12}
	second := CaseResult{Case: CaseSpec{ID: "case-1"}, InputTokens: 80,
		CacheReadInputTokens: 40, OutputTokens: 8}
	if err := recordLongMemEvalAnswerUsage(options, first, "fingerprint-a"); err != nil {
		t.Fatal(err)
	}
	if err := recordLongMemEvalAnswerUsage(options, second, "fingerprint-b"); err != nil {
		t.Fatal(err)
	}
	usage, err := loadLongMemEvalStageUsage(longMemEvalTokenUsagePath(options),
		LongMemEvalPhaseAnswer, "case-1", "fingerprint-b")
	if err != nil {
		t.Fatal(err)
	}
	answer := usage["answer"]
	if answer.Calls != 1 || answer.InputTokens != 80 || answer.CacheReadInputTokens != 40 ||
		answer.OutputTokens != 8 {
		t.Fatalf("fingerprint usage was contaminated: %+v", answer)
	}
}
