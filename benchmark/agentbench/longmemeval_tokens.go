package agentbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"LuminaCode/memory"
)

const longMemEvalTokenUsageFilename = "token-usage.jsonl"

type longMemEvalTokenUsageRecord struct {
	Phase                    string `json:"phase"`
	CaseID                   string `json:"case_id"`
	Fingerprint              string `json:"fingerprint,omitempty"`
	Stage                    string `json:"stage"`
	Calls                    int    `json:"calls"`
	InputTokens              int    `json:"input_tokens"`
	CacheReadInputTokens     int    `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int    `json:"cache_creation_input_tokens"`
	OutputTokens             int    `json:"output_tokens"`
	Model                    string `json:"model,omitempty"`
	Error                    string `json:"error,omitempty"`
	RecordedAt               string `json:"recorded_at"`
}

var longMemEvalTokenUsageMu sync.Mutex

func longMemEvalTokenUsagePath(options RunnerOptions) string {
	return filepath.Join(options.OutputDir, longMemEvalTokenUsageFilename)
}

func longMemEvalUsageObserver(options RunnerOptions, caseID string) memory.APIUsageObserver {
	path := longMemEvalTokenUsagePath(options)
	return func(_ context.Context, event memory.APIUsageEvent) error {
		recordedAt := event.RecordedAt
		if recordedAt.IsZero() {
			recordedAt = time.Now().UTC()
		}
		return appendLongMemEvalTokenUsage(path, longMemEvalTokenUsageRecord{
			Phase: options.LongMemEvalPhase, CaseID: caseID, Stage: event.Stage,
			Calls: event.Usage.Calls, InputTokens: event.Usage.InputTokens,
			CacheReadInputTokens:     event.Usage.CacheReadInputTokens,
			CacheCreationInputTokens: event.Usage.CacheCreationInputTokens,
			OutputTokens:             event.Usage.OutputTokens, Model: event.Usage.Model,
			Error: event.Error, RecordedAt: recordedAt.UTC().Format(time.RFC3339Nano),
		})
	}
}

func appendLongMemEvalTokenUsage(path string, record longMemEvalTokenUsageRecord) error {
	longMemEvalTokenUsageMu.Lock()
	defer longMemEvalTokenUsageMu.Unlock()
	return appendJSONLine(path, record)
}

func recordLongMemEvalAnswerUsage(options RunnerOptions, result CaseResult, fingerprint string) error {
	calls := 1
	for _, event := range result.Timeline {
		if event.Name != "memory_qa_response" {
			continue
		}
		switch value := event.Metadata["api_calls"].(type) {
		case int:
			calls = value
		case float64:
			calls = int(value)
		}
	}
	if result.InputTokens == 0 && result.OutputTokens == 0 && strings.HasPrefix(result.ErrorType, "answer_memory_") {
		calls = 0
	}
	return appendLongMemEvalTokenUsage(longMemEvalTokenUsagePath(options), longMemEvalTokenUsageRecord{
		Phase: LongMemEvalPhaseAnswer, CaseID: result.Case.ID, Fingerprint: fingerprint,
		Stage: "answer", Calls: calls,
		InputTokens: result.InputTokens, CacheReadInputTokens: result.CacheReadInputTokens,
		CacheCreationInputTokens: result.CacheCreationInputTokens,
		OutputTokens:             result.OutputTokens, Model: options.Config.APIModel,
		Error: result.ErrorType, RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func recordLongMemEvalEvaluatorUsage(options RunnerOptions, usages []longMemEvalEvaluatorUsage) {
	for _, usage := range usages {
		_ = appendLongMemEvalTokenUsage(longMemEvalTokenUsagePath(options), longMemEvalTokenUsageRecord{
			Phase: LongMemEvalPhaseEvaluate, CaseID: usage.CaseID, Stage: "evaluate", Calls: 1,
			InputTokens: usage.InputTokens, CacheReadInputTokens: usage.CacheReadInputTokens,
			OutputTokens: usage.OutputTokens, Model: usage.Model,
			RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
	}
}

func aggregateLongMemEvalEvaluatorUsage(usages []longMemEvalEvaluatorUsage) map[string]StageTokenUsage {
	total := defaultLongMemEvalStageUsage(LongMemEvalPhaseEvaluate)
	for _, usage := range usages {
		mergeLongMemEvalStageUsage(total, "evaluate", StageTokenUsage{Calls: 1,
			InputTokens: usage.InputTokens, CacheReadInputTokens: usage.CacheReadInputTokens,
			OutputTokens: usage.OutputTokens, Models: nonEmptyStrings(usage.Model)})
	}
	return total
}

func loadLongMemEvalStageUsage(path, phase, caseID, fingerprint string) (map[string]StageTokenUsage, error) {
	usage := defaultLongMemEvalStageUsage(phase)
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return usage, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, 2*1024*1024)
	for scanner.Scan() {
		var record longMemEvalTokenUsageRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, err
		}
		if record.Phase != phase || record.CaseID != caseID ||
			(fingerprint != "" && record.Fingerprint != fingerprint) {
			continue
		}
		mergeLongMemEvalStageUsage(usage, record.Stage, StageTokenUsage{
			Calls: record.Calls, InputTokens: record.InputTokens,
			CacheReadInputTokens:     record.CacheReadInputTokens,
			CacheCreationInputTokens: record.CacheCreationInputTokens,
			OutputTokens:             record.OutputTokens,
			Models:                   nonEmptyStrings(record.Model),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return usage, nil
}

func defaultLongMemEvalStageUsage(phase string) map[string]StageTokenUsage {
	usage := map[string]StageTokenUsage{}
	switch phase {
	case LongMemEvalPhasePrepare:
		for _, stage := range []string{"evidence_ingest", memory.APIStageSemanticCompile,
			memory.APIStageConflictAdjudication, "local_embedding"} {
			usage[stage] = StageTokenUsage{}
		}
	case LongMemEvalPhaseAnswer:
		usage["local_retrieval"] = StageTokenUsage{}
		usage["answer"] = StageTokenUsage{}
	case LongMemEvalPhaseEvaluate:
		usage["evaluate"] = StageTokenUsage{}
	}
	return usage
}

func mergeLongMemEvalStageUsage(target map[string]StageTokenUsage, stage string, addition StageTokenUsage) {
	stage = strings.TrimSpace(stage)
	if stage == "" {
		return
	}
	current := target[stage]
	current.Calls += addition.Calls
	current.InputTokens += addition.InputTokens
	current.CacheReadInputTokens += addition.CacheReadInputTokens
	current.CacheCreationInputTokens += addition.CacheCreationInputTokens
	current.OutputTokens += addition.OutputTokens
	current.Models = uniqueSortedStrings(append(current.Models, addition.Models...))
	target[stage] = current
}

func aggregateLongMemEvalStageUsage(results []CaseResult, phase string) map[string]StageTokenUsage {
	total := defaultLongMemEvalStageUsage(phase)
	for _, result := range results {
		for stage, usage := range result.StageTokenUsage {
			mergeLongMemEvalStageUsage(total, stage, usage)
		}
	}
	return total
}

func applyLongMemEvalStageUsage(result *CaseResult, usage map[string]StageTokenUsage) {
	result.StageTokenUsage = usage
	result.InputTokens = 0
	result.CacheReadInputTokens = 0
	result.CacheCreationInputTokens = 0
	result.OutputTokens = 0
	for _, stage := range usage {
		result.InputTokens += stage.InputTokens + stage.CacheReadInputTokens + stage.CacheCreationInputTokens
		result.CacheReadInputTokens += stage.CacheReadInputTokens
		result.CacheCreationInputTokens += stage.CacheCreationInputTokens
		result.OutputTokens += stage.OutputTokens
	}
}

func nonEmptyStrings(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
