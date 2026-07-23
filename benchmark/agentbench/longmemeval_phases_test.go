package agentbench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"LuminaCode/config"
	"LuminaCode/memory"
)

func TestLongMemEvalHistoryFingerprintExcludesQuestionAndGold(t *testing.T) {
	base := longMemEvalCase{QuestionID: "q1", Question: "first question", Answer: "secret",
		HaystackSessionIDs: []string{"s1"}, HaystackDates: []string{"2026-07-20"},
		HaystackSessions: [][]map[string]any{{{
			"role": "user", "content": "Remember blue.", "has_answer": true,
		}}}}
	changedGold := base
	changedGold.Question = "different question"
	changedGold.Answer = "different answer"
	changedGold.HaystackSessions = [][]map[string]any{{{
		"role": "user", "content": "Remember blue.", "has_answer": false,
	}}}
	if got, want := longMemEvalHistorySHA(changedGold), longMemEvalHistorySHA(base); got != want {
		t.Fatalf("history fingerprint included question/gold metadata: got %s want %s", got, want)
	}
	changedHistory := base
	changedHistory.HaystackSessions = [][]map[string]any{{{
		"role": "user", "content": "Remember green.", "has_answer": true,
	}}}
	if longMemEvalHistorySHA(changedHistory) == longMemEvalHistorySHA(base) {
		t.Fatal("history fingerprint ignored a content change")
	}
}

func TestLongMemEvalRequiresExplicitNonChainedPhase(t *testing.T) {
	if err := validateLongMemEvalPhaseOptions(RunnerOptions{}); err == nil {
		t.Fatal("LongMemEval accepted an implicit phase")
	}
	if err := validateLongMemEvalPhaseOptions(RunnerOptions{LongMemEvalPhase: LongMemEvalPhasePrepare,
		LongMemEvalPredictions: "predictions.jsonl"}); err == nil {
		t.Fatal("prepare phase accepted evaluator input")
	}
	if err := validateLongMemEvalPhaseOptions(RunnerOptions{LongMemEvalPhase: LongMemEvalPhaseAnswer}); err == nil {
		t.Fatal("answer phase accepted a missing run ID")
	}
	if err := validateLongMemEvalPhaseOptions(RunnerOptions{LongMemEvalPhase: LongMemEvalPhaseEvaluate,
		LongMemEvalPredictions: "predictions.jsonl", Limit: 1}); err == nil {
		t.Fatal("evaluate phase accepted a partial dataset selection")
	}
	if err := validateLongMemEvalPhaseOptions(RunnerOptions{LongMemEvalPhase: LongMemEvalPhaseAnswer,
		LongMemEvalRunID: "run", LongMemEvalQuestionType: "multi-session"}); err == nil {
		t.Fatal("question-type filter was accepted without smoke mode")
	}
}

func TestLongMemEvalQuestionTypeSmokeSelectionIsStable(t *testing.T) {
	var cases []longMemEvalCase
	for index := 0; index < 60; index++ {
		questionType := "multi-session"
		if index%3 == 0 {
			questionType = "temporal-reasoning"
		}
		cases = append(cases, longMemEvalCase{QuestionID: fmt.Sprintf("case-%03d", index),
			QuestionType: questionType})
	}
	options := RunnerOptions{LongMemEvalSmokeSize: 12, LongMemEvalQuestionType: "multi-session"}
	first := selectedLongMemEvalCases(cases, options)
	second := selectedLongMemEvalCases(cases, options)
	if len(first) != 12 || len(second) != 12 {
		t.Fatalf("selection sizes=%d/%d, want 12", len(first), len(second))
	}
	for index := range first {
		if first[index].QuestionType != "multi-session" || first[index].QuestionID != second[index].QuestionID {
			t.Fatalf("unstable or wrong-type selection at %d: %+v / %+v", index, first[index], second[index])
		}
	}
}

