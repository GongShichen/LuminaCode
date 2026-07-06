package sessionmemory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"LuminaCode/api"
	"LuminaCode/config"

	_ "modernc.org/sqlite"
)

const schemaVersion = "1"

type Store struct {
	db        *sql.DB
	cfg       config.Config
	sessionID string
	path      string
	complete  SummaryFunc
}

type SummaryFunc func(ctx context.Context, systemPrompt string, messages []map[string]any, maxTokens int) (string, error)

type CommitJob struct {
	StartTurn int
	EndTurn   int
}

type Manager struct {
	mu       sync.Mutex
	workers  map[string]*worker
	complete SummaryFunc
}

type worker struct {
	manager   *Manager
	sessionID string
	cfg       config.Config
	jobs      chan CommitJob
	queuedEnd int
}

func NewManager() *Manager {
	return &Manager{workers: map[string]*worker{}}
}

func NewManagerWithSummaryFunc(complete SummaryFunc) *Manager {
	return &Manager{workers: map[string]*worker{}, complete: complete}
}

func (m *Manager) Observe(ctx context.Context, cfg config.Config, sessionID string, messages []map[string]any, force bool) error {
	if m == nil || !cfg.SessionMemoryEnabled || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	store, err := Open(ctx, cfg, sessionID, nil)
	if err != nil {
		return err
	}
	if err := store.IngestMessages(ctx, messages); err != nil {
		_ = store.Close()
		return err
	}
	maxTurn := store.maxUserTurn(ctx)
	lastCommitted := store.metaInt(ctx, "last_committed_user_turn")
	_ = store.Close()

	m.mu.Lock()
	w := m.workers[sessionID]
	if w == nil {
		w = &worker{
			manager:   m,
			sessionID: sessionID,
			cfg:       cfg,
			jobs:      make(chan CommitJob, 256),
			queuedEnd: lastCommitted,
		}
		m.workers[sessionID] = w
		go w.run()
	}
	w.cfg = cfg
	if w.queuedEnd < lastCommitted {
		w.queuedEnd = lastCommitted
	}
	interval := cfg.SessionMemoryTurnInterval
	if interval <= 0 {
		interval = 5
	}
	for maxTurn >= w.queuedEnd+interval || (force && maxTurn > w.queuedEnd) {
		end := w.queuedEnd + interval
		if force && maxTurn < end {
			end = maxTurn
		}
		job := CommitJob{StartTurn: w.queuedEnd + 1, EndTurn: end}
		select {
		case w.jobs <- job:
			w.queuedEnd = end
		default:
			slog.Warn("session memory queue is full; dropping commit job", "session_id", sessionID, "start_turn", job.StartTurn, "end_turn", job.EndTurn)
			m.mu.Unlock()
			return fmt.Errorf("session memory queue is full")
		}
		if !force && maxTurn < w.queuedEnd+interval {
			break
		}
	}
	m.mu.Unlock()
	return nil
}

func (w *worker) run() {
	for job := range w.jobs {
		ctx := context.Background()
		store, err := Open(ctx, w.cfg, w.sessionID, w.manager.complete)
		if err == nil {
			err = store.CommitRange(ctx, job.StartTurn, job.EndTurn)
			if err == nil {
				err = store.Evict(ctx)
			}
			_ = store.Close()
		}
		if err != nil {
			slog.Warn("session memory commit failed", "session_id", w.sessionID, "start_turn", job.StartTurn, "end_turn", job.EndTurn, "error", err)
			w.manager.resetWorker(w.sessionID, job.StartTurn-1)
			return
		}
	}
}

func (m *Manager) resetWorker(sessionID string, queuedEnd int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	if w := m.workers[sessionID]; w != nil {
		w.queuedEnd = queuedEnd
		delete(m.workers, sessionID)
	}
	m.mu.Unlock()
}

