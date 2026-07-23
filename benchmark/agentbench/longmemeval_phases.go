package agentbench

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"
	"sync"
	"time"

	"LuminaCode/agent"
	luminaapi "LuminaCode/api"
	"LuminaCode/config"
)

type longMemEvalAnswerFingerprintInput struct {
	DatasetSHA256   string `json:"dataset_sha256"`
	IndexSHA256     string `json:"index_sha256"`
	RunID           string `json:"run_id"`
	PromptIdentity  string `json:"prompt_identity"`
	AnswerModel     string `json:"answer_model"`
	FallbackEnabled bool   `json:"fallback_enabled"`
	FallbackModel   string `json:"fallback_model"`
	CodeIdentity    string `json:"code_identity"`
	MemoryBackend   string `json:"memory_backend"`
	EmbeddingModel  string `json:"embedding_model"`
	RecallMaxItems  int    `json:"recall_max_items"`
	ContextTokens   int    `json:"context_tokens"`
	LatencyBudgetMS int    `json:"latency_budget_ms"`
}

type longMemEvalPrediction struct {
	QuestionID string `json:"question_id"`
	Hypothesis string `json:"hypothesis"`
}

func runLongMemEvalPhasedSuite(ctx context.Context, options RunnerOptions) (Report, error) {
	options = normalizeLongMemEvalOptions(options)
	if err := validateLongMemEvalPhaseOptions(options); err != nil {
		return Report{}, err
	}
	datasetPath, datasetData, allCases, err := loadLongMemEvalDataset(options)
	if err != nil {
		return Report{}, err
	}
	if len(allCases) == 0 {
		return Report{}, errors.New("LongMemEval cleaned dataset is empty")
	}
	datasetSHA := longMemEvalDatasetSHA(datasetData)
	selected := selectedLongMemEvalCases(allCases, options)
	if len(selected) == 0 && options.LongMemEvalPhase != LongMemEvalPhaseEvaluate {
		return Report{}, errors.New("LongMemEval phase selected no cases")
	}
	if options.LongMemEvalPhase == LongMemEvalPhaseAnswer && options.LongMemEvalSmokeSize == 0 &&
		options.CaseID == "" && options.Limit <= 0 && len(allCases) != 500 {
		return Report{}, fmt.Errorf("LongMemEval full answer requires 500 dataset cases, got %d", len(allCases))
	}
	now := options.Now()
	report := Report{
		Suite:       SuiteLongMemEval,
		GeneratedAt: now.Format(time.RFC3339),
		DebugRun: options.LongMemEvalSmokeSize > 0 || options.Limit > 0 || strings.TrimSpace(options.CaseID) != "" ||
			strings.TrimSpace(options.LongMemEvalQuestionType) != "",
		RootDir:      options.RootDir,
		OutputDir:    options.OutputDir,
		WorkDir:      options.WorkDir,
		BenchmarkDir: filepath.Dir(filepath.Dir(datasetPath)),
		Model:        options.Config.APIModel,
		OfficialMetrics: map[string]any{
			"phase":                options.LongMemEvalPhase,
			"dataset_path":         datasetPath,
			"dataset_sha256":       datasetSHA,
			"dataset_cases":        len(allCases),
			"selected_cases":       len(selected),
			"index_dir":            options.LongMemEvalIndexDir,
			"index_source":         options.LongMemEvalIndexSource,
			"automatic_chain":      false,
			"oracle_allowed":       false,
			"smoke_size":           options.LongMemEvalSmokeSize,
			"question_type_filter": strings.TrimSpace(options.LongMemEvalQuestionType),
		},
	}
	selectionPath, err := writeLongMemEvalSmokeSelection(options, datasetSHA, selected)
	if err != nil {
		return Report{}, fmt.Errorf("write LongMemEval smoke selection: %w", err)
	}
	if selectionPath != "" {
		report.OfficialMetrics["smoke_selection_path"] = selectionPath
		report.OfficialMetrics["smoke_stratified"] = true
	}

	switch options.LongMemEvalPhase {
	case LongMemEvalPhasePrepare:
		restoreResources := useLongMemEvalPrepareResources()
		defer restoreResources()
		report.OfficialMetrics["embedding_threads"] = 8
		report.OfficialMetrics["go_memory_limit_bytes"] = int64(16 << 30)
		report.OfficialMetrics["memory_backend"] = "fabric"
		report.OfficialMetrics["semantic_compiler_batch_tokens"] = minInt(10_000, options.Config.MemoryCompileBatchTokens)
		results, checkpointPath, prepareErr := prepareLongMemEvalIndexes(ctx, options, datasetPath, datasetData, selected)
		report.Results = results
		report.Summary = BuildSummary(results)
		report.StageTokenUsage = aggregateLongMemEvalStageUsage(results, LongMemEvalPhasePrepare)
		report.OfficialMetrics["checkpoint_path"] = checkpointPath
		report.OfficialMetrics["token_usage_path"] = longMemEvalTokenUsagePath(options)
		return report, prepareErr
	case LongMemEvalPhaseAnswer:
		var fallbackStatus string
		options.Config, fallbackStatus = checkLongMemEvalFallbackHealth(ctx, options.Config)
		report.OfficialMetrics["fallback_health"] = fallbackStatus
		results, checkpointPath, answerFingerprint, indexDigest, answerErr := runLongMemEvalAnswerSuite(
			ctx, options, datasetSHA, selected)
		report.Results = results
		report.Summary = BuildSummary(results)
		report.StageTokenUsage = aggregateLongMemEvalStageUsage(results, LongMemEvalPhaseAnswer)
		report.OfficialMetrics["checkpoint_path"] = checkpointPath
		report.OfficialMetrics["token_usage_path"] = longMemEvalTokenUsagePath(options)
		report.OfficialMetrics["answer_fingerprint"] = answerFingerprint
		report.OfficialMetrics["index_sha256"] = indexDigest
		report.OfficialMetrics["run_id"] = options.LongMemEvalRunID
		report.OfficialMetrics["case_parallel"] = normalizedCaseParallel(options)
		report.OfficialMetrics["case_resume"] = !options.NoResume
		report.OfficialMetrics["remote_query_expansion"] = false
		report.OfficialMetrics["retrieval_cache"] = false
		if answerErr != nil {
			return report, answerErr
		}
		predictionPath, predictionErr := writeLongMemEvalAnswerPredictions(options, answerFingerprint, selected, results)
		if predictionErr != nil {
			return report, predictionErr
		}
		report.PredictionsPath = predictionPath
		report.OfficialMetrics["prediction_path"] = predictionPath
		report.OfficialMetrics["upstream_evaluator_status"] = "not_run_explicit_phase_required"
		return report, nil
	case LongMemEvalPhaseEvaluate:
		if options.LongMemEvalSmokeSize == 0 && len(allCases) != 500 {
			return report, fmt.Errorf("LongMemEval full evaluation requires 500 dataset cases, got %d", len(allCases))
		}
		if err := validateLongMemEvalPredictionFile(options.LongMemEvalPredictions, selected); err != nil {
			return report, err
		}
		report.PredictionsPath = options.LongMemEvalPredictions
		report.OfficialMetrics["prediction_path"] = options.LongMemEvalPredictions
		evaluatorConfig, fallbackStatus := checkLongMemEvalFallbackHealth(ctx, options.Config)
		report.OfficialMetrics["fallback_health"] = fallbackStatus
		status, output, evaluatorUsage := runLongMemEvalEvaluator(ctx, report.BenchmarkDir, options.LongMemEvalPredictions,
			datasetPath, evaluatorConfig)
		recordLongMemEvalEvaluatorUsage(options, evaluatorUsage)
		report.StageTokenUsage = aggregateLongMemEvalEvaluatorUsage(evaluatorUsage)
		report.OfficialMetrics["token_usage_path"] = longMemEvalTokenUsagePath(options)
		report.OfficialMetrics["upstream_evaluator_status"] = status
		if strings.TrimSpace(output) != "" {
			report.OfficialMetrics["upstream_evaluator_output"] = output
		}
		if options.LongMemEvalSmokeSize > 0 {
			gate := assessLongMemEvalSmoke(status, output, selected)
			gatePath := filepath.Join(options.OutputDir, "longmemeval-smoke-gate.json")
			if err := writeJSONAtomic(gatePath, gate, 0o600); err != nil {
				return report, fmt.Errorf("write LongMemEval smoke gate: %w", err)
			}
			report.OfficialMetrics["smoke_gate"] = gate
			report.OfficialMetrics["smoke_gate_path"] = gatePath
			report.OfficialMetrics["full_benchmark_allowed"] = gate.FullBenchmarkAllowed
		}
		return report, nil
	default:
		return Report{}, fmt.Errorf("unsupported LongMemEval phase %q", options.LongMemEvalPhase)
	}
}

