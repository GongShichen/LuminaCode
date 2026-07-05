package test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"LuminaCode/benchmark"
)

func TestBenchmarkReportRunnerOutputPathAndPruningMatchPython(t *testing.T) {
	tmp := t.TempDir()
	outputPath := benchmark.BuildOutputPath(tmp, time.Date(2026, 6, 3, 14, 35, 22, 0, time.UTC))
	if outputPath != filepath.Join(tmp, "unified-eval-report-20260603-143522.txt") {
		t.Fatalf("output path=%q", outputPath)
	}
	for _, name := range []string{
		"unified-eval-report-20260603-140000.txt",
		"unified-eval-report-20260603-140100.txt",
		"unified-eval-report-20260603-140200.txt",
		"unified-eval-report-20260603-140300.txt",
		"unified-eval-report-20260603-140400.txt",
		"other-report.txt",
	} {
		if err := os.WriteFile(filepath.Join(tmp, name), []byte(name), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := benchmark.PruneOldReports(tmp, 4); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(tmp)
	if err != nil {
		t.Fatal(err)
	}
	var remaining []string
	for _, entry := range entries {
		remaining = append(remaining, entry.Name())
	}
	sort.Strings(remaining)
	want := []string{
		"other-report.txt",
		"unified-eval-report-20260603-140100.txt",
		"unified-eval-report-20260603-140200.txt",
		"unified-eval-report-20260603-140300.txt",
		"unified-eval-report-20260603-140400.txt",
	}
	if !reflect.DeepEqual(remaining, want) {
		t.Fatalf("remaining=%#v want %#v", remaining, want)
	}
}

func TestRunUnifiedEvalReportWritesFileAndForwardsOptionsLikePython(t *testing.T) {
	tmp := t.TempDir()
	var stdout bytes.Buffer
	profile := "memory_off"
	exitCode := benchmark.RunUnifiedEvalReport(context.Background(), benchmark.UnifiedReportOptions{
		OutputDir:       filepath.Join(tmp, "reports"),
		WorkDir:         filepath.Join(tmp, "work"),
		Keep:            4,
		BaselineProfile: &profile,
		Tiers:           []string{"smoke"},
		Now:             func() time.Time { return time.Date(2026, 6, 3, 14, 35, 22, 0, time.UTC) },
		Stdout:          &stdout,
	})
	if exitCode != 0 {
		t.Fatalf("exitCode=%d stdout:\n%s", exitCode, stdout.String())
	}
	written := filepath.Join(tmp, "reports", "unified-eval-report-20260603-143522.txt")
	content, err := os.ReadFile(written)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != strings.TrimSuffix(stdout.String(), "\n") {
		t.Fatalf("written report should match printed report")
	}
	for _, want := range []string{
		"# 基准评测报告",
		"- 基线变体: baseline",
		"| `selected_recall_cases` | `1.000` |",
		"| `selected_security_cases` | `4.000` |",
		"`recall_mean_f1_at_k_delta=1.000`",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("report missing %q:\n%s", want, stdout.String())
		}
	}
}

func TestRunBenchmarkSuiteExitCodesAndForwardingMatchPython(t *testing.T) {
	tmp := t.TempDir()
	var stdout, stderr bytes.Buffer
	reportCalled := false
	code := benchmark.RunBenchmarkSuite(context.Background(), benchmark.BenchmarkSuiteOptions{
		OutputDir: filepath.Join(tmp, "reports"),
		WorkDir:   filepath.Join(tmp, "work"),
		Keep:      4,
		Stdout:    &stdout,
		Stderr:    &stderr,
		RunTests: func(context.Context, string, io.Writer, io.Writer) int {
			return 1
		},
		RunReport: func(context.Context, benchmark.UnifiedReportOptions) int {
			reportCalled = true
			return 0
		},
	})
	if code != 3 || reportCalled {
		t.Fatalf("failed tests should return 3 and skip report, code=%d reportCalled=%t", code, reportCalled)
	}
	if !strings.Contains(stderr.String(), "benchmark tests failed; skipping report generation") {
		t.Fatalf("missing failure message: %q", stderr.String())
	}

	stdout.Reset()
	stderr.Reset()
	profile := "memory_off"
	var captured benchmark.UnifiedReportOptions
	code = benchmark.RunBenchmarkSuite(context.Background(), benchmark.BenchmarkSuiteOptions{
		OutputDir:       filepath.Join(tmp, "reports"),
		WorkDir:         filepath.Join(tmp, "work"),
		Keep:            2,
		BaselineProfile: &profile,
		Tiers:           []string{"smoke"},
		Stdout:          &stdout,
		Stderr:          &stderr,
		RunTests: func(context.Context, string, io.Writer, io.Writer) int {
			return 0
		},
		RunReport: func(_ context.Context, options benchmark.UnifiedReportOptions) int {
			captured = options
			return 1
		},
	})
	if code != 1 {
		t.Fatalf("suite code=%d want report code 1", code)
	}
	if captured.BaselineProfile == nil || *captured.BaselineProfile != "memory_off" || !reflect.DeepEqual(captured.Tiers, []string{"smoke"}) || captured.Keep != 2 {
		t.Fatalf("report options not forwarded: %#v", captured)
	}

	stdout.Reset()
	stderr.Reset()
	code = benchmark.RunBenchmarkSuite(context.Background(), benchmark.BenchmarkSuiteOptions{
		Stdout: &stdout,
		Stderr: &stderr,
		RunTests: func(context.Context, string, io.Writer, io.Writer) int {
			return 0
		},
		RunReport: func(context.Context, benchmark.UnifiedReportOptions) int {
			return 2
		},
	})
	if code != 2 {
		t.Fatalf("report runtime failure code should be forwarded as 2 like Python, got %d", code)
	}

	stdout.Reset()
	stderr.Reset()
	code = benchmark.RunBenchmarkSuite(context.Background(), benchmark.BenchmarkSuiteOptions{
		Stdout: &stdout,
		Stderr: &stderr,
		RunTests: func(context.Context, string, io.Writer, io.Writer) int {
			panic("boom")
		},
	})
	if code != 4 || !strings.Contains(stderr.String(), "benchmark suite runner failed: boom") {
		t.Fatalf("wrapper panic should return 4 with Python error prefix, code=%d stderr=%q", code, stderr.String())
	}
}