type CommitListItem struct {
	CommitNo       int     `json:"commit_no"`
	Title          string  `json:"title"`
	SummaryPreview string  `json:"summary_preview"`
	StartMessageID int64   `json:"start_message_id"`
	EndMessageID   int64   `json:"end_message_id"`
	StartTurnCount int     `json:"start_turn_count"`
	EndTurnCount   int     `json:"end_turn_count"`
	CreatedAt      float64 `json:"created_at"`
}

type MessageSnippet struct {
	ID            int64   `json:"id"`
	Role          string  `json:"role"`
	UserTurnCount int     `json:"user_turn_count"`
	TextPreview   string  `json:"text_preview"`
	Content       any     `json:"content,omitempty"`
	CreatedAt     float64 `json:"created_at"`
}

type CommitDetail struct {
	CommitNo        int              `json:"commit_no"`
	Title           string           `json:"title"`
	Summary         string           `json:"summary"`
	Tags            []string         `json:"tags"`
	StartMessageID  int64            `json:"start_message_id"`
	EndMessageID    int64            `json:"end_message_id"`
	StartTurnCount  int              `json:"start_turn_count"`
	EndTurnCount    int              `json:"end_turn_count"`
	CreatedAt       float64          `json:"created_at"`
	Messages        []MessageSnippet `json:"messages,omitempty"`
	OmittedMessages int              `json:"omitted_messages,omitempty"`
}