func useLongMemEvalPrepareResources() func() {
	const name = "LUMINA_MEMORY_EMBEDDING_THREADS"
	previousThreads, hadThreads := os.LookupEnv(name)
	_ = os.Setenv(name, "8")
	previousLimit := debug.SetMemoryLimit(16 << 30)
	return func() {
		debug.SetMemoryLimit(previousLimit)
		if hadThreads {
			_ = os.Setenv(name, previousThreads)
		} else {
			_ = os.Unsetenv(name)
		}
	}
}

func runLongMemEvalAnswerSuite(ctx context.Context, options RunnerOptions, datasetSHA string,
	cases []longMemEvalCase) ([]CaseResult, string, string, string, error) {
	manifests := make([]longMemEvalIndexManifest, 0, len(cases))
	for _, c := range cases {
		id := strings.TrimSpace(c.QuestionID)
		manifest, err := readLongMemEvalIndexManifest(longMemEvalIndexManifestPath(options.LongMemEvalIndexDir, id))
		if err != nil {
			return nil, "", "", "", fmt.Errorf("read prepared index %s: %w", id, err)
		}
		if err := validateLongMemEvalIndexManifest(ctx, manifest, longMemEvalIndexCaseDir(options.LongMemEvalIndexDir, id),
			datasetSHA, longMemEvalHistorySHA(c), c, options.Config); err != nil {
			return nil, "", "", "", err
		}
		manifests = append(manifests, manifest)
	}
	indexDigest, err := aggregateLongMemEvalIndexDigest(manifests)
	if err != nil {
		return nil, "", "", "", err
	}
	answerFingerprint, err := longMemEvalAnswerFingerprint(options, datasetSHA, indexDigest)
	if err != nil {
		return nil, "", "", "", err
	}
	checkpointPath := filepath.Join(options.OutputDir, "checkpoints", "answers", answerFingerprint+".jsonl")
	checkpoint := map[string]CaseResult{}
	if !options.NoResume {
		checkpoint, err = loadMemoryCheckpoint(checkpointPath)
		if err != nil {
			return nil, checkpointPath, answerFingerprint, indexDigest, err
		}
	}
	results := make([]CaseResult, len(cases))
	manifestByID := make(map[string]longMemEvalIndexManifest, len(manifests))
	var pending []longMemEvalJob
	for index, c := range cases {
		id := strings.TrimSpace(c.QuestionID)
		manifestByID[id] = manifests[index]
		if previous, ok := checkpoint[id]; ok && completedLongMemEvalCheckpoint(previous) {
			results[index] = previous
			continue
		}
		pending = append(pending, longMemEvalJob{Index: index, Case: c, ID: id})
	}
	if len(pending) == 0 {
		return compactMemoryResults(results), checkpointPath, answerFingerprint, indexDigest, nil
	}

	runner := options.LongMemEvalAnswerRunner
	if runner == nil {
		runner = dedicatedLongMemEvalAnswerRunner{}
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan longMemEvalJob)
	outputs := make(chan memoryCaseOutput, len(pending))
	workers := minInt(normalizedCaseParallel(options), len(pending))
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for job := range jobs {
				if runCtx.Err() != nil {
					return
				}
				result := runLongMemEvalAnswerCase(runCtx, job.Case, manifestByID[job.ID], answerFingerprint,
					options, runner)
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
	go func() {
		wait.Wait()
		close(outputs)
	}()
	completed := 0
	for output := range outputs {
		completed++
		result := output.Result
		if err := recordLongMemEvalAnswerUsage(options, result, answerFingerprint); err != nil {
			cancel()
			return compactMemoryResults(results), checkpointPath, answerFingerprint, indexDigest, err
		}
		stageUsage, err := loadLongMemEvalStageUsage(longMemEvalTokenUsagePath(options),
			LongMemEvalPhaseAnswer, result.Case.ID, answerFingerprint)
		if err != nil {
			cancel()
			return compactMemoryResults(results), checkpointPath, answerFingerprint, indexDigest, err
		}
		applyLongMemEvalStageUsage(&result, stageUsage)
		results[output.Index] = result
		reportMemoryBenchmarkProgress(SuiteLongMemEval+"-answer", completed, len(pending), result.Case.ID)
		if luminaapi.IsQuotaExhaustedMessage(result.ErrorType) {
			cancel()
			return compactMemoryResults(results), checkpointPath, answerFingerprint, indexDigest,
				fmt.Errorf("API quota exhausted during LongMemEval answer %s: %s", result.Case.ID, result.ErrorType)
		}
		if completedLongMemEvalCheckpoint(result) {
			if err := appendMemoryCheckpoint(checkpointPath, result); err != nil {
				cancel()
				return compactMemoryResults(results), checkpointPath, answerFingerprint, indexDigest, err
			}
		}
	}
	if completed != len(pending) && ctx.Err() == nil {
		return compactMemoryResults(results), checkpointPath, answerFingerprint, indexDigest,
			fmt.Errorf("LongMemEval answer workers stopped after %d/%d cases", completed, len(pending))
	}
	if err := ctx.Err(); err != nil {
		return compactMemoryResults(results), checkpointPath, answerFingerprint, indexDigest, err
	}
	return compactMemoryResults(results), checkpointPath, answerFingerprint, indexDigest, nil
}

func runLongMemEvalAnswerCase(ctx context.Context, c longMemEvalCase, manifest longMemEvalIndexManifest,
	answerFingerprint string, options RunnerOptions, runner LongMemEvalAnswerRunner) CaseResult {
	id := strings.TrimSpace(c.QuestionID)
	spec := CaseSpec{ID: id, Benchmark: SuiteLongMemEval, Prompt: c.Question, TimeoutSeconds: options.TimeoutSeconds}
	start := time.Now()
	timeline := []TimelineEvent{newTimelineEvent(start, start, "case_start", map[string]any{
		"case_id": id, "phase": LongMemEvalPhaseAnswer, "answer_fingerprint": answerFingerprint,
	})}
	caseCtx, cancel := contextWithOptionalTimeout(ctx, time.Duration(options.TimeoutSeconds)*time.Second)
	defer cancel()
	runRoot := filepath.Join(options.WorkDir, "longmemeval-answer-runs", answerFingerprint[:16])
	caseDir := filepath.Join(runRoot, "cases", sanitizeCaseID(id))
	artifactDir := filepath.Join(options.ArtifactsDir, "longmemeval-answers", sanitizeCaseID(options.LongMemEvalRunID),
		sanitizeCaseID(id))
	result := CaseResult{Case: spec, WorkDir: caseDir, ExpectedSatisfied: true}
	setArtifactPaths(&result, artifactDir)
	resumeSidecarBuild := reusableLongMemEvalAnswerWorkDir(caseDir)
	if !resumeSidecarBuild {
		if err := os.RemoveAll(caseDir); err != nil {
			return failMemoryCase(result, artifactDir, start, timeline, "answer_workdir_cleanup_failed: "+err.Error())
		}
	}
	if err := os.RemoveAll(artifactDir); err != nil {
		return failMemoryCase(result, artifactDir, start, timeline, "answer_artifact_cleanup_failed: "+err.Error())
	}
	if err := os.MkdirAll(filepath.Dir(caseDir), 0o755); err != nil {
		return failMemoryCase(result, artifactDir, start, timeline, "answer_workdir_create_failed: "+err.Error())
	}
	preparedFabricDir := longMemEvalFabricDir(longMemEvalIndexCaseDir(options.LongMemEvalIndexDir, id))
	preparedLedger := filepath.Join(preparedFabricDir, "ledger.sqlite")
	preparedIndex := filepath.Join(preparedFabricDir, "index.sqlite")
	preparedSidecar := filepath.Join(preparedFabricDir, "retrieval-bge-m3.sqlite")
	cachedSidecar := longMemEvalRetrievalSidecarCache(options, id)
	workingFabricDir := longMemEvalFabricDir(caseDir)
	workingSidecar := filepath.Join(workingFabricDir, "retrieval-bge-m3.sqlite")
	if !resumeSidecarBuild {
		if err := cloneSQLiteStore(caseCtx, preparedLedger, filepath.Join(workingFabricDir, "ledger.sqlite")); err != nil {
			return failMemoryCase(result, artifactDir, start, timeline, "answer_ledger_clone_failed: "+err.Error())
		}
		if err := cloneSQLiteStore(caseCtx, preparedIndex, filepath.Join(workingFabricDir, "index.sqlite")); err != nil {
			return failMemoryCase(result, artifactDir, start, timeline, "answer_index_clone_failed: "+err.Error())
		}
		for _, source := range []string{cachedSidecar, preparedSidecar} {
			if _, err := os.Stat(source); os.IsNotExist(err) {
				continue
			} else if err != nil {
				return failMemoryCase(result, artifactDir, start, timeline, "answer_sidecar_stat_failed: "+err.Error())
			}
			if err := cloneSQLiteStore(caseCtx, source, workingSidecar); err != nil {
				return failMemoryCase(result, artifactDir, start, timeline, "answer_sidecar_clone_failed: "+err.Error())
			}
			break
		}
	}
	cfg := longMemEvalFabricConfig(options.Config, caseDir, workingFabricDir, manifest.ScopeRoot, true)
	sessionID := fmt.Sprintf("longmemeval-answer:%s:%s:%s:%d", sanitizeCaseID(options.LongMemEvalRunID),
		sanitizeCaseID(id), answerFingerprint[:12], start.UnixNano())
	timeline = append(timeline, newTimelineEvent(start, time.Now(), "memory_qa_start", map[string]any{
		"prepared_ledger": preparedLedger, "prepared_index": preparedIndex, "query_session_id": sessionID,
		"memory_backend": "fabric", "remote_memory_api": false,
	}))
	agentResult := runLongMemEvalAnswerWithDeadline(caseCtx, runner, cfg, c.Question, sessionID,
		parseLongMemEvalDate(c.QuestionDate))
	result = finishLongMemEvalFabricCase(result, artifactDir, start, timeline, agentResult, c.Answer, preparedLedger)
	if _, err := os.Stat(workingSidecar); err == nil {
		if _, checkpointErr := storePathCheckpoint(caseCtx, workingSidecar); checkpointErr != nil && result.ErrorType == "" {
			result.ErrorType = "answer_sidecar_checkpoint_failed: " + checkpointErr.Error()
		} else if publishErr := publishLongMemEvalRetrievalSidecar(caseCtx, workingSidecar, cachedSidecar); publishErr != nil && result.ErrorType == "" {
			result.ErrorType = "answer_sidecar_cache_failed: " + publishErr.Error()
		}
	} else if !os.IsNotExist(err) && result.ErrorType == "" {
		result.ErrorType = "answer_sidecar_stat_failed: " + err.Error()
	}
	if result.ErrorType == "" {
		if err := os.RemoveAll(caseDir); err != nil {
			result.ErrorType = "answer_workdir_cleanup_failed: " + err.Error()
		}
	}
	return result
}

func reusableLongMemEvalAnswerWorkDir(caseDir string) bool {
	fabricDir := longMemEvalFabricDir(caseDir)
	for _, path := range []string{
		filepath.Join(fabricDir, "ledger.sqlite"),
		filepath.Join(fabricDir, "index.sqlite"),
		filepath.Join(fabricDir, "retrieval-bge-m3.sqlite.staging"),
	} {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			return false
		}
	}
	return true
}