func TestRecoverRepairableLongMemEvalCompilerJobsIsNarrowAndOneShot(t *testing.T) {
	ledgerPath := filepath.Join(t.TempDir(), "ledger.sqlite")
	db, err := openLongMemEvalSQLite(ledgerPath, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE jobs (job_id TEXT PRIMARY KEY, job_kind TEXT NOT NULL, status TEXT NOT NULL,
			attempts INTEGER NOT NULL, available_at TEXT NOT NULL, lease_until TEXT NOT NULL,
			last_error TEXT NOT NULL, payload_json TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE events (event_id TEXT PRIMARY KEY, semantic_status TEXT NOT NULL)`,
		`INSERT INTO events VALUES ('event-empty','proposed')`,
		`INSERT INTO jobs VALUES ('repairable','compile_events','failed',1,'','','semantic compiler contract failed: decode structured memory field "nodes": invalid character '':'' after array element','{}','')`,
		`INSERT INTO jobs VALUES ('empty-output','compile_events','failed',1,'','','semantic compiler contract failed: model exhausted the output budget without a semantic result','{"event_ids":["event-empty"]}','')`,
		`INSERT INTO jobs VALUES ('quota','compile_events','failed',1,'','','quota exhausted','{}','')`,
	} {
		if _, err := db.Exec(statement); err != nil {
			_ = db.Close()
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	requeued, err := recoverRepairableLongMemEvalCompilerJobs(context.Background(), ledgerPath)
	if err != nil || requeued != 2 {
		t.Fatalf("first recovery = %d, %v; want 2, nil", requeued, err)
	}
	requeued, err = recoverRepairableLongMemEvalCompilerJobs(context.Background(), ledgerPath)
	if err != nil || requeued != 0 {
		t.Fatalf("second recovery = %d, %v; want 0, nil", requeued, err)
	}
	db, err = openLongMemEvalSQLite(ledgerPath, true)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var repairStatus, emptyStatus, quotaStatus, eventStatus string
	var repairAttempts, emptyAttempts int
	if err := db.QueryRow(`SELECT status, attempts FROM jobs WHERE job_id='repairable'`).Scan(&repairStatus, &repairAttempts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status FROM jobs WHERE job_id='quota'`).Scan(&quotaStatus); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT status, attempts FROM jobs WHERE job_id='empty-output'`).Scan(&emptyStatus, &emptyAttempts); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT semantic_status FROM events WHERE event_id='event-empty'`).Scan(&eventStatus); err != nil {
		t.Fatal(err)
	}
	if repairStatus != "pending" || repairAttempts != 0 || emptyStatus != "complete" || emptyAttempts != 1 ||
		quotaStatus != "failed" || eventStatus != string(memory.SemanticSkipped) {
		t.Fatalf("unexpected recovery states: repair=%s/%d empty=%s/%d event=%s quota=%s",
			repairStatus, repairAttempts, emptyStatus, emptyAttempts, eventStatus, quotaStatus)
	}
}

func TestStratifiedLongMemEvalSmokeIsBalancedAndStable(t *testing.T) {
	counts := map[string]int{
		"knowledge-update": 78, "multi-session": 133, "single-session-assistant": 56,
		"single-session-preference": 30, "single-session-user": 70, "temporal-reasoning": 133,
	}
	var cases []longMemEvalCase
	for questionType, count := range counts {
		for index := 0; index < count; index++ {
			cases = append(cases, longMemEvalCase{QuestionID: fmt.Sprintf("%s-%03d", questionType, index),
				QuestionType: questionType})
		}
	}
	first := stratifiedLongMemEvalCases(cases, 40)
	second := stratifiedLongMemEvalCases(cases, 40)
	if len(first) != 40 || len(second) != 40 {
		t.Fatalf("stratified sizes = %d/%d, want 40", len(first), len(second))
	}
	selectedCounts := map[string]int{}
	for index := range first {
		selectedCounts[first[index].QuestionType]++
		if first[index].QuestionID != second[index].QuestionID {
			t.Fatalf("selection changed at %d: %s != %s", index, first[index].QuestionID, second[index].QuestionID)
		}
	}
	want := map[string]int{"knowledge-update": 7, "multi-session": 7,
		"single-session-assistant": 6, "single-session-preference": 6,
		"single-session-user": 7, "temporal-reasoning": 7}
	for questionType, count := range want {
		if selectedCounts[questionType] != count {
			t.Fatalf("%s count=%d, want %d; all=%v", questionType, selectedCounts[questionType], count, selectedCounts)
		}
	}
}

