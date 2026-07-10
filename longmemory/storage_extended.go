package longmemory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/viant/sqlite-vec/vector"
	"gonum.org/v1/gonum/graph/simple"
)

func (s *Store) migrateMemoryStorage(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS memory_schema (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS memory_episodes (
			episode_id TEXT PRIMARY KEY,
			scope_type TEXT NOT NULL,
			scope_key TEXT NOT NULL,
			session_id TEXT NOT NULL DEFAULT '',
			team_session_id TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT '',
			message_ids_json TEXT NOT NULL DEFAULT '[]',
			kind TEXT NOT NULL DEFAULT 'conversation',
			content TEXT NOT NULL,
			occurred_at TEXT NOT NULL DEFAULT '',
			observed_at TEXT NOT NULL,
			content_hash TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS memory_facts (
			fact_id TEXT PRIMARY KEY,
			memory_id TEXT NOT NULL DEFAULT '',
			scope_type TEXT NOT NULL,
			scope_key TEXT NOT NULL,
			subject TEXT NOT NULL,
			predicate TEXT NOT NULL,
			object TEXT NOT NULL,
			qualifiers_json TEXT NOT NULL DEFAULT '{}',
			fact_key TEXT NOT NULL,
			confidence REAL NOT NULL DEFAULT 0.5,
			valid_from TEXT NOT NULL DEFAULT '',
			valid_until TEXT NOT NULL DEFAULT '',
			observed_at TEXT NOT NULL,
			invalidated_at TEXT NOT NULL DEFAULT '',
			superseded_by TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active'
		);`,
		`CREATE TABLE IF NOT EXISTS memory_session_index (
			index_id TEXT PRIMARY KEY,
			scope_type TEXT NOT NULL,
			scope_key TEXT NOT NULL,
			session_id TEXT NOT NULL,
			summary TEXT NOT NULL DEFAULT '',
			keyphrases_json TEXT NOT NULL DEFAULT '[]',
			entities_json TEXT NOT NULL DEFAULT '[]',
			roles_json TEXT NOT NULL DEFAULT '[]',
			started_at TEXT NOT NULL DEFAULT '',
			ended_at TEXT NOT NULL DEFAULT '',
			content_hash TEXT NOT NULL,
			UNIQUE(scope_type, scope_key, session_id)
		);`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memory_session_fts USING fts5(index_id UNINDEXED, session_id UNINDEXED, summary, keyphrases, entities);`,
		`CREATE TABLE IF NOT EXISTS memory_entities (
			memory_id TEXT NOT NULL,
			scope_type TEXT NOT NULL,
			scope_key TEXT NOT NULL,
			normalized_entity TEXT NOT NULL,
			original_text TEXT NOT NULL,
			entity_type TEXT NOT NULL DEFAULT '',
			confidence REAL NOT NULL DEFAULT 0.5,
			PRIMARY KEY(memory_id, normalized_entity)
		);`,
		`CREATE TABLE IF NOT EXISTS memory_edges (
			edge_id TEXT PRIMARY KEY,
			scope_type TEXT NOT NULL,
			scope_key TEXT NOT NULL,
			from_id TEXT NOT NULL,
			to_id TEXT NOT NULL,
			edge_type TEXT NOT NULL,
			weight REAL NOT NULL DEFAULT 1,
			confidence REAL NOT NULL DEFAULT 0.5,
			created_at TEXT NOT NULL,
			valid_until TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE TABLE IF NOT EXISTS memory_embeddings (
			memory_id TEXT NOT NULL,
			model TEXT NOT NULL,
			dimensions INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			embedding BLOB NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(memory_id, model)
		);`,
		`CREATE TABLE IF NOT EXISTS memory_core_blocks (
			block_id TEXT PRIMARY KEY,
			scope_type TEXT NOT NULL,
			scope_key TEXT NOT NULL,
			label TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL,
			read_only INTEGER NOT NULL DEFAULT 0,
			generation INTEGER NOT NULL DEFAULT 1,
			updated_at TEXT NOT NULL,
			UNIQUE(scope_type, scope_key, label)
		);`,
		`CREATE TABLE IF NOT EXISTS memory_evidence_spans (
			span_id TEXT PRIMARY KEY,
			memory_id TEXT NOT NULL,
			scope_type TEXT NOT NULL,
			scope_key TEXT NOT NULL,
			session_id TEXT NOT NULL DEFAULT '',
			message_id TEXT NOT NULL DEFAULT '',
			source_path TEXT NOT NULL DEFAULT '',
			text TEXT NOT NULL,
			start_rune INTEGER NOT NULL DEFAULT 0,
			end_rune INTEGER NOT NULL DEFAULT 0,
			occurred_at TEXT NOT NULL DEFAULT '',
			content_hash TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS memory_jobs (
			job_id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			scope_type TEXT NOT NULL,
			scope_key TEXT NOT NULL,
			payload TEXT NOT NULL DEFAULT '{}',
			status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0,
			last_error TEXT NOT NULL DEFAULT '',
			available_at TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS memory_cursors (
			consumer_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			last_message_id TEXT NOT NULL DEFAULT '',
			last_message_index INTEGER NOT NULL DEFAULT -1,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(consumer_id, session_id)
		);`,
		`CREATE TABLE IF NOT EXISTS memory_retrieval_runs (
			run_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL DEFAULT '',
			team_session_id TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT '',
			query TEXT NOT NULL,
			plan_json TEXT NOT NULL,
			selected_ids_json TEXT NOT NULL DEFAULT '[]',
			estimated_tokens INTEGER NOT NULL DEFAULT 0,
			duration_ms INTEGER NOT NULL DEFAULT 0,
			error TEXT NOT NULL DEFAULT '',
			run_json TEXT NOT NULL DEFAULT '{}',
			created_at TEXT NOT NULL
		);`,
		`CREATE TABLE IF NOT EXISTS memory_retrieval_items (
			run_id TEXT NOT NULL,
			memory_id TEXT NOT NULL,
			channel_ranks_json TEXT NOT NULL DEFAULT '{}',
			channel_scores_json TEXT NOT NULL DEFAULT '{}',
			fused_score REAL NOT NULL DEFAULT 0,
			graph_score REAL NOT NULL DEFAULT 0,
			selected INTEGER NOT NULL DEFAULT 0,
			drop_reason TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(run_id, memory_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_memory_episodes_scope ON memory_episodes(scope_type, scope_key, occurred_at);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_episodes_hash ON memory_episodes(scope_type, scope_key, content_hash);`,
		`CREATE INDEX IF NOT EXISTS idx_memory_facts_key ON memory_facts(scope_type, scope_key, fact_key, status);`,
		`CREATE INDEX IF NOT EXISTS idx_memory_facts_time ON memory_facts(valid_from, valid_until, observed_at);`,
		`CREATE INDEX IF NOT EXISTS idx_memory_session_scope ON memory_session_index(scope_type, scope_key, started_at, ended_at);`,
		`CREATE INDEX IF NOT EXISTS idx_memory_entities_lookup ON memory_entities(scope_type, scope_key, normalized_entity);`,
		`CREATE INDEX IF NOT EXISTS idx_memory_edges_from ON memory_edges(scope_type, scope_key, from_id, edge_type);`,
		`CREATE INDEX IF NOT EXISTS idx_memory_edges_to ON memory_edges(scope_type, scope_key, to_id, edge_type);`,
		`CREATE INDEX IF NOT EXISTS idx_memory_spans_memory ON memory_evidence_spans(memory_id, occurred_at);`,
		`CREATE INDEX IF NOT EXISTS idx_memory_jobs_ready ON memory_jobs(status, available_at);`,
		`CREATE INDEX IF NOT EXISTS idx_memory_retrieval_created ON memory_retrieval_runs(created_at);`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate long-term memory storage: %w", err)
		}
	}
	if err := ensureTableColumn(ctx, s.db, "memory_retrieval_runs", "run_json", "TEXT NOT NULL DEFAULT '{}'"); err != nil {
		return fmt.Errorf("migrate retrieval run diagnostics: %w", err)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO memory_schema(key, value, updated_at) VALUES ('extended_storage', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		"complete", formatTime(time.Now().UTC()))
	return err
}

func (s *Store) AppendEpisode(ctx context.Context, episode Episode) (*Episode, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("memory store is closed")
	}
	episode = normalizeEpisode(episode)
	_, err := s.db.ExecContext(ctx, `INSERT INTO memory_episodes(
		episode_id, scope_type, scope_key, session_id, team_session_id, agent_id, message_ids_json,
		kind, content, occurred_at, observed_at, content_hash
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(episode_id) DO UPDATE SET message_ids_json=excluded.message_ids_json, kind=excluded.kind,
		content=excluded.content, occurred_at=excluded.occurred_at, observed_at=excluded.observed_at,
		content_hash=excluded.content_hash`, episode.EpisodeID, episode.ScopeType, episode.ScopeKey,
		episode.SessionID, episode.TeamSessionID, episode.AgentID, toJSON(episode.MessageIDs), episode.Kind,
		episode.Content, formatTime(episode.OccurredAt), formatTime(episode.ObservedAt), episode.ContentHash)
	if err != nil {
		return nil, err
	}
	return &episode, nil
}