func longMemEvalRetrievalSidecarCache(options RunnerOptions, questionID string) string {
	return filepath.Join(options.LongMemEvalIndexDir, ".retrieval-sidecars", sanitizeCaseID(questionID),
		"retrieval-bge-m3.sqlite")
}

func publishLongMemEvalRetrievalSidecar(ctx context.Context, source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return err
	}
	temporary := destination + fmt.Sprintf(".tmp-%d", time.Now().UnixNano())
	defer os.Remove(temporary)
	if err := cloneSQLiteStore(ctx, source, temporary); err != nil {
		return err
	}
	return os.Rename(temporary, destination)
}

func finishLongMemEvalFabricCase(result CaseResult, artifactDir string, start time.Time,
	timeline []TimelineEvent, agentResult AgentRunResult, expected any, preparedLedger string) CaseResult {
	timeline = append(timeline, agentResult.Timeline...)
	result.TTFTMillis = agentResult.TTFTMillis
	result.FirstToolCallMS = agentResult.FirstToolCallMS
	result.InputTokens = agentResult.InputTokens
	result.CacheReadInputTokens = agentResult.CacheReadInputTokens
	result.CacheCreationInputTokens = agentResult.CacheCreationInputTokens
	result.OutputTokens = agentResult.OutputTokens
	result.ToolCalls = agentResult.ToolCalls
	result.ErrorType = agentResult.ErrorType
	result.Hypothesis = strings.TrimSpace(agentResult.FinalText)
	result.ExpectedAnswer = expected
	result.MemoryStorePath = preparedLedger
	result.AnswerMatch = answerContainsExpected(result.Hypothesis, expected)
	result.ExpectedSatisfied = false
	result.Resolved = false
	result.DurationSeconds = time.Since(start).Seconds()
	result.Timeline = append(timeline, newTimelineEvent(start, time.Now(), "validation_end", map[string]any{
		"upstream_evaluation_pending": true,
	}))
	writeCaseArtifacts(artifactDir, result.Case, agentResult, result)
	return result
}

