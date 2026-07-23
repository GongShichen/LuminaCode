//go:build cgo

package localmodel

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/sugarme/tokenizer"
	ort "github.com/yalue/onnxruntime_go"
)

const bgeProviderSelectionSchema = "lumina-bge-provider-selection-v1"

type bgeProviderSelection struct {
	Schema                 string `json:"schema"`
	Fingerprint            string `json:"fingerprint"`
	Candidate              string `json:"candidate"`
	Selected               string `json:"selected"`
	Reason                 string `json:"reason"`
	CPUDurationNanos       int64  `json:"cpu_duration_nanos,omitempty"`
	CandidateDurationNanos int64  `json:"candidate_duration_nanos,omitempty"`
}

func selectBGEExecutionProvider(modelDir, modelPath string, tok *tokenizer.Tokenizer,
	colbertWeight, colbertBias, sparseWeight []float32, sparseBias float32) (
	string, *ort.DynamicAdvancedSession, error) {
	requested, explicit := requestedBGEExecutionProvider(modelDir)
	threadBudget := bgeSessionThreadBudget(bgeSessionPoolSize())
	if explicit {
		session, err := newLocalBGESession(modelPath, threadBudget, requested)
		if err != nil {
			return "", nil, fmt.Errorf("load explicitly requested BGE-M3 %s provider: %w", requested, err)
		}
		return requested, session, nil
	}
	fingerprint := bgeProviderFingerprint(modelDir, requested, bgeModelIdentity(modelDir))
	if cached, ok := loadBGEProviderSelection(modelDir, fingerprint); ok {
		session, err := newLocalBGESession(modelPath, threadBudget, cached.Selected)
		if err == nil {
			return cached.Selected, session, nil
		}
	}
	cpuSession, err := newLocalBGESession(modelPath, threadBudget, "cpu")
	if err != nil {
		return "", nil, err
	}
	if requested == "cpu" {
		storeBGEProviderSelection(modelDir, bgeProviderSelection{
			Schema: bgeProviderSelectionSchema, Fingerprint: fingerprint, Candidate: "cpu",
			Selected: "cpu", Reason: "no compatible accelerator runtime was installed",
		})
		return "cpu", cpuSession, nil
	}
	probe := &LocalBGEEncoder{
		tokenizer: tok, colbertWeight: colbertWeight, colbertBias: colbertBias,
		sparseWeight: sparseWeight, sparseBias: sparseBias,
	}
	cpuDuration, cpuOutput, cpuErr := benchmarkBGESession(cpuSession, probe)
	_ = cpuSession.Destroy()
	if cpuErr != nil {
		return "", nil, fmt.Errorf("probe BGE-M3 CPU provider: %w", cpuErr)
	}
	candidateSession, candidateErr := newLocalBGESession(modelPath, threadBudget, requested)
	if candidateErr != nil {
		storeBGEProviderSelection(modelDir, bgeProviderSelection{
			Schema: bgeProviderSelectionSchema, Fingerprint: fingerprint, Candidate: requested,
			Selected: "cpu", Reason: "accelerator provider could not load: " + candidateErr.Error(),
		})
		cpuSession, err = newLocalBGESession(modelPath, threadBudget, "cpu")
		return "cpu", cpuSession, err
	}
	candidateDuration, candidateOutput, acceleratorErr := benchmarkBGESession(candidateSession, probe)
	selection := bgeProviderSelection{
		Schema: bgeProviderSelectionSchema, Fingerprint: fingerprint, Candidate: requested,
		Selected: "cpu", CPUDurationNanos: cpuDuration.Nanoseconds(),
		CandidateDurationNanos: candidateDuration.Nanoseconds(),
	}
	switch {
	case acceleratorErr != nil:
		selection.Reason = "accelerator inference probe failed: " + acceleratorErr.Error()
	case !equivalentBGEProbeOutput(cpuOutput, candidateOutput):
		selection.Reason = "accelerator output failed the CPU consistency gate"
	case candidateDuration*100 >= cpuDuration*95:
		selection.Reason = "accelerator was not at least five percent faster than CPU"
	default:
		selection.Selected = requested
		selection.Reason = "accelerator passed output and throughput gates"
	}
	storeBGEProviderSelection(modelDir, selection)
	if selection.Selected == requested {
		return requested, candidateSession, nil
	}
	_ = candidateSession.Destroy()
	cpuSession, err = newLocalBGESession(modelPath, threadBudget, "cpu")
	return "cpu", cpuSession, err
}