// CommitExtraction atomically commits one incremental extraction window. If any
// memory, fact, provenance span, edge, or core block fails, none of the window is
// advanced and the message cursor can be retried after restart.
func (s *Store) CommitExtraction(ctx context.Context, batch ExtractionBatch) error {
	if s == nil || s.db == nil {
		return errors.New("memory store is closed")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var committedEpisode *Episode
	if batch.Episode != nil {
		episode := normalizeEpisode(*batch.Episode)
		if err := appendEpisodeTx(ctx, tx, episode); err != nil {
			return err
		}
		committedEpisode = &episode
	}
	priorSessionMemoryID := ""
	if committedEpisode != nil && len(batch.Memories) > 0 {
		_ = tx.QueryRowContext(ctx, `SELECT memory_id FROM memories WHERE scope_type=? AND scope_key=?
			AND source_session_id=? AND status=? ORDER BY updated_at DESC LIMIT 1`, committedEpisode.ScopeType,
			committedEpisode.ScopeKey, committedEpisode.SessionID, StatusActive).Scan(&priorSessionMemoryID)
	}
	memoryIDs := make([]string, 0, len(batch.Memories))
	for idx := range batch.Memories {
		entry := normalizeCandidate(batch.Memories[idx])
		if entry.MemoryID == "" {
			entry.MemoryID = StableID(entry.ScopeType, entry.ScopeKey, entry.Title, entry.Content)
		}
		if err := upsertEntryTx(ctx, tx, &entry); err != nil {
			return err
		}
		if err := upsertMemoryEntityTx(ctx, tx, entry); err != nil {
			return err
		}
		action := strings.ToLower(strings.TrimSpace(batch.Memories[idx].Action))
		targetID := strings.TrimSpace(batch.Memories[idx].TargetMemoryID)
		if action == "supersede" && targetID != "" && targetID != entry.MemoryID {
			if _, err := tx.ExecContext(ctx, `UPDATE memories SET status=?, superseded_by=?, updated_at=? WHERE memory_id=?`,
				StatusSuperseded, entry.MemoryID, formatTime(time.Now().UTC()), targetID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id=?`, targetID); err != nil {
				return err
			}
			if err := upsertEdgeTx(ctx, tx, normalizeEdge(Edge{ScopeType: entry.ScopeType, ScopeKey: entry.ScopeKey,
				FromID: targetID, ToID: entry.MemoryID, Type: EdgeSupersedes, Weight: 1, Confidence: entry.Confidence})); err != nil {
				return err
			}
		}
		memoryIDs = append(memoryIDs, entry.MemoryID)
	}
	if committedEpisode != nil && len(memoryIDs) > 0 {
		if priorSessionMemoryID != "" && priorSessionMemoryID != memoryIDs[0] {
			if err := upsertEdgeTx(ctx, tx, normalizeEdge(Edge{ScopeType: committedEpisode.ScopeType,
				ScopeKey: committedEpisode.ScopeKey, FromID: priorSessionMemoryID, ToID: memoryIDs[0],
				Type: EdgeNextEvent, Weight: 1, Confidence: 1})); err != nil {
				return err
			}
		}
		for index := 1; index < len(memoryIDs); index++ {
			if err := upsertEdgeTx(ctx, tx, normalizeEdge(Edge{ScopeType: committedEpisode.ScopeType,
				ScopeKey: committedEpisode.ScopeKey, FromID: memoryIDs[index-1], ToID: memoryIDs[index],
				Type: EdgeNextEvent, Weight: 1, Confidence: 1})); err != nil {
				return err
			}
		}
	}
	if committedEpisode != nil {
		sessionIndex := buildSessionIndex(*committedEpisode, batch.Memories)
		if err := upsertSessionIndexTx(ctx, tx, sessionIndex); err != nil {
			return err
		}
		for _, span := range batch.EpisodeSpans {
			span.MemoryID = sessionIndex.IndexID
			span.ScopeType = sessionIndex.ScopeType
			span.ScopeKey = sessionIndex.ScopeKey
			span.SessionID = sessionIndex.SessionID
			if err := upsertEvidenceSpanTx(ctx, tx, normalizeEvidenceSpan(span)); err != nil {
				return err
			}
		}
		if batch.SessionEmbedding != nil && len(batch.SessionEmbedding.Vector) > 0 {
			if err := upsertEmbeddingTx(ctx, tx, sessionIndex.IndexID, *batch.SessionEmbedding); err != nil {
				return err
			}
		}
	}
	for idx := range batch.Facts {
		fact := normalizeFact(batch.Facts[idx])
		memoryIndex := fact.MemoryIndex
		if memoryIndex < 0 || memoryIndex >= len(memoryIDs) {
			memoryIndex = idx
		}
		if fact.MemoryID == "" && memoryIndex < len(memoryIDs) {
			fact.MemoryID = memoryIDs[memoryIndex]
		}
		if err := upsertFactTx(ctx, tx, fact); err != nil {
			return err
		}
		if err := upsertFactEntitiesTx(ctx, tx, fact); err != nil {
			return err
		}
	}
	for _, span := range batch.Spans {
		if span.MemoryID == "" && span.MemoryIndex >= 0 && span.MemoryIndex < len(memoryIDs) {
			span.MemoryID = memoryIDs[span.MemoryIndex]
		}
		if err := upsertEvidenceSpanTx(ctx, tx, normalizeEvidenceSpan(span)); err != nil {
			return err
		}
	}
	for _, edge := range batch.Edges {
		if edge.FromID == "" && edge.FromMemoryIndex >= 0 && edge.FromMemoryIndex < len(memoryIDs) {
			edge.FromID = memoryIDs[edge.FromMemoryIndex]
		}
		if edge.ToID == "" && edge.ToMemoryIndex >= 0 && edge.ToMemoryIndex < len(memoryIDs) {
			edge.ToID = memoryIDs[edge.ToMemoryIndex]
		}
		if edge.FromID == "" || edge.ToID == "" {
			continue
		}
		if err := upsertEdgeTx(ctx, tx, normalizeEdge(edge)); err != nil {
			return err
		}
	}
	for _, block := range batch.CoreBlocks {
		if err := upsertCoreBlockTx(ctx, tx, normalizeCoreBlock(block)); err != nil {
			return err
		}
	}
	for _, embedding := range batch.Embeddings {
		memoryID := strings.TrimSpace(embedding.MemoryID)
		if memoryID == "" && embedding.MemoryIndex >= 0 && embedding.MemoryIndex < len(memoryIDs) {
			memoryID = memoryIDs[embedding.MemoryIndex]
		}
		if memoryID == "" || len(embedding.Vector) == 0 {
			continue
		}
		blob, err := vector.EncodeEmbedding(embedding.Vector)
		if err != nil {
			return err
		}
		now := formatTime(time.Now().UTC())
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_embeddings(memory_id, model, dimensions, content_hash, embedding, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(memory_id, model) DO UPDATE SET dimensions=excluded.dimensions,
			content_hash=excluded.content_hash, embedding=excluded.embedding, updated_at=excluded.updated_at`, memoryID,
			embedding.Model, len(embedding.Vector), embedding.ContentHash, blob, now, now); err != nil {
			return err
		}
	}
	for left := range batch.Memories {
		for right := left + 1; right < len(batch.Memories); right++ {
			if batch.Memories[left].ScopeType != batch.Memories[right].ScopeType || batch.Memories[left].ScopeKey != batch.Memories[right].ScopeKey {
				continue
			}
			if !sharesString(batch.Memories[left].Entities, batch.Memories[right].Entities) {
				continue
			}
			if err := upsertEdgeTx(ctx, tx, normalizeEdge(Edge{ScopeType: batch.Memories[left].ScopeType,
				ScopeKey: batch.Memories[left].ScopeKey, FromID: memoryIDs[left], ToID: memoryIDs[right],
				Type: EdgeRelatedTo, Weight: 0.7, Confidence: minFloat(batch.Memories[left].Confidence, batch.Memories[right].Confidence)})); err != nil {
				return err
			}
		}
	}
	for _, job := range batch.Jobs {
		job = normalizeJob(job)
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_jobs(job_id, kind, scope_type, scope_key, payload,
			status, attempts, last_error, available_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(job_id) DO UPDATE SET status=CASE WHEN memory_jobs.status='complete' THEN memory_jobs.status ELSE 'pending' END,
			payload=excluded.payload, available_at=excluded.available_at, updated_at=excluded.updated_at`, job.JobID, job.Kind,
			job.ScopeType, job.ScopeKey, job.Payload, job.Status, job.Attempts, job.LastError, formatTime(job.AvailableAt),
			formatTime(job.CreatedAt), formatTime(job.UpdatedAt)); err != nil {
			return err
		}
	}
	if batch.ConsumerID != "" && batch.SessionID != "" && batch.LastMessageIndex >= 0 {
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_cursors(consumer_id, session_id, last_message_id, last_message_index, updated_at)
			VALUES (?, ?, ?, ?, ?) ON CONFLICT(consumer_id, session_id) DO UPDATE SET
			last_message_id=excluded.last_message_id, last_message_index=excluded.last_message_index,
			updated_at=excluded.updated_at`, batch.ConsumerID, batch.SessionID, batch.LastMessageID,
			batch.LastMessageIndex, formatTime(time.Now().UTC())); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) UpsertEmbedding(ctx context.Context, memoryID, model, contentHash string, embedding []float32) error {
	if strings.TrimSpace(memoryID) == "" || strings.TrimSpace(model) == "" || len(embedding) == 0 {
		return errors.New("memory_id, model, and embedding are required")
	}
	blob, err := vector.EncodeEmbedding(embedding)
	if err != nil {
		return err
	}
	now := formatTime(time.Now().UTC())
	_, err = s.db.ExecContext(ctx, `INSERT INTO memory_embeddings(memory_id, model, dimensions, content_hash, embedding, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(memory_id, model) DO UPDATE SET dimensions=excluded.dimensions, content_hash=excluded.content_hash,
		embedding=excluded.embedding, updated_at=excluded.updated_at`, memoryID, model, len(embedding), contentHash, blob, now, now)
	return err
}

func (s *Store) SearchVector(ctx context.Context, embedding []float32, model string, opts SearchOptions) ([]Entry, error) {
	if len(embedding) == 0 || strings.TrimSpace(model) == "" {
		return nil, nil
	}
	blob, err := vector.EncodeEmbedding(embedding)
	if err != nil {
		return nil, err
	}
	if opts.MaxCandidates <= 0 {
		opts.MaxCandidates = 40
	}
	where, args := buildFilters(opts, true)
	if where == "" {
		where = "WHERE e.model = ? AND e.dimensions = ?"
	} else {
		where += " AND e.model = ? AND e.dimensions = ?"
	}
	args = append(args, model, len(embedding), blob, opts.MaxCandidates)
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixedMemoryColumns("m")+`, vec_cosine(e.embedding, ?) AS rank
		FROM memory_embeddings e JOIN memories m ON m.memory_id=e.memory_id `+where+`
		ORDER BY rank DESC LIMIT ?`, reorderVectorArgs(args)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries, err := scanEntriesWithRank(rows)
	if err != nil {
		return nil, err
	}
	for idx := range entries {
		entries[idx].Score = -entries[idx].Score
		entries[idx].MatchReason = "vector"
	}
	return entries, nil
}

func (s *Store) SearchSessionVector(ctx context.Context, embedding []float32, model string, scopes []Scope, limit int) ([]Entry, error) {
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
	scopeSQL, scopeArgs := scopedClauses(scopes, "s.")
	clauses := []string{"e.model=?", "e.dimensions=?"}
	args := []any{blob, model, len(embedding)}
	if scopeSQL != "" {
		clauses = append(clauses, scopeSQL)
		args = append(args, scopeArgs...)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT s.session_id, s.scope_type, s.scope_key, vec_cosine(e.embedding, ?) AS rank
		FROM memory_embeddings e JOIN memory_session_index s ON s.index_id=e.memory_id
		WHERE `+strings.Join(clauses, " AND ")+` ORDER BY rank DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sessions []sessionRank
	for rows.Next() {
		var session sessionRank
		if err := rows.Scan(&session.SessionID, &session.ScopeType, &session.ScopeKey, &session.Rank); err != nil {
			return nil, err
		}
		sessions = append(sessions, session)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return s.memoriesForSessions(ctx, sessions, scopes, limit)
}

func (s *Store) LoadEmbeddings(ctx context.Context, memoryIDs []string, model string) (map[string][]float32, error) {
	result := map[string][]float32{}
	if len(memoryIDs) == 0 || strings.TrimSpace(model) == "" {
		return result, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(memoryIDs)), ",")
	args := make([]any, 0, len(memoryIDs)+1)
	args = append(args, model)
	for _, memoryID := range memoryIDs {
		args = append(args, memoryID)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT memory_id, embedding FROM memory_embeddings WHERE model=? AND memory_id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var memoryID string
		var blob []byte
		if err := rows.Scan(&memoryID, &blob); err != nil {
			return nil, err
		}
		embedding, err := vector.DecodeEmbedding(blob)
		if err != nil {
			return nil, err
		}
		result[memoryID] = embedding
	}
	return result, rows.Err()
}

// reorderVectorArgs places the query embedding at the SELECT placeholder before
// the WHERE arguments assembled by buildFilters.
func reorderVectorArgs(args []any) []any {
	if len(args) < 4 {
		return args
	}
	embedding := args[len(args)-2]
	limit := args[len(args)-1]
	whereArgs := args[:len(args)-2]
	out := make([]any, 0, len(args))
	out = append(out, embedding)
	out = append(out, whereArgs...)
	out = append(out, limit)
	return out
}

func (s *Store) ResolveFactsAt(ctx context.Context, scopes []Scope, entities []string, at time.Time, limit int) ([]Fact, error) {
	if limit <= 0 {
		limit = 40
	}
	var clauses []string
	var args []any
	if len(scopes) > 0 {
		var scopeClauses []string
		for _, scope := range scopes {
			if scope.Type == "" || strings.TrimSpace(scope.Key) == "" {
				continue
			}
			scopeClauses = append(scopeClauses, "(scope_type=? AND scope_key=?)")
			args = append(args, scope.Type, scope.Key)
		}
		if len(scopeClauses) > 0 {
			clauses = append(clauses, "("+strings.Join(scopeClauses, " OR ")+")")
		}
	}
	if !at.IsZero() {
		stamp := formatTime(at)
		clauses = append(clauses, "(valid_from='' OR valid_from<=?)", "(valid_until='' OR valid_until>?)")
		args = append(args, stamp, stamp)
	} else {
		clauses = append(clauses, "status=?")
		args = append(args, StatusActive)
	}
	if len(entities) > 0 {
		var terms []string
		for _, entity := range normalizeStrings(entities) {
			terms = append(terms, "(subject LIKE ? OR object LIKE ?)")
			args = append(args, "%"+entity+"%", "%"+entity+"%")
		}
		if len(terms) > 0 {
			clauses = append(clauses, "("+strings.Join(terms, " OR ")+")")
		}
	}
	query := `SELECT fact_id, memory_id, scope_type, scope_key, subject, predicate, object, qualifiers_json,
		fact_key, confidence, valid_from, valid_until, observed_at, invalidated_at, superseded_by, status
		FROM memory_facts`
	if len(clauses) > 0 {
		query += " WHERE " + strings.Join(clauses, " AND ")
	}
	query += " ORDER BY valid_from DESC, observed_at DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanFacts(rows)
}

func (s *Store) ExpandGraph(ctx context.Context, seeds []string, scopes []Scope, maxHops, limit int) (map[string]float64, error) {
	if maxHops <= 0 || len(seeds) == 0 {
		return map[string]float64{}, nil
	}
	if maxHops > 2 {
		maxHops = 2
	}
	if limit <= 0 {
		limit = 20
	}
	allowed := scopeSet(scopes)
	graph := simple.NewWeightedDirectedGraph(0, 0)
	nodeIDs := map[string]int64{}
	memoryIDs := map[int64]string{}
	nextNodeID := int64(1)
	ensureNode := func(memoryID string) int64 {
		if nodeID, ok := nodeIDs[memoryID]; ok {
			return nodeID
		}
		nodeID := nextNodeID
		nextNodeID++
		nodeIDs[memoryID] = nodeID
		memoryIDs[nodeID] = memoryID
		graph.AddNode(simple.Node(nodeID))
		return nodeID
	}
	frontier := append([]string(nil), seeds...)
	seen := map[string]struct{}{}
	for _, seed := range seeds {
		seen[seed] = struct{}{}
		ensureNode(seed)
	}
	for hop := 1; hop <= maxHops && len(frontier) > 0; hop++ {
		marks := strings.TrimSuffix(strings.Repeat("?,", len(frontier)), ",")
		args := make([]any, 0, len(frontier)*2)
		for _, id := range frontier {
			args = append(args, id)
		}
		for _, id := range frontier {
			args = append(args, id)
		}
		rows, err := s.db.QueryContext(ctx, `SELECT scope_type, scope_key, from_id, to_id, weight, confidence
			FROM memory_edges WHERE (valid_until='' OR valid_until>?) AND (from_id IN (`+marks+`) OR to_id IN (`+marks+`))`,
			append([]any{formatTime(time.Now().UTC())}, args...)...)
		if err != nil {
			return nil, err
		}
		var next []string
		for rows.Next() {
			var scopeType ScopeType
			var scopeKey, fromID, toID string
			var weight, confidence float64
			if err := rows.Scan(&scopeType, &scopeKey, &fromID, &toID, &weight, &confidence); err != nil {
				rows.Close()
				return nil, err
			}
			if len(allowed) > 0 {
				if _, ok := allowed[string(scopeType)+"\x00"+scopeKey]; !ok {
					continue
				}
			}
			fromNode := ensureNode(fromID)
			toNode := ensureNode(toID)
			edgeWeight := weight * confidence
			graph.SetWeightedEdge(graph.NewWeightedEdge(graph.Node(fromNode), graph.Node(toNode), edgeWeight))
			graph.SetWeightedEdge(graph.NewWeightedEdge(graph.Node(toNode), graph.Node(fromNode), edgeWeight*0.85))
			neighbor := toID
			if _, ok := seen[toID]; ok {
				neighbor = fromID
			}
			if _, ok := seen[neighbor]; ok {
				continue
			}
			seen[neighbor] = struct{}{}
			next = append(next, neighbor)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		frontier = next
	}
	seedNodes := map[int64]struct{}{}
	for _, seed := range seeds {
		if nodeID, ok := nodeIDs[seed]; ok {
			seedNodes[nodeID] = struct{}{}
		}
	}
	pageScores := personalizedPageRank(graph, seedNodes, 0.85, 1e-7, 60)
	scores := map[string]float64{}
	for nodeID, score := range pageScores {
		memoryID := memoryIDs[nodeID]
		if _, isSeed := seenSeed(seeds, memoryID); isSeed {
			continue
		}
		scores[memoryID] = score
	}
	if len(scores) <= limit {
		return scores, nil
	}
	type pair struct {
		id    string
		score float64
	}
	pairs := make([]pair, 0, len(scores))
	for id, score := range scores {
		pairs = append(pairs, pair{id, score})
	}
	sort.Slice(pairs, func(i, j int) bool { return pairs[i].score > pairs[j].score })
	trimmed := make(map[string]float64, limit)
	for _, item := range pairs[:limit] {
		trimmed[item.id] = item.score
	}
	return trimmed, nil
}

func personalizedPageRank(graph *simple.WeightedDirectedGraph, seeds map[int64]struct{}, damping, tolerance float64, maxIterations int) map[int64]float64 {
	nodes := graph.Nodes()
	var nodeIDs []int64
	for nodes.Next() {
		nodeIDs = append(nodeIDs, nodes.Node().ID())
	}
	if len(nodeIDs) == 0 || len(seeds) == 0 {
		return map[int64]float64{}
	}
	personalization := map[int64]float64{}
	seedWeight := 1 / float64(len(seeds))
	for seed := range seeds {
		personalization[seed] = seedWeight
	}
	scores := map[int64]float64{}
	for _, nodeID := range nodeIDs {
		scores[nodeID] = personalization[nodeID]
	}
	for iteration := 0; iteration < maxIterations; iteration++ {
		next := map[int64]float64{}
		for _, nodeID := range nodeIDs {
			next[nodeID] = (1 - damping) * personalization[nodeID]
		}
		for _, fromID := range nodeIDs {
			neighbors := graph.From(fromID)
			var targets []int64
			var weights []float64
			var total float64
			for neighbors.Next() {
				toID := neighbors.Node().ID()
				edge := graph.WeightedEdge(fromID, toID)
				weight := 1.0
				if edge != nil {
					weight = edge.Weight()
				}
				targets = append(targets, toID)
				weights = append(weights, weight)
				total += weight
			}
			if total == 0 {
				for seed, weight := range personalization {
					next[seed] += damping * scores[fromID] * weight
				}
				continue
			}
			for index, toID := range targets {
				next[toID] += damping * scores[fromID] * weights[index] / total
			}
		}
		var delta float64
		for _, nodeID := range nodeIDs {
			delta += math.Abs(next[nodeID] - scores[nodeID])
		}
		scores = next
		if delta < tolerance {
			break
		}
	}
	return scores
}

func seenSeed(seeds []string, memoryID string) (int, bool) {
	for index, seed := range seeds {
		if seed == memoryID {
			return index, true
		}
	}
	return -1, false
}

func (s *Store) GetMany(ctx context.Context, ids []string) ([]Entry, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for idx := range ids {
		args[idx] = ids[idx]
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+memoryColumns+` FROM memories WHERE memory_id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries, err := scanEntries(rows)
	if err != nil {
		return nil, err
	}
	found := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		found[entry.MemoryID] = struct{}{}
	}
	var missing []string
	for _, id := range ids {
		if _, ok := found[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sessionEntries, sessionErr := s.sessionEntriesByIndexIDs(ctx, missing)
		if sessionErr != nil {
			return nil, sessionErr
		}
		entries = append(entries, sessionEntries...)
	}
	order := make(map[string]int, len(ids))
	for idx, id := range ids {
		order[id] = idx
	}
	sort.SliceStable(entries, func(i, j int) bool { return order[entries[i].MemoryID] < order[entries[j].MemoryID] })
	return entries, nil
}

func (s *Store) ListEvidenceSpans(ctx context.Context, memoryIDs []string) (map[string][]EvidenceSpan, error) {
	result := map[string][]EvidenceSpan{}
	if len(memoryIDs) == 0 {
		return result, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(memoryIDs)), ",")
	args := make([]any, len(memoryIDs))
	for idx := range memoryIDs {
		args[idx] = memoryIDs[idx]
	}
	rows, err := s.db.QueryContext(ctx, `SELECT span_id, memory_id, scope_type, scope_key, session_id, message_id,
		source_path, text, start_rune, end_rune, occurred_at, content_hash
		FROM memory_evidence_spans WHERE memory_id IN (`+marks+`) ORDER BY occurred_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var span EvidenceSpan
		var occurredAt string
		if err := rows.Scan(&span.SpanID, &span.MemoryID, &span.ScopeType, &span.ScopeKey, &span.SessionID,
			&span.MessageID, &span.SourcePath, &span.Text, &span.StartRune, &span.EndRune, &occurredAt, &span.ContentHash); err != nil {
			return nil, err
		}
		span.OccurredAt = parseTime(occurredAt)
		result[span.MemoryID] = append(result[span.MemoryID], span)
	}
	return result, rows.Err()
}

func (s *Store) ListCoreBlocks(ctx context.Context, scopes []Scope) ([]CoreBlock, error) {
	if len(scopes) == 0 {
		return nil, nil
	}
	var clauses []string
	var args []any
	for _, scope := range scopes {
		if scope.Type == "" || scope.Key == "" {
			continue
		}
		clauses = append(clauses, "(scope_type=? AND scope_key=?)")
		args = append(args, scope.Type, scope.Key)
	}
	if len(clauses) == 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT block_id, scope_type, scope_key, label, description, content,
		read_only, generation, updated_at FROM memory_core_blocks WHERE `+strings.Join(clauses, " OR "), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var blocks []CoreBlock
	for rows.Next() {
		var block CoreBlock
		var readOnly int
		var updatedAt string
		if err := rows.Scan(&block.BlockID, &block.ScopeType, &block.ScopeKey, &block.Label, &block.Description,
			&block.Content, &readOnly, &block.Generation, &updatedAt); err != nil {
			return nil, err
		}
		block.ReadOnly = readOnly != 0
		block.UpdatedAt = parseTime(updatedAt)
		blocks = append(blocks, block)
	}
	return blocks, rows.Err()
}

func (s *Store) RecordRetrievalTrace(ctx context.Context, trace RetrievalTrace) error {
	if trace.RunID == "" {
		trace.RunID = StableID(ScopeProject, "retrieval", trace.Query(), formatTime(time.Now().UTC()))
	}
	if trace.CreatedAt.IsZero() {
		trace.CreatedAt = time.Now().UTC()
	}
	planJSON, _ := json.Marshal(trace.Plan)
	runJSON, _ := json.Marshal(trace.Run)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO memory_retrieval_runs(
		run_id, session_id, team_session_id, agent_id, query, plan_json, selected_ids_json,
		estimated_tokens, duration_ms, error, run_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		trace.RunID, trace.SessionID, trace.TeamSessionID, trace.AgentID, trace.Plan.Query, string(planJSON),
		toJSON(trace.SelectedIDs), trace.EstimatedTokens, trace.DurationMS, trace.Error, string(runJSON), formatTime(trace.CreatedAt)); err != nil {
		return err
	}
	for _, item := range trace.Candidates {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO memory_retrieval_items(
			run_id, memory_id, channel_ranks_json, channel_scores_json, fused_score, graph_score, selected, drop_reason
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, trace.RunID, item.MemoryID, marshalJSON(item.ChannelRanks),
			marshalJSON(item.ChannelScores), item.FusedScore, item.GraphScore, boolInt(item.Selected), item.DropReason); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListRetrievalTraces(ctx context.Context, limit int) ([]RetrievalTrace, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT run_id, session_id, team_session_id, agent_id, plan_json,
		selected_ids_json, estimated_tokens, duration_ms, error, run_json, created_at
		FROM memory_retrieval_runs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var traces []RetrievalTrace
	for rows.Next() {
		var trace RetrievalTrace
		var planJSON, selectedJSON, runJSON, createdAt string
		if err := rows.Scan(&trace.RunID, &trace.SessionID, &trace.TeamSessionID, &trace.AgentID, &planJSON,
			&selectedJSON, &trace.EstimatedTokens, &trace.DurationMS, &trace.Error, &runJSON, &createdAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(planJSON), &trace.Plan)
		if strings.TrimSpace(runJSON) != "" && runJSON != "null" && runJSON != "{}" {
			var run RetrievalRun
			if json.Unmarshal([]byte(runJSON), &run) == nil {
				trace.Run = &run
			}
		}
		trace.SelectedIDs = fromJSONList(selectedJSON)
		trace.CreatedAt = parseTime(createdAt)
		items, err := s.listRetrievalItems(ctx, trace.RunID)
		if err != nil {
			return nil, err
		}
		trace.Candidates = items
		traces = append(traces, trace)
	}
	return traces, rows.Err()
}

func ensureTableColumn(ctx context.Context, db *sql.DB, table, column, definition string) error {
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+table+")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid int
		var name, kind string
		var notNull, primaryKey int
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = db.ExecContext(ctx, "ALTER TABLE "+table+" ADD COLUMN "+column+" "+definition)
	return err
}

func (s *Store) listRetrievalItems(ctx context.Context, runID string) ([]CandidateScore, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT memory_id, channel_ranks_json, channel_scores_json,
		fused_score, graph_score, selected, drop_reason FROM memory_retrieval_items WHERE run_id=?
		ORDER BY selected DESC, fused_score DESC`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []CandidateScore
	for rows.Next() {
		var item CandidateScore
		var ranksJSON, scoresJSON string
		var selected int
		if err := rows.Scan(&item.MemoryID, &ranksJSON, &scoresJSON, &item.FusedScore, &item.GraphScore,
			&selected, &item.DropReason); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ranksJSON), &item.ChannelRanks)
		_ = json.Unmarshal([]byte(scoresJSON), &item.ChannelScores)
		item.Selected = selected != 0
		items = append(items, item)
	}
	return items, rows.Err()
}

func (t RetrievalTrace) Query() string { return t.Plan.Query }

func (s *Store) GetCursor(ctx context.Context, consumerID, sessionID string) (string, int, error) {
	var messageID, updatedAt string
	var index int
	err := s.db.QueryRowContext(ctx, `SELECT last_message_id, last_message_index, updated_at FROM memory_cursors
		WHERE consumer_id=? AND session_id=?`, consumerID, sessionID).Scan(&messageID, &index, &updatedAt)
	return messageID, index, err
}

func (s *Store) SetCursor(ctx context.Context, consumerID, sessionID, messageID string, index int) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO memory_cursors(consumer_id, session_id, last_message_id, last_message_index, updated_at)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(consumer_id, session_id) DO UPDATE SET
		last_message_id=excluded.last_message_id, last_message_index=excluded.last_message_index, updated_at=excluded.updated_at`,
		consumerID, sessionID, messageID, index, formatTime(time.Now().UTC()))
	return err
}

func buildSessionIndex(episode Episode, memories []Candidate) SessionIndex {
	var summaries, keyphrases, entities []string
	for _, candidate := range memories {
		if summary := strings.TrimSpace(candidate.Summary); summary != "" {
			summaries = append(summaries, summary)
		}
		keyphrases = append(keyphrases, candidate.Tags...)
		entities = append(entities, candidate.Entities...)
	}
	summary := strings.Join(normalizeStrings(summaries), "\n")
	if summary == "" {
		summary = episode.Content
	}
	summary = clipRunes(summary, 6000)
	sessionID := strings.TrimSpace(episode.SessionID)
	if sessionID == "" {
		sessionID = episode.EpisodeID
	}
	return SessionIndex{
		IndexID:   StableID(episode.ScopeType, episode.ScopeKey, "session-index", sessionID),
		ScopeType: episode.ScopeType, ScopeKey: episode.ScopeKey, SessionID: sessionID,
		Summary: summary, Keyphrases: normalizeStrings(keyphrases), Entities: normalizeStrings(entities),
		Roles:     normalizeStrings([]string{episode.AgentID}),
		StartedAt: episode.OccurredAt, EndedAt: episode.OccurredAt,
		ContentHash: StableID(episode.ScopeType, episode.ScopeKey, sessionID, summary),
	}
}

func upsertSessionIndexTx(ctx context.Context, tx *sql.Tx, index SessionIndex) error {
	var oldSummary, oldKeyphrases, oldEntities, oldRoles, oldStarted, oldEnded string
	err := tx.QueryRowContext(ctx, `SELECT summary, keyphrases_json, entities_json, roles_json, started_at, ended_at
		FROM memory_session_index WHERE scope_type=? AND scope_key=? AND session_id=?`,
		index.ScopeType, index.ScopeKey, index.SessionID).Scan(&oldSummary, &oldKeyphrases, &oldEntities, &oldRoles, &oldStarted, &oldEnded)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		index.Summary = mergeSessionText(oldSummary, index.Summary)
		index.Keyphrases = normalizeStrings(append(fromJSONList(oldKeyphrases), index.Keyphrases...))
		index.Entities = normalizeStrings(append(fromJSONList(oldEntities), index.Entities...))
		index.Roles = normalizeStrings(append(fromJSONList(oldRoles), index.Roles...))
		oldStart, oldEnd := parseTime(oldStarted), parseTime(oldEnded)
		if !oldStart.IsZero() && (index.StartedAt.IsZero() || oldStart.Before(index.StartedAt)) {
			index.StartedAt = oldStart
		}
		if oldEnd.After(index.EndedAt) {
			index.EndedAt = oldEnd
		}
	}
	index.ContentHash = StableID(index.ScopeType, index.ScopeKey, index.SessionID, index.Summary)
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_session_index(index_id, scope_type, scope_key, session_id,
		summary, keyphrases_json, entities_json, roles_json, started_at, ended_at, content_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(scope_type, scope_key, session_id) DO UPDATE SET
		summary=excluded.summary, keyphrases_json=excluded.keyphrases_json, entities_json=excluded.entities_json,
		roles_json=excluded.roles_json, started_at=CASE WHEN memory_session_index.started_at='' OR excluded.started_at<memory_session_index.started_at THEN excluded.started_at ELSE memory_session_index.started_at END,
		ended_at=CASE WHEN excluded.ended_at>memory_session_index.ended_at THEN excluded.ended_at ELSE memory_session_index.ended_at END,
		content_hash=excluded.content_hash`, index.IndexID, index.ScopeType, index.ScopeKey, index.SessionID,
		index.Summary, toJSON(index.Keyphrases), toJSON(index.Entities), toJSON(index.Roles),
		formatTime(index.StartedAt), formatTime(index.EndedAt), index.ContentHash)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_session_fts WHERE index_id=?`, index.IndexID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_session_fts(index_id, session_id, summary, keyphrases, entities)
		VALUES (?, ?, ?, ?, ?)`, index.IndexID, index.SessionID, index.Summary,
		strings.Join(index.Keyphrases, " "), strings.Join(index.Entities, " "))
	return err
}