func runLongMemEvalAnswerWithDeadline(ctx context.Context, runner LongMemEvalAnswerRunner, cfg config.Config,
	question, sessionID string, queryTime time.Time) AgentRunResult {
	done := make(chan AgentRunResult, 1)
	go func() { done <- runner.RunAnswer(ctx, cfg, question, sessionID, queryTime) }()
	select {
	case result := <-done:
		return result
	case <-ctx.Done():
		return AgentRunResult{ErrorType: "answer_timeout: " + ctx.Err().Error()}
	}
}

func longMemEvalAnswerFingerprint(options RunnerOptions, datasetSHA, indexDigest string) (string, error) {
	input := longMemEvalAnswerFingerprintInput{
		DatasetSHA256: datasetSHA, IndexSHA256: indexDigest, RunID: options.LongMemEvalRunID,
		PromptIdentity: longMemEvalAnswerPromptIdentity, AnswerModel: options.Config.APIModel,
		FallbackEnabled: options.Config.FallbackAPIEnabled, FallbackModel: options.Config.FallbackAPIModel,
		CodeIdentity: longMemEvalCodeIdentity(), MemoryBackend: "fabric",
		EmbeddingModel:  options.Config.MemoryEmbeddingModel,
		RecallMaxItems:  options.Config.MemoryRecallMaxItems,
		ContextTokens:   minPositive(options.Config.MemoryContextTargetTokens, 2500),
		LatencyBudgetMS: options.Config.MemorySearchLatencyMS,
	}
	data, err := json.Marshal(input)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func checkLongMemEvalFallbackHealth(ctx context.Context, cfg config.Config) (config.Config, string) {
	if !cfg.FallbackAPIEnabled {
		return cfg, "disabled"
	}
	if strings.TrimSpace(cfg.FallbackAPIKey) == "" || strings.TrimSpace(cfg.FallbackAPIBaseURL) == "" ||
		strings.TrimSpace(cfg.FallbackAPIModel) == "" {
		cfg.FallbackAPIEnabled = false
		return cfg, "disabled_invalid_config"
	}
	healthConfig := cfg
	healthConfig.APIKey = cfg.FallbackAPIKey
	healthConfig.APIBaseURL = cfg.FallbackAPIBaseURL
	healthConfig.APIModel = cfg.FallbackAPIModel
	healthConfig.APIType = cfg.FallbackAPIType
	healthConfig.FallbackAPIEnabled = false
	retry := luminaapi.DefaultRetryConfigPtr()
	retry.MaxRetries = 0
	client, err := agent.CreateConfiguredLLMClient(healthConfig, healthConfig.APIModel, 4, nil, retry)
	if err != nil {
		cfg.FallbackAPIEnabled = false
		return cfg, "disabled_client_error"
	}
	checkCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	text, err := client.Complete(checkCtx, "Reply with OK only.", []map[string]any{{"role": "user", "content": "OK"}},
		luminaapi.CompleteOptions{MaxTokens: 4})
	if err != nil {
		cfg.FallbackAPIEnabled = false
		if luminaapi.IsQuotaExhaustedError(err) || luminaapi.IsQuotaExhaustedMessage(err.Error()) {
			return cfg, "disabled_api_quota_exhausted"
		}
		return cfg, "disabled_unhealthy"
	}
	if strings.TrimSpace(text) == "" {
		cfg.FallbackAPIEnabled = false
		return cfg, "disabled_empty_health_response"
	}
	return cfg, "healthy"
}

func longMemEvalCodeIdentity() string {
	if compatible := strings.TrimSpace(os.Getenv("LUMINA_LONGMEMEVAL_COMPATIBLE_CODE_IDENTITY")); compatible != "" &&
		strings.HasPrefix(compatible, longMemEvalCodeBaseIdentity+":") {
		return compatible
	}
	identity := longMemEvalCodeBaseIdentity
	modifiedBuild := true
	if info, ok := debug.ReadBuildInfo(); ok {
		var revision, modified string
		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = setting.Value
			case "vcs.modified":
				modified = setting.Value
			}
		}
		if revision != "" {
			identity += ":" + revision
		}
		if modified == "true" {
			identity += ":modified"
		} else if revision != "" {
			modifiedBuild = false
		}
	}
	if modifiedBuild {
		if executable, err := os.Executable(); err == nil {
			if file, err := os.Open(executable); err == nil {
				hash := sha256.New()
				if _, err := io.Copy(hash, file); err == nil {
					identity += ":binary-" + hex.EncodeToString(hash.Sum(nil))[:16]
				}
				_ = file.Close()
			}
		}
	}
	return identity
}