func Open(ctx context.Context, cfg config.Config, sessionID string, complete SummaryFunc) (*Store, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("session id is required")
	}
	if cfg.SessionDir == "" {
		cfg = config.NewConfigForCWD(cfg.CWD)
	}
	if err := os.MkdirAll(cfg.SessionDir, 0o755); err != nil {
		return nil, err
	}
	path := sessionSQLitePath(cfg.SessionDir, sessionID)
	if err := migrateLegacySQLite(cfg.SessionDir, sessionID, path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	store := &Store{db: db, cfg: cfg, sessionID: sessionID, path: path, complete: complete}
	if err := store.init(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Path() string { return s.path }

func SyncAndMaybeCommit(ctx context.Context, cfg config.Config, sessionID string, messages []map[string]any, force bool) error {
	if !cfg.SessionMemoryEnabled || strings.TrimSpace(sessionID) == "" {
		return nil
	}
	store, err := Open(ctx, cfg, sessionID, nil)
	if err != nil {
		return err
	}
	defer store.Close()
	if err := store.IngestMessages(ctx, messages); err != nil {
		return err
	}
	if err := store.MaybeCommit(ctx, force); err != nil {
		return err
	}
	return store.Evict(ctx)
}

func (s *Store) init(ctx context.Context) error {
	stmts := []string{
		`PRAGMA journal_mode=WAL`,
		`CREATE TABLE IF NOT EXISTS schema_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS session_info(session_id TEXT PRIMARY KEY, created_at REAL, updated_at REAL, cwd TEXT, model TEXT)`,
		`CREATE TABLE IF NOT EXISTS messages(id INTEGER PRIMARY KEY AUTOINCREMENT, turn_count INTEGER, user_turn_count INTEGER, role TEXT, content_json TEXT NOT NULL, text_preview TEXT, message_hash TEXT UNIQUE, created_at REAL, last_accessed_at REAL)`,
		`CREATE TABLE IF NOT EXISTS commits(id INTEGER PRIMARY KEY AUTOINCREMENT, commit_no INTEGER UNIQUE, start_message_id INTEGER, end_message_id INTEGER, start_turn_count INTEGER, end_turn_count INTEGER, summary TEXT NOT NULL, title TEXT, tags_json TEXT, model TEXT, input_tokens INTEGER, output_tokens INTEGER, created_at REAL, last_accessed_at REAL)`,
		`CREATE TABLE IF NOT EXISTS commit_messages(commit_id INTEGER, message_id INTEGER, PRIMARY KEY(commit_id, message_id))`,
		`CREATE INDEX IF NOT EXISTS idx_messages_hash ON messages(message_hash)`,
		`CREATE INDEX IF NOT EXISTS idx_messages_user_turn ON messages(user_turn_count, id)`,
		`CREATE INDEX IF NOT EXISTS idx_commits_no ON commits(commit_no)`,
	}
	for _, stmt := range stmts {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if _, err := s.db.ExecContext(ctx, `CREATE VIRTUAL TABLE IF NOT EXISTS commit_fts USING fts5(commit_id UNINDEXED, title, summary, tags)`); err == nil {
		_, _ = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO schema_meta(key, value) VALUES('fts_available', 'true')`)
	} else {
		_, _ = s.db.ExecContext(ctx, `INSERT OR REPLACE INTO schema_meta(key, value) VALUES('fts_available', 'false')`)
	}
	now := unixNow()
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO schema_meta(key, value) VALUES('schema_version', ?)`, schemaVersion)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO session_info(session_id, created_at, updated_at, cwd, model)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(session_id) DO UPDATE SET updated_at=excluded.updated_at, cwd=excluded.cwd, model=excluded.model`,
		s.sessionID, now, now, s.cfg.CWD, s.cfg.APIModel)
	return err
}

func (s *Store) IngestMessages(ctx context.Context, messages []map[string]any) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	userTurn := 0
	for _, msg := range messages {
		if isTransientMessage(msg) || isCompactionHandoffMessage(msg) {
			continue
		}
		if taggedTurn := sessionUserTurn(msg); taggedTurn > 0 {
			userTurn = taggedTurn
		} else if isRealUserRequest(msg) {
			userTurn++
		}
		if userTurn == 0 {
			continue
		}
		role := stringValue(msg["role"])
		contentJSON, err := json.Marshal(msg["content"])
		if err != nil {
			continue
		}
		hash := messageHash(userTurn, role, contentJSON)
		preview := textPreview(msg, 1200)
		now := unixNow()
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO messages(turn_count, user_turn_count, role, content_json, text_preview, message_hash, created_at, last_accessed_at)
			VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, userTurn, userTurn, role, string(contentJSON), preview, hash, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) MaybeCommit(ctx context.Context, force bool) error {
	interval := s.cfg.SessionMemoryTurnInterval
	if interval <= 0 {
		interval = 5
	}
	lastCommitted := s.metaInt(ctx, "last_committed_user_turn")
	maxUserTurn := s.maxUserTurn(ctx)
	if maxUserTurn <= lastCommitted {
		return nil
	}
	for maxUserTurn >= lastCommitted+interval || (force && maxUserTurn > lastCommitted) {
		end := lastCommitted + interval
		if force && maxUserTurn < end {
			end = maxUserTurn
		}
		if err := s.CommitRange(ctx, lastCommitted+1, end); err != nil {
			return err
		}
		lastCommitted = end
		if !force && maxUserTurn < lastCommitted+interval {
			break
		}
	}
	return nil
}

func (s *Store) CommitRange(ctx context.Context, startTurn, endTurn int) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, role, content_json, text_preview, created_at FROM messages WHERE user_turn_count >= ? AND user_turn_count <= ? ORDER BY id`, startTurn, endTurn)
	if err != nil {
		return err
	}
	var ids []int64
	var previews []string
	var startID, endID int64
	for rows.Next() {
		var id int64
		var role, contentJSON, preview string
		var created float64
		if err := rows.Scan(&id, &role, &contentJSON, &preview, &created); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
		if startID == 0 {
			startID = id
		}
		endID = id
		previews = append(previews, fmt.Sprintf("[%s] %s", role, clamp(preview, 1500)))
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	summary, title, tags, err := s.summarize(ctx, startTurn, endTurn, previews)
	if err != nil {
		return err
	}
	now := unixNow()
	commitNo := s.metaInt(ctx, "next_commit_no")
	if commitNo <= 0 {
		commitNo = 1
	}
	ftsAvailable := s.ftsAvailable(ctx)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	tagJSON, _ := json.Marshal(tags)
	res, err := tx.ExecContext(ctx, `INSERT INTO commits(commit_no, start_message_id, end_message_id, start_turn_count, end_turn_count, summary, title, tags_json, model, input_tokens, output_tokens, created_at, last_accessed_at)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?)`, commitNo, startID, endID, startTurn, endTurn, summary, title, string(tagJSON), s.summaryModel(), now, now)
	if err != nil {
		return err
	}
	commitID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO commit_messages(commit_id, message_id) VALUES(?, ?)`, commitID, id); err != nil {
			return err
		}
	}
	if ftsAvailable {
		_, _ = tx.ExecContext(ctx, `INSERT INTO commit_fts(commit_id, title, summary, tags) VALUES(?, ?, ?, ?)`, commitID, title, summary, strings.Join(tags, " "))
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO schema_meta(key, value) VALUES('last_committed_user_turn', ?)`, fmt.Sprint(endTurn)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO schema_meta(key, value) VALUES('next_commit_no', ?)`, fmt.Sprint(commitNo+1)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) summarize(ctx context.Context, startTurn, endTurn int, previews []string) (string, string, []string, error) {
	joined := strings.Join(previews, "\n")
	system := "You summarize a coding-agent conversation segment. Return concise JSON only with keys: title, summary, tags. The summary must preserve user goals, decisions, files, tool findings, errors, and next steps. Do not invent facts."
	user := fmt.Sprintf("Summarize session turns %d-%d. This is one commit covering the whole interval, not separate summaries per turn.\n\n%s", startTurn, endTurn, joined)
	complete := s.complete
	if complete == nil {
		client, err := api.CreateLLMClient(s.cfg.APIKey, s.cfg.APIBaseURL, s.summaryModel(), s.cfg.SessionMemorySummaryMaxTokens, nil, api.DefaultRetryConfigPtr(), s.cfg.APIType)
		if err != nil {
			return "", "", nil, err
		}
		complete = func(ctx context.Context, systemPrompt string, messages []map[string]any, maxTokens int) (string, error) {
			return client.Complete(ctx, systemPrompt, messages, api.CompleteOptions{MaxTokens: maxTokens})
		}
	}
	text, err := complete(ctx, system, []map[string]any{{"role": "user", "content": []map[string]any{{"type": "text", "text": user}}}}, s.cfg.SessionMemorySummaryMaxTokens)
	if err != nil {
		return "", "", nil, err
	}
	title, summary, tags := parseSummaryResponse(text)
	if strings.TrimSpace(summary) == "" {
		summary = strings.TrimSpace(text)
	}
	if strings.TrimSpace(title) == "" {
		title = firstLine(summary)
	}
	if strings.TrimSpace(title) == "" {
		title = fmt.Sprintf("Session turns %d-%d", startTurn, endTurn)
	}
	return summary, clamp(title, 160), tags, nil
}

func (s *Store) summaryModel() string {
	if strings.TrimSpace(s.cfg.SessionMemorySummaryModel) != "" {
		return strings.TrimSpace(s.cfg.SessionMemorySummaryModel)
	}
	return s.cfg.APIModel
}

func (s *Store) ListCommits(ctx context.Context, query string, limit int) ([]CommitListItem, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	query = strings.TrimSpace(query)
	var rows *sql.Rows
	var err error
	if query != "" && s.ftsAvailable(ctx) {
		rows, err = s.db.QueryContext(ctx, `SELECT c.commit_no, c.title, c.summary, c.start_message_id, c.end_message_id, c.start_turn_count, c.end_turn_count, c.created_at
			FROM commits c JOIN commit_fts f ON f.commit_id = c.id WHERE commit_fts MATCH ? ORDER BY c.commit_no DESC LIMIT ?`, query, limit)
	}
	if rows == nil || err != nil {
		if rows != nil {
			rows.Close()
		}
		like := "%" + query + "%"
		if query == "" {
			rows, err = s.db.QueryContext(ctx, `SELECT commit_no, title, summary, start_message_id, end_message_id, start_turn_count, end_turn_count, created_at FROM commits ORDER BY commit_no DESC LIMIT ?`, limit)
		} else {
			rows, err = s.db.QueryContext(ctx, `SELECT commit_no, title, summary, start_message_id, end_message_id, start_turn_count, end_turn_count, created_at FROM commits WHERE title LIKE ? OR summary LIKE ? OR tags_json LIKE ? ORDER BY commit_no DESC LIMIT ?`, like, like, like, limit)
		}
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []CommitListItem
	var commitNos []int
	for rows.Next() {
		var item CommitListItem
		var summary string
		if err := rows.Scan(&item.CommitNo, &item.Title, &summary, &item.StartMessageID, &item.EndMessageID, &item.StartTurnCount, &item.EndTurnCount, &item.CreatedAt); err != nil {
			return nil, err
		}
		item.SummaryPreview = clamp(summary, 300)
		items = append(items, item)
		commitNos = append(commitNos, item.CommitNo)
	}
	s.touchCommits(ctx, commitNos)
	return items, rows.Err()
}

func (s *Store) GetCommit(ctx context.Context, commitNo int, includeMessages bool) (CommitDetail, error) {
	var detail CommitDetail
	var tagsJSON string
	err := s.db.QueryRowContext(ctx, `SELECT commit_no, title, summary, tags_json, start_message_id, end_message_id, start_turn_count, end_turn_count, created_at FROM commits WHERE commit_no = ?`, commitNo).
		Scan(&detail.CommitNo, &detail.Title, &detail.Summary, &tagsJSON, &detail.StartMessageID, &detail.EndMessageID, &detail.StartTurnCount, &detail.EndTurnCount, &detail.CreatedAt)
	if err != nil {
		return detail, err
	}
	_ = json.Unmarshal([]byte(tagsJSON), &detail.Tags)
	s.touchCommits(ctx, []int{commitNo})
	if !includeMessages {
		return detail, nil
	}
	messages, omitted, err := s.commitMessages(ctx, commitNo, s.cfg.SessionHistoryGetMessageLimit)
	if err != nil {
		return detail, err
	}
	detail.Messages = messages
	detail.OmittedMessages = omitted
	return detail, nil
}

func (s *Store) commitMessages(ctx context.Context, commitNo, limit int) ([]MessageSnippet, int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT m.id, m.role, m.user_turn_count, m.text_preview, m.content_json, m.created_at
		FROM messages m JOIN commit_messages cm ON cm.message_id = m.id JOIN commits c ON c.id = cm.commit_id
		WHERE c.commit_no = ? ORDER BY m.id`, commitNo)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var all []MessageSnippet
	for rows.Next() {
		var msg MessageSnippet
		var contentJSON string
		if err := rows.Scan(&msg.ID, &msg.Role, &msg.UserTurnCount, &msg.TextPreview, &contentJSON, &msg.CreatedAt); err != nil {
			return nil, 0, err
		}
		_ = json.Unmarshal([]byte(contentJSON), &msg.Content)
		all = append(all, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	selected := limitMessages(all, limit)
	var ids []int64
	for _, msg := range selected {
		ids = append(ids, msg.ID)
	}
	s.touchMessages(ctx, ids)
	return selected, len(all) - len(selected), nil
}

func (s *Store) Evict(ctx context.Context) error {
	if s.cfg.SessionMemoryMaxCommits > 0 {
		if err := s.evictCommits(ctx, s.cfg.SessionMemoryMaxCommits); err != nil {
			return err
		}
	}
	if s.cfg.SessionMemoryMaxMessages > 0 {
		if err := s.evictMessages(ctx, s.cfg.SessionMemoryMaxMessages); err != nil {
			return err
		}
	}
	if s.cfg.SessionMemoryVacuumAfterEviction {
		_, _ = s.db.ExecContext(ctx, `VACUUM`)
	}
	return nil
}

func (s *Store) evictCommits(ctx context.Context, maxCommits int) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM commits`).Scan(&count); err != nil {
		return err
	}
	extra := count - maxCommits
	if extra <= 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM commits ORDER BY last_accessed_at ASC, id ASC LIMIT ?`, extra)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM commit_messages WHERE commit_id = ?`, id); err != nil {
			return err
		}
		if s.ftsAvailable(ctx) {
			_, _ = s.db.ExecContext(ctx, `DELETE FROM commit_fts WHERE commit_id = ?`, id)
		}
		if _, err := s.db.ExecContext(ctx, `DELETE FROM commits WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) evictMessages(ctx context.Context, maxMessages int) error {
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM messages`).Scan(&count); err != nil {
		return err
	}
	extra := count - maxMessages
	if extra <= 0 {
		return nil
	}
	lastCommitted := s.metaInt(ctx, "last_committed_user_turn")
	rows, err := s.db.QueryContext(ctx, `SELECT m.id FROM messages m
		LEFT JOIN commit_messages cm ON cm.message_id = m.id
		WHERE cm.message_id IS NULL AND m.user_turn_count <= ?
		ORDER BY m.last_accessed_at ASC, m.id ASC LIMIT ?`, lastCommitted, extra)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM messages WHERE id = ?`, id); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) maxUserTurn(ctx context.Context) int {
	var value sql.NullInt64
	_ = s.db.QueryRowContext(ctx, `SELECT MAX(user_turn_count) FROM messages`).Scan(&value)
	if value.Valid {
		return int(value.Int64)
	}
	return 0
}

func (s *Store) metaInt(ctx context.Context, key string) int {
	var value string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key = ?`, key).Scan(&value); err != nil {
		return 0
	}
	var parsed int
	_, _ = fmt.Sscanf(value, "%d", &parsed)
	return parsed
}

func (s *Store) ftsAvailable(ctx context.Context) bool {
	var value string
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM schema_meta WHERE key = 'fts_available'`).Scan(&value); err != nil {
		return false
	}
	return value == "true"
}

