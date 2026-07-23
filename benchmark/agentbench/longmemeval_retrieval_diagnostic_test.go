package agentbench

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unicode"

	"LuminaCode/agent"
	"LuminaCode/config"
	"LuminaCode/memory"
)

// TestLongMemEvalPreparedRetrievalDiagnostic is opt-in so prepared indexes can
// be inspected without invoking the compiler, answer model, or evaluator.
func TestLongMemEvalPreparedRetrievalDiagnostic(t *testing.T) {
	caseIDs := strings.Fields(os.Getenv("LONGMEMEVAL_DIAGNOSTIC_CASES"))
	dataset := strings.TrimSpace(os.Getenv("LONGMEMEVAL_DIAGNOSTIC_DATASET"))
	indexDir := strings.TrimSpace(os.Getenv("LONGMEMEVAL_DIAGNOSTIC_INDEX"))
	if len(caseIDs) == 0 || dataset == "" || indexDir == "" {
		t.Skip("LongMemEval retrieval diagnostic is not configured")
	}
	_, _, cases, err := loadLongMemEvalDataset(RunnerOptions{CasesPath: dataset})
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]longMemEvalCase, len(cases))
	for _, item := range cases {
		byID[item.QuestionID] = item
	}
	concurrency, _ := strconv.Atoi(strings.TrimSpace(os.Getenv("LONGMEMEVAL_DIAGNOSTIC_CONCURRENCY")))
	if concurrency <= 0 {
		concurrency = 1
	}
	concurrency = minInt(concurrency, len(caseIDs))
	baseDir := t.TempDir()
	outputs := make([][]byte, len(caseIDs))
	errorsByIndex := make([]error, len(caseIDs))
	var outputFile *os.File
	var outputMu sync.Mutex
	if outputPath := strings.TrimSpace(os.Getenv("LONGMEMEVAL_DIAGNOSTIC_OUTPUT")); outputPath != "" {
		outputFile, err = os.OpenFile(outputPath, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatalf("open retrieval diagnostics: %v", err)
		}
		defer outputFile.Close()
	}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for index := range jobs {
				outputs[index], errorsByIndex[index] = runLongMemEvalPreparedRetrievalDiagnostic(
					context.Background(), baseDir, index, caseIDs[index], byID, indexDir)
				if errorsByIndex[index] == nil && outputFile != nil {
					outputMu.Lock()
					_, writeErr := outputFile.Write(append(outputs[index], '\n'))
					if writeErr == nil {
						writeErr = outputFile.Sync()
					}
					outputMu.Unlock()
					if writeErr != nil {
						errorsByIndex[index] = fmt.Errorf("write retrieval diagnostics: %w", writeErr)
					}
				}
			}
		}()
	}
	for index := range caseIDs {
		jobs <- index
	}
	close(jobs)
	workers.Wait()
	for index, caseID := range caseIDs {
		if errorsByIndex[index] != nil {
			t.Errorf("%s: %v", caseID, errorsByIndex[index])
			continue
		}
		t.Log(string(outputs[index]))
	}
}

