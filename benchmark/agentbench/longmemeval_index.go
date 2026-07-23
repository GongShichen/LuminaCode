package agentbench

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
	"LuminaCode/memory"
)

const (
	longMemEvalPreparedIndexFormat  = "longmemeval-memory-index"
	longMemEvalMemorySchemaRevision = "ledger-event-fts-session-window-index"
	longMemEvalCompilerSchema       = "semantic-compiler-planned-session-artifacts"
	longMemEvalRetrievalPipeline    = "memory-fabric"
	longMemEvalCodeBaseIdentity     = "agentbench-longmemeval-fabric"
	longMemEvalStructuredRepair     = "colon-array-delimiter"
	longMemEvalEmptyOutputRepair    = "semantic-skip-empty-output"
)

type longMemEvalIndexCounts struct {
	Events          int `json:"events"`
	Contexts        int `json:"contexts"`
	Identities      int `json:"identities"`
	Aliases         int `json:"aliases"`
	Nodes           int `json:"nodes"`
	Slots           int `json:"slots"`
	Conflicts       int `json:"conflicts"`
	Resolutions     int `json:"resolutions"`
	Documents       int `json:"documents"`
	FTSDocuments    int `json:"fts_documents"`
	KeyPostings     int `json:"key_postings"`
	ActiveSlots     int `json:"active_slots"`
	VectorRows      int `json:"vector_rows"`
	VectorDocuments int `json:"vector_documents"`
}

type longMemEvalIndexIntegrity struct {
	LedgerQuickCheck     string `json:"ledger_quick_check"`
	IndexQuickCheck      string `json:"index_quick_check"`
	ExpectedEvents       int    `json:"expected_events"`
	ActualEvents         int    `json:"actual_events"`
	ExpectedContexts     int    `json:"expected_contexts"`
	ActualContexts       int    `json:"actual_contexts"`
	PendingSemanticEvent int    `json:"pending_semantic_events"`
	PendingJobs          int    `json:"pending_jobs"`
	FailedJobs           int    `json:"failed_jobs"`
	PendingOutbox        int    `json:"pending_outbox"`
	MaxOutboxSeq         int64  `json:"max_outbox_seq"`
	IndexedLedgerSeq     int64  `json:"indexed_ledger_seq"`
	IndexGeneration      int64  `json:"index_generation"`
	FTSReady             bool   `json:"fts_ready"`
	VectorReady          bool   `json:"vector_ready"`
}

type longMemEvalIndexManifest struct {
	Format              string                    `json:"format"`
	Status              string                    `json:"status"`
	Backend             string                    `json:"backend"`
	DatasetSHA256       string                    `json:"dataset_sha256"`
	HistorySHA256       string                    `json:"history_sha256"`
	QuestionID          string                    `json:"question_id"`
	ScopeRoot           string                    `json:"scope_root"`
	Space               string                    `json:"space"`
	MemorySchema        string                    `json:"memory_schema"`
	RetrievalPipeline   string                    `json:"retrieval_pipeline"`
	CompilerSchema      string                    `json:"compiler_schema"`
	CompilerModel       string                    `json:"compiler_model,omitempty"`
	RemoteProcessing    string                    `json:"remote_processing"`
	CompileBatchTokens  int                       `json:"compile_batch_tokens"`
	EmbeddingModel      string                    `json:"embedding_model,omitempty"`
	EmbeddingDimensions int                       `json:"embedding_dimensions,omitempty"`
	LedgerSHA256        string                    `json:"ledger_sha256"`
	IndexSHA256         string                    `json:"index_sha256"`
	Counts              longMemEvalIndexCounts    `json:"counts"`
	Integrity           longMemEvalIndexIntegrity `json:"integrity"`
	CreatedAt           string                    `json:"created_at"`
	ReadyAt             string                    `json:"ready_at,omitempty"`
}

type longMemEvalIndexCheckpointRecord struct {
	QuestionID     string `json:"question_id"`
	ManifestSHA256 string `json:"manifest_sha256"`
	Status         string `json:"status"`
	CompletedAt    string `json:"completed_at"`
}

type longMemEvalPrepareTimingRecord struct {
	Phase           string  `json:"phase"`
	CaseID          string  `json:"case_id"`
	Stage           string  `json:"stage"`
	DurationSeconds float64 `json:"duration_seconds"`
	Error           string  `json:"error,omitempty"`
	RecordedAt      string  `json:"recorded_at"`
}

var longMemEvalPrepareTimingMu sync.Mutex

func recordLongMemEvalPrepareTiming(options RunnerOptions, caseID, stage string, started time.Time, err error) {
	record := longMemEvalPrepareTimingRecord{Phase: LongMemEvalPhasePrepare, CaseID: caseID, Stage: stage,
		DurationSeconds: time.Since(started).Seconds(), RecordedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err != nil {
		record.Error = err.Error()
	}
	longMemEvalPrepareTimingMu.Lock()
	defer longMemEvalPrepareTimingMu.Unlock()
	_ = appendJSONLine(filepath.Join(options.OutputDir, "prepare-timing.jsonl"), record)
}

func normalizeLongMemEvalOptions(options RunnerOptions) RunnerOptions {
	options.LongMemEvalPhase = strings.ToLower(strings.TrimSpace(options.LongMemEvalPhase))
	options.Config.LongTermMemoryEnabled = true
	options.Config.MemoryBackend = "fabric"
	if options.LongMemEvalIndexDir == "" {
		options.LongMemEvalIndexDir = filepath.Join(options.WorkDir, longMemEvalPreparedIndexFormat)
	}
	if options.LongMemEvalIndexSource == "" {
		options.LongMemEvalIndexSource = filepath.Join(options.WorkDir, "cases")
	}
	options.LongMemEvalIndexDir = absoluteLongMemEvalPath(options.LongMemEvalIndexDir)
	options.LongMemEvalIndexSource = absoluteLongMemEvalPath(options.LongMemEvalIndexSource)
	options.LongMemEvalPredictions = absoluteLongMemEvalPath(options.LongMemEvalPredictions)
	return options
}

func absoluteLongMemEvalPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" || filepath.IsAbs(path) {
		return path
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	return absolute
}

