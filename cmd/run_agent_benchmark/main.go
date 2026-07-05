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
	suite := flag.String("suite", agentbench.SuiteAiderPolyglotSmoke, "Benchmark suite: terminal_bench, tau_bench, swebench_verified, aider_polyglot_smoke, swebench_verified_subset, terminal_bench_smoke.")
	casesPath := flag.String("cases", "", "Optional JSON/JSONL case manifest path.")
	caseID := flag.String("case", "", "Optional upstream case/task identifier. Debug mode only; passed to official harness via LUMINA_BENCHMARK_CASE.")
	limit := flag.Int("limit", 0, "Optional case limit.")
	root := flag.String("root", agentbench.DefaultRootDir(), "Benchmark content root. Defaults under ~/Documents/benchmark.")
	benchmarkDir := flag.String("benchmark-dir", "", "Official benchmark checkout directory. Defaults to <root>/<benchmark-name> for official suites.")
	outputDir := flag.String("output-dir", "", "Report directory. Defaults to <root>/reports.")
	workDir := flag.String("work-dir", "", "Case working directory. Defaults to <root>/work.")
	artifactsDir := flag.String("artifacts-dir", "", "Artifact directory. Defaults to <root>/artifacts.")
	timeout := flag.Int("timeout", agentbench.DefaultCaseTimeout, "Default per-case timeout in seconds.")
	keep := flag.Int("keep", 8, "How many timestamped report pairs to retain.")
	harnessCmd := flag.String("harness-cmd", "", "Official benchmark harness command for terminal_bench, tau_bench, or swebench_verified.")
	swebenchHarnessCmd := flag.String("swebench-harness-cmd", "", "Legacy SWE-bench harness command. Prefer -harness-cmd.")
	flag.Parse()

	cfg := config.NewConfig()
	cfg.Yolo = true
	cfg.AutoMemoryEnabled = false
	cfg.AutoMemoryDirectory = nil
	if cfg.APIKey == "" || cfg.APIBaseURL == "" || cfg.APIModel == "" {
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
		Suite:              *suite,
		CasesPath:          expandHome(*casesPath),
		CaseID:             *caseID,
		Limit:              *limit,
		RootDir:            rootDir,
		OutputDir:          expandHome(*outputDir),
		WorkDir:            expandHome(*workDir),
		ArtifactsDir:       expandHome(*artifactsDir),
		BenchmarkDir:       expandHome(*benchmarkDir),
		TimeoutSeconds:     *timeout,
		Config:             cfg,
		HarnessCmd:         *harnessCmd,
		SWEBenchHarnessCmd: *swebenchHarnessCmd,
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
		fmt.Printf("SWE-bench predictions: %s\n", report.PredictionsPath)
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
