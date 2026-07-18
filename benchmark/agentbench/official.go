package agentbench

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"LuminaCode/apppaths"
	"LuminaCode/config"
	bashpkg "LuminaCode/tools/bash"
)

func IsOfficialSuite(suite string) bool {
	switch suite {
	case SuiteTerminalBench, SuiteTauBench, SuiteSWEBenchVerified:
		return true
	default:
		return false
	}
}

func DefaultBenchmarkDir(rootDir, suite string) string {
	switch suite {
	case SuiteTerminalBench:
		return filepath.Join(rootDir, "terminal-bench")
	case SuiteTauBench:
		return filepath.Join(rootDir, "tau-bench")
	case SuiteSWEBenchVerified:
		return filepath.Join(rootDir, "swe-bench")
	default:
		return filepath.Join(rootDir, sanitizeCaseID(suite))
	}
}

func RunOfficialSuite(ctx context.Context, options RunnerOptions) (Report, error) {
	options = normalizeOptions(options)
	if !IsOfficialSuite(options.Suite) {
		return Report{}, fmt.Errorf("%s is not an official benchmark suite", options.Suite)
	}
	if strings.TrimSpace(options.HarnessCmd) == "" && strings.TrimSpace(options.SWEBenchHarnessCmd) != "" {
		options.HarnessCmd = options.SWEBenchHarnessCmd
	}
	if strings.TrimSpace(options.HarnessCmd) == "" {
		return Report{}, fmt.Errorf("%s requires -harness-cmd pointing to the official benchmark harness command", options.Suite)
	}
	if err := validatePreparedOfficialHarness(options); err != nil {
		return Report{}, err
	}
	if options.BenchmarkDir == "" {
		options.BenchmarkDir = DefaultBenchmarkDir(options.RootDir, options.Suite)
	}
	for _, dir := range []string{options.OutputDir, options.WorkDir, options.ArtifactsDir, options.BenchmarkDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return Report{}, err
		}
	}
	if err := prepareOfficialSuiteAssets(ctx, &options); err != nil {
		return Report{}, err
	}
	start := options.Now()
	debugRun := options.Limit > 0 || strings.TrimSpace(options.CaseID) != ""
	before := gitStatusShort(ctx, options.BenchmarkDir)
	result := runOfficialHarness(ctx, options, start)
	after := gitStatusShort(ctx, options.BenchmarkDir)
	exitCode := result.ExitCode
	harnessOutput := result.Stdout + "\n" + result.Stderr
	metrics := parseOfficialHarnessMetrics(harnessOutput)
	summary := summaryFromOfficialHarness(exitCode, metrics)
	diagnostics := collectOfficialHarnessDiagnostics(options, harnessOutput)
	mergeDiagnosticFailures(&summary, diagnostics)
	options.Config = config.ReloadDynamicConfig(options.Config)
	report := Report{
		Suite:                options.Suite,
		GeneratedAt:          start.Format(time.RFC3339),
		DebugRun:             debugRun,
		RootDir:              options.RootDir,
		OutputDir:            options.OutputDir,
		WorkDir:              options.WorkDir,
		BenchmarkDir:         options.BenchmarkDir,
		Model:                options.Config.APIModel,
		Summary:              summary,
		Results:              nil,
		HarnessOutputPath:    result.OutputPath,
		HarnessCommand:       options.HarnessCmd,
		HarnessExitCode:      &exitCode,
		HarnessParsedStats:   metrics,
		OfficialMetrics:      metrics,
		LuminaDiagnostics:    diagnostics,
		UpstreamStatusBefore: before,
		UpstreamStatusAfter:  after,
		UpstreamDirtyAfter:   strings.TrimSpace(after) != "",
	}
	return report, nil
}

