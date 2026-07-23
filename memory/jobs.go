package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/viant/sqlite-vec/vector"
)

const (
	jobCompileEvents      = "compile_events"
	jobAdjudicateConflict = "adjudicate_conflict"
	jobEmbedDocument      = "embed_document"
	vectorModelMetaKey    = "vector_model_contract"
)

type outboxRecord struct {
	Seq          int64
	ResourceKind string
	ResourceID   string
	Operation    string
}

type fabricJob struct {
	ID         string
	Kind       string
	Space      string
	ResourceID string
	Payload    string
	Attempts   int
}

func (f *Fabric) processNextWork(ctx context.Context) (bool, error) {
	worked, err := f.processOutboxOnce(ctx)
	if err != nil {
		return worked, fmt.Errorf("process memory outbox: %w", err)
	}
	if err != nil || worked {
		return worked, err
	}
	// Semantic compilation is latency-sensitive and can run concurrently with
	// the local embedding lane. Claim it first so a large import cannot starve
	// the compiler behind hundreds of embedding jobs.
	worked, err = f.processJobOnce(ctx)
	if err != nil {
		return worked, fmt.Errorf("process semantic memory job: %w", err)
	}
	if err != nil || worked {
		return worked, err
	}
	worked, err = f.processEmbeddingBatchOnce(ctx)
	if err != nil {
		return worked, fmt.Errorf("process memory embedding batch: %w", err)
	}
	return worked, nil
}

func (f *Fabric) processOutboxOnce(ctx context.Context) (bool, error) {
	// SQLite is a single-writer store. Serializing the short projection commit
	// avoids competing index transactions while compiler and embedding work can
	// still run on their independent lanes.
	f.outboxMu.Lock()
	defer f.outboxMu.Unlock()

	var record outboxRecord
	err := f.ledger.QueryRowContext(ctx, `SELECT seq, resource_kind, resource_id, operation FROM outbox
		WHERE status='pending' ORDER BY seq LIMIT 1`).Scan(&record.Seq, &record.ResourceKind, &record.ResourceID, &record.Operation)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	claim, err := f.ledger.ExecContext(ctx, `UPDATE outbox SET status='running', attempts=attempts+1, updated_at=?
		WHERE seq=? AND status='pending'`, formatFabricTime(f.now()), record.Seq)
	if err != nil {
		return false, err
	}
	rows, _ := claim.RowsAffected()
	if rows == 0 {
		return true, nil
	}
	if err := f.projectOutboxRecord(ctx, record); err != nil {
		_, _ = f.ledger.ExecContext(context.WithoutCancel(ctx), `UPDATE outbox SET status='pending', updated_at=? WHERE seq=?`,
			formatFabricTime(f.now()), record.Seq)
		return true, err
	}
	_, err = f.ledger.ExecContext(ctx, `UPDATE outbox SET status='done', updated_at=? WHERE seq=?`, formatFabricTime(f.now()), record.Seq)
	return true, err
}

func (f *Fabric) recoverInterruptedWork(ctx context.Context) error {
	now := formatFabricTime(f.now())
	if _, err := f.ledger.ExecContext(ctx, `UPDATE outbox SET status='pending', updated_at=? WHERE status='running'`, now); err != nil {
		return fmt.Errorf("recover memory outbox: %w", err)
	}
	if _, err := f.ledger.ExecContext(ctx, `UPDATE jobs SET status='pending', lease_until='', updated_at=?
		WHERE status='running' AND (lease_until='' OR lease_until<=?)`, now, now); err != nil {
		return fmt.Errorf("recover memory jobs: %w", err)
	}
	return nil
}

func (f *Fabric) projectResources(ctx context.Context, kind string, ids []string) (int64, error) {
	f.outboxMu.Lock()
	defer f.outboxMu.Unlock()

	ids = uniqueStrings(ids)
	if kind == "event" && len(ids) > 1 {
		return f.projectEventResourcesBatch(ctx, ids)
	}
	var maxSeq int64
	for _, id := range ids {
		var record outboxRecord
		err := f.ledger.QueryRowContext(ctx, `SELECT seq, resource_kind, resource_id, operation FROM outbox
			WHERE resource_kind=? AND resource_id=? AND status!='done' ORDER BY seq LIMIT 1`, kind, id).
			Scan(&record.Seq, &record.ResourceKind, &record.ResourceID, &record.Operation)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return maxSeq, err
		}
		if err := f.projectOutboxRecord(ctx, record); err != nil {
			return maxSeq, err
		}
		if _, err := f.ledger.ExecContext(ctx, `UPDATE outbox SET status='done', attempts=attempts+1, updated_at=? WHERE seq=?`,
			formatFabricTime(f.now()), record.Seq); err != nil {
			return maxSeq, err
		}
		if record.Seq > maxSeq {
			maxSeq = record.Seq
		}
	}
	return maxSeq, nil
}

