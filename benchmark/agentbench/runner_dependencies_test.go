package agentbench

import (
	"context"
	"strings"
	"testing"

	"LuminaCode/config"
)

func TestRunSuiteRejectsMissingAgentRunnerDependency(t *testing.T) {
	_, err := RunSuite(context.Background(), RunnerOptions{Suite: SuiteAiderPolyglotSmoke, RootDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "agent runner dependency is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestZeroHeadlessRunnerReportsMissingFactory(t *testing.T) {
	result := (*HeadlessAgentRunner)(nil).Run(context.Background(), config.Config{}, "hello", "session")
	if !strings.Contains(result.ErrorType, "query engine factory is required") {
		t.Fatalf("unexpected result: %#v", result)
	}
}
