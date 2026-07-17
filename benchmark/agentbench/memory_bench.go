package agentbench

import (
	"bufio"
	"context"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"LuminaCode/agent"
	luminaapi "LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/longmemory"
	coretools "LuminaCode/tools"
)

type longMemEvalCase struct {
	QuestionID         string             `json:"question_id"`
	Question           string             `json:"question"`
	Answer             any                `json:"answer"`
	QuestionDate       string             `json:"question_date"`
	HaystackSessionIDs []string           `json:"haystack_session_ids"`
	HaystackDates      []string           `json:"haystack_dates"`
	HaystackSessions   [][]map[string]any `json:"haystack_sessions"`
}

type memoryArenaCase struct {
	ID          any      `json:"id"`
	Questions   []string `json:"questions"`
	Answers     []any    `json:"answers"`
	Backgrounds any      `json:"backgrounds"`
	BasePerson  any      `json:"base_person"`
}

type memoryMetricInput struct {
	Suite                 string
	SubtaskTotal          int
	SubtaskAnswered       int
	SubtaskMemorySessions []string
	GoldChunkIDs          []string
	GoldMessageTexts      map[string]string
	GoldSourceSessions    []string
}

type longMemEvalSeedEvidence struct {
	ChunkIDs       []string
	MessageTexts   map[string]string
	SourceSessions []string
}

type longMemEvalJob struct {
	Index int
	Case  longMemEvalCase
	ID    string
}

type memoryArenaJob struct {
	Index int
	Case  memoryArenaCase
	ID    string
}

type memoryCaseOutput struct {
	Index  int
	Result CaseResult
}

func isMemoryBenchmarkSuite(suite string) bool {
	return suite == SuiteLongMemEval || suite == SuiteMemoryArena
}

func RunMemoryBenchmarkSuite(ctx context.Context, options RunnerOptions) (Report, error) {
	options = normalizeOptions(options)
	if err := os.MkdirAll(options.OutputDir, 0o755); err != nil {
		return Report{}, err
	}
	if err := os.MkdirAll(options.WorkDir, 0o755); err != nil {
		return Report{}, err
	}
	if err := os.MkdirAll(options.ArtifactsDir, 0o755); err != nil {
		return Report{}, err
	}
	var results []CaseResult
	var datasetPath string
	var err error
	switch options.Suite {
	case SuiteLongMemEval:
		datasetPath, results, err = runLongMemEvalSuite(ctx, options)
	case SuiteMemoryArena:
		datasetPath, results, err = runMemoryArenaSuite(ctx, options)
	default:
		err = fmt.Errorf("unknown memory benchmark suite %q", options.Suite)
	}
	if err != nil {
		return Report{}, err
	}
	now := options.Now()
	report := Report{
		Suite:        options.Suite,
		GeneratedAt:  now.Format(time.RFC3339),
		DebugRun:     options.Limit > 0 || strings.TrimSpace(options.CaseID) != "",
		RootDir:      options.RootDir,
		OutputDir:    options.OutputDir,
		WorkDir:      options.WorkDir,
		BenchmarkDir: filepath.Dir(datasetPath),
		Model:        options.Config.APIModel,
		Summary:      BuildSummary(results),
		Results:      results,
		OfficialMetrics: map[string]any{
			"dataset_path":      datasetPath,
			"checkpoint_path":   memoryCheckpointPath(options, datasetPath),
			"case_parallel":     normalizedCaseParallel(options),
			"case_resume":       !options.NoResume,
			"answer_match_note": "answer_match is a lightweight exact/substring diagnostic; use upstream evaluation scripts for official scores when available.",
		},
	}
	if options.Suite == SuiteLongMemEval {
		predictionPath, predictionErr := writeLongMemEvalPredictions(options.OutputDir, now, results)
		report.OfficialMetrics["prediction_path"] = predictionPath
		if predictionErr != nil {
			report.OfficialMetrics["upstream_evaluator_status"] = "prediction_write_failed"
			report.OfficialMetrics["upstream_evaluator_error"] = predictionErr.Error()
		} else {
			status, output := runLongMemEvalEvaluator(ctx, filepath.Dir(filepath.Dir(datasetPath)), predictionPath, datasetPath, options.Config)
			report.OfficialMetrics["upstream_evaluator_status"] = status
			if output != "" {
				report.OfficialMetrics["upstream_evaluator_output"] = output
			}
		}
	}
	if options.Suite == SuiteMemoryArena && strings.Contains(filepath.ToSlash(datasetPath), "/bundled_shopping/") {
		report.OfficialMetrics["environment_status"] = "proxy_invalid_environment"
		report.OfficialMetrics["score_included"] = false
		report.OfficialMetrics["reason"] = "the upstream WebShop environment is unavailable; sequential text answers are not an official MemoryArena environment loop"
	}
	return report, nil
}

func writeLongMemEvalPredictions(outputDir string, generatedAt time.Time, results []CaseResult) (string, error) {
	dir := filepath.Join(outputDir, "predictions")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, "longmemeval-"+generatedAt.Format("20060102-150405")+".jsonl")
	file, err := os.Create(path)
	if err != nil {
		return path, err
	}
	defer file.Close()
	encoder := json.NewEncoder(file)
	for _, result := range results {
		if result.Case.ID == "" {
			continue
		}
		if err := encoder.Encode(map[string]any{"question_id": result.Case.ID, "hypothesis": result.Hypothesis}); err != nil {
			return path, err
		}
	}
	return path, file.Sync()
}