func TestLongMemEvalSmokeGateRequiresOverallAndTypeQuality(t *testing.T) {
	types := []string{"knowledge-update", "multi-session", "single-session-assistant",
		"single-session-preference", "single-session-user", "temporal-reasoning"}
	var selected []longMemEvalCase
	lines := []string{"Accuracy: 0.8"}
	for _, questionType := range types {
		selected = append(selected, longMemEvalCase{QuestionID: questionType, QuestionType: questionType})
		lines = append(lines, fmt.Sprintf("\t%s: 0.8 (1)", questionType))
	}
	gate := assessLongMemEvalSmoke("completed", strings.Join(lines, "\n"), selected)
	if !gate.Passed || !gate.FullBenchmarkAllowed {
		t.Fatalf("passing smoke was rejected: %+v", gate)
	}
	lines[2] = "\tmulti-session: 0.4 (1)"
	gate = assessLongMemEvalSmoke("completed", strings.Join(lines, "\n"), selected)
	if gate.Passed || gate.FullBenchmarkAllowed {
		t.Fatalf("weak type score was accepted: %+v", gate)
	}
}

func TestLongMemEvalDatasetLoaderRejectsOracle(t *testing.T) {
	_, _, _, err := loadLongMemEvalDataset(RunnerOptions{CasesPath: filepath.Join(t.TempDir(), "longmemeval_oracle.json")})
	if err == nil || !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("oracle dataset was not rejected: %v", err)
	}
}

func TestLongMemEvalAnswerFingerprintIncludesRunAndRetrievalConfig(t *testing.T) {
	cfg := config.NewConfig()
	options := RunnerOptions{LongMemEvalRunID: "pilot-a", Config: cfg}
	first, err := longMemEvalAnswerFingerprint(options, "dataset", "index")
	if err != nil {
		t.Fatal(err)
	}
	options.LongMemEvalRunID = "pilot-b"
	second, err := longMemEvalAnswerFingerprint(options, "dataset", "index")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("answer fingerprint ignored run ID")
	}
	options.LongMemEvalRunID = "pilot-a"
	options.Config.MemorySearchLatencyMS++
	third, err := longMemEvalAnswerFingerprint(options, "dataset", "index")
	if err != nil {
		t.Fatal(err)
	}
	if first == third {
		t.Fatal("answer fingerprint ignored retrieval configuration")
	}
}

func TestLongMemEvalFallbackMustBeExplicitlyEnabled(t *testing.T) {
	cfg := config.NewConfig()
	cfg.FallbackAPIEnabled = false
	cfg.FallbackAPIKey = "configured-but-disabled"
	checked, status := checkLongMemEvalFallbackHealth(context.Background(), cfg)
	if checked.FallbackAPIEnabled || status != "disabled" {
		t.Fatalf("implicit fallback was accepted: enabled=%t status=%q", checked.FallbackAPIEnabled, status)
	}
}

func TestLongMemEvalScopeRelocationPreservesOriginalProjectScope(t *testing.T) {
	runtimeRoot := filepath.Join(t.TempDir(), "answer-copy")
	originalRoot := filepath.Join(t.TempDir(), "original-case")
	memoryPath := filepath.Join(runtimeRoot, "data", "memory", "fabric")
	cfg := memoryCaseConfigWithScope(config.NewConfig(), runtimeRoot, memoryPath, originalRoot)
	if got, want := filepath.Clean(cfg.ProjectPaths.CanonicalRoot), filepath.Clean(originalRoot); got != want {
		t.Fatalf("canonical root=%q, want original scope %q", got, want)
	}
}

