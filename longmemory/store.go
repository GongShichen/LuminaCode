package longmemory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/viant/sqlite-vec/engine"
	_ "modernc.org/sqlite"
)

type Store struct {
	path          string
	db            *sql.DB
	busyTimeoutMS int64
}

type storeMigrationState struct {
	sync.Mutex
	complete bool
}

var storeMigrations sync.Map

func Open(ctx context.Context, path string) (*Store, error) {
	return OpenWithBusyTimeout(ctx, path, 5*time.Second)
}

// OpenWithBusyTimeout creates a store whose pooled SQLite connections use the
// supplied writer wait. Long-running maintenance can retain computed work
// while an ingestion transaction commits, without changing interactive store
// behavior or globally extending lock waits.
func OpenWithBusyTimeout(ctx context.Context, path string, busyTimeout time.Duration) (*Store, error) {
	if err := engine.RegisterVectorFunctions(nil); err != nil {
		return nil, fmt.Errorf("register memory vector functions: %w", err)
	}
	if busyTimeout <= 0 {
		busyTimeout = 5 * time.Second
	}
	busyTimeoutMS := busyTimeout.Milliseconds()
	path = ExpandPath(path)
	if path == "" {
		path = DefaultStorePath()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	// Configure the timeout in the DSN so every pooled connection waits for a
	// short-lived WAL writer instead of immediately surfacing SQLITE_BUSY.
	// PRAGMA busy_timeout executed once would only affect whichever connection
	// the pool happened to use for migration.
	dsn := path
	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	dsn += separator + fmt.Sprintf("_pragma=busy_timeout%%3D%d", busyTimeoutMS)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, db: db, busyTimeoutMS: busyTimeoutMS}
	stateValue, _ := storeMigrations.LoadOrStore(filepath.Clean(path), &storeMigrationState{})
	state := stateValue.(*storeMigrationState)
	state.Lock()
	if !state.complete {
		if err := s.migrate(ctx); err != nil {
			_ = db.Close()
			state.Unlock()
			return nil, err
		}
		if filepath.Clean(path) == filepath.Clean(DefaultStorePath()) {
			if _, err := s.MigrateLegacyMarkdown(ctx); err != nil {
				_ = db.Close()
				state.Unlock()
				return nil, fmt.Errorf("migrate legacy long-term memory: %w", err)
			}
		}
		state.complete = true
	}
	state.Unlock()
	return s, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	stmts := []string{
		`PRAGMA journal_mode=WAL;`,
		fmt.Sprintf(`PRAGMA busy_timeout=%d;`, s.busyTimeoutMS),
		`CREATE TABLE IF NOT EXISTS memories (
			memory_id TEXT PRIMARY KEY,
			scope_type TEXT NOT NULL,
			scope_key TEXT NOT NULL,
			memory_type TEXT NOT NULL,
			title TEXT NOT NULL,
			content TEXT NOT NULL,
			summary TEXT NOT NULL DEFAULT '',
			tags_json TEXT NOT NULL DEFAULT '[]',
			entities_json TEXT NOT NULL DEFAULT '[]',
			importance REAL NOT NULL DEFAULT 0.5,
			confidence REAL NOT NULL DEFAULT 0.5,
			source_session_id TEXT NOT NULL DEFAULT '',
			source_message_ids_json TEXT NOT NULL DEFAULT '[]',
			source_agent_id TEXT NOT NULL DEFAULT '',
			source_team_session_id TEXT NOT NULL DEFAULT '',
			source_paths_json TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			last_accessed_at TEXT NOT NULL,
			temperature TEXT NOT NULL DEFAULT 'warm',
			retention_expires_at TEXT NOT NULL DEFAULT '',
			access_count INTEGER NOT NULL DEFAULT 0,
			last_reinforced_at TEXT NOT NULL DEFAULT '',
			archived_at TEXT NOT NULL DEFAULT '',
			archive_reason TEXT NOT NULL DEFAULT '',
			pinned INTEGER NOT NULL DEFAULT 0,
			value_score REAL NOT NULL DEFAULT 0.5,
			value_score_updated_at TEXT NOT NULL DEFAULT '',
			valid_from TEXT NOT NULL DEFAULT '',
			valid_until TEXT NOT NULL DEFAULT '',
			superseded_by TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL DEFAULT 'active'
		);`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS memory_fts USING fts5(memory_id UNINDEXED, title, content, summary, tags, entities);`,
		`CREATE TABLE IF NOT EXISTS memory_used (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL DEFAULT '',
			team_session_id TEXT NOT NULL DEFAULT '',
			agent_id TEXT NOT NULL DEFAULT '',
			query TEXT NOT NULL DEFAULT '',
			memory_ids_json TEXT NOT NULL DEFAULT '[]',
			created_at TEXT NOT NULL
		);`,
		`CREATE INDEX IF NOT EXISTS idx_memories_scope ON memories(scope_type, scope_key, status);`,
		`CREATE INDEX IF NOT EXISTS idx_memories_type ON memories(memory_type, status);`,
		`CREATE INDEX IF NOT EXISTS idx_memories_updated ON memories(updated_at);`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return s.migrateMemoryStorage(ctx)
}

func (s *Store) Upsert(ctx context.Context, candidate Candidate) (*Entry, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("memory store is closed")
	}
	entry := normalizeCandidate(candidate)
	if entry.MemoryID == "" {
		entry.MemoryID = StableID(entry.ScopeType, entry.ScopeKey, entry.Title, entry.Content)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	wasExisting := false
	var oldTemperature Temperature
	var oldStatus Status
	if existing, _ := s.getTx(ctx, tx, entry.MemoryID); existing != nil {
		wasExisting = true
		oldTemperature, oldStatus = existing.Temperature, existing.Status
		entry.CreatedAt = existing.CreatedAt
		entry.LastAccessedAt = existing.LastAccessedAt
		entry.Temperature = existing.Temperature
		entry.AccessCount = existing.AccessCount
		entry.LastReinforcedAt = existing.LastReinforcedAt
		entry.ArchivedAt = existing.ArchivedAt
		entry.ArchiveReason = existing.ArchiveReason
		entry.Pinned = existing.Pinned
		entry.ValueScore = existing.ValueScore
		entry.ValueScoreUpdatedAt = existing.ValueScoreUpdatedAt
		if entry.RetentionExpiresAt.IsZero() {
			entry.RetentionExpiresAt = existing.RetentionExpiresAt
		}
		entry.LastReinforcedAt = time.Now().UTC()
		entry.Temperature = TemperatureHot
		if entry.Status == StatusActive {
			entry.ArchivedAt = time.Time{}
			entry.ArchiveReason = ""
		}
	}
	if entry.CreatedAt.IsZero() {
		entry.CreatedAt = time.Now().UTC()
	}
	entry.UpdatedAt = time.Now().UTC()
	if entry.LastAccessedAt.IsZero() {
		entry.LastAccessedAt = entry.UpdatedAt
	}
	if entry.Temperature == "" {
		entry.Temperature = TemperatureWarm
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memories (
		memory_id, scope_type, scope_key, memory_type, title, content, summary, tags_json, entities_json,
		importance, confidence, source_session_id, source_message_ids_json, source_agent_id, source_team_session_id,
		source_paths_json, created_at, updated_at, last_accessed_at, temperature, retention_expires_at, access_count,
		last_reinforced_at, archived_at, archive_reason, pinned, value_score, value_score_updated_at,
		valid_from, valid_until, superseded_by, status
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(memory_id) DO UPDATE SET
		scope_type=excluded.scope_type, scope_key=excluded.scope_key, memory_type=excluded.memory_type,
		title=excluded.title, content=excluded.content, summary=excluded.summary, tags_json=excluded.tags_json,
		entities_json=excluded.entities_json, importance=excluded.importance, confidence=excluded.confidence,
		source_session_id=excluded.source_session_id, source_message_ids_json=excluded.source_message_ids_json,
		source_agent_id=excluded.source_agent_id, source_team_session_id=excluded.source_team_session_id,
		source_paths_json=excluded.source_paths_json, updated_at=excluded.updated_at, last_accessed_at=excluded.last_accessed_at,
		temperature=excluded.temperature, retention_expires_at=excluded.retention_expires_at,
		access_count=excluded.access_count, last_reinforced_at=excluded.last_reinforced_at,
		archived_at=excluded.archived_at, archive_reason=excluded.archive_reason, pinned=excluded.pinned,
		value_score=excluded.value_score, value_score_updated_at=excluded.value_score_updated_at,
		valid_from=excluded.valid_from, valid_until=excluded.valid_until, superseded_by=excluded.superseded_by, status=excluded.status`,
		entry.MemoryID, entry.ScopeType, entry.ScopeKey, entry.MemoryType, entry.Title, entry.Content, entry.Summary,
		toJSON(entry.Tags), toJSON(entry.Entities), clamp01(entry.Importance), clamp01(entry.Confidence),
		entry.SourceSessionID, toJSON(entry.SourceMessageIDs), entry.SourceAgentID, entry.SourceTeamSessionID, toJSON(entry.SourcePaths),
		formatTime(entry.CreatedAt), formatTime(entry.UpdatedAt), formatTime(entry.LastAccessedAt), entry.Temperature,
		formatTime(entry.RetentionExpiresAt), entry.AccessCount, formatTime(entry.LastReinforcedAt), formatTime(entry.ArchivedAt),
		entry.ArchiveReason, entry.Pinned, clamp01(entry.ValueScore), formatTime(entry.ValueScoreUpdatedAt), formatTime(entry.ValidFrom),
		formatTime(entry.ValidUntil), entry.SupersededBy, entry.Status); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id = ?`, entry.MemoryID); err != nil {
		return nil, err
	}
	if entry.Status == StatusActive {
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_fts(memory_id, title, content, summary, tags, entities) VALUES (?, ?, ?, ?, ?, ?)`,
			entry.MemoryID, entry.Title, entry.Content, entry.Summary, strings.Join(entry.Tags, " "), strings.Join(entry.Entities, " ")); err != nil {
			return nil, err
		}
	}
	if err := upsertMemoryEntityTx(ctx, tx, entry); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	if wasExisting && oldStatus != StatusActive && entry.Status == StatusActive {
		if err := s.reindexMemory(ctx, entry.MemoryID); err != nil {
			return nil, err
		}
	}
	if wasExisting {
		_ = s.recordLifecycleEvent(context.WithoutCancel(ctx), LifecycleEvent{ResourceKind: "memory", ResourceID: entry.MemoryID,
			EventType: "reinforcement", OldStatus: oldStatus, NewStatus: entry.Status,
			OldTemperature: oldTemperature, NewTemperature: TemperatureHot, Score: entry.ValueScore,
			Reasons: []string{"memory_upsert"}, CreatedAt: entry.UpdatedAt})
	}
	s.bumpIndexGeneration(ctx)
	return &entry, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Entry, error) {
	if strings.TrimSpace(id) == "" {
		return nil, sql.ErrNoRows
	}
	entries, err := s.GetMany(ctx, []string{id})
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		return nil, sql.ErrNoRows
	}
	return &entries[0], nil
}