func validateLongMemEvalPhaseOptions(options RunnerOptions) error {
	if options.LongMemEvalSmokeSize < 0 {
		return errors.New("longmemeval smoke size cannot be negative")
	}
	if options.LongMemEvalSmokeSize > 0 && (strings.TrimSpace(options.CaseID) != "" || options.Limit > 0) {
		return errors.New("longmemeval stratified smoke cannot be combined with -case or -limit")
	}
	if strings.TrimSpace(options.LongMemEvalQuestionType) != "" && options.LongMemEvalSmokeSize <= 0 {
		return errors.New("longmemeval question-type filter requires -longmemeval-smoke-size")
	}
	switch options.LongMemEvalPhase {
	case LongMemEvalPhasePrepare:
		if strings.TrimSpace(options.LongMemEvalPredictions) != "" {
			return errors.New("longmemeval prepare phase does not accept -longmemeval-predictions")
		}
	case LongMemEvalPhaseAnswer:
		if strings.TrimSpace(options.LongMemEvalRunID) == "" {
			return errors.New("longmemeval answer phase requires -longmemeval-run-id")
		}
		if strings.TrimSpace(options.LongMemEvalPredictions) != "" {
			return errors.New("longmemeval answer phase does not accept -longmemeval-predictions")
		}
	case LongMemEvalPhaseEvaluate:
		if strings.TrimSpace(options.LongMemEvalPredictions) == "" {
			return errors.New("longmemeval evaluate phase requires -longmemeval-predictions")
		}
		if options.LongMemEvalSmokeSize == 0 && (options.CaseID != "" || options.Limit > 0) {
			return errors.New("longmemeval evaluate phase requires the complete dataset; -case and -limit are not allowed")
		}
	default:
		return errors.New("longmemeval requires explicit -longmemeval-phase prepare|answer|evaluate")
	}
	return nil
}

func loadLongMemEvalDataset(options RunnerOptions) (string, []byte, []longMemEvalCase, error) {
	path := strings.TrimSpace(options.CasesPath)
	if path == "" {
		path = filepath.Join(options.RootDir, "longmemeval", "repo", "data", "longmemeval_s_cleaned.json")
	}
	path = absoluteLongMemEvalPath(path)
	if strings.Contains(strings.ToLower(filepath.Base(path)), "oracle") {
		return path, nil, nil, fmt.Errorf("oracle LongMemEval dataset is forbidden: %s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return path, nil, nil, err
	}
	var cases []longMemEvalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		return path, data, nil, err
	}
	return path, data, cases, nil
}

func selectedLongMemEvalCases(cases []longMemEvalCase, options RunnerOptions) []longMemEvalCase {
	if questionType := strings.TrimSpace(options.LongMemEvalQuestionType); questionType != "" {
		filtered := make([]longMemEvalCase, 0, len(cases))
		for _, item := range cases {
			if strings.TrimSpace(item.QuestionType) == questionType {
				filtered = append(filtered, item)
			}
		}
		cases = filtered
	}
	if options.LongMemEvalSmokeSize > 0 {
		return stratifiedLongMemEvalCases(cases, options.LongMemEvalSmokeSize)
	}
	if options.CaseID != "" {
		cases = filterLongMemEvalCases(cases, options.CaseID)
	}
	if options.Limit > 0 && options.Limit < len(cases) {
		cases = cases[:options.Limit]
	}
	return cases
}

func stratifiedLongMemEvalCases(cases []longMemEvalCase, size int) []longMemEvalCase {
	if size <= 0 || size >= len(cases) {
		return append([]longMemEvalCase(nil), cases...)
	}
	type candidate struct {
		index int
		item  longMemEvalCase
		score string
	}
	groups := map[string][]candidate{}
	for index, item := range cases {
		questionType := strings.TrimSpace(item.QuestionType)
		if questionType == "" {
			questionType = "unknown"
		}
		sum := sha256.Sum256([]byte("longmemeval-stratified-smoke\x00" + questionType + "\x00" +
			strings.TrimSpace(item.QuestionID)))
		groups[questionType] = append(groups[questionType], candidate{index: index, item: item,
			score: hex.EncodeToString(sum[:])})
	}
	types := make([]string, 0, len(groups))
	for questionType := range groups {
		types = append(types, questionType)
		sort.SliceStable(groups[questionType], func(i, j int) bool {
			return groups[questionType][i].score < groups[questionType][j].score
		})
	}
	sort.Strings(types)
	if len(types) == 0 {
		return nil
	}

	quota := make(map[string]int, len(types))
	base := size / len(types)
	for _, questionType := range types {
		quota[questionType] = minInt(base, len(groups[questionType]))
	}
	assigned := 0
	for _, count := range quota {
		assigned += count
	}
	for assigned < size {
		eligible := append([]string(nil), types...)
		sort.SliceStable(eligible, func(i, j int) bool {
			remainingI := len(groups[eligible[i]]) - quota[eligible[i]]
			remainingJ := len(groups[eligible[j]]) - quota[eligible[j]]
			if remainingI != remainingJ {
				return remainingI > remainingJ
			}
			return eligible[i] < eligible[j]
		})
		progressed := false
		for _, questionType := range eligible {
			if assigned >= size {
				break
			}
			if quota[questionType] >= len(groups[questionType]) {
				continue
			}
			quota[questionType]++
			assigned++
			progressed = true
		}
		if !progressed {
			break
		}
	}

	selected := make([]candidate, 0, assigned)
	for _, questionType := range types {
		selected = append(selected, groups[questionType][:quota[questionType]]...)
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].index < selected[j].index })
	result := make([]longMemEvalCase, 0, len(selected))
	for _, item := range selected {
		result = append(result, item.item)
	}
	return result
}