func (s *Store) touchCommits(ctx context.Context, commitNos []int) {
	if len(commitNos) == 0 {
		return
	}
	now := unixNow()
	for _, commitNo := range commitNos {
		_, _ = s.db.ExecContext(ctx, `UPDATE commits SET last_accessed_at = ? WHERE commit_no = ?`, now, commitNo)
	}
}

func (s *Store) touchMessages(ctx context.Context, ids []int64) {
	if len(ids) == 0 {
		return
	}
	now := unixNow()
	for _, id := range ids {
		_, _ = s.db.ExecContext(ctx, `UPDATE messages SET last_accessed_at = ? WHERE id = ?`, now, id)
	}
}

func parseSummaryResponse(text string) (string, string, []string) {
	text = strings.TrimSpace(text)
	var payload struct {
		Title   string   `json:"title"`
		Summary string   `json:"summary"`
		Tags    []string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(text), &payload); err == nil {
		return payload.Title, payload.Summary, payload.Tags
	}
	return firstLine(text), text, nil
}

func limitMessages(messages []MessageSnippet, limit int) []MessageSnippet {
	if limit <= 0 || len(messages) <= limit {
		return messages
	}
	if limit == 1 {
		return messages[:1]
	}
	head := limit / 2
	tail := limit - head
	out := append([]MessageSnippet(nil), messages[:head]...)
	out = append(out, messages[len(messages)-tail:]...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func safeSessionID(sessionID string) string {
	re := regexp.MustCompile(`[^A-Za-z0-9_.-]+`)
	clean := re.ReplaceAllString(sessionID, "_")
	clean = strings.Trim(clean, "._-")
	if clean == "" {
		return "session"
	}
	return clean
}

func sessionSQLitePath(sessionDir, sessionID string) string {
	return filepath.Join(sessionDir, safeSessionID(sessionID), "session.sqlite")
}

func migrateLegacySQLite(sessionDir, sessionID, targetPath string) error {
	legacyPath := filepath.Join(sessionDir, safeSessionID(sessionID)+".sqlite")
	if _, err := os.Stat(legacyPath); err != nil {
		return os.MkdirAll(filepath.Dir(targetPath), 0o755)
	}
	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(targetPath); err == nil {
		_ = os.Remove(legacyPath)
		return nil
	}
	return os.Rename(legacyPath, targetPath)
}

func messageHash(userTurn int, role string, content []byte) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d\n%s\n", userTurn, role)
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

func isTransientMessage(msg map[string]any) bool {
	if value, _ := msg["isMeta"].(bool); value {
		return true
	}
	metadata, _ := msg["metadata"].(map[string]any)
	if metadata["lumina_memory_context"] == true {
		return true
	}
	switch stringValue(metadata["source"]) {
	case "skill_inline", "skill_listing", "skill_recovery", "memory_index", "memory_recall", "task_notification":
		return true
	default:
		return false
	}
}

func isCompactionHandoffMessage(msg map[string]any) bool {
	if stringValue(msg["role"]) != "user" {
		return false
	}
	text := textPreview(msg, 200)
	return strings.Contains(text, "[Compaction handoff summary]") ||
		strings.Contains(text, "[Previous user requests]")
}

func sessionUserTurn(msg map[string]any) int {
	metadata, _ := msg["metadata"].(map[string]any)
	value := metadata["session_user_turn"]
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	default:
		var parsed int
		if _, err := fmt.Sscanf(strings.TrimSpace(fmt.Sprint(value)), "%d", &parsed); err == nil {
			return parsed
		}
		return 0
	}
}

func isRealUserRequest(msg map[string]any) bool {
	return stringValue(msg["role"]) == "user" && !hasToolResult(msg)
}

func hasToolResult(msg map[string]any) bool {
	for _, block := range contentBlocks(msg["content"]) {
		if stringValue(block["type"]) == "tool_result" {
			return true
		}
	}
	return false
}

func textPreview(msg map[string]any, limit int) string {
	var parts []string
	for _, block := range contentBlocks(msg["content"]) {
		switch stringValue(block["type"]) {
		case "text":
			parts = append(parts, stringValue(block["text"]))
		case "tool_use":
			parts = append(parts, "[tool_use: "+stringValue(block["name"])+" id="+stringValue(block["id"])+"]")
		case "tool_result":
			parts = append(parts, "[tool_result id="+stringValue(block["tool_use_id"])+"] "+stringValue(block["content"]))
		case "thinking", "redacted_thinking":
			continue
		}
	}
	return clamp(strings.TrimSpace(strings.Join(parts, "\n")), limit)
}

func contentBlocks(content any) []map[string]any {
	switch c := content.(type) {
	case []map[string]any:
		return c
	case []any:
		out := make([]map[string]any, 0, len(c))
		for _, raw := range c {
			if block, ok := raw.(map[string]any); ok {
				out = append(out, block)
			}
		}
		return out
	case string:
		return []map[string]any{{"type": "text", "text": c}}
	default:
		return nil
	}
}

func firstLine(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func stringValue(v any) string {
	if v == nil {
		return ""
	}
	switch value := v.(type) {
	case string:
		return value
	default:
		return fmt.Sprint(value)
	}
}

func clamp(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "..."
}

func unixNow() float64 {
	return float64(time.Now().UnixNano()) / 1e9
}