func validatePreparedOfficialHarness(options RunnerOptions) error {
	if !options.PreparedEnv || options.Suite != SuiteTerminalBench {
		return nil
	}
	missing := []string{}
	for _, flag := range []string{"--no-rebuild", "--no-cleanup"} {
		if !strings.Contains(options.HarnessCmd, flag) {
			missing = append(missing, flag)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("terminal_bench -prepared-env requires harness command to include %s so prebuilt task images are reused and not deleted", strings.Join(missing, " and "))
	}
	return nil
}

type officialHarnessResult struct {
	Command    string
	ExitCode   int
	Stdout     string
	Stderr     string
	OutputPath string
	Duration   time.Duration
}

func runOfficialHarness(ctx context.Context, options RunnerOptions, now time.Time) officialHarnessResult {
	outputPath := filepath.Join(options.OutputDir, suiteReportBaseName(options.Suite, now)+"-harness-output.txt")
	start := time.Now()
	argv := bashpkg.ShellArgv(options.HarnessCmd, "")
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = options.BenchmarkDir
	cmd.Env = officialHarnessEnv(os.Environ(), options)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
			stderr.WriteString(err.Error())
		}
	}
	combined := stdout.String()
	if stderr.Len() > 0 {
		combined += "\n" + stderr.String()
	}
	_ = os.WriteFile(outputPath, []byte(combined), 0o644)
	return officialHarnessResult{
		Command:    options.HarnessCmd,
		ExitCode:   exitCode,
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		OutputPath: outputPath,
		Duration:   time.Since(start),
	}
}

func officialHarnessEnv(base []string, options RunnerOptions) []string {
	env := append([]string{}, base...)
	repoRoot := findRepoRootFromCWD()
	env = append(env, prependPathListEnv(base, defaultHarnessPathPrefixes()))
	if !envHasKey(base, "DOCKER_HOST") {
		if dockerHost := defaultDockerHost(); dockerHost != "" {
			env = append(env, "DOCKER_HOST="+dockerHost)
		}
	}
	env = append(env,
		"LUMINA_BENCHMARK_SUITE="+options.Suite,
		"LUMINA_BENCHMARK_ROOT="+options.RootDir,
		"LUMINA_BENCHMARK_DIR="+options.BenchmarkDir,
		"LUMINA_BENCHMARK_WORK_DIR="+options.WorkDir,
		"LUMINA_BENCHMARK_ARTIFACTS_DIR="+options.ArtifactsDir,
		"LUMINA_BENCHMARK_OUTPUT_DIR="+options.OutputDir,
		"LUMINA_BENCHMARK_DEBUG_RUN="+strconv.FormatBool(options.Limit > 0 || strings.TrimSpace(options.CaseID) != ""),
		"LUMINA_AGENT_MODEL="+options.Config.APIModel,
		"LUMINA_AGENT_API_TYPE="+options.Config.APIType,
		"LUMINA_API_KEY="+options.Config.APIKey,
		"LUMINA_API_BASE_URL="+options.Config.APIBaseURL,
		"LUMINA_API_MODEL="+options.Config.APIModel,
		"LUMINA_API_TYPE="+options.Config.APIType,
		"YOLO_MODE=true",
		"LUMINA_MAX_PARENT_TURNS="+strconv.Itoa(options.Config.MaxParentTurns),
	)
	if options.Suite == SuiteTerminalBench {
		env = append(env, "LUMINA_HARNESS_MODE=terminal-bench")
	}
	if repoRoot != "" {
		env = append(env, prependPathEnv(base, "PYTHONPATH", repoRoot))
		env = append(env, "LUMINA_TBENCH_AGENT_IMPORT_PATH=benchmark.agentbench.terminal_adapter.lumina_terminal_agent:LuminaTerminalAgent")
	}
	if options.Suite == SuiteTerminalBench {
		installScript := terminalBenchInstallScriptPath(options.RootDir)
		if installScript != "" {
			env = append(env, "LUMINA_TBENCH_INSTALL_SCRIPT="+installScript)
		}
	}
	if options.CaseID != "" {
		env = append(env, "LUMINA_BENCHMARK_CASE="+options.CaseID)
	}
	if options.Limit > 0 {
		env = append(env, "LUMINA_BENCHMARK_LIMIT="+strconv.Itoa(options.Limit))
	}
	if runnerCmd := defaultAgentRunnerCommand(); runnerCmd != "" {
		env = append(env, "LUMINA_AGENT_RUNNER="+runnerCmd)
	}
	return env
}

func envHasKey(env []string, key string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			return true
		}
	}
	return false
}

func defaultDockerHost() string {
	candidates := []string{}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".colima", "default", "docker.sock"),
			filepath.Join(home, ".docker", "run", "docker.sock"),
		)
	}
	candidates = append(candidates, "/var/run/docker.sock")
	for _, path := range candidates {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return "unix://" + path
		}
	}
	return ""
}