func longMemEvalDatasetSHA(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func longMemEvalHistorySHA(c longMemEvalCase) string {
	type turn struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	}
	type session struct {
		ID    string `json:"id"`
		Date  string `json:"date"`
		Turns []turn `json:"turns"`
	}
	payload := make([]session, 0, len(c.HaystackSessions))
	for index, source := range c.HaystackSessions {
		item := session{ID: stringAt(c.HaystackSessionIDs, index, fmt.Sprintf("session-%d", index+1)),
			Date: stringAt(c.HaystackDates, index, "")}
		for _, sourceTurn := range source {
			item.Turns = append(item.Turns, turn{Role: longMemEvalTurnString(sourceTurn, "role"),
				Content: longMemEvalTurnString(sourceTurn, "content")})
		}
		payload = append(payload, item)
	}
	encoded, _ := json.Marshal(payload)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func longMemEvalIndexCaseDir(indexDir, questionID string) string {
	return filepath.Join(indexDir, "cases", sanitizeCaseID(questionID))
}

func longMemEvalIndexManifestPath(indexDir, questionID string) string {
	return filepath.Join(longMemEvalIndexCaseDir(indexDir, questionID), "index-manifest.json")
}

func longMemEvalFabricDir(caseDir string) string {
	return filepath.Join(caseDir, "data", "memory", "fabric")
}

func longMemEvalIndexStorePath(indexDir, questionID string) string {
	return filepath.Join(longMemEvalFabricDir(longMemEvalIndexCaseDir(indexDir, questionID)), "ledger.sqlite")
}

func longMemEvalIndexIndexPath(indexDir, questionID string) string {
	return filepath.Join(longMemEvalFabricDir(longMemEvalIndexCaseDir(indexDir, questionID)), "index.sqlite")
}

func prepareLongMemEvalIndexes(ctx context.Context, options RunnerOptions, datasetPath string, datasetData []byte,
	cases []longMemEvalCase) ([]CaseResult, string, error) {
	datasetSHA := longMemEvalDatasetSHA(datasetData)
	checkpointPath := filepath.Join(options.OutputDir, "checkpoints", "index", datasetSHA+".jsonl")
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	pipeline, err := startLongMemEvalSemanticPrewarm(runCtx, options, datasetSHA, cases)
	if err != nil {
		return nil, checkpointPath, err
	}
	dependencies, err := newLongMemEvalSemanticDependencyTracker(cases, pipeline.CaseTaskKeys,
		pipeline.ReadyTaskKeys)
	if err != nil {
		return nil, checkpointPath, err
	}
	results := make([]CaseResult, len(cases))
	type prepareOutput struct {
		index    int
		result   CaseResult
		manifest longMemEvalIndexManifest
		err      error
	}
	jobs := make(chan longMemEvalJob, len(cases))
	outputs := make(chan prepareOutput, len(cases))
	workers := minInt(normalizedCaseParallel(options), len(cases))
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for job := range jobs {
				if runCtx.Err() != nil {
					return
				}
				started := time.Now()
				result := CaseResult{Case: CaseSpec{ID: job.ID, Benchmark: SuiteLongMemEval},
					WorkDir: options.LongMemEvalIndexDir}
				manifest, err := prepareLongMemEvalIndexCase(runCtx, options, datasetSHA, job.Case)
				result.DurationSeconds = time.Since(started).Seconds()
				stageUsage, usageErr := loadLongMemEvalStageUsage(longMemEvalTokenUsagePath(options),
					LongMemEvalPhasePrepare, job.ID, "")
				if usageErr != nil {
					result.ErrorType = "token_usage_read_failed: " + usageErr.Error()
					err = errors.Join(err, usageErr)
				} else {
					applyLongMemEvalStageUsage(&result, stageUsage)
				}
				if err != nil && result.ErrorType == "" {
					result.ErrorType = "index_prepare_failed: " + err.Error()
				}
				if err == nil {
					result.Resolved, result.TestPassRate, result.ExpectedSatisfied = true, 1, true
					result.MemoryStorePath = longMemEvalIndexStorePath(options.LongMemEvalIndexDir, job.ID)
				}
				outputs <- prepareOutput{index: job.Index, result: result, manifest: manifest, err: err}
			}
		}()
	}
	go func() {
		wait.Wait()
		close(outputs)
	}()
	queued := 0
	queueReadyCases := func(indexes []int) {
		for _, index := range indexes {
			c := cases[index]
			id := firstNonEmptyString(c.QuestionID, "longmemeval-case")
			jobs <- longMemEvalJob{Index: index, Case: c, ID: id}
			queued++
		}
	}
	queueReadyCases(dependencies.ReleaseReadyCases())
	jobsClosed := false
	closeJobs := func() {
		if !jobsClosed {
			close(jobs)
			jobsClosed = true
		}
	}
	if queued == len(cases) {
		closeJobs()
	}
	completed := 0
	var firstErr error
	progress := pipeline.Progress
	outputStream := (<-chan prepareOutput)(outputs)
	for progress != nil || outputStream != nil {
		select {
		case update, ok := <-progress:
			if !ok {
				progress = nil
				if firstErr == nil && queued != len(cases) {
					unreleased := dependencies.UnreleasedCaseIDs()
					firstErr = fmt.Errorf("semantic prewarm ended with %d/%d cases released; waiting cases: %s",
						queued, len(cases), strings.Join(unreleased, ", "))
					cancel()
					closeJobs()
				}
				continue
			}
			if update.Err != nil {
				if firstErr == nil {
					firstErr = update.Err
					cancel()
					closeJobs()
				}
				continue
			}
			if firstErr == nil {
				queueReadyCases(dependencies.MarkTasksReady(update.TaskKeys))
				if queued == len(cases) {
					closeJobs()
				}
			}
		case output, ok := <-outputStream:
			if !ok {
				outputStream = nil
				continue
			}
			results[output.index] = output.result
			completed++
			reportMemoryBenchmarkProgress(SuiteLongMemEval+"-prepare", completed, len(cases), output.result.Case.ID)
			if output.err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("prepare LongMemEval index %s: %w", output.result.Case.ID, output.err)
					cancel()
					closeJobs()
				}
				continue
			}
			digest, _ := longMemEvalManifestDigest(output.manifest)
			record := longMemEvalIndexCheckpointRecord{QuestionID: output.result.Case.ID, ManifestSHA256: digest,
				Status: "ready", CompletedAt: time.Now().UTC().Format(time.RFC3339Nano)}
			if err := appendJSONLine(checkpointPath, record); err != nil && firstErr == nil {
				firstErr = err
				cancel()
				closeJobs()
			}
		}
	}
	closeJobs()
	if firstErr != nil {
		return compactMemoryResults(results), checkpointPath, firstErr
	}
	if completed != len(cases) {
		return compactMemoryResults(results), checkpointPath,
			fmt.Errorf("LongMemEval prepare workers stopped after %d/%d cases", completed, len(cases))
	}
	_ = datasetPath
	return compactMemoryResults(results), checkpointPath, nil
}

