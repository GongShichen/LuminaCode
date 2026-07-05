package test

import (
	"context"
	"reflect"
	"testing"

	"LuminaCode/benchmark"
)

func TestBenchmarkDefaultPluginsMatchPythonTypedPluginContract(t *testing.T) {
	plugins := benchmark.DefaultBenchmarkPlugins()
	if len(plugins) != 3 {
		t.Fatalf("default plugins=%d want 3", len(plugins))
	}
	wantNames := []string{"memory", "context", "security"}
	for i, plugin := range plugins {
		if plugin.Name() != wantNames[i] {
			t.Fatalf("plugin[%d].Name()=%q want %q", i, plugin.Name(), wantNames[i])
		}
		result, err := plugin.RunSuite(context.Background(), nil)
		if err != nil {
			t.Fatalf("%s run suite: %v", plugin.Name(), err)
		}
		if result.Plugin != wantNames[i] {
			t.Fatalf("result.Plugin=%q want %q", result.Plugin, wantNames[i])
		}
		metrics := result.Scorecard.MetricMap()
		switch plugin.Name() {
		case "memory":
			if _, ok := metrics["memory_lift_rate"]; !ok {
				t.Fatalf("memory scorecard missing memory_lift_rate: %#v", metrics)
			}
		case "context":
			if _, ok := metrics["budget_pass_rate"]; !ok {
				t.Fatalf("context scorecard missing budget_pass_rate: %#v", metrics)
			}
		case "security":
			if _, ok := metrics["static_bypass_rate"]; !ok {
				t.Fatalf("security scorecard missing static_bypass_rate: %#v", metrics)
			}
		}
	}
}

func TestBenchmarkPluginRepeatRunsAreStableLikePython(t *testing.T) {
	for _, plugin := range benchmark.DefaultBenchmarkPlugins() {
		first, err := plugin.RunSuite(context.Background(), nil)
		if err != nil {
			t.Fatalf("%s first run: %v", plugin.Name(), err)
		}
		second, err := plugin.RunSuite(context.Background(), nil)
		if err != nil {
			t.Fatalf("%s second run: %v", plugin.Name(), err)
		}
		if !reflect.DeepEqual(first.Scorecard.MetricMap(), second.Scorecard.MetricMap()) {
			t.Fatalf("%s scorecard should be repeatable: first=%#v second=%#v", plugin.Name(), first.Scorecard.MetricMap(), second.Scorecard.MetricMap())
		}
	}
}

func TestBenchmarkBuiltInPluginsSmokeTierMatchesPythonCountsAndMetrics(t *testing.T) {
	variant := &benchmark.VariantOverride{Name: "candidate", Description: "feature on", ConfigOverrides: map[string]any{"tiers": []string{"smoke"}}}
	results := runBenchmarkPlugins(t, variant)

	memory := results["memory"]
	assertMetric(t, memory.Scorecard.MetricMap(), "recall_mean_f1_at_k", 1.0)
	assertMetric(t, memory.Scorecard.MetricMap(), "memory_lift_rate", 0.0)
	assertMetric(t, memory.Scorecard.MetricMap(), "index_mean_coverage_rate", 0.0)
	assertMetric(t, memory.Scorecard.MetricMap(), "effectiveness_mean_answer_fact_coverage", 1.0)
	assertProcessMetric(t, memory.ProcessMetrics, "selected_recall_cases", 1.0)
	assertProcessMetric(t, memory.ProcessMetrics, "selected_index_cases", 0.0)
	assertProcessMetric(t, memory.ProcessMetrics, "selected_extraction_cases", 1.0)
	assertProcessMetric(t, memory.ProcessMetrics, "selected_effectiveness_cases", 1.0)

	contextResult := results["context"]
	assertMetric(t, contextResult.Scorecard.MetricMap(), "budget_pass_rate", 1.0)
	assertMetric(t, contextResult.Scorecard.MetricMap(), "l4_amnesia_rate", 0.0)
	assertProcessMetric(t, contextResult.ProcessMetrics, "selected_context_cases", 3.0)

	security := results["security"]
	assertMetric(t, security.Scorecard.MetricMap(), "static_bypass_rate", 0.0)
	assertMetric(t, security.Scorecard.MetricMap(), "safe_command_false_positive_rate", 0.0)
	assertMetric(t, security.Scorecard.MetricMap(), "classification_match_rate", 1.0)
	assertProcessMetric(t, security.ProcessMetrics, "selected_security_cases", 4.0)
}