func defaultHarnessPathPrefixes() []string {
	values := []string{}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		values = append(values, filepath.Join(home, ".local", "bin"))
	}
	values = append(values, "/opt/homebrew/bin", "/usr/local/bin")
	return values
}

func prependPathListEnv(base []string, values []string) string {
	current := ""
	for _, entry := range base {
		if strings.HasPrefix(entry, "PATH=") {
			current = strings.TrimPrefix(entry, "PATH=")
			break
		}
	}
	seen := map[string]struct{}{}
	parts := []string{}
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		parts = append(parts, value)
	}
	for _, value := range filepath.SplitList(current) {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		parts = append(parts, value)
	}
	return "PATH=" + strings.Join(parts, string(os.PathListSeparator))
}

func defaultAgentRunnerCommand() string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return ""
	}
	root := findRepoRoot(wd)
	if root == "" {
		root = wd
	}
	return "go run " + shellQuote(filepath.Join(root, "cmd", "lumina_benchmark_agent"))
}

func findRepoRootFromCWD() string {
	wd, err := os.Getwd()
	if err != nil || wd == "" {
		return ""
	}
	return findRepoRoot(wd)
}

func prependPathEnv(base []string, key string, value string) string {
	prefix := key + "="
	for _, entry := range base {
		if strings.HasPrefix(entry, prefix) {
			current := strings.TrimPrefix(entry, prefix)
			if current == "" {
				return prefix + value
			}
			return prefix + value + string(os.PathListSeparator) + current
		}
	}
	return prefix + value
}

func terminalBenchInstallScriptPath(rootDir string) string {
	if rootDir == "" {
		return ""
	}
	return filepath.Join(rootDir, "lumina-assets", "terminal-bench", "install-lumina-agent.sh")
}

func prepareOfficialSuiteAssets(ctx context.Context, options *RunnerOptions) error {
	if options == nil || options.Suite != SuiteTerminalBench {
		return nil
	}
	repoRoot := findRepoRootFromCWD()
	if repoRoot == "" {
		return fmt.Errorf("cannot locate LuminaCode repo root for Terminal-Bench assets")
	}
	assetDir := filepath.Join(options.RootDir, "lumina-assets", "terminal-bench")
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		return err
	}
	binPath := filepath.Join(assetDir, "lumina-linux-"+runtime.GOARCH)
	build := exec.CommandContext(ctx, "go", "build", "-o", binPath, ".")
	build.Dir = repoRoot
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH="+runtime.GOARCH)
	if output, err := build.CombinedOutput(); err != nil {
		return fmt.Errorf("build linux lumina binary failed: %w\n%s", err, output)
	}
	resources, err := archiveLuminaResources(apppaths.ProjectLocalRoot(repoRoot))
	if err != nil {
		return err
	}
	binary, err := os.ReadFile(binPath)
	if err != nil {
		return err
	}
	script := renderTerminalBenchInstallScript(binary, resources)
	installScript := terminalBenchInstallScriptPath(options.RootDir)
	if err := os.WriteFile(installScript, []byte(script), 0o755); err != nil {
		return err
	}
	return nil
}