func prepareLongMemEvalIndexCase(ctx context.Context, options RunnerOptions, datasetSHA string,
	c longMemEvalCase) (longMemEvalIndexManifest, error) {
	id := firstNonEmptyString(c.QuestionID, "longmemeval-case")
	historySHA := longMemEvalHistorySHA(c)
	finalDir := longMemEvalIndexCaseDir(options.LongMemEvalIndexDir, id)
	if manifest, err := readLongMemEvalIndexManifest(filepath.Join(finalDir, "index-manifest.json")); err == nil {
		if err := validateLongMemEvalIndexManifest(ctx, manifest, finalDir, datasetSHA, historySHA, c, options.Config); err != nil {
			return manifest, err
		}
		return manifest, nil
	} else if !os.IsNotExist(err) {
		return longMemEvalIndexManifest{}, err
	}

	scopeRoot := longMemEvalFabricScopeRoot(options, id)
	stagingDir := finalDir + ".preparing"
	stagingFabricDir := longMemEvalFabricDir(stagingDir)
	stagingManifestPath := filepath.Join(stagingDir, "index-manifest.json")
	cfg := longMemEvalFabricConfig(options.Config, stagingDir, stagingFabricDir, scopeRoot, false)
	manifest := longMemEvalIndexManifest{
		Format: longMemEvalPreparedIndexFormat, Status: "preparing", Backend: "fabric",
		DatasetSHA256: datasetSHA, HistorySHA256: historySHA, QuestionID: id, ScopeRoot: scopeRoot,
		Space: agent.MemoryFabricSpace(cfg), MemorySchema: longMemEvalMemorySchemaRevision,
		RetrievalPipeline: longMemEvalRetrievalPipeline, CompilerSchema: longMemEvalCompilerSchema,
		CompilerModel: longMemEvalCompilerModel(cfg), RemoteProcessing: cfg.MemoryRemoteProcessing,
		CompileBatchTokens: minInt(10_000, cfg.MemoryCompileBatchTokens),
		EmbeddingModel:     longMemEvalEmbeddingModel(cfg), CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := os.MkdirAll(stagingFabricDir, 0o700); err != nil {
		return manifest, err
	}
	if staged, err := readLongMemEvalIndexManifest(stagingManifestPath); err == nil {
		if staged.DatasetSHA256 != datasetSHA || staged.HistorySHA256 != historySHA || staged.QuestionID != id ||
			staged.Format != longMemEvalPreparedIndexFormat || staged.Space != manifest.Space ||
			staged.ScopeRoot != manifest.ScopeRoot || staged.RemoteProcessing != manifest.RemoteProcessing ||
			staged.MemorySchema != manifest.MemorySchema || staged.CompilerSchema != manifest.CompilerSchema ||
			staged.RetrievalPipeline != manifest.RetrievalPipeline ||
			staged.CompilerModel != manifest.CompilerModel || staged.EmbeddingModel != manifest.EmbeddingModel ||
			staged.CompileBatchTokens != manifest.CompileBatchTokens {
			return manifest, fmt.Errorf("staging index fingerprint mismatch for %s", id)
		}
		manifest = staged
	} else if os.IsNotExist(err) {
		if err := writeJSONAtomic(stagingManifestPath, manifest, 0o600); err != nil {
			return manifest, err
		}
	} else {
		return manifest, err
	}

	stageStarted := time.Now()
	if _, err := recoverRepairableLongMemEvalCompilerJobs(ctx, filepath.Join(stagingFabricDir, "ledger.sqlite")); err != nil {
		recordLongMemEvalPrepareTiming(options, id, "recover_jobs", stageStarted, err)
		return manifest, err
	}
	recordLongMemEvalPrepareTiming(options, id, "recover_jobs", stageStarted, nil)
	stageStarted = time.Now()
	fabric, err := agent.OpenConfiguredMemoryFabricWithUsageObserver(ctx, cfg, false,
		longMemEvalUsageObserver(options, id))
	recordLongMemEvalPrepareTiming(options, id, "open", stageStarted, err)
	if err != nil {
		return manifest, err
	}
	if fabric == nil {
		return manifest, errors.New("Fabric is disabled for LongMemEval prepare")
	}
	stageStarted = time.Now()
	expectedEvents, expectedContexts, ingestErr := ingestLongMemEvalFabricHistory(ctx, fabric, cfg, c)
	recordLongMemEvalPrepareTiming(options, id, "ingest_plan", stageStarted, ingestErr)
	if ingestErr == nil {
		stageStarted = time.Now()
		ingestErr = fabric.Flush(ctx)
		recordLongMemEvalPrepareTiming(options, id, "flush", stageStarted, ingestErr)
	}
	if ingestErr == nil && !strings.EqualFold(strings.TrimSpace(cfg.MemoryRemoteProcessing), "off") {
		stageStarted = time.Now()
		_, ingestErr = fabric.ResolvePendingConflicts(ctx, agent.MemoryFabricSpace(cfg), 8)
		recordLongMemEvalPrepareTiming(options, id, "resolve_conflicts", stageStarted, ingestErr)
	}
	if ingestErr == nil {
		stageStarted = time.Now()
		ingestErr = fabric.Flush(ctx)
		recordLongMemEvalPrepareTiming(options, id, "final_flush", stageStarted, ingestErr)
	}
	if ingestErr == nil && fabric.RetrievalSidecarEnabled() {
		stageStarted = time.Now()
		ingestErr = fabric.SyncRetrievalSidecar(ctx)
		recordLongMemEvalPrepareTiming(options, id, "retrieval_sidecar", stageStarted, ingestErr)
	}
	stageStarted = time.Now()
	health, doctorErr := fabric.Doctor(ctx)
	closeErr := fabric.Close()
	doctorCloseErr := errors.Join(doctorErr, closeErr)
	recordLongMemEvalPrepareTiming(options, id, "doctor_close", stageStarted, doctorCloseErr)
	if ingestErr != nil {
		return manifest, ingestErr
	}
	if doctorErr != nil {
		return manifest, doctorErr
	}
	if closeErr != nil {
		return manifest, closeErr
	}
	if !health.Healthy {
		return manifest, fmt.Errorf("Fabric health check failed: %+v", health)
	}
	ledgerPath := filepath.Join(stagingFabricDir, "ledger.sqlite")
	indexPath := filepath.Join(stagingFabricDir, "index.sqlite")
	sidecarPath := filepath.Join(stagingFabricDir, "retrieval-bge-m3.sqlite")
	stageStarted = time.Now()
	if _, err := storePathCheckpoint(ctx, ledgerPath); err != nil {
		recordLongMemEvalPrepareTiming(options, id, "checkpoint", stageStarted, err)
		return manifest, err
	}
	if _, err := storePathCheckpoint(ctx, indexPath); err != nil {
		recordLongMemEvalPrepareTiming(options, id, "checkpoint", stageStarted, err)
		return manifest, err
	}
	if _, err := os.Stat(sidecarPath); err == nil {
		if _, err := storePathCheckpoint(ctx, sidecarPath); err != nil {
			recordLongMemEvalPrepareTiming(options, id, "checkpoint", stageStarted, err)
			return manifest, err
		}
	} else if !os.IsNotExist(err) {
		return manifest, err
	}
	recordLongMemEvalPrepareTiming(options, id, "checkpoint", stageStarted, nil)
	stageStarted = time.Now()
	counts, integrity, dimensions, err := inspectLongMemEvalFabricIndex(ctx, ledgerPath, indexPath,
		expectedEvents, expectedContexts, true)
	recordLongMemEvalPrepareTiming(options, id, "inspect", stageStarted, err)
	if err != nil {
		return manifest, err
	}
	if err := requireReadyLongMemEvalHealth(integrity, cfg.MemoryRemoteProcessing, true); err != nil {
		return manifest, err
	}
	stageStarted = time.Now()
	manifest.LedgerSHA256, err = fileSHA256(ledgerPath)
	if err != nil {
		return manifest, err
	}
	manifest.IndexSHA256, err = fileSHA256(indexPath)
	if err != nil {
		return manifest, err
	}
	manifest.Status = "ready"
	manifest.Counts = counts
	manifest.Integrity = integrity
	manifest.EmbeddingDimensions = dimensions
	manifest.ReadyAt = time.Now().UTC().Format(time.RFC3339Nano)
	if err := writeJSONAtomic(stagingManifestPath, manifest, 0o444); err != nil {
		return manifest, err
	}
	if err := makeLongMemEvalIndexReadOnly(stagingDir); err != nil {
		return manifest, err
	}
	if _, err := os.Stat(finalDir); err == nil {
		return manifest, fmt.Errorf("prepared index destination already exists: %s", finalDir)
	} else if !os.IsNotExist(err) {
		return manifest, err
	}
	if err := os.MkdirAll(filepath.Dir(finalDir), 0o755); err != nil {
		return manifest, err
	}
	if err := os.Rename(stagingDir, finalDir); err != nil {
		recordLongMemEvalPrepareTiming(options, id, "publish", stageStarted, err)
		return manifest, err
	}
	recordLongMemEvalPrepareTiming(options, id, "publish", stageStarted, nil)
	return manifest, nil
}

func recoverRepairableLongMemEvalCompilerJobs(ctx context.Context, ledgerPath string) (int64, error) {
	if _, err := os.Stat(ledgerPath); os.IsNotExist(err) {
		return 0, nil
	} else if err != nil {
		return 0, err
	}
	db, err := openLongMemEvalSQLite(ledgerPath, false)
	if err != nil {
		return 0, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	recovered := int64(0)
	repairApplied := func(key, value string) (bool, error) {
		var applied string
		scanErr := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&applied)
		if scanErr != nil && !errors.Is(scanErr, sql.ErrNoRows) {
			return false, scanErr
		}
		return applied == value, nil
	}
	markRepair := func(key, value string) error {
		_, execErr := tx.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
		return execErr
	}

	applied, err := repairApplied("longmemeval_structured_repair", longMemEvalStructuredRepair)
	if err != nil {
		return 0, err
	}
	if !applied {
		result, execErr := tx.ExecContext(ctx, `UPDATE jobs SET status='pending', attempts=0, available_at=?,
			lease_until='', last_error='', updated_at=? WHERE status='failed' AND job_kind='compile_events'
			AND last_error LIKE ?`, now, now, "%invalid character ':' after array element%")
		if execErr != nil {
			return 0, execErr
		}
		rows, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return 0, rowsErr
		}
		recovered += rows
		if err := markRepair("longmemeval_structured_repair", longMemEvalStructuredRepair); err != nil {
			return 0, err
		}
	}

	applied, err = repairApplied("longmemeval_empty_output_repair", longMemEvalEmptyOutputRepair)
	if err != nil {
		return 0, err
	}
	if !applied {
		rows, queryErr := tx.QueryContext(ctx, `SELECT job_id, payload_json FROM jobs
			WHERE status='failed' AND job_kind='compile_events' AND last_error LIKE ?`,
			"%exhausted the output budget without a semantic result%")
		if queryErr != nil {
			return 0, queryErr
		}
		type emptyOutputJob struct {
			id       string
			eventIDs []string
		}
		var jobs []emptyOutputJob
		for rows.Next() {
			var id, payloadJSON string
			if scanErr := rows.Scan(&id, &payloadJSON); scanErr != nil {
				_ = rows.Close()
				return 0, scanErr
			}
			var payload struct {
				EventIDs []string `json:"event_ids"`
			}
			if decodeErr := json.Unmarshal([]byte(payloadJSON), &payload); decodeErr != nil {
				_ = rows.Close()
				return 0, decodeErr
			}
			jobs = append(jobs, emptyOutputJob{id: id, eventIDs: uniqueSortedStrings(payload.EventIDs)})
		}
		if rowsErr := rows.Close(); rowsErr != nil {
			return 0, rowsErr
		}
		for _, job := range jobs {
			if len(job.eventIDs) > 0 {
				args := make([]any, 0, len(job.eventIDs)+1)
				args = append(args, memory.SemanticSkipped)
				for _, id := range job.eventIDs {
					args = append(args, id)
				}
				marks := strings.TrimSuffix(strings.Repeat("?,", len(job.eventIDs)), ",")
				if _, execErr := tx.ExecContext(ctx, `UPDATE events SET semantic_status=? WHERE event_id IN (`+
					marks+`)`, args...); execErr != nil {
					return 0, execErr
				}
			}
			if _, execErr := tx.ExecContext(ctx, `UPDATE jobs SET status='complete', lease_until='', updated_at=?
				WHERE job_id=? AND status='failed'`, now, job.id); execErr != nil {
				return 0, execErr
			}
			recovered++
		}
		if err := markRepair("longmemeval_empty_output_repair", longMemEvalEmptyOutputRepair); err != nil {
			return 0, err
		}
	}
	return recovered, tx.Commit()
}