func TestLongMemEvalFabricPrepareBuildsRawSnapshotWithoutSemanticAPI(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	t.Cleanup(func() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			if info.IsDir() {
				return os.Chmod(path, 0o755)
			}
			return os.Chmod(path, 0o600)
		})
	})
	cfg := config.NewConfig()
	cfg.MemoryBackend = "fabric"
	cfg.MemoryRemoteProcessing = "off"
	options := normalizeLongMemEvalOptions(RunnerOptions{WorkDir: filepath.Join(root, "work"),
		OutputDir: filepath.Join(root, "reports"), LongMemEvalIndexDir: filepath.Join(root, "index"), Config: cfg})
	caseData := longMemEvalCase{QuestionID: "q1", Question: "gold question must not be indexed", Answer: "gold answer",
		HaystackSessionIDs: []string{"s1"}, HaystackDates: []string{"2026/07/20 09:00"},
		HaystackSessions: [][]map[string]any{{
			{"role": "user", "content": "My preferred color is blue.", "has_answer": true},
			{"role": "assistant", "content": "I will remember that."},
		}}}
	manifest, err := prepareLongMemEvalIndexCase(ctx, options, "dataset", caseData)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Backend != "fabric" || manifest.Counts.Events != 2 || manifest.Counts.Documents != 3 ||
		manifest.EmbeddingModel != "bge-m3" || manifest.Counts.VectorDocuments != 1 || !manifest.Integrity.VectorReady {
		t.Fatalf("unexpected Fabric manifest: %#v", manifest)
	}
	if manifest.Integrity.PendingSemanticEvent != 0 || manifest.RemoteProcessing != "off" {
		t.Fatalf("remote-off raw snapshot lost its durable state: %#v", manifest.Integrity)
	}
	ledger, err := openLongMemEvalSQLite(longMemEvalIndexStorePath(options.LongMemEvalIndexDir, "q1"), true)
	if err != nil {
		t.Fatal(err)
	}
	defer ledger.Close()
	var rawOnly int
	if err := ledger.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE semantic_status='raw_only'`).Scan(&rawOnly); err != nil || rawOnly != 2 {
		t.Fatalf("remote-off events were not marked raw_only: count=%d err=%v", rawOnly, err)
	}
	rows, err := ledger.QueryContext(ctx, `SELECT content, metadata_json FROM events ORDER BY occurred_at`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var indexed strings.Builder
	for rows.Next() {
		var content, metadata string
		if err := rows.Scan(&content, &metadata); err != nil {
			t.Fatal(err)
		}
		indexed.WriteString(content)
		indexed.WriteString(metadata)
	}
	if strings.Contains(indexed.String(), caseData.Question) || strings.Contains(indexed.String(), fmt.Sprint(caseData.Answer)) ||
		strings.Contains(indexed.String(), "has_answer") {
		t.Fatalf("prepared Fabric snapshot leaked gold data: %s", indexed.String())
	}
}

func TestLongMemEvalReadyManifestAtomicWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "case", "index-manifest.json")
	manifest := longMemEvalIndexManifest{Format: longMemEvalPreparedIndexFormat, Status: "ready",
		QuestionID: "q1", ReadyAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if err := writeJSONAtomic(path, manifest, 0o444); err != nil {
		t.Fatal(err)
	}
	loaded, err := readLongMemEvalIndexManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != "ready" || loaded.QuestionID != "q1" {
		t.Fatalf("unexpected ready manifest: %#v", loaded)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("atomic manifest left a temporary file: %v", err)
	}
}

func TestLongMemEvalIndexManifestCannotSerializeGoldFields(t *testing.T) {
	data, err := json.Marshal(longMemEvalIndexManifest{Format: longMemEvalPreparedIndexFormat,
		Status: "ready", QuestionID: "q1"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, forbidden := range []string{"\"answer\"", "question_type", "has_answer"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("prepared index manifest leaked gold metadata %q: %s", forbidden, text)
		}
	}
}

func TestLongMemEvalManifestMismatchFailsBeforeOpeningStore(t *testing.T) {
	manifest := longMemEvalIndexManifest{Format: longMemEvalPreparedIndexFormat, Status: "ready",
		Backend: "fabric", QuestionID: "q1", DatasetSHA256: "wrong", HistorySHA256: "history",
		MemorySchema: longMemEvalMemorySchemaRevision, CompilerSchema: longMemEvalCompilerSchema,
		RetrievalPipeline: longMemEvalRetrievalPipeline}
	err := validateLongMemEvalIndexManifest(context.Background(), manifest, t.TempDir(), "expected",
		"history", longMemEvalCase{QuestionID: "q1"}, config.NewConfig())
	if err == nil || !strings.Contains(err.Error(), "fingerprint mismatch") {
		t.Fatalf("manifest mismatch did not fail fast: %v", err)
	}
}

func TestLongMemEvalPredictionValidationRequiresExactIDSet(t *testing.T) {
	results := []CaseResult{{Case: CaseSpec{ID: "q1"}, Hypothesis: "one"},
		{Case: CaseSpec{ID: "q3"}, Hypothesis: "three"}}
	err := validateLongMemEvalResultSet(results, map[string]struct{}{"q1": {}, "q2": {}})
	if err == nil || !strings.Contains(err.Error(), "missing=[q2]") || !strings.Contains(err.Error(), "unexpected=[q3]") {
		t.Fatalf("exact prediction set was not enforced: %v", err)
	}
}

type countingLongMemEvalAnswerRunner struct {
	calls int
	bad   string
}

func (runner *countingLongMemEvalAnswerRunner) RunAnswer(_ context.Context, cfg config.Config, _ string,
	_ string, _ time.Time) AgentRunResult {
	runner.calls++
	if cfg.MemoryBackend != "fabric" || cfg.MemoryRemoteProcessing != "off" {
		runner.bad = "answer did not use local-only Fabric"
	}
	return AgentRunResult{FinalText: "blue"}
}

func TestLongMemEvalAnswerUsesImmutableIndexAndOneAnswerCall(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	indexDir := filepath.Join(root, "index")
	preparedDir := longMemEvalFabricDir(longMemEvalIndexCaseDir(indexDir, "q1"))
	fabricOptions := memory.DefaultFabricOptions(preparedDir)
	fabricOptions.RemoteProcessing = memory.RemoteProcessingOff
	fabricOptions.StartWorkers = false
	fabric, err := memory.OpenFabric(ctx, fabricOptions)
	if err != nil {
		t.Fatal(err)
	}
	if err := fabric.Close(); err != nil {
		t.Fatal(err)
	}
	ledgerPath := filepath.Join(preparedDir, "ledger.sqlite")
	indexPath := filepath.Join(preparedDir, "index.sqlite")
	beforeLedger, err := fileSHA256(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeIndex, err := fileSHA256(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	runner := &countingLongMemEvalAnswerRunner{}
	cfg := config.NewConfig()
	cfg.MemoryEmbeddingEnabled = false
	options := RunnerOptions{WorkDir: filepath.Join(root, "work"), ArtifactsDir: filepath.Join(root, "artifacts"),
		LongMemEvalIndexDir: indexDir, LongMemEvalRunID: "smoke", Config: cfg}
	result := runLongMemEvalAnswerCase(ctx, longMemEvalCase{QuestionID: "q1", Question: "What color?", Answer: "blue"},
		longMemEvalIndexManifest{Backend: "fabric", QuestionID: "q1", ScopeRoot: filepath.Join(root, "original-scope")},
		strings.Repeat("a", 64), options, runner)
	if result.ErrorType != "" || result.Hypothesis != "blue" {
		t.Fatalf("answer result=%#v", result)
	}
	if runner.calls != 1 || runner.bad != "" {
		t.Fatalf("answer runner calls=%d bad=%q", runner.calls, runner.bad)
	}
	afterLedger, err := fileSHA256(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	afterIndex, err := fileSHA256(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if beforeLedger != afterLedger || beforeIndex != afterIndex {
		t.Fatal("prepared Fabric snapshot was modified by answer phase")
	}
}

func TestReusableLongMemEvalAnswerWorkDirRequiresCompleteSidecarCheckpoint(t *testing.T) {
	caseDir := t.TempDir()
	fabricDir := longMemEvalFabricDir(caseDir)
	if err := os.MkdirAll(fabricDir, 0o700); err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join(fabricDir, "ledger.sqlite"),
		filepath.Join(fabricDir, "index.sqlite"),
		filepath.Join(fabricDir, "retrieval-bge-m3.sqlite.staging"),
	}
	for index, path := range paths {
		if index > 0 && reusableLongMemEvalAnswerWorkDir(caseDir) {
			t.Fatalf("incomplete work directory was reusable after %d files", index)
		}
		if err := os.WriteFile(path, []byte("checkpoint"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if !reusableLongMemEvalAnswerWorkDir(caseDir) {
		t.Fatal("complete sidecar checkpoint was not reusable")
	}
}

func TestLongMemEvalCompatibleCodeIdentityRequiresBenchmarkPrefix(t *testing.T) {
	want := longMemEvalCodeBaseIdentity + ":compatible-test"
	t.Setenv("LUMINA_LONGMEMEVAL_COMPATIBLE_CODE_IDENTITY", want)
	if got := longMemEvalCodeIdentity(); got != want {
		t.Fatalf("compatible code identity = %q, want %q", got, want)
	}
	t.Setenv("LUMINA_LONGMEMEVAL_COMPATIBLE_CODE_IDENTITY", "untrusted")
	if got := longMemEvalCodeIdentity(); got == "untrusted" {
		t.Fatal("unprefixed compatible code identity was accepted")
	}
}