func runLongMemEvalPreparedRetrievalDiagnostic(ctx context.Context, baseDir string, index int, caseID string,
	byID map[string]longMemEvalCase, indexDir string) ([]byte, error) {
	item, ok := byID[caseID]
	if !ok {
		return nil, fmt.Errorf("unknown case")
	}
	manifest, err := readLongMemEvalIndexManifest(longMemEvalIndexManifestPath(indexDir, caseID))
	if err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	caseDir := filepath.Join(baseDir, fmt.Sprintf("%03d-%s", index, caseID))
	fabricDir := longMemEvalFabricDir(caseDir)
	prepared := longMemEvalFabricDir(longMemEvalIndexCaseDir(indexDir, caseID))
	if err := cloneSQLiteStore(ctx, filepath.Join(prepared, "ledger.sqlite"), filepath.Join(fabricDir, "ledger.sqlite")); err != nil {
		return nil, err
	}
	if err := cloneSQLiteStore(ctx, filepath.Join(prepared, "index.sqlite"), filepath.Join(fabricDir, "index.sqlite")); err != nil {
		return nil, err
	}
	expectedEvents, _, expectedCount := buildLongMemEvalFabricHistory(item, manifest.Space)
	writeAudit, err := auditLongMemEvalPreparedLedger(ctx, filepath.Join(fabricDir, "ledger.sqlite"),
		expectedEvents, expectedCount, manifest)
	if err != nil {
		return nil, fmt.Errorf("write integrity: %w", err)
	}
	workingSidecar := filepath.Join(fabricDir, "retrieval-bge-m3.sqlite")
	options := RunnerOptions{LongMemEvalIndexDir: indexDir}
	// The prepared sidecar is the immutable diagnostic baseline. The completed
	// replay republishes the validated working copy for answer runs.
	for _, source := range []string{filepath.Join(prepared, "retrieval-bge-m3.sqlite"),
		longMemEvalRetrievalSidecarCache(options, caseID)} {
		if _, statErr := os.Stat(source); os.IsNotExist(statErr) {
			continue
		} else if statErr != nil {
			return nil, statErr
		}
		if err := cloneSQLiteStore(ctx, source, workingSidecar); err != nil {
			return nil, err
		}
		break
	}
	cfg := longMemEvalFabricConfig(config.GetConfig(), caseDir, fabricDir, manifest.ScopeRoot, true)
	fabric, err := agent.OpenConfiguredMemoryFabric(ctx, cfg, false)
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	result, searchErr := fabric.Search(ctx, memory.SearchRequest{
		Space: manifest.Space, Query: item.Question,
		ReferenceTime: longMemEvalRetrievalReferenceTime(parseLongMemEvalDate(item.QuestionDate)),
		MaxEvidence:   cfg.MemoryRecallMaxItems, MaxContextTokens: cfg.MemoryContextTargetTokens,
		IncludeDiagnostics: true,
	})
	closeErr := fabric.Close()
	if searchErr != nil {
		return nil, fmt.Errorf("search: %w", searchErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close: %w", closeErr)
	}
	if _, statErr := os.Stat(workingSidecar); statErr == nil {
		sidecarAudit, auditErr := auditLongMemEvalRetrievalSidecar(ctx, workingSidecar, expectedEvents)
		if auditErr != nil {
			return nil, fmt.Errorf("sidecar integrity: %w", auditErr)
		}
		writeAudit.SidecarEvents = sidecarAudit.Events
		writeAudit.SidecarSpans = sidecarAudit.Spans
		writeAudit.SidecarQuickCheck = sidecarAudit.QuickCheck
		writeAudit.SidecarSourceMapping = sidecarAudit.SourceMapping
		if _, err := storePathCheckpoint(ctx, workingSidecar); err != nil {
			return nil, err
		}
		if err := publishLongMemEvalRetrievalSidecar(ctx, workingSidecar,
			longMemEvalRetrievalSidecarCache(options, caseID)); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(statErr) {
		return nil, statErr
	}
	evidence := make([]map[string]any, 0, len(result.Evidence))
	for _, entry := range result.Evidence {
		content := []rune(entry.Content)
		if len(content) > 1_800 {
			content = content[:1_800]
		}
		evidence = append(evidence, map[string]any{"id": entry.ID, "kind": entry.ResourceKind,
			"context_id": entry.ContextID, "actor": entry.Actor, "source_event_ids": entry.SourceEventIDs,
			"content": string(content), "reasons": entry.MatchReasons, "score": entry.Score})
	}
	current := make([]string, 0, len(result.CurrentView))
	for _, node := range result.CurrentView {
		current = append(current, node.Statement)
	}
	encoded, _ := json.Marshal(map[string]any{
		"question_id": caseID, "question_type": item.QuestionType, "question": item.Question,
		"route":        result.Route,
		"insufficient": result.Insufficient, "diagnostics": result.Diagnostics,
		"current_view": current, "evidence": evidence, "write_integrity": writeAudit,
	})
	return encoded, nil
}

type longMemEvalWriteIntegrityAudit struct {
	InputMessages         int    `json:"input_messages"`
	LedgerRawEvents       int    `json:"ledger_raw_events"`
	LedgerQuickCheck      string `json:"ledger_quick_check"`
	EventIDsMatch         bool   `json:"event_ids_match"`
	ContentChecksumsMatch bool   `json:"content_checksums_match"`
	ContextMappingMatch   bool   `json:"context_mapping_match"`
	ActorMappingMatch     bool   `json:"actor_mapping_match"`
	TimeMappingMatch      bool   `json:"time_mapping_match"`
	TurnOrdinalMatch      bool   `json:"turn_ordinal_match"`
	RawContentPreserved   bool   `json:"raw_content_preserved"`
	SidecarEvents         int    `json:"sidecar_events"`
	SidecarSpans          int    `json:"sidecar_spans"`
	SidecarQuickCheck     string `json:"sidecar_quick_check,omitempty"`
	SidecarSourceMapping  bool   `json:"sidecar_source_mapping"`
}

type longMemEvalSidecarIntegrityAudit struct {
	Events        int
	Spans         int
	QuickCheck    string
	SourceMapping bool
}

func auditLongMemEvalPreparedLedger(ctx context.Context, path string, expected []memory.RawEvent,
	expectedCount int, manifest longMemEvalIndexManifest) (longMemEvalWriteIntegrityAudit, error) {
	audit := longMemEvalWriteIntegrityAudit{InputMessages: expectedCount, EventIDsMatch: true,
		ContentChecksumsMatch: true, ContextMappingMatch: true, ActorMappingMatch: true,
		TimeMappingMatch: true, TurnOrdinalMatch: true, RawContentPreserved: true}
	db, err := openLongMemEvalSQLite(path, true)
	if err != nil {
		return audit, err
	}
	defer db.Close()
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&audit.LedgerQuickCheck); err != nil {
		return audit, err
	}
	rows, err := db.QueryContext(ctx, `SELECT event_id,space,context_id,session_id,actor,source_kind,content,
		occurred_at,source_ref,checksum,metadata_json FROM events WHERE tombstoned=0`)
	if err != nil {
		return audit, err
	}
	type ledgerEvent struct {
		space, contextID, sessionID, actor, sourceKind, content, occurredAt, sourceRef, checksum, metadata string
	}
	actual := make(map[string]ledgerEvent, expectedCount)
	for rows.Next() {
		var id string
		var event ledgerEvent
		if err := rows.Scan(&id, &event.space, &event.contextID, &event.sessionID, &event.actor,
			&event.sourceKind, &event.content, &event.occurredAt, &event.sourceRef, &event.checksum, &event.metadata); err != nil {
			_ = rows.Close()
			return audit, err
		}
		actual[id] = event
	}
	if err := rows.Close(); err != nil {
		return audit, err
	}
	audit.LedgerRawEvents = len(actual)
	if len(actual) != expectedCount || manifest.Integrity.ExpectedEvents != expectedCount ||
		manifest.Integrity.ActualEvents != expectedCount {
		audit.EventIDsMatch = false
	}
	for _, event := range expected {
		stored, ok := actual[event.ID]
		if !ok {
			audit.EventIDsMatch = false
			continue
		}
		delete(actual, event.ID)
		metadataJSON, _ := json.Marshal(event.Metadata)
		expectedChecksum := longMemEvalRawEventChecksum(event, string(metadataJSON))
		if stored.checksum != expectedChecksum {
			audit.ContentChecksumsMatch = false
		}
		if stored.content != event.Content || stored.space != normalizeMemoryChecksumSpace(event.Space) || stored.sourceKind != event.SourceKind ||
			stored.sourceRef != event.SourceRef || stored.sessionID != event.SessionID {
			audit.RawContentPreserved = false
		}
		if stored.contextID != event.ContextID {
			audit.ContextMappingMatch = false
		}
		if stored.actor != event.Actor {
			audit.ActorMappingMatch = false
		}
		if stored.occurredAt != event.OccurredAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00") {
			audit.TimeMappingMatch = false
		}
		var storedMetadata map[string]string
		if json.Unmarshal([]byte(stored.metadata), &storedMetadata) != nil ||
			storedMetadata["turn_index"] != event.Metadata["turn_index"] ||
			storedMetadata["session_id"] != event.Metadata["session_id"] {
			audit.TurnOrdinalMatch = false
		}
	}
	if len(actual) != 0 {
		audit.EventIDsMatch = false
	}
	if audit.LedgerQuickCheck != "ok" || !audit.EventIDsMatch || !audit.ContentChecksumsMatch ||
		!audit.ContextMappingMatch || !audit.ActorMappingMatch || !audit.TimeMappingMatch ||
		!audit.TurnOrdinalMatch || !audit.RawContentPreserved {
		return audit, fmt.Errorf("raw event audit failed: %+v", audit)
	}
	return audit, nil
}

