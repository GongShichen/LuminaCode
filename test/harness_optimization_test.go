package test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/benchmark/agentbench"
	"LuminaCode/config"
	coretools "LuminaCode/tools"
)

func TestTerminalBenchHarnessPromptAppendixIsModeScoped(t *testing.T) {
	if got := agent.HarnessSystemPromptAppendix(""); got != "" {
		t.Fatalf("ordinary mode should not inject harness prompt, got %q", got)
	}
	appendix := agent.HarnessSystemPromptAppendix("terminal-bench")
	if !strings.Contains(appendix, "/app/results.txt") || !strings.Contains(appendix, "Do not finish with only a verbal explanation") {
		t.Fatalf("terminal-bench appendix missing artifact instructions: %q", appendix)
	}
}

func TestBashTimeoutAcceptsNumericStringsAndKeepsLegacyMilliseconds(t *testing.T) {
	legacySeconds, errMsg := coretools.ParseBashTimeoutSecondsForTest(30000, nil, 30)
	if errMsg != "" || legacySeconds != 30 {
		t.Fatalf("legacy int timeout should stay milliseconds, got seconds=%v err=%q", legacySeconds, errMsg)
	}
	stringSeconds, errMsg := coretools.ParseBashTimeoutSecondsForTest("120", nil, 30)
	if errMsg != "" || stringSeconds != 120 {
		t.Fatalf("numeric string timeout should be seconds, got seconds=%v err=%q", stringSeconds, errMsg)
	}
	timeoutSeconds, errMsg := coretools.ParseBashTimeoutSecondsForTest(nil, "45", 30)
	if errMsg != "" || timeoutSeconds != 45 {
		t.Fatalf("timeout_seconds string should be seconds, got seconds=%v err=%q", timeoutSeconds, errMsg)
	}
	capped, errMsg := coretools.ParseBashTimeoutSecondsForTest("999", nil, 30)
	if errMsg != "" || capped != 120 {
		t.Fatalf("timeout should cap at 120 seconds, got seconds=%v err=%q", capped, errMsg)
	}
	if _, errMsg := coretools.ParseBashTimeoutSecondsForTest("later", nil, 30); errMsg == "" {
		t.Fatal("invalid timeout string should return an error message")
	}
}

func TestTerminalBenchDiagnosticsAreAggregatedWithoutChangingOfficialScore(t *testing.T) {
	tmp := t.TempDir()
	benchmarkDir := filepath.Join(tmp, "terminal-bench")
	outputDir := filepath.Join(tmp, "reports")
	workDir := filepath.Join(tmp, "work")
	artifactsDir := filepath.Join(tmp, "artifacts")
	for _, dir := range []string{benchmarkDir, outputDir, workDir, artifactsDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	diagnosticsJSON := `{"agent_exit_status":0,"final_agent_exit_status":0,"explicit_artifact_checks":[{"path":"/app/answer.txt","concrete":true,"exists":false}],"explicit_missing_artifacts":["/app/answer.txt"],"post_flight_repair":{"triggered":true,"exit_status":0},"failure_category":"missing_artifact"}`
	harnessCmd := "mkdir -p \"$LUMINA_BENCHMARK_OUTPUT_DIR/tb-runs/test-run/case-a/logs\"; " +
		"printf '%s\n' '" + diagnosticsJSON + "' > \"$LUMINA_BENCHMARK_OUTPUT_DIR/tb-runs/test-run/case-a/logs/lumina-diagnostics.json\"; " +
		"echo '{\"total\":1,\"resolved\":0}' # --run-id test-run"

	report, err := agentbench.RunSuite(context.Background(), agentbench.RunnerOptions{
		Suite:        agentbench.SuiteTerminalBench,
		RootDir:      tmp,
		OutputDir:    outputDir,
		WorkDir:      workDir,
		ArtifactsDir: artifactsDir,
		BenchmarkDir: benchmarkDir,
		HarnessCmd:   harnessCmd,
		Config:       config.Config{APIModel: "fake-model", APIType: "openai_compatible", CWD: tmp},
		Now:          func() time.Time { return time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.ResolvedCases != 0 || report.Summary.TotalCases != 1 {
		t.Fatalf("official score should come from harness metrics, got %#v", report.Summary)
	}
	if len(report.LuminaDiagnostics) != 1 {
		t.Fatalf("expected diagnostics to be collected, got %#v", report.LuminaDiagnostics)
	}
	if report.LuminaDiagnostics[0].TaskID != "case-a" || !report.LuminaDiagnostics[0].PostFlightRepair.Triggered {
		t.Fatalf("diagnostic details not preserved: %#v", report.LuminaDiagnostics[0])
	}
	if report.Summary.FailureCategories["missing_artifact"] != 1 {
		t.Fatalf("missing_artifact category not merged: %#v", report.Summary.FailureCategories)
	}
}
