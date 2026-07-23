package agentbench

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type longMemEvalCase struct {
	QuestionID         string             `json:"question_id"`
	Question           string             `json:"question"`
	Answer             any                `json:"answer"`
	QuestionType       string             `json:"question_type"`
	QuestionDate       string             `json:"question_date"`
	AnswerSessionIDs   []string           `json:"answer_session_ids"`
	HaystackSessionIDs []string           `json:"haystack_session_ids"`
	HaystackDates      []string           `json:"haystack_dates"`
	HaystackSessions   [][]map[string]any `json:"haystack_sessions"`
}

type longMemEvalJob struct {
	Index int
	Case  longMemEvalCase
	ID    string
}

type memoryCaseOutput struct {
	Index  int
	Result CaseResult
}

func isMemoryBenchmarkSuite(suite string) bool {
	return suite == SuiteLongMemEval
}

func RunMemoryBenchmarkSuite(ctx context.Context, options RunnerOptions) (Report, error) {
	options = normalizeOptions(options)
	if options.Suite != SuiteLongMemEval {
		return Report{}, fmt.Errorf("unsupported memory benchmark suite %q", options.Suite)
	}
	options = normalizeLongMemEvalOptions(options)
	if err := validateLongMemEvalPhaseOptions(options); err != nil {
		return Report{}, err
	}
	for _, dir := range []string{options.OutputDir, options.WorkDir, options.ArtifactsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Report{}, err
		}
	}
	return runLongMemEvalPhasedSuite(ctx, options)
}

func completedLongMemEvalCheckpoint(result CaseResult) bool {
	return strings.TrimSpace(result.Case.ID) != "" && strings.TrimSpace(result.ErrorType) == "" &&
		strings.TrimSpace(result.Hypothesis) != ""
}

func normalizedCaseParallel(options RunnerOptions) int {
	if options.CaseParallel <= 1 {
		return 1
	}
	return options.CaseParallel
}

func compactMemoryResults(results []CaseResult) []CaseResult {
	compact := make([]CaseResult, 0, len(results))
	for _, result := range results {
		if strings.TrimSpace(result.Case.ID) != "" {
			compact = append(compact, result)
		}
	}
	return compact
}