func longMemEvalFabricScopeRoot(options RunnerOptions, questionID string) string {
	candidate := filepath.Join(options.LongMemEvalIndexSource, sanitizeCaseID(questionID))
	if info, err := os.Stat(candidate); err == nil && info.IsDir() {
		return candidate
	}
	return filepath.Join(options.WorkDir, "longmemeval-scopes", sanitizeCaseID(questionID))
}

func longMemEvalFabricConfig(base config.Config, cwd, fabricDir, scopeRoot string, answerOnly bool) config.Config {
	cfg := memoryCaseConfigWithScope(base, cwd, filepath.Join(fabricDir, "ledger.sqlite"), scopeRoot)
	cfg.MemoryBackend = "fabric"
	cfg.MemoryPath = fabricDir
	caseContainer := filepath.Dir(filepath.Dir(filepath.Clean(cwd)))
	cfg.MemoryArtifactPath = filepath.Join(caseContainer, "semantic-artifacts")
	if answerOnly {
		cfg.MemoryRemoteProcessing = "off"
	}
	config.PinFields(&cfg, "memory_backend", "memory_path", "memory_remote_processing")
	return cfg
}

func longMemEvalCompilerModel(cfg config.Config) string {
	if strings.EqualFold(strings.TrimSpace(cfg.MemoryRemoteProcessing), "off") {
		return ""
	}
	model := strings.TrimSpace(cfg.MemoryCompilerModel)
	if model == "" || strings.EqualFold(model, "inherit") {
		return strings.TrimSpace(cfg.APIModel)
	}
	return model
}

