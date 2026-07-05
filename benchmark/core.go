package benchmark

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

type BenchmarkRun struct {
	Variant       VariantOverride
	PluginResults map[string]PluginRunResult
	PluginOrder   []string
}

type MetricDelta struct {
	Plugin         string
	MetricName     string
	BaselineValue  any
	CandidateValue any
	Delta          *float64
}

type BenchmarkComparison struct {
	Baseline     BenchmarkRun
	Candidate    BenchmarkRun
	MetricDeltas map[string]map[string]any
	DeltaRecords []MetricDelta
}

type BenchmarkVerdict struct {
	Passed         bool
	FailureRecords []FailureRecord
	Warnings       []FailureRecord
}

type PluginSection struct {
	Plugin           string
	Scorecard        BenchmarkScorecard
	ProcessMetrics   map[string]float64
	FailureRecords   []FailureRecord
	SLARules         []PluginSLA
	BenefitDirection []string
}

type BenchmarkReport struct {
	Passed         bool
	Markdown       string
	Overview       []string
	PluginSections []PluginSection
	FailureRecords []FailureRecord
	Warnings       []FailureRecord
	DeltaRecords   []MetricDelta
	Notes          []string
}

type BenchmarkCore struct{}

var reportLabels = map[string]string{
	"report_title":           "基准评测报告",
	"overview":               "总览",
	"core_scorecard":         "核心评分卡",
	"process_metrics":        "过程指标",
	"sla_rules":              "SLA 规则",
	"failure_explanations":   "失败说明",
	"benefit_direction":      "收益方向",
	"notes":                  "备注",
	"metric_header":          "指标",
	"value_header":           "值",
	"empty_metric":           "（无）",
	"not_applicable":         "不适用",
	"empty":                  "无",
	"no_baseline_comparison": "无基线对比",
	"no_warnings":            "无警告",
	"warning":                "警告",
}

var overviewLabels = map[string]string{
	"passed":   "通过状态",
	"plugins":  "插件数",
	"failures": "失败数",
	"warnings": "警告数",
	"variant":  "评测变体",
	"baseline": "基线变体",
}

func (BenchmarkCore) BuildRun(variant VariantOverride, pluginResults map[string]PluginRunResult) BenchmarkRun {
	return BenchmarkRun{Variant: variant, PluginResults: pluginResults}
}

func (BenchmarkCore) BuildRunWithOrder(variant VariantOverride, pluginResults map[string]PluginRunResult, pluginOrder []string) BenchmarkRun {
	return BenchmarkRun{Variant: variant, PluginResults: pluginResults, PluginOrder: normalizeRunPluginOrder(pluginResults, pluginOrder)}
}

func (BenchmarkCore) ComputeDeltas(baseline, candidate BenchmarkRun) BenchmarkComparison {
	var deltaRecords []MetricDelta
	metricDeltas := map[string]map[string]any{}
	for _, pluginName := range runPluginNames(candidate) {
		candidateResult := candidate.PluginResults[pluginName]
		baselineResult, ok := baseline.PluginResults[pluginName]
		if !ok {
			continue
		}
		candidateMetrics := candidateResult.Scorecard.MetricMap()
		baselineMetrics := baselineResult.Scorecard.MetricMap()
		pluginDeltas := map[string]any{}
		for _, metricName := range sortedMetricNames(candidateMetrics) {
			candidateValue := candidateMetrics[metricName]
			baselineValue := baselineMetrics[metricName]
			deltaValue := deltaValue(baselineValue, candidateValue)
			if deltaValue != nil {
				pluginDeltas[metricName+"_delta"] = *deltaValue
			}
			deltaRecords = append(deltaRecords, MetricDelta{
				Plugin:         pluginName,
				MetricName:     metricName,
				BaselineValue:  baselineValue,
				CandidateValue: candidateValue,
				Delta:          deltaValue,
			})
		}
		metricDeltas[pluginName] = pluginDeltas
	}
	return BenchmarkComparison{
		Baseline:     baseline,
		Candidate:    candidate,
		MetricDeltas: metricDeltas,
		DeltaRecords: deltaRecords,
	}
}

func (BenchmarkCore) RunSuite(ctx context.Context, plugin BenchmarkPlugin, variant *VariantOverride) (PluginRunResult, error) {
	return plugin.RunSuite(ctx, variant)
}

