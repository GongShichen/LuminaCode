package agentbench

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	longMemEvalSmokeOverallThreshold = 0.75
	longMemEvalSmokeTypeThreshold    = 0.50
	longMemEvalSmokeCoreThreshold    = 0.60
)

type longMemEvalSmokeSelection struct {
	DatasetSHA256 string                          `json:"dataset_sha256"`
	Strategy      string                          `json:"strategy"`
	Size          int                             `json:"size"`
	TypeCounts    map[string]int                  `json:"type_counts"`
	Cases         []longMemEvalSmokeSelectionCase `json:"cases"`
	CreatedAt     string                          `json:"created_at"`
}

type longMemEvalSmokeSelectionCase struct {
	QuestionID   string `json:"question_id"`
	QuestionType string `json:"question_type"`
}

type longMemEvalEvaluatorMetrics struct {
	Accuracy float64                            `json:"accuracy"`
	ByType   map[string]longMemEvalTypeAccuracy `json:"by_type"`
}

type longMemEvalTypeAccuracy struct {
	Accuracy float64 `json:"accuracy"`
	Count    int     `json:"count"`
}

type longMemEvalSmokeGate struct {
	Passed               bool                        `json:"passed"`
	EvaluatorStatus      string                      `json:"evaluator_status"`
	OverallThreshold     float64                     `json:"overall_threshold"`
	TypeThreshold        float64                     `json:"type_threshold"`
	CoreTypeThreshold    float64                     `json:"core_type_threshold"`
	Metrics              longMemEvalEvaluatorMetrics `json:"metrics"`
	Failures             []string                    `json:"failures,omitempty"`
	FullBenchmarkAllowed bool                        `json:"full_benchmark_allowed"`
	AssessedAt           string                      `json:"assessed_at"`
}

func writeLongMemEvalSmokeSelection(options RunnerOptions, datasetSHA string,
	cases []longMemEvalCase) (string, error) {
	if options.LongMemEvalSmokeSize <= 0 {
		return "", nil
	}
	selection := longMemEvalSmokeSelection{DatasetSHA256: datasetSHA,
		Strategy: "balanced-question-type-stable-hash", Size: len(cases),
		TypeCounts: map[string]int{}, CreatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	for _, item := range cases {
		questionType := strings.TrimSpace(item.QuestionType)
		if questionType == "" {
			questionType = "unknown"
		}
		selection.TypeCounts[questionType]++
		selection.Cases = append(selection.Cases, longMemEvalSmokeSelectionCase{
			QuestionID: strings.TrimSpace(item.QuestionID), QuestionType: questionType})
	}
	path := filepath.Join(options.OutputDir, "longmemeval-smoke-selection.json")
	if err := writeJSONAtomic(path, selection, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

var longMemEvalEvaluatorAccuracyPattern = regexp.MustCompile(`^Accuracy:\s*([0-9]+(?:\.[0-9]+)?)\s*$`)
var longMemEvalEvaluatorTypePattern = regexp.MustCompile(`^\s*([^:]+):\s*([0-9]+(?:\.[0-9]+)?)\s*\(([0-9]+)\)\s*$`)

func parseLongMemEvalEvaluatorMetrics(output string) (longMemEvalEvaluatorMetrics, error) {
	metrics := longMemEvalEvaluatorMetrics{ByType: map[string]longMemEvalTypeAccuracy{}}
	found := false
	collecting := false
	for _, line := range strings.Split(output, "\n") {
		if match := longMemEvalEvaluatorAccuracyPattern.FindStringSubmatch(strings.TrimSpace(line)); len(match) == 2 {
			accuracy, err := strconv.ParseFloat(match[1], 64)
			if err != nil {
				return metrics, err
			}
			metrics = longMemEvalEvaluatorMetrics{Accuracy: accuracy,
				ByType: map[string]longMemEvalTypeAccuracy{}}
			found = true
			collecting = true
			continue
		}
		if !collecting {
			continue
		}
		match := longMemEvalEvaluatorTypePattern.FindStringSubmatch(line)
		if len(match) != 4 {
			continue
		}
		accuracy, err := strconv.ParseFloat(match[2], 64)
		if err != nil {
			return metrics, err
		}
		count, err := strconv.Atoi(match[3])
		if err != nil {
			return metrics, err
		}
		metrics.ByType[strings.TrimSpace(match[1])] = longMemEvalTypeAccuracy{Accuracy: accuracy, Count: count}
	}
	if !found {
		return metrics, fmt.Errorf("official evaluator output did not contain Accuracy")
	}
	return metrics, nil
}

func assessLongMemEvalSmoke(status, output string, selected []longMemEvalCase) longMemEvalSmokeGate {
	gate := longMemEvalSmokeGate{EvaluatorStatus: status,
		OverallThreshold:  longMemEvalSmokeOverallThreshold,
		TypeThreshold:     longMemEvalSmokeTypeThreshold,
		CoreTypeThreshold: longMemEvalSmokeCoreThreshold,
		AssessedAt:        time.Now().UTC().Format(time.RFC3339Nano)}
	if status != "completed" && status != "completed_fallback" {
		gate.Failures = append(gate.Failures, "official evaluator did not complete")
	}
	metrics, err := parseLongMemEvalEvaluatorMetrics(output)
	if err != nil {
		gate.Failures = append(gate.Failures, err.Error())
		return gate
	}
	gate.Metrics = metrics
	if metrics.Accuracy < longMemEvalSmokeOverallThreshold {
		gate.Failures = append(gate.Failures, fmt.Sprintf("overall accuracy %.4f is below %.2f",
			metrics.Accuracy, longMemEvalSmokeOverallThreshold))
	}
	expected := map[string]int{}
	for _, item := range selected {
		expected[strings.TrimSpace(item.QuestionType)]++
	}
	types := make([]string, 0, len(expected))
	for questionType := range expected {
		types = append(types, questionType)
	}
	sort.Strings(types)
	for _, questionType := range types {
		metric, ok := metrics.ByType[questionType]
		if !ok || metric.Count != expected[questionType] {
			gate.Failures = append(gate.Failures, fmt.Sprintf("%s evaluated %d/%d cases",
				questionType, metric.Count, expected[questionType]))
			continue
		}
		threshold := longMemEvalSmokeTypeThreshold
		if questionType == "multi-session" || questionType == "temporal-reasoning" {
			threshold = longMemEvalSmokeCoreThreshold
		}
		if metric.Accuracy < threshold {
			gate.Failures = append(gate.Failures, fmt.Sprintf("%s accuracy %.4f is below %.2f",
				questionType, metric.Accuracy, threshold))
		}
	}
	gate.Passed = len(gate.Failures) == 0
	gate.FullBenchmarkAllowed = gate.Passed
	return gate
}