func longMemEvalEmbeddingModel(cfg config.Config) string {
	return strings.TrimSpace(cfg.MemoryEmbeddingModel)
}

func ingestLongMemEvalFabricHistory(ctx context.Context, fabric *memory.Fabric, cfg config.Config,
	c longMemEvalCase) (int, int, error) {
	space := agent.MemoryFabricSpace(cfg)
	allEvents, contexts, expectedEvents := buildLongMemEvalFabricHistory(c, space)
	if _, err := fabric.AppendEvents(ctx, allEvents,
		memory.IngestOptions{SemanticPolicy: memory.SemanticDurableOnly}); err != nil {
		return expectedEvents, len(contexts), err
	}
	if _, err := fabric.SealImport(ctx, contexts, memory.ImportPlanningOptions{
		MaxCompilerCalls: 4, MaxSources: 32, MaxSourcesPerSession: 2, MaxSourceRunes: 900,
	}); err != nil {
		return expectedEvents, len(contexts), err
	}
	return expectedEvents, len(contexts), nil
}

func buildLongMemEvalFabricHistory(c longMemEvalCase, space string) ([]memory.RawEvent, []memory.ContextRef, int) {
	var contexts []memory.ContextRef
	var allEvents []memory.RawEvent
	expectedEvents := 0
	for sessionIndex, session := range c.HaystackSessions {
		sessionID, duplicate := longMemEvalSessionIdentityAt(c, sessionIndex)
		if duplicate {
			continue
		}
		contextID := longMemEvalFabricContextID(sessionID, stringAt(c.HaystackDates, sessionIndex, ""))
		baseTime := longMemEvalFabricBaseTime(stringAt(c.HaystackDates, sessionIndex, ""), sessionIndex)
		events := make([]memory.RawEvent, 0, len(session))
		for turnIndex, turn := range session {
			content := strings.TrimSpace(longMemEvalTurnString(turn, "content"))
			if content == "" {
				continue
			}
			role := strings.ToLower(strings.TrimSpace(longMemEvalTurnString(turn, "role")))
			if role == "" {
				role = "unknown"
			}
			eventTime := baseTime.Add(time.Duration(turnIndex) * time.Second)
			eventID := longMemEvalFabricEventID(sessionID, turnIndex, role, content)
			events = append(events, memory.RawEvent{
				ID: eventID, Space: space, ContextID: contextID, SessionID: sessionID, Actor: role,
				SourceKind: "conversation", Content: content, OccurredAt: eventTime,
				SourceRef: fmt.Sprintf("longmemeval:%s:%04d", sessionID, turnIndex),
				Metadata: map[string]string{"session_id": sessionID,
					"session_date": stringAt(c.HaystackDates, sessionIndex, ""), "turn_index": fmt.Sprintf("%d", turnIndex)},
			})
			expectedEvents++
		}
		if len(events) == 0 {
			continue
		}
		allEvents = append(allEvents, events...)
		contexts = append(contexts, memory.ContextRef{ID: contextID, Space: space,
			Type: "longmemeval-session", Label: sessionID,
			OpenedAt: events[0].OccurredAt, ClosedAt: events[len(events)-1].OccurredAt.Add(time.Second)})
	}
	sort.Slice(allEvents, func(i, j int) bool {
		if !allEvents[i].OccurredAt.Equal(allEvents[j].OccurredAt) {
			return allEvents[i].OccurredAt.Before(allEvents[j].OccurredAt)
		}
		return allEvents[i].ID < allEvents[j].ID
	})
	return allEvents, contexts, expectedEvents
}

func longMemEvalFabricContextID(sessionID, date string) string {
	return "lme-session-" + shortStableHash(sessionID, date)
}

func longMemEvalFabricEventID(sessionID string, turnIndex int, role, content string) string {
	return "evt-lme-" + shortStableHash(sessionID, fmt.Sprintf("%d", turnIndex), role, content)
}

func shortStableHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x1f")))
	return hex.EncodeToString(sum[:16])
}

func longMemEvalFabricBaseTime(value string, sessionIndex int) time.Time {
	if parsed := parseLongMemEvalDate(value); !parsed.IsZero() {
		return parsed
	}
	return time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, sessionIndex)
}