func (s *Store) List(ctx context.Context, opts SearchOptions) ([]Entry, error) {
	if opts.Limit <= 0 {
		opts.Limit = 50
	}
	where, args := buildFilters(opts, false)
	query := `SELECT ` + memoryColumns + ` FROM memories ` + where + ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, opts.Limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntries(rows)
}

func (s *Store) Search(ctx context.Context, opts SearchOptions) ([]Entry, error) {
	if opts.Limit <= 0 {
		opts.Limit = 8
	}
	if opts.MaxCandidates <= 0 {
		opts.MaxCandidates = max(opts.Limit*4, 30)
	}
	where, args := buildFilters(opts, true)
	queryText := strings.TrimSpace(opts.Query)
	var rows *sql.Rows
	var err error
	if queryText != "" {
		ftsQuery := sanitizeFTSQuery(queryText)
		if ftsQuery != "" {
			matchClause := "WHERE memory_fts MATCH ?"
			if strings.TrimSpace(where) != "" {
				matchClause = where + " AND memory_fts MATCH ?"
			}
			q := `SELECT ` + prefixedMemoryColumns("m") + `, bm25(memory_fts) AS rank
				FROM memory_fts JOIN memories m ON m.memory_id = memory_fts.memory_id ` + matchClause + `
				ORDER BY rank ASC LIMIT ?`
			args = append(args, ftsQuery, opts.MaxCandidates)
			rows, err = s.db.QueryContext(ctx, q, args...)
		}
	}
	if rows == nil {
		q := `SELECT ` + memoryColumns + `, 0 AS rank FROM memories ` + where + ` ORDER BY updated_at DESC LIMIT ?`
		args = append(args, opts.MaxCandidates)
		rows, err = s.db.QueryContext(ctx, q, args...)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries, err := scanEntriesWithRank(rows)
	if err != nil {
		return nil, err
	}
	if extra, err := s.supplementImportantCandidates(ctx, opts); err == nil && len(extra) > 0 {
		entries = appendUniqueEntries(entries, extra)
	}
	scoreEntries(entries, opts)
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].Score > entries[j].Score })
	entries = diversifySources(entries, opts.Limit)
	entries = constrainContext(entries, opts.ContextMaxRunes)
	if len(entries) > opts.Limit {
		entries = entries[:opts.Limit]
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		ids = append(ids, entry.MemoryID)
	}
	_ = s.MarkAccess(ctx, ids)
	return entries, nil
}

