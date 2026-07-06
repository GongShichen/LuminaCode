package agentbench

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func collectOfficialHarnessDiagnostics(options RunnerOptions, harnessOutput string) []HarnessDiagnostic {
	if options.Suite != SuiteTerminalBench {
		return nil
	}
	root := terminalBenchRunOutputRoot(options)
	diagnostics := collectLuminaDiagnostics(root, harnessOutput)
	sort.Slice(diagnostics, func(i, j int) bool {
		if diagnostics[i].TaskID == diagnostics[j].TaskID {
			return diagnostics[i].Path < diagnostics[j].Path
		}
		return diagnostics[i].TaskID < diagnostics[j].TaskID
	})
	return diagnostics
}

func mergeDiagnosticFailures(summary *SuiteSummary, diagnostics []HarnessDiagnostic) {
	if summary == nil || len(diagnostics) == 0 {
		return
	}
	if summary.FailureCategories == nil {
		summary.FailureCategories = map[string]int{}
	}
	for _, diagnostic := range diagnostics {
		category := strings.TrimSpace(diagnostic.FailureCategory)
		if category == "" {
			continue
		}
		summary.FailureCategories[category]++
	}
}

func terminalBenchRunOutputRoot(options RunnerOptions) string {
	if runID := terminalBenchRunID(options.HarnessCmd); runID != "" {
		candidate := filepath.Join(options.OutputDir, "tb-runs", runID)
		if fileExists(candidate) {
			return candidate
		}
	}
	return options.OutputDir
}

func terminalBenchRunID(command string) string {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`--run-id=([^\s]+)`),
		regexp.MustCompile(`--run-id\s+([^\s]+)`),
	}
	for _, pattern := range patterns {
		if match := pattern.FindStringSubmatch(command); len(match) == 2 {
			return strings.Trim(match[1], `"'`)
		}
	}
	return ""
}

func collectLuminaDiagnostics(root string, harnessOutput string) []HarnessDiagnostic {
	if root == "" {
		return nil
	}
	diagnostics := []HarnessDiagnostic{}
	_ = filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry == nil || entry.IsDir() || entry.Name() != "lumina-diagnostics.json" {
			return nil
		}
		if diagnostic, ok := readLuminaDiagnostic(root, path, harnessOutput); ok {
			diagnostics = append(diagnostics, diagnostic)
		}
		return nil
	})
	return diagnostics
}

func readLuminaDiagnostic(root, path, harnessOutput string) (HarnessDiagnostic, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return HarnessDiagnostic{}, false
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return HarnessDiagnostic{}, false
	}
	diagnostic := HarnessDiagnostic{
		TaskID:               taskIDFromDiagnosticPath(root, path),
		Path:                 path,
		InstructionPath:      stringField(raw, "instruction_path"),
		AgentExitStatus:      intPointerField(raw, "agent_exit_status"),
		FinalAgentExitStatus: intPointerField(raw, "final_agent_exit_status"),
		ProcessSnapshotPath:  stringField(raw, "process_snapshot_path"),
		HighCPUProcesses:     stringSliceField(raw, "high_cpu_processes"),
		FailureCategory:      stringField(raw, "failure_category"),
		Raw:                  raw,
	}
	diagnostic.ExplicitArtifactChecks = artifactChecksField(raw, "explicit_artifact_checks")
	diagnostic.ExplicitMissingArtifacts = stringSliceField(raw, "explicit_missing_artifacts")
	diagnostic.PostFlightRepair = repairDiagnosticField(raw, "post_flight_repair")
	if diagnostic.FailureCategory == "" {
		diagnostic.FailureCategory = classifyHarnessDiagnostic(diagnostic, harnessOutput)
	}
	return diagnostic, true
}

func taskIDFromDiagnosticPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for _, part := range parts {
		if part == "" || part == "." || part == "logs" || part == "sessions" {
			continue
		}
		if part == "lumina-diagnostics.json" {
			break
		}
		return part
	}
	return ""
}

func classifyHarnessDiagnostic(diagnostic HarnessDiagnostic, harnessOutput string) string {
	if len(diagnostic.ExplicitMissingArtifacts) > 0 {
		return "missing_artifact"
	}
	lower := strings.ToLower(harnessOutput)
	artifactPresent := false
	for _, check := range diagnostic.ExplicitArtifactChecks {
		if check.Exists {
			artifactPresent = true
			break
		}
	}
	if strings.Contains(lower, "timeout") && artifactPresent {
		return "test_timeout_after_artifact_present"
	}
	if strings.Contains(lower, "error collecting") || strings.Contains(lower, "traceback") || strings.Contains(lower, "harness error") {
		return "harness_test_error"
	}
	if diagnostic.FinalAgentExitStatus != nil && *diagnostic.FinalAgentExitStatus != 0 {
		return "agent_timeout"
	}
	if diagnostic.AgentExitStatus != nil && *diagnostic.AgentExitStatus != 0 {
		return "agent_timeout"
	}
	return ""
}

func artifactChecksField(raw map[string]any, key string) []HarnessArtifactCheck {
	values, _ := raw[key].([]any)
	checks := make([]HarnessArtifactCheck, 0, len(values))
	for _, value := range values {
		item, _ := value.(map[string]any)
		if len(item) == 0 {
			continue
		}
		checks = append(checks, HarnessArtifactCheck{
			Path:      stringField(item, "path"),
			Concrete:  boolField(item, "concrete"),
			Exists:    boolField(item, "exists"),
			SizeBytes: int64PointerField(item, "size_bytes"),
		})
	}
	return checks
}

func repairDiagnosticField(raw map[string]any, key string) HarnessRepairDiagnostic {
	item, _ := raw[key].(map[string]any)
	if len(item) == 0 {
		return HarnessRepairDiagnostic{}
	}
	return HarnessRepairDiagnostic{
		Triggered:           boolField(item, "triggered"),
		ExitStatus:          intPointerField(item, "exit_status"),
		MissingBeforeRepair: stringSliceField(item, "missing_before_repair"),
	}
}

func stringField(raw map[string]any, key string) string {
	value, _ := raw[key].(string)
	return value
}

func boolField(raw map[string]any, key string) bool {
	value, _ := raw[key].(bool)
	return value
}

func intPointerField(raw map[string]any, key string) *int {
	switch value := raw[key].(type) {
	case float64:
		parsed := int(value)
		return &parsed
	case int:
		parsed := value
		return &parsed
	default:
		return nil
	}
}

func int64PointerField(raw map[string]any, key string) *int64 {
	switch value := raw[key].(type) {
	case float64:
		parsed := int64(value)
		return &parsed
	case int64:
		parsed := value
		return &parsed
	case int:
		parsed := int64(value)
		return &parsed
	default:
		return nil
	}
}

func stringSliceField(raw map[string]any, key string) []string {
	values, _ := raw[key].([]any)
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if text, ok := value.(string); ok {
			out = append(out, text)
		}
	}
	return out
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