func inspectLongMemEvalFabricIndex(ctx context.Context, ledgerPath, indexPath string,
	expectedEvents, expectedContexts int, embeddingsRequired bool) (longMemEvalIndexCounts,
	longMemEvalIndexIntegrity, int, error) {
	ledger, err := openLongMemEvalSQLite(ledgerPath, true)
	if err != nil {
		return longMemEvalIndexCounts{}, longMemEvalIndexIntegrity{}, 0, err
	}
	defer ledger.Close()
	index, err := openLongMemEvalSQLite(indexPath, true)
	if err != nil {
		return longMemEvalIndexCounts{}, longMemEvalIndexIntegrity{}, 0, err
	}
	defer index.Close()
	counts := longMemEvalIndexCounts{}
	integrity := longMemEvalIndexIntegrity{ExpectedEvents: expectedEvents, ExpectedContexts: expectedContexts}
	if err := ledger.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity.LedgerQuickCheck); err != nil {
		return counts, integrity, 0, err
	}
	if err := index.QueryRowContext(ctx, "PRAGMA quick_check").Scan(&integrity.IndexQuickCheck); err != nil {
		return counts, integrity, 0, err
	}
	ledgerCounts := []struct {
		target *int
		query  string
	}{
		{&counts.Events, `SELECT COUNT(*) FROM events WHERE tombstoned=0`},
		{&counts.Contexts, `SELECT COUNT(*) FROM contexts`},
		{&counts.Identities, `SELECT COUNT(*) FROM identities WHERE status='active'`},
		{&counts.Aliases, `SELECT COUNT(*) FROM identity_aliases WHERE status='active'`},
		{&counts.Nodes, `SELECT COUNT(*) FROM memory_nodes WHERE tombstoned=0`},
		{&counts.Slots, `SELECT COUNT(*) FROM slots`},
		{&counts.Conflicts, `SELECT COUNT(*) FROM conflict_sets`},
		{&counts.Resolutions, `SELECT COUNT(*) FROM resolutions`},
		{&integrity.PendingSemanticEvent, `SELECT COUNT(*) FROM events WHERE tombstoned=0 AND semantic_status IN ('event_durable','proposed')`},
		{&integrity.PendingJobs, `SELECT COUNT(*) FROM jobs WHERE status IN ('pending','running','retry')`},
		{&integrity.FailedJobs, `SELECT COUNT(*) FROM jobs WHERE status='failed'`},
		{&integrity.PendingOutbox, `SELECT COUNT(*) FROM outbox WHERE status!='done'`},
	}
	for _, item := range ledgerCounts {
		if err := ledger.QueryRowContext(ctx, item.query).Scan(item.target); err != nil {
			return counts, integrity, 0, err
		}
	}
	integrity.ActualEvents = counts.Events
	integrity.ActualContexts = counts.Contexts
	if err := ledger.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM outbox`).Scan(&integrity.MaxOutboxSeq); err != nil {
		return counts, integrity, 0, err
	}
	indexCounts := []struct {
		target *int
		query  string
	}{
		{&counts.Documents, `SELECT COUNT(*) FROM documents`},
		{&counts.FTSDocuments, `SELECT COUNT(*) FROM document_fts`},
		{&counts.KeyPostings, `SELECT COUNT(*) FROM key_postings`},
		{&counts.ActiveSlots, `SELECT COUNT(*) FROM active_slots`},
		{&counts.VectorRows, `SELECT COUNT(*) FROM _vec_memory_vectors`},
		{&counts.VectorDocuments, `SELECT COUNT(DISTINCT id) FROM _vec_memory_vectors`},
	}
	for _, item := range indexCounts {
		if err := index.QueryRowContext(ctx, item.query).Scan(item.target); err != nil {
			return counts, integrity, 0, err
		}
	}
	if err := index.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM index_meta WHERE key='indexed_ledger_seq'`).
		Scan(&integrity.IndexedLedgerSeq); err != nil {
		return counts, integrity, 0, err
	}
	if err := index.QueryRowContext(ctx, `SELECT CAST(value AS INTEGER) FROM index_meta WHERE key='index_generation'`).
		Scan(&integrity.IndexGeneration); err != nil {
		return counts, integrity, 0, err
	}
	var expectedFTS, expectedVectors int
	if err := index.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents WHERE resource_kind='event' OR
		(resource_kind='node' AND semantic_status NOT IN ('tombstoned','rejected','quarantined'))`).Scan(&expectedFTS); err != nil {
		return counts, integrity, 0, err
	}
	if err := index.QueryRowContext(ctx, `SELECT COUNT(*) FROM documents WHERE resource_kind='chunk' OR
		(resource_kind='node' AND semantic_status NOT IN ('tombstoned','rejected','quarantined'))`).Scan(&expectedVectors); err != nil {
		return counts, integrity, 0, err
	}
	integrity.FTSReady = counts.FTSDocuments == expectedFTS
	integrity.VectorReady = !embeddingsRequired || counts.VectorDocuments == expectedVectors
	dimensions := 0
	if counts.VectorRows > 0 {
		var bytes int
		if err := index.QueryRowContext(ctx, `SELECT COALESCE(MAX(length(embedding)),0) FROM _vec_memory_vectors`).Scan(&bytes); err != nil {
			return counts, integrity, 0, err
		}
		dimensions = bytes / 4
	}
	return counts, integrity, dimensions, nil
}

func requireReadyLongMemEvalHealth(health longMemEvalIndexIntegrity, remotePolicy string,
	embeddingsRequired bool) error {
	semanticReady := strings.EqualFold(strings.TrimSpace(remotePolicy), "off") || health.PendingSemanticEvent == 0
	if !strings.EqualFold(health.LedgerQuickCheck, "ok") || !strings.EqualFold(health.IndexQuickCheck, "ok") ||
		health.ActualEvents != health.ExpectedEvents || health.ActualContexts != health.ExpectedContexts ||
		health.PendingJobs != 0 || health.FailedJobs != 0 || health.PendingOutbox != 0 ||
		health.IndexedLedgerSeq < health.MaxOutboxSeq || !health.FTSReady ||
		(embeddingsRequired && !health.VectorReady) || !semanticReady {
		return fmt.Errorf("prepared Fabric index is not ready: ledger=%s index=%s events=%d/%d contexts=%d/%d semantic_pending=%d jobs=%d failed=%d outbox=%d indexed=%d/%d fts=%t vectors=%t",
			health.LedgerQuickCheck, health.IndexQuickCheck, health.ActualEvents, health.ExpectedEvents,
			health.ActualContexts, health.ExpectedContexts, health.PendingSemanticEvent, health.PendingJobs,
			health.FailedJobs, health.PendingOutbox, health.IndexedLedgerSeq, health.MaxOutboxSeq,
			health.FTSReady, health.VectorReady)
	}
	return nil
}

func maxIntLongMemEval(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func validateLongMemEvalIndexManifest(ctx context.Context, manifest longMemEvalIndexManifest, caseDir,
	datasetSHA, historySHA string, c longMemEvalCase, cfg config.Config) error {
	if manifest.Format != longMemEvalPreparedIndexFormat || manifest.Status != "ready" || manifest.Backend != "fabric" {
		return fmt.Errorf("LongMemEval index %s is not a ready Fabric snapshot (format=%q backend=%q status=%q)",
			manifest.QuestionID, manifest.Format, manifest.Backend, manifest.Status)
	}
	if manifest.DatasetSHA256 != datasetSHA || manifest.HistorySHA256 != historySHA || manifest.QuestionID != c.QuestionID {
		return fmt.Errorf("LongMemEval index fingerprint mismatch for %s", c.QuestionID)
	}
	if manifest.MemorySchema != longMemEvalMemorySchemaRevision || manifest.CompilerSchema != longMemEvalCompilerSchema ||
		manifest.RetrievalPipeline != longMemEvalRetrievalPipeline {
		return fmt.Errorf("LongMemEval Fabric index schema mismatch for %s", c.QuestionID)
	}
	// The durable ledger, lexical index, and semantic projections are reusable
	// across local vector model generations. Answer runs clone the snapshot;
	// Fabric then invalidates incompatible vector rows and rebuilds the BGE
	// sidecar from raw events without changing the published prepared index.
	ledgerPath := filepath.Join(longMemEvalFabricDir(caseDir), "ledger.sqlite")
	indexPath := filepath.Join(longMemEvalFabricDir(caseDir), "index.sqlite")
	ledgerHash, err := fileSHA256(ledgerPath)
	if err != nil {
		return err
	}
	indexHash, err := fileSHA256(indexPath)
	if err != nil {
		return err
	}
	if ledgerHash != manifest.LedgerSHA256 || indexHash != manifest.IndexSHA256 {
		return fmt.Errorf("LongMemEval ready Fabric snapshot changed after publication for %s", c.QuestionID)
	}
	counts, health, dimensions, err := inspectLongMemEvalFabricIndex(ctx, ledgerPath, indexPath,
		manifest.Integrity.ExpectedEvents, manifest.Integrity.ExpectedContexts, manifest.EmbeddingModel != "")
	if err != nil {
		return err
	}
	if dimensions != manifest.EmbeddingDimensions || counts != manifest.Counts || health != manifest.Integrity {
		return fmt.Errorf("LongMemEval ready Fabric index metadata changed for %s", c.QuestionID)
	}
	return requireReadyLongMemEvalHealth(health, manifest.RemoteProcessing, manifest.EmbeddingModel != "")
}

func readLongMemEvalIndexManifest(path string) (longMemEvalIndexManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return longMemEvalIndexManifest{}, err
	}
	var manifest longMemEvalIndexManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func longMemEvalManifestDigest(manifest longMemEvalIndexManifest) (string, error) {
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func aggregateLongMemEvalIndexDigest(manifests []longMemEvalIndexManifest) (string, error) {
	parts := make([]string, 0, len(manifests))
	for _, manifest := range manifests {
		digest, err := longMemEvalManifestDigest(manifest)
		if err != nil {
			return "", err
		}
		parts = append(parts, manifest.QuestionID+"="+digest)
	}
	sort.Strings(parts)
	sum := sha256.Sum256([]byte(strings.Join(parts, "\n")))
	return hex.EncodeToString(sum[:]), nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func openLongMemEvalSQLite(path string, readOnly bool) (*sql.DB, error) {
	dsn := path
	if readOnly {
		location := &url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
		query := url.Values{}
		query.Set("mode", "ro")
		query.Set("_pragma", "query_only=1")
		location.RawQuery = query.Encode()
		dsn = location.String()
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func storePathCheckpoint(ctx context.Context, path string) (string, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	var busy, logFrames, checkpointed int
	if err := db.QueryRowContext(ctx, "PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return "", err
	}
	if busy != 0 {
		return "", fmt.Errorf("SQLite WAL checkpoint remained busy for %s", path)
	}
	return fmt.Sprintf("log=%d checkpointed=%d", logFrames, checkpointed), nil
}

func cloneSQLiteStore(ctx context.Context, source, destination string) error {
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		src, dst := source+suffix, destination+suffix
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		if err := cloneFile(ctx, src, dst); err != nil {
			return err
		}
	}
	return nil
}

func cloneFile(ctx context.Context, source, destination string) error {
	if err := exec.CommandContext(ctx, "cp", "-c", source, destination).Run(); err == nil {
		return os.Chmod(destination, 0o600)
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(output, input); err == nil {
		err = output.Sync()
	}
	closeErr := output.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func makeLongMemEvalIndexReadOnly(caseDir string) error {
	return filepath.Walk(caseDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return os.Chmod(path, 0o555)
		}
		return os.Chmod(path, 0o444)
	})
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), mode); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func appendJSONLine(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

func memoryCaseConfigWithScope(base config.Config, cwd, memoryPath, scopeRoot string) config.Config {
	cfg := base
	cfg.CWD = cwd
	cfg.HarnessMode = ""
	cfg.LongTermMemoryEnabled = true
	cfg.MemoryBackend = "fabric"
	cfg.MemoryPath = memoryPath
	cfg.SessionMemoryEnabled = false
	cfg.ProjectRuntimeDir = filepath.Join(cwd, "state", "projects", "benchmark")
	if strings.TrimSpace(scopeRoot) != "" {
		cfg.ProjectPaths.CanonicalRoot = scopeRoot
	}
	config.PinFields(&cfg, "harness_mode", "long_term_memory_enabled", "memory_backend", "memory_path",
		"session_memory_enabled", "project_runtime_dir")
	return cfg
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
	leftIndex, rightIndex := 0, 0
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
