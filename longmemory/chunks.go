package longmemory

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/viant/sqlite-vec/vector"
)

const (
	chunkTargetRunes  = 768
	chunkOverlapRunes = 192
)

func BuildEvidenceChunks(span EvidenceSpan) []EvidenceChunk {
	text := strings.TrimSpace(span.Text)
	if text == "" {
		return nil
	}
	span = normalizeEvidenceSpan(span)
	runes := []rune(text)
	var chunks []EvidenceChunk
	for start := 0; start < len(runes); {
		end := minInt(start+chunkTargetRunes, len(runes))
		if end < len(runes) {
			end = chunkBoundary(runes, start, end)
		}
		if end <= start {
			end = minInt(start+chunkTargetRunes, len(runes))
		}
		chunkText := strings.TrimSpace(string(runes[start:end]))
		if chunkText != "" {
			absoluteStart := span.StartRune + start
			absoluteEnd := span.StartRune + end
			contentHash := StableID(span.ScopeType, span.ScopeKey, "chunk-content", chunkText)
			chunkID := StableID(span.ScopeType, span.ScopeKey, "chunk:"+span.SpanID,
				strings.Join([]string{span.MessageID, itoa(absoluteStart), itoa(absoluteEnd), contentHash}, "\x00"))
			chunks = append(chunks, EvidenceChunk{
				ChunkID: chunkID, SpanID: span.SpanID, ParentMemoryID: span.MemoryID,
				ScopeType: span.ScopeType, ScopeKey: span.ScopeKey, SessionID: span.SessionID,
				MessageID: span.MessageID, Role: span.Role, Text: chunkText,
				StartRune: absoluteStart, EndRune: absoluteEnd, OccurredAt: span.OccurredAt,
				ValidFrom: span.OccurredAt, ContentHash: contentHash,
			})
		}
		if end >= len(runes) {
			break
		}
		next := end - chunkOverlapRunes
		if next <= start {
			next = end
		}
		start = next
	}
	return chunks
}

func chunkBoundary(runes []rune, start, target int) int {
	minimum := start + chunkTargetRunes/2
	for index := target; index > minimum; index-- {
		if isChunkBoundary(runes[index-1]) {
			return index
		}
	}
	return target
}

func isChunkBoundary(value rune) bool {
	return value == '\n' || value == '.' || value == '?' || value == '!' || value == ';' ||
		value == '。' || value == '？' || value == '！' || value == '；' || unicode.IsSpace(value)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits [20]byte
	position := len(digits)
	for value > 0 {
		position--
		digits[position] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		position--
		digits[position] = '-'
	}
	return string(digits[position:])
}

func upsertEvidenceChunkTx(ctx context.Context, tx *sql.Tx, chunk EvidenceChunk) error {
	if strings.TrimSpace(chunk.ChunkID) == "" || strings.TrimSpace(chunk.Text) == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_evidence_chunks(chunk_id, span_id, parent_memory_id,
		scope_type, scope_key, session_id, message_id, role, text, start_rune, end_rune, occurred_at,
		valid_from, valid_until, content_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chunk_id) DO UPDATE SET parent_memory_id=excluded.parent_memory_id, role=excluded.role,
		text=excluded.text, occurred_at=excluded.occurred_at, valid_from=excluded.valid_from,
		valid_until=excluded.valid_until, content_hash=excluded.content_hash`,
		chunk.ChunkID, chunk.SpanID, chunk.ParentMemoryID, chunk.ScopeType, chunk.ScopeKey, chunk.SessionID,
		chunk.MessageID, chunk.Role, chunk.Text, chunk.StartRune, chunk.EndRune, formatTime(chunk.OccurredAt),
		formatTime(chunk.ValidFrom), formatTime(chunk.ValidUntil), chunk.ContentHash)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_chunk_fts WHERE chunk_id=?`, chunk.ChunkID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_chunk_fts(chunk_id, session_id, message_id, role, text)
		VALUES (?, ?, ?, ?, ?)`, chunk.ChunkID, chunk.SessionID, chunk.MessageID, chunk.Role, chunk.Text)
	return err
}

func upsertChunkEmbeddingTx(ctx context.Context, tx *sql.Tx, chunkID string, embedding MemoryEmbedding) error {
	if strings.TrimSpace(chunkID) == "" || strings.TrimSpace(embedding.Model) == "" || len(embedding.Vector) == 0 {
		return nil
	}
	blob, err := vector.EncodeEmbedding(embedding.Vector)
	if err != nil {
		return err
	}
	now := formatTime(time.Now().UTC())
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_chunk_embeddings(chunk_id, model, dimensions, content_hash,
		embedding, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chunk_id, model) DO UPDATE SET dimensions=excluded.dimensions,
		content_hash=excluded.content_hash, embedding=excluded.embedding, updated_at=excluded.updated_at`,
		chunkID, embedding.Model, len(embedding.Vector), embedding.ContentHash, blob, now, now)
	return err
}