func requestedBGEExecutionProvider(modelDir string) (string, bool) {
	for _, name := range []string{"LUMINA_MEMORY_BGE_EXECUTION_PROVIDER", "LUMINA_MEMORY_EMBEDDING_DEVICE"} {
		if configured := strings.ToLower(strings.TrimSpace(os.Getenv(name))); configured != "" && configured != "auto" {
			return normalizeBGEExecutionProvider(configured), true
		}
	}
	content, _ := os.ReadFile(filepath.Join(modelDir, "runtime", "provider"))
	return normalizeBGEExecutionProvider(strings.TrimSpace(string(content))), false
}

func normalizeBGEExecutionProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "coreml", "metal":
		return "coreml"
	case "cuda", "nvidia":
		return "cuda"
	case "directml", "dml":
		return "directml"
	case "migraphx":
		return "migraphx"
	case "rocm":
		return "rocm"
	case "amd":
		if runtime.GOOS == "windows" {
			return "directml"
		}
		if runtime.GOOS == "linux" {
			return "migraphx"
		}
		return "cpu"
	default:
		return "cpu"
	}
}

func benchmarkBGESession(session *ort.DynamicAdvancedSession, encoder *LocalBGEEncoder) (
	time.Duration, []BGEEmbedding, error) {
	const (
		batchSize = 32
		tokens    = 128
	)
	inputs := make([]bgeInput, batchSize)
	for batch := range inputs {
		inputs[batch].ids = make([]int64, tokens)
		inputs[batch].attention = make([]int64, tokens)
		inputs[batch].specialMask = make([]int, tokens)
		for index := 0; index < tokens; index++ {
			inputs[batch].ids[index] = int64(1000 + (batch*tokens+index)%4096)
			inputs[batch].attention[index] = 1
		}
	}
	if _, err := encoder.encodePreparedBatch(session, inputs, false); err != nil {
		return 0, nil, err
	}
	start := time.Now()
	output, err := encoder.encodePreparedBatch(session, inputs, false)
	return time.Since(start), output, err
}

func equivalentBGEProbeOutput(left, right []BGEEmbedding) bool {
	if len(left) != len(right) || len(left) == 0 {
		return false
	}
	for index := range left {
		if len(left[index].Dense) != len(right[index].Dense) || len(left[index].Dense) == 0 {
			return false
		}
		var dot, leftNorm, rightNorm float64
		for dimension, value := range left[index].Dense {
			other := right[index].Dense[dimension]
			dot += float64(value * other)
			leftNorm += float64(value * value)
			rightNorm += float64(other * other)
		}
		denominator := math.Sqrt(leftNorm * rightNorm)
		if denominator == 0 || dot/denominator < .999 {
			return false
		}
	}
	return true
}

func bgeProviderFingerprint(modelDir, candidate, modelIdentity string) string {
	runtimePath, _ := findBGERuntime(modelDir)
	var runtimeState string
	if info, err := os.Stat(runtimePath); err == nil {
		runtimeState = fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
	}
	modelPath := filepath.Join(modelDir, "onnx", "model.onnx")
	var modelState string
	if info, err := os.Stat(modelPath); err == nil {
		modelState = fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
	}
	return fmt.Sprintf("%s|%s|%s|%s|%d|%s|%s|%s", modelIdentity, runtime.GOOS, runtime.GOARCH,
		candidate, runtime.NumCPU(), runtimePath, runtimeState, modelState)
}

func bgeProviderSelectionPath(modelDir string) string {
	return filepath.Join(modelDir, "runtime", "provider-selection.json")
}

func loadBGEProviderSelection(modelDir, fingerprint string) (bgeProviderSelection, bool) {
	var selection bgeProviderSelection
	content, err := os.ReadFile(bgeProviderSelectionPath(modelDir))
	if err != nil || json.Unmarshal(content, &selection) != nil {
		return bgeProviderSelection{}, false
	}
	return selection, selection.Schema == bgeProviderSelectionSchema &&
		selection.Fingerprint == fingerprint && selection.Selected != ""
}

func storeBGEProviderSelection(modelDir string, selection bgeProviderSelection) {
	content, err := json.MarshalIndent(selection, "", "  ")
	if err != nil {
		return
	}
	content = append(content, '\n')
	path := bgeProviderSelectionPath(modelDir)
	temp := path + ".tmp"
	if os.MkdirAll(filepath.Dir(path), 0o700) != nil ||
		os.WriteFile(temp, content, 0o600) != nil ||
		os.Rename(temp, path) != nil {
		_ = os.Remove(temp)
	}
}
