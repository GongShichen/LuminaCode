package agentbench

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
)

func DefaultRootDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return filepath.Join(".", "benchmark")
	}
	return filepath.Join(home, "Documents", "benchmark")
}

func RunSuite(ctx context.Context, options RunnerOptions) (Report, error) {
	options = normalizeOptions(options)
	if IsOfficialSuite(options.Suite) {
		return RunOfficialSuite(ctx, options)
	}
	if isMemoryBenchmarkSuite(options.Suite) {
		return RunMemoryBenchmarkSuite(ctx, options)
	}
	cases, err := LoadCases(options.Suite, options.CasesPath, options.Limit)
	if err != nil {
		return Report{}, err
	}
	if err := os.MkdirAll(options.OutputDir, 0o755); err != nil {
		return Report{}, err
	}
	if err := os.MkdirAll(options.WorkDir, 0o755); err != nil {
		return Report{}, err
	}
	if err := os.MkdirAll(options.ArtifactsDir, 0o755); err != nil {
		return Report{}, err
	}
	results := make([]CaseResult, 0, len(cases))
	for _, c := range cases {
		result := runCase(ctx, c, options)
		results = append(results, result)
	}
	options.Config = config.ReloadDynamicConfig(options.Config)
	now := options.Now()
	report := Report{
		Suite:       options.Suite,
		GeneratedAt: now.Format(time.RFC3339),
		RootDir:     options.RootDir,
		OutputDir:   options.OutputDir,
		WorkDir:     options.WorkDir,
		Model:       options.Config.APIModel,
		Summary:     BuildSummary(results),
		Results:     results,
	}
	if options.Suite == SuiteSWEBenchVerifiedSubset {
		predictionsPath, err := WriteSWEBenchPredictions(report, options.OutputDir)
		if err != nil {
			return Report{}, err
		}
		report.PredictionsPath = predictionsPath
		if strings.TrimSpace(options.SWEBenchHarnessCmd) != "" {
			outputPath, exitCode, parsed := runSWEBenchHarness(ctx, options.SWEBenchHarnessCmd, predictionsPath, options.OutputDir)
			report.HarnessOutputPath = outputPath
			report.HarnessExitCode = &exitCode
			report.HarnessParsedStats = parsed
		}
	}
	return report, nil
}

