package benchmark

import "context"

type BenchmarkScorecard interface {
	MetricMap() map[string]any
}

type FailureRecord struct {
	CaseID           string
	Plugin           string
	Category         string
	Severity         string
	MetricName       string
	ExpectedBehavior string
	ActualBehavior   string
	BaselineBehavior *string
	ObservedValue    any
	ExpectedValue    any
	ReproductionCmd  *string
	TraceID          *string
	ReplayLogPath    *string
}

type PluginSLA struct {
	Severity   string
	MetricName string
	Operator   string
	Threshold  float64
	Reason     string
}

type VariantOverride struct {
	Name            string
	Description     string
	ConfigOverrides map[string]any
	BranchRef       *string
	ArtifactPath    *string
}

type PluginRunResult struct {
	Plugin         string
	Scorecard      BenchmarkScorecard
	ProcessMetrics map[string]float64
	FailureRecords []FailureRecord
}

type BenchmarkPlugin interface {
	Name() string
	RunSuite(context.Context, *VariantOverride) (PluginRunResult, error)
	BuildSLARules() []PluginSLA
}
