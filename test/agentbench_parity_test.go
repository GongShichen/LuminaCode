package test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/benchmark/agentbench"
	"LuminaCode/config"
)

func TestAgentBenchLoadCasesJSONAndLimit(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "cases.json")
	data := `[
		{"id":"one","benchmark":"terminal_bench_smoke","prompt":"do one"},
		{"id":"two","benchmark":"terminal_bench_smoke","prompt":"do two"}
	]`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err := agentbench.LoadCases(agentbench.SuiteTerminalBenchSmoke, path, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) != 1 || cases[0].ID != "one" {
		t.Fatalf("cases=%#v", cases)
	}
	if cases[0].TimeoutSeconds != agentbench.DefaultCaseTimeout {
		t.Fatalf("default timeout not applied: %#v", cases[0])
	}
}

func TestMemoryBenchmarkMetricsDoNotDependOnSessionNamePrefixes(t *testing.T) {
	sourcePath := filepath.Join("..", "benchmark", "agentbench", "memory_bench.go")
	source, err := os.ReadFile(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	for _, forbidden := range []string{"countMemoriesBySourcePrefix", `HasPrefix(hit.SourceSessionID`, `"memoryarena-"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("memory benchmark metrics must use exact persisted provenance, found %q", forbidden)
		}
	}
}

func TestAgentBenchPercentileInterpolation(t *testing.T) {
	if got := agentbench.Percentile(nil, 90); got != nil {
		t.Fatalf("empty percentile should be nil")
	}
	single := agentbench.Percentile([]float64{7}, 95)
	if single == nil || *single != 7 {
		t.Fatalf("single percentile=%v", single)
	}
	values := []float64{1, 2, 3, 4}
	p50 := agentbench.Percentile(values, 50)
	p90 := agentbench.Percentile(values, 90)
	p95 := agentbench.Percentile(values, 95)
	if p50 == nil || *p50 != 2.5 {
		t.Fatalf("p50=%v", p50)
	}
	if p90 == nil || *p90 < 3.69 || *p90 > 3.71 {
		t.Fatalf("p90=%v", p90)
	}
	if p95 == nil || *p95 < 3.84 || *p95 > 3.86 {
		t.Fatalf("p95=%v", p95)
	}
}

func TestAgentBenchRunShellCommandCapturesExitAndTimeout(t *testing.T) {
	tmp := t.TempDir()
	ok := agentbench.RunShellCommand(context.Background(), tmp, "printf ok", time.Second)
	if ok.ExitCode != 0 || ok.Stdout != "ok" {
		t.Fatalf("ok result=%#v", ok)
	}
	timeout := agentbench.RunShellCommand(context.Background(), tmp, "sleep 1", 20*time.Millisecond)
	if !timeout.TimedOut || timeout.ExitCode == 0 {
		t.Fatalf("timeout result=%#v", timeout)
	}
}

func TestAgentBenchBuildSummaryIncludesTailMetrics(t *testing.T) {
	ttftA, ttftB := 100.0, 500.0
	firstTool := 250.0
	firstTest := 1000.0
	summary := agentbench.BuildSummary([]agentbench.CaseResult{
		{
			Case:            agentbench.CaseSpec{ID: "pass", Benchmark: agentbench.SuiteTerminalBenchSmoke},
			Resolved:        true,
			DurationSeconds: 1,
			TTFTMillis:      &ttftA,
			FirstToolCallMS: &firstTool,
			FirstTestMS:     &firstTest,
			InputTokens:     100,
			OutputTokens:    20,
			PatchApplyRate:  1,
			TestPassRate:    1,
		},
		{
			Case:            agentbench.CaseSpec{ID: "fail", Benchmark: agentbench.SuiteTerminalBenchSmoke},
			Resolved:        false,
			DurationSeconds: 3,
			TTFTMillis:      &ttftB,
			InputTokens:     300,
			OutputTokens:    40,
			ErrorType:       "validation_failed",
		},
	})
	if summary.PassRate != 0.5 || summary.ResolvedCases != 1 {
		t.Fatalf("summary=%#v", summary)
	}
	if summary.DurationSeconds.P90 == nil || *summary.DurationSeconds.P90 < 2.79 || *summary.DurationSeconds.P90 > 2.81 {
		t.Fatalf("duration p90=%v", summary.DurationSeconds.P90)
	}
	if summary.TTFTMillis.P95 == nil || *summary.TTFTMillis.P95 < 479 || *summary.TTFTMillis.P95 > 481 {
		t.Fatalf("ttft p95=%v", summary.TTFTMillis.P95)
	}
	if summary.FailureCategories["validation_failed"] != 1 {
		t.Fatalf("failure categories=%#v", summary.FailureCategories)
	}
}

func TestAgentBenchRunSuiteWithFakeAgentWritesArtifacts(t *testing.T) {
	tmp := t.TempDir()
	report, err := agentbench.RunSuite(context.Background(), agentbench.RunnerOptions{
		Suite:        agentbench.SuiteTerminalBenchSmoke,
		Limit:        1,
		RootDir:      tmp,
		OutputDir:    filepath.Join(tmp, "reports"),
		WorkDir:      filepath.Join(tmp, "work"),
		ArtifactsDir: filepath.Join(tmp, "artifacts"),
		Config:       config.Config{APIModel: "fake-model", CWD: tmp},
		AgentRunner:  fakeAgentRunner{},
		Now:          func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Summary.PassRate != 1 || len(report.Results) != 1 {
		t.Fatalf("report=%#v", report)
	}
	result := report.Results[0]
	for _, path := range []string{result.PromptPath, result.TranscriptPath, result.TimelinePath, result.FinalPatchPath, result.TestOutputPath, result.ResultPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing artifact %s: %v", path, err)
		}
	}
	if result.TTFTMillis == nil || *result.TTFTMillis != 10 {
		t.Fatalf("ttft=%v", result.TTFTMillis)
	}
	if result.FirstToolCallMS == nil || *result.FirstToolCallMS != 20 {
		t.Fatalf("first tool=%v", result.FirstToolCallMS)
	}
	if result.FirstTestMS == nil {
		t.Fatalf("missing first test metric")
	}
	jsonPath, mdPath, err := agentbench.WriteReport(report, filepath.Join(tmp, "reports"), time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(jsonPath); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mdPath); err != nil {
		t.Fatal(err)
	}
}

func TestHeadlessAgentRetriesInitialEOFInsideAPIClient(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Fatal("test server does not support hijacking")
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_ = connection.Close()
			return
		}
		writeOpenAIStream(w, "recovered after eof", 3, 4)
	}))
	defer server.Close()

	cfg := config.NewConfigForCWD(t.TempDir())
	cfg.APIType = "openai_compatible"
	cfg.APIBaseURL = server.URL
	cfg.APIKey = "test-key"
	cfg.APIModel = "custom-router-model"
	cfg.APIMaxTokens = 256
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false
	cfg.LongTermMemoryEnabled = false
	config.PinFields(&cfg, "api_key", "api_base_url", "api_type", "api_model", "api_max_tokens", "mcp_enabled", "skills_enabled", "long_term_memory_enabled")
	result := agentbench.HeadlessAgentRunner{}.Run(context.Background(), cfg, "hello", "headless-eof")
	if calls.Load() != 2 {
		t.Fatalf("expected transport retry inside API client, calls=%d", calls.Load())
	}
	if result.ErrorType != "" || len(result.TransientErrors) != 0 || !strings.Contains(result.FinalText, "recovered after eof") {
		t.Fatalf("initial EOF leaked into final result: %#v", result)
	}
}

func TestHeadlessAgentDoesNotTreatRecoveredOuterRetryAsFinalError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) == 1 {
			http.Error(w, "temporary router failure", http.StatusBadRequest)
			return
		}
		writeOpenAIStream(w, "outer retry recovered", 3, 4)
	}))
	defer server.Close()

	cfg := config.NewConfigForCWD(t.TempDir())
	cfg.APIType = "openai_compatible"
	cfg.APIBaseURL = server.URL
	cfg.APIKey = "test-key"
	cfg.APIModel = "custom-router-model"
	cfg.APIMaxTokens = 256
	cfg.MCPEnabled = false
	cfg.SkillsEnabled = false
	cfg.LongTermMemoryEnabled = false
	config.PinFields(&cfg, "api_key", "api_base_url", "api_type", "api_model", "api_max_tokens", "mcp_enabled", "skills_enabled", "long_term_memory_enabled")
	result := agentbench.HeadlessAgentRunner{}.Run(context.Background(), cfg, "hello", "headless-outer-retry")
	if calls.Load() != 2 {
		t.Fatalf("expected outer agent retry, calls=%d", calls.Load())
	}
	if result.ErrorType != "" || len(result.TransientErrors) != 1 || !strings.Contains(result.FinalText, "outer retry recovered") {
		t.Fatalf("recovered outer retry was classified as final failure: %#v", result)
	}
}

func TestOfficialBenchmarkReportPathsAreSeparated(t *testing.T) {
	tmp := t.TempDir()
	jsonPath, mdPath := agentbench.BuildSuiteReportPaths(tmp, agentbench.SuiteTerminalBench, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	if filepath.Base(jsonPath) != "terminal-bench-20260705-120000.json" || filepath.Base(mdPath) != "terminal-bench-20260705-120000.md" {
		t.Fatalf("terminal report paths=%s %s", jsonPath, mdPath)
	}
	jsonPath, mdPath = agentbench.BuildSuiteReportPaths(tmp, agentbench.SuiteTauBench, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	if filepath.Base(jsonPath) != "tau-bench-20260705-120000.json" || filepath.Base(mdPath) != "tau-bench-20260705-120000.md" {
		t.Fatalf("tau report paths=%s %s", jsonPath, mdPath)
	}
	jsonPath, mdPath = agentbench.BuildSuiteReportPaths(tmp, agentbench.SuiteSWEBenchVerified, time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC))
	if filepath.Base(jsonPath) != "swebench-verified-20260705-120000.json" || filepath.Base(mdPath) != "swebench-verified-20260705-120000.md" {
		t.Fatalf("swebench report paths=%s %s", jsonPath, mdPath)
	}
}

func TestOfficialBenchmarkAdapterRunsHarnessWithoutMutatingUpstream(t *testing.T) {
	tmp := t.TempDir()
	upstream := filepath.Join(tmp, "terminal-bench")
	if err := os.MkdirAll(upstream, 0o755); err != nil {
		t.Fatal(err)
	}
	runAgentBenchGit(t, upstream, "init")
	runAgentBenchGit(t, upstream, "config", "user.name", "Agent Bench Test")
	runAgentBenchGit(t, upstream, "config", "user.email", "agentbench@example.invalid")
	if err := os.WriteFile(filepath.Join(upstream, "task.yaml"), []byte("id: original\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runAgentBenchGit(t, upstream, "add", ".")
	runAgentBenchGit(t, upstream, "commit", "-m", "init")

	report, err := agentbench.RunSuite(context.Background(), agentbench.RunnerOptions{
		Suite:        agentbench.SuiteTerminalBench,
		RootDir:      tmp,
		OutputDir:    filepath.Join(tmp, "reports"),
		WorkDir:      filepath.Join(tmp, "work"),
		ArtifactsDir: filepath.Join(tmp, "artifacts"),
		BenchmarkDir: upstream,
		HarnessCmd:   `test "$LUMINA_BENCHMARK_CASE" = "case-1" && test "$LUMINA_BENCHMARK_LIMIT" = "2" && test -n "$LUMINA_AGENT_RUNNER" && printf '{"total":2,"resolved":1,"pass_rate":0.5}\n'`,
		CaseID:       "case-1",
		Limit:        2,
		Config:       config.Config{APIModel: "fake-model", APIType: "anthropic", CWD: tmp},
		Now:          func() time.Time { return time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.DebugRun {
		t.Fatalf("limit/case should mark official report as debug: %#v", report)
	}
	if report.HarnessExitCode == nil || *report.HarnessExitCode != 0 {
		t.Fatalf("harness failed: %#v", report)
	}
	if report.Summary.TotalCases != 2 || report.Summary.ResolvedCases != 1 || report.Summary.PassRate != 0.5 {
		t.Fatalf("summary should come from official harness output: %#v", report.Summary)
	}
	if report.UpstreamDirtyAfter {
		t.Fatalf("official adapter should not mutate upstream, after=%q", report.UpstreamStatusAfter)
	}
	data, err := os.ReadFile(report.HarnessOutputPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"resolved":1`) {
		t.Fatalf("harness output not saved: %s", data)
	}
}

func TestAgentBenchPreparedTerminalBenchRequiresNoRebuildAndNoCleanup(t *testing.T) {
	_, err := agentbench.RunSuite(context.Background(), agentbench.RunnerOptions{
		Suite:       agentbench.SuiteTerminalBench,
		RootDir:     t.TempDir(),
		HarnessCmd:  `printf '{"total":0,"resolved":0}'`,
		PreparedEnv: true,
		Config:      config.Config{APIModel: "fake-model"},
	})
	if err == nil {
		t.Fatal("expected prepared terminal_bench run to reject harness command without --no-rebuild and --no-cleanup")
	}
	msg := err.Error()
	if !strings.Contains(msg, "--no-rebuild") || !strings.Contains(msg, "--no-cleanup") {
		t.Fatalf("prepared-env error should mention required flags, got: %v", err)
	}
}

func runAgentBenchGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, output)
	}
}

type fakeAgentRunner struct{}

func (fakeAgentRunner) Run(_ context.Context, cfg config.Config, _ string, _ string) agentbench.AgentRunResult {
	if err := os.WriteFile(filepath.Join(cfg.CWD, "result.txt"), []byte("terminal-bench-ok"), 0o644); err != nil {
		panic(err)
	}
	ttft := 10.0
	firstTool := 20.0
	return agentbench.AgentRunResult{
		Events: []agent.StreamEvent{
			agent.NewStreamEvent("text", "done", nil),
			agent.NewStreamEvent("tool_call", "bash", map[string]any{"id": "tool-1"}),
			agent.NewStreamEvent("done", "", nil),
		},
		FinalText:       "done",
		InputTokens:     123,
		OutputTokens:    45,
		ToolCalls:       1,
		TTFTMillis:      &ttft,
		FirstToolCallMS: &firstTool,
		Timeline: []agentbench.TimelineEvent{
			{Name: "first_text_delta", ElapsedMillis: 10},
			{Name: "first_tool_call", ElapsedMillis: 20},
			{Name: "final_answer", ElapsedMillis: 30},
		},
	}
}
