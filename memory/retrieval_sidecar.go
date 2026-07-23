package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const retrievalSidecarSchema = "bge-m3-event-span-v4"
const retrievalSidecarEncodeBatch = 64
const retrievalSidecarEventWindowTokens = 512
const retrievalSidecarEventWindowOverlap = 64

type sidecarEvent struct {
	id, space, contextID, actor, content, occurredAt, checksum string
}

func (f *Fabric) migrateRetrievalSidecar(ctx context.Context) error {
	if f == nil || f.sidecar == nil {
		return nil
	}
	if _, err := f.sidecar.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		return fmt.Errorf("create BGE retrieval metadata: %w", err)
	}
	var existingSchema string
	_ = f.sidecar.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&existingSchema)
	if existingSchema != "" && existingSchema != retrievalSidecarSchema {
		// Keep the last published database untouched. Sync builds the new schema
		// in a separate database and only swaps it in after all checks pass.
		return nil
	}
	statements := []string{
		`PRAGMA journal_mode=WAL;`,
		`CREATE TABLE IF NOT EXISTS event_state (
			event_id TEXT PRIMARY KEY, checksum TEXT NOT NULL, updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS event_vectors (
			event_id TEXT PRIMARY KEY, source_event_id TEXT NOT NULL, space TEXT NOT NULL,
			context_id TEXT NOT NULL DEFAULT '', actor TEXT NOT NULL DEFAULT '', occurred_at TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL, token_count INTEGER NOT NULL, window_count INTEGER NOT NULL,
			dense_fp16 BLOB NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_event_vectors_space ON event_vectors(space, occurred_at);`,
		`CREATE TABLE IF NOT EXISTS spans (
			span_id TEXT PRIMARY KEY, event_id TEXT NOT NULL, source_event_id TEXT NOT NULL,
			space TEXT NOT NULL, context_id TEXT NOT NULL DEFAULT '', actor TEXT NOT NULL DEFAULT '',
			occurred_at TEXT NOT NULL DEFAULT '', content TEXT NOT NULL, token_count INTEGER NOT NULL,
			span_index INTEGER NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_spans_space ON spans(space, occurred_at);`,
		`CREATE INDEX IF NOT EXISTS idx_spans_event ON spans(event_id);`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS span_fts USING fts5(span_id UNINDEXED, space UNINDEXED, content);`,
		`CREATE TABLE IF NOT EXISTS event_sparse_postings (
			token_id INTEGER NOT NULL, event_id TEXT NOT NULL, weight REAL NOT NULL,
			PRIMARY KEY(token_id, event_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_event_sparse_event ON event_sparse_postings(event_id);`,
		`CREATE TABLE IF NOT EXISTS graph_edges (
			source_id TEXT NOT NULL, target_id TEXT NOT NULL, weight REAL NOT NULL,
			PRIMARY KEY(source_id, target_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_graph_target ON graph_edges(target_id);`,
	}
	for _, statement := range statements {
		if _, err := f.sidecar.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate BGE retrieval sidecar: %w", err)
		}
	}
	return nil
}

// SyncRetrievalSidecar materializes event spans from the durable ledger. A
// sidecar under construction is never eligible for Search.
func (f *Fabric) SyncRetrievalSidecar(ctx context.Context) error {
	if f == nil || f.sidecar == nil || f.options.RetrievalEncoder == nil {
		return errors.New("BGE retrieval sidecar is not configured")
	}
	f.sidecarSyncMu.Lock()
	defer f.sidecarSyncMu.Unlock()
	events, ledgerChecksum, err := f.loadSidecarEvents(ctx)
	if err != nil {
		return err
	}
	if ready, _ := f.retrievalSidecarReady(ctx, ledgerChecksum); ready {
		return nil
	}
	return f.rebuildRetrievalSidecarAtomically(ctx, events, ledgerChecksum)
}

func (f *Fabric) rebuildRetrievalSidecarAtomically(ctx context.Context, events []sidecarEvent,
	ledgerChecksum string) error {
	finalPath := filepath.Clean(f.options.RetrievalSidecarPath)
	stagingPath := finalPath + ".staging"
	staging, err := openFabricDB(stagingPath, false)
	if err != nil {
		return fmt.Errorf("open staging BGE sidecar: %w", err)
	}
	builder := &Fabric{options: f.options, ledger: f.ledger, sidecar: staging}
	closeStaging := func() { _ = staging.Close() }
	if err := builder.migrateRetrievalSidecar(ctx); err != nil {
		closeStaging()
		return err
	}
	var stageSchema, stageRevision, stageTokenizer string
	_ = staging.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&stageSchema)
	_ = staging.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='model_revision'`).Scan(&stageRevision)
	_ = staging.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='tokenizer_hash'`).Scan(&stageTokenizer)
	if (stageSchema != "" && stageSchema != retrievalSidecarSchema) ||
		(stageRevision != "" && stageRevision != f.options.RetrievalEncoder.Revision()) ||
		(stageTokenizer != "" && stageTokenizer != f.options.RetrievalEncoder.TokenizerHash()) {
		closeStaging()
		removeSQLiteArtifacts(stagingPath)
		staging, err = openFabricDB(stagingPath, false)
		if err != nil {
			return fmt.Errorf("replace incompatible staging BGE sidecar: %w", err)
		}
		builder.sidecar = staging
		closeStaging = func() { _ = staging.Close() }
		if err := builder.migrateRetrievalSidecar(ctx); err != nil {
			closeStaging()
			return err
		}
	}
	for key, value := range map[string]string{
		"schema_version": retrievalSidecarSchema,
		"model":          f.options.RetrievalEncoder.Model(),
		"model_revision": f.options.RetrievalEncoder.Revision(),
		"tokenizer_hash": f.options.RetrievalEncoder.TokenizerHash(),
		"build_state":    "building",
	} {
		if err := setSidecarMeta(ctx, staging, key, value); err != nil {
			closeStaging()
			return err
		}
	}
	active := make(map[string]struct{}, len(events))
	pending := make([]sidecarEvent, 0, len(events))
	for _, event := range events {
		active[event.id] = struct{}{}
		var current string
		stateErr := staging.QueryRowContext(ctx, `SELECT checksum FROM event_state WHERE event_id=?`, event.id).Scan(&current)
		if stateErr == nil && current == event.checksum {
			continue
		}
		if stateErr != nil && stateErr != sql.ErrNoRows {
			closeStaging()
			return stateErr
		}
		pending = append(pending, event)
	}
	if err := builder.indexSidecarEvents(ctx, pending); err != nil {
		closeStaging()
		return err
	}
	if err := builder.pruneStagingSidecarEvents(ctx, active); err != nil {
		closeStaging()
		return err
	}
	if err := builder.rebuildSidecarGraph(ctx); err != nil {
		closeStaging()
		return err
	}
	currentEvents, currentChecksum, err := f.loadSidecarEvents(ctx)
	if err != nil {
		closeStaging()
		return err
	}
	if currentChecksum != ledgerChecksum || len(currentEvents) != len(events) {
		closeStaging()
		return errors.New("memory ledger changed while building the BGE sidecar")
	}
	for key, value := range map[string]string{
		"schema_version":  retrievalSidecarSchema,
		"ledger_checksum": ledgerChecksum,
		"model":           f.options.RetrievalEncoder.Model(),
		"model_revision":  f.options.RetrievalEncoder.Revision(),
		"tokenizer_hash":  f.options.RetrievalEncoder.TokenizerHash(),
		"event_count":     fmt.Sprintf("%d", len(events)),
		"build_state":     "ready",
	} {
		if err := setSidecarMeta(ctx, staging, key, value); err != nil {
			closeStaging()
			return err
		}
	}
	var indexedEvents, indexedStates int
	if err := staging.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_vectors`).Scan(&indexedEvents); err != nil {
		closeStaging()
		return err
	}
	if err := staging.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_state`).Scan(&indexedStates); err != nil {
		closeStaging()
		return err
	}
	if indexedEvents != len(events) || indexedStates != len(events) {
		closeStaging()
		return fmt.Errorf("BGE staging event mismatch: vectors=%d state=%d ledger=%d",
			indexedEvents, indexedStates, len(events))
	}
	var quickCheck string
	if err := staging.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&quickCheck); err != nil || quickCheck != "ok" {
		closeStaging()
		if err != nil {
			return fmt.Errorf("check staging BGE sidecar: %w", err)
		}
		return fmt.Errorf("check staging BGE sidecar: %s", quickCheck)
	}
	_, _ = staging.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	if err := staging.Close(); err != nil {
		return fmt.Errorf("close staging BGE sidecar: %w", err)
	}
	if err := os.Chmod(stagingPath, 0o600); err != nil {
		return fmt.Errorf("secure staging BGE sidecar: %w", err)
	}
	if err := f.publishRetrievalSidecar(stagingPath); err != nil {
		return err
	}
	return nil
}

func (f *Fabric) publishRetrievalSidecar(stagingPath string) error {
	finalPath := filepath.Clean(f.options.RetrievalSidecarPath)
	backupPath := finalPath + ".previous"
	f.sidecarMu.Lock()
	defer f.sidecarMu.Unlock()
	if f.sidecar != nil {
		_, _ = f.sidecar.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		if err := f.sidecar.Close(); err != nil {
			return fmt.Errorf("close published BGE sidecar: %w", err)
		}
		f.sidecar = nil
	}
	removeSQLiteArtifacts(backupPath)
	if _, err := os.Stat(finalPath); err == nil {
		if err := os.Rename(finalPath, backupPath); err != nil {
			f.sidecar, _ = openFabricDB(finalPath, false)
			return fmt.Errorf("preserve published BGE sidecar: %w", err)
		}
	} else if !os.IsNotExist(err) {
		f.sidecar, _ = openFabricDB(finalPath, false)
		return err
	}
	removeSQLiteArtifacts(finalPath)
	if err := os.Rename(stagingPath, finalPath); err != nil {
		_ = os.Rename(backupPath, finalPath)
		f.sidecar, _ = openFabricDB(finalPath, false)
		return fmt.Errorf("publish BGE sidecar: %w", err)
	}
	published, err := openFabricDB(finalPath, false)
	if err == nil {
		var state string
		err = published.QueryRow(`SELECT value FROM meta WHERE key='build_state'`).Scan(&state)
		if err == nil && state != "ready" {
			err = fmt.Errorf("published BGE sidecar state is %q", state)
		}
	}
	if err != nil {
		if published != nil {
			_ = published.Close()
		}
		removeSQLiteArtifacts(finalPath)
		_ = os.Rename(backupPath, finalPath)
		f.sidecar, _ = openFabricDB(finalPath, false)
		return fmt.Errorf("open published BGE sidecar: %w", err)
	}
	f.sidecar = published
	_ = os.Chmod(finalPath, 0o600)
	removeSQLiteArtifacts(backupPath)
	return nil
}

func removeSQLiteArtifacts(path string) {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		_ = os.Remove(candidate)
	}
}

func (f *Fabric) syncRetrievalSidecarEvents(ctx context.Context, eventIDs []string) error {
	if f == nil || f.sidecar == nil || f.options.RetrievalEncoder == nil || len(eventIDs) == 0 {
		return nil
	}
	f.sidecarSyncMu.Lock()
	defer f.sidecarSyncMu.Unlock()
	f.sidecarMu.RLock()
	defer f.sidecarMu.RUnlock()
	var schema, revision, tokenizerHash string
	_ = f.sidecar.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&schema)
	_ = f.sidecar.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='model_revision'`).Scan(&revision)
	_ = f.sidecar.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='tokenizer_hash'`).Scan(&tokenizerHash)
	if schema != retrievalSidecarSchema || !retrievalEncoderRevisionCompatible(f.options.RetrievalEncoder, revision) ||
		tokenizerHash != f.options.RetrievalEncoder.TokenizerHash() {
		// Search will build a complete staging replacement. Never mutate an
		// incompatible published database during ingestion.
		return nil
	}
	eventIDs = uniqueStrings(eventIDs)
	args := make([]any, len(eventIDs))
	for index, id := range eventIDs {
		args[index] = id
	}
	rows, err := f.ledger.QueryContext(ctx, `SELECT event_id,space,context_id,actor,content,occurred_at,checksum
		FROM events WHERE tombstoned=0 AND event_id IN (`+placeholders(len(args))+`) ORDER BY event_id`, args...)
	if err != nil {
		return err
	}
	var pending []sidecarEvent
	for rows.Next() {
		var event sidecarEvent
		if err := rows.Scan(&event.id, &event.space, &event.contextID, &event.actor, &event.content,
			&event.occurredAt, &event.checksum); err != nil {
			_ = rows.Close()
			return err
		}
		var current string
		stateErr := f.sidecar.QueryRowContext(ctx, `SELECT checksum FROM event_state WHERE event_id=?`, event.id).Scan(&current)
		if stateErr == nil && current == event.checksum {
			continue
		}
		if stateErr != nil && stateErr != sql.ErrNoRows {
			_ = rows.Close()
			return stateErr
		}
		pending = append(pending, event)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(pending) == 0 {
		return nil
	}
	if err := f.indexSidecarEvents(ctx, pending); err != nil {
		return err
	}
	if err := f.rebuildSidecarGraph(ctx); err != nil {
		return err
	}
	allEvents, ledgerChecksum, err := f.loadSidecarEvents(ctx)
	if err != nil {
		return err
	}
	var indexed int
	if err := f.sidecar.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_state`).Scan(&indexed); err != nil {
		return err
	}
	if indexed != len(allEvents) {
		return fmt.Errorf("incremental BGE sidecar event mismatch: indexed=%d ledger=%d", indexed, len(allEvents))
	}
	for key, value := range map[string]string{
		"ledger_checksum": ledgerChecksum,
		"event_count":     fmt.Sprintf("%d", len(allEvents)),
		"build_state":     "ready",
	} {
		if err := setSidecarMeta(ctx, f.sidecar, key, value); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fabric) loadSidecarEvents(ctx context.Context) ([]sidecarEvent, string, error) {
	rows, err := f.ledger.QueryContext(ctx, `SELECT event_id, space, context_id, actor, content, occurred_at, checksum
		FROM events WHERE tombstoned=0 ORDER BY event_id`)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	hash := sha256.New()
	var events []sidecarEvent
	for rows.Next() {
		var event sidecarEvent
		if err := rows.Scan(&event.id, &event.space, &event.contextID, &event.actor, &event.content,
			&event.occurredAt, &event.checksum); err != nil {
			return nil, "", err
		}
		_, _ = hash.Write([]byte(event.id))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(event.checksum))
		_, _ = hash.Write([]byte{0})
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	return events, hex.EncodeToString(hash.Sum(nil)), nil
}

func (f *Fabric) retrievalSidecarReady(ctx context.Context, ledgerChecksum string) (bool, string) {
	if f == nil || f.sidecar == nil || f.options.RetrievalEncoder == nil {
		return false, "not_configured"
	}
	wanted := map[string]string{
		"build_state": "ready", "schema_version": retrievalSidecarSchema,
		"ledger_checksum": ledgerChecksum, "model_revision": f.options.RetrievalEncoder.Revision(),
		"tokenizer_hash": f.options.RetrievalEncoder.TokenizerHash(),
	}
	for key, expected := range wanted {
		var actual string
		if err := f.sidecar.QueryRowContext(ctx, `SELECT value FROM meta WHERE key=?`, key).Scan(&actual); err != nil {
			return false, key + "_missing"
		}
		if key == "model_revision" &&
			retrievalEncoderRevisionCompatible(f.options.RetrievalEncoder, actual) {
			continue
		}
		if actual != expected {
			return false, key + "_mismatch"
		}
	}
	return true, ""
}

func retrievalEncoderRevisionCompatible(encoder RetrievalEncoder, existing string) bool {
	if encoder == nil || strings.TrimSpace(existing) == "" {
		return false
	}
	if existing == encoder.Revision() {
		return true
	}
	compatible, ok := encoder.(interface {
		CompatibleRevision(string) bool
	})
	return ok && compatible.CompatibleRevision(existing)
}

type sidecarEventBatch struct {
	event     sidecarEvent
	spans     []string
	windows   []string
	encodings []RetrievalEncoding
}

func (f *Fabric) indexSidecarEvents(ctx context.Context, events []sidecarEvent) error {
	events = append([]sidecarEvent(nil), events...)
	sort.SliceStable(events, func(left, right int) bool {
		if len(events[left].content) != len(events[right].content) {
			return len(events[left].content) < len(events[right].content)
		}
		return events[left].id < events[right].id
	})
	for batchStart := 0; batchStart < len(events); batchStart += retrievalSidecarEncodeBatch {
		batchEnd := minIntMemory(len(events), batchStart+retrievalSidecarEncodeBatch)
		prepared := make([]sidecarEventBatch, batchEnd-batchStart)
		for index, event := range events[batchStart:batchEnd] {
			splits, err := splitRetrievalEvent(f.options.RetrievalEncoder, event.content)
			if err != nil {
				return fmt.Errorf("split BGE event %s: %w", event.id, err)
			}
			spans, windows := splits[0], splits[1]
			if len(spans) == 0 || len(windows) == 0 {
				return fmt.Errorf("BGE event %s produced no indexable text", event.id)
			}
			prepared[index] = sidecarEventBatch{event: event, spans: spans, windows: windows}
		}
		var channelTexts []string
		var channelOwners []int
		for index := range prepared {
			item := &prepared[index]
			for _, window := range item.windows {
				channelTexts = append(channelTexts, window)
				channelOwners = append(channelOwners, index)
			}
		}
		encodings, err := f.encodeSidecarChannels(ctx, channelTexts)
		if err != nil {
			return err
		}
		for index, encoding := range encodings {
			owner := channelOwners[index]
			prepared[owner].encodings = append(prepared[owner].encodings, encoding)
		}
		for _, item := range prepared {
			encoding, err := aggregateSidecarEventEncoding(item.encodings)
			if err != nil {
				return fmt.Errorf("aggregate BGE event %s: %w", item.event.id, err)
			}
			if err := f.storeSidecarEvent(ctx, item.event, item.spans, len(item.windows), encoding); err != nil {
				return fmt.Errorf("index BGE event %s: %w", item.event.id, err)
			}
		}
	}
	return nil
}

func splitRetrievalEvent(encoder RetrievalEncoder, text string) ([][]string, error) {
	specs := []RetrievalSplitSpec{
		{MaxTokens: 192, Overlap: 32},
		{MaxTokens: retrievalSidecarEventWindowTokens, Overlap: retrievalSidecarEventWindowOverlap},
	}
	if multi, ok := encoder.(RetrievalMultiSplitter); ok {
		result, err := multi.SplitMany(text, specs)
		if err != nil {
			return nil, err
		}
		if len(result) != len(specs) {
			return nil, fmt.Errorf("multi-split result count %d, want %d", len(result), len(specs))
		}
		return result, nil
	}
	result := make([][]string, len(specs))
	for index, spec := range specs {
		split, err := encoder.Split(text, spec.MaxTokens, spec.Overlap)
		if err != nil {
			return nil, err
		}
		result[index] = split
	}
	return result, nil
}

func (f *Fabric) pruneStagingSidecarEvents(ctx context.Context, active map[string]struct{}) error {
	rows, err := f.sidecar.QueryContext(ctx, `SELECT event_id FROM event_state`)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var eventID string
		if err := rows.Scan(&eventID); err != nil {
			_ = rows.Close()
			return err
		}
		if _, ok := active[eventID]; !ok {
			stale = append(stale, eventID)
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, eventID := range stale {
		tx, err := f.sidecar.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		spanRows, err := tx.QueryContext(ctx, `SELECT span_id FROM spans WHERE event_id=?`, eventID)
		if err != nil {
			_ = tx.Rollback()
			return err
		}
		var spanIDs []string
		for spanRows.Next() {
			var spanID string
			if err := spanRows.Scan(&spanID); err != nil {
				_ = spanRows.Close()
				_ = tx.Rollback()
				return err
			}
			spanIDs = append(spanIDs, spanID)
		}
		_ = spanRows.Close()
		for _, spanID := range spanIDs {
			if _, err := tx.ExecContext(ctx, `DELETE FROM span_fts WHERE span_id=?`, spanID); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		for _, statement := range []string{
			`DELETE FROM event_sparse_postings WHERE event_id=?`,
			`DELETE FROM spans WHERE event_id=?`,
			`DELETE FROM event_vectors WHERE event_id=?`,
			`DELETE FROM event_state WHERE event_id=?`,
		} {
			if _, err := tx.ExecContext(ctx, statement, eventID); err != nil {
				_ = tx.Rollback()
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fabric) encodeSidecarChannels(ctx context.Context, texts []string) ([]RetrievalEncoding, error) {
	result := make([]RetrievalEncoding, 0, len(texts))
	encoder, channelsOnly := f.options.RetrievalEncoder.(RetrievalChannelEncoder)
	for start := 0; start < len(texts); start += retrievalSidecarEncodeBatch {
		end := minIntMemory(len(texts), start+retrievalSidecarEncodeBatch)
		var batch []RetrievalEncoding
		var err error
		if channelsOnly {
			batch, err = encoder.EncodeChannels(ctx, texts[start:end], RetrievalDocument)
		} else {
			batch, err = f.options.RetrievalEncoder.Encode(ctx, texts[start:end], RetrievalDocument)
		}
		if err != nil {
			return nil, fmt.Errorf("encode BGE event channel batch: %w", err)
		}
		if len(batch) != end-start {
			return nil, fmt.Errorf("BGE encoding count %d, want %d", len(batch), end-start)
		}
		result = append(result, batch...)
	}
	return result, nil
}

func aggregateSidecarEventEncoding(encodings []RetrievalEncoding) (RetrievalEncoding, error) {
	if len(encodings) == 0 {
		return RetrievalEncoding{}, errors.New("event has no channel encodings")
	}
	dimensions := len(encodings[0].Dense)
	if dimensions == 0 {
		return RetrievalEncoding{}, errors.New("event has no dense encoding")
	}
	result := RetrievalEncoding{Dense: make([]float32, dimensions), Sparse: map[int64]float32{}}
	for _, encoding := range encodings {
		if len(encoding.Dense) != dimensions {
			return RetrievalEncoding{}, errors.New("event dense dimensions are inconsistent")
		}
		for dimension, value := range encoding.Dense {
			result.Dense[dimension] += value
		}
		for tokenID, weight := range encoding.Sparse {
			if weight > result.Sparse[tokenID] {
				result.Sparse[tokenID] = weight
			}
		}
	}
	var norm float64
	for dimension := range result.Dense {
		result.Dense[dimension] /= float32(len(encodings))
		norm += float64(result.Dense[dimension] * result.Dense[dimension])
	}
	if norm = math.Sqrt(norm); norm > 0 {
		for dimension := range result.Dense {
			result.Dense[dimension] /= float32(norm)
		}
	}
	return result, nil
}

func (f *Fabric) storeSidecarEvent(ctx context.Context, event sidecarEvent, spans []string,
	windowCount int, encoding RetrievalEncoding) error {
	if len(encoding.Dense) == 0 || len(spans) == 0 {
		return errors.New("incomplete BGE event representation")
	}
	tx, err := f.sidecar.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	oldRows, err := tx.QueryContext(ctx, `SELECT span_id FROM spans WHERE event_id=?`, event.id)
	if err != nil {
		return err
	}
	var oldSpanIDs []string
	for oldRows.Next() {
		var id string
		if err := oldRows.Scan(&id); err != nil {
			_ = oldRows.Close()
			return err
		}
		oldSpanIDs = append(oldSpanIDs, id)
	}
	_ = oldRows.Close()
	for _, id := range oldSpanIDs {
		if _, err := tx.ExecContext(ctx, `DELETE FROM span_fts WHERE span_id=?`, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM event_sparse_postings WHERE event_id=?`, event.id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM spans WHERE event_id=?`, event.id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM event_vectors WHERE event_id=?`, event.id); err != nil {
		return err
	}
	dense, err := encodeFP16Vector(encoding.Dense)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO event_vectors(event_id,source_event_id,space,context_id,actor,
		occurred_at,content,token_count,window_count,dense_fp16) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		event.id, event.id, normalizeSpace(event.space), event.contextID, event.actor, event.occurredAt,
		event.content, maxIntMemory(1, estimateTokens(event.content)), maxIntMemory(1, windowCount), dense); err != nil {
		return err
	}
	for index, content := range spans {
		spanID := fmt.Sprintf("%s:%03d", event.id, index)
		tokenCount := maxIntMemory(1, estimateTokens(content))
		if _, err := tx.ExecContext(ctx, `INSERT INTO spans(span_id,event_id,source_event_id,space,context_id,actor,
			occurred_at,content,token_count,span_index) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			spanID, event.id, event.id, normalizeSpace(event.space), event.contextID, event.actor,
			event.occurredAt, content, tokenCount, index); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO span_fts(span_id,space,content) VALUES(?,?,?)`,
			spanID, normalizeSpace(event.space), content); err != nil {
			return err
		}
	}
	for tokenID, weight := range encoding.Sparse {
		if weight <= 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO event_sparse_postings(token_id,event_id,weight) VALUES(?,?,?)`,
			tokenID, event.id, weight); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO event_state(event_id,checksum,updated_at) VALUES(?,?,?)
		ON CONFLICT(event_id) DO UPDATE SET checksum=excluded.checksum,updated_at=excluded.updated_at`,
		event.id, event.checksum, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (f *Fabric) rebuildSidecarGraph(ctx context.Context) error {
	tx, err := f.sidecar.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM graph_edges`); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT event_id, context_id FROM event_vectors`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var eventID, contextID string
		if err := rows.Scan(&eventID, &contextID); err != nil {
			_ = rows.Close()
			return err
		}
		if strings.TrimSpace(contextID) == "" {
			continue
		}
		if err := insertGraphPair(ctx, tx, "event:"+eventID, "context:"+contextID, 1); err != nil {
			_ = rows.Close()
			return err
		}
	}
	_ = rows.Close()
	semanticRows, err := f.ledger.QueryContext(ctx, `SELECT node_id,event_id FROM node_sources`)
	if err != nil {
		return err
	}
	for semanticRows.Next() {
		var nodeID, eventID string
		if err := semanticRows.Scan(&nodeID, &eventID); err != nil {
			_ = semanticRows.Close()
			return err
		}
		if err := insertGraphPair(ctx, tx, "event:"+eventID, "semantic:"+nodeID, .8); err != nil {
			_ = semanticRows.Close()
			return err
		}
	}
	_ = semanticRows.Close()
	var eventCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_vectors`).Scan(&eventCount); err != nil {
		return err
	}
	dfLimit := maxIntMemory(2, eventCount/20)
	conceptRows, err := tx.QueryContext(ctx, `SELECT token_id,COUNT(DISTINCT event_id) AS df
		FROM event_sparse_postings GROUP BY token_id HAVING df<=?`, dfLimit)
	if err != nil {
		return err
	}
	type concept struct {
		tokenID int64
		df      int
	}
	var concepts []concept
	for conceptRows.Next() {
		var item concept
		if err := conceptRows.Scan(&item.tokenID, &item.df); err != nil {
			_ = conceptRows.Close()
			return err
		}
		concepts = append(concepts, item)
	}
	_ = conceptRows.Close()
	for _, item := range concepts {
		weight := math.Log(1 + float64(maxIntMemory(1, eventCount))/float64(maxIntMemory(1, item.df)))
		postingRows, err := tx.QueryContext(ctx, `SELECT event_id FROM event_sparse_postings WHERE token_id=?`, item.tokenID)
		if err != nil {
			return err
		}
		for postingRows.Next() {
			var eventID string
			if err := postingRows.Scan(&eventID); err != nil {
				_ = postingRows.Close()
				return err
			}
			if err := insertGraphPair(ctx, tx, "event:"+eventID, fmt.Sprintf("concept:%d", item.tokenID), weight); err != nil {
				_ = postingRows.Close()
				return err
			}
		}
		_ = postingRows.Close()
	}
	return tx.Commit()
}

func insertGraphPair(ctx context.Context, tx *sql.Tx, left, right string, weight float64) error {
	for _, edge := range [][2]string{{left, right}, {right, left}} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO graph_edges(source_id,target_id,weight) VALUES(?,?,?)
			ON CONFLICT(source_id,target_id) DO UPDATE SET weight=MAX(weight,excluded.weight)`, edge[0], edge[1], weight); err != nil {
			return err
		}
	}
	return nil
}

func setSidecarMeta(ctx context.Context, db *sql.DB, key, value string) error {
	_, err := db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func encodeFP16Vector(values []float32) ([]byte, error) {
	if len(values) == 0 {
		return nil, errors.New("empty dense vector")
	}
	result := make([]byte, len(values)*2)
	for index, value := range values {
		binary.LittleEndian.PutUint16(result[index*2:], float32ToFloat16(value))
	}
	return result, nil
}

func decodeFP16Vector(content []byte) []float32 {
	result := make([]float32, len(content)/2)
	for index := range result {
		result[index] = float16ToFloat32Memory(binary.LittleEndian.Uint16(content[index*2:]))
	}
	return result
}

func float32ToFloat16(value float32) uint16 {
	bits := math.Float32bits(value)
	sign := uint16((bits >> 16) & 0x8000)
	exponent := int((bits>>23)&0xff) - 127 + 15
	fraction := bits & 0x7fffff
	if exponent <= 0 {
		if exponent < -10 {
			return sign
		}
		fraction = (fraction | 0x800000) >> uint(1-exponent)
		return sign | uint16((fraction+0x1000)>>13)
	}
	if exponent >= 31 {
		return sign | 0x7c00
	}
	return sign | uint16(exponent<<10) | uint16((fraction+0x1000)>>13)
}

func float16ToFloat32Memory(value uint16) float32 {
	sign := uint32(value&0x8000) << 16
	exponent := uint32(value>>10) & 0x1f
	fraction := uint32(value & 0x03ff)
	switch exponent {
	case 0:
		if fraction == 0 {
			return math.Float32frombits(sign)
		}
		exponent = 1
		for fraction&0x0400 == 0 {
			fraction <<= 1
			exponent--
		}
		fraction &= 0x03ff
		exponent += 127 - 15
	case 31:
		exponent = 255
	default:
		exponent += 127 - 15
	}
	return math.Float32frombits(sign | exponent<<23 | fraction<<13)
}

func sortedSourceEvents(evidence []Evidence) []string {
	seen := map[string]struct{}{}
	var result []string
	for _, item := range evidence {
		for _, id := range item.SourceEventIDs {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				result = append(result, id)
			}
		}
	}
	sort.Strings(result)
	return result
}