func normalizeOptions(options RunnerOptions) RunnerOptions {
	if options.Suite == "" {
		options.Suite = SuiteAiderPolyglotSmoke
	}
	if options.RootDir == "" {
		options.RootDir = DefaultRootDir()
	}
	if options.OutputDir == "" {
		options.OutputDir = filepath.Join(options.RootDir, "reports")
	}
	if options.WorkDir == "" {
		options.WorkDir = filepath.Join(options.RootDir, "work")
	}
	if options.ArtifactsDir == "" {
		options.ArtifactsDir = filepath.Join(options.RootDir, "artifacts")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.AgentRunner == nil {
		options.AgentRunner = HeadlessAgentRunner{}
	}
	if options.Config.CWD == "" {
		cwd, _ := os.Getwd()
		options.Config.CWD = cwd
	}
	options.Config.Yolo = true
	return options
}

func runCase(ctx context.Context, c CaseSpec, options RunnerOptions) CaseResult {
	caseStart := time.Now()
	timeline := []TimelineEvent{newTimelineEvent(caseStart, caseStart, "case_start", map[string]any{"case_id": c.ID})}
	timeout := c.TimeoutSeconds
	if timeout <= 0 {
		timeout = options.TimeoutSeconds
	}
	caseCtx, cancel := contextWithOptionalTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()

	artifactDir := filepath.Join(options.ArtifactsDir, sanitizeCaseID(c.ID))
	_ = os.RemoveAll(artifactDir)
	_ = os.MkdirAll(artifactDir, 0o755)
	result := CaseResult{Case: c, WorkDir: "", ExpectedArtifact: c.ExpectedArtifact}
	setArtifactPaths(&result, artifactDir)
	caseWorkDir, err := MaterializeCase(caseCtx, c, filepath.Join(options.WorkDir, sanitizeCaseID(c.Benchmark)))
	if err != nil {
		result.ErrorType = "materialize_failed: " + err.Error()
		result.DurationSeconds = time.Since(caseStart).Seconds()
		result.Timeline = timeline
		writeCaseArtifacts(artifactDir, c, AgentRunResult{}, result)
		return result
	}
	result.WorkDir = caseWorkDir

	for _, command := range c.SetupCommands {
		result.SetupResults = append(result.SetupResults, RunShellCommand(caseCtx, caseWorkDir, command, time.Duration(timeout)*time.Second))
	}
	if !commandsPassed(result.SetupResults) {
		result.ErrorType = "setup_failed"
		result.DurationSeconds = time.Since(caseStart).Seconds()
		timeline = append(timeline, newTimelineEvent(caseStart, time.Now(), "setup_failed", nil))
		result.Timeline = timeline
		savePatch(caseCtx, caseWorkDir, result.FinalPatchPath)
		writeCaseArtifacts(artifactDir, c, AgentRunResult{}, result)
		return result
	}

	cfg := options.Config
	cfg.CWD = caseWorkDir
	timeline = append(timeline, newTimelineEvent(caseStart, time.Now(), "first_model_request", nil))
	agentResult := options.AgentRunner.Run(caseCtx, cfg, c.Prompt, "agentbench-"+sanitizeCaseID(c.ID))
	timeline = append(timeline, agentResult.Timeline...)
	result.TTFTMillis = agentResult.TTFTMillis
	result.FirstToolCallMS = agentResult.FirstToolCallMS
	result.InputTokens = agentResult.InputTokens
	result.OutputTokens = agentResult.OutputTokens
	result.ToolCalls = agentResult.ToolCalls

	validationStart := time.Now()
	timeline = append(timeline, newTimelineEvent(caseStart, validationStart, "validation_start", nil))
	firstTestMS := float64(validationStart.Sub(caseStart).Milliseconds())
	if len(c.TestCommands) > 0 || c.ExpectedArtifact != "" {
		result.FirstTestMS = &firstTestMS
	}
	for _, command := range c.TestCommands {
		result.TestResults = append(result.TestResults, RunShellCommand(caseCtx, caseWorkDir, command, time.Duration(timeout)*time.Second))
		if len(result.TestResults) == 1 {
			timeline = append(timeline, newTimelineEvent(caseStart, time.Now(), "first_test_command", map[string]any{"command": command}))
		}
	}
	result.ExpectedSatisfied = true
	if c.ExpectedArtifact != "" {
		if _, err := os.Stat(filepath.Join(caseWorkDir, c.ExpectedArtifact)); err != nil {
			result.ExpectedSatisfied = false
		}
	}
	timeline = append(timeline, newTimelineEvent(caseStart, time.Now(), "validation_end", nil))
	testsPassed := commandsPassed(result.TestResults) && result.ExpectedSatisfied
	if testsPassed {
		result.TestPassRate = 1
	} else {
		if agentResult.ErrorType != "" {
			result.ErrorType = agentResult.ErrorType
		} else if result.ErrorType == "" {
			result.ErrorType = "validation_failed"
		}
	}
	savePatch(caseCtx, caseWorkDir, result.FinalPatchPath)
	if patchApplyCheck(caseCtx, caseWorkDir, result.FinalPatchPath) {
		result.PatchApplyRate = 1
	} else if result.ErrorType == "" && !testsPassed {
		result.ErrorType = "patch_check_failed"
	}
	result.DurationSeconds = time.Since(caseStart).Seconds()
	result.Resolved = testsPassed && result.ErrorType == ""
	result.Timeline = timeline
	writeCaseArtifacts(artifactDir, c, agentResult, result)
	return result
}

func savePatch(ctx context.Context, dir, outputPath string) {
	if outputPath == "" {
		return
	}
	result := RunShellCommand(ctx, dir, "git add -N . >/dev/null 2>&1 || true; git diff --binary", 30*time.Second)
	_ = os.WriteFile(outputPath, []byte(result.Stdout), 0o644)
}

func patchApplyCheck(ctx context.Context, dir, patchPath string) bool {
	data, err := os.ReadFile(patchPath)
	if err != nil || strings.TrimSpace(string(data)) == "" {
		return false
	}
	result := RunShellCommand(ctx, dir, "git apply --check "+shellQuote(patchPath), 30*time.Second)
	return result.ExitCode == 0
}

func writeCaseArtifacts(dir string, c CaseSpec, agentResult AgentRunResult, result CaseResult) {
	_ = os.MkdirAll(dir, 0o755)
	setArtifactPaths(&result, dir)
	_ = os.WriteFile(result.PromptPath, []byte(c.Prompt), 0o644)
	writeTranscript(result.TranscriptPath, agentResult.Events)
	writeJSON(result.TimelinePath, result.Timeline)
	writeTestOutput(result.TestOutputPath, result)
	writeJSON(result.ResultPath, result)
}

func setArtifactPaths(result *CaseResult, dir string) {
	if result.PromptPath == "" {
		result.PromptPath = filepath.Join(dir, "prompt.txt")
	}
	if result.TranscriptPath == "" {
		result.TranscriptPath = filepath.Join(dir, "transcript.jsonl")
	}
	if result.TimelinePath == "" {
		result.TimelinePath = filepath.Join(dir, "timeline.json")
	}
	if result.FinalPatchPath == "" {
		result.FinalPatchPath = filepath.Join(dir, "patch.diff")
	}
	if result.TestOutputPath == "" {
		result.TestOutputPath = filepath.Join(dir, "test-output.txt")
	}
	if result.ResultPath == "" {
		result.ResultPath = filepath.Join(dir, "result.json")
	}
}

func writeTranscript(path string, events []agent.StreamEvent) {
	file, err := os.Create(path)
	if err != nil {
		return
	}
	defer file.Close()
	writer := bufio.NewWriter(file)
	defer writer.Flush()
	for _, event := range events {
		line, _ := json.Marshal(event)
		_, _ = writer.Write(append(line, '\n'))
	}
}

func writeJSON(path string, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(path, append(data, '\n'), 0o644)
}

func writeTestOutput(path string, result CaseResult) {
	var b strings.Builder
	for _, command := range append(append([]CommandResult{}, result.SetupResults...), result.TestResults...) {
		fmt.Fprintf(&b, "$ %s\nexit=%d timed_out=%t duration=%.3fs\n", command.Command, command.ExitCode, command.TimedOut, command.DurationSecond)
		if command.Stdout != "" {
			fmt.Fprintf(&b, "stdout:\n%s\n", command.Stdout)
		}
		if command.Stderr != "" {
			fmt.Fprintf(&b, "stderr:\n%s\n", command.Stderr)
		}
	}
	_ = os.WriteFile(path, []byte(b.String()), 0o644)
}

func BuildSummary(results []CaseResult) SuiteSummary {
	summary := SuiteSummary{
		TotalCases:              len(results),
		FailureCategories:       map[string]int{},
		BenchmarkSuiteBreakdown: map[string]float64{},
	}
	var durations, ttft, firstTool, firstTest, inputTokens, outputTokens, patchRates, testRates []float64
	var retrievedCounts, evidenceRecalls, evidenceMRRs, sourceRecalls, memoryTokenEstimates, memoryTokenRatios []float64
	var subtaskAnswerRates, memoryUpdateRates, previousSubtaskHitRates, retrievalDurations, staleUseRates []float64
	var evidenceMetricCases, evidenceHitCases int
	var failing []TopFailingCase
	byBenchmarkTotal := map[string]int{}
	byBenchmarkResolved := map[string]int{}
	for _, result := range results {
		durations = append(durations, result.DurationSeconds)
		if result.TTFTMillis != nil {
			ttft = append(ttft, *result.TTFTMillis)
		}
		if result.FirstToolCallMS != nil {
			firstTool = append(firstTool, *result.FirstToolCallMS)
		}
		if result.FirstTestMS != nil {
			firstTest = append(firstTest, *result.FirstTestMS)
		}
		inputTokens = append(inputTokens, float64(result.InputTokens))
		outputTokens = append(outputTokens, float64(result.OutputTokens))
		patchRates = append(patchRates, result.PatchApplyRate)
		testRates = append(testRates, result.TestPassRate)
		summary.TotalToolCalls += result.ToolCalls
		if result.MemoryMetrics != nil {
			m := result.MemoryMetrics
			retrievedCounts = append(retrievedCounts, float64(m.RetrievedCount))
			if m.EvidenceTotal > 0 {
				evidenceMetricCases++
				if m.EvidenceHit {
					evidenceHitCases++
				}
				evidenceRecalls = append(evidenceRecalls, m.EvidenceRecallAtK)
				evidenceMRRs = append(evidenceMRRs, m.EvidenceMRR)
			}
			if m.GoldSourceSessionCount > 0 {
				sourceRecalls = append(sourceRecalls, m.SourceSessionRecall)
			}
			if m.MemoryTokenEstimate > 0 {
				memoryTokenEstimates = append(memoryTokenEstimates, float64(m.MemoryTokenEstimate))
			}
			if m.MemoryTokenRatio > 0 {
				memoryTokenRatios = append(memoryTokenRatios, m.MemoryTokenRatio)
			}
			if m.RetrievalRuns > 0 {
				retrievalDurations = append(retrievalDurations, float64(m.RetrievalDurationMS)/float64(m.RetrievalRuns))
			}
			staleUseRates = append(staleUseRates, m.StaleUseRate)
			if m.SubtaskTotal > 0 {
				subtaskAnswerRates = append(subtaskAnswerRates, m.SubtaskAnswerRate)
				memoryUpdateRates = append(memoryUpdateRates, m.MemoryUpdateSuccessRate)
			}
			if m.SubtaskTotal > 1 {
				previousSubtaskHitRates = append(previousSubtaskHitRates, m.PreviousSubtaskHitRate)
			}
			if m.RetrievalErrorType != "" {
				if summary.Memory.RetrievalErrorCategories == nil {
					summary.Memory.RetrievalErrorCategories = map[string]int{}
				}
				summary.Memory.RetrievalErrorCategories[m.RetrievalErrorType]++
			}
		}
		byBenchmarkTotal[result.Case.Benchmark]++
		if result.Resolved {
			summary.ResolvedCases++
			byBenchmarkResolved[result.Case.Benchmark]++
			continue
		}
		errorType := result.ErrorType
		if errorType == "" {
			errorType = "unresolved"
		}
		summary.FailureCategories[errorType]++
		failing = append(failing, TopFailingCase{ID: result.Case.ID, ErrorType: errorType, Duration: result.DurationSeconds})
	}
	if summary.TotalCases > 0 {
		summary.PassRate = float64(summary.ResolvedCases) / float64(summary.TotalCases)
	}
	summary.AverageDurationSeconds = Average(durations)
	summary.DurationSeconds = latencySummary(durations)
	summary.TTFTMillis = latencySummary(ttft)
	summary.FirstToolCallMillis = latencySummary(firstTool)
	summary.FirstTestMillis = latencySummary(firstTest)
	summary.AverageInputTokens = Average(inputTokens)
	summary.AverageOutputTokens = Average(outputTokens)
	summary.InputTokens = latencySummary(inputTokens)
	summary.OutputTokens = latencySummary(outputTokens)
	summary.AveragePatchApplyRate = Average(patchRates)
	summary.AverageTestPassRate = Average(testRates)
	summary.Memory.AverageRetrievedCount = Average(retrievedCounts)
	if evidenceMetricCases > 0 {
		summary.Memory.EvidenceCaseCount = evidenceMetricCases
		summary.Memory.EvidenceHitCases = evidenceHitCases
		summary.Memory.EvidenceHitRate = float64(evidenceHitCases) / float64(evidenceMetricCases)
	}
	summary.Memory.AverageEvidenceRecallAtK = Average(evidenceRecalls)
	summary.Memory.AverageEvidenceMRR = Average(evidenceMRRs)
	summary.Memory.AverageSourceSessionRecall = Average(sourceRecalls)
	summary.Memory.AverageMemoryTokenEstimate = Average(memoryTokenEstimates)
	summary.Memory.AverageMemoryTokenRatio = Average(memoryTokenRatios)
	summary.Memory.AverageRetrievalDurationMS = Average(retrievalDurations)
	summary.Memory.AverageStaleUseRate = Average(staleUseRates)
	summary.Memory.AverageSubtaskAnswerRate = Average(subtaskAnswerRates)
	summary.Memory.AverageMemoryUpdateSuccessRate = Average(memoryUpdateRates)
	summary.Memory.AveragePreviousSubtaskHitRate = Average(previousSubtaskHitRates)
	for benchmark, total := range byBenchmarkTotal {
		if total > 0 {
			summary.BenchmarkSuiteBreakdown[benchmark] = float64(byBenchmarkResolved[benchmark]) / float64(total)
		}
	}
	sort.Slice(failing, func(i, j int) bool {
		return failing[i].Duration > failing[j].Duration
	})
	if len(failing) > 10 {
		failing = failing[:10]
	}
	summary.TopFailingCases = failing
	return summary
}