func upsertChunkEntityTx(ctx context.Context, tx *sql.Tx, chunk EvidenceChunk, entity string, confidence float64) error {
	normalized := normalizeEntityValue(entity)
	if normalized == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_chunk_entities(chunk_id, scope_type, scope_key,
		normalized_entity, original_text, entity_type, confidence) VALUES (?, ?, ?, ?, ?, '', ?)
		ON CONFLICT(chunk_id, normalized_entity) DO UPDATE SET original_text=excluded.original_text,
		confidence=excluded.confidence`, chunk.ChunkID, chunk.ScopeType, chunk.ScopeKey, normalized, entity, clamp01(confidence))
	return err
}

func chunkEntry(chunk EvidenceChunk, score float64, reason string) Entry {
	return Entry{
		MemoryID: chunk.ChunkID, ScopeType: chunk.ScopeType, ScopeKey: chunk.ScopeKey,
		MemoryType: TypeEpisodic, Title: "Message " + chunk.MessageID, Content: chunk.Text,
		Summary: chunk.Text, SourceSessionID: chunk.SessionID, SourceMessageIDs: []string{chunk.MessageID},
		CreatedAt: chunk.OccurredAt, UpdatedAt: chunk.OccurredAt, ValidFrom: chunk.ValidFrom,
		ValidUntil: chunk.ValidUntil, Status: StatusActive, Score: score, MatchReason: reason,
		DocumentKind: "chunk", ParentID: chunk.ParentMemoryID, MessageID: chunk.MessageID,
		Role: chunk.Role, OccurredAt: chunk.OccurredAt,
	}
}

func scanChunk(scanner interface{ Scan(...any) error }) (EvidenceChunk, float64, error) {
	var chunk EvidenceChunk
	var occurredAt, validFrom, validUntil string
	var score float64
	err := scanner.Scan(&chunk.ChunkID, &chunk.SpanID, &chunk.ParentMemoryID, &chunk.ScopeType,
		&chunk.ScopeKey, &chunk.SessionID, &chunk.MessageID, &chunk.Role, &chunk.Text, &chunk.StartRune,
		&chunk.EndRune, &occurredAt, &validFrom, &validUntil, &chunk.ContentHash, &score)
	chunk.OccurredAt = parseTime(occurredAt)
	chunk.ValidFrom = parseTime(validFrom)
	chunk.ValidUntil = parseTime(validUntil)
	return chunk, score, err
}

const chunkColumns = `c.chunk_id, c.span_id, c.parent_memory_id, c.scope_type, c.scope_key,
	c.session_id, c.message_id, c.role, c.text, c.start_rune, c.end_rune, c.occurred_at,
	c.valid_from, c.valid_until, c.content_hash`

func (s *Store) SearchChunkBM25(ctx context.Context, queries []string, scopes []Scope, limit int) ([]Entry, error) {
	return s.searchChunkText(ctx, queries, nil, scopes, limit, "bm25")
}

func (s *Store) searchChunkText(ctx context.Context, queries, extraTerms []string, scopes []Scope, limit int, reason string) ([]Entry, error) {
	if limit <= 0 {
		limit = 40
	}
	best := map[string]Entry{}
	for _, query := range normalizeStrings(append(append([]string(nil), queries...), extraTerms...)) {
		ftsQuery := sanitizeFTSQuery(query)
		if ftsQuery == "" {
			continue
		}
		scopeSQL, scopeArgs := scopedClauses(scopes, "c.")
		clauses := []string{"memory_chunk_fts MATCH ?"}
		args := []any{ftsQuery}
		if scopeSQL != "" {
			clauses = append(clauses, "("+scopeSQL+")")
			args = append(args, scopeArgs...)
		}
		args = append(args, limit)
		rows, err := s.db.QueryContext(ctx, `SELECT `+chunkColumns+`, bm25(memory_chunk_fts)
			FROM memory_chunk_fts JOIN memory_evidence_chunks c ON c.chunk_id=memory_chunk_fts.chunk_id
			WHERE `+strings.Join(clauses, " AND ")+` ORDER BY bm25(memory_chunk_fts) LIMIT ?`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			chunk, rank, scanErr := scanChunk(rows)
			if scanErr != nil {
				rows.Close()
				return nil, scanErr
			}
			entry := chunkEntry(chunk, -rank, reason)
			if prior, ok := best[entry.MemoryID]; !ok || entry.Score > prior.Score {
				best[entry.MemoryID] = entry
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return sortedChunkEntries(best, limit), nil
}

func (s *Store) SearchChunkEntities(ctx context.Context, queries, entities []string, scopes []Scope, limit int) ([]Entry, error) {
	entries, err := s.searchChunkText(ctx, queries, entities, scopes, limit, "entity")
	if err != nil {
		return nil, err
	}
	best := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		best[entry.MemoryID] = entry
	}
	normalized := make([]string, 0, len(entities))
	for _, entity := range normalizeStrings(entities) {
		if value := normalizeEntityValue(entity); value != "" {
			normalized = append(normalized, value)
		}
	}
	if len(normalized) == 0 {
		return sortedChunkEntries(best, limit), nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(normalized)), ",")
	args := make([]any, 0, len(normalized)+len(scopes)*2+1)
	for _, entity := range normalized {
		args = append(args, entity)
	}
	where := "ce.normalized_entity IN (" + marks + ")"
	if scopeSQL, scopeArgs := scopedClauses(scopes, "c."); scopeSQL != "" {
		where += " AND (" + scopeSQL + ")"
		args = append(args, scopeArgs...)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT `+chunkColumns+`, ce.confidence
		FROM memory_chunk_entities ce JOIN memory_evidence_chunks c ON c.chunk_id=ce.chunk_id
		WHERE `+where+` ORDER BY ce.confidence DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		chunk, score, scanErr := scanChunk(rows)
		if scanErr != nil {
			rows.Close()
			return nil, scanErr
		}
		entry := chunkEntry(chunk, score+1, "entity")
		if prior, ok := best[entry.MemoryID]; !ok || entry.Score > prior.Score {
			best[entry.MemoryID] = entry
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return sortedChunkEntries(best, limit), nil
}

func (s *Store) SearchChunkTemporal(ctx context.Context, queries []string, constraints []TemporalConstraint, scopes []Scope, limit int) ([]Entry, error) {
	entries, err := s.searchChunkText(ctx, queries, nil, scopes, maxInt(limit*2, 40), "temporal")
	if err != nil {
		return nil, err
	}
	best := make(map[string]Entry, len(entries))
	for _, entry := range entries {
		if temporalEntryAllowed(entry, constraints) {
			best[entry.MemoryID] = entry
		}
	}
	if len(constraints) > 0 {
		scopeSQL, scopeArgs := scopedClauses(scopes, "c.")
		query := `SELECT ` + chunkColumns + `, 0 FROM memory_evidence_chunks c`
		if scopeSQL != "" {
			query += " WHERE " + scopeSQL
		}
		query += " ORDER BY c.occurred_at DESC LIMIT ?"
		args := append(scopeArgs, maxInt(limit*8, 160))
		rows, queryErr := s.db.QueryContext(ctx, query, args...)
		if queryErr != nil {
			return nil, queryErr
		}
		for rows.Next() {
			chunk, _, scanErr := scanChunk(rows)
			if scanErr != nil {
				rows.Close()
				return nil, scanErr
			}
			entry := chunkEntry(chunk, 1, "temporal")
			if temporalEntryAllowed(entry, constraints) {
				best[entry.MemoryID] = entry
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	filtered := make([]Entry, 0, len(best))
	for _, entry := range best {
		filtered = append(filtered, entry)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if filtered[i].ValidFrom.Equal(filtered[j].ValidFrom) {
			return filtered[i].Score > filtered[j].Score
		}
		return filtered[i].ValidFrom.After(filtered[j].ValidFrom)
	})
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

func (s *Store) chunksForSessions(ctx context.Context, sessions []sessionRank, queries []string, scopes []Scope, limit int) ([]Entry, error) {
	if len(sessions) == 0 {
		return nil, nil
	}
	allowed := map[string]float64{}
	for _, session := range sessions {
		allowed[sessionScopeKey(session.ScopeType, session.ScopeKey, session.SessionID)] = session.Rank
	}
	entries, err := s.SearchChunkBM25(ctx, queries, scopes, maxInt(limit*4, 40))
	if err != nil {
		return nil, err
	}
	filtered := entries[:0]
	for _, entry := range entries {
		rank, ok := allowed[sessionScopeKey(entry.ScopeType, entry.ScopeKey, entry.SourceSessionID)]
		if !ok {
			continue
		}
		entry.Score += rank
		entry.MatchReason = "session"
		filtered = append(filtered, entry)
	}
	if len(filtered) == 0 {
		var clauses []string
		var args []any
		for _, session := range sessions {
			clauses = append(clauses, "(c.scope_type=? AND c.scope_key=? AND c.session_id=?)")
			args = append(args, session.ScopeType, session.ScopeKey, session.SessionID)
		}
		args = append(args, limit)
		rows, queryErr := s.db.QueryContext(ctx, `SELECT `+chunkColumns+`, 0 FROM memory_evidence_chunks c
			WHERE (`+strings.Join(clauses, " OR ")+`) ORDER BY c.occurred_at LIMIT ?`, args...)
		if queryErr != nil {
			return nil, queryErr
		}
		for rows.Next() {
			chunk, _, scanErr := scanChunk(rows)
			if scanErr != nil {
				rows.Close()
				return nil, scanErr
			}
			entry := chunkEntry(chunk, allowed[sessionScopeKey(chunk.ScopeType, chunk.ScopeKey, chunk.SessionID)], "session")
			filtered = append(filtered, entry)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool { return filtered[i].Score > filtered[j].Score })
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

func (s *Store) SearchChunkVector(ctx context.Context, embedding []float32, model string, scopes []Scope, limit int) ([]Entry, error) {
	if len(embedding) == 0 || strings.TrimSpace(model) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 40
	}
	blob, err := vector.EncodeEmbedding(embedding)
	if err != nil {
		return nil, err
	}
	scopeSQL, scopeArgs := scopedClauses(scopes, "c.")
	clauses := []string{"e.model=?", "e.dimensions=?"}
	args := []any{model, len(embedding)}
	if scopeSQL != "" {
		clauses = append(clauses, "("+scopeSQL+")")
		args = append(args, scopeArgs...)
	}
	args = append(args, blob, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT `+chunkColumns+`, vec_cosine(e.embedding, ?)
		FROM memory_chunk_embeddings e JOIN memory_evidence_chunks c ON c.chunk_id=e.chunk_id
		WHERE `+strings.Join(clauses, " AND ")+` ORDER BY vec_cosine(e.embedding, ?) DESC LIMIT ?`,
		reorderChunkVectorArgs(args)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		chunk, score, scanErr := scanChunk(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		entries = append(entries, chunkEntry(chunk, score, "vector"))
	}
	return entries, rows.Err()
}