func diversifySources(entries []Entry, limit int) []Entry {
	if limit <= 1 || len(entries) <= limit {
		return entries
	}
	maxPerSource := 2
	if distinctSourceCount(entries) >= limit {
		maxPerSource = 1
	}
	sourceCounts := map[string]int{}
	selected := make([]Entry, 0, len(entries))
	deferred := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		source := strings.TrimSpace(entry.SourceSessionID)
		if source == "" {
			source = entry.MemoryID
		}
		if len(selected) < limit && sourceCounts[source] < maxPerSource {
			selected = append(selected, entry)
			sourceCounts[source]++
			continue
		}
		deferred = append(deferred, entry)
	}
	selected = append(selected, deferred...)
	return selected
}

func distinctSourceCount(entries []Entry) int {
	sources := map[string]struct{}{}
	for _, entry := range entries {
		source := strings.TrimSpace(entry.SourceSessionID)
		if source == "" {
			source = entry.MemoryID
		}
		sources[source] = struct{}{}
	}
	return len(sources)
}

func (s *Store) supplementImportantCandidates(ctx context.Context, opts SearchOptions) ([]Entry, error) {
	if opts.MaxCandidates <= 0 {
		return nil, nil
	}
	extraLimit := max(4, opts.MaxCandidates/3)
	if extraLimit > opts.MaxCandidates {
		extraLimit = opts.MaxCandidates
	}
	where, args := buildFilters(opts, false)
	query := `SELECT ` + memoryColumns + `, 0 AS rank FROM memories ` + where + ` ORDER BY importance DESC, confidence DESC, updated_at DESC LIMIT ?`
	args = append(args, extraLimit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEntriesWithRank(rows)
}

func appendUniqueEntries(entries []Entry, extra []Entry) []Entry {
	seen := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		seen[entry.MemoryID] = struct{}{}
	}
	for _, entry := range extra {
		if _, ok := seen[entry.MemoryID]; ok {
			continue
		}
		entries = append(entries, entry)
		seen[entry.MemoryID] = struct{}{}
	}
	return entries
}

