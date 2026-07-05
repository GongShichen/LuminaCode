package benchmark

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	ReportPrefix = "unified-eval-report-"
	ReportSuffix = ".txt"
)

type UnifiedReportOptions struct {
	OutputDir       string
	WorkDir         string
	Keep            int
	BaselineProfile *string
	Tiers           []string
	Now             func() time.Time
	Stdout          io.Writer
	Stderr          io.Writer
}

type BenchmarkSuiteOptions struct {
	OutputDir       string
	WorkDir         string
	Keep            int
	BaselineProfile *string
	Tiers           []string
	RepoRoot        string
	Stdout          io.Writer
	Stderr          io.Writer
	RunTests        func(context.Context, string, io.Writer, io.Writer) int
	RunReport       func(context.Context, UnifiedReportOptions) int
}

func BuildOutputPath(outputDir string, now time.Time) string {
	return filepath.Join(outputDir, fmt.Sprintf("%s%s%s", ReportPrefix, now.Format("20060102-150405"), ReportSuffix))
}

func PruneOldReports(outputDir string, keep int) error {
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return err
	}
	var reportFiles []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, ReportPrefix) && strings.HasSuffix(name, ReportSuffix) {
			reportFiles = append(reportFiles, name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(reportFiles)))
	if keep >= len(reportFiles) {
		return nil
	}
	for _, name := range reportFiles[keep:] {
		if err := os.Remove(filepath.Join(outputDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func RunUnifiedEvalReport(ctx context.Context, options UnifiedReportOptions) int {
	stdout := writerOrDefault(options.Stdout, os.Stdout)
	stderr := writerOrDefault(options.Stderr, os.Stderr)
	if options.Keep == 0 {
		options.Keep = 4
	}
	now := time.Now
	if options.Now != nil {
		now = options.Now
	}
	if options.OutputDir == "" {
		options.OutputDir = filepath.Join("docs", "reports")
	}
	if options.WorkDir == "" {
		options.WorkDir = filepath.Join(".tmp", "unified-eval-run")
	}
	if err := os.MkdirAll(options.OutputDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "unified eval report failed: %v\n", err)
		return 2
	}
	if err := os.MkdirAll(options.WorkDir, 0o755); err != nil {
		fmt.Fprintf(stderr, "unified eval report failed: %v\n", err)
		return 2
	}
	buildOptions := BuildReportOptions{Tiers: options.Tiers}
	plugins := DefaultBenchmarkPlugins()
	if options.BaselineProfile != nil {
		buildOptions.BaselinePlugins = plugins
		buildOptions.BaselineProfile = options.BaselineProfile
	}
	report, err := BuildBenchmarkReport(ctx, plugins, buildOptions)
	if err != nil {
		fmt.Fprintf(stderr, "unified eval report failed: %v\n", err)
		return 2
	}
	summary := FormatBenchmarkReport(report)
	fmt.Fprintln(stdout, summary)
	outputPath := BuildOutputPath(options.OutputDir, now())
	if err := os.WriteFile(outputPath, []byte(summary), 0o644); err != nil {
		fmt.Fprintf(stderr, "unified eval report failed: %v\n", err)
		return 2
	}
	if err := PruneOldReports(options.OutputDir, options.Keep); err != nil {
		fmt.Fprintf(stderr, "unified eval report failed: %v\n", err)
		return 2
	}
	if report.Passed {
		return 0
	}
	return 1
}

func RunBenchmarkTests(ctx context.Context, repoRoot string, stdout, stderr io.Writer) int {
	if repoRoot == "" {
		repoRoot = "."
	}
	cmd := exec.CommandContext(ctx, "go", "test", "./test", "-run", "Benchmark", "-count=1")
	cmd.Dir = repoRoot
	cmd.Stdout = writerOrDefault(stdout, os.Stdout)
	cmd.Stderr = writerOrDefault(stderr, os.Stderr)
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode()
		}
		return 1
	}
	return 0
}

func RunBenchmarkSuite(ctx context.Context, options BenchmarkSuiteOptions) (exitCode int) {
	stdout := writerOrDefault(options.Stdout, os.Stdout)
	stderr := writerOrDefault(options.Stderr, os.Stderr)
	defer func() {
		if recovered := recover(); recovered != nil {
			fmt.Fprintf(stderr, "benchmark suite runner failed: %v\n", recovered)
			exitCode = 4
		}
	}()
	return runBenchmarkSuite(ctx, options, stdout, stderr)
}

func runBenchmarkSuite(ctx context.Context, options BenchmarkSuiteOptions, stdout, stderr io.Writer) int {
	if options.Keep == 0 {
		options.Keep = 4
	}
	runTests := options.RunTests
	if runTests == nil {
		runTests = RunBenchmarkTests
	}
	runReport := options.RunReport
	if runReport == nil {
		runReport = RunUnifiedEvalReport
	}
	fmt.Fprintln(stdout, "==> Running benchmark tests")
	testExitCode := runTests(ctx, options.RepoRoot, stdout, stderr)
	if testExitCode != 0 {
		fmt.Fprintln(stderr, "benchmark tests failed; skipping report generation")
		return 3
	}
	fmt.Fprintln(stdout, "==> Generating benchmark report")
	reportExitCode := runReport(ctx, UnifiedReportOptions{
		OutputDir:       options.OutputDir,
		WorkDir:         options.WorkDir,
		Keep:            options.Keep,
		BaselineProfile: options.BaselineProfile,
		Tiers:           options.Tiers,
		Stdout:          stdout,
		Stderr:          stderr,
	})
	if reportExitCode == 2 {
		return 2
	}
	return reportExitCode
}

func writerOrDefault(candidate io.Writer, fallback io.Writer) io.Writer {
	if candidate != nil {
		return candidate
	}
	return fallback
}