func mergeSessionText(existing, incoming string) string {
	existing, incoming = strings.TrimSpace(existing), strings.TrimSpace(incoming)
	if existing == "" {
		return clipRunes(incoming, 12000)
	}
	if incoming == "" || strings.Contains(existing, incoming) {
		return clipRunes(existing, 12000)
	}
	return clipRunes(existing+"\n"+incoming, 12000)
}

func upsertEmbeddingTx(ctx context.Context, tx *sql.Tx, memoryID string, embedding MemoryEmbedding) error {
	if strings.TrimSpace(memoryID) == "" || strings.TrimSpace(embedding.Model) == "" || len(embedding.Vector) == 0 {
		return nil
	}
	blob, err := vector.EncodeEmbedding(embedding.Vector)
	if err != nil {
		return err
	}
	now := formatTime(time.Now().UTC())
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_embeddings(memory_id, model, dimensions, content_hash, embedding, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT(memory_id, model) DO UPDATE SET dimensions=excluded.dimensions,
		content_hash=excluded.content_hash, embedding=excluded.embedding, updated_at=excluded.updated_at`, memoryID,
		embedding.Model, len(embedding.Vector), embedding.ContentHash, blob, now, now)
	return err
}

func normalizeEntityValue(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(strings.TrimSpace(value)), " "))
}

func upsertMemoryEntityTx(ctx context.Context, tx *sql.Tx, entry Entry) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_entities WHERE memory_id=?`, entry.MemoryID); err != nil {
		return err
	}
	for _, original := range normalizeStrings(entry.Entities) {
		normalized := normalizeEntityValue(original)
		if normalized == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO memory_entities(memory_id, scope_type, scope_key,
			normalized_entity, original_text, entity_type, confidence) VALUES (?, ?, ?, ?, ?, ?, ?)`, entry.MemoryID,
			entry.ScopeType, entry.ScopeKey, normalized, original, "", clamp01(entry.Confidence)); err != nil {
			return err
		}
	}
	return nil
}

func upsertFactEntitiesTx(ctx context.Context, tx *sql.Tx, fact Fact) error {
	if strings.TrimSpace(fact.MemoryID) == "" {
		return nil
	}
	for _, original := range []string{fact.Subject, fact.Object} {
		normalized := normalizeEntityValue(original)
		if normalized == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO memory_entities(memory_id, scope_type, scope_key,
			normalized_entity, original_text, entity_type, confidence) VALUES (?, ?, ?, ?, ?, ?, ?)`, fact.MemoryID,
			fact.ScopeType, fact.ScopeKey, normalized, original, "fact", clamp01(fact.Confidence)); err != nil {
			return err
		}
	}
	return nil
}