func (s *Store) MarkAccess(ctx context.Context, ids []string) error {
	return s.RecordAccess(ctx, ids)
}

func (s *Store) SetStatus(ctx context.Context, id string, status Status) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("memory_id is required")
	}
	if status == "" {
		status = StatusArchived
	}
	result, err := s.db.ExecContext(ctx, `UPDATE memories SET status = ?, updated_at = ? WHERE memory_id = ?`, status, formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	if status != StatusActive {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id = ?`, id)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM memory_chunk_fts WHERE chunk_id IN
			(SELECT chunk_id FROM memory_evidence_chunks WHERE parent_memory_id=?)`, id)
		_, _ = s.db.ExecContext(ctx, `UPDATE memory_evidence_chunks SET archived_at=?, archive_reason=?, temperature=?
			WHERE parent_memory_id=?`, formatTime(time.Now().UTC()), "status_"+string(status), TemperatureCold, id)
	} else if err := s.reindexMemory(ctx, id); err != nil {
		return err
	}
	return nil
}

func (s *Store) UpdateImportance(ctx context.Context, id string, importance float64) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("memory_id is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE memories SET importance = ?, updated_at = ? WHERE memory_id = ?`, clamp01(importance), formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (s *Store) Approve(ctx context.Context, id string) error {
	return s.SetStatus(ctx, id, StatusActive)
}

func (s *Store) Restore(ctx context.Context, id string) error {
	return s.RestoreLifecycle(ctx, id)
}

func (s *Store) Deprioritize(ctx context.Context, id string) error {
	return s.UpdateImportance(ctx, id, 0)
}

func (s *Store) SupersedeWith(ctx context.Context, oldID string, candidate Candidate) (*Entry, error) {
	entry, err := s.Upsert(ctx, candidate)
	if err != nil {
		return nil, err
	}
	if err := s.Supersede(ctx, oldID, entry.MemoryID); err != nil {
		return nil, err
	}
	return entry, nil
}