func writeLongMemEvalAnswerPredictions(options RunnerOptions, answerFingerprint string,
	cases []longMemEvalCase, results []CaseResult) (string, error) {
	if err := validateLongMemEvalResultSet(results, longMemEvalExpectedIDs(cases)); err != nil {
		return "", err
	}
	path := filepath.Join(options.OutputDir, "predictions", "longmemeval-"+
		sanitizeCaseID(options.LongMemEvalRunID)+"-"+answerFingerprint[:16]+".jsonl")
	ordered := make(map[string]CaseResult, len(results))
	for _, result := range results {
		ordered[result.Case.ID] = result
	}
	var content bytes.Buffer
	encoder := json.NewEncoder(&content)
	for _, c := range cases {
		result := ordered[c.QuestionID]
		if err := encoder.Encode(longMemEvalPrediction{QuestionID: c.QuestionID, Hypothesis: result.Hypothesis}); err != nil {
			return path, err
		}
	}
	if existing, err := os.ReadFile(path); err == nil {
		if bytes.Equal(existing, content.Bytes()) {
			return path, nil
		}
		return path, fmt.Errorf("refusing to overwrite existing LongMemEval predictions: %s", path)
	} else if !os.IsNotExist(err) {
		return path, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return path, err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, content.Bytes(), 0o644); err != nil {
		return path, err
	}
	if err := os.Rename(temporary, path); err != nil {
		return path, err
	}
	return path, nil
}