func (f *Fabric) projectEventResourcesBatch(ctx context.Context, ids []string) (int64, error) {
	args := make([]any, 0, len(ids)+1)
	args = append(args, "event")
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := f.ledger.QueryContext(ctx, `SELECT seq, resource_kind, resource_id, operation FROM outbox
		WHERE resource_kind=? AND resource_id IN (`+placeholders(len(ids))+`)
		AND status!='done' ORDER BY seq`, args...)
	if err != nil {
		return 0, err
	}
	var records []outboxRecord
	for rows.Next() {
		var record outboxRecord
		if err := rows.Scan(&record.Seq, &record.ResourceKind, &record.ResourceID, &record.Operation); err != nil {
			_ = rows.Close()
			return 0, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(records) == 0 {
		return 0, nil
	}
	for _, record := range records {
		if record.Operation != "upsert" {
			return f.projectOutboxRecordsIndividually(ctx, records)
		}
	}

	recordIDs := make([]string, 0, len(records))
	for _, record := range records {
		recordIDs = append(recordIDs, record.ResourceID)
	}
	events, err := f.loadEvents(ctx, recordIDs)
	if err != nil {
		return 0, err
	}
	byID := make(map[string]RawEvent, len(events))
	for _, event := range events {
		byID[event.ID] = event
	}
	documents := make([]indexedDocument, 0, len(records))
	var maxSeq int64
	for _, record := range records {
		event, exists := byID[record.ResourceID]
		if !exists {
			return f.projectOutboxRecordsIndividually(ctx, records)
		}
		documents = append(documents, indexedDocument{
			ID: event.ID, Space: event.Space, ResourceKind: "event", ResourceID: event.ID,
			Content: event.Content, Keys: eventIndexKeys(event), ContextID: event.ContextID,
			OccurredAt: event.OccurredAt, Status: SemanticEventDurable,
			SourceEventIDs: []string{event.ID}, LedgerSeq: record.Seq, IndexFTS: true,
			Metadata: map[string]any{"actor": event.Actor, "source_kind": event.SourceKind,
				"source_ref": event.SourceRef, "session_id": event.SessionID},
		})
		if record.Seq > maxSeq {
			maxSeq = record.Seq
		}
	}
	if err := f.upsertIndexDocuments(ctx, documents); err != nil {
		return maxSeq, err
	}
	if err := f.finishOutboxRecords(ctx, records); err != nil {
		return maxSeq, err
	}
	return maxSeq, nil
}

func (f *Fabric) projectOutboxRecordsIndividually(ctx context.Context, records []outboxRecord) (int64, error) {
	var maxSeq int64
	for _, record := range records {
		if err := f.projectOutboxRecord(ctx, record); err != nil {
			return maxSeq, err
		}
		if err := f.finishOutboxRecords(ctx, []outboxRecord{record}); err != nil {
			return maxSeq, err
		}
		if record.Seq > maxSeq {
			maxSeq = record.Seq
		}
	}
	return maxSeq, nil
}

func (f *Fabric) finishOutboxRecords(ctx context.Context, records []outboxRecord) error {
	tx, err := f.ledger.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := formatFabricTime(f.now())
	for _, record := range records {
		if _, err := tx.ExecContext(ctx, `UPDATE outbox SET status='done', attempts=attempts+1,
			updated_at=? WHERE seq=?`, now, record.Seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (f *Fabric) projectOutboxRecord(ctx context.Context, record outboxRecord) error {
	switch record.Operation {
	case "delete":
		return f.deleteIndexedResource(ctx, record.ResourceID, record.Seq)
	case "upsert":
		switch record.ResourceKind {
		case "event":
			return f.projectEvent(ctx, record.ResourceID, record.Seq)
		case "node":
			return f.projectNode(ctx, record.ResourceID, record.Seq)
		default:
			return fmt.Errorf("unsupported memory outbox resource %q", record.ResourceKind)
		}
	default:
		return fmt.Errorf("unsupported memory outbox operation %q", record.Operation)
	}
}

func (f *Fabric) projectEvent(ctx context.Context, eventID string, seq int64) error {
	event, err := scanEvent(f.ledger.QueryRowContext(ctx, `SELECT event_id, space, context_id, session_id,
		actor, source_kind, content, occurred_at, source_ref, metadata_json FROM events WHERE event_id=? AND tombstoned=0`, eventID))
	if errors.Is(err, sql.ErrNoRows) {
		return f.deleteIndexedResource(ctx, eventID, seq)
	}
	if err != nil {
		return err
	}
	keys := eventIndexKeys(event)
	return f.upsertIndexDocument(ctx, indexedDocument{
		ID: event.ID, Space: event.Space, ResourceKind: "event", ResourceID: event.ID,
		Content: event.Content, Keys: keys, ContextID: event.ContextID, OccurredAt: event.OccurredAt,
		Status: SemanticEventDurable, SourceEventIDs: []string{event.ID}, LedgerSeq: seq,
		IndexFTS: true,
		Metadata: map[string]any{"actor": event.Actor, "source_kind": event.SourceKind,
			"source_ref": event.SourceRef, "session_id": event.SessionID},
	})
}

func eventIndexKeys(event RawEvent) []string {
	values := []string{event.Actor, event.SourceKind, event.SourceRef}
	for key, value := range event.Metadata {
		if strings.TrimSpace(value) != "" {
			values = append(values, key, value)
		}
	}
	return normalizeStringList(values, 16)
}

type indexedDocument struct {
	ID             string
	Space          string
	ResourceKind   string
	ResourceID     string
	Content        string
	Keys           []string
	ContextID      string
	OccurredAt     time.Time
	SlotID         string
	Status         SemanticStatus
	SourceEventIDs []string
	LedgerSeq      int64
	Metadata       map[string]any
	IndexFTS       bool
	IndexVector    bool
	VectorText     string
}

func (f *Fabric) upsertIndexDocument(ctx context.Context, document indexedDocument) error {
	return f.upsertIndexDocuments(ctx, []indexedDocument{document})
}

func (f *Fabric) upsertIndexDocuments(ctx context.Context, documents []indexedDocument) error {
	if len(documents) == 0 {
		return nil
	}
	tx, err := f.index.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, document := range documents {
		if err := f.upsertIndexDocumentTx(ctx, tx, document); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return f.scheduleDocumentEmbeddings(ctx, documents)
}

func (f *Fabric) upsertIndexDocumentTx(ctx context.Context, tx *sql.Tx, document indexedDocument) error {
	keysText := strings.Join(normalizeStringList(document.Keys, 32), " ")
	if _, err := tx.ExecContext(ctx, `INSERT INTO documents(
		doc_id, space, resource_kind, resource_id, content, keys_text, context_id, occurred_at,
		slot_id, semantic_status, source_event_ids_json, ledger_seq, metadata_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(doc_id) DO UPDATE SET
			space=excluded.space, resource_kind=excluded.resource_kind, resource_id=excluded.resource_id,
			content=excluded.content, keys_text=excluded.keys_text, context_id=excluded.context_id,
			occurred_at=excluded.occurred_at, slot_id=excluded.slot_id, semantic_status=excluded.semantic_status,
			source_event_ids_json=excluded.source_event_ids_json, ledger_seq=excluded.ledger_seq,
			metadata_json=excluded.metadata_json`, document.ID, document.Space, document.ResourceKind,
		document.ResourceID, document.Content, keysText, document.ContextID, formatFabricTime(document.OccurredAt),
		document.SlotID, document.Status, marshalJSONArray(document.SourceEventIDs), document.LedgerSeq, marshalJSON(document.Metadata)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM document_fts WHERE doc_id=?`, document.ID); err != nil {
		return err
	}
	indexable := document.Status != SemanticTombstoned && document.Status != SemanticRejected && document.Status != SemanticQuarantined
	if document.IndexFTS && indexable {
		if _, err := tx.ExecContext(ctx, `INSERT INTO document_fts(doc_id, space, resource_kind, content, keys_text)
			VALUES (?, ?, ?, ?, ?)`, document.ID, document.Space, document.ResourceKind, document.Content, keysText); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM key_postings WHERE doc_id=?`, document.ID); err != nil {
		return err
	}
	for _, key := range normalizeStringList(document.Keys, 32) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO key_postings(space, key_text, doc_id, weight)
			VALUES (?, ?, ?, 1) ON CONFLICT DO NOTHING`, document.Space, key, document.ID); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO index_meta(key, value) VALUES ('indexed_ledger_seq', ?)
		ON CONFLICT(key) DO UPDATE SET value=CAST(MAX(CAST(index_meta.value AS INTEGER), CAST(excluded.value AS INTEGER)) AS TEXT)`,
		fmt.Sprintf("%d", document.LedgerSeq)); err != nil {
		return err
	}
	if !document.IndexVector || !indexable {
		for _, view := range []string{"content", "trigger"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM _vec_memory_vectors WHERE dataset_id=? AND id=?`,
				vectorDataset(document.Space, view), document.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

type fabricJobSpec struct {
	kind, space, resourceID, hash string
	payload                       any
	availableAt                   time.Time
}

func (f *Fabric) scheduleDocumentEmbeddings(ctx context.Context, documents []indexedDocument) error {
	if f.options.Vectorizer == nil {
		return nil
	}
	if err := f.ensureVectorModel(ctx); err != nil {
		return err
	}
	jobs := make([]fabricJobSpec, 0, len(documents))
	for _, document := range documents {
		indexable := document.Status != SemanticTombstoned && document.Status != SemanticRejected &&
			document.Status != SemanticQuarantined
		if !document.IndexVector || !indexable {
			continue
		}
		vectorText := strings.TrimSpace(document.VectorText)
		if vectorText == "" {
			vectorText = document.Content
		}
		jobs = append(jobs, fabricJobSpec{kind: jobEmbedDocument, space: document.Space,
			resourceID: document.ID, hash: contentHash(document.ID, vectorText, f.options.Vectorizer.Model()),
			payload:     map[string]any{"doc_id": document.ID, "content": vectorText, "space": document.Space},
			availableAt: f.now()})
	}
	return f.enqueueJobs(ctx, jobs)
}

func (f *Fabric) vectorModelCompatible(ctx context.Context) (bool, error) {
	if f.options.Vectorizer == nil {
		return false, nil
	}
	var current string
	err := f.index.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key=?`, vectorModelMetaKey).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return current == f.options.Vectorizer.Model(), nil
}

func (f *Fabric) ensureVectorModel(ctx context.Context) error {
	compatible, err := f.vectorModelCompatible(ctx)
	if err != nil || compatible {
		return err
	}
	tx, err := f.index.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM _vec_memory_vectors`); err != nil {
		return fmt.Errorf("invalidate incompatible memory vectors: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO index_meta(key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, vectorModelMetaKey, f.options.Vectorizer.Model()); err != nil {
		return fmt.Errorf("record memory vector model: %w", err)
	}
	return tx.Commit()
}

func (f *Fabric) enqueueJobs(ctx context.Context, jobs []fabricJobSpec) error {
	if len(jobs) == 0 {
		return nil
	}
	tx, err := f.ledger.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := formatFabricTime(f.now())
	for _, job := range jobs {
		jobID := stableFabricID("job", job.kind, job.hash)
		if _, err := tx.ExecContext(ctx, `INSERT INTO jobs(
			job_id, job_kind, space, resource_id, content_hash, payload_json, status, available_at, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)
			ON CONFLICT(job_kind, content_hash) DO UPDATE SET
				status=CASE WHEN jobs.status IN ('complete','running') THEN jobs.status ELSE 'pending' END,
				available_at=CASE WHEN jobs.status='complete' THEN jobs.available_at ELSE excluded.available_at END,
				updated_at=excluded.updated_at`, jobID, job.kind, normalizeSpace(job.space), job.resourceID, job.hash,
			marshalJSON(job.payload), formatFabricTime(job.availableAt), now, now); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	f.wakeWorker()
	return nil
}

func (f *Fabric) deleteIndexedResource(ctx context.Context, resourceID string, seq int64) error {
	return f.deleteIndexedResources(ctx, []string{resourceID}, seq)
}

func (f *Fabric) deleteIndexedResources(ctx context.Context, resourceIDs []string, seq int64) error {
	resourceIDs = uniqueStrings(resourceIDs)
	if len(resourceIDs) == 0 {
		return nil
	}
	tx, err := f.index.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, resourceID := range resourceIDs {
		var space string
		_ = tx.QueryRowContext(ctx, `SELECT space FROM documents WHERE doc_id=?`, resourceID).Scan(&space)
		for _, statement := range []string{
			`DELETE FROM document_fts WHERE doc_id=?`,
			`DELETE FROM key_postings WHERE doc_id=?`,
			`DELETE FROM active_slots WHERE node_id=?`,
			`DELETE FROM documents WHERE doc_id=?`,
		} {
			if _, err := tx.ExecContext(ctx, statement, resourceID); err != nil {
				return err
			}
		}
		if space != "" {
			for _, view := range []string{"content", "trigger"} {
				if _, err := tx.ExecContext(ctx, `DELETE FROM _vec_memory_vectors WHERE dataset_id=? AND id=?`, vectorDataset(space, view), resourceID); err != nil {
					return err
				}
			}
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO index_meta(key, value) VALUES ('indexed_ledger_seq', ?)
		ON CONFLICT(key) DO UPDATE SET value=CAST(MAX(CAST(index_meta.value AS INTEGER), CAST(excluded.value AS INTEGER)) AS TEXT)`,
		fmt.Sprintf("%d", seq)); err != nil {
		return err
	}
	return tx.Commit()
}

func (f *Fabric) enqueueJob(ctx context.Context, kind, space, resourceID, hash string, payload any, availableAt time.Time) (JobRef, error) {
	now := f.now()
	jobID := stableFabricID("job", kind, hash)
	_, err := f.ledger.ExecContext(ctx, `INSERT INTO jobs(
		job_id, job_kind, space, resource_id, content_hash, payload_json, status, available_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?, ?)
		ON CONFLICT(job_kind, content_hash) DO UPDATE SET
			status=CASE WHEN jobs.status IN ('complete','running') THEN jobs.status ELSE 'pending' END,
			available_at=CASE WHEN jobs.status='complete' THEN jobs.available_at ELSE excluded.available_at END,
			updated_at=excluded.updated_at`, jobID, kind, normalizeSpace(space), resourceID, hash,
		marshalJSON(payload), formatFabricTime(availableAt), formatFabricTime(now), formatFabricTime(now))
	if err != nil {
		return JobRef{}, err
	}
	f.wakeWorker()
	return JobRef{ID: jobID, Kind: kind, Status: "pending"}, nil
}

func (f *Fabric) processJobOnce(ctx context.Context) (bool, error) {
	var job fabricJob
	now := f.now()
	err := f.claimNonEmbeddingJob(ctx, now, &job)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, fmt.Errorf("claim job: %w", err)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	jobs := []fabricJob{job}
	executeErr := f.executeJobs(ctx, jobs)
	if isPermanentMemoryAPIError(executeErr) {
		f.tripRemoteCircuit(executeErr)
	}
	finishErr := f.finishJobs(context.WithoutCancel(ctx), jobs, executeErr)
	if executeErr != nil {
		return true, fmt.Errorf("execute %s job: %w", job.Kind, executeErr)
	}
	if finishErr != nil {
		return true, fmt.Errorf("finish %s job: %w", job.Kind, finishErr)
	}
	return true, nil
}

func (f *Fabric) claimNonEmbeddingJob(ctx context.Context, now time.Time, job *fabricJob) error {
	return f.ledger.QueryRowContext(ctx, `UPDATE jobs SET status='running', attempts=attempts+1,
		lease_until=?, updated_at=? WHERE job_id=(
			SELECT job_id FROM jobs WHERE status='pending' AND job_kind<>? AND available_at<=?
			AND (job_kind<>? OR (SELECT COUNT(*) FROM jobs AS active_jobs
				WHERE active_jobs.status='running' AND active_jobs.job_kind=?) < ?)
			ORDER BY CASE WHEN job_kind=? THEN 0 ELSE 1 END, available_at, created_at LIMIT 1
		) AND status='pending'
		RETURNING job_id, job_kind, space, resource_id, payload_json, attempts`,
		formatFabricTime(now.Add(2*time.Minute)), formatFabricTime(now), jobEmbedDocument, formatFabricTime(now),
		jobCompileEvents, jobCompileEvents, f.options.CompileConcurrency, jobCompileEvents).
		Scan(&job.ID, &job.Kind, &job.Space, &job.ResourceID, &job.Payload, &job.Attempts)
}

func (f *Fabric) processEmbeddingBatchOnce(ctx context.Context) (bool, error) {
	now := f.now()
	rows, err := f.ledger.QueryContext(ctx, `UPDATE jobs SET status='running', attempts=attempts+1,
		lease_until=?, updated_at=? WHERE job_id IN (
			SELECT job_id FROM jobs WHERE status='pending' AND job_kind=? AND available_at<=?
			ORDER BY available_at, created_at LIMIT ?
		) AND status='pending'
		RETURNING job_id, job_kind, space, resource_id, payload_json, attempts`,
		formatFabricTime(now.Add(2*time.Minute)), formatFabricTime(now), jobEmbedDocument,
		formatFabricTime(now), f.options.EmbeddingBatchSize)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var jobs []fabricJob
	for rows.Next() {
		var job fabricJob
		if err := rows.Scan(&job.ID, &job.Kind, &job.Space, &job.ResourceID, &job.Payload, &job.Attempts); err != nil {
			return false, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	if len(jobs) == 0 {
		return false, nil
	}
	executeErr := f.embedDocumentJobs(ctx, jobs)
	finishErr := f.finishJobs(context.WithoutCancel(ctx), jobs, executeErr)
	if executeErr != nil {
		return true, executeErr
	}
	return true, finishErr
}

func (f *Fabric) finishJobs(ctx context.Context, jobs []fabricJob, executeErr error) error {
	now := f.now()
	if executeErr == nil {
		for _, job := range jobs {
			if _, err := f.ledger.ExecContext(ctx, `UPDATE jobs SET status='complete', lease_until='', last_error='', updated_at=? WHERE job_id=?`,
				formatFabricTime(now), job.ID); err != nil {
				return err
			}
		}
		return nil
	}
	for _, job := range jobs {
		status := "pending"
		if isPermanentMemoryJobError(executeErr) || job.Attempts >= 2 {
			status = "failed"
		}
		if _, err := f.ledger.ExecContext(ctx, `UPDATE jobs SET status=?, lease_until='', last_error=?,
			available_at=?, updated_at=? WHERE job_id=?`, status, truncateMemoryError(executeErr.Error()),
			formatFabricTime(now.Add(time.Minute)), formatFabricTime(now), job.ID); err != nil {
			return err
		}
	}
	return nil
}

func (f *Fabric) executeJobs(ctx context.Context, jobs []fabricJob) error {
	if len(jobs) == 0 {
		return nil
	}
	if jobs[0].Kind == jobEmbedDocument {
		return f.embedDocumentJobs(ctx, jobs)
	}
	return f.executeJob(ctx, jobs[0])
}

func (f *Fabric) executeJob(ctx context.Context, job fabricJob) error {
	if job.Kind == jobCompileEvents || job.Kind == jobAdjudicateConflict {
		if err := f.remoteCircuitError(); err != nil {
			return err
		}
	}
	switch job.Kind {
	case jobEmbedDocument:
		return f.embedDocumentJob(ctx, job)
	case jobCompileEvents:
		return f.compileEventsJob(ctx, job)
	case jobAdjudicateConflict:
		return f.adjudicateConflictJob(ctx, job)
	default:
		return fmt.Errorf("unsupported memory job kind %q", job.Kind)
	}
}

func (f *Fabric) remoteCircuitError() error {
	f.remoteMu.RLock()
	defer f.remoteMu.RUnlock()
	return f.remoteErr
}

func (f *Fabric) tripRemoteCircuit(err error) {
	if err == nil {
		return
	}
	f.remoteMu.Lock()
	if f.remoteErr == nil {
		f.remoteErr = err
	}
	f.remoteMu.Unlock()
}

func (f *Fabric) embedDocumentJob(ctx context.Context, job fabricJob) error {
	return f.embedDocumentJobs(ctx, []fabricJob{job})
}

type embeddingJobPayload struct {
	DocID   string `json:"doc_id"`
	Space   string `json:"space"`
	Content string `json:"content"`
}

type pendingEmbedding struct {
	Payload embeddingJobPayload
	View    string
	Text    string
	Purpose VectorPurpose
}

func (f *Fabric) embedDocumentJobs(ctx context.Context, jobs []fabricJob) error {
	if f.options.Vectorizer == nil {
		return nil
	}
	entries := make([]pendingEmbedding, 0, len(jobs))
	for _, job := range jobs {
		var payload embeddingJobPayload
		if err := json.Unmarshal([]byte(job.Payload), &payload); err != nil {
			return err
		}
		entries = append(entries, pendingEmbedding{
			Payload: payload, View: "content", Text: payload.Content, Purpose: VectorContent})
	}
	encodedVectors := make([][]byte, len(entries))
	if len(entries) > 0 {
		texts := make([]string, len(entries))
		for index := range entries {
			texts[index] = entries[index].Text
		}
		vectors, err := f.options.Vectorizer.Embed(ctx, texts, VectorContent)
		if err != nil {
			return err
		}
		if len(vectors) != len(entries) {
			return fmt.Errorf("memory vectorizer returned %d vectors for %d documents", len(vectors), len(entries))
		}
		for index, entry := range entries {
			if len(vectors[index]) != f.options.Vectorizer.Dimensions() {
				return fmt.Errorf("memory vectorizer returned invalid dimensions for %s", entry.Payload.DocID)
			}
			encoded, err := vector.EncodeEmbedding(vectors[index])
			if err != nil {
				return err
			}
			encodedVectors[index] = encoded
		}
	}

	// Embedding can take seconds on a cold local runtime. Keep that work outside
	// the SQLite transaction so event/node projection remains available and the
	// compiler can commit while the local model is running.
	tx, err := f.index.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for index, entry := range entries {
		meta := marshalJSON(map[string]any{"model": f.options.Vectorizer.Model(), "view": entry.View})
		if _, err := tx.ExecContext(ctx, `INSERT INTO _vec_memory_vectors(dataset_id, id, content, meta, embedding)
				VALUES (?, ?, ?, ?, ?) ON CONFLICT(dataset_id, id) DO UPDATE SET
					content=excluded.content, meta=excluded.meta, embedding=excluded.embedding`,
			vectorDataset(entry.Payload.Space, entry.View), entry.Payload.DocID, entry.Text, meta, encodedVectors[index]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func vectorDataset(space, view string) string {
	return normalizeSpace(space) + "\x1f" + view
}

func (f *Fabric) Flush(ctx context.Context) error {
	workerCount := maxIntMemory(1, f.options.WorkerCount)
	type workResult struct {
		worked bool
		err    error
	}
	for {
		results := make(chan workResult, workerCount)
		var wg sync.WaitGroup
		for range workerCount {
			wg.Add(1)
			go func() {
				defer wg.Done()
				worked, err := f.processNextWork(ctx)
				results <- workResult{worked: worked, err: err}
			}()
		}
		wg.Wait()
		close(results)
		worked := false
		for result := range results {
			if result.err != nil {
				return result.err
			}
			worked = worked || result.worked
		}
		if !worked {
			if err := f.pruneOrphanVectors(ctx); err != nil {
				return fmt.Errorf("prune orphan memory vectors: %w", err)
			}
			return ctx.Err()
		}
	}
}

func (f *Fabric) pruneOrphanVectors(ctx context.Context) error {
	rows, err := f.index.QueryContext(ctx, `SELECT vectors.dataset_id, vectors.id
		FROM _vec_memory_vectors AS vectors
		WHERE NOT EXISTS (
			SELECT 1 FROM documents
			WHERE documents.doc_id=vectors.id
			AND (documents.resource_kind='chunk' OR
				(documents.resource_kind='node' AND documents.semantic_status NOT IN ('tombstoned','rejected','quarantined')))
		)`)
	if err != nil {
		return err
	}
	type orphanVector struct {
		datasetID string
		id        string
	}
	var orphans []orphanVector
	for rows.Next() {
		var orphan orphanVector
		if err := rows.Scan(&orphan.datasetID, &orphan.id); err != nil {
			_ = rows.Close()
			return err
		}
		orphans = append(orphans, orphan)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(orphans) == 0 {
		return nil
	}
	tx, err := f.index.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, orphan := range orphans {
		if _, err := tx.ExecContext(ctx, `DELETE FROM _vec_memory_vectors WHERE dataset_id=? AND id=?`,
			orphan.datasetID, orphan.id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func isPermanentMemoryAPIError(err error) bool {
	if err == nil {
		return false
	}
	text := strings.ToLower(err.Error())
	for _, fragment := range []string{"quota exhausted", "insufficient balance", "payment required", "status 402", "api_quota_exhausted"} {
		if strings.Contains(text, fragment) {
			return true
		}
	}
	return false
}

func isPermanentMemoryJobError(err error) bool {
	if isPermanentMemoryAPIError(err) {
		return true
	}
	var contractErr *CompileContractError
	return errors.As(err, &contractErr)
}

func truncateMemoryError(value string) string {
	const limit = 2000
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
