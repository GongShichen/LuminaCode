package agentbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"LuminaCode/agent"
	luminaapi "LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/memory"
)

const longMemEvalSemanticPrewarmCaseID = "_semantic_prewarm"

const (
	longMemEvalSemanticPrewarmDefaultWorkers = 2
	longMemEvalSemanticPrewarmMaxWorkers     = 64
)

type longMemEvalSemanticTask struct {
	Key        string
	SessionRef string
	Class      string
	Sources    []memory.CompileSource
}

type longMemEvalSemanticPrewarmRecord struct {
	DatasetSHA256 string `json:"dataset_sha256"`
	TaskKey       string `json:"task_key"`
	SessionRef    string `json:"session_ref"`
	Class         string `json:"class"`
	SourceCount   int    `json:"source_count"`
	Status        string `json:"status"`
	CompletedAt   string `json:"completed_at"`
}

type longMemEvalSemanticBatchOutput struct {
	Tasks    []longMemEvalSemanticTask
	Response memory.CompileResponse
	Status   string
	Err      error
}

type longMemEvalSemanticWorkPlan struct {
	Tasks        []longMemEvalSemanticTask
	CaseTaskKeys map[string][]string
}

type longMemEvalSemanticProgress struct {
	TaskKeys []string
	Err      error
}

type longMemEvalSemanticPipeline struct {
	CaseTaskKeys  map[string][]string
	ReadyTaskKeys []string
	Progress      <-chan longMemEvalSemanticProgress
}

type longMemEvalSemanticDependencyTracker struct {
	caseIDs    []string
	remaining  []int
	dependents map[string][]int
	readyTasks map[string]struct{}
	released   []bool
}

func newLongMemEvalSemanticDependencyTracker(cases []longMemEvalCase, caseTaskKeys map[string][]string,
	readyTaskKeys []string) (*longMemEvalSemanticDependencyTracker, error) {
	tracker := &longMemEvalSemanticDependencyTracker{
		caseIDs: make([]string, len(cases)), remaining: make([]int, len(cases)),
		dependents: map[string][]int{}, readyTasks: map[string]struct{}{}, released: make([]bool, len(cases)),
	}
	for _, key := range readyTaskKeys {
		tracker.readyTasks[key] = struct{}{}
	}
	seenCases := map[string]struct{}{}
	for caseIndex, c := range cases {
		caseID := firstNonEmptyString(c.QuestionID, "longmemeval-case")
		if _, exists := seenCases[caseID]; exists {
			return nil, fmt.Errorf("duplicate LongMemEval case ID %q", caseID)
		}
		seenCases[caseID] = struct{}{}
		tracker.caseIDs[caseIndex] = caseID
		seenKeys := map[string]struct{}{}
		for _, key := range caseTaskKeys[caseID] {
			if _, duplicate := seenKeys[key]; duplicate {
				continue
			}
			seenKeys[key] = struct{}{}
			if _, ready := tracker.readyTasks[key]; ready {
				continue
			}
			tracker.remaining[caseIndex]++
			tracker.dependents[key] = append(tracker.dependents[key], caseIndex)
		}
	}
	for caseID := range caseTaskKeys {
		if _, exists := seenCases[caseID]; !exists {
			return nil, fmt.Errorf("semantic dependency references unknown case %q", caseID)
		}
	}
	return tracker, nil
}

func (t *longMemEvalSemanticDependencyTracker) ReleaseReadyCases() []int {
	ready := make([]int, 0)
	for caseIndex, remaining := range t.remaining {
		if remaining == 0 && !t.released[caseIndex] {
			t.released[caseIndex] = true
			ready = append(ready, caseIndex)
		}
	}
	return ready
}

func (t *longMemEvalSemanticDependencyTracker) MarkTasksReady(taskKeys []string) []int {
	for _, key := range taskKeys {
		if _, ready := t.readyTasks[key]; ready {
			continue
		}
		t.readyTasks[key] = struct{}{}
		for _, caseIndex := range t.dependents[key] {
			if t.remaining[caseIndex] > 0 {
				t.remaining[caseIndex]--
			}
		}
	}
	return t.ReleaseReadyCases()
}