func (s *Store) ApplyRetention(candidate Candidate, policy RetentionPolicy, now time.Time) Candidate {
	if !candidate.RetentionExpiresAt.IsZero() || len(policy) == 0 {
		return candidate
	}
	days, ok := policy[candidate.MemoryType]
	if !ok && candidate.MemoryType == "" {
		days, ok = policy[TypeSemantic]
	}
	if !ok || days <= 0 {
		return candidate
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	candidate.RetentionExpiresAt = now.AddDate(0, 0, days)
	return candidate
}

func ApplyRetention(candidate Candidate, policy RetentionPolicy, now time.Time) Candidate {
	var store *Store
	return store.ApplyRetention(candidate, policy, now)
}

func (s *Store) SetSupersededBy(ctx context.Context, oldID, newID string) error {
	return s.Supersede(ctx, oldID, newID)
}

func (s *Store) UpdateStatus(ctx context.Context, id string, status Status) error {
	return s.SetStatus(ctx, id, status)
}

func (s *Store) UpdateLifecycle(ctx context.Context, id string, validFrom, validUntil time.Time) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("memory_id is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE memories SET valid_from = ?, valid_until = ?, updated_at = ? WHERE memory_id = ?`, formatTime(validFrom), formatTime(validUntil), formatTime(time.Now().UTC()), id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return err
}

func (s *Store) Delete(ctx context.Context, id string, hard bool) error {
	if hard {
		existing, err := s.Get(ctx, id)
		if err != nil {
			return err
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err := insertLifecycleEventTx(ctx, tx, LifecycleEvent{ResourceKind: "memory", ResourceID: id,
			EventType: "hard_delete", OldStatus: existing.Status, NewStatus: StatusDeleted,
			OldTemperature: existing.Temperature, NewTemperature: existing.Temperature,
			Score: existing.ValueScore, CreatedAt: time.Now().UTC()}); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id = ?`, id); err != nil {
			return err
		}
		for _, statement := range []string{
			`DELETE FROM memory_edges WHERE from_id IN (SELECT chunk_id FROM memory_evidence_chunks WHERE parent_memory_id=?) OR to_id IN (SELECT chunk_id FROM memory_evidence_chunks WHERE parent_memory_id=?)`,
			`DELETE FROM memory_chunk_fts WHERE chunk_id IN (SELECT chunk_id FROM memory_evidence_chunks WHERE parent_memory_id=?)`,
			`DELETE FROM memory_chunk_embeddings WHERE chunk_id IN (SELECT chunk_id FROM memory_evidence_chunks WHERE parent_memory_id=?)`,
			`DELETE FROM memory_chunk_entities WHERE chunk_id IN (SELECT chunk_id FROM memory_evidence_chunks WHERE parent_memory_id=?)`,
		} {
			if strings.Contains(statement, " OR ") {
				if _, err := tx.ExecContext(ctx, statement, id, id); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, statement, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_evidence_chunks WHERE parent_memory_id=?`, id); err != nil {
			return err
		}
		for _, statement := range []string{
			`DELETE FROM memory_embeddings WHERE memory_id = ?`,
			`DELETE FROM memory_evidence_spans WHERE memory_id = ?`,
			`DELETE FROM memory_facts WHERE memory_id = ?`,
			`DELETE FROM memory_edges WHERE from_id = ? OR to_id = ?`,
		} {
			if strings.Contains(statement, " OR ") {
				if _, err := tx.ExecContext(ctx, statement, id, id); err != nil {
					return err
				}
				continue
			}
			if _, err := tx.ExecContext(ctx, statement, id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM memories WHERE memory_id = ?`, id); err != nil {
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		s.bumpIndexGeneration(ctx)
		return nil
	}
	entry, getErr := s.Get(ctx, id)
	if getErr != nil {
		return getErr
	}
	err := s.SetStatus(ctx, id, StatusDeleted)
	if err == nil {
		_ = s.recordLifecycleEvent(context.WithoutCancel(ctx), LifecycleEvent{ResourceKind: "memory", ResourceID: id,
			EventType: "soft_delete", OldStatus: entry.Status, NewStatus: StatusDeleted,
			OldTemperature: entry.Temperature, NewTemperature: entry.Temperature, Score: entry.ValueScore, CreatedAt: time.Now().UTC()})
		s.bumpIndexGeneration(ctx)
	}
	return err
}

func (s *Store) Supersede(ctx context.Context, oldID, newID string) error {
	if strings.TrimSpace(oldID) == "" || strings.TrimSpace(newID) == "" {
		return errors.New("old and new memory ids are required")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE memories SET status = ?, superseded_by = ?, updated_at = ? WHERE memory_id = ?`, StatusSuperseded, newID, formatTime(time.Now().UTC()), oldID)
	if err == nil {
		_, _ = s.db.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id = ?`, oldID)
		_, _ = s.db.ExecContext(ctx, `DELETE FROM memory_chunk_fts WHERE chunk_id IN
			(SELECT chunk_id FROM memory_evidence_chunks WHERE parent_memory_id=?)`, oldID)
		_, _ = s.db.ExecContext(ctx, `UPDATE memory_evidence_chunks SET archived_at=?, archive_reason=?, temperature=?
			WHERE parent_memory_id=?`, formatTime(time.Now().UTC()), "superseded", TemperatureCold, oldID)
		_ = s.recordLifecycleEvent(context.WithoutCancel(ctx), LifecycleEvent{ResourceKind: "memory", ResourceID: oldID,
			EventType: "supersede", OldStatus: StatusActive, NewStatus: StatusSuperseded,
			Reasons: []string{"superseded_by:" + newID}, CreatedAt: time.Now().UTC()})
	}
	return err
}

func (s *Store) RecordUsed(ctx context.Context, record UsedRecord) error {
	record.CreatedAt = time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `INSERT INTO memory_used(session_id, team_session_id, agent_id, query, memory_ids_json, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		record.SessionID, record.TeamSessionID, record.AgentID, record.Query, toJSON(record.MemoryIDs), formatTime(record.CreatedAt))
	return err
}

func (s *Store) ListUsed(ctx context.Context, limit int) ([]UsedRecord, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT session_id, team_session_id, agent_id, query, memory_ids_json, created_at FROM memory_used ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UsedRecord
	for rows.Next() {
		var record UsedRecord
		var idsJSON string
		var created string
		if err := rows.Scan(&record.SessionID, &record.TeamSessionID, &record.AgentID, &record.Query, &idsJSON, &created); err != nil {
			return nil, err
		}
		record.MemoryIDs = fromJSONList(idsJSON)
		record.CreatedAt = parseTime(created)
		out = append(out, record)
	}
	return out, rows.Err()
}

func normalizeCandidate(c Candidate) Entry {
	scopeType := c.ScopeType
	if scopeType == "" {
		scopeType = ScopeProject
	}
	scopeKey := strings.TrimSpace(c.ScopeKey)
	if scopeKey == "" {
		scopeKey = "default"
	}
	memoryType := c.MemoryType
	if memoryType == "" {
		memoryType = TypeSemantic
	}
	status := c.Status
	if status == "" {
		status = StatusActive
	}
	title := strings.TrimSpace(c.Title)
	if title == "" {
		title = firstLine(c.Summary)
	}
	if title == "" {
		title = firstLine(c.Content)
	}
	if title == "" {
		title = "Untitled memory"
	}
	content := strings.TrimSpace(c.Content)
	if content == "" {
		content = strings.TrimSpace(c.Summary)
	}
	return Entry{
		MemoryID:            strings.TrimSpace(c.MemoryID),
		ScopeType:           scopeType,
		ScopeKey:            scopeKey,
		MemoryType:          memoryType,
		Title:               title,
		Content:             content,
		Summary:             strings.TrimSpace(c.Summary),
		Tags:                normalizeStrings(c.Tags),
		Entities:            normalizeStrings(c.Entities),
		Importance:          defaultFloat(c.Importance, 0.5),
		Confidence:          defaultFloat(c.Confidence, 0.5),
		SourceSessionID:     strings.TrimSpace(c.SourceSessionID),
		SourceMessageIDs:    normalizeStrings(c.SourceMessageIDs),
		SourceAgentID:       strings.TrimSpace(c.SourceAgentID),
		SourceTeamSessionID: strings.TrimSpace(c.SourceTeamSessionID),
		SourcePaths:         normalizeStrings(c.SourcePaths),
		ValidFrom:           c.ValidFrom,
		ValidUntil:          c.ValidUntil,
		RetentionExpiresAt:  c.RetentionExpiresAt,
		Temperature:         TemperatureHot,
		ValueScore:          0.5,
		Status:              status,
	}
}

const memoryColumns = `memory_id, scope_type, scope_key, memory_type, title, content, summary, tags_json, entities_json, importance, confidence, source_session_id, source_message_ids_json, source_agent_id, source_team_session_id, source_paths_json, created_at, updated_at, last_accessed_at, temperature, retention_expires_at, access_count, last_reinforced_at, archived_at, archive_reason, pinned, value_score, value_score_updated_at, valid_from, valid_until, superseded_by, status`

func prefixedMemoryColumns(prefix string) string {
	parts := strings.Split(memoryColumns, ", ")
	for i, part := range parts {
		parts[i] = prefix + "." + part
	}
	return strings.Join(parts, ", ")
}

func (s *Store) getTx(ctx context.Context, tx *sql.Tx, id string) (*Entry, error) {
	row := tx.QueryRowContext(ctx, `SELECT `+memoryColumns+` FROM memories WHERE memory_id = ?`, id)
	return scanEntry(row)
}

func buildFilters(opts SearchOptions, prefixed bool) (string, []any) {
	prefix := ""
	if prefixed {
		prefix = "m."
	}
	var clauses []string
	var args []any
	if !opts.IncludeInactive {
		clauses = append(clauses, prefix+"status = ?")
		args = append(args, StatusActive)
	}
	if !opts.IncludeExpired {
		now := formatTime(time.Now().UTC())
		clauses = append(clauses, "("+prefix+"valid_from = '' OR "+prefix+"valid_from <= ?)")
		args = append(args, now)
		clauses = append(clauses, "("+prefix+"valid_until = '' OR "+prefix+"valid_until >= ?)")
		args = append(args, now)
	}
	if len(opts.Scopes) > 0 {
		var scopeParts []string
		for _, scope := range opts.Scopes {
			if scope.Type == "" || scope.Key == "" {
				continue
			}
			scopeParts = append(scopeParts, "("+prefix+"scope_type = ? AND "+prefix+"scope_key = ?)")
			args = append(args, scope.Type, scope.Key)
		}
		if len(scopeParts) > 0 {
			clauses = append(clauses, "("+strings.Join(scopeParts, " OR ")+")")
		}
	}
	if len(opts.Types) > 0 {
		var marks []string
		for _, t := range opts.Types {
			marks = append(marks, "?")
			args = append(args, t)
		}
		clauses = append(clauses, prefix+"memory_type IN ("+strings.Join(marks, ",")+")")
	}
	for _, tag := range normalizeStrings(opts.Tags) {
		clauses = append(clauses, prefix+"tags_json LIKE ?")
		args = append(args, "%"+tag+"%")
	}
	if !opts.CreatedAfter.IsZero() {
		clauses = append(clauses, prefix+"created_at >= ?")
		args = append(args, formatTime(opts.CreatedAfter))
	}
	if !opts.CreatedBefore.IsZero() {
		clauses = append(clauses, prefix+"created_at <= ?")
		args = append(args, formatTime(opts.CreatedBefore))
	}
	if len(opts.ExcludeIDs) > 0 {
		var marks []string
		for id := range opts.ExcludeIDs {
			if strings.TrimSpace(id) == "" {
				continue
			}
			marks = append(marks, "?")
			args = append(args, id)
		}
		if len(marks) > 0 {
			clauses = append(clauses, prefix+"memory_id NOT IN ("+strings.Join(marks, ",")+")")
		}
	}
	if len(clauses) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

func constrainContext(entries []Entry, maxRunes int) []Entry {
	if maxRunes <= 0 || len(entries) == 0 {
		return entries
	}
	var out []Entry
	used := 0
	for _, entry := range entries {
		cost := memoryContextRunes(entry)
		if used+cost > maxRunes {
			if len(out) == 0 {
				entry.Content = clipRunes(entry.Content, maxRunes)
				out = append(out, entry)
			}
			break
		}
		out = append(out, entry)
		used += cost
	}
	return out
}

func memoryContextRunes(entry Entry) int {
	return len([]rune(entry.Title)) + len([]rune(entry.Summary)) + len([]rune(entry.Content)) + len(strings.Join(entry.Tags, " ")) + len(strings.Join(entry.Entities, " ")) + 160
}

func clipRunes(text string, maxRunes int) string {
	runes := []rune(text)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return text
	}
	if maxRunes <= 20 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-20]) + "\n...[truncated]"
}

func scanEntry(row interface{ Scan(dest ...any) error }) (*Entry, error) {
	var e Entry
	var tags, entities, messageIDs, sourcePaths string
	var created, updated, accessed, retentionExpires, reinforced, archived, valueUpdated, validFrom, validUntil string
	if err := row.Scan(&e.MemoryID, &e.ScopeType, &e.ScopeKey, &e.MemoryType, &e.Title, &e.Content, &e.Summary, &tags, &entities, &e.Importance, &e.Confidence, &e.SourceSessionID, &messageIDs, &e.SourceAgentID, &e.SourceTeamSessionID, &sourcePaths, &created, &updated, &accessed, &e.Temperature, &retentionExpires, &e.AccessCount, &reinforced, &archived, &e.ArchiveReason, &e.Pinned, &e.ValueScore, &valueUpdated, &validFrom, &validUntil, &e.SupersededBy, &e.Status); err != nil {
		return nil, err
	}
	e.Tags = fromJSONList(tags)
	e.Entities = fromJSONList(entities)
	e.SourceMessageIDs = fromJSONList(messageIDs)
	e.SourcePaths = fromJSONList(sourcePaths)
	e.CreatedAt = parseTime(created)
	e.UpdatedAt = parseTime(updated)
	e.LastAccessedAt = parseTime(accessed)
	e.RetentionExpiresAt = parseTime(retentionExpires)
	e.LastReinforcedAt = parseTime(reinforced)
	e.ArchivedAt = parseTime(archived)
	e.ValueScoreUpdatedAt = parseTime(valueUpdated)
	e.ValidFrom = parseTime(validFrom)
	e.ValidUntil = parseTime(validUntil)
	return &e, nil
}

func scanEntries(rows *sql.Rows) ([]Entry, error) {
	var entries []Entry
	for rows.Next() {
		entry, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, *entry)
	}
	return entries, rows.Err()
}

func scanEntriesWithRank(rows *sql.Rows) ([]Entry, error) {
	var entries []Entry
	for rows.Next() {
		var e Entry
		var tags, entities, messageIDs, sourcePaths string
		var created, updated, accessed, retentionExpires, reinforced, archived, valueUpdated, validFrom, validUntil string
		var rank float64
		if err := rows.Scan(&e.MemoryID, &e.ScopeType, &e.ScopeKey, &e.MemoryType, &e.Title, &e.Content, &e.Summary, &tags, &entities, &e.Importance, &e.Confidence, &e.SourceSessionID, &messageIDs, &e.SourceAgentID, &e.SourceTeamSessionID, &sourcePaths, &created, &updated, &accessed, &e.Temperature, &retentionExpires, &e.AccessCount, &reinforced, &archived, &e.ArchiveReason, &e.Pinned, &e.ValueScore, &valueUpdated, &validFrom, &validUntil, &e.SupersededBy, &e.Status, &rank); err != nil {
			return nil, err
		}
		e.Tags = fromJSONList(tags)
		e.Entities = fromJSONList(entities)
		e.SourceMessageIDs = fromJSONList(messageIDs)
		e.SourcePaths = fromJSONList(sourcePaths)
		e.CreatedAt = parseTime(created)
		e.UpdatedAt = parseTime(updated)
		e.LastAccessedAt = parseTime(accessed)
		e.RetentionExpiresAt = parseTime(retentionExpires)
		e.LastReinforcedAt = parseTime(reinforced)
		e.ArchivedAt = parseTime(archived)
		e.ValueScoreUpdatedAt = parseTime(valueUpdated)
		e.ValidFrom = parseTime(validFrom)
		e.ValidUntil = parseTime(validUntil)
		e.Score = -rank
		entries = append(entries, e)
	}
	return entries, rows.Err()
}

func scoreEntries(entries []Entry, opts SearchOptions) {
	now := time.Now().UTC()
	scopeWeights := map[string]float64{}
	for idx, scope := range opts.Scopes {
		scopeWeights[string(scope.Type)+"\x00"+scope.Key] = float64(len(opts.Scopes)-idx) * 0.12
	}
	query := strings.ToLower(opts.Query)
	for i := range entries {
		e := &entries[i]
		score := e.Score
		score += clamp01(e.Importance) * 1.8
		score += clamp01(e.Confidence) * 0.9
		if w := scopeWeights[string(e.ScopeType)+"\x00"+e.ScopeKey]; w > 0 {
			score += w
		}
		ageDays := now.Sub(e.UpdatedAt).Hours() / 24
		if ageDays < 0 {
			ageDays = 0
		}
		score += 1.0 / (1.0 + ageDays/14.0)
		if query != "" {
			haystack := strings.ToLower(e.Title + " " + e.Summary + " " + strings.Join(e.Tags, " ") + " " + strings.Join(e.Entities, " "))
			for _, token := range strings.Fields(query) {
				if len([]rune(token)) >= 2 && strings.Contains(haystack, token) {
					score += 0.3
				}
			}
		}
		e.Score = math.Round(score*1000) / 1000
	}
}

func sanitizeFTSQuery(query string) string {
	var terms, fallback []string
	for _, token := range strings.FieldsFunc(query, func(r rune) bool {
		return !(r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '@' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127)
	}) {
		token = strings.TrimSpace(strings.ReplaceAll(token, `"`, ""))
		if token != "" {
			quoted := `"` + token + `"`
			fallback = append(fallback, quoted)
			if !isRetrievalStopword(token) {
				terms = append(terms, quoted)
			}
		}
	}
	if len(terms) == 0 {
		terms = fallback
	}
	return strings.Join(terms, " OR ")
}

var retrievalStopwords = map[string]struct{}{
	"a": {}, "an": {}, "the": {}, "is": {}, "are": {}, "was": {}, "were": {}, "be": {}, "been": {}, "being": {},
	"do": {}, "does": {}, "did": {}, "have": {}, "has": {}, "had": {}, "i": {}, "me": {}, "my": {}, "mine": {},
	"you": {}, "your": {}, "yours": {}, "we": {}, "our": {}, "ours": {}, "he": {}, "she": {}, "it": {}, "its": {},
	"they": {}, "their": {}, "them": {}, "this": {}, "that": {}, "these": {}, "those": {}, "what": {}, "which": {},
	"who": {}, "whom": {}, "whose": {}, "where": {}, "when": {}, "why": {}, "how": {}, "of": {}, "in": {}, "on": {},
	"at": {}, "for": {}, "from": {}, "by": {}, "with": {}, "and": {}, "or": {}, "but": {}, "if": {}, "then": {},
	"than": {}, "to": {}, "as": {},
}

func isRetrievalStopword(value string) bool {
	_, ok := retrievalStopwords[strings.ToLower(strings.TrimSpace(value))]
	return ok
}

func toJSON(values []string) string {
	data, _ := json.Marshal(normalizeStrings(values))
	return string(data)
}

func fromJSONList(raw string) []string {
	var out []string
	_ = json.Unmarshal([]byte(raw), &out)
	return normalizeStrings(out)
}

func normalizeStrings(values []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	line, _, _ := strings.Cut(text, "\n")
	if len([]rune(line)) > 80 {
		line = string([]rune(line)[:80])
	}
	return strings.TrimSpace(line)
}

func defaultFloat(value, fallback float64) float64 {
	if value <= 0 {
		return fallback
	}
	return value
}

func clamp01(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(text string) time.Time {
	if strings.TrimSpace(text) == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, text)
	return t
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func IsNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}

func (e Entry) ShortLine() string {
	return fmt.Sprintf("%s [%s/%s] %s", e.MemoryID, e.ScopeType, e.MemoryType, e.Title)
}