func loadMemoryCheckpoint(path string) (map[string]CaseResult, error) {
	results := map[string]CaseResult{}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return results, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1024*1024), 32*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var result CaseResult
		if err := json.Unmarshal([]byte(line), &result); err != nil {
			return nil, fmt.Errorf("load LongMemEval checkpoint %s: %w", path, err)
		}
		if id := strings.TrimSpace(result.Case.ID); id != "" {
			results[id] = result
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func appendMemoryCheckpoint(path string, result CaseResult) error {
	if !completedLongMemEvalCheckpoint(result) {
		return fmt.Errorf("refusing to checkpoint incomplete LongMemEval result %q", result.Case.ID)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	data, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

func validateLongMemEvalPredictions(results []CaseResult) error {
	if len(results) == 0 {
		return fmt.Errorf("longmemeval prediction invalid: no results")
	}
	seen := make(map[string]struct{}, len(results))
	var missingID, duplicateID, emptyAnswer, errorResult int
	var examples []string
	for _, result := range results {
		id := strings.TrimSpace(result.Case.ID)
		if id == "" {
			missingID++
			continue
		}
		if _, ok := seen[id]; ok {
			duplicateID++
			if len(examples) < 8 {
				examples = append(examples, id+": duplicate")
			}
			continue
		}
		seen[id] = struct{}{}
		if errType := strings.TrimSpace(result.ErrorType); errType != "" {
			errorResult++
			if len(examples) < 8 {
				examples = append(examples, id+": "+errType)
			}
		}
		if strings.TrimSpace(result.Hypothesis) == "" {
			emptyAnswer++
			if len(examples) < 8 {
				examples = append(examples, id+": empty_answer")
			}
		}
	}
	if missingID == 0 && duplicateID == 0 && emptyAnswer == 0 && errorResult == 0 {
		return nil
	}
	details := []string{
		fmt.Sprintf("results=%d", len(results)),
		fmt.Sprintf("unique=%d", len(seen)),
		fmt.Sprintf("missing_id=%d", missingID),
		fmt.Sprintf("duplicate_id=%d", duplicateID),
		fmt.Sprintf("empty_answer=%d", emptyAnswer),
		fmt.Sprintf("error_result=%d", errorResult),
	}
	if len(examples) > 0 {
		details = append(details, "examples="+strings.Join(examples, ", "))
	}
	return fmt.Errorf("longmemeval prediction invalid: %s", strings.Join(details, "; "))
}

func reportMemoryBenchmarkProgress(suite string, index, total int, id string) {
	fmt.Fprintf(os.Stderr, "[%s] case %d/%d %s\n", suite, index, total, id)
}

func contextWithOptionalTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func failMemoryCase(result CaseResult, artifactDir string, start time.Time, timeline []TimelineEvent, errType string) CaseResult {
	result.ErrorType = errType
	result.DurationSeconds = time.Since(start).Seconds()
	result.Timeline = timeline
	writeCaseArtifacts(artifactDir, result.Case, AgentRunResult{}, result)
	return result
}

func parseLongMemEvalDate(value string) time.Time {
	for _, layout := range []string{"2006/01/02 (Mon) 15:04", "2006/01/02 15:04", time.RFC3339} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func longMemEvalRetrievalReferenceTime(value time.Time) time.Time {
	if value.IsZero() {
		return value
	}
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 23, 59, 59, int(time.Second-time.Nanosecond), time.UTC)
}

func filterLongMemEvalCases(cases []longMemEvalCase, id string) []longMemEvalCase {
	filtered := make([]longMemEvalCase, 0, 1)
	for _, current := range cases {
		if current.QuestionID == id {
			filtered = append(filtered, current)
		}
	}
	return filtered
}

func answerContainsExpected(hypothesis string, expected any) bool {
	h := normalizeAnswer(hypothesis)
	if h == "" || (isUncertainAnswer(h) && !isAbstentionExpected(expected)) {
		return false
	}
	switch value := expected.(type) {
	case string:
		return looseStringMatch(h, value)
	case []any:
		if len(value) == 0 {
			return false
		}
		matches := 0
		for _, item := range value {
			if answerContainsExpected(hypothesis, item) {
				matches++
			}
		}
		return float64(matches)/float64(len(value)) >= 0.6
	default:
		normalized := normalizeAnswer(stringifyAny(value))
		return normalized != "" && len([]rune(normalized)) < 300 && strings.Contains(h, normalized)
	}
}

func isUncertainAnswer(value string) bool {
	for _, marker := range []string{"i don t know", "do not know", "cannot determine", "can t determine",
		"not enough information", "no specific", "unable to determine"} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}

func isAbstentionExpected(expected any) bool {
	text := normalizeAnswer(stringifyAny(expected))
	for _, marker := range []string{"i don t know", "do not know", "not enough information", "unknown", "cannot determine"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

var answerPunctuation = regexp.MustCompile(`[^a-z0-9一-龥]+`)
var answerOrdinalSuffix = regexp.MustCompile(`\b([0-9]+)(?:st|nd|rd|th)\b`)

var answerStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "be": true, "is": true,
	"of": true, "on": true, "or": true, "the": true, "to": true, "was": true, "were": true,
	"with": true, "correct": true, "answer": true, "exactly": true, "select": true, "all": true,
	"that": true, "this": true,
}

func normalizeAnswer(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = answerOrdinalSuffix.ReplaceAllString(value, "$1")
	return strings.Join(strings.Fields(answerPunctuation.ReplaceAllString(value, " ")), " ")
}

func looseStringMatch(hypothesis, expected string) bool {
	expected = normalizeAnswer(expected)
	if expected == "" {
		return false
	}
	if strings.Contains(" "+hypothesis+" ", " "+expected+" ") {
		return true
	}
	tokens := meaningfulTokens(expected)
	if len(tokens) == 0 {
		return false
	}
	hypothesisTokens := map[string]struct{}{}
	for _, token := range strings.Fields(hypothesis) {
		hypothesisTokens[token] = struct{}{}
	}
	matches := 0
	for _, token := range tokens {
		if _, ok := hypothesisTokens[token]; ok {
			matches++
		}
	}
	if len(tokens) <= 2 {
		return matches == len(tokens)
	}
	return float64(matches)/float64(len(tokens)) >= 0.6
}

func meaningfulTokens(value string) []string {
	var result []string
	for _, token := range strings.Fields(value) {
		if answerStopwords[token] || (len([]rune(token)) <= 1 && !containsASCIIDigit(token)) {
			continue
		}
		result = append(result, token)
	}
	return result
}

func containsASCIIDigit(value string) bool {
	for _, current := range value {
		if current >= '0' && current <= '9' {
			return true
		}
	}
	return false
}

func extractJSONObject(text string) string {
	text = strings.TrimSpace(text)
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start >= 0 && end >= start {
		return text[start : end+1]
	}
	return text
}

func stringifyAny(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	data, _ := json.Marshal(value)
	return string(data)
}

func stringAt(values []string, index int, fallback string) string {
	if index >= 0 && index < len(values) && strings.TrimSpace(values[index]) != "" {
		return values[index]
	}
	return fallback
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
