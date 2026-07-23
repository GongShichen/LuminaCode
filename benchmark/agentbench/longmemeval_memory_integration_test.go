//go:build darwin && cgo

package agentbench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strconv"
	"strings"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
)

type localRetrievalMemoryProbeRunner struct{}

func (localRetrievalMemoryProbeRunner) RunAnswer(ctx context.Context, cfg config.Config, question,
	sessionID string, queryTime time.Time) AgentRunResult {
	fabric, err := agent.OpenConfiguredMemoryFabric(ctx, cfg, false)
	if err != nil {
		return AgentRunResult{ErrorType: err.Error()}
	}
	state := agent.NewAgentState()
	state.MemorySessionID = sessionID
	state.MemoryQueryTime = queryTime
	state.MemoryQueryTimeExplicit = !queryTime.IsZero()
	recalls := agent.RunMemoryRecallWithEngine(ctx, cfg, &state, question, fabric)
	if err := fabric.Close(); err != nil {
		return AgentRunResult{ErrorType: err.Error()}
	}
	if len(recalls) == 0 {
		return AgentRunResult{ErrorType: "local retrieval returned no evidence"}
	}
	contents := make([]string, 0, len(recalls))
	for _, recall := range recalls {
		contents = append(contents, recall.Content)
	}
	return AgentRunResult{FinalText: strings.Join(contents, "\n")}
}

func TestLongMemEvalLocalRetrievalNativeMemoryStaysBounded(t *testing.T) {
	indexDir := strings.TrimSpace(os.Getenv("LUMINA_TEST_LONGMEMEVAL_INDEX_DIR"))
	datasetPath := strings.TrimSpace(os.Getenv("LUMINA_TEST_LONGMEMEVAL_DATASET"))
	if indexDir == "" || datasetPath == "" {
		t.Skip("set LUMINA_TEST_LONGMEMEVAL_INDEX_DIR and LUMINA_TEST_LONGMEMEVAL_DATASET")
	}
	data, err := os.ReadFile(datasetPath)
	if err != nil {
		t.Fatal(err)
	}
	var cases []longMemEvalCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) < 10 {
		t.Fatalf("dataset has %d cases, want at least 10", len(cases))
	}
	root := t.TempDir()
	options := RunnerOptions{Config: config.GetConfig(), WorkDir: filepath.Join(root, "work"),
		ArtifactsDir: filepath.Join(root, "artifacts"), LongMemEvalIndexDir: indexDir,
		LongMemEvalRunID: "local-memory-probe"}
	fingerprint := strings.Repeat("a", 64)
	runner := localRetrievalMemoryProbeRunner{}
	var baseline, peak uint64
	evidenceHits := 0
	loggedCases := parseLongMemEvalTestCaseSet(os.Getenv("LUMINA_TEST_LONGMEMEVAL_LOG_EVIDENCE"))
	for index, c := range cases[:10] {
		manifest, err := readLongMemEvalIndexManifest(longMemEvalIndexManifestPath(indexDir, c.QuestionID))
		if err != nil {
			t.Fatal(err)
		}
		result := runLongMemEvalAnswerCase(context.Background(), c, manifest, fingerprint, options, runner)
		if result.ErrorType != "" {
			t.Fatalf("case %s: %s", c.QuestionID, result.ErrorType)
		}
		evidenceHit := longMemEvalEvidenceContainsExpected(result.Hypothesis, c.Answer)
		if evidenceHit {
			evidenceHits++
		} else {
			evidence := []rune(result.Hypothesis)
			if len(evidence) > 1200 {
				evidence = evidence[:1200]
			}
			t.Logf("case=%s expected=%v evidence=%s", c.QuestionID, c.Answer, string(evidence))
		}
		if _, ok := loggedCases[c.QuestionID]; ok {
			t.Logf("case=%s full_evidence:\n%s", c.QuestionID, result.Hypothesis)
		}
		debug.FreeOSMemory()
		time.Sleep(100 * time.Millisecond)
		current := currentAgentbenchFootprint(t)
		if index == 0 {
			baseline = current
			peak = current
		}
		if current > peak {
			peak = current
		}
		t.Logf("case=%s duration=%s footprint=%s evidence_hit=%t", c.QuestionID,
			time.Duration(result.DurationSeconds*float64(time.Second)), formatAgentbenchBytes(current), evidenceHit)
	}
	final := currentAgentbenchFootprint(t)
	t.Logf("footprint baseline=%s peak=%s final=%s evidence_hits=%d/10", formatAgentbenchBytes(baseline),
		formatAgentbenchBytes(peak), formatAgentbenchBytes(final), evidenceHits)
	const maxGrowth = 2 << 30
	if final > baseline+maxGrowth {
		t.Fatalf("local retrieval footprint grew by %s after warm-up", formatAgentbenchBytes(final-baseline))
	}
}

func parseLongMemEvalTestCaseSet(value string) map[string]struct{} {
	result := map[string]struct{}{}
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			result[item] = struct{}{}
		}
	}
	return result
}

func longMemEvalEvidenceContainsExpected(evidence string, expected any) bool {
	normalized := normalizeAnswer(evidence)
	switch value := expected.(type) {
	case string:
		return strings.Contains(normalized, normalizeAnswer(value))
	case []any:
		for _, item := range value {
			if longMemEvalEvidenceContainsExpected(evidence, item) {
				return true
			}
		}
	case []string:
		for _, item := range value {
			if longMemEvalEvidenceContainsExpected(evidence, item) {
				return true
			}
		}
	}
	return false
}

var agentbenchFootprintPattern = regexp.MustCompile(`Footprint:\s+([0-9.]+)\s+([KMGT]B)`)

func currentAgentbenchFootprint(t *testing.T) uint64 {
	t.Helper()
	output, err := exec.Command("/usr/bin/footprint", strconv.Itoa(os.Getpid())).CombinedOutput()
	if err != nil {
		t.Fatalf("read native footprint: %v: %s", err, output)
	}
	match := agentbenchFootprintPattern.FindStringSubmatch(string(output))
	if len(match) != 3 {
		t.Fatalf("parse native footprint from: %s", output)
	}
	value, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		t.Fatal(err)
	}
	multiplier := map[string]float64{"KB": 1 << 10, "MB": 1 << 20, "GB": 1 << 30, "TB": 1 << 40}[match[2]]
	return uint64(value * multiplier)
}

func formatAgentbenchBytes(value uint64) string {
	return fmt.Sprintf("%.2f GiB", float64(value)/(1<<30))
}
