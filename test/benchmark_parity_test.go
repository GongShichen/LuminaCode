package test

import (
	"context"
	"strings"
	"testing"

	"LuminaCode/benchmark"
)

type benchmarkTestScorecard struct {
	metrics map[string]any
}

func (s benchmarkTestScorecard) MetricMap() map[string]any {
	return s.metrics
}

type benchmarkTestPlugin struct {
	name     string
	metrics  map[string]any
	rules    []benchmark.PluginSLA
	variants []*benchmark.VariantOverride
}

func (p *benchmarkTestPlugin) Name() string { return p.name }

func (p *benchmarkTestPlugin) RunSuite(_ context.Context, variant *benchmark.VariantOverride) (benchmark.PluginRunResult, error) {
	p.variants = append(p.variants, variant)
	return benchmark.PluginRunResult{
		Plugin:         p.name,
		Scorecard:      benchmarkTestScorecard{metrics: p.metrics},
		ProcessMetrics: map[string]float64{"elapsed": 1.25},
	}, nil
}

func (p *benchmarkTestPlugin) BuildSLARules() []benchmark.PluginSLA { return p.rules }

func TestBenchmarkProfilesAndRegistryMatchPython(t *testing.T) {
	profiles := benchmark.AvailableProfiles()
	want := []string{"context_off", "memory_off", "security_relaxed"}
	for i, name := range want {
		if profiles[i] != name {
			t.Fatalf("profile[%d]=%q want %q from %#v", i, profiles[i], name, profiles)
		}
	}
	if _, err := benchmark.GetProfile("missing"); err == nil || !strings.Contains(err.Error(), "unknown benchmark profile: missing. available=context_off, memory_off, security_relaxed") {
		t.Fatalf("unexpected missing profile error: %v", err)
	}

	registry := benchmark.NewBenchmarkRegistry()
	registry.Register("security", &benchmarkTestPlugin{name: "security"})
	registry.Register("memory", &benchmarkTestPlugin{name: "memory"})
	names := registry.Names()
	if len(names) != 2 || names[0] != "memory" || names[1] != "security" {
		t.Fatalf("expected sorted registry names, got %#v", names)
	}
	if registry.Get("memory") == nil {
		t.Fatal("expected registered plugin to be retrievable")
	}
	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatalf("expected missing registry key to panic like Python KeyError, got %#v", recovered)
			}
		}()
		_ = registry.Get("missing")
	}()
}