func appendEpisodeTx(ctx context.Context, tx *sql.Tx, episode Episode) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_episodes(episode_id, scope_type, scope_key, session_id,
		team_session_id, agent_id, message_ids_json, kind, content, occurred_at, observed_at, content_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(episode_id) DO NOTHING`, episode.EpisodeID,
		episode.ScopeType, episode.ScopeKey, episode.SessionID, episode.TeamSessionID, episode.AgentID,
		toJSON(episode.MessageIDs), episode.Kind, episode.Content, formatTime(episode.OccurredAt),
		formatTime(episode.ObservedAt), episode.ContentHash)
	return err
}

func upsertEntryTx(ctx context.Context, tx *sql.Tx, entry *Entry) error {
	if existing, _ := getEntryTx(ctx, tx, entry.MemoryID); existing != nil {
		entry.CreatedAt = existing.CreatedAt
		entry.LastAccessedAt = existing.LastAccessedAt
	}
	now := time.Now().UTC()
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = now
	}
	entry.UpdatedAt = now
	if entry.LastAccessedAt.IsZero() {
		entry.LastAccessedAt = now
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO memories (`+memoryColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(memory_id) DO UPDATE SET scope_type=excluded.scope_type, scope_key=excluded.scope_key,
		memory_type=excluded.memory_type, title=excluded.title, content=excluded.content, summary=excluded.summary,
		tags_json=excluded.tags_json, entities_json=excluded.entities_json, importance=excluded.importance,
		confidence=excluded.confidence, source_session_id=excluded.source_session_id,
		source_message_ids_json=excluded.source_message_ids_json, source_agent_id=excluded.source_agent_id,
		source_team_session_id=excluded.source_team_session_id, source_paths_json=excluded.source_paths_json,
		updated_at=excluded.updated_at, last_accessed_at=excluded.last_accessed_at, valid_from=excluded.valid_from,
		valid_until=excluded.valid_until, superseded_by=excluded.superseded_by, status=excluded.status`,
		entry.MemoryID, entry.ScopeType, entry.ScopeKey, entry.MemoryType, entry.Title, entry.Content, entry.Summary,
		toJSON(entry.Tags), toJSON(entry.Entities), clamp01(entry.Importance), clamp01(entry.Confidence),
		entry.SourceSessionID, toJSON(entry.SourceMessageIDs), entry.SourceAgentID, entry.SourceTeamSessionID,
		toJSON(entry.SourcePaths), formatTime(entry.CreatedAt), formatTime(entry.UpdatedAt), formatTime(entry.LastAccessedAt),
		formatTime(entry.ValidFrom), formatTime(entry.ValidUntil), entry.SupersededBy, entry.Status)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id=?`, entry.MemoryID); err != nil {
		return err
	}
	if entry.Status == StatusActive {
		_, err = tx.ExecContext(ctx, `INSERT INTO memory_fts(memory_id, title, content, summary, tags, entities)
			VALUES (?, ?, ?, ?, ?, ?)`, entry.MemoryID, entry.Title, entry.Content, entry.Summary,
			strings.Join(entry.Tags, " "), strings.Join(entry.Entities, " "))
	}
	return err
}