func runLongMemEvalEvaluator(ctx context.Context, benchmarkDir, predictionPath, datasetPath string, cfg config.Config) (string, string) {
	metricModel := strings.TrimSpace(os.Getenv("LONGMEMEVAL_EVAL_MODEL"))
	if metricModel == "" {
		metricModel = "deepseek-v4-pro"
	}
	apiKey := strings.TrimSpace(os.Getenv("DEEPSEEK_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(cfg.FallbackAPIKey)
	}
	if apiKey == "" {
		return "not_run_missing_deepseek_api_key", ""
	}
	baseURL := strings.TrimSpace(os.Getenv("DEEPSEEK_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(cfg.FallbackAPIBaseURL)
	}
	baseURL = strings.TrimSuffix(strings.TrimSuffix(baseURL, "/"), "/anthropic")
	if baseURL == "" {
		baseURL = "https://api.deepseek.com"
	}
	evaluator := filepath.Join(benchmarkDir, "src", "evaluation", "evaluate_qa.py")
	if _, err := os.Stat(evaluator); err != nil {
		return "not_run_evaluator_missing", err.Error()
	}
	python := filepath.Join(filepath.Dir(benchmarkDir), ".venv", "bin", "python")
	if _, err := os.Stat(python); err != nil {
		return "not_run_evaluator_environment_missing", err.Error()
	}
	command := exec.CommandContext(ctx, python, evaluator, metricModel, predictionPath, datasetPath)
	command.Dir = benchmarkDir
	command.Env = append(os.Environ(), "PYTHONPATH="+benchmarkDir, "DEEPSEEK_API_KEY="+apiKey,
		"DEEPSEEK_BASE_URL="+baseURL)
	output, err := command.CombinedOutput()
	if err != nil {
		return "failed", strings.TrimSpace(string(output) + "\n" + err.Error())
	}
	return "completed", strings.TrimSpace(string(output))
}

func runLongMemEvalSuite(ctx context.Context, options RunnerOptions) (string, []CaseResult, error) {
	path := options.CasesPath
	if path == "" {
		path = filepath.Join(options.RootDir, "longmemeval", "repo", "data", "longmemeval_oracle.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return path, nil, err
	}
	var cases []longMemEvalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		return path, nil, err
	}
	if options.CaseID != "" {
		cases = filterLongMemEvalCases(cases, options.CaseID)
	}
	if options.Limit > 0 && options.Limit < len(cases) {
		cases = cases[:options.Limit]
	}
	checkpointPath := memoryCheckpointPath(options, path)
	checkpointed := map[string]CaseResult{}
	if !options.NoResume {
		var err error
		checkpointed, err = loadMemoryCheckpoint(checkpointPath)
		if err != nil {
			return path, nil, err
		}
	}
	results := make([]CaseResult, len(cases))
	var pending []longMemEvalJob
	for i, c := range cases {
		id := firstNonEmptyString(c.QuestionID, "longmemeval-case")
		if result, ok := checkpointed[id]; ok && completedLongMemEvalCheckpoint(result) {
			reportMemoryBenchmarkProgress(options.Suite, i+1, len(cases), id+" checkpoint")
			results[i] = result
			continue
		}
		pending = append(pending, longMemEvalJob{Index: i, Case: c, ID: id})
	}
	if len(pending) == 0 {
		return path, compactMemoryResults(results), nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan longMemEvalJob)
	outputs := make(chan memoryCaseOutput)
	workers := minInt(normalizedCaseParallel(options), len(pending))
	for worker := 0; worker < workers; worker++ {
		go func() {
			for job := range jobs {
				if runCtx.Err() != nil {
					return
				}
				reportMemoryBenchmarkProgress(options.Suite, job.Index+1, len(cases), job.ID)
				result := runLongMemEvalCase(runCtx, job.Case, options)
				select {
				case outputs <- memoryCaseOutput{Index: job.Index, Result: result}:
				case <-runCtx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, job := range pending {
			select {
			case jobs <- job:
			case <-runCtx.Done():
				return
			}
		}
	}()
	for completed := 0; completed < len(pending); completed++ {
		select {
		case output := <-outputs:
			result := output.Result
			results[output.Index] = result
			if err := appendMemoryCheckpoint(checkpointPath, result); err != nil {
				cancel()
				return path, compactMemoryResults(results), err
			}
		case <-runCtx.Done():
			return path, compactMemoryResults(results), runCtx.Err()
		}
	}
	return path, compactMemoryResults(results), nil
}

func completedLongMemEvalCheckpoint(result CaseResult) bool {
	return result.Case.ID != "" && result.ErrorType == "" && strings.TrimSpace(result.Hypothesis) != ""
}

func runMemoryArenaSuite(ctx context.Context, options RunnerOptions) (string, []CaseResult, error) {
	domain := "group_travel_planner"
	if options.CasesPath != "" {
		domain = inferMemoryArenaDomain(options.CasesPath, domain)
	}
	if strings.TrimSpace(options.CaseID) != "" && strings.Contains(options.CaseID, "/") {
		parts := strings.SplitN(options.CaseID, "/", 2)
		domain = parts[0]
		options.CaseID = parts[1]
	}
	path := options.CasesPath
	if path == "" {
		path = filepath.Join(options.RootDir, "memoryarena", domain, "data.jsonl")
	}
	cases, err := loadMemoryArenaCases(path)
	if err != nil {
		return path, nil, err
	}
	if options.CaseID != "" {
		cases = filterMemoryArenaCases(cases, options.CaseID)
	}
	if options.Limit > 0 && options.Limit < len(cases) {
		cases = cases[:options.Limit]
	}
	checkpointPath := memoryCheckpointPath(options, path)
	checkpointed := map[string]CaseResult{}
	if !options.NoResume {
		var err error
		checkpointed, err = loadMemoryCheckpoint(checkpointPath)
		if err != nil {
			return path, nil, err
		}
	}
	results := make([]CaseResult, len(cases))
	var pending []memoryArenaJob
	for i, c := range cases {
		id := domain + "-" + sanitizeCaseID(fmt.Sprintf("%v", c.ID))
		if result, ok := checkpointed[id]; ok {
			reportMemoryBenchmarkProgress(options.Suite, i+1, len(cases), id+" checkpoint")
			results[i] = result
			continue
		}
		pending = append(pending, memoryArenaJob{Index: i, Case: c, ID: fmt.Sprintf("%v", c.ID)})
	}
	if len(pending) == 0 {
		return path, compactMemoryResults(results), nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan memoryArenaJob)
	outputs := make(chan memoryCaseOutput)
	workers := minInt(normalizedCaseParallel(options), len(pending))
	for worker := 0; worker < workers; worker++ {
		go func() {
			for job := range jobs {
				if runCtx.Err() != nil {
					return
				}
				reportMemoryBenchmarkProgress(options.Suite, job.Index+1, len(cases), job.ID)
				result := runMemoryArenaCase(runCtx, domain, job.Case, options)
				select {
				case outputs <- memoryCaseOutput{Index: job.Index, Result: result}:
				case <-runCtx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, job := range pending {
			select {
			case jobs <- job:
			case <-runCtx.Done():
				return
			}
		}
	}()
	for completed := 0; completed < len(pending); completed++ {
		select {
		case output := <-outputs:
			result := output.Result
			results[output.Index] = result
			if err := appendMemoryCheckpoint(checkpointPath, result); err != nil {
				cancel()
				return path, compactMemoryResults(results), err
			}
		case <-runCtx.Done():
			return path, compactMemoryResults(results), runCtx.Err()
		}
	}
	return path, compactMemoryResults(results), nil
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
		if result.Case.ID == "" {
			continue
		}
		compact = append(compact, result)
	}
	return compact
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func memoryCheckpointPath(options RunnerOptions, datasetPath string) string {
	keyPath, err := filepath.Abs(datasetPath)
	if err != nil {
		keyPath = datasetPath
	}
	key := fmt.Sprintf("%s\n%s\ncase=%s\nlimit=%d\ntimeout=%d\nmodel=%s", options.Suite, keyPath, strings.TrimSpace(options.CaseID), options.Limit, options.TimeoutSeconds, options.Config.APIModel)
	sum := sha1.Sum([]byte(key))
	name := options.Suite + "-" + hex.EncodeToString(sum[:])[:12] + ".jsonl"
	return filepath.Join(options.OutputDir, "checkpoints", name)
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
			return nil, fmt.Errorf("load memory checkpoint %s: %w", path, err)
		}
		if result.Case.ID != "" {
			results[result.Case.ID] = result
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return results, nil
}

func appendMemoryCheckpoint(path string, result CaseResult) error {
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

func runLongMemEvalCase(ctx context.Context, c longMemEvalCase, options RunnerOptions) CaseResult {
	id := firstNonEmptyString(c.QuestionID, "longmemeval-case")
	spec := CaseSpec{ID: id, Benchmark: SuiteLongMemEval, Prompt: c.Question, TimeoutSeconds: options.TimeoutSeconds}
	result, caseCtx, caseDir, artifactDir, start, timeline, cancel := prepareMemoryCase(ctx, spec, options)
	defer cancel()
	if result.ErrorType != "" {
		return result
	}
	storePath := filepath.Join(caseDir, ".lumina", "memory", "lumina-memory.sqlite")
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(caseDir)}
	cfg := memoryCaseConfig(options.Config, caseDir, storePath)
	err := ingestLongMemEvalHistory(caseCtx, storePath, c, cfg)
	if err != nil {
		return failMemoryCase(result, artifactDir, start, timeline, "memory_seed_failed: "+err.Error())
	}
	prompt := c.Question
	timeline = append(timeline, newTimelineEvent(start, time.Now(), "first_model_request", map[string]any{"memory_store": storePath}))
	agentResult := runAgentAt(caseCtx, options.AgentRunner, cfg, prompt,
		"query-"+sanitizeCaseID(id), parseLongMemEvalDate(c.QuestionDate))
	seedEvidence, goldErr := collectLongMemEvalGold(caseCtx, storePath, scope, c)
	if goldErr != nil {
		timeline = append(timeline, newTimelineEvent(start, time.Now(), "offline_gold_mapping_failed", map[string]any{"error": goldErr.Error()}))
	}
	return finishMemoryCase(caseCtx, result, artifactDir, start, timeline, agentResult, c.Answer, storePath, nil, cfg, memoryMetricInput{
		Suite:              SuiteLongMemEval,
		GoldChunkIDs:       seedEvidence.ChunkIDs,
		GoldMessageTexts:   seedEvidence.MessageTexts,
		GoldSourceSessions: seedEvidence.SourceSessions,
	})
}

func runMemoryArenaCase(ctx context.Context, domain string, c memoryArenaCase, options RunnerOptions) CaseResult {
	id := fmt.Sprintf("%v", c.ID)
	spec := CaseSpec{ID: domain + "-" + sanitizeCaseID(id), Benchmark: SuiteMemoryArena, Prompt: strings.Join(c.Questions, "\n\n"), TimeoutSeconds: options.TimeoutSeconds}
	result, caseCtx, caseDir, artifactDir, start, timeline, cancel := prepareMemoryCase(ctx, spec, options)
	defer cancel()
	if result.ErrorType != "" {
		return result
	}
	storePath := filepath.Join(caseDir, ".lumina", "memory", "lumina-memory.sqlite")
	cfg := memoryCaseConfig(options.Config, caseDir, storePath)
	if err := ingestMemoryArenaBackground(caseCtx, cfg, domain, c); err != nil {
		return failMemoryCase(result, artifactDir, start, timeline, "memory_seed_failed: "+err.Error())
	}
	var combined strings.Builder
	var aggregate AgentRunResult
	answeredSubtasks := 0
	var memorySessions []string
	for i, question := range c.Questions {
		if caseCtx.Err() != nil {
			aggregate.ErrorType = "agent_timeout: " + caseCtx.Err().Error()
			break
		}
		prompt := question
		subTimeout := memoryArenaSubtaskTimeout(options.TimeoutSeconds, len(c.Questions))
		subCtx, subCancel := contextWithOptionalTimeout(caseCtx, subTimeout)
		timeline = append(timeline, newTimelineEvent(start, time.Now(), "memoryarena_subtask_start", map[string]any{
			"subtask":      i + 1,
			"memory_store": storePath,
			"timeout_ms":   subTimeout.Milliseconds(),
		}))
		subResult := runMemoryArenaSubtaskWithRetry(subCtx, options.AgentRunner, cfg, prompt,
			fmt.Sprintf("query-%s-%03d", sanitizeCaseID(id), i+1))
		subCancel()
		mergeAgentRunResult(&aggregate, subResult)
		answer := strings.TrimSpace(subResult.FinalText)
		if answer != "" {
			answeredSubtasks++
			fmt.Fprintf(&combined, "Subtask %d: %s\n\n", i+1, answer)
			extractionSessionID := fmt.Sprintf("history-%s-%03d", sanitizeCaseID(id), i+1)
			if extractionErr := extractMemoryArenaSubtask(caseCtx, cfg, extractionSessionID, question, answer); extractionErr != nil {
				timeline = append(timeline, newTimelineEvent(start, time.Now(), "memoryarena_extraction_failed", map[string]any{
					"subtask": i + 1, "error": extractionErr.Error(),
				}))
			} else {
				memorySessions = append(memorySessions, extractionSessionID)
			}
		}
		if subResult.ErrorType != "" {
			break
		}
	}
	aggregate.FinalText = strings.TrimSpace(combined.String())
	return finishMemoryCase(caseCtx, result, artifactDir, start, timeline, aggregate, c.Answers, storePath, nil, cfg, memoryMetricInput{
		Suite:                 SuiteMemoryArena,
		SubtaskTotal:          len(c.Questions),
		SubtaskAnswered:       answeredSubtasks,
		SubtaskMemorySessions: memorySessions,
	})
}

func reportMemoryBenchmarkProgress(suite string, index, total int, id string) {
	fmt.Fprintf(os.Stderr, "[%s] case %d/%d %s\n", suite, index, total, id)
}

func runAgentWithDeadline(ctx context.Context, runner AgentRunner, cfg config.Config, prompt string, sessionID string) AgentRunResult {
	done := make(chan AgentRunResult, 1)
	go func() {
		done <- runner.Run(ctx, cfg, prompt, sessionID)
	}()
	select {
	case result := <-done:
		return result
	case <-ctx.Done():
		return AgentRunResult{ErrorType: "agent_timeout: " + ctx.Err().Error()}
	}
}

type timestampedAgentRunner interface {
	RunAt(context.Context, config.Config, string, string, time.Time) AgentRunResult
}

func runAgentAt(ctx context.Context, runner AgentRunner, cfg config.Config, prompt, sessionID string, queryTime time.Time) AgentRunResult {
	if timestamped, ok := runner.(timestampedAgentRunner); ok {
		return timestamped.RunAt(ctx, cfg, prompt, sessionID, queryTime)
	}
	return runAgentWithDeadline(ctx, runner, cfg, prompt, sessionID)
}

func runMemoryArenaSubtaskWithRetry(ctx context.Context, runner AgentRunner, cfg config.Config, prompt string, sessionID string) AgentRunResult {
	first := runAgentWithDeadline(ctx, runner, cfg, prompt, sessionID)
	if ctx.Err() != nil ||
		(first.ErrorType != "" && !isRetryableAgentTransportError(first.ErrorType)) ||
		(first.ErrorType == "" && strings.TrimSpace(first.FinalText) != "") {
		return first
	}
	second := runAgentWithDeadline(ctx, runner, cfg, prompt, sessionID+"-retry1")
	secondErr := second.ErrorType
	mergeAgentRunResult(&second, first)
	second.ErrorType = secondErr
	if second.ErrorType == "" || strings.TrimSpace(second.FinalText) != "" {
		if second.ErrorType == "" && strings.TrimSpace(second.FinalText) == "" {
			second.ErrorType = "empty_answer"
		}
		return second
	}
	return first
}

func isRetryableAgentTransportError(errType string) bool {
	lower := strings.ToLower(strings.TrimSpace(errType))
	if lower == "" {
		return false
	}
	for _, needle := range []string{
		" eof",
		": eof",
		"unexpected eof",
		"connection reset",
		"connection refused",
		"stream error",
		"server closed",
	} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

func memoryArenaSubtaskTimeout(caseTimeoutSeconds int, subtaskCount int) time.Duration {
	if caseTimeoutSeconds <= 0 {
		return 0
	}
	if subtaskCount <= 0 {
		subtaskCount = 1
	}
	seconds := caseTimeoutSeconds / subtaskCount
	if seconds < 90 {
		seconds = 90
	}
	if seconds > 180 {
		seconds = 180
	}
	return time.Duration(seconds) * time.Second
}

func contextWithOptionalTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func prepareMemoryCase(ctx context.Context, spec CaseSpec, options RunnerOptions) (CaseResult, context.Context, string, string, time.Time, []TimelineEvent, context.CancelFunc) {
	start := time.Now()
	timeline := []TimelineEvent{newTimelineEvent(start, start, "case_start", map[string]any{"case_id": spec.ID})}
	timeout := spec.TimeoutSeconds
	if timeout <= 0 {
		timeout = options.TimeoutSeconds
	}
	caseCtx, cancel := contextWithOptionalTimeout(ctx, time.Duration(timeout)*time.Second)
	artifactDir := filepath.Join(options.ArtifactsDir, sanitizeCaseID(spec.ID))
	_ = os.RemoveAll(artifactDir)
	_ = os.MkdirAll(artifactDir, 0o755)
	result := CaseResult{Case: spec, ExpectedSatisfied: true}
	setArtifactPaths(&result, artifactDir)
	caseDir := filepath.Join(options.WorkDir, "cases", sanitizeCaseID(spec.ID))
	if err := resetMemoryCaseDirectory(caseDir, !options.NoResume); err != nil {
		result.ErrorType = "workdir_cleanup_failed: " + err.Error()
		return result, caseCtx, caseDir, artifactDir, start, timeline, cancel
	}
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		result.ErrorType = "workdir_create_failed: " + err.Error()
		return result, caseCtx, caseDir, artifactDir, start, timeline, cancel
	}
	if err := os.WriteFile(filepath.Join(caseDir, "README.md"), []byte("# Workspace\n"), 0o644); err != nil {
		result.ErrorType = "workdir_seed_failed: " + err.Error()
		return result, caseCtx, caseDir, artifactDir, start, timeline, cancel
	}
	if err := ensureGitBaseline(caseCtx, caseDir); err != nil {
		result.ErrorType = "git_baseline_failed: " + err.Error()
		return result, caseCtx, caseDir, artifactDir, start, timeline, cancel
	}
	result.WorkDir = caseDir
	return result, caseCtx, caseDir, artifactDir, start, timeline, cancel
}

func resetMemoryCaseDirectory(caseDir string, preserveMemory bool) error {
	memoryDir := filepath.Join(caseDir, ".lumina", "memory")
	storePath := filepath.Join(memoryDir, "lumina-memory.sqlite")
	if !preserveMemory {
		return os.RemoveAll(caseDir)
	}
	if info, err := os.Stat(storePath); err != nil || info.IsDir() {
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return os.RemoveAll(caseDir)
	}
	stashDir := caseDir + ".memory-resume"
	if err := os.RemoveAll(stashDir); err != nil {
		return err
	}
	if err := os.Rename(memoryDir, stashDir); err != nil {
		return err
	}
	restore := func() {
		_ = os.MkdirAll(filepath.Dir(memoryDir), 0o755)
		_ = os.Rename(stashDir, memoryDir)
	}
	if err := os.RemoveAll(caseDir); err != nil {
		restore()
		return err
	}
	if err := os.MkdirAll(filepath.Dir(memoryDir), 0o755); err != nil {
		restore()
		return err
	}
	if err := os.Rename(stashDir, memoryDir); err != nil {
		restore()
		return err
	}
	return nil
}

func finishMemoryCase(ctx context.Context, result CaseResult, artifactDir string, start time.Time, timeline []TimelineEvent, agentResult AgentRunResult, expected any, storePath string, hits []string, cfg config.Config, metricInput memoryMetricInput) CaseResult {
	timeline = append(timeline, agentResult.Timeline...)
	result.TTFTMillis = agentResult.TTFTMillis
	result.FirstToolCallMS = agentResult.FirstToolCallMS
	result.InputTokens = agentResult.InputTokens
	result.OutputTokens = agentResult.OutputTokens
	result.ToolCalls = agentResult.ToolCalls
	result.ErrorType = agentResult.ErrorType
	result.Hypothesis = strings.TrimSpace(agentResult.FinalText)
	result.ExpectedAnswer = expected
	result.MemoryStorePath = storePath
	result.MemoryHits = hits
	result.AnswerMatch = answerContainsExpected(result.Hypothesis, expected)
	usesUpstreamEvaluator := result.Case.Benchmark == SuiteLongMemEval
	if !usesUpstreamEvaluator && !result.AnswerMatch && strings.TrimSpace(result.Hypothesis) != "" && agentResult.ErrorType == "" {
		if judged, reason, err := judgeAnswerMatch(ctx, cfg, result.Case.Prompt, expected, result.Hypothesis); err == nil {
			result.AnswerMatch = judged
			timeline = append(timeline, newTimelineEvent(start, time.Now(), "semantic_judge", map[string]any{
				"answer_match": judged,
				"reason":       reason,
			}))
		} else {
			timeline = append(timeline, newTimelineEvent(start, time.Now(), "semantic_judge_failed", map[string]any{"error": err.Error()}))
		}
	}
	result.ExpectedSatisfied = result.AnswerMatch && !usesUpstreamEvaluator
	if result.AnswerMatch && result.ErrorType == "" && !usesUpstreamEvaluator {
		result.Resolved = true
		result.TestPassRate = 1
	} else if result.ErrorType == "" && !usesUpstreamEvaluator {
		result.ErrorType = "answer_mismatch"
	}
	details, metrics := collectMemoryMetrics(ctx, storePath, metricInput, result.InputTokens, result.Resolved, result.ErrorType)
	if len(details) > 0 {
		result.MemoryHitDetails = details
		result.MemoryHits = memoryHitTitles(details)
	}
	if metrics != nil {
		result.MemoryMetrics = metrics
		timeline = append(timeline, newTimelineEvent(start, time.Now(), "memory_recall_metrics", map[string]any{
			"retrieved_count":       metrics.RetrievedCount,
			"evidence_hit":          metrics.EvidenceHit,
			"evidence_recall_at_k":  metrics.EvidenceRecallAtK,
			"retrieval_error_type":  metrics.RetrievalErrorType,
			"subtask_answer_rate":   metrics.SubtaskAnswerRate,
			"memory_update_success": metrics.MemoryUpdateSuccessRate,
		}))
	}
	result.DurationSeconds = time.Since(start).Seconds()
	result.Timeline = append(timeline, newTimelineEvent(start, time.Now(), "validation_end", map[string]any{"answer_match": result.AnswerMatch}))
	writeCaseArtifacts(artifactDir, result.Case, agentResult, result)
	return result
}

func judgeAnswerMatch(ctx context.Context, cfg config.Config, question string, expected any, hypothesis string) (bool, string, error) {
	client, err := agent.CreateConfiguredLLMClient(cfg, cfg.APIModel, cfg.APIMaxTokens, nil, nil)
	if err != nil {
		return false, "", err
	}
	expectedText := stringifyAny(expected)
	if len([]rune(expectedText)) > 12000 {
		expectedText = firstRunes(expectedText, 12000)
	}
	if len([]rune(hypothesis)) > 12000 {
		hypothesis = firstRunes(hypothesis, 12000)
	}
	if len([]rune(question)) > 6000 {
		question = firstRunes(question, 6000)
	}
	prompt := fmt.Sprintf(`Question:
%s

Expected answer:
%s

Candidate answer:
%s

Decide whether the candidate answer is semantically correct for the expected answer. Accept concise paraphrases, equivalent math statements, and correct structured content even if wording differs. Reject missing key facts, contradictions, or unsupported "I don't know".

Return strict JSON only:
{"correct": true|false, "reason": "short reason"}`, question, expectedText, hypothesis)
	raw, err := streamJudgeAnswer(ctx, client, prompt)
	if err != nil {
		return false, "", err
	}
	var parsed struct {
		Correct bool   `json:"correct"`
		Reason  string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(extractJSONObject(raw)), &parsed); err != nil {
		return false, "", err
	}
	return parsed.Correct, parsed.Reason, nil
}

func streamJudgeAnswer(ctx context.Context, client luminaapi.LLMClient, prompt string) (string, error) {
	streamCtx := luminaapi.ContextWithStreamIdleTimeout(ctx, 10*time.Minute)
	var raw strings.Builder
	for result := range client.StreamChat(streamCtx, "You are a strict benchmark answer judge. Output JSON only.", []map[string]any{{"role": "user", "content": prompt}}, nil, nil) {
		if result.Err != nil {
			return raw.String(), result.Err
		}
		event := result.Event
		switch memoryEventString(event["type"]) {
		case "text_delta":
			raw.WriteString(memoryEventString(event["text"]))
		case "error":
			message := memoryEventString(event["message"])
			if message == "" {
				message = stringifyAny(event)
			}
			return raw.String(), fmt.Errorf("%s", message)
		}
	}
	return raw.String(), nil
}

func memoryEventString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	case nil:
		return ""
	default:
		return fmt.Sprint(v)
	}
}

func extractJSONObject(text string) string {
	text = strings.TrimSpace(text)
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start >= 0 && end >= start {
		return text[start : end+1]
	}
	return text
}

func failMemoryCase(result CaseResult, artifactDir string, start time.Time, timeline []TimelineEvent, errType string) CaseResult {
	result.ErrorType = errType
	result.DurationSeconds = time.Since(start).Seconds()
	result.Timeline = timeline
	writeCaseArtifacts(artifactDir, result.Case, AgentRunResult{}, result)
	return result
}

func collectMemoryMetrics(ctx context.Context, storePath string, input memoryMetricInput, inputTokens int, resolved bool, errorType string) ([]MemoryHit, *MemoryMetrics) {
	store, err := longmemory.Open(ctx, storePath)
	if err != nil {
		return nil, &MemoryMetrics{RetrievalErrorType: "memory_metrics_failed", RecallWarnings: []string{err.Error()}}
	}
	defer store.Close()
	usedIDs := actualRecalledMemoryIDs(ctx, store)
	traces, traceErr := store.ListRetrievalTraces(ctx, 10000)
	details := make([]MemoryHit, 0, len(usedIDs))
	seenSources := map[string]struct{}{}
	staleUses := 0
	for _, id := range usedIDs {
		entry, err := store.Get(ctx, id)
		if err == nil && entry != nil {
			source := strings.TrimSpace(entry.SourceSessionID)
			if source != "" {
				seenSources[source] = struct{}{}
			}
			stale := entry.Status != longmemory.StatusActive || (!entry.ValidUntil.IsZero() && !entry.ValidUntil.After(time.Now().UTC()))
			if stale {
				staleUses++
			}
			details = append(details, MemoryHit{Rank: len(details) + 1, MemoryID: entry.MemoryID,
				DocumentKind: entry.DocumentKind, MessageID: entry.MessageID, Title: entry.Title,
				SourceSessionID: source, Tags: entry.Tags, Evidence: false, Stale: stale})
			continue
		}
		if chunks, chunkErr := store.GetChunks(ctx, []string{id}); chunkErr == nil && len(chunks) == 1 {
			chunk := chunks[0]
			seenSources[chunk.SessionID] = struct{}{}
			details = append(details, MemoryHit{Rank: len(details) + 1, MemoryID: chunk.ChunkID,
				DocumentKind: "chunk", MessageID: chunk.MessageID, Title: "Message " + chunk.MessageID,
				SourceSessionID: chunk.SessionID})
			continue
		}
		if atoms, atomErr := store.GetAtoms(ctx, []string{id}); atomErr == nil && len(atoms) == 1 {
			atom := atoms[0]
			seenSources[atom.SessionID] = struct{}{}
			details = append(details, MemoryHit{Rank: len(details) + 1, MemoryID: atom.AtomID,
				DocumentKind: "atom", ParentID: atom.ChunkID, MessageID: atom.MessageID,
				Title: "Message " + atom.MessageID, SourceSessionID: atom.SessionID})
		}
	}
	metrics := &MemoryMetrics{
		RetrievedCount:  len(details),
		SubtaskTotal:    input.SubtaskTotal,
		SubtaskAnswered: input.SubtaskAnswered,
		StaleUseCount:   staleUses,
	}
	if traceErr != nil {
		metrics.RecallWarnings = append(metrics.RecallWarnings, "retrieval trace: "+traceErr.Error())
	}
	for _, trace := range traces {
		metrics.RetrievalRuns++
		metrics.RetrievalDurationMS += trace.DurationMS
		metrics.MemoryTokenEstimate += trace.EstimatedTokens
		if trace.Error != "" {
			metrics.RecallWarnings = append(metrics.RecallWarnings, trace.Error)
		}
	}
	if metrics.MemoryTokenEstimate == 0 {
		metrics.MemoryTokenEstimate = estimateMemoryHitTokens(ctx, store, usedIDs)
	}
	if metrics.RetrievedCount > 0 {
		metrics.StaleUseRate = float64(metrics.StaleUseCount) / float64(metrics.RetrievedCount)
	}
	if inputTokens > 0 && metrics.MemoryTokenEstimate > 0 {
		metrics.MemoryTokenRatio = float64(metrics.MemoryTokenEstimate) / float64(inputTokens)
	}
	if metrics.SubtaskTotal > 0 {
		metrics.SubtaskAnswerRate = float64(metrics.SubtaskAnswered) / float64(metrics.SubtaskTotal)
		memorySessions := map[string]struct{}{}
		for _, sessionID := range input.SubtaskMemorySessions {
			if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
				memorySessions[sessionID] = struct{}{}
			}
		}
		metrics.MemoryUpdateCount = len(memorySessions)
		metrics.MemoryUpdateSuccessRate = float64(metrics.MemoryUpdateCount) / float64(metrics.SubtaskTotal)
		if metrics.MemoryUpdateSuccessRate > 1 {
			metrics.MemoryUpdateSuccessRate = 1
		}
		previousSources := map[string]struct{}{}
		for _, hit := range details {
			if _, ok := memorySessions[hit.SourceSessionID]; ok {
				previousSources[hit.SourceSessionID] = struct{}{}
			}
		}
		metrics.PreviousSubtaskHitCount = len(previousSources)
		if metrics.SubtaskTotal > 1 {
			metrics.PreviousSubtaskHitRate = float64(metrics.PreviousSubtaskHitCount) / float64(metrics.SubtaskTotal-1)
			if metrics.PreviousSubtaskHitRate > 1 {
				metrics.PreviousSubtaskHitRate = 1
			}
		}
	}
	if input.Suite == SuiteLongMemEval {
		goldSources := map[string]struct{}{}
		goldChunkIDs := map[string]struct{}{}
		goldMessageIDs := map[string]struct{}{}
		for _, chunkID := range input.GoldChunkIDs {
			if strings.TrimSpace(chunkID) != "" {
				goldChunkIDs[strings.TrimSpace(chunkID)] = struct{}{}
			}
		}
		for messageID := range input.GoldMessageTexts {
			goldMessageIDs[messageID] = struct{}{}
		}
		for _, source := range input.GoldSourceSessions {
			if strings.TrimSpace(source) != "" {
				goldSources[strings.TrimSpace(source)] = struct{}{}
			}
		}
		metrics.EvidenceTotal = len(goldChunkIDs)
		metrics.GoldChunkCount = len(goldChunkIDs)
		metrics.GoldMessageCount = len(goldMessageIDs)
		metrics.GoldSourceSessionCount = len(goldSources)
		hitMessages, hitChunks := map[string]struct{}{}, map[string]struct{}{}
		for index := range details {
			chunkID := details[index].MemoryID
			if details[index].DocumentKind == "atom" && details[index].ParentID != "" {
				chunkID = details[index].ParentID
			}
			_, isGold := goldChunkIDs[chunkID]
			details[index].Evidence = isGold
			if isGold {
				hitChunks[chunkID] = struct{}{}
				if metrics.FirstEvidenceRank == nil {
					rank := details[index].Rank
					metrics.FirstEvidenceRank = &rank
					metrics.EvidenceMRR = 1 / float64(rank)
				}
			}
			if _, ok := goldMessageIDs[details[index].MessageID]; ok {
				hitMessages[details[index].MessageID] = struct{}{}
			}
			if _, ok := goldSources[details[index].SourceSessionID]; ok {
				seenSources[details[index].SourceSessionID] = struct{}{}
			}
		}
		metrics.EvidenceHitCount, metrics.GoldChunkHitCount = len(hitChunks), len(hitChunks)
		if metrics.EvidenceTotal > 0 {
			metrics.EvidenceHit = metrics.EvidenceHitCount > 0
			metrics.EvidenceRecallAtK = float64(metrics.EvidenceHitCount) / float64(metrics.EvidenceTotal)
			if metrics.EvidenceRecallAtK > 1 {
				metrics.EvidenceRecallAtK = 1
			}
		}
		metrics.GoldMessageHitCount = len(hitMessages)
		if metrics.GoldMessageCount > 0 {
			metrics.GoldMessageRecall = float64(metrics.GoldMessageHitCount) / float64(metrics.GoldMessageCount)
		}
		if metrics.GoldChunkCount > 0 {
			metrics.InjectedChunkRecall = float64(metrics.GoldChunkHitCount) / float64(metrics.GoldChunkCount)
		}
		metrics.InjectedTextCoverage = injectedGoldTextCoverage(ctx, store, usedIDs, input.GoldMessageTexts)
		if len(goldSources) > 0 {
			for source := range seenSources {
				if _, ok := goldSources[source]; ok {
					metrics.GoldSourceSessionHitCount++
				}
			}
			metrics.SourceSessionRecall = float64(metrics.GoldSourceSessionHitCount) / float64(len(goldSources))
			if metrics.SourceSessionRecall > 1 {
				metrics.SourceSessionRecall = 1
			}
		}
	}
	metrics.RetrievalErrorType = classifyMemoryRetrievalError(metrics, input.Suite, resolved, errorType)
	return details, metrics
}

func actualRecalledMemoryIDs(ctx context.Context, store *longmemory.Store) []string {
	records, err := store.ListUsed(ctx, 10000)
	if err != nil {
		return nil
	}
	for i, j := 0, len(records)-1; i < j; i, j = i+1, j-1 {
		records[i], records[j] = records[j], records[i]
	}
	seen := map[string]struct{}{}
	var ids []string
	for _, record := range records {
		for _, id := range record.MemoryIDs {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if _, ok := seen[id]; ok {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
		}
	}
	return ids
}

type runeInterval struct {
	start int
	end   int
}

func injectedGoldTextCoverage(ctx context.Context, store *longmemory.Store, usedIDs []string, goldMessages map[string]string) float64 {
	if len(goldMessages) == 0 || len(usedIDs) == 0 {
		return 0
	}
	chunks, err := store.GetChunks(ctx, usedIDs)
	if err != nil {
		return 0
	}
	covered := map[string][]runeInterval{}
	for _, chunk := range chunks {
		text, ok := goldMessages[chunk.MessageID]
		if !ok {
			continue
		}
		length := len([]rune(strings.TrimSpace(text)))
		start := maxIntMemoryMetric(0, minIntMemoryMetric(chunk.StartRune, length))
		end := maxIntMemoryMetric(start, minIntMemoryMetric(chunk.EndRune, length))
		if end > start {
			covered[chunk.MessageID] = append(covered[chunk.MessageID], runeInterval{start: start, end: end})
		}
	}
	atoms, atomErr := store.GetAtoms(ctx, usedIDs)
	if atomErr == nil {
		for _, atom := range atoms {
			text, ok := goldMessages[atom.MessageID]
			if !ok {
				continue
			}
			length := len([]rune(strings.TrimSpace(text)))
			start := maxIntMemoryMetric(0, minIntMemoryMetric(atom.StartRune, length))
			end := maxIntMemoryMetric(start, minIntMemoryMetric(atom.EndRune, length))
			if end > start {
				covered[atom.MessageID] = append(covered[atom.MessageID], runeInterval{start: start, end: end})
			}
		}
	}
	totalRunes, coveredRunes := 0, 0
	for messageID, text := range goldMessages {
		length := len([]rune(strings.TrimSpace(text)))
		totalRunes += length
		intervals := covered[messageID]
		sort.Slice(intervals, func(i, j int) bool {
			if intervals[i].start == intervals[j].start {
				return intervals[i].end < intervals[j].end
			}
			return intervals[i].start < intervals[j].start
		})
		cursor := 0
		for _, interval := range intervals {
			if interval.end <= cursor {
				continue
			}
			start := maxIntMemoryMetric(cursor, interval.start)
			coveredRunes += interval.end - start
			cursor = interval.end
		}
	}
	if totalRunes == 0 {
		return 0
	}
	return float64(coveredRunes) / float64(totalRunes)
}

func minIntMemoryMetric(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxIntMemoryMetric(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func memoryHitTitles(details []MemoryHit) []string {
	out := make([]string, 0, len(details))
	for _, hit := range details {
		out = append(out, hit.Title)
	}
	return out
}

func estimateMemoryHitTokens(ctx context.Context, store *longmemory.Store, ids []string) int {
	runes := 0
	seen := map[string]struct{}{}
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		entry, err := store.Get(ctx, id)
		if err == nil && entry != nil {
			runes += len([]rune(entry.Title)) + len([]rune(entry.Summary)) + len([]rune(entry.Content)) + 160
			continue
		}
		if atoms, atomErr := store.GetAtoms(ctx, []string{id}); atomErr == nil && len(atoms) == 1 {
			runes += len([]rune(atoms[0].Text)) + 80
			continue
		}
		if chunks, chunkErr := store.GetChunks(ctx, []string{id}); chunkErr == nil && len(chunks) == 1 {
			runes += len([]rune(chunks[0].Text)) + 80
		}
	}
	if runes == 0 {
		return 0
	}
	return (runes + 3) / 4
}

func classifyMemoryRetrievalError(metrics *MemoryMetrics, suite string, resolved bool, errorType string) string {
	if metrics == nil {
		return ""
	}
	if resolved {
		if suite == SuiteLongMemEval && metrics.EvidenceTotal > 0 && !metrics.EvidenceHit {
			return "answer_correct_without_gold_evidence"
		}
		return ""
	}
	if errorType == "" {
		errorType = "unresolved"
	}
	if strings.Contains(errorType, "timeout") {
		return "agent_timeout"
	}
	if suite == SuiteLongMemEval && metrics.EvidenceTotal > 0 && !metrics.EvidenceHit {
		return "retrieval_miss"
	}
	if suite == SuiteLongMemEval && errorType == "unresolved" {
		return "evaluation_pending"
	}
	if suite == SuiteMemoryArena && metrics.SubtaskTotal > 0 {
		if metrics.SubtaskAnswered < metrics.SubtaskTotal {
			return "loop_incomplete"
		}
		if metrics.MemoryUpdateCount < metrics.SubtaskAnswered {
			return "memory_update_miss"
		}
	}
	if metrics.RetrievedCount == 0 {
		return "no_memory_recalled"
	}
	return "reasoning_miss"
}

func memoryCaseConfig(base config.Config, cwd, storePath string) config.Config {
	cfg := base
	cfg.CWD = cwd
	cfg.HarnessMode = ""
	cfg.LongTermMemoryEnabled = true
	cfg.LongTermMemoryStore = storePath
	cfg.SessionMemoryEnabled = false
	cfg.MemoryBackgroundExtractionEnabled = false
	cfg.ProjectRuntimeDir = filepath.Join(cwd, ".lumina", "project-runtime")
	config.PinFields(&cfg, "harness_mode", "long_term_memory_enabled", "long_term_memory_store", "memory_background_extraction_enabled", "session_memory_enabled", "memory_embedding_enabled", "memory_graph_max_hops", "project_runtime_dir")
	return cfg
}

func ingestLongMemEvalHistory(ctx context.Context, storePath string, c longMemEvalCase, cfg config.Config) error {
	extractionCfg := cfg
	extractionCfg.LongTermMemoryEnabled = false
	extractionCfg.LongTermMemoryStore = storePath
	for sessionIndex, session := range c.HaystackSessions {
		sessionID, duplicate := longMemEvalSessionIdentityAt(c, sessionIndex)
		if duplicate {
			continue
		}
		baseTime := parseLongMemEvalDate(stringAt(c.HaystackDates, sessionIndex, ""))
		state := agent.NewAgentState()
		state.MemorySessionID = sessionID
		state.MemoryAgentID = "history-replay"
		state.MemoryAgentType = "history-replay"
		for turnIndex, turn := range session {
			content, _ := turn["content"].(string)
			if strings.TrimSpace(content) == "" {
				continue
			}
			role, _ := turn["role"].(string)
			messageID := longmemory.StableID(longmemory.ScopeProject, sessionID,
				fmt.Sprintf("turn-%03d-%s", turnIndex+1, role), content)
			occurredAt := baseTime
			if !occurredAt.IsZero() {
				occurredAt = occurredAt.Add(time.Duration(turnIndex) * time.Second)
			}
			state.Messages = append(state.Messages, map[string]any{
				"role": role, "id": messageID, "timestamp": occurredAt.Format(time.RFC3339),
				"content": []map[string]any{{"type": "text", "text": content}},
			})
			if role == "user" {
				state.UserTurnCount++
			}
		}
		controller := agent.NewExtractionController(extractionCfg, coretools.NewToolRegistry())
		controller.SourceSessionID = sessionID
		controller.SourceAgentID = "history-replay"
		for {
			ingested, err := controller.IngestMessages(ctx, &state)
			if err != nil {
				return fmt.Errorf("ingest session %s: %w", sessionID, err)
			}
			if ingested == 0 {
				break
			}
		}
		if err := syncLongMemEvalExtractionCursor(ctx, storePath, sessionID, &state); err != nil {
			return fmt.Errorf("resume extract session %s: %w", sessionID, err)
		}
		if err := completeMemoryExtraction(ctx, &state, func(attemptCtx context.Context) error {
			_, err := controller.ExtractNow(attemptCtx, &state)
			return err
		}, time.Second, 30*time.Second); err != nil {
			return fmt.Errorf("extract session %s: %w", sessionID, err)
		}
	}
	return nil
}

func completeMemoryExtraction(ctx context.Context, state *agent.AgentState, extract func(context.Context) error,
	retryBase, retryMax time.Duration) error {
	if state == nil {
		return errors.New("memory extraction state is required")
	}
	failures := 0
	for state.MemoryExtractionCursor < len(state.Messages) {
		if err := ctx.Err(); err != nil {
			return err
		}
		before := state.MemoryExtractionCursor
		if err := extract(ctx); err != nil {
			failures++
			if err := waitMemoryExtractionRetry(ctx, retryDelay(failures, retryBase, retryMax)); err != nil {
				return err
			}
			continue
		}
		failures = 0
		if state.MemoryExtractionCursor <= before {
			return fmt.Errorf("made no cursor progress at message %d", before)
		}
	}
	return nil
}

func retryDelay(failures int, base, maximum time.Duration) time.Duration {
	if failures <= 0 || base <= 0 {
		return 0
	}
	if maximum < base {
		maximum = base
	}
	delay := base
	for attempt := 1; attempt < failures && delay < maximum; attempt++ {
		if delay > maximum/2 {
			return maximum
		}
		delay *= 2
	}
	if delay > maximum {
		return maximum
	}
	return delay
}

func waitMemoryExtractionRetry(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func longMemEvalSessionIDAt(c longMemEvalCase, sessionIndex int) string {
	sessionID, _ := longMemEvalSessionIdentityAt(c, sessionIndex)
	return sessionID
}

func longMemEvalSessionIdentityAt(c longMemEvalCase, sessionIndex int) (string, bool) {
	base := stringAt(c.HaystackSessionIDs, sessionIndex, fmt.Sprintf("session-%d", sessionIndex+1))
	var representatives []int
	for index := 0; index < sessionIndex; index++ {
		if stringAt(c.HaystackSessionIDs, index, fmt.Sprintf("session-%d", index+1)) != base {
			continue
		}
		known := false
		for _, representative := range representatives {
			if longMemEvalSessionContentEqual(c.HaystackSessions[index], c.HaystackSessions[representative]) {
				known = true
				break
			}
		}
		if !known {
			representatives = append(representatives, index)
		}
	}
	for variant, representative := range representatives {
		if longMemEvalSessionContentEqual(c.HaystackSessions[sessionIndex], c.HaystackSessions[representative]) {
			if variant == 0 {
				return base, true
			}
			return fmt.Sprintf("%s#%d", base, variant+1), true
		}
	}
	if len(representatives) == 0 {
		return base, false
	}
	return fmt.Sprintf("%s#%d", base, len(representatives)+1), false
}

func longMemEvalSessionContentEqual(left, right []map[string]any) bool {
	leftIndex := 0
	rightIndex := 0
	for {
		for leftIndex < len(left) && strings.TrimSpace(longMemEvalTurnString(left[leftIndex], "content")) == "" {
			leftIndex++
		}
		for rightIndex < len(right) && strings.TrimSpace(longMemEvalTurnString(right[rightIndex], "content")) == "" {
			rightIndex++
		}
		if leftIndex == len(left) || rightIndex == len(right) {
			return leftIndex == len(left) && rightIndex == len(right)
		}
		if longMemEvalTurnString(left[leftIndex], "role") != longMemEvalTurnString(right[rightIndex], "role") ||
			longMemEvalTurnString(left[leftIndex], "content") != longMemEvalTurnString(right[rightIndex], "content") {
			return false
		}
		leftIndex++
		rightIndex++
	}
}

func longMemEvalTurnString(turn map[string]any, key string) string {
	value, _ := turn[key].(string)
	return value
}

func syncLongMemEvalExtractionCursor(ctx context.Context, storePath, sessionID string, state *agent.AgentState) error {
	store, err := longmemory.Open(ctx, storePath)
	if err != nil {
		return err
	}
	defer store.Close()
	_, index, err := store.GetCursor(ctx, "long-term-extraction:history-replay", sessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if index+1 > state.MemoryExtractionCursor {
		state.MemoryExtractionCursor = index + 1
	}
	return nil
}

func collectLongMemEvalGold(ctx context.Context, storePath string, scope longmemory.Scope, c longMemEvalCase) (longMemEvalSeedEvidence, error) {
	gold := longMemEvalSeedEvidence{MessageTexts: map[string]string{}}
	store, err := longmemory.Open(ctx, storePath)
	if err != nil {
		return gold, err
	}
	defer store.Close()
	goldMessages := longMemEvalGoldMessages(c)
	goldSessions := longMemEvalGoldSessions(c)
	for sessionID := range goldSessions {
		gold.SourceSessions = append(gold.SourceSessions, sessionID)
	}
	messageIDs := make([]string, 0, len(goldMessages))
	for messageID, text := range goldMessages {
		messageIDs = append(messageIDs, messageID)
		gold.MessageTexts[messageID] = text
	}
	chunks, err := store.ChunksByMessageIDs(ctx, []longmemory.Scope{scope}, messageIDs)
	if err != nil {
		return gold, err
	}
	for _, chunk := range chunks {
		gold.ChunkIDs = append(gold.ChunkIDs, chunk.ChunkID)
	}
	gold.ChunkIDs = uniqueMemoryStrings(gold.ChunkIDs)
	gold.SourceSessions = uniqueMemoryStrings(gold.SourceSessions)
	return gold, nil
}

func longMemEvalGoldSessions(c longMemEvalCase) map[string]struct{} {
	result := map[string]struct{}{}
	for sessionIndex, session := range c.HaystackSessions {
		for _, turn := range session {
			hasAnswer, _ := turn["has_answer"].(bool)
			if hasAnswer {
				result[longMemEvalSessionIDAt(c, sessionIndex)] = struct{}{}
				break
			}
		}
	}
	return result
}

func longMemEvalGoldMessages(c longMemEvalCase) map[string]string {
	gold := map[string]string{}
	for sessionIndex, session := range c.HaystackSessions {
		sessionID := longMemEvalSessionIDAt(c, sessionIndex)
		for turnIndex, turn := range session {
			content, _ := turn["content"].(string)
			role, _ := turn["role"].(string)
			hasAnswer, _ := turn["has_answer"].(bool)
			if !hasAnswer || strings.TrimSpace(content) == "" {
				continue
			}
			messageID := longmemory.StableID(longmemory.ScopeProject, sessionID,
				fmt.Sprintf("turn-%03d-%s", turnIndex+1, role), content)
			gold[messageID] = strings.TrimSpace(content)
		}
	}
	return gold
}

func longMemEvalGoldMessageIDs(c longMemEvalCase) map[string]struct{} {
	gold := map[string]struct{}{}
	for messageID := range longMemEvalGoldMessages(c) {
		gold[messageID] = struct{}{}
	}
	return gold
}

func ingestMemoryArenaBackground(ctx context.Context, cfg config.Config, domain string, c memoryArenaCase) error {
	background := stringifyAny(c.Backgrounds)
	if strings.TrimSpace(background) == "" || strings.TrimSpace(background) == "null" {
		background = stringifyAny(c.BasePerson)
	}
	if strings.TrimSpace(background) == "" || strings.TrimSpace(background) == "null" {
		return nil
	}
	extractionCfg := cfg
	extractionCfg.LongTermMemoryEnabled = false
	sessionID := "environment-background-" + sanitizeCaseID(domain)
	state := agent.NewAgentState()
	state.MemorySessionID = sessionID
	state.MemoryAgentID = "environment"
	state.MemoryAgentType = "environment"
	state.UserTurnCount = 1
	state.Messages = []map[string]any{{
		"role": "user", "id": sessionID + "-message",
		"content": []map[string]any{{"type": "text", "text": background}},
	}}
	controller := agent.NewExtractionController(extractionCfg, coretools.NewToolRegistry())
	controller.SourceSessionID = sessionID
	controller.SourceAgentID = "environment"
	_, err := controller.ExtractNow(ctx, &state)
	return err
}

func extractMemoryArenaSubtask(ctx context.Context, cfg config.Config, sessionID, question, answer string) error {
	extractionCfg := cfg
	extractionCfg.LongTermMemoryEnabled = false
	state := agent.NewAgentState()
	state.MemorySessionID = sessionID
	state.MemoryAgentID = "main"
	state.MemoryAgentType = "main"
	state.UserTurnCount = 1
	state.TurnCount = 1
	state.Messages = []map[string]any{
		{"role": "user", "id": sessionID + "-user", "content": []map[string]any{{"type": "text", "text": question}}},
		{"role": "assistant", "id": sessionID + "-assistant", "content": []map[string]any{{"type": "text", "text": answer}}},
	}
	controller := agent.NewExtractionController(extractionCfg, coretools.NewToolRegistry())
	controller.SourceSessionID = sessionID
	controller.SourceAgentID = "main"
	if _, err := controller.ExtractNow(ctx, &state); err != nil {
		return err
	}
	store, err := longmemory.Open(ctx, cfg.LongTermMemoryStore)
	if err != nil {
		return err
	}
	defer store.Close()
	scope := longmemory.Scope{Type: longmemory.ScopeProject, Key: longmemory.ProjectScopeKey(cfg.CWD)}
	memoryID := longmemory.StableID(scope.Type, scope.Key, "session-index", sessionID)
	if _, err := store.Get(ctx, memoryID); err != nil {
		return fmt.Errorf("verify persisted subtask Session %s: %w", sessionID, err)
	}
	spans, err := store.ListEvidenceSpans(ctx, []string{memoryID})
	if err != nil {
		return fmt.Errorf("verify persisted subtask evidence %s: %w", sessionID, err)
	}
	if len(spans[memoryID]) == 0 {
		return fmt.Errorf("verify persisted subtask evidence %s: no evidence spans", sessionID)
	}
	return nil
}

func parseLongMemEvalDate(value string) time.Time {
	for _, layout := range []string{"2006/01/02 (Mon) 15:04", "2006/01/02 15:04", time.RFC3339} {
		if parsed, err := time.Parse(layout, strings.TrimSpace(value)); err == nil {
			return parsed.UTC()
		}
	}
	return time.Time{}
}

func mergeAgentRunResult(dst *AgentRunResult, src AgentRunResult) {
	if dst == nil {
		return
	}
	dst.Events = append(dst.Events, src.Events...)
	dst.Timeline = append(dst.Timeline, src.Timeline...)
	dst.TransientErrors = append(dst.TransientErrors, src.TransientErrors...)
	dst.InputTokens += src.InputTokens
	dst.OutputTokens += src.OutputTokens
	dst.ToolCalls += src.ToolCalls
	if dst.TTFTMillis == nil && src.TTFTMillis != nil {
		dst.TTFTMillis = src.TTFTMillis
	}
	if dst.FirstToolCallMS == nil && src.FirstToolCallMS != nil {
		dst.FirstToolCallMS = src.FirstToolCallMS
	}
	if dst.ErrorType == "" && src.ErrorType != "" {
		dst.ErrorType = src.ErrorType
	}
}

func loadMemoryArenaCases(path string) ([]memoryArenaCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []memoryArenaCase
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var c memoryArenaCase
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, scanner.Err()
}

func filterLongMemEvalCases(cases []longMemEvalCase, id string) []longMemEvalCase {
	filtered := make([]longMemEvalCase, 0, 1)
	for _, c := range cases {
		if c.QuestionID == id {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

func filterMemoryArenaCases(cases []memoryArenaCase, id string) []memoryArenaCase {
	filtered := make([]memoryArenaCase, 0, 1)
	for _, c := range cases {
		if fmt.Sprintf("%v", c.ID) == id {
			filtered = append(filtered, c)
		}
	}
	return filtered
}

func inferMemoryArenaDomain(path string, fallback string) string {
	dir := filepath.Base(filepath.Dir(path))
	if strings.TrimSpace(dir) == "" || dir == "." || dir == string(filepath.Separator) {
		return fallback
	}
	return dir
}

func answerContainsExpected(hypothesis string, expected any) bool {
	h := normalizeAnswer(hypothesis)
	if h == "" {
		return false
	}
	if isUncertainAnswer(h) && !isAbstentionExpected(expected) {
		return false
	}
	switch v := expected.(type) {
	case string:
		return looseStringMatch(h, v)
	case []any:
		if len(v) == 0 {
			return false
		}
		matches := 0
		for _, item := range v {
			if answerContainsExpected(hypothesis, item) {
				matches++
			}
		}
		return float64(matches)/float64(len(v)) >= 0.6
	default:
		e := normalizeAnswer(stringifyAny(v))
		return e != "" && len([]rune(e)) < 300 && strings.Contains(h, e)
	}
}

func isUncertainAnswer(normalizedHypothesis string) bool {
	uncertain := []string{
		"i don t know",
		"do not know",
		"cannot determine",
		"can t determine",
		"not enough information",
		"no specific",
		"unable to determine",
	}
	for _, marker := range uncertain {
		if strings.Contains(normalizedHypothesis, marker) {
			return true
		}
	}
	return false
}

func isAbstentionExpected(expected any) bool {
	text := normalizeAnswer(stringifyAny(expected))
	abstainMarkers := []string{"i don t know", "do not know", "not enough information", "unknown", "cannot determine"}
	for _, marker := range abstainMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

var answerPunctuation = regexp.MustCompile(`[^a-z0-9一-龥]+`)
var answerStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "be": true, "is": true, "of": true, "on": true, "or": true, "the": true, "to": true, "was": true, "were": true, "with": true,
	"correct": true, "answer": true, "exactly": true, "select": true, "all": true, "that": true, "this": true,
}

func normalizeAnswer(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = answerPunctuation.ReplaceAllString(value, " ")
	return strings.Join(strings.Fields(value), " ")
}

func looseStringMatch(normalizedHypothesis string, expected string) bool {
	expectedNorm := normalizeAnswer(expected)
	if expectedNorm == "" {
		return false
	}
	if strings.Contains(" "+normalizedHypothesis+" ", " "+expectedNorm+" ") {
		return true
	}
	tokens := meaningfulTokens(expectedNorm)
	if len(tokens) == 0 {
		return false
	}
	hypothesisTokens := map[string]struct{}{}
	for _, token := range strings.Fields(normalizedHypothesis) {
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
	var out []string
	for _, token := range strings.Fields(value) {
		if answerStopwords[token] || (len([]rune(token)) <= 1 && !containsASCIIDigit(token)) {
			continue
		}
		out = append(out, token)
	}
	return out
}

func containsASCIIDigit(value string) bool {
	for _, current := range value {
		if current >= '0' && current <= '9' {
			return true
		}
	}
	return false
}

func uniqueMemoryStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func stringifyAny(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	data, _ := json.MarshalIndent(value, "", "  ")
	return string(data)
}

func firstRunes(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
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