func (BenchmarkCore) Evaluate(run BenchmarkRun, pluginRules map[string][]PluginSLA) (BenchmarkVerdict, error) {
	if pluginRules == nil {
		pluginRules = map[string][]PluginSLA{}
	}
	var failures []FailureRecord
	var warnings []FailureRecord
	passed := true
	for _, pluginName := range runPluginNames(run) {
		result := run.PluginResults[pluginName]
		metrics := result.Scorecard.MetricMap()
		for _, sla := range pluginRules[pluginName] {
			value, ok := metrics[sla.MetricName]
			if !ok {
				continue
			}
			ok, err := satisfies(value, sla.Operator, sla.Threshold)
			if err != nil {
				return BenchmarkVerdict{}, err
			}
			if ok {
				continue
			}
			record := FailureRecord{
				CaseID:           "aggregate",
				Plugin:           pluginName,
				Category:         "sla",
				Severity:         sla.Severity,
				MetricName:       sla.MetricName,
				ExpectedBehavior: sla.Reason,
				ActualBehavior:   fmt.Sprintf("observed=%v", value),
				ObservedValue:    value,
				ExpectedValue:    sla.Threshold,
			}
			if sla.Severity == "fail" {
				failures = append(failures, record)
				passed = false
			} else {
				warnings = append(warnings, record)
			}
		}
	}
	return BenchmarkVerdict{Passed: passed, FailureRecords: failures, Warnings: warnings}, nil
}

func (BenchmarkCore) BuildBenchmarkReport(comparison *BenchmarkComparison, verdict BenchmarkVerdict, run BenchmarkRun, pluginRules map[string][]PluginSLA) BenchmarkReport {
	if pluginRules == nil {
		pluginRules = map[string][]PluginSLA{}
	}
	overview := buildOverviewLines(verdict, run, comparison)
	var sections []PluginSection
	for _, pluginName := range runPluginNames(run) {
		result := run.PluginResults[pluginName]
		deltaMetrics := map[string]any{}
		if comparison != nil {
			if found := comparison.MetricDeltas[pluginName]; found != nil {
				deltaMetrics = found
			}
		}
		failures := append([]FailureRecord{}, result.FailureRecords...)
		for _, record := range verdict.FailureRecords {
			if record.Plugin == pluginName {
				failures = append(failures, record)
			}
		}
		sections = append(sections, PluginSection{
			Plugin:           pluginName,
			Scorecard:        result.Scorecard,
			ProcessMetrics:   result.ProcessMetrics,
			FailureRecords:   failures,
			SLARules:         pluginRules[pluginName],
			BenefitDirection: mapToBenefitLines(deltaMetrics),
		})
	}
	notes := buildNoteLines(verdict, comparison)
	markdown := renderMarkdown(overview, sections, verdict, notes)
	var deltaRecords []MetricDelta
	if comparison != nil {
		deltaRecords = comparison.DeltaRecords
	}
	return BenchmarkReport{
		Passed:         verdict.Passed,
		Markdown:       markdown,
		Overview:       overview,
		PluginSections: sections,
		FailureRecords: verdict.FailureRecords,
		Warnings:       verdict.Warnings,
		DeltaRecords:   deltaRecords,
		Notes:          notes,
	}
}

func satisfies(value any, operator string, threshold float64) (bool, error) {
	numeric, ok := numericValue(value)
	if !ok {
		return true, nil
	}
	switch operator {
	case ">=":
		return numeric >= threshold, nil
	case "<=":
		return numeric <= threshold, nil
	case "==":
		return numeric == threshold, nil
	default:
		return false, fmt.Errorf("unsupported operator: %s", operator)
	}
}

func deltaValue(baselineValue, candidateValue any) *float64 {
	baseline, ok := numericValue(baselineValue)
	if !ok {
		return nil
	}
	candidate, ok := numericValue(candidateValue)
	if !ok {
		return nil
	}
	delta := candidate - baseline
	return &delta
}

func buildOverviewLines(verdict BenchmarkVerdict, run BenchmarkRun, comparison *BenchmarkComparison) []string {
	lines := []string{
		fmt.Sprintf("%s: %t", overviewLabels["passed"], verdict.Passed),
		fmt.Sprintf("%s: %d", overviewLabels["plugins"], len(run.PluginResults)),
		fmt.Sprintf("%s: %d", overviewLabels["failures"], len(verdict.FailureRecords)),
		fmt.Sprintf("%s: %d", overviewLabels["warnings"], len(verdict.Warnings)),
		fmt.Sprintf("%s: %s", overviewLabels["variant"], run.Variant.Name),
	}
	if comparison != nil {
		lines = append(lines, fmt.Sprintf("%s: %s", overviewLabels["baseline"], comparison.Baseline.Variant.Name))
	}
	return lines
}

func buildNoteLines(verdict BenchmarkVerdict, comparison *BenchmarkComparison) []string {
	baseline := "baseline_comparison=unavailable"
	if comparison != nil {
		baseline = fmt.Sprintf("baseline_comparison=deltas:%d", len(comparison.DeltaRecords))
	}
	return []string{fmt.Sprintf("warnings=%d", len(verdict.Warnings)), baseline}
}

func normalizeActualBehavior(text string) string {
	if strings.HasPrefix(text, "observed=") {
		return "观测值=" + strings.TrimPrefix(text, "observed=")
	}
	return text
}

