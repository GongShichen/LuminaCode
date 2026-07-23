//go:build cgo

package localmodel

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"

	ort "github.com/yalue/onnxruntime_go"
)

var bgeRuntime struct {
	sync.Mutex
	initialized bool
	path        string
}

func initializeBGERuntime(modelDir string) error {
	bgeRuntime.Lock()
	defer bgeRuntime.Unlock()
	if bgeRuntime.initialized || ort.IsInitialized() {
		bgeRuntime.initialized = true
		return nil
	}
	runtimePath, err := findBGERuntime(modelDir)
	if err != nil {
		return err
	}
	ort.SetSharedLibraryPath(runtimePath)
	if err := ort.InitializeEnvironment(ort.WithLogLevelError()); err != nil {
		return fmt.Errorf("initialize ONNX Runtime for BGE-M3: %w", err)
	}
	bgeRuntime.initialized = true
	bgeRuntime.path = runtimePath
	return nil
}

func findBGERuntime(modelDir string) (string, error) {
	if configured := strings.TrimSpace(os.Getenv("LUMINA_ONNXRUNTIME_PATH")); configured != "" {
		configured = ExpandPath(configured)
		if info, err := os.Stat(configured); err == nil && !info.IsDir() {
			return configured, nil
		}
		return "", fmt.Errorf("LUMINA_ONNXRUNTIME_PATH does not point to a file: %s", configured)
	}
	var names []string
	switch runtime.GOOS {
	case "darwin":
		names = []string{"libonnxruntime.dylib"}
	case "windows":
		names = []string{"onnxruntime.dll"}
	default:
		names = []string{"libonnxruntime.so", "libonnxruntime.so.1"}
	}
	for _, directory := range []string{modelDir, filepath.Join(modelDir, "runtime"),
		filepath.Join(modelDir, "runtime", "lib"), filepath.Join(modelDir, "lib")} {
		for _, name := range names {
			path := filepath.Join(directory, name)
			if info, err := os.Stat(path); err == nil && !info.IsDir() {
				return path, nil
			}
		}
	}
	return "", fmt.Errorf("ONNX Runtime library not found under %s; run make install or make doctor", modelDir)
}

func configureBGESessionOptions(options *ort.SessionOptions, cpuThreads int) error {
	var optimizationLevel ort.GraphOptimizationLevel = ort.GraphOptimizationLevelEnableAll
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LUMINA_MEMORY_BGE_GRAPH_OPTIMIZATION"))) {
	case "disabled", "none":
		optimizationLevel = ort.GraphOptimizationLevelDisableAll
	case "basic":
		optimizationLevel = ort.GraphOptimizationLevelEnableBasic
	case "extended":
		optimizationLevel = ort.GraphOptimizationLevelEnableExtended
	case "", "all":
	default:
		return fmt.Errorf("unsupported BGE-M3 graph optimization level")
	}
	if err := options.SetGraphOptimizationLevel(optimizationLevel); err != nil {
		return err
	}
	if err := options.SetMemPattern(false); err != nil {
		return err
	}
	if err := options.SetCpuMemArena(false); err != nil {
		return err
	}
	if cpuThreads <= 0 {
		cpuThreads = bgeCPUThreadBudget()
	}
	if err := options.SetIntraOpNumThreads(cpuThreads); err != nil {
		return err
	}
	return options.SetInterOpNumThreads(1)
}

func configureBGEExecutionProvider(options *ort.SessionOptions, modelPath, provider string) error {
	provider = strings.ToLower(strings.TrimSpace(provider))
	switch provider {
	case "", "cpu":
		return nil
	case "coreml":
		if runtime.GOOS != "darwin" {
			return fmt.Errorf("BGE-M3 CoreML execution provider requires macOS")
		}
		cacheDir := filepath.Join(filepath.Dir(filepath.Dir(modelPath)), "runtime", "coreml-cache")
		if err := os.MkdirAll(cacheDir, 0o700); err != nil {
			return fmt.Errorf("create BGE-M3 CoreML cache: %w", err)
		}
		return options.AppendExecutionProviderCoreMLV2(map[string]string{
			"ModelFormat":                        "MLProgram",
			"MLComputeUnits":                     "ALL",
			"RequireStaticInputShapes":           "0",
			"EnableOnSubgraphs":                  "0",
			"ModelCacheDirectory":                cacheDir,
			"AllowLowPrecisionAccumulationOnGPU": "0",
		})
	case "cuda":
		cudaOptions, err := ort.NewCUDAProviderOptions()
		if err != nil {
			return err
		}
		defer cudaOptions.Destroy()
		if err := cudaOptions.Update(map[string]string{
			"device_id":                 "0",
			"do_copy_in_default_stream": "1",
		}); err != nil {
			return err
		}
		return options.AppendExecutionProviderCUDA(cudaOptions)
	case "directml":
		if runtime.GOOS != "windows" {
			return fmt.Errorf("BGE-M3 DirectML execution provider requires Windows")
		}
		return options.AppendExecutionProviderDirectML(0)
	case "migraphx":
		if runtime.GOOS != "linux" {
			return fmt.Errorf("BGE-M3 MIGraphX execution provider requires Linux")
		}
		return options.AppendExecutionProvider("MIGraphX", map[string]string{"device_id": "0"})
	case "rocm":
		if runtime.GOOS != "linux" {
			return fmt.Errorf("BGE-M3 ROCm execution provider requires Linux")
		}
		return options.AppendExecutionProvider("ROCM", map[string]string{"device_id": "0"})
	default:
		return fmt.Errorf("unsupported BGE-M3 execution provider %q", provider)
	}
}

func bgeCPUThreadBudget() int {
	threads := minInt(runtime.NumCPU(), 16)
	if configured, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LUMINA_MEMORY_EMBEDDING_THREADS"))); err == nil && configured > 0 {
		threads = configured
	}
	return maxInt(1, threads)
}

func bgeSessionPoolSize() int {
	return bgeSessionPoolSizeForProvider("cpu")
}

func bgeSessionPoolSizeForProvider(provider string) int {
	totalThreads := bgeCPUThreadBudget()
	if configured, err := strconv.Atoi(strings.TrimSpace(os.Getenv("LUMINA_MEMORY_EMBEDDING_SESSIONS"))); err == nil && configured > 0 {
		configured = minInt(minInt(configured, totalThreads), 4)
		if normalizeBGEExecutionProvider(provider) != "cpu" {
			return 1
		}
		return configured
	}
	if normalizeBGEExecutionProvider(provider) != "cpu" {
		// Device execution providers schedule internally and duplicate model
		// weights per session. DirectML also disallows concurrent Run calls.
		return 1
	}
	if totalThreads >= 8 {
		return 2
	}
	return 1
}

func bgeSessionThreadBudget(sessionCount int) int {
	if sessionCount <= 1 {
		return bgeCPUThreadBudget()
	}
	return maxInt(1, bgeCPUThreadBudget()/sessionCount)
}

func bgeInferenceWorkCapacity(provider string, sessionCount int) int64 {
	if sessionCount <= 1 || normalizeBGEExecutionProvider(provider) != "cpu" {
		return 1 << 62
	}
	// Native activation memory follows batch*sequence^2 more closely than a
	// linear token count. This lets short CPU batches overlap while granting a
	// long sequence exclusive access to the shared inference budget.
	return 16 * 1024 * 1024
}

func intsToInt64(values []int) []int64 {
	result := make([]int64, len(values))
	for index, value := range values {
		result[index] = int64(value)
	}
	return result
}