func getEntryTx(ctx context.Context, tx *sql.Tx, memoryID string) (*Entry, error) {
	return scanEntry(tx.QueryRowContext(ctx, `SELECT `+memoryColumns+` FROM memories WHERE memory_id=?`, memoryID))
}

func upsertFactTx(ctx context.Context, tx *sql.Tx, fact Fact) error {
	var old Fact
	var qualifiers, validFrom, validUntil, observedAt, invalidatedAt string
	err := tx.QueryRowContext(ctx, `SELECT fact_id, memory_id, scope_type, scope_key, subject, predicate, object,
		qualifiers_json, fact_key, confidence, valid_from, valid_until, observed_at, invalidated_at, superseded_by, status
		FROM memory_facts WHERE scope_type=? AND scope_key=? AND fact_key=? AND status=? ORDER BY observed_at DESC LIMIT 1`,
		fact.ScopeType, fact.ScopeKey, fact.FactKey, StatusActive).Scan(&old.FactID, &old.MemoryID, &old.ScopeType,
		&old.ScopeKey, &old.Subject, &old.Predicate, &old.Object, &qualifiers, &old.FactKey, &old.Confidence,
		&validFrom, &validUntil, &observedAt, &invalidatedAt, &old.SupersededBy, &old.Status)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil && normalizeFactValue(old.Object) != normalizeFactValue(fact.Object) {
		cutoff := fact.ValidFrom
		if cutoff.IsZero() {
			cutoff = fact.ObservedAt
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_facts SET status=?, valid_until=?, invalidated_at=?, superseded_by=? WHERE fact_id=?`,
			StatusSuperseded, formatTime(cutoff), formatTime(fact.ObservedAt), fact.FactID, old.FactID); err != nil {
			return err
		}
		if err := upsertEdgeTx(ctx, tx, normalizeEdge(Edge{ScopeType: fact.ScopeType, ScopeKey: fact.ScopeKey,
			FromID: old.FactID, ToID: fact.FactID, Type: EdgeSupersedes, Weight: 1, Confidence: fact.Confidence})); err != nil {
			return err
		}
		if err := upsertEdgeTx(ctx, tx, normalizeEdge(Edge{ScopeType: fact.ScopeType, ScopeKey: fact.ScopeKey,
			FromID: fact.FactID, ToID: old.FactID, Type: EdgeContradicts, Weight: 1, Confidence: fact.Confidence})); err != nil {
			return err
		}
	} else if err == nil {
		fact.FactID = old.FactID
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_facts(fact_id, memory_id, scope_type, scope_key, subject,
		predicate, object, qualifiers_json, fact_key, confidence, valid_from, valid_until, observed_at,
		invalidated_at, superseded_by, status) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(fact_id) DO UPDATE SET memory_id=excluded.memory_id, object=excluded.object,
		qualifiers_json=excluded.qualifiers_json, confidence=excluded.confidence, valid_from=excluded.valid_from,
		valid_until=excluded.valid_until, observed_at=excluded.observed_at, invalidated_at=excluded.invalidated_at,
		superseded_by=excluded.superseded_by, status=excluded.status`, fact.FactID, fact.MemoryID, fact.ScopeType,
		fact.ScopeKey, fact.Subject, fact.Predicate, fact.Object, marshalJSON(fact.Qualifiers), fact.FactKey,
		clamp01(fact.Confidence), formatTime(fact.ValidFrom), formatTime(fact.ValidUntil), formatTime(fact.ObservedAt),
		formatTime(fact.InvalidatedAt), fact.SupersededBy, fact.Status)
	return err
}

func upsertEvidenceSpanTx(ctx context.Context, tx *sql.Tx, span EvidenceSpan) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_evidence_spans(span_id, memory_id, scope_type, scope_key,
		session_id, message_id, source_path, text, start_rune, end_rune, occurred_at, content_hash)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(span_id) DO NOTHING`, span.SpanID, span.MemoryID,
		span.ScopeType, span.ScopeKey, span.SessionID, span.MessageID, span.SourcePath, span.Text, span.StartRune,
		span.EndRune, formatTime(span.OccurredAt), span.ContentHash)
	return err
}

func upsertEdgeTx(ctx context.Context, tx *sql.Tx, edge Edge) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_edges(edge_id, scope_type, scope_key, from_id, to_id,
		edge_type, weight, confidence, created_at, valid_until) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(edge_id) DO UPDATE SET weight=excluded.weight, confidence=excluded.confidence,
		valid_until=excluded.valid_until`, edge.EdgeID, edge.ScopeType, edge.ScopeKey, edge.FromID, edge.ToID,
		edge.Type, edge.Weight, edge.Confidence, formatTime(edge.CreatedAt), formatTime(edge.ValidUntil))
	return err
}

