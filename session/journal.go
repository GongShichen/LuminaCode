package session

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"LuminaCode/harness"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

const RuntimeJournalSchemaVersion = 1

var ErrStreamConflict = errors.New("event stream sequence conflict")

type RuntimeJournal struct {
	db           *sql.DB
	path         string
	sessionID    string
	now          func() time.Time
	mu           sync.Mutex
	projectionMu sync.Mutex
}

func RuntimeJournalPath(sessionDir, sessionID string) string {
	return filepath.Join(sessionDir, safeSessionID(sessionID), "runtime.sqlite")
}

func OpenRuntimeJournal(ctx context.Context, sessionDir, sessionID string) (*RuntimeJournal, error) {
	return openRuntimeJournal(ctx, RuntimeJournalPath(sessionDir, sessionID), sessionID, time.Now)
}

func openRuntimeJournal(ctx context.Context, path, sessionID string, now func() time.Time) (*RuntimeJournal, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, fmt.Errorf("session id is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	j := &RuntimeJournal{db: db, path: path, sessionID: sessionID, now: now}
	if err := j.init(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return j, nil
}

func (j *RuntimeJournal) Path() string { return j.path }

func (j *RuntimeJournal) SessionID() string { return j.sessionID }

func (j *RuntimeJournal) WithProjectionLock(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	j.projectionMu.Lock()
	defer j.projectionMu.Unlock()
	return fn()
}

func (j *RuntimeJournal) init(ctx context.Context) error {
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
		`PRAGMA foreign_keys=ON`,
		`PRAGMA busy_timeout=5000`,
		`CREATE TABLE IF NOT EXISTS schema_meta(key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS streams(
			stream_id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			parent_stream_id TEXT REFERENCES streams(stream_id),
			head_seq INTEGER NOT NULL DEFAULT 0,
			status TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS blobs(
			digest TEXT PRIMARY KEY,
			mime_type TEXT NOT NULL,
			size_bytes INTEGER NOT NULL,
			data BLOB NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS events(
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			event_id TEXT NOT NULL UNIQUE,
			stream_id TEXT NOT NULL REFERENCES streams(stream_id),
			stream_seq INTEGER NOT NULL,
			type TEXT NOT NULL,
			schema_version INTEGER NOT NULL,
			occurred_at TEXT NOT NULL,
			causation_id TEXT,
			correlation_id TEXT,
			audience_json TEXT NOT NULL,
			payload_json BLOB NOT NULL,
			payload_blob_id TEXT REFERENCES blobs(digest),
			UNIQUE(stream_id, stream_seq)
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoints(
			stream_id TEXT NOT NULL REFERENCES streams(stream_id),
			projector TEXT NOT NULL,
			projector_version INTEGER NOT NULL,
			upto_seq INTEGER NOT NULL,
			state_json BLOB NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY(stream_id, projector)
		)`,
		`CREATE TABLE IF NOT EXISTS consumer_offsets(
			consumer TEXT PRIMARY KEY,
			upto_seq INTEGER NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS command_results(
			command_id TEXT PRIMARY KEY,
			session_id TEXT NOT NULL,
			accepted_seq INTEGER NOT NULL,
			result_json BLOB NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS migration_runs(
			id TEXT PRIMARY KEY,
			source_version INTEGER NOT NULL,
			target_version INTEGER NOT NULL,
			status TEXT NOT NULL,
			started_at TEXT NOT NULL,
			completed_at TEXT,
			report_json BLOB
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_type_seq ON events(type, seq)`,
		`CREATE INDEX IF NOT EXISTS idx_events_correlation_seq ON events(correlation_id, seq)`,
		`CREATE INDEX IF NOT EXISTS idx_streams_parent_kind ON streams(parent_stream_id, kind)`,
	}
	for _, statement := range statements {
		if _, err := j.db.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	if _, err := j.db.ExecContext(ctx, `INSERT INTO schema_meta(key, value) VALUES('schema_version', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, RuntimeJournalSchemaVersion); err != nil {
		return err
	}
	return j.CreateStream(ctx, harness.StreamDescriptor{ID: j.sessionID, Kind: string(harness.ScopeSession), Status: "idle"})
}

func (j *RuntimeJournal) CreateStream(ctx context.Context, stream harness.StreamDescriptor) error {
	if strings.TrimSpace(stream.ID) == "" || strings.TrimSpace(stream.Kind) == "" {
		return fmt.Errorf("stream id and kind are required")
	}
	if stream.Status == "" {
		stream.Status = "idle"
	}
	now := j.now().UTC().Format(time.RFC3339Nano)
	_, err := j.db.ExecContext(ctx, `INSERT INTO streams(stream_id, kind, parent_stream_id, status, created_at, updated_at)
		VALUES(?, ?, NULLIF(?, ''), ?, ?, ?)
		ON CONFLICT(stream_id) DO NOTHING`, stream.ID, stream.Kind, stream.ParentID, stream.Status, now, now)
	return err
}

func (j *RuntimeJournal) Append(ctx context.Context, expectedStreamSeq int64, pending ...harness.PendingEvent) ([]harness.Event, error) {
	if len(pending) == 0 {
		return nil, nil
	}
	streamID := pending[0].StreamID
	if streamID == "" {
		streamID = j.sessionID
	}
	for i := range pending {
		if pending[i].StreamID == "" {
			pending[i].StreamID = streamID
		}
		if pending[i].StreamID != streamID {
			return nil, fmt.Errorf("one append batch must target one stream")
		}
		if strings.TrimSpace(pending[i].Type) == "" {
			return nil, fmt.Errorf("event type is required")
		}
	}

	j.mu.Lock()
	defer j.mu.Unlock()
	tx, err := j.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var head int64
	if err := tx.QueryRowContext(ctx, `SELECT head_seq FROM streams WHERE stream_id = ?`, streamID).Scan(&head); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("stream %q does not exist", streamID)
		}
		return nil, err
	}
	if expectedStreamSeq != harness.AnyStreamSeq && expectedStreamSeq != head {
		return nil, fmt.Errorf("%w: stream %s expected %d, current %d", ErrStreamConflict, streamID, expectedStreamSeq, head)
	}

	created := make([]harness.Event, 0, len(pending))
	for _, item := range pending {
		payload, err := json.Marshal(item.Payload)
		if err != nil {
			return nil, fmt.Errorf("marshal %s payload: %w", item.Type, err)
		}
		audience := item.Audience
		if len(audience) == 0 {
			audience = []harness.Audience{harness.AudienceInternal}
		}
		audienceJSON, err := json.Marshal(audience)
		if err != nil {
			return nil, err
		}
		id := item.ID
		if id == "" {
			id = uuid.NewString()
		}
		occurredAt := item.OccurredAt
		if occurredAt.IsZero() {
			occurredAt = j.now()
		}
		head++
		result, err := tx.ExecContext(ctx, `INSERT INTO events(
			event_id, stream_id, stream_seq, type, schema_version, occurred_at,
			causation_id, correlation_id, audience_json, payload_json
		) VALUES(?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?)`,
			id, streamID, head, item.Type, maxInt(item.SchemaVersion, 1), occurredAt.UTC().Format(time.RFC3339Nano),
			item.CausationID, item.CorrelationID, string(audienceJSON), payload)
		if err != nil {
			return nil, err
		}
		seq, err := result.LastInsertId()
		if err != nil {
			return nil, err
		}
		created = append(created, harness.Event{
			Seq: seq, StreamSeq: head, ID: id, SessionID: j.sessionID, StreamID: streamID,
			Type: item.Type, SchemaVersion: maxInt(item.SchemaVersion, 1), OccurredAt: occurredAt.UTC(),
			CausationID: item.CausationID, CorrelationID: item.CorrelationID,
			Audience: audience, Payload: payload,
		})
	}
	if _, err := tx.ExecContext(ctx, `UPDATE streams SET head_seq = ?, updated_at = ? WHERE stream_id = ?`,
		head, j.now().UTC().Format(time.RFC3339Nano), streamID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return created, nil
}

func (j *RuntimeJournal) Load(ctx context.Context, afterSeq int64, limit int) ([]harness.Event, error) {
	return j.load(ctx, `SELECT seq, event_id, stream_id, stream_seq, type, schema_version, occurred_at,
		COALESCE(causation_id, ''), COALESCE(correlation_id, ''), audience_json, payload_json
		FROM events WHERE seq > ? ORDER BY seq LIMIT ?`, afterSeq, normalizedLimit(limit))
}

func (j *RuntimeJournal) LoadStream(ctx context.Context, streamID string, afterStreamSeq int64, limit int) ([]harness.Event, error) {
	return j.load(ctx, `SELECT seq, event_id, stream_id, stream_seq, type, schema_version, occurred_at,
		COALESCE(causation_id, ''), COALESCE(correlation_id, ''), audience_json, payload_json
		FROM events WHERE stream_id = ? AND stream_seq > ? ORDER BY stream_seq LIMIT ?`, streamID, afterStreamSeq, normalizedLimit(limit))
}

// StreamHead returns the optimistic-concurrency position for a stream without
// replaying it. Append still checks the position transactionally.
func (j *RuntimeJournal) StreamHead(ctx context.Context, streamID string) (int64, error) {
	var head int64
	err := j.db.QueryRowContext(ctx, `SELECT head_seq FROM streams WHERE stream_id = ?`, streamID).Scan(&head)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("stream %q does not exist", streamID)
	}
	return head, err
}

func (j *RuntimeJournal) load(ctx context.Context, query string, args ...any) ([]harness.Event, error) {
	rows, err := j.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []harness.Event
	for rows.Next() {
		var event harness.Event
		var occurredAt, audienceJSON string
		if err := rows.Scan(&event.Seq, &event.ID, &event.StreamID, &event.StreamSeq, &event.Type,
			&event.SchemaVersion, &occurredAt, &event.CausationID, &event.CorrelationID, &audienceJSON, &event.Payload); err != nil {
			return nil, err
		}
		event.SessionID = j.sessionID
		if event.OccurredAt, err = time.Parse(time.RFC3339Nano, occurredAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(audienceJSON), &event.Audience); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}

func (j *RuntimeJournal) Head(ctx context.Context) (int64, error) {
	var head int64
	err := j.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM events`).Scan(&head)
	return head, err
}

func (j *RuntimeJournal) SaveCheckpoint(ctx context.Context, checkpoint harness.Checkpoint) error {
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = j.now()
	}
	_, err := j.db.ExecContext(ctx, `INSERT INTO checkpoints(stream_id, projector, projector_version, upto_seq, state_json, created_at)
		VALUES(?, ?, ?, ?, ?, ?)
		ON CONFLICT(stream_id, projector) DO UPDATE SET
			projector_version=excluded.projector_version, upto_seq=excluded.upto_seq,
			state_json=excluded.state_json, created_at=excluded.created_at`,
		checkpoint.StreamID, checkpoint.Projector, checkpoint.ProjectorVersion, checkpoint.UpToSeq,
		checkpoint.State, checkpoint.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (j *RuntimeJournal) LoadCheckpoint(ctx context.Context, streamID, projector string) (*harness.Checkpoint, error) {
	var checkpoint harness.Checkpoint
	var createdAt string
	err := j.db.QueryRowContext(ctx, `SELECT stream_id, projector, projector_version, upto_seq, state_json, created_at
		FROM checkpoints WHERE stream_id = ? AND projector = ?`, streamID, projector).Scan(
		&checkpoint.StreamID, &checkpoint.Projector, &checkpoint.ProjectorVersion, &checkpoint.UpToSeq, &checkpoint.State, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	checkpoint.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	return &checkpoint, err
}

func (j *RuntimeJournal) SaveCommandResult(ctx context.Context, result harness.CommandResult) error {
	if result.CreatedAt.IsZero() {
		result.CreatedAt = j.now()
	}
	_, err := j.db.ExecContext(ctx, `INSERT INTO command_results(command_id, session_id, accepted_seq, result_json, created_at)
		VALUES(?, ?, ?, ?, ?) ON CONFLICT(command_id) DO NOTHING`, result.CommandID, result.SessionID,
		result.AcceptedSeq, result.Result, result.CreatedAt.UTC().Format(time.RFC3339Nano))
	return err
}

func (j *RuntimeJournal) GetCommandResult(ctx context.Context, commandID string) (*harness.CommandResult, error) {
	var result harness.CommandResult
	var createdAt string
	err := j.db.QueryRowContext(ctx, `SELECT command_id, session_id, accepted_seq, result_json, created_at
		FROM command_results WHERE command_id = ?`, commandID).Scan(
		&result.CommandID, &result.SessionID, &result.AcceptedSeq, &result.Result, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	result.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	return &result, err
}

func (j *RuntimeJournal) ConsumerOffset(ctx context.Context, consumer string) (int64, error) {
	var offset int64
	err := j.db.QueryRowContext(ctx, `SELECT upto_seq FROM consumer_offsets WHERE consumer = ?`, consumer).Scan(&offset)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return offset, err
}

func (j *RuntimeJournal) SaveConsumerOffset(ctx context.Context, consumer string, offset int64) error {
	_, err := j.db.ExecContext(ctx, `INSERT INTO consumer_offsets(consumer, upto_seq, updated_at)
		VALUES(?, ?, ?) ON CONFLICT(consumer) DO UPDATE SET upto_seq=excluded.upto_seq, updated_at=excluded.updated_at`,
		consumer, offset, j.now().UTC().Format(time.RFC3339Nano))
	return err
}

func (j *RuntimeJournal) PutBlob(ctx context.Context, mimeType string, data []byte) (string, error) {
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	_, err := j.db.ExecContext(ctx, `INSERT INTO blobs(digest, mime_type, size_bytes, data, created_at)
		VALUES(?, ?, ?, ?, ?) ON CONFLICT(digest) DO NOTHING`, digest, mimeType, len(data), data, j.now().UTC().Format(time.RFC3339Nano))
	return digest, err
}

func (j *RuntimeJournal) GetBlob(ctx context.Context, digest string) ([]byte, string, error) {
	var data []byte
	var mimeType string
	err := j.db.QueryRowContext(ctx, `SELECT data, mime_type FROM blobs WHERE digest = ?`, digest).Scan(&data, &mimeType)
	return data, mimeType, err
}

func (j *RuntimeJournal) IntegrityCheck(ctx context.Context) error {
	var result string
	if err := j.db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&result); err != nil {
		return err
	}
	if result != "ok" {
		return fmt.Errorf("sqlite integrity check failed: %s", result)
	}
	return nil
}

func (j *RuntimeJournal) Close() error {
	if j == nil || j.db == nil {
		return nil
	}
	return j.db.Close()
}

func normalizedLimit(limit int) int {
	if limit <= 0 {
		return 500
	}
	if limit > 500 {
		return 500
	}
	return limit
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