func (s *Store) LoadDocumentEmbeddings(ctx context.Context, documentIDs []string, model string) (map[string][]float32, error) {
	result, err := s.LoadEmbeddings(ctx, documentIDs, model)
	if err != nil || len(documentIDs) == 0 || strings.TrimSpace(model) == "" {
		return result, err
	}
	missing := make([]string, 0, len(documentIDs))
	for _, documentID := range documentIDs {
		if _, ok := result[documentID]; !ok {
			missing = append(missing, documentID)
		}
	}
	if len(missing) == 0 {
		return result, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(missing)), ",")
	args := make([]any, 0, len(missing)+1)
	args = append(args, model)
	for _, documentID := range missing {
		args = append(args, documentID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT chunk_id, embedding FROM memory_chunk_embeddings
		WHERE model=? AND chunk_id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var documentID string
		var blob []byte
		if err := rows.Scan(&documentID, &blob); err != nil {
			return nil, err
		}
		embedding, err := vector.DecodeEmbedding(blob)
		if err != nil {
			return nil, err
		}
		result[documentID] = embedding
	}
	return result, rows.Err()
}

func reorderChunkVectorArgs(args []any) []any {
	if len(args) < 4 {
		return args
	}
	blob := args[len(args)-2]
	limit := args[len(args)-1]
	base := append([]any(nil), args[:len(args)-2]...)
	return append([]any{blob}, append(base, blob, limit)...)
}