func validateLongMemEvalResultSet(results []CaseResult, expected map[string]struct{}) error {
	if err := validateLongMemEvalPredictions(results); err != nil {
		return err
	}
	seen := make(map[string]struct{}, len(results))
	for _, result := range results {
		seen[strings.TrimSpace(result.Case.ID)] = struct{}{}
	}
	missing, unexpected := differenceLongMemEvalIDs(expected, seen), differenceLongMemEvalIDs(seen, expected)
	if len(missing) != 0 || len(unexpected) != 0 {
		return fmt.Errorf("longmemeval prediction ID set mismatch: expected=%d actual=%d missing=%s unexpected=%s",
			len(expected), len(seen), summarizeLongMemEvalIDs(missing), summarizeLongMemEvalIDs(unexpected))
	}
	return nil
}

func validateLongMemEvalPredictionFile(path string, cases []longMemEvalCase) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	expected := longMemEvalExpectedIDs(cases)
	seen := map[string]struct{}{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 1024*1024), 32*1024*1024)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" {
			return fmt.Errorf("LongMemEval prediction line %d is empty", line)
		}
		var prediction longMemEvalPrediction
		if err := json.Unmarshal([]byte(text), &prediction); err != nil {
			return fmt.Errorf("LongMemEval prediction line %d: %w", line, err)
		}
		prediction.QuestionID = strings.TrimSpace(prediction.QuestionID)
		if prediction.QuestionID == "" || strings.TrimSpace(prediction.Hypothesis) == "" {
			return fmt.Errorf("LongMemEval prediction line %d has empty question_id or hypothesis", line)
		}
		if _, duplicate := seen[prediction.QuestionID]; duplicate {
			return fmt.Errorf("LongMemEval prediction has duplicate question_id %s", prediction.QuestionID)
		}
		seen[prediction.QuestionID] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	missing, unexpected := differenceLongMemEvalIDs(expected, seen), differenceLongMemEvalIDs(seen, expected)
	if len(seen) != len(expected) || len(missing) != 0 || len(unexpected) != 0 {
		return fmt.Errorf("LongMemEval prediction file is incomplete: rows=%d expected=%d missing=%s unexpected=%s",
			len(seen), len(expected), summarizeLongMemEvalIDs(missing), summarizeLongMemEvalIDs(unexpected))
	}
	return nil
}

func longMemEvalExpectedIDs(cases []longMemEvalCase) map[string]struct{} {
	ids := make(map[string]struct{}, len(cases))
	for _, c := range cases {
		if id := strings.TrimSpace(c.QuestionID); id != "" {
			ids[id] = struct{}{}
		}
	}
	return ids
}

func differenceLongMemEvalIDs(left, right map[string]struct{}) []string {
	var result []string
	for id := range left {
		if _, ok := right[id]; !ok {
			result = append(result, id)
		}
	}
	sort.Strings(result)
	return result
}

func summarizeLongMemEvalIDs(ids []string) string {
	if len(ids) == 0 {
		return "[]"
	}
	limit := minInt(8, len(ids))
	result := append([]string(nil), ids[:limit]...)
	if len(ids) > limit {
		result = append(result, fmt.Sprintf("...(+%d)", len(ids)-limit))
	}
	return "[" + strings.Join(result, ",") + "]"
}