func (t *longMemEvalSemanticDependencyTracker) UnreleasedCaseIDs() []string {
	var ids []string
	for index, released := range t.released {
		if !released {
			ids = append(ids, t.caseIDs[index])
		}
	}
	return ids
}

func startLongMemEvalSemanticPrewarm(ctx context.Context, options RunnerOptions, datasetSHA string,
	cases []longMemEvalCase) (longMemEvalSemanticPipeline, error) {
	closedProgress := func() <-chan longMemEvalSemanticProgress {
		progress := make(chan longMemEvalSemanticProgress)
		close(progress)
		return progress
	}
	if strings.EqualFold(strings.TrimSpace(options.Config.MemoryRemoteProcessing), "off") {
		return longMemEvalSemanticPipeline{CaseTaskKeys: map[string][]string{}, Progress: closedProgress()}, nil
	}
	pending := longMemEvalCasesNeedingSemanticPrewarm(options, datasetSHA, cases)
	if len(pending) == 0 {
		return longMemEvalSemanticPipeline{CaseTaskKeys: map[string][]string{}, Progress: closedProgress()}, nil
	}
	prewarmRoot := filepath.Join(options.LongMemEvalIndexDir, "cases", ".semantic-prewarm")
	cfg := longMemEvalFabricConfig(options.Config, prewarmRoot, filepath.Join(prewarmRoot, "fabric"),
		filepath.Join(options.WorkDir, "longmemeval-scopes", ".semantic-prewarm"), false)
	planner := agent.ConfiguredMemorySemanticPlanner(cfg)
	work, err := planLongMemEvalSemanticWork(ctx, planner, pending)
	if err != nil {
		return longMemEvalSemanticPipeline{}, fmt.Errorf("plan dataset semantic artifacts: %w", err)
	}
	compiler := agent.ConfiguredMemorySemanticCompiler(cfg)
	if compiler == nil {
		return longMemEvalSemanticPipeline{CaseTaskKeys: map[string][]string{}, Progress: closedProgress()}, nil
	}
	store, ok := compiler.(memory.CompileArtifactStore)
	if !ok {
		return longMemEvalSemanticPipeline{}, errors.New(
			"configured semantic compiler does not expose artifact state")
	}
	missing := make([]longMemEvalSemanticTask, 0, len(work.Tasks))
	ready := make([]string, 0, len(work.Tasks))
	checkpointPath := filepath.Join(options.OutputDir, "checkpoints", "semantic", datasetSHA+".jsonl")
	for _, task := range work.Tasks {
		cached, err := store.CompileArtifactCached(task.Sources)
		if err != nil {
			return longMemEvalSemanticPipeline{}, err
		}
		if !cached {
			missing = append(missing, task)
			continue
		}
		if err := appendLongMemEvalSemanticPrewarmRecord(checkpointPath, datasetSHA, task, "cached"); err != nil {
			return longMemEvalSemanticPipeline{}, err
		}
		ready = append(ready, task.Key)
	}
	if len(missing) == 0 {
		return longMemEvalSemanticPipeline{CaseTaskKeys: work.CaseTaskKeys, ReadyTaskKeys: ready,
			Progress: closedProgress()}, nil
	}
	missing = prioritizeLongMemEvalSemanticTasks(missing, pending, work.CaseTaskKeys)
	batches, err := packLongMemEvalSemanticTasks(compiler, missing)
	if err != nil {
		return longMemEvalSemanticPipeline{}, err
	}
	progress := make(chan longMemEvalSemanticProgress, 2*longMemEvalSemanticPrewarmWorkers())
	go func() {
		defer close(progress)
		err := compileLongMemEvalSemanticBatches(ctx, options, cfg, datasetSHA, checkpointPath, batches,
			func(tasks []longMemEvalSemanticTask) error {
				keys := make([]string, len(tasks))
				for index, task := range tasks {
					keys[index] = task.Key
				}
				select {
				case progress <- longMemEvalSemanticProgress{TaskKeys: keys}:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			})
		if err != nil {
			select {
			case progress <- longMemEvalSemanticProgress{Err: err}:
			case <-ctx.Done():
			}
		}
	}()
	return longMemEvalSemanticPipeline{CaseTaskKeys: work.CaseTaskKeys, ReadyTaskKeys: ready,
		Progress: progress}, nil
}

func prioritizeLongMemEvalSemanticTasks(tasks []longMemEvalSemanticTask, cases []longMemEvalCase,
	caseTaskKeys map[string][]string) []longMemEvalSemanticTask {
	taskByKey := make(map[string]longMemEvalSemanticTask, len(tasks))
	for _, task := range tasks {
		taskByKey[task.Key] = task
	}
	type casePriority struct {
		index   int
		id      string
		missing int
	}
	priorities := make([]casePriority, 0, len(cases))
	for index, c := range cases {
		id := firstNonEmptyString(c.QuestionID, "longmemeval-case")
		missing := 0
		seenKeys := map[string]struct{}{}
		for _, key := range caseTaskKeys[id] {
			if _, duplicate := seenKeys[key]; duplicate {
				continue
			}
			seenKeys[key] = struct{}{}
			if _, exists := taskByKey[key]; exists {
				missing++
			}
		}
		if missing > 0 {
			priorities = append(priorities, casePriority{index: index, id: id, missing: missing})
		}
	}
	sort.SliceStable(priorities, func(i, j int) bool {
		if priorities[i].missing != priorities[j].missing {
			return priorities[i].missing < priorities[j].missing
		}
		return priorities[i].index < priorities[j].index
	})
	ordered := make([]longMemEvalSemanticTask, 0, len(tasks))
	seen := map[string]struct{}{}
	for _, priority := range priorities {
		var local []longMemEvalSemanticTask
		for _, key := range caseTaskKeys[priority.id] {
			if _, exists := seen[key]; exists {
				continue
			}
			if task, exists := taskByKey[key]; exists {
				local = append(local, task)
			}
		}
		sort.SliceStable(local, func(i, j int) bool {
			if local[i].Class != local[j].Class {
				return local[i].Class < local[j].Class
			}
			if local[i].SessionRef != local[j].SessionRef {
				return local[i].SessionRef < local[j].SessionRef
			}
			return local[i].Key < local[j].Key
		})
		for _, task := range local {
			seen[task.Key] = struct{}{}
			ordered = append(ordered, task)
		}
	}
	for _, task := range tasks {
		if _, exists := seen[task.Key]; exists {
			continue
		}
		ordered = append(ordered, task)
	}
	return ordered
}

func longMemEvalCasesNeedingSemanticPrewarm(options RunnerOptions, datasetSHA string,
	cases []longMemEvalCase) []longMemEvalCase {
	pending := make([]longMemEvalCase, 0, len(cases))
	for _, c := range cases {
		id := firstNonEmptyString(c.QuestionID, "longmemeval-case")
		manifest, err := readLongMemEvalIndexManifest(longMemEvalIndexManifestPath(options.LongMemEvalIndexDir, id))
		if err == nil && manifest.Status == "ready" && manifest.DatasetSHA256 == datasetSHA &&
			manifest.HistorySHA256 == longMemEvalHistorySHA(c) {
			continue
		}
		pending = append(pending, c)
	}
	return pending
}

func planLongMemEvalSemanticTasks(ctx context.Context, planner memory.SemanticPlanner,
	cases []longMemEvalCase) ([]longMemEvalSemanticTask, error) {
	work, err := planLongMemEvalSemanticWork(ctx, planner, cases)
	return work.Tasks, err
}

func planLongMemEvalSemanticWork(ctx context.Context, planner memory.SemanticPlanner,
	cases []longMemEvalCase) (longMemEvalSemanticWorkPlan, error) {
	batches := make([]memory.SemanticPlanningBatch, len(cases))
	for index, c := range cases {
		events, _, _ := buildLongMemEvalFabricHistory(c, "semantic-import")
		batches[index] = memory.SemanticPlanningBatch{Events: events, Options: memory.PlanningOptions{
			Mode: memory.WriteImport, MaxSources: 32, MaxSourcesPerSession: 2, MaxSourceRunes: 900,
		}}
	}
	var plans []memory.SemanticPlan
	var err error
	if batchPlanner, ok := planner.(memory.BatchSemanticPlanner); ok {
		plans, err = batchPlanner.PlanBatch(ctx, batches)
	} else {
		plans = make([]memory.SemanticPlan, len(batches))
		for index, batch := range batches {
			plans[index], err = planner.Plan(ctx, batch.Events, batch.Options)
			if err != nil {
				break
			}
		}
	}
	if err != nil {
		return longMemEvalSemanticWorkPlan{}, err
	}
	if len(plans) != len(cases) {
		return longMemEvalSemanticWorkPlan{}, fmt.Errorf("semantic planner returned %d plans for %d cases",
			len(plans), len(cases))
	}
	tasksByKey := map[string]longMemEvalSemanticTask{}
	caseTaskKeys := make(map[string][]string, len(cases))
	for caseIndex, plan := range plans {
		caseID := firstNonEmptyString(cases[caseIndex].QuestionID, "longmemeval-case")
		caseKeys := map[string]struct{}{}
		bySession := map[string][]memory.CompileSource{}
		for _, candidate := range plan.Candidates {
			session := strings.TrimSpace(candidate.Source.SessionRef)
			if session == "" {
				session = strings.TrimSpace(candidate.Source.SourceRef)
			}
			bySession[session] = append(bySession[session], candidate.Source)
		}
		for session, sources := range bySession {
			sort.SliceStable(sources, func(i, j int) bool { return sources[i].SourceRef < sources[j].SourceRef })
			key, err := longMemEvalSemanticTaskKey(sources)
			if err != nil {
				return longMemEvalSemanticWorkPlan{}, err
			}
			if _, exists := tasksByKey[key]; !exists {
				tasksByKey[key] = longMemEvalSemanticTask{Key: key, SessionRef: session,
					Class: classifyLongMemEvalSemanticTask(sources), Sources: sources}
			}
			caseKeys[key] = struct{}{}
		}
		for key := range caseKeys {
			caseTaskKeys[caseID] = append(caseTaskKeys[caseID], key)
		}
		sort.Strings(caseTaskKeys[caseID])
	}
	tasks := make([]longMemEvalSemanticTask, 0, len(tasksByKey))
	for _, task := range tasksByKey {
		tasks = append(tasks, task)
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Class != tasks[j].Class {
			return tasks[i].Class < tasks[j].Class
		}
		if tasks[i].SessionRef != tasks[j].SessionRef {
			return tasks[i].SessionRef < tasks[j].SessionRef
		}
		return tasks[i].Key < tasks[j].Key
	})
	return longMemEvalSemanticWorkPlan{Tasks: tasks, CaseTaskKeys: caseTaskKeys}, nil
}