func TestBenchmarkBuiltInProfilesDisableSubsystemMetricsLikePython(t *testing.T) {
	memoryOff, err := benchmark.GetProfile("memory_off")
	if err != nil {
		t.Fatal(err)
	}
	memoryResult, err := (benchmark.MemoryBenchmarkPlugin{}).RunSuite(context.Background(), &memoryOff)
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range []string{
		"recall_mean_f1_at_k",
		"recall_full_match_rate",
		"memory_lift_rate",
		"quality_mean_f1_at_k",
		"stability_repeat_consistency_rate",
		"index_mean_coverage_rate",
		"extraction_mean_write_validity_rate",
		"effectiveness_mean_answer_fact_coverage",
	} {
		assertMetric(t, memoryResult.Scorecard.MetricMap(), metric, 0.0)
	}

	contextOff, err := benchmark.GetProfile("context_off")
	if err != nil {
		t.Fatal(err)
	}
	contextResult, err := (benchmark.ContextBenchmarkPlugin{}).RunSuite(context.Background(), &contextOff)
	if err != nil {
		t.Fatal(err)
	}
	assertMetric(t, contextResult.Scorecard.MetricMap(), "budget_pass_rate", 0.0)
	assertMetric(t, contextResult.Scorecard.MetricMap(), "l4_amnesia_rate", 1.0)

	securityRelaxed, err := benchmark.GetProfile("security_relaxed")
	if err != nil {
		t.Fatal(err)
	}
	securityResult, err := (benchmark.SecurityBenchmarkPlugin{}).RunSuite(context.Background(), &securityRelaxed)
	if err != nil {
		t.Fatal(err)
	}
	for _, metric := range []string{
		"static_bypass_rate",
		"safe_command_false_positive_rate",
		"sandbox_read_containment_failure_rate",
		"sandbox_write_containment_failure_rate",
		"network_containment_failure_rate",
		"secret_leakage_rate",
	} {
		assertMetric(t, securityResult.Scorecard.MetricMap(), metric, 1.0)
	}
	assertMetric(t, securityResult.Scorecard.MetricMap(), "classification_match_rate", 0.0)
}

func TestBenchmarkReportWithDefaultPluginsFiltersTierAndBaselineLikePython(t *testing.T) {
	profile := "memory_off"
	report, err := benchmark.BuildBenchmarkReport(context.Background(), benchmark.DefaultBenchmarkPlugins(), benchmark.BuildReportOptions{
		BaselinePlugins: benchmark.DefaultBenchmarkPlugins(),
		BaselineProfile: &profile,
		Tiers:           []string{"smoke"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed {
		t.Fatalf("smoke report should pass with warnings only:\n%s", report.Markdown)
	}
	sections := map[string]benchmark.PluginSection{}
	for _, section := range report.PluginSections {
		sections[section.Plugin] = section
	}
	assertProcessMetric(t, sections["memory"].ProcessMetrics, "selected_recall_cases", 1.0)
	assertProcessMetric(t, sections["context"].ProcessMetrics, "selected_context_cases", 3.0)
	assertProcessMetric(t, sections["security"].ProcessMetrics, "selected_security_cases", 4.0)
	if len(report.DeltaRecords) == 0 {
		t.Fatalf("expected baseline delta records")
	}
	if !containsBenchmarkDelta(report.DeltaRecords, "memory", "recall_mean_f1_at_k", 1.0) {
		t.Fatalf("expected memory recall delta of 1.0, got %#v", report.DeltaRecords)
	}
}

func runBenchmarkPlugins(t *testing.T, variant *benchmark.VariantOverride) map[string]benchmark.PluginRunResult {
	t.Helper()
	results := map[string]benchmark.PluginRunResult{}
	for _, plugin := range benchmark.DefaultBenchmarkPlugins() {
		result, err := plugin.RunSuite(context.Background(), variant)
		if err != nil {
			t.Fatalf("%s run suite: %v", plugin.Name(), err)
		}
		results[plugin.Name()] = result
	}
	return results
}

func assertMetric(t *testing.T, metrics map[string]any, name string, want float64) {
	t.Helper()
	got, ok := metrics[name].(float64)
	if !ok {
		t.Fatalf("metric %s=%#v is not float64 in %#v", name, metrics[name], metrics)
	}
	if got != want {
		t.Fatalf("metric %s=%v want %v", name, got, want)
	}
}

func assertProcessMetric(t *testing.T, metrics map[string]float64, name string, want float64) {
	t.Helper()
	if got := metrics[name]; got != want {
		t.Fatalf("process metric %s=%v want %v in %#v", name, got, want, metrics)
	}
}

func containsBenchmarkDelta(records []benchmark.MetricDelta, plugin string, metric string, delta float64) bool {
	for _, record := range records {
		if record.Plugin == plugin && record.MetricName == metric && record.Delta != nil && *record.Delta == delta {
			return true
		}
	}
	return false
}