func archiveLuminaResources(resourceDir string) ([]byte, error) {
	info, err := os.Stat(resourceDir)
	if err != nil {
		return nil, fmt.Errorf("Lumina resources not found at %s: %w", resourceDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("Lumina resources path is not a directory: %s", resourceDir)
	}
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(resourceDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(resourceDir, path)
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		header.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		_, err = io.Copy(tw, file)
		return err
	})
	if err != nil {
		_ = tw.Close()
		_ = gz.Close()
		return nil, err
	}
	if err := tw.Close(); err != nil {
		_ = gz.Close()
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func renderTerminalBenchInstallScript(binary []byte, resources []byte) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("set -eu\n")
	b.WriteString("mkdir -p /usr/local/bin /usr/local/share/lumina\n")
	b.WriteString("cat > /tmp/lumina.b64 <<'LUMINA_BIN_EOF'\n")
	b.WriteString(base64.StdEncoding.EncodeToString(binary))
	b.WriteString("\nLUMINA_BIN_EOF\n")
	b.WriteString("(base64 -d /tmp/lumina.b64 2>/dev/null || base64 --decode /tmp/lumina.b64) > /usr/local/bin/lumina\n")
	b.WriteString("chmod +x /usr/local/bin/lumina\n")
	b.WriteString("cat > /tmp/lumina-resources.tgz.b64 <<'LUMINA_RESOURCES_EOF'\n")
	b.WriteString(base64.StdEncoding.EncodeToString(resources))
	b.WriteString("\nLUMINA_RESOURCES_EOF\n")
	b.WriteString("(base64 -d /tmp/lumina-resources.tgz.b64 2>/dev/null || base64 --decode /tmp/lumina-resources.tgz.b64) > /tmp/lumina-resources.tgz\n")
	b.WriteString("tar -xzf /tmp/lumina-resources.tgz -C /usr/local/share/lumina\n")
	b.WriteString("rm -f /tmp/lumina.b64 /tmp/lumina-resources.tgz /tmp/lumina-resources.tgz.b64\n")
	b.WriteString("LUMINA_RESOURCE_ROOT=/usr/local/share/lumina lumina --help >/dev/null || true\n")
	return b.String()
}

func findRepoRoot(start string) string {
	current, err := filepath.Abs(start)
	if err != nil {
		current = start
	}
	for {
		if _, err := os.Stat(filepath.Join(current, "go.mod")); err == nil {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

func gitStatusShort(ctx context.Context, dir string) string {
	if dir == "" {
		return ""
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err != nil {
		return ""
	}
	cmd := exec.CommandContext(ctx, "git", "status", "--short")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func parseOfficialHarnessMetrics(output string) map[string]any {
	metrics := map[string]any{}
	if parsed := parseLastJSONObject(output); len(parsed) > 0 {
		for key, value := range parsed {
			metrics[key] = value
		}
	}
	patterns := map[string]string{
		"resolved":  `(?i)\bresolved\b[^0-9]*(\d+)`,
		"passed":    `(?i)\b(pass(?:ed)?)\b[^0-9]*(\d+)`,
		"failed":    `(?i)\b(fail(?:ed)?)\b[^0-9]*(\d+)`,
		"total":     `(?i)\b(total|instances|tasks|cases)\b[^0-9]*(\d+)`,
		"pass_rate": `(?i)\b(pass rate|accuracy|score)\b[^0-9]*(\d+(?:\.\d+)?)%?`,
	}
	for key, pattern := range patterns {
		if _, exists := metrics[key]; exists {
			continue
		}
		re := regexp.MustCompile(pattern)
		if match := re.FindStringSubmatch(output); len(match) > 0 {
			value := match[len(match)-1]
			if strings.Contains(value, ".") {
				if f, err := strconv.ParseFloat(value, 64); err == nil {
					if key == "pass_rate" && f > 1 {
						f = f / 100
					}
					metrics[key] = f
				}
			} else if i, err := strconv.Atoi(value); err == nil {
				metrics[key] = i
			}
		}
	}
	return metrics
}

func parseLastJSONObject(output string) map[string]any {
	lines := strings.Split(output, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			continue
		}
		var parsed map[string]any
		if err := json.Unmarshal([]byte(line), &parsed); err == nil {
			return parsed
		}
	}
	return nil
}

func summaryFromOfficialHarness(exitCode int, metrics map[string]any) SuiteSummary {
	total := intMetric(metrics, "total")
	resolved := intMetric(metrics, "resolved")
	if resolved == 0 {
		resolved = intMetric(metrics, "passed")
	}
	passRate := floatMetric(metrics, "pass_rate")
	if total > 0 && passRate == 0 {
		passRate = float64(resolved) / float64(total)
	}
	if total == 0 {
		total = 1
		if exitCode == 0 {
			resolved = 1
			passRate = 1
		}
	}
	failures := map[string]int{}
	if exitCode != 0 {
		failures["official_harness_failed"] = 1
	}
	return SuiteSummary{
		TotalCases:        total,
		ResolvedCases:     resolved,
		PassRate:          passRate,
		FailureCategories: failures,
	}
}

func intMetric(metrics map[string]any, key string) int {
	switch v := metrics[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case string:
		i, _ := strconv.Atoi(v)
		return i
	default:
		return 0
	}
}

func floatMetric(metrics map[string]any, key string) float64 {
	switch v := metrics[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case string:
		f, _ := strconv.ParseFloat(v, 64)
		return f
	default:
		return 0
	}
}
