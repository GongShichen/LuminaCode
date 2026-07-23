//go:build cgo

package localmodel

import (
	"math"
	"runtime"
	"testing"
)

func TestBGESessionPoolSplitsTheGlobalThreadBudget(t *testing.T) {
	t.Setenv("LUMINA_MEMORY_EMBEDDING_THREADS", "10")
	t.Setenv("LUMINA_MEMORY_EMBEDDING_SESSIONS", "")
	if got := bgeSessionPoolSize(); got != 2 {
		t.Fatalf("default session pool = %d, want 2", got)
	}
	if got := bgeSessionThreadBudget(2); got != 5 {
		t.Fatalf("threads per session = %d, want 5", got)
	}

	t.Setenv("LUMINA_MEMORY_EMBEDDING_SESSIONS", "3")
	if got := bgeSessionPoolSize(); got != 3 {
		t.Fatalf("configured session pool = %d, want 3", got)
	}
	if got := bgeSessionThreadBudget(3); got != 3 {
		t.Fatalf("threads per configured session = %d, want 3", got)
	}
}

func TestBGESessionPoolStaysSingleOnSmallThreadBudgets(t *testing.T) {
	t.Setenv("LUMINA_MEMORY_EMBEDDING_THREADS", "4")
	t.Setenv("LUMINA_MEMORY_EMBEDDING_SESSIONS", "")
	if got := bgeSessionPoolSize(); got != 1 {
		t.Fatalf("small-machine session pool = %d, want 1", got)
	}

	t.Setenv("LUMINA_MEMORY_EMBEDDING_THREADS", "2")
	t.Setenv("LUMINA_MEMORY_EMBEDDING_SESSIONS", "8")
	if got := bgeSessionPoolSize(); got != 2 {
		t.Fatalf("bounded configured session pool = %d, want 2", got)
	}
}

func TestBGEAcceleratorUsesSingleSession(t *testing.T) {
	t.Setenv("LUMINA_MEMORY_EMBEDDING_THREADS", "16")
	t.Setenv("LUMINA_MEMORY_EMBEDDING_SESSIONS", "4")
	for _, provider := range []string{"coreml", "cuda", "directml", "migraphx", "rocm"} {
		if got := bgeSessionPoolSizeForProvider(provider); got != 1 {
			t.Fatalf("%s session pool = %d, want 1", provider, got)
		}
	}
}

func TestBGETokenizerWorkersFollowTheExecutionDevice(t *testing.T) {
	t.Setenv("LUMINA_MEMORY_TOKENIZER_WORKERS", "")
	acceleratorWant := runtime.GOMAXPROCS(0)
	if acceleratorWant > 8 {
		acceleratorWant = 8
	}
	if got := bgeTokenizerWorkerCount("metal"); got != acceleratorWant {
		t.Fatalf("Metal tokenizer workers = %d, want %d", got, acceleratorWant)
	}
	cpuWant := runtime.GOMAXPROCS(0)
	if cpuWant > 2 {
		cpuWant = 2
	}
	if got := bgeTokenizerWorkerCount("cpu"); got != cpuWant {
		t.Fatalf("CPU tokenizer workers = %d, want %d", got, cpuWant)
	}

	t.Setenv("LUMINA_MEMORY_TOKENIZER_WORKERS", "40")
	if got := bgeTokenizerWorkerCount("metal"); got != 32 {
		t.Fatalf("configured tokenizer workers = %d, want bounded 32", got)
	}
}

func TestBGEInferenceWorkUsesQuadraticSequenceCost(t *testing.T) {
	short := []bgeInput{{ids: make([]int64, 128)}, {ids: make([]int64, 64)}}
	if got, want := bgeInferenceWorkForBatch(short), int64(2*128*128); got != want {
		t.Fatalf("short inference work = %d, want %d", got, want)
	}
	long := []bgeInput{{ids: make([]int64, 4096)}}
	if got, want := bgeInferenceWorkForBatch(long), int64(4096*4096); got != want {
		t.Fatalf("long inference work = %d, want %d", got, want)
	}
}

func TestBGECPUInferenceWorkSerializesLongBatches(t *testing.T) {
	if got := bgeInferenceWorkCapacity("cpu", 2); got != 16*1024*1024 {
		t.Fatalf("CPU inference work capacity = %d", got)
	}
	if got := bgeInferenceWorkCapacity("cuda", 1); got < 1<<60 {
		t.Fatalf("accelerator inference work capacity = %d", got)
	}
}

func TestNormalizeBGEExecutionProviderUsesPortableNames(t *testing.T) {
	for input, want := range map[string]string{
		"": "cpu", "CPU": "cpu", "metal": "coreml", "nvidia": "cuda",
		"dml": "directml", "migraphx": "migraphx", "rocm": "rocm",
	} {
		if got := normalizeBGEExecutionProvider(input); got != want {
			t.Errorf("normalize provider %q = %q, want %q", input, got, want)
		}
	}
}