func longMemEvalSemanticTaskKey(sources []memory.CompileSource) (string, error) {
	encoded, err := json.Marshal(struct {
		Sources []memory.CompileSource `json:"sources"`
	}{Sources: sources})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func classifyLongMemEvalSemanticTask(sources []memory.CompileSource) string {
	runes := 0
	hasHan := false
	for _, source := range sources {
		for _, r := range source.Text {
			runes++
			if unicode.Is(unicode.Han, r) {
				hasHan = true
			}
		}
	}
	language := "latin"
	if hasHan {
		language = "han"
	}
	size := "short"
	if runes > 1200 {
		size = "long"
	} else if runes > 500 {
		size = "medium"
	}
	return fmt.Sprintf("%s-%s-%d", language, size, len(sources))
}

func packLongMemEvalSemanticTasks(compiler memory.SemanticCompiler,
	tasks []longMemEvalSemanticTask) ([][]longMemEvalSemanticTask, error) {
	sizer, ok := compiler.(memory.CompileRequestSizer)
	if !ok {
		return nil, errors.New("configured semantic compiler does not expose request sizing")
	}
	var batches [][]longMemEvalSemanticTask
	var current []longMemEvalSemanticTask
	flush := func() {
		if len(current) > 0 {
			batches = append(batches, current)
			current = nil
		}
	}
	for _, task := range tasks {
		trial := append(append([]longMemEvalSemanticTask(nil), current...), task)
		if duplicateLongMemEvalTaskSession(trial) || longMemEvalSemanticSourceCount(trial) > 8 {
			flush()
			trial = []longMemEvalSemanticTask{task}
		}
		request := longMemEvalSemanticCompileRequest(trial)
		estimated, err := sizer.EstimateInputTokens(request)
		if err != nil {
			return nil, err
		}
		if estimated > request.MaxInputTokens {
			if len(current) == 0 {
				return nil, fmt.Errorf("semantic task %s exceeds input budget: %d > %d",
					task.Key, estimated, request.MaxInputTokens)
			}
			flush()
			trial = []longMemEvalSemanticTask{task}
			request = longMemEvalSemanticCompileRequest(trial)
			estimated, err = sizer.EstimateInputTokens(request)
			if err != nil {
				return nil, err
			}
			if estimated > request.MaxInputTokens {
				return nil, fmt.Errorf("semantic task %s exceeds input budget: %d > %d",
					task.Key, estimated, request.MaxInputTokens)
			}
		}
		current = trial
	}
	flush()
	return batches, nil
}

func duplicateLongMemEvalTaskSession(tasks []longMemEvalSemanticTask) bool {
	seen := map[string]struct{}{}
	for _, task := range tasks {
		if _, exists := seen[task.SessionRef]; exists {
			return true
		}
		seen[task.SessionRef] = struct{}{}
	}
	return false
}

func longMemEvalSemanticSourceCount(tasks []longMemEvalSemanticTask) int {
	total := 0
	for _, task := range tasks {
		total += len(task.Sources)
	}
	return total
}

func longMemEvalSemanticCompileRequest(tasks []longMemEvalSemanticTask) memory.CompileRequest {
	var sources []memory.CompileSource
	for _, task := range tasks {
		sources = append(sources, task.Sources...)
	}
	return memory.CompileRequest{Mode: memory.WriteImport, Sources: sources,
		MaxInputTokens: 10_000, MaxOutputTokens: 2_560, MaxNodes: minInt(8, len(sources))}
}

func compileLongMemEvalSemanticBatches(ctx context.Context, options RunnerOptions, cfg config.Config,
	datasetSHA, checkpointPath string, batches [][]longMemEvalSemanticTask,
	onReady func([]longMemEvalSemanticTask) error) error {
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan []longMemEvalSemanticTask)
	outputs := make(chan longMemEvalSemanticBatchOutput, len(batches))
	workers := minInt(longMemEvalSemanticPrewarmWorkers(), len(batches))
	fmt.Printf("[longmemeval-semantic-prewarm] workers=%d batches=%d tasks=%d\n",
		workers, len(batches), longMemEvalSemanticTaskCount(batches))
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			compiler := agent.ConfiguredMemorySemanticCompiler(cfg)
			if compiler == nil {
				outputs <- longMemEvalSemanticBatchOutput{Err: errors.New("semantic compiler is disabled")}
				cancel()
				return
			}
			store, ok := compiler.(memory.CompileArtifactStore)
			if !ok {
				outputs <- longMemEvalSemanticBatchOutput{Err: errors.New(
					"configured semantic compiler does not expose artifact state")}
				cancel()
				return
			}
			for batch := range jobs {
				if runCtx.Err() != nil {
					return
				}
				response, err := compiler.Compile(runCtx, longMemEvalSemanticCompileRequest(batch))
				status := "compiled"
				if response.CacheHit {
					status = "cached"
				}
				if isLongMemEvalSemanticSkipError(err) {
					status = "semantic_skipped"
					var sources []memory.CompileSource
					for _, task := range batch {
						sources = append(sources, task.Sources...)
					}
					if markErr := store.MarkCompileArtifactSkipped(sources); markErr != nil {
						err = errors.Join(err, markErr)
					} else {
						err = nil
					}
				}
				if err == nil {
					for _, task := range batch {
						cached, cacheErr := store.CompileArtifactCached(task.Sources)
						if cacheErr != nil {
							err = cacheErr
							break
						}
						if !cached {
							err = fmt.Errorf("semantic compiler did not persist task %s", task.Key)
							break
						}
					}
				}
				outputs <- longMemEvalSemanticBatchOutput{Tasks: batch, Response: response, Status: status, Err: err}
				if err != nil {
					cancel()
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobs)
		for _, batch := range batches {
			select {
			case jobs <- batch:
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
	var firstErr error
	for output := range outputs {
		usage := output.Response.Usage
		errorText := ""
		if output.Err != nil {
			errorText = output.Err.Error()
		}
		if err := appendLongMemEvalTokenUsage(longMemEvalTokenUsagePath(options), longMemEvalTokenUsageRecord{
			Phase: LongMemEvalPhasePrepare, CaseID: longMemEvalSemanticPrewarmCaseID, Stage: memory.APIStageSemanticCompile,
			Calls: usage.Calls, InputTokens: usage.InputTokens, CacheReadInputTokens: usage.CacheReadInputTokens,
			CacheCreationInputTokens: usage.CacheCreationInputTokens, OutputTokens: usage.OutputTokens,
			Model: usage.Model, Error: errorText, RecordedAt: time.Now().UTC().Format(time.RFC3339Nano),
		}); err != nil && firstErr == nil {
			firstErr = err
			cancel()
		}
		for _, task := range output.Tasks {
			if err := appendLongMemEvalSemanticPrewarmRecord(checkpointPath, datasetSHA, task, output.Status); err != nil && firstErr == nil {
				firstErr = err
				cancel()
			}
		}
		if output.Err == nil && firstErr == nil && onReady != nil {
			if err := onReady(output.Tasks); err != nil {
				firstErr = err
				cancel()
			}
		}
		completed += len(output.Tasks)
		fmt.Printf("[longmemeval-semantic-prewarm] task %d/%d %s\n", completed,
			longMemEvalSemanticTaskCount(batches), output.Status)
		if output.Err != nil && firstErr == nil {
			if longMemEvalQuotaError(output.Err) {
				firstErr = fmt.Errorf("API quota exhausted during semantic prewarm: %w", output.Err)
			} else {
				firstErr = output.Err
			}
			cancel()
		}
	}
	if firstErr != nil {
		return firstErr
	}
	if completed != longMemEvalSemanticTaskCount(batches) {
		return fmt.Errorf("semantic prewarm stopped after %d/%d tasks", completed,
			longMemEvalSemanticTaskCount(batches))
	}
	return nil
}

func longMemEvalSemanticPrewarmWorkers() int {
	configured := strings.TrimSpace(os.Getenv("LUMINA_LONGMEMEVAL_SEMANTIC_PREWARM_CONCURRENCY"))
	if configured == "" {
		return longMemEvalSemanticPrewarmDefaultWorkers
	}
	workers, err := strconv.Atoi(configured)
	if err != nil || workers < 1 {
		return longMemEvalSemanticPrewarmDefaultWorkers
	}
	return minInt(workers, longMemEvalSemanticPrewarmMaxWorkers)
}

func longMemEvalSemanticTaskCount(batches [][]longMemEvalSemanticTask) int {
	total := 0
	for _, batch := range batches {
		total += len(batch)
	}
	return total
}

func appendLongMemEvalSemanticPrewarmRecord(path, datasetSHA string, task longMemEvalSemanticTask,
	status string) error {
	return appendJSONLine(path, longMemEvalSemanticPrewarmRecord{DatasetSHA256: datasetSHA,
		TaskKey: task.Key, SessionRef: task.SessionRef, Class: task.Class, SourceCount: len(task.Sources),
		Status: status, CompletedAt: time.Now().UTC().Format(time.RFC3339Nano)})
}

func isLongMemEvalSemanticSkipError(err error) bool {
	var contractErr *memory.CompileContractError
	return errors.As(err, &contractErr) && strings.Contains(strings.ToLower(contractErr.Reason),
		"exhausted the output budget without a semantic result")
}

func longMemEvalQuotaError(err error) bool {
	return luminaapi.IsQuotaExhaustedError(err) || (err != nil && luminaapi.IsQuotaExhaustedMessage(err.Error()))
}
