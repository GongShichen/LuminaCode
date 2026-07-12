package longmemory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/viant/sqlite-vec/vector"
)

type MaintenanceResult struct {
	Embedded        int `json:"embedded"`
	ChunkEmbedded   int `json:"chunk_embedded"`
	AtomEmbedded    int `json:"atom_embedded"`
	SessionEmbedded int `json:"session_embedded"`
	Enriched        int `json:"enriched"`
	Consolidated    int `json:"consolidated"`
	Linked          int `json:"linked"`
	Promoted        int `json:"promoted"`
	Archived        int `json:"archived"`
}

type pendingEmbeddingWrite struct {
	id          string
	contentHash string
	vector      []float32
}

func (s *Store) commitEmbeddingBatch(ctx context.Context, kind, model string, writes []pendingEmbeddingWrite) error {
	if len(writes) == 0 {
		return nil
	}
	type encodedWrite struct {
		id          string
		contentHash string
		dimensions  int
		blob        []byte
	}
	encoded := make([]encodedWrite, 0, len(writes))
	for _, write := range writes {
		blob, err := vector.EncodeEmbedding(write.vector)
		if err != nil {
			return err
		}
		encoded = append(encoded, encodedWrite{id: write.id, contentHash: write.contentHash,
			dimensions: len(write.vector), blob: blob})
	}
	var statement string
	switch kind {
	case "memory":
		statement = `INSERT INTO memory_embeddings(memory_id, model, dimensions, content_hash, embedding, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(memory_id, model) DO UPDATE SET dimensions=excluded.dimensions,
			content_hash=excluded.content_hash, embedding=excluded.embedding, updated_at=excluded.updated_at`
	case "chunk":
		statement = `INSERT INTO memory_chunk_embeddings(chunk_id, model, dimensions, content_hash, embedding, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(chunk_id, model) DO UPDATE SET dimensions=excluded.dimensions,
			content_hash=excluded.content_hash, embedding=excluded.embedding, updated_at=excluded.updated_at`
	case "atom":
		statement = `INSERT INTO memory_atom_embeddings(atom_id, model, dimensions, content_hash, embedding, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(atom_id, model) DO UPDATE SET dimensions=excluded.dimensions,
			content_hash=excluded.content_hash, embedding=excluded.embedding, updated_at=excluded.updated_at`
	default:
		return fmt.Errorf("unsupported embedding batch kind %q", kind)
	}
	writer := extractionCommitLock(s.path)
	writer.Lock()
	defer writer.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, statement)
	if err != nil {
		return err
	}
	defer stmt.Close()
	now := formatTime(time.Now().UTC())
	for _, write := range encoded {
		if _, err := stmt.ExecContext(ctx, write.id, model, write.dimensions, write.contentHash, write.blob, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) EnqueueJob(ctx context.Context, job Job) error {
	job = normalizeJob(job)
	_, err := s.db.ExecContext(ctx, `INSERT INTO memory_jobs(job_id, kind, scope_type, scope_key, payload,
		status, attempts, last_error, available_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(job_id) DO UPDATE SET payload=excluded.payload, status=CASE WHEN memory_jobs.status='complete'
		THEN memory_jobs.status ELSE 'pending' END, available_at=excluded.available_at, updated_at=excluded.updated_at`,
		job.JobID, job.Kind, job.ScopeType, job.ScopeKey, job.Payload, job.Status, job.Attempts, job.LastError,
		formatTime(job.AvailableAt), formatTime(job.CreatedAt), formatTime(job.UpdatedAt))
	return err
}

func (s *Store) StartJob(ctx context.Context, jobID string) error {
	if strings.TrimSpace(jobID) == "" {
		return errors.New("memory job id is required")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE memory_jobs SET status='running',
		attempts=attempts+CASE WHEN status='running' THEN 0 ELSE 1 END,
		updated_at=? WHERE job_id=?`, formatTime(time.Now().UTC()), jobID)
	return err
}

func (s *Store) ClaimJobs(ctx context.Context, kinds []string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 16
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	query := `SELECT job_id, kind, scope_type, scope_key, payload, status, attempts, last_error,
		available_at, created_at, updated_at FROM memory_jobs WHERE status IN ('pending','retry') AND available_at<=?`
	args := []any{formatTime(time.Now().UTC())}
	if len(kinds) > 0 {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",")
		query += " AND kind IN (" + marks + ")"
		for _, kind := range kinds {
			args = append(args, kind)
		}
	}
	query += " ORDER BY available_at, created_at LIMIT ?"
	args = append(args, limit)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	jobs, err := scanJobs(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	for index := range jobs {
		jobs[index].Status = "running"
		jobs[index].Attempts++
		jobs[index].UpdatedAt = time.Now().UTC()
		if _, err := tx.ExecContext(ctx, `UPDATE memory_jobs SET status='running', attempts=?, updated_at=? WHERE job_id=?`,
			jobs[index].Attempts, formatTime(jobs[index].UpdatedAt), jobs[index].JobID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return jobs, nil
}

func (s *Store) ListJobs(ctx context.Context, kinds []string, statuses []string, limit int) ([]Job, error) {
	if limit <= 0 {
		limit = 100
	}
	query := `SELECT job_id, kind, scope_type, scope_key, payload, status, attempts, last_error,
		available_at, created_at, updated_at FROM memory_jobs WHERE 1=1`
	var args []any
	if len(kinds) > 0 {
		query += " AND kind IN (" + strings.TrimSuffix(strings.Repeat("?,", len(kinds)), ",") + ")"
		for _, kind := range kinds {
			args = append(args, kind)
		}
	}
	if len(statuses) > 0 {
		query += " AND status IN (" + strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",") + ")"
		for _, status := range statuses {
			args = append(args, status)
		}
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanJobs(rows)
}

func (s *Store) CompleteJob(ctx context.Context, jobID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE memory_jobs SET status='complete', last_error='', updated_at=? WHERE job_id=?`,
		formatTime(time.Now().UTC()), jobID)
	return err
}

func (s *Store) RetryJob(ctx context.Context, jobID string, jobErr error, delay time.Duration) error {
	if delay <= 0 {
		delay = time.Minute
	}
	message := ""
	if jobErr != nil {
		message = jobErr.Error()
	}
	_, err := s.db.ExecContext(ctx, `UPDATE memory_jobs SET status='retry', last_error=?, available_at=?, updated_at=? WHERE job_id=?`,
		message, formatTime(time.Now().UTC().Add(delay)), formatTime(time.Now().UTC()), jobID)
	return err
}

func (s *Store) MemoriesMissingEmbedding(ctx context.Context, model string, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixedMemoryColumns("m")+` FROM memories m
		LEFT JOIN memory_embeddings e ON e.memory_id=m.memory_id AND e.model=?
		WHERE m.status=? AND e.memory_id IS NULL ORDER BY m.updated_at LIMIT ?`, model, StatusActive, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntries(rows)
}

func (s *Store) RunMaintenance(ctx context.Context, embedder Embedder, limit int) (MaintenanceResult, error) {
	var result MaintenanceResult
	if embedder == nil {
		return result, errors.New("memory embedder is required")
	}
	entries, err := s.MemoriesMissingEmbedding(ctx, embedder.Model(), limit)
	if err != nil {
		return result, err
	}
	if len(entries) > 0 {
		texts := make([]string, len(entries))
		for index, entry := range entries {
			texts[index] = entry.Title + "\n" + entry.Summary + "\n" + entry.Content
		}
		vectors, err := embedder.Embed(ctx, texts, EmbeddingPassage)
		if err != nil {
			return result, err
		}
		writes := make([]pendingEmbeddingWrite, 0, minInt(len(entries), len(vectors)))
		for index, entry := range entries {
			if index >= len(vectors) {
				break
			}
			writes = append(writes, pendingEmbeddingWrite{id: entry.MemoryID,
				contentHash: StableID(entry.ScopeType, entry.ScopeKey, "embedding-content", texts[index]), vector: vectors[index]})
		}
		if err := s.commitEmbeddingBatch(ctx, "memory", embedder.Model(), writes); err != nil {
			return result, err
		}
		for _, entry := range entries[:len(writes)] {
			result.Embedded++
			if enriched, enrichErr := s.enrichLegacyMemory(ctx, entry); enrichErr == nil && enriched {
				result.Enriched++
			}
		}
	}
	chunks, err := s.ChunksMissingEmbedding(ctx, embedder.Model(), limit)
	if err != nil {
		return result, err
	}
	atoms, err := s.AtomsMissingEmbedding(ctx, embedder.Model(), limit)
	if err != nil {
		return result, err
	}
	for start := 0; start < len(atoms); start += EmbeddingBatchSize(embedder) {
		end := minInt(start+EmbeddingBatchSize(embedder), len(atoms))
		texts := make([]string, end-start)
		for index := start; index < end; index++ {
			texts[index-start] = atomSearchText(atoms[index])
		}
		vectors, embedErr := embedder.Embed(ctx, texts, EmbeddingPassage)
		if embedErr != nil {
			return result, embedErr
		}
		writes := make([]pendingEmbeddingWrite, 0, minInt(len(atoms[start:end]), len(vectors)))
		for index, atom := range atoms[start:end] {
			if index >= len(vectors) {
				break
			}
			writes = append(writes, pendingEmbeddingWrite{id: atom.AtomID, contentHash: atom.ContentHash, vector: vectors[index]})
		}
		if err := s.commitEmbeddingBatch(ctx, "atom", embedder.Model(), writes); err != nil {
			return result, err
		}
		result.AtomEmbedded += len(writes)
	}
	for start := 0; start < len(chunks); start += EmbeddingBatchSize(embedder) {
		end := minInt(start+EmbeddingBatchSize(embedder), len(chunks))
		texts := make([]string, end-start)
		for index := start; index < end; index++ {
			texts[index-start] = chunks[index].Text
		}
		vectors, embedErr := embedder.Embed(ctx, texts, EmbeddingPassage)
		if embedErr != nil {
			return result, embedErr
		}
		writes := make([]pendingEmbeddingWrite, 0, minInt(len(chunks[start:end]), len(vectors)))
		for index, chunk := range chunks[start:end] {
			if index >= len(vectors) {
				break
			}
			writes = append(writes, pendingEmbeddingWrite{id: chunk.ChunkID, contentHash: chunk.ContentHash, vector: vectors[index]})
		}
		if err := s.commitEmbeddingBatch(ctx, "chunk", embedder.Model(), writes); err != nil {
			return result, err
		}
		result.ChunkEmbedded += len(writes)
	}
	sessions, err := s.sessionsMissingEmbedding(ctx, embedder.Model(), limit)
	if err != nil {
		return result, err
	}
	if len(sessions) > 0 {
		texts := make([]string, len(sessions))
		for index := range sessions {
			texts[index] = sessions[index].Content
		}
		vectors, err := embedder.Embed(ctx, texts, EmbeddingPassage)
		if err != nil {
			return result, err
		}
		writes := make([]pendingEmbeddingWrite, 0, minInt(len(sessions), len(vectors)))
		for index, session := range sessions {
			if index >= len(vectors) {
				break
			}
			writes = append(writes, pendingEmbeddingWrite{id: session.MemoryID,
				contentHash: StableID(session.ScopeType, session.ScopeKey, session.SourceSessionID, texts[index]), vector: vectors[index]})
		}
		if err := s.commitEmbeddingBatch(ctx, "memory", embedder.Model(), writes); err != nil {
			return result, err
		}
		result.SessionEmbedded += len(writes)
	}
	consolidated, err := s.Consolidate(ctx, embedder.Model(), maxInt(limit*8, 128))
	if err != nil {
		return result, err
	}
	result.Consolidated = consolidated.Consolidated
	result.Linked = consolidated.Linked
	result.Promoted = consolidated.Promoted
	return result, nil
}

func (s *Store) sessionsMissingEmbedding(ctx context.Context, model string, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := s.db.QueryContext(ctx, `SELECT s.index_id, s.scope_type, s.scope_key, s.session_id, s.summary,
		s.keyphrases_json, s.entities_json, s.roles_json, s.started_at, s.ended_at
		FROM memory_session_index s LEFT JOIN memory_embeddings e ON e.memory_id=s.index_id AND e.model=?
		WHERE e.memory_id IS NULL OR e.content_hash<>s.content_hash ORDER BY s.ended_at LIMIT ?`, model, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		entry, _, scanErr := scanSessionEntry(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

type ConsolidationResult struct {
	Consolidated int `json:"consolidated"`
	Linked       int `json:"linked"`
	Promoted     int `json:"promoted"`
}

func (s *Store) Consolidate(ctx context.Context, embeddingModel string, limit int) (ConsolidationResult, error) {
	var result ConsolidationResult
	if limit <= 0 {
		limit = 256
	}
	entries, err := s.List(ctx, SearchOptions{Limit: limit})
	if err != nil {
		return result, err
	}
	ids := make([]string, len(entries))
	for index := range entries {
		ids[index] = entries[index].MemoryID
	}
	embeddings, err := s.LoadEmbeddings(ctx, ids, embeddingModel)
	if err != nil {
		return result, err
	}
	consumed := map[int]struct{}{}
	for left := 0; left < len(entries); left++ {
		if _, ok := consumed[left]; ok {
			continue
		}
		for right := left + 1; right < len(entries); right++ {
			if _, ok := consumed[right]; ok {
				continue
			}
			if !sameMemoryNamespace(entries[left], entries[right]) || !isSemanticDuplicate(entries[left], entries[right], embeddings) {
				continue
			}
			conflicts, conflictErr := s.memoriesConflict(ctx, entries[left].MemoryID, entries[right].MemoryID)
			if conflictErr != nil {
				return result, conflictErr
			}
			if conflicts {
				continue
			}
			canonical, duplicate := chooseCanonicalMemory(entries[left], entries[right])
			if err := s.mergeDuplicateMemory(ctx, canonical, duplicate); err != nil {
				return result, err
			}
			if duplicate.MemoryID == entries[left].MemoryID {
				entries[left] = canonical
			}
			consumed[right] = struct{}{}
			result.Consolidated++
		}
	}
	active := make([]Entry, 0, len(entries)-len(consumed))
	for index, entry := range entries {
		if _, ok := consumed[index]; ok {
			continue
		}
		active = append(active, entry)
	}
	linked, err := s.linkRelatedMemories(ctx, active, 1000)
	if err != nil {
		return result, err
	}
	result.Linked = linked
	promoted, err := s.promoteRepeatedProcedures(ctx, active)
	if err != nil {
		return result, err
	}
	result.Promoted = promoted
	return result, nil
}

func sameMemoryNamespace(left, right Entry) bool {
	return left.ScopeType == right.ScopeType && left.ScopeKey == right.ScopeKey && left.MemoryType == right.MemoryType
}

func isSemanticDuplicate(left, right Entry, embeddings map[string][]float32) bool {
	if normalizeMaintenanceText(left.Title) == "" || normalizeMaintenanceText(left.Title) != normalizeMaintenanceText(right.Title) {
		return false
	}
	if normalizeMaintenanceText(left.Content) == normalizeMaintenanceText(right.Content) {
		return true
	}
	return cosineSimilarity(embeddings[left.MemoryID], embeddings[right.MemoryID]) >= 0.94 && lexicalSimilarity(left, right) >= 0.5
}

func chooseCanonicalMemory(left, right Entry) (Entry, Entry) {
	leftQuality := left.Confidence*2 + left.Importance + float64(len(left.SourceMessageIDs))*0.01
	rightQuality := right.Confidence*2 + right.Importance + float64(len(right.SourceMessageIDs))*0.01
	if rightQuality > leftQuality || (rightQuality == leftQuality && right.UpdatedAt.After(left.UpdatedAt)) {
		return right, left
	}
	return left, right
}

func (s *Store) mergeDuplicateMemory(ctx context.Context, canonical, duplicate Entry) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	canonical.Tags = normalizeStrings(append(canonical.Tags, duplicate.Tags...))
	canonical.Entities = normalizeStrings(append(canonical.Entities, duplicate.Entities...))
	canonical.SourceMessageIDs = normalizeStrings(append(canonical.SourceMessageIDs, duplicate.SourceMessageIDs...))
	canonical.SourcePaths = normalizeStrings(append(canonical.SourcePaths, duplicate.SourcePaths...))
	canonical.Importance = maxMemoryFloat(canonical.Importance, duplicate.Importance)
	canonical.Confidence = maxMemoryFloat(canonical.Confidence, duplicate.Confidence)
	canonical.AccessCount += duplicate.AccessCount
	if duplicate.LastAccessedAt.After(canonical.LastAccessedAt) {
		canonical.LastAccessedAt = duplicate.LastAccessedAt
	}
	if duplicate.LastReinforcedAt.After(canonical.LastReinforcedAt) {
		canonical.LastReinforcedAt = duplicate.LastReinforcedAt
	}
	canonical.Temperature = TemperatureHot
	if canonical.SourceSessionID == "" {
		canonical.SourceSessionID = duplicate.SourceSessionID
	}
	if err := upsertEntryTx(ctx, tx, &canonical); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT session_id, message_id, role, source_path, text, start_rune, end_rune, occurred_at
		FROM memory_evidence_spans WHERE memory_id=?`, duplicate.MemoryID)
	if err != nil {
		return err
	}
	var spans []EvidenceSpan
	for rows.Next() {
		var span EvidenceSpan
		var occurredAt string
		if err := rows.Scan(&span.SessionID, &span.MessageID, &span.Role, &span.SourcePath, &span.Text, &span.StartRune, &span.EndRune, &occurredAt); err != nil {
			rows.Close()
			return err
		}
		span.MemoryID = canonical.MemoryID
		span.ScopeType = canonical.ScopeType
		span.ScopeKey = canonical.ScopeKey
		span.OccurredAt = parseTime(occurredAt)
		spans = append(spans, normalizeEvidenceSpan(span))
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, span := range spans {
		if err := upsertEvidenceSpanTx(ctx, tx, span); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_facts SET memory_id=? WHERE memory_id=?`, canonical.MemoryID, duplicate.MemoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memories SET status=?, superseded_by=?, updated_at=? WHERE memory_id=?`,
		StatusSuperseded, canonical.MemoryID, formatTime(time.Now().UTC()), duplicate.MemoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id=?`, duplicate.MemoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_chunk_fts WHERE chunk_id IN
		(SELECT chunk_id FROM memory_evidence_chunks WHERE parent_memory_id=?)`, duplicate.MemoryID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_evidence_chunks SET archived_at=?, archive_reason=?, temperature=?
		WHERE parent_memory_id=?`, formatTime(time.Now().UTC()), "superseded", TemperatureCold, duplicate.MemoryID); err != nil {
		return err
	}
	if err := upsertEdgeTx(ctx, tx, normalizeEdge(Edge{ScopeType: canonical.ScopeType, ScopeKey: canonical.ScopeKey,
		FromID: duplicate.MemoryID, ToID: canonical.MemoryID, Type: EdgeDerivedFrom, Weight: 1, Confidence: canonical.Confidence})); err != nil {
		return err
	}
	if err := insertLifecycleEventTx(ctx, tx, LifecycleEvent{ResourceKind: "memory", ResourceID: duplicate.MemoryID,
		EventType: "supersede", OldStatus: duplicate.Status, NewStatus: StatusSuperseded,
		OldTemperature: duplicate.Temperature, NewTemperature: duplicate.Temperature,
		Score: duplicate.ValueScore, Reasons: []string{"consolidated_into:" + canonical.MemoryID}, CreatedAt: time.Now().UTC()}); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) memoriesConflict(ctx context.Context, leftID, rightID string) (bool, error) {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_edges WHERE edge_type=? AND
		((from_id=? AND to_id=?) OR (from_id=? AND to_id=?)) AND (valid_until='' OR valid_until>?)`,
		EdgeContradicts, leftID, rightID, rightID, leftID, formatTime(time.Now().UTC())).Scan(&count); err != nil {
		return false, err
	}
	if count > 0 {
		return true, nil
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_facts l JOIN memory_facts r
		ON l.fact_key=r.fact_key WHERE l.memory_id=? AND r.memory_id=? AND l.object<>r.object
		AND l.status=? AND r.status=?`, leftID, rightID, StatusActive, StatusActive).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Store) linkRelatedMemories(ctx context.Context, entries []Entry, maxLinks int) (int, error) {
	byEntity := map[string][]Entry{}
	for _, entry := range entries {
		for _, entity := range normalizeStrings(entry.Entities) {
			byEntity[strings.ToLower(entity)] = append(byEntity[strings.ToLower(entity)], entry)
		}
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	seen := map[string]struct{}{}
	linked := 0
	for _, related := range byEntity {
		for left := 0; left < len(related); left++ {
			for right := left + 1; right < len(related); right++ {
				if !sameScope(related[left], related[right]) {
					continue
				}
				key := related[left].MemoryID + "\x00" + related[right].MemoryID
				if related[left].MemoryID > related[right].MemoryID {
					key = related[right].MemoryID + "\x00" + related[left].MemoryID
				}
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				if err := upsertEdgeTx(ctx, tx, normalizeEdge(Edge{ScopeType: related[left].ScopeType,
					ScopeKey: related[left].ScopeKey, FromID: related[left].MemoryID, ToID: related[right].MemoryID,
					Type: EdgeRelatedTo, Weight: 0.65, Confidence: minFloat(related[left].Confidence, related[right].Confidence)})); err != nil {
					return 0, err
				}
				linked++
				if linked >= maxLinks {
					return linked, tx.Commit()
				}
			}
		}
	}
	return linked, tx.Commit()
}

func (s *Store) promoteRepeatedProcedures(ctx context.Context, entries []Entry) (int, error) {
	groups := map[string][]Entry{}
	for _, entry := range entries {
		if entry.MemoryType != TypeEpisodic || !hasSuccessTag(entry.Tags) {
			continue
		}
		key := string(entry.ScopeType) + "\x00" + entry.ScopeKey + "\x00" + normalizeMaintenanceText(entry.Title)
		groups[key] = append(groups[key], entry)
	}
	promoted := 0
	for _, group := range groups {
		sessions := map[string]struct{}{}
		for _, entry := range group {
			if entry.SourceSessionID != "" {
				sessions[entry.SourceSessionID] = struct{}{}
			}
		}
		if len(group) < 3 || len(sessions) < 3 {
			continue
		}
		first := group[0]
		var evidence []string
		var messageIDs, paths, entities []string
		for _, entry := range group {
			evidence = append(evidence, firstNonEmpty(entry.Summary, entry.Content))
			messageIDs = append(messageIDs, entry.SourceMessageIDs...)
			paths = append(paths, entry.SourcePaths...)
			entities = append(entities, entry.Entities...)
		}
		candidate := Candidate{ScopeType: first.ScopeType, ScopeKey: first.ScopeKey, MemoryType: TypeProcedural,
			Title:   "Repeated successful procedure: " + first.Title,
			Summary: "A procedure confirmed by successful outcomes in multiple sessions.",
			Content: strings.Join(normalizeStrings(evidence), "\n"), Tags: []string{"procedure", "promoted", "verified"},
			Entities: normalizeStrings(entities), Importance: 0.8, Confidence: 0.8,
			SourceSessionID: first.SourceSessionID, SourceMessageIDs: normalizeStrings(messageIDs), SourcePaths: normalizeStrings(paths),
			Status: StatusActive}
		procedure, err := s.Upsert(ctx, candidate)
		if err != nil {
			return promoted, err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return promoted, err
		}
		for _, entry := range group {
			if err := upsertEdgeTx(ctx, tx, normalizeEdge(Edge{ScopeType: first.ScopeType, ScopeKey: first.ScopeKey,
				FromID: entry.MemoryID, ToID: procedure.MemoryID, Type: EdgeSupports, Weight: 0.8, Confidence: entry.Confidence})); err != nil {
				_ = tx.Rollback()
				return promoted, err
			}
		}
		if err := tx.Commit(); err != nil {
			return promoted, err
		}
		promoted++
	}
	return promoted, nil
}

func sameScope(left, right Entry) bool {
	return left.ScopeType == right.ScopeType && left.ScopeKey == right.ScopeKey && left.MemoryID != right.MemoryID
}

func normalizeMaintenanceText(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func hasSuccessTag(tags []string) bool {
	for _, tag := range tags {
		switch strings.ToLower(strings.TrimSpace(tag)) {
		case "success", "successful", "completed", "verified", "test-pass", "test_pass":
			return true
		}
	}
	return false
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func maxMemoryFloat(left, right float64) float64 {
	if left > right {
		return left
	}
	return right
}

func (s *Store) enrichLegacyMemory(ctx context.Context, entry Entry) (bool, error) {
	var spanCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_evidence_spans WHERE memory_id=?`, entry.MemoryID).Scan(&spanCount); err != nil {
		return false, err
	}
	if spanCount > 0 {
		return false, nil
	}
	text := strings.TrimSpace(entry.Content)
	if text == "" {
		text = entry.Summary
	}
	if text == "" {
		return false, nil
	}
	span := normalizeEvidenceSpan(EvidenceSpan{MemoryID: entry.MemoryID, ScopeType: entry.ScopeType,
		ScopeKey: entry.ScopeKey, SessionID: entry.SourceSessionID, Text: text, OccurredAt: entry.ValidFrom})
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO memory_evidence_spans(span_id, memory_id, scope_type, scope_key,
		session_id, message_id, role, source_path, text, start_rune, end_rune, occurred_at, content_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, span.SpanID, span.MemoryID, span.ScopeType, span.ScopeKey,
		span.SessionID, span.MessageID, span.Role, firstString(entry.SourcePaths), span.Text, span.StartRune, span.EndRune,
		formatTime(span.OccurredAt), span.ContentHash)
	return err == nil, err
}

func normalizeJob(job Job) Job {
	job.Kind = strings.TrimSpace(job.Kind)
	job.ScopeType = normalizeScopeType(job.ScopeType)
	job.ScopeKey = defaultScopeKey(job.ScopeKey)
	if job.Payload == "" {
		job.Payload = "{}"
	}
	if job.Status == "" {
		job.Status = "pending"
	}
	now := time.Now().UTC()
	if job.AvailableAt.IsZero() {
		job.AvailableAt = now
	}
	if job.CreatedAt.IsZero() {
		job.CreatedAt = now
	}
	job.UpdatedAt = now
	if job.JobID == "" {
		job.JobID = StableID(job.ScopeType, job.ScopeKey, job.Kind, job.Payload)
	}
	return job
}

func scanJobs(rows *sql.Rows) ([]Job, error) {
	var jobs []Job
	for rows.Next() {
		var job Job
		var availableAt, createdAt, updatedAt string
		if err := rows.Scan(&job.JobID, &job.Kind, &job.ScopeType, &job.ScopeKey, &job.Payload, &job.Status,
			&job.Attempts, &job.LastError, &availableAt, &createdAt, &updatedAt); err != nil {
			return nil, err
		}
		job.AvailableAt = parseTime(availableAt)
		job.CreatedAt = parseTime(createdAt)
		job.UpdatedAt = parseTime(updatedAt)
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func firstString(values []string) string {
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func (r MaintenanceResult) String() string {
	return fmt.Sprintf("embedded=%d chunk_embedded=%d atom_embedded=%d session_embedded=%d enriched=%d consolidated=%d linked=%d promoted=%d archived=%d",
		r.Embedded, r.ChunkEmbedded, r.AtomEmbedded, r.SessionEmbedded, r.Enriched, r.Consolidated, r.Linked, r.Promoted, r.Archived)
}
