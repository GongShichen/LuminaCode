package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"LuminaCode/benchmark/agentbench"
	"LuminaCode/config"
)

func main() {
	suite := flag.String("suite", agentbench.SuiteAiderPolyglotSmoke, "Benchmark suite: terminal_bench, tau_bench, swebench_verified, aider_polyglot_smoke, swebench_verified_subset, terminal_bench_smoke, longmemeval.")
	casesPath := flag.String("cases", "", "Optional JSON/JSONL case manifest path.")
	caseID := flag.String("case", "", "Optional upstream case/task identifier. Debug mode only; passed to official harness via LUMINA_BENCHMARK_CASE.")
	limit := flag.Int("limit", 0, "Optional case limit.")
	root := flag.String("root", agentbench.DefaultRootDir(), "Benchmark content root. Defaults under ~/Documents/benchmark.")
	benchmarkDir := flag.String("benchmark-dir", "", "Official benchmark checkout directory. Defaults to <root>/<benchmark-name> for official suites.")
	outputDir := flag.String("output-dir", "", "Report directory. Defaults to <root>/reports.")
	workDir := flag.String("work-dir", "", "Case working directory. Defaults to <root>/work.")
	artifactsDir := flag.String("artifacts-dir", "", "Artifact directory. Defaults to <root>/artifacts.")
	timeout := flag.Int("timeout", agentbench.DefaultCaseTimeout, "Default per-case timeout in seconds. Use 0 for no limit.")
	caseParallel := flag.Int("case-parallel", 1, "Number of benchmark cases to run concurrently. Case-internal steps remain serial.")
	resume := flag.Bool("resume", true, "Resume from the suite checkpoint and skip completed cases.")
	keep := flag.Int("keep", 8, "How many timestamped report pairs to retain.")
	harnessCmd := flag.String("harness-cmd", "", "Official benchmark harness command for terminal_bench, tau_bench, or swebench_verified.")
	swebenchHarnessCmd := flag.String("swebench-harness-cmd", "", "Legacy SWE-bench harness command. Prefer -harness-cmd.")
	preparedEnv := flag.Bool("prepared-env", false, "Require the official harness to reuse prebuilt benchmark environments. For terminal_bench this validates --no-rebuild and --no-cleanup.")
	longMemEvalPhase := flag.String("longmemeval-phase", "", "Required for LongMemEval: prepare, answer, or evaluate. Phases never auto-chain.")
	longMemEvalIndexDir := flag.String("longmemeval-index-dir", "", "Prepared LongMemEval index directory. Defaults to <work-dir>/longmemeval-prepared-index.")
	longMemEvalIndexSource := flag.String("longmemeval-index-source", "", "Existing per-case store source. Defaults to <work-dir>/cases.")
	longMemEvalRunID := flag.String("longmemeval-run-id", "", "Answer run identifier used for isolated retrieval traces and checkpoints.")
	longMemEvalPredictions := flag.String("longmemeval-predictions", "", "Prediction JSONL to validate and evaluate in the evaluate phase.")
	longMemEvalSmokeSize := flag.Int("longmemeval-smoke-size", 0, "Deterministic question-type-stratified LongMemEval smoke size. Reuse the same value across prepare, answer, and evaluate.")
	longMemEvalQuestionType := flag.String("longmemeval-question-type", "", "Optional LongMemEval question type filter for a deterministic smoke run.")
	flag.Parse()
	if *suite == agentbench.SuiteLongMemEval {
		phase := strings.TrimSpace(*longMemEvalPhase)
		if phase != agentbench.LongMemEvalPhasePrepare && phase != agentbench.LongMemEvalPhaseAnswer && phase != agentbench.LongMemEvalPhaseEvaluate {
			fmt.Fprintln(os.Stderr, "LongMemEval requires -longmemeval-phase prepare|answer|evaluate")
			os.Exit(2)
		}
	}

	cfg := config.NewConfig()
	cfg.Yolo = true
	requiresAPI := *suite != agentbench.SuiteLongMemEval || strings.TrimSpace(*longMemEvalPhase) != agentbench.LongMemEvalPhasePrepare
	if requiresAPI && (cfg.APIKey == "" || cfg.APIBaseURL == "" || cfg.APIModel == "") {
		fmt.Fprintln(os.Stderr, "agent benchmark requires API key, base URL, and model configuration")
		os.Exit(2)
	}
	rootDir := expandHome(*root)
	if *outputDir == "" {
		*outputDir = filepath.Join(rootDir, "reports")
	}
	if *workDir == "" {
		*workDir = filepath.Join(rootDir, "work")
	}
	if *artifactsDir == "" {
		*artifactsDir = filepath.Join(rootDir, "artifacts")
	}
	start := time.Now()
	report, err := agentbench.RunSuite(context.Background(), agentbench.RunnerOptions{
		Suite:                   *suite,
		CasesPath:               expandHome(*casesPath),
		CaseID:                  *caseID,
		Limit:                   *limit,
		RootDir:                 rootDir,
		OutputDir:               expandHome(*outputDir),
		WorkDir:                 expandHome(*workDir),
		ArtifactsDir:            expandHome(*artifactsDir),
		BenchmarkDir:            expandHome(*benchmarkDir),
		TimeoutSeconds:          *timeout,
		CaseParallel:            *caseParallel,
		NoResume:                !*resume,
		Config:                  cfg,
		HarnessCmd:              *harnessCmd,
		SWEBenchHarnessCmd:      *swebenchHarnessCmd,
		PreparedEnv:             *preparedEnv,
		LongMemEvalPhase:        strings.TrimSpace(*longMemEvalPhase),
		LongMemEvalIndexDir:     expandHome(*longMemEvalIndexDir),
		LongMemEvalIndexSource:  expandHome(*longMemEvalIndexSource),
		LongMemEvalRunID:        strings.TrimSpace(*longMemEvalRunID),
		LongMemEvalPredictions:  expandHome(*longMemEvalPredictions),
		LongMemEvalSmokeSize:    *longMemEvalSmokeSize,
		LongMemEvalQuestionType: strings.TrimSpace(*longMemEvalQuestionType),
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "agent benchmark failed: %v\n", err)
		os.Exit(2)
	}
	jsonPath, mdPath, err := agentbench.WriteReport(report, report.OutputDir, start)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to write report: %v\n", err)
		os.Exit(2)
	}
	if err := agentbench.PruneOldReports(report.OutputDir, *keep); err != nil {
		fmt.Fprintf(os.Stderr, "failed to prune old reports: %v\n", err)
		os.Exit(2)
	}
	fmt.Printf("Suite: %s\n", report.Suite)
	fmt.Printf("Resolved: %d/%d (%.2f%%)\n", report.Summary.ResolvedCases, report.Summary.TotalCases, report.Summary.PassRate*100)
	fmt.Printf("JSON report: %s\n", jsonPath)
	fmt.Printf("Markdown report: %s\n", mdPath)
	if report.PredictionsPath != "" {
		fmt.Printf("Predictions: %s\n", report.PredictionsPath)
	}
	if report.HarnessOutputPath != "" {
		fmt.Printf("Harness output: %s\n", report.HarnessOutputPath)
	}
	if report.DebugRun {
		fmt.Println("Debug run: --limit or --case was used; do not treat this as an official full benchmark score.")
	}
	if report.HarnessExitCode != nil && *report.HarnessExitCode != 0 {
		os.Exit(1)
	}
	if agentbench.IsOfficialSuite(report.Suite) {
		return
	}
	if report.Suite == agentbench.SuiteLongMemEval {
		return
	}
	if report.Summary.ResolvedCases != report.Summary.TotalCases {
		os.Exit(1)
	}
}

func expandHome(path string) string {
	if path == "" {
		return ""
	}
	if path == "~" || strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err == nil && home != "" {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}