func TestProjectBGETokensUsesPyTorchLinearOrientation(t *testing.T) {
	hidden := [][]float32{make([]float32, bgeDimensions), make([]float32, bgeDimensions)}
	hidden[0][3] = 2
	hidden[1][7] = 4
	weight := make([]float32, bgeDimensions*bgeDimensions)
	weight[11*bgeDimensions+3] = 3
	weight[13*bgeDimensions+7] = 5
	bias := make([]float32, bgeDimensions)
	projected := projectBGETokens(hidden, weight, bias)
	if len(projected) != 2 {
		t.Fatalf("projected rows = %d", len(projected))
	}
	if math.Abs(float64(projected[0][11]-1)) > 1e-5 || math.Abs(float64(projected[1][13]-1)) > 1e-5 {
		t.Fatalf("unexpected projected values: row0[11]=%f row1[13]=%f", projected[0][11], projected[1][13])
	}
	if projected[0][3] != 0 || projected[1][7] != 0 {
		t.Fatalf("weight was treated as input-major rather than output-major")
	}
}

func TestBGEProbeMaxSimUsesEveryQueryTokenWithoutSparseWeighting(t *testing.T) {
	query := []BGETokenVector{
		{Weight: 100, Values: []float32{1, 0}},
		{Weight: 0, Values: []float32{0, 1}},
	}
	document := []BGETokenVector{
		{Values: []float32{1, 0}},
		{Values: []float32{0, .5}},
	}
	got := bgeProbeMaxSim(query, document)
	if math.Abs(got-.75) > 1e-6 {
		t.Fatalf("MaxSim = %.6f, want .75", got)
	}
}

func TestBGERevisionCompatibilityIsLimitedToPinnedINT8Variants(t *testing.T) {
	metal := &LocalBGEEncoder{revision: bgeMetalINT8Identity}
	if !metal.CompatibleRevision(bgeMetalINT8Identity) {
		t.Fatal("current Metal revision is not compatible with itself")
	}
	if !metal.CompatibleRevision(bgeCPUINT8Identity) {
		t.Fatal("pinned CPU INT8 revision is not compatible with pinned Metal INT8 revision")
	}
	for _, revision := range []string{
		BGEModelRevision,
		BGEModelRevision + ":metal-fp16@unknown",
		"other-model:cpu-int8@3fa3a927e7bc973ae751a8add34455b52d915ac0",
		"",
	} {
		if metal.CompatibleRevision(revision) {
			t.Fatalf("unexpected compatible revision %q", revision)
		}
	}
}

func TestPlanBGEInferenceBatchesBoundsPaddingWork(t *testing.T) {
	lengths := []int{8192, 12, 4000, 18, 4000, 256, 300, 20, 22, 24, 26, 28, 30, 32, 34, 36,
		38, 40, 42, 44, 46, 48, 50, 52, 54, 56, 58, 60, 62, 64, 66, 68, 70, 72, 74, 76}
	inputs := make([]bgeInput, len(lengths))
	for index, length := range lengths {
		inputs[index].ids = make([]int64, length)
	}
	batches := planBGEInferenceBatches(inputs)
	seen := make(map[int]bool, len(inputs))
	for _, batch := range batches {
		if len(batch) == 0 || len(batch) > bgeInferenceBatchLimit {
			t.Fatalf("invalid batch size: %d", len(batch))
		}
		maxTokens := 0
		for _, index := range batch {
			if seen[index] {
				t.Fatalf("input %d appeared twice", index)
			}
			seen[index] = true
			if len(inputs[index].ids) > maxTokens {
				maxTokens = len(inputs[index].ids)
			}
		}
		if maxTokens*len(batch) > bgeInferenceTokenBudget {
			t.Fatalf("batch padding work = %d tokens, budget %d", maxTokens*len(batch), bgeInferenceTokenBudget)
		}
	}
	if len(seen) != len(inputs) {
		t.Fatalf("planned inputs = %d, want %d", len(seen), len(inputs))
	}
}

func TestPlanBGEInferenceBatchesFillsShortDocumentBatch(t *testing.T) {
	inputs := make([]bgeInput, bgeInferenceBatchLimit)
	for index := range inputs {
		inputs[index].ids = make([]int64, 128)
	}
	batches := planBGEInferenceBatches(inputs)
	if len(batches) != 1 || len(batches[0]) != bgeInferenceBatchLimit {
		t.Fatalf("short document batches = %v, want one batch of %d", batches, bgeInferenceBatchLimit)
	}
}
