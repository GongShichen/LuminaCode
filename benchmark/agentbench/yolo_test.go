package agentbench

import (
	"strings"
	"testing"

	"LuminaCode/config"
)

func TestBenchmarkEntryPointsDefaultToYolo(t *testing.T) {
	options := normalizeOptions(RunnerOptions{Config: config.Config{}})
	if !options.Config.Yolo {
		t.Fatal("benchmark runner options must default to YOLO")
	}

	env := officialHarnessEnv([]string{"PATH=/usr/bin"}, RunnerOptions{Suite: SuiteTerminalBench})
	if !containsEnvValue(env, "YOLO_MODE=true") {
		t.Fatalf("official harness environment missing YOLO_MODE=true: %#v", env)
	}

	cfg := config.Config{HarnessMode: "terminal-bench", ShellTimeoutSeconds: 30}
	config.ApplyHarnessDefaults(&cfg)
	if !cfg.Yolo || cfg.ShellTimeoutSeconds != 120 {
		t.Fatalf("harness defaults should enable YOLO and terminal timeout, got yolo=%t timeout=%v", cfg.Yolo, cfg.ShellTimeoutSeconds)
	}
}

func containsEnvValue(env []string, want string) bool {
	for _, value := range env {
		if strings.TrimSpace(value) == want {
			return true
		}
	}
	return false
}