func renderMarkdown(overview []string, pluginSections []PluginSection, verdict BenchmarkVerdict, notes []string) string {
	lines := []string{
		"# " + reportLabels["report_title"],
		"",
		"## " + reportLabels["overview"],
	}
	for _, line := range overview {
		lines = append(lines, "- "+line)
	}
	lines = append(lines, "")
	for _, section := range pluginSections {
		lines = append(lines,
			"## "+section.Plugin,
			"",
			"### "+reportLabels["core_scorecard"],
			fmt.Sprintf("| %s | %s |", reportLabels["metric_header"], reportLabels["value_header"]),
			"| --- | --- |",
		)
		for _, metric := range sortedMetricNames(section.Scorecard.MetricMap()) {
			value := section.Scorecard.MetricMap()[metric]
			lines = append(lines, fmt.Sprintf("| `%s` | `%s` |", metric, formatMetricValue(value)))
		}
		lines = append(lines,
			"",
			"### "+reportLabels["process_metrics"],
			fmt.Sprintf("| %s | %s |", reportLabels["metric_header"], reportLabels["value_header"]),
			"| --- | --- |",
		)
		if len(section.ProcessMetrics) == 0 {
			lines = append(lines, fmt.Sprintf("| `%s` | `%s` |", reportLabels["empty_metric"], reportLabels["not_applicable"]))
		} else {
			for _, metric := range sortedFloatMetricNames(section.ProcessMetrics) {
				lines = append(lines, fmt.Sprintf("| `%s` | `%s` |", metric, formatMetricValue(section.ProcessMetrics[metric])))
			}
		}
		lines = append(lines, "", "### "+reportLabels["sla_rules"])
		if len(section.SLARules) == 0 {
			lines = append(lines, "- "+reportLabels["empty"])
		} else {
			for _, rule := range section.SLARules {
				lines = append(lines, fmt.Sprintf("- `%s`: `%s %s %s` - %s", rule.Severity, rule.MetricName, rule.Operator, formatMetricValue(rule.Threshold), rule.Reason))
			}
		}
		lines = append(lines, "", "### "+reportLabels["failure_explanations"])
		if len(section.FailureRecords) == 0 {
			lines = append(lines, "- "+reportLabels["empty"])
		} else {
			for _, record := range section.FailureRecords {
				lines = append(lines, fmt.Sprintf("- `%s`: 期望：%s；实际：%s", record.MetricName, record.ExpectedBehavior, normalizeActualBehavior(record.ActualBehavior)))
			}
		}
		lines = append(lines, "", "### "+reportLabels["benefit_direction"])
		if len(section.BenefitDirection) == 0 {
			lines = append(lines, "- "+reportLabels["no_baseline_comparison"])
		} else {
			for _, line := range section.BenefitDirection {
				lines = append(lines, "- `"+line+"`")
			}
		}
		lines = append(lines, "")
	}
	lines = append(lines, "## "+reportLabels["notes"])
	for _, note := range notes {
		lines = append(lines, "- "+note)
	}
	if len(verdict.Warnings) == 0 {
		lines = append(lines, "- "+reportLabels["no_warnings"])
	} else {
		for _, warning := range verdict.Warnings {
			lines = append(lines, fmt.Sprintf("- %s `%s.%s`：%s", reportLabels["warning"], warning.Plugin, warning.MetricName, normalizeActualBehavior(warning.ActualBehavior)))
		}
	}
	return strings.Join(lines, "\n")
}

func formatMetricValue(value any) string {
	switch v := value.(type) {
	case float64:
		return fmt.Sprintf("%.3f", v)
	case float32:
		return fmt.Sprintf("%.3f", float64(v))
	default:
		return fmt.Sprint(value)
	}
}

func numericValue(value any) (float64, bool) {
	switch v := value.(type) {
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case float64:
		return v, true
	case float32:
		return float64(v), true
	default:
		return 0, false
	}
}

func sortedRunPluginNames(results map[string]PluginRunResult) []string {
	names := make([]string, 0, len(results))
	for name := range results {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func runPluginNames(run BenchmarkRun) []string {
	if len(run.PluginOrder) == 0 {
		return sortedRunPluginNames(run.PluginResults)
	}
	return normalizeRunPluginOrder(run.PluginResults, run.PluginOrder)
}

func normalizeRunPluginOrder(results map[string]PluginRunResult, order []string) []string {
	seen := map[string]struct{}{}
	names := make([]string, 0, len(results))
	for _, name := range order {
		if _, ok := results[name]; !ok {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	for _, name := range sortedRunPluginNames(results) {
		if _, ok := seen[name]; ok {
			continue
		}
		names = append(names, name)
	}
	return names
}

func sortedMetricNames(metrics map[string]any) []string {
	names := make([]string, 0, len(metrics))
	for name := range metrics {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func sortedFloatMetricNames(metrics map[string]float64) []string {
	names := make([]string, 0, len(metrics))
	for name := range metrics {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func mapToBenefitLines(metrics map[string]any) []string {
	names := sortedMetricNames(metrics)
	lines := make([]string, 0, len(names))
	for _, metric := range names {
		lines = append(lines, fmt.Sprintf("%s=%s", metric, formatMetricValue(metrics[metric])))
	}
	return lines
}