func longMemEvalRawEventChecksum(event memory.RawEvent, metadataJSON string) string {
	parts := []string{normalizeMemoryChecksumSpace(event.Space), event.ContextID, event.SessionID, event.Actor, event.SourceKind,
		event.SourceRef, event.OccurredAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), event.Content, metadataJSON}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return fmt.Sprintf("%x", sum[:])
}

// Event checksums canonicalize their space before hashing. Keep the diagnostic
// independent from unexported memory internals while matching that contract.
func normalizeMemoryChecksumSpace(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	lastSeparator := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("./@:+#", r) {
			out.WriteRune(r)
			lastSeparator = false
			continue
		}
		if !lastSeparator {
			out.WriteByte('-')
			lastSeparator = true
		}
	}
	if normalized := strings.Trim(out.String(), "-"); normalized != "" {
		return normalized
	}
	return "default"
}

func auditLongMemEvalRetrievalSidecar(ctx context.Context, path string,
	expected []memory.RawEvent) (longMemEvalSidecarIntegrityAudit, error) {
	audit := longMemEvalSidecarIntegrityAudit{SourceMapping: true}
	db, err := openLongMemEvalSQLite(path, true)
	if err != nil {
		return audit, err
	}
	defer db.Close()
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&audit.QuickCheck); err != nil {
		return audit, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_vectors`).Scan(&audit.Events); err != nil {
		return audit, err
	}
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM spans`).Scan(&audit.Spans); err != nil {
		return audit, err
	}
	for _, event := range expected {
		var sourceEventID, contextID, actor, occurredAt, content string
		var spanCount int
		err := db.QueryRowContext(ctx, `SELECT source_event_id,context_id,actor,occurred_at,content,
			(SELECT COUNT(*) FROM spans WHERE event_id=event_vectors.event_id) FROM event_vectors WHERE event_id=?`,
			event.ID).Scan(&sourceEventID, &contextID, &actor, &occurredAt, &content, &spanCount)
		if err != nil || sourceEventID != event.ID || contextID != event.ContextID || actor != event.Actor ||
			occurredAt != event.OccurredAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00") ||
			content != event.Content || spanCount == 0 {
			audit.SourceMapping = false
			break
		}
	}
	if audit.QuickCheck != "ok" || audit.Events != len(expected) || audit.Spans < len(expected) || !audit.SourceMapping {
		return audit, fmt.Errorf("event/span audit failed: %+v", audit)
	}
	return audit, nil
}
