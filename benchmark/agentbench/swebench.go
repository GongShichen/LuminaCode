package agentbench

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type sweBenchPrediction struct {
	InstanceID      string `json:"instance_id"`
	ModelNameOrPath string `json:"model_name_or_path"`
	ModelPatch      string `json:"model_patch"`
}

func WriteSWEBenchPredictions(report Report, outputDir string) (string, error) {
	path := filepath.Join(outputDir, "swebench-predictions-"+strings.ReplaceAll(report.GeneratedAt, ":", "")+".jsonl")
	file, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	for _, result := range report.Results {
		patch := ""
		if result.FinalPatchPath != "" {
			data, err := os.ReadFile(result.FinalPatchPath)
			if err == nil {
				patch = string(data)
			}
		}
		instanceID := result.Case.InstanceID
		if instanceID == "" {
			instanceID = result.Case.ID
		}
		line, err := json.Marshal(sweBenchPrediction{
			InstanceID:      instanceID,
			ModelNameOrPath: report.Model,
			ModelPatch:      patch,
		})
		if err != nil {
			return "", err
		}
		if _, err := file.Write(append(line, '\n')); err != nil {
			return "", err
		}
	}
	return path, nil
}

func runSWEBenchHarness(ctx context.Context, command, predictionsPath, outputDir string) (string, int, map[string]any) {
	outputPath := filepath.Join(outputDir, "swebench-harness-output.txt")
	result := RunShellCommand(ctx, outputDir, "PREDICTIONS_PATH="+shellQuote(predictionsPath)+" "+command, 24*time.Hour)
	combined := result.Stdout
	if result.Stderr != "" {
		combined += "\n" + result.Stderr
	}
	_ = os.WriteFile(outputPath, []byte(combined), 0o644)
	return outputPath, result.ExitCode, parseSWEBenchHarnessOutput(combined)
}

func parseSWEBenchHarnessOutput(output string) map[string]any {
	stats := map[string]any{}
	resolvedRe := regexp.MustCompile(`(?i)resolved[^0-9]*(\d+)`)
	totalRe := regexp.MustCompile(`(?i)(total|instances)[^0-9]*(\d+)`)
	if match := resolvedRe.FindStringSubmatch(output); len(match) == 2 {
		stats["resolved"] = match[1]
	}
	if match := totalRe.FindStringSubmatch(output); len(match) >= 3 {
		stats["total"] = match[2]
	}
	return stats
}