func sortedChunkEntries(values map[string]Entry, limit int) []Entry {
	entries := make([]Entry, 0, len(values))
	for _, entry := range values {
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Score == entries[j].Score {
			return entries[i].MemoryID < entries[j].MemoryID
		}
		return entries[i].Score > entries[j].Score
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries
}

func (s *Store) GetChunks(ctx context.Context, ids []string) ([]EvidenceChunk, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for index := range ids {
		args[index] = ids[index]
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+chunkColumns+`, 0 FROM memory_evidence_chunks c
		WHERE c.chunk_id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chunks []EvidenceChunk
	for rows.Next() {
		chunk, _, scanErr := scanChunk(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		chunks = append(chunks, chunk)
	}
	return chunks, rows.Err()
}

func (s *Store) ChunksByMessageIDs(ctx context.Context, scopes []Scope, messageIDs []string) ([]EvidenceChunk, error) {
	messageIDs = normalizeStrings(messageIDs)
	if len(messageIDs) == 0 {
		return nil, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(messageIDs)), ",")
	args := make([]any, 0, len(messageIDs)+len(scopes)*2)
	for _, messageID := range messageIDs {
		args = append(args, messageID)
	}
	where := "c.message_id IN (" + marks + ")"
	if scopeSQL, scopeArgs := scopedClauses(scopes, "c."); scopeSQL != "" {
		where += " AND (" + scopeSQL + ")"
		args = append(args, scopeArgs...)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+chunkColumns+`, 0 FROM memory_evidence_chunks c
		WHERE `+where+` ORDER BY c.session_id, c.message_id, c.start_rune`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chunks []EvidenceChunk
	for rows.Next() {
		chunk, _, scanErr := scanChunk(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		chunks = append(chunks, chunk)
	}
	return chunks, rows.Err()
}

func (s *Store) NeighborChunks(ctx context.Context, chunk EvidenceChunk, radius int) ([]EvidenceChunk, error) {
	if radius <= 0 || chunk.MessageID == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+chunkColumns+`, 0 FROM memory_evidence_chunks c
		WHERE c.scope_type=? AND c.scope_key=? AND c.session_id=? AND c.message_id=?
		ORDER BY c.start_rune`, chunk.ScopeType, chunk.ScopeKey, chunk.SessionID, chunk.MessageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []EvidenceChunk
	selected := -1
	for rows.Next() {
		current, _, scanErr := scanChunk(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if current.ChunkID == chunk.ChunkID {
			selected = len(all)
		}
		all = append(all, current)
	}
	if selected < 0 {
		return nil, rows.Err()
	}
	start, end := maxInt(0, selected-radius), minInt(len(all), selected+radius+1)
	return append([]EvidenceChunk(nil), all[start:end]...), rows.Err()
}

func (s *Store) BackfillEvidenceChunks(ctx context.Context, limit int) (int, error) {
	query := `SELECT s.span_id, s.memory_id, s.scope_type, s.scope_key, s.session_id, s.message_id,
		s.role, s.source_path, s.text, s.start_rune, s.end_rune, s.occurred_at, s.content_hash
		FROM memory_evidence_spans s
		LEFT JOIN memory_evidence_chunks c ON c.span_id=s.span_id
		LEFT JOIN memory_session_index si ON si.index_id=s.memory_id
		WHERE c.chunk_id IS NULL
		ORDER BY s.occurred_at, CASE WHEN si.index_id IS NULL THEN 1 ELSE 0 END`
	var args []any
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	var spans []EvidenceSpan
	for rows.Next() {
		var span EvidenceSpan
		var occurredAt string
		if err := rows.Scan(&span.SpanID, &span.MemoryID, &span.ScopeType, &span.ScopeKey, &span.SessionID,
			&span.MessageID, &span.Role, &span.SourcePath, &span.Text, &span.StartRune, &span.EndRune,
			&occurredAt, &span.ContentHash); err != nil {
			rows.Close()
			return 0, err
		}
		span.OccurredAt = parseTime(occurredAt)
		spans = append(spans, span)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(spans) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	count := 0
	for _, span := range spans {
		for _, chunk := range BuildEvidenceChunks(span) {
			canonicalID, lookupErr := existingCanonicalChunkIDTx(ctx, tx, chunk)
			if lookupErr != nil {
				return count, lookupErr
			}
			if canonicalID == "" {
				if err := upsertEvidenceChunkTx(ctx, tx, chunk); err != nil {
					return count, err
				}
				canonicalID = chunk.ChunkID
				count++
			}
			if err := upsertEdgeTx(ctx, tx, normalizeEdge(Edge{ScopeType: chunk.ScopeType, ScopeKey: chunk.ScopeKey,
				FromID: chunk.ParentMemoryID, ToID: canonicalID, Type: EdgeContains, Weight: 1, Confidence: 1})); err != nil {
				return count, err
			}
		}
	}
	return count, tx.Commit()
}

func existingCanonicalChunkIDTx(ctx context.Context, tx *sql.Tx, chunk EvidenceChunk) (string, error) {
	var chunkID string
	err := tx.QueryRowContext(ctx, `SELECT chunk_id FROM memory_evidence_chunks
		WHERE scope_type=? AND scope_key=? AND session_id=? AND message_id=?
		AND start_rune=? AND end_rune=? AND content_hash=? LIMIT 1`, chunk.ScopeType, chunk.ScopeKey,
		chunk.SessionID, chunk.MessageID, chunk.StartRune, chunk.EndRune, chunk.ContentHash).Scan(&chunkID)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return chunkID, err
}

func (s *Store) ChunksMissingEmbedding(ctx context.Context, model string, limit int) ([]EvidenceChunk, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+chunkColumns+`, 0 FROM memory_evidence_chunks c
		LEFT JOIN memory_chunk_embeddings e ON e.chunk_id=c.chunk_id AND e.model=?
		WHERE e.chunk_id IS NULL ORDER BY c.occurred_at LIMIT ?`, model, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var chunks []EvidenceChunk
	for rows.Next() {
		chunk, _, scanErr := scanChunk(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		chunks = append(chunks, chunk)
	}
	return chunks, rows.Err()
}

func (s *Store) UpsertChunkEmbedding(ctx context.Context, chunkID, model, contentHash string, embedding []float32) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertChunkEmbeddingTx(ctx, tx, chunkID, MemoryEmbedding{Model: model, ContentHash: contentHash, Vector: embedding}); err != nil {
		return err
	}
	return tx.Commit()
}