func TestBenchmarkReportRunnerSLAAndDeltas(t *testing.T) {
	candidate := &benchmarkTestPlugin{
		name:    "memory",
		metrics: map[string]any{"accuracy": 0.8, "status": "ok"},
		rules: []benchmark.PluginSLA{
			{Severity: "fail", MetricName: "accuracy", Operator: ">=", Threshold: 0.9, Reason: "accuracy floor"},
			{Severity: "warn", MetricName: "accuracy", Operator: ">=", Threshold: 0.85, Reason: "accuracy warning"},
		},
	}
	baseline := &benchmarkTestPlugin{
		name:    "memory",
		metrics: map[string]any{"accuracy": 0.7, "status": "base"},
	}
	report, err := benchmark.BuildBenchmarkReport(context.Background(), []benchmark.BenchmarkPlugin{candidate}, benchmark.BuildReportOptions{
		BaselinePlugins: []benchmark.BenchmarkPlugin{baseline},
		Tiers:           []string{"smoke", "core", "smoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Passed {
		t.Fatal("expected SLA failure to fail report")
	}
	if len(report.FailureRecords) != 1 || len(report.Warnings) != 1 {
		t.Fatalf("expected one failure and one warning, got failures=%#v warnings=%#v", report.FailureRecords, report.Warnings)
	}
	if len(report.DeltaRecords) != 2 {
		t.Fatalf("expected numeric and string delta records, got %#v", report.DeltaRecords)
	}
	if candidate.variants[0].ConfigOverrides["tiers"].([]string)[2-1] != "core" {
		t.Fatalf("expected tiers to be deduplicated preserving order, got %#v", candidate.variants[0].ConfigOverrides["tiers"])
	}
	for _, want := range []string{
		"# 基准评测报告",
		"- 通过状态: false",
		"- 基线变体: baseline",
		"### SLA 规则",
		"`accuracy >= 0.900` - accuracy floor",
		"实际：观测值=0.8",
		"`accuracy_delta=0.100`",
		"baseline_comparison=deltas:2",
		"警告 `memory.accuracy`：观测值=0.8",
	} {
		if !strings.Contains(report.Markdown, want) {
			t.Fatalf("expected markdown to contain %q:\n%s", want, report.Markdown)
		}
	}
	if benchmark.FormatBenchmarkReport(report) != report.Markdown {
		t.Fatal("format_benchmark_report should return markdown unchanged")
	}
}

func TestBenchmarkReportPreservesPluginListOrderLikePython(t *testing.T) {
	security := &benchmarkTestPlugin{name: "security", metrics: map[string]any{"risk": 0.1}}
	memory := &benchmarkTestPlugin{name: "memory", metrics: map[string]any{"accuracy": 1.0}}
	report, err := benchmark.BuildBenchmarkReport(context.Background(), []benchmark.BenchmarkPlugin{security, memory}, benchmark.BuildReportOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.PluginSections) != 2 || report.PluginSections[0].Plugin != "security" || report.PluginSections[1].Plugin != "memory" {
		t.Fatalf("plugin sections should preserve Python dict insertion order, got %#v", report.PluginSections)
	}
	securityIdx := strings.Index(report.Markdown, "## security")
	memoryIdx := strings.Index(report.Markdown, "## memory")
	if securityIdx < 0 || memoryIdx < 0 || securityIdx > memoryIdx {
		t.Fatalf("markdown sections should preserve plugin list order:\n%s", report.Markdown)
	}
}

func TestBenchmarkUnsupportedSLAOperatorMatchesPython(t *testing.T) {
	plugin := &benchmarkTestPlugin{
		name:    "security",
		metrics: map[string]any{"risk": 0.2},
		rules: []benchmark.PluginSLA{
			{Severity: "fail", MetricName: "risk", Operator: "!=", Threshold: 0.1, Reason: "invalid rule"},
		},
	}
	_, err := benchmark.BuildBenchmarkReport(context.Background(), []benchmark.BenchmarkPlugin{plugin}, benchmark.BuildReportOptions{})
	if err == nil || err.Error() != "unsupported operator: !=" {
		t.Fatalf("expected Python-style unsupported operator error, got %v", err)
	}
}

func TestBenchmarkCaseCatalogsMatchPythonAssets(t *testing.T) {
	ids := map[string]bool{}
	pluginCounts := map[benchmark.BenchmarkPluginName]int{}
	tierCounts := map[benchmark.BenchmarkTier]int{}
	expectations := map[benchmark.VariantExpectation]bool{}
	for _, spec := range benchmark.AllCaseSpecs {
		if ids[spec.CaseID] {
			t.Fatalf("duplicate case id: %s", spec.CaseID)
		}
		ids[spec.CaseID] = true
		pluginCounts[spec.Plugin]++
		tierCounts[spec.Tier]++
		expectations[spec.VariantExpectation] = true
		if len(spec.ExecutionCaseIDs) == 0 {
			t.Fatalf("case %s should define execution mappings like Python", spec.CaseID)
		}
	}
	if len(benchmark.AllCaseSpecs) != 27 {
		t.Fatalf("expected 27 benchmark case specs, got %d", len(benchmark.AllCaseSpecs))
	}
	for _, plugin := range []benchmark.BenchmarkPluginName{benchmark.BenchmarkPluginMemory, benchmark.BenchmarkPluginContext, benchmark.BenchmarkPluginSecurity} {
		if pluginCounts[plugin] != 9 {
			t.Fatalf("plugin %s count=%d want 9 counts=%#v", plugin, pluginCounts[plugin], pluginCounts)
		}
	}
	if tierCounts[benchmark.BenchmarkTierSmoke] != 9 || tierCounts[benchmark.BenchmarkTierCore] != 13 || tierCounts[benchmark.BenchmarkTierStress] != 5 {
		t.Fatalf("unexpected tier counts: %#v", tierCounts)
	}
	if !expectations[benchmark.VariantExpectationCandidateOnly] || !expectations[benchmark.VariantExpectationBaselineVsCandidate] || len(expectations) != 2 {
		t.Fatalf("unexpected variant expectation set: %#v", expectations)
	}
	if len(benchmark.MemoryCaseSpecs) != 9 || len(benchmark.ContextCaseSpecs) != 9 || len(benchmark.SecurityCaseSpecs) != 9 {
		t.Fatalf("plugin-specific catalogs should each have 9 specs")
	}
}

func TestBenchmarkCaseCatalogSelectionHelpersMatchPython(t *testing.T) {
	memorySmoke := benchmark.CaseSpecsFor(benchmark.BenchmarkPluginMemory, []string{"smoke"})
	if len(memorySmoke) != 3 {
		t.Fatalf("memory smoke cases=%d want 3", len(memorySmoke))
	}
	for _, spec := range memorySmoke {
		if spec.Plugin != benchmark.BenchmarkPluginMemory || spec.Tier != benchmark.BenchmarkTierSmoke {
			t.Fatalf("case_specs_for returned wrong spec: %#v", spec)
		}
	}
	securityIDs := benchmark.ExecutionCaseIDsFor(benchmark.BenchmarkPluginSecurity, []string{"smoke"}, "security")
	for _, want := range []string{"dangerous-command-substitution", "safe-git-status", "safe-grep", "containment-read"} {
		if _, ok := securityIDs[want]; !ok {
			t.Fatalf("execution_case_ids_for missing %q from %#v", want, securityIDs)
		}
	}
	if _, ok := securityIDs["containment-write"]; ok {
		t.Fatalf("smoke-only security IDs should not include core-only containment-write: %#v", securityIDs)
	}
}