func upsertCoreBlockTx(ctx context.Context, tx *sql.Tx, block CoreBlock) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_core_blocks(block_id, scope_type, scope_key, label,
		description, content, read_only, generation, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(scope_type, scope_key, label) DO UPDATE SET description=excluded.description,
		content=excluded.content, read_only=excluded.read_only, generation=memory_core_blocks.generation+1,
		updated_at=excluded.updated_at WHERE memory_core_blocks.read_only=0`, block.BlockID, block.ScopeType,
		block.ScopeKey, block.Label, block.Description, block.Content, boolInt(block.ReadOnly), block.Generation,
		formatTime(block.UpdatedAt))
	return err
}

func normalizeEpisode(episode Episode) Episode {
	episode.ScopeType = normalizeScopeType(episode.ScopeType)
	episode.ScopeKey = defaultScopeKey(episode.ScopeKey)
	episode.Content = strings.TrimSpace(episode.Content)
	episode.MessageIDs = normalizeStrings(episode.MessageIDs)
	if episode.Kind == "" {
		episode.Kind = "conversation"
	}
	if episode.ObservedAt.IsZero() {
		episode.ObservedAt = time.Now().UTC()
	}
	if episode.OccurredAt.IsZero() {
		episode.OccurredAt = episode.ObservedAt
	}
	if episode.ContentHash == "" {
		episode.ContentHash = StableID(episode.ScopeType, episode.ScopeKey, "episode-content", episode.Content)
	}
	if episode.EpisodeID == "" {
		episode.EpisodeID = StableID(episode.ScopeType, episode.ScopeKey, episode.SessionID,
			strings.Join([]string{strings.Join(episode.MessageIDs, ","), episode.ContentHash}, "\x00"))
	}
	return episode
}

func normalizeFact(fact Fact) Fact {
	fact.ScopeType = normalizeScopeType(fact.ScopeType)
	fact.ScopeKey = defaultScopeKey(fact.ScopeKey)
	fact.Subject = strings.TrimSpace(fact.Subject)
	fact.Predicate = strings.TrimSpace(fact.Predicate)
	fact.Object = strings.TrimSpace(fact.Object)
	if fact.Qualifiers == nil {
		fact.Qualifiers = map[string]any{}
	}
	if fact.ObservedAt.IsZero() {
		fact.ObservedAt = time.Now().UTC()
	}
	if fact.ValidFrom.IsZero() {
		fact.ValidFrom = fact.ObservedAt
	}
	if fact.Status == "" {
		fact.Status = StatusActive
	}
	if fact.Confidence <= 0 {
		fact.Confidence = 0.5
	}
	if fact.FactKey == "" {
		fact.FactKey = StableID(fact.ScopeType, fact.ScopeKey, normalizeFactValue(fact.Subject),
			strings.Join([]string{normalizeFactValue(fact.Predicate), canonicalJSON(fact.Qualifiers)}, "\x00"))
	}
	if fact.FactID == "" {
		fact.FactID = StableID(fact.ScopeType, fact.ScopeKey, fact.FactKey,
			strings.Join([]string{normalizeFactValue(fact.Object), formatTime(fact.ValidFrom), formatTime(fact.ObservedAt)}, "\x00"))
	}
	return fact
}

func normalizeEvidenceSpan(span EvidenceSpan) EvidenceSpan {
	span.ScopeType = normalizeScopeType(span.ScopeType)
	span.ScopeKey = defaultScopeKey(span.ScopeKey)
	span.Text = strings.TrimSpace(span.Text)
	if span.EndRune <= 0 {
		span.EndRune = span.StartRune + len([]rune(span.Text))
	}
	if span.ContentHash == "" {
		span.ContentHash = StableID(span.ScopeType, span.ScopeKey, "evidence", span.Text)
	}
	if span.SpanID == "" {
		span.SpanID = StableID(span.ScopeType, span.ScopeKey, span.MemoryID,
			strings.Join([]string{span.SessionID, span.MessageID, fmt.Sprint(span.StartRune), span.ContentHash}, "\x00"))
	}
	return span
}

func normalizeEdge(edge Edge) Edge {
	edge.ScopeType = normalizeScopeType(edge.ScopeType)
	edge.ScopeKey = defaultScopeKey(edge.ScopeKey)
	if edge.Type == "" {
		edge.Type = EdgeRelatedTo
	}
	if edge.Weight <= 0 {
		edge.Weight = 1
	}
	if edge.Confidence <= 0 {
		edge.Confidence = 0.5
	}
	if edge.CreatedAt.IsZero() {
		edge.CreatedAt = time.Now().UTC()
	}
	if edge.EdgeID == "" {
		edge.EdgeID = StableID(edge.ScopeType, edge.ScopeKey, edge.FromID,
			strings.Join([]string{edge.ToID, string(edge.Type)}, "\x00"))
	}
	return edge
}

func normalizeCoreBlock(block CoreBlock) CoreBlock {
	block.ScopeType = normalizeScopeType(block.ScopeType)
	block.ScopeKey = defaultScopeKey(block.ScopeKey)
	block.Label = strings.TrimSpace(block.Label)
	block.Content = strings.TrimSpace(block.Content)
	if block.Generation <= 0 {
		block.Generation = 1
	}
	if block.UpdatedAt.IsZero() {
		block.UpdatedAt = time.Now().UTC()
	}
	if block.BlockID == "" {
		block.BlockID = StableID(block.ScopeType, block.ScopeKey, "core", block.Label)
	}
	return block
}

func normalizeScopeType(scopeType ScopeType) ScopeType {
	if scopeType == "" {
		return ScopeProject
	}
	return scopeType
}

func defaultScopeKey(scopeKey string) string {
	if strings.TrimSpace(scopeKey) == "" {
		return "default"
	}
	return strings.TrimSpace(scopeKey)
}

func normalizeFactValue(value string) string {
	return strings.ToLower(strings.Join(strings.Fields(value), " "))
}

func scanFacts(rows *sql.Rows) ([]Fact, error) {
	var facts []Fact
	for rows.Next() {
		var fact Fact
		var qualifiers, validFrom, validUntil, observedAt, invalidatedAt string
		if err := rows.Scan(&fact.FactID, &fact.MemoryID, &fact.ScopeType, &fact.ScopeKey, &fact.Subject,
			&fact.Predicate, &fact.Object, &qualifiers, &fact.FactKey, &fact.Confidence, &validFrom,
			&validUntil, &observedAt, &invalidatedAt, &fact.SupersededBy, &fact.Status); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(qualifiers), &fact.Qualifiers)
		fact.ValidFrom = parseTime(validFrom)
		fact.ValidUntil = parseTime(validUntil)
		fact.ObservedAt = parseTime(observedAt)
		fact.InvalidatedAt = parseTime(invalidatedAt)
		facts = append(facts, fact)
	}
	return facts, rows.Err()
}

func scopeSet(scopes []Scope) map[string]struct{} {
	result := map[string]struct{}{}
	for _, scope := range scopes {
		if scope.Type != "" && scope.Key != "" {
			result[string(scope.Type)+"\x00"+scope.Key] = struct{}{}
		}
	}
	return result
}

func marshalJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func canonicalJSON(value any) string {
	return marshalJSON(value)
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func sharesString(left, right []string) bool {
	values := map[string]struct{}{}
	for _, value := range normalizeStrings(left) {
		values[strings.ToLower(value)] = struct{}{}
	}
	for _, value := range normalizeStrings(right) {
		if _, ok := values[strings.ToLower(value)]; ok {
			return true
		}
	}
	return false
}

func minFloat(left, right float64) float64 {
	if left < right {
		return left
	}
	return right
}
