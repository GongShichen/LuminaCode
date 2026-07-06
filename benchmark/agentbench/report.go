package agentbench

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

func BuildReportPaths(outputDir string, now time.Time) (string, string) {
	return BuildSuiteReportPaths(outputDir, "", now)
}

func BuildSuiteReportPaths(outputDir, suite string, now time.Time) (string, string) {
	base := filepath.Join(outputDir, suiteReportBaseName(suite, now))
	return base + ".json", base + ".md"
}

func WriteReport(report Report, outputDir string, now time.Time) (string, string, error) {
	if err := os.MkdirAll(outputDir, 0o755); err != nil {
		return "", "", err
	}
	jsonPath, mdPath := BuildSuiteReportPaths(outputDir, report.Suite, now)
	jsonData, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(jsonPath, append(jsonData, '\n'), 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(mdPath, []byte(RenderMarkdown(report)), 0o644); err != nil {
		return "", "", err
	}
	return jsonPath, mdPath, nil
}

func PruneOldReports(outputDir string, keep int) error {
	if keep <= 0 {
		return nil
	}
	entries, err := os.ReadDir(outputDir)
	if err != nil {
		return err
	}
	var reports []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if isBenchmarkReportFile(name) {
			reports = append(reports, name)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(reports)))
	keepFiles := keep * 2
	if len(reports) <= keepFiles {
		return nil
	}
	for _, name := range reports[keepFiles:] {
		if err := os.Remove(filepath.Join(outputDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func RenderMarkdown(report Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# LuminaCode Agent Benchmark\n\n")
	fmt.Fprintf(&b, "- Suite: `%s`\n", report.Suite)
	fmt.Fprintf(&b, "- Model: `%s`\n", emptyDash(report.Model))
	fmt.Fprintf(&b, "- Generated: `%s`\n", report.GeneratedAt)
	fmt.Fprintf(&b, "- Root: `%s`\n\n", report.RootDir)
	if report.DebugRun {
		fmt.Fprintf(&b, "> Debug run: `--limit` or `--case` was used. Do not treat this as an official full benchmark score.\n\n")
	}
	if report.BenchmarkDir != "" {
		fmt.Fprintf(&b, "- Benchmark dir: `%s`\n", report.BenchmarkDir)
	}
	if report.HarnessCommand != "" {
		fmt.Fprintf(&b, "- Harness command: `%s`\n", report.HarnessCommand)
	}
	if report.HarnessOutputPath != "" {
		fmt.Fprintf(&b, "- Harness output: `%s`\n", report.HarnessOutputPath)
	}
	if report.HarnessExitCode != nil {
		fmt.Fprintf(&b, "- Harness exit code: `%d`\n", *report.HarnessExitCode)
	}
	if report.BenchmarkDir != "" || report.HarnessCommand != "" || report.HarnessOutputPath != "" {
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "## Summary\n\n")
	fmt.Fprintf(&b, "| Metric | Value |\n| --- | ---: |\n")
	fmt.Fprintf(&b, "| Cases | %d |\n", report.Summary.TotalCases)
	fmt.Fprintf(&b, "| Resolved | %d |\n", report.Summary.ResolvedCases)
	fmt.Fprintf(&b, "| Pass rate | %.2f%% |\n", report.Summary.PassRate*100)
	fmt.Fprintf(&b, "| Avg duration | %.2fs |\n", report.Summary.AverageDurationSeconds)
	fmt.Fprintf(&b, "| Duration p50 / p90 / p95 | %s / %s / %s |\n", fmtFloat(report.Summary.DurationSeconds.P50), fmtFloat(report.Summary.DurationSeconds.P90), fmtFloat(report.Summary.DurationSeconds.P95))
	fmt.Fprintf(&b, "| TTFT p50 / p90 / p95 | %s / %s / %s ms |\n", fmtFloat(report.Summary.TTFTMillis.P50), fmtFloat(report.Summary.TTFTMillis.P90), fmtFloat(report.Summary.TTFTMillis.P95))
	fmt.Fprintf(&b, "| First tool p50 / p90 / p95 | %s / %s / %s ms |\n", fmtFloat(report.Summary.FirstToolCallMillis.P50), fmtFloat(report.Summary.FirstToolCallMillis.P90), fmtFloat(report.Summary.FirstToolCallMillis.P95))
	fmt.Fprintf(&b, "| First test p50 / p90 / p95 | %s / %s / %s ms |\n", fmtFloat(report.Summary.FirstTestMillis.P50), fmtFloat(report.Summary.FirstTestMillis.P90), fmtFloat(report.Summary.FirstTestMillis.P95))
	fmt.Fprintf(&b, "| Avg input/output tokens | %.0f / %.0f |\n", report.Summary.AverageInputTokens, report.Summary.AverageOutputTokens)
	fmt.Fprintf(&b, "| Token p90/p95 input | %s / %s |\n", fmtFloat(report.Summary.InputTokens.P90), fmtFloat(report.Summary.InputTokens.P95))
	fmt.Fprintf(&b, "| Token p90/p95 output | %s / %s |\n", fmtFloat(report.Summary.OutputTokens.P90), fmtFloat(report.Summary.OutputTokens.P95))
	fmt.Fprintf(&b, "| Tool calls | %d |\n", report.Summary.TotalToolCalls)
	fmt.Fprintf(&b, "\n## Cases\n\n")
	fmt.Fprintf(&b, "| Case | Resolved | Duration | TTFT | First tool | First test | Tokens | Error |\n")
	fmt.Fprintf(&b, "| --- | ---: | ---: | ---: | ---: | ---: | ---: | --- |\n")
	for _, result := range report.Results {
		fmt.Fprintf(&b, "| `%s` | %t | %.2fs | %s | %s | %s | %d/%d | %s |\n",
			result.Case.ID,
			result.Resolved,
			result.DurationSeconds,
			fmtFloat(result.TTFTMillis),
			fmtFloat(result.FirstToolCallMS),
			fmtFloat(result.FirstTestMS),
			result.InputTokens,
			result.OutputTokens,
			emptyDash(result.ErrorType),
		)
	}
	if len(report.Summary.FailureCategories) > 0 {
		fmt.Fprintf(&b, "\n## Failure Categories\n\n")
		keys := make([]string, 0, len(report.Summary.FailureCategories))
		for key := range report.Summary.FailureCategories {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&b, "- `%s`: %d\n", key, report.Summary.FailureCategories[key])
		}
	}
	if len(report.LuminaDiagnostics) > 0 {
		fmt.Fprintf(&b, "\n## Lumina Diagnostics\n\n")
		fmt.Fprintf(&b, "| Task | Agent exit | Repair | Missing artifacts | Failure category | Diagnostics |\n")
		fmt.Fprintf(&b, "| --- | ---: | --- | --- | --- | --- |\n")
		for _, diagnostic := range report.LuminaDiagnostics {
			fmt.Fprintf(&b, "| `%s` | %s | %s | %s | `%s` | `%s` |\n",
				emptyDash(diagnostic.TaskID),
				fmtIntPointer(diagnostic.FinalAgentExitStatus, diagnostic.AgentExitStatus),
				fmtRepairDiagnostic(diagnostic.PostFlightRepair),
				emptyDash(strings.Join(diagnostic.ExplicitMissingArtifacts, "<br>")),
				emptyDash(diagnostic.FailureCategory),
				diagnostic.Path,
			)
		}
	}
	if report.PredictionsPath != "" {
		fmt.Fprintf(&b, "\n## SWE-bench\n\n")
		fmt.Fprintf(&b, "- Predictions: `%s`\n", report.PredictionsPath)
		if report.HarnessOutputPath != "" {
			fmt.Fprintf(&b, "- Harness output: `%s`\n", report.HarnessOutputPath)
		}
	}
	if len(report.OfficialMetrics) > 0 {
		fmt.Fprintf(&b, "\n## Official Metrics\n\n")
		keys := make([]string, 0, len(report.OfficialMetrics))
		for key := range report.OfficialMetrics {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&b, "- `%s`: `%v`\n", key, report.OfficialMetrics[key])
		}
	}
	if report.UpstreamStatusBefore != "" || report.UpstreamStatusAfter != "" {
		fmt.Fprintf(&b, "\n## Upstream Status\n\n")
		fmt.Fprintf(&b, "- Dirty after run: `%t`\n", report.UpstreamDirtyAfter)
		if report.UpstreamStatusBefore != "" {
			fmt.Fprintf(&b, "- Before:\n\n```text\n%s\n```\n", report.UpstreamStatusBefore)
		}
		if report.UpstreamStatusAfter != "" {
			fmt.Fprintf(&b, "- After:\n\n```text\n%s\n```\n", report.UpstreamStatusAfter)
		}
	}
	return b.String()
}

func suiteReportBaseName(suite string, now time.Time) string {
	stamp := now.Format("20060102-150405")
	switch suite {
	case SuiteTerminalBench:
		return "terminal-bench-" + stamp
	case SuiteTauBench:
		return "tau-bench-" + stamp
	case SuiteSWEBenchVerified:
		return "swebench-verified-" + stamp
	case SuiteSWEBenchVerifiedSubset:
		return "swebench-verified-subset-" + stamp
	case SuiteTerminalBenchSmoke:
		return "terminal-bench-smoke-" + stamp
	case SuiteAiderPolyglotSmoke:
		return "aider-polyglot-smoke-" + stamp
	default:
		return ReportPrefix + stamp
	}
}

func isBenchmarkReportFile(name string) bool {
	if !(strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".md")) {
		return false
	}
	for _, prefix := range []string{
		ReportPrefix,
		"terminal-bench-",
		"tau-bench-",
		"swebench-verified-",
		"swebench-verified-subset-",
		"terminal-bench-smoke-",
		"aider-polyglot-smoke-",
	} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func fmtFloat(value *float64) string {
	if value == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *value)
}

func emptyDash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func fmtIntPointer(primary *int, fallback *int) string {
	if primary != nil {
		return fmt.Sprintf("%d", *primary)
	}
	if fallback != nil {
		return fmt.Sprintf("%d", *fallback)
	}
	return "-"
}

func fmtRepairDiagnostic(repair HarnessRepairDiagnostic) string {
	if !repair.Triggered {
		return "-"
	}
	if repair.ExitStatus == nil {
		return "triggered"
	}
	return fmt.Sprintf("triggered (%d)", *repair.ExitStatus)
}
