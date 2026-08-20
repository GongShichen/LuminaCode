package session

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"LuminaCode/harness"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const postgresSessionSchemaVersion = 4

type PostgresRepository struct {
	pool      *pgxpool.Pool
	clusterID string
	now       func() time.Time
}

func NewPostgresRepository(ctx context.Context, postgresURL, clusterID string) (*PostgresRepository, error) {
	if strings.TrimSpace(postgresURL) == "" {
		return nil, errors.New("postgres URL is required")
	}
	if strings.TrimSpace(clusterID) == "" {
		clusterID = "default"
	}
	config, err := pgxpool.ParseConfig(postgresURL)
	if err != nil {
		return nil, errors.New("invalid postgres_url")
	}
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open postgres session repository: %w", err)
	}
	repository := &PostgresRepository{pool: pool, clusterID: clusterID, now: time.Now}
	if err := repository.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return repository, nil
}

func (r *PostgresRepository) Pool() *pgxpool.Pool { return r.pool }

func (r *PostgresRepository) migrate(ctx context.Context) error {
	connection, err := r.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock(hashtext('lumina_session_schema'))`); err != nil {
		return err
	}
	defer connection.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('lumina_session_schema'))`)
	statements := []string{
		`CREATE TABLE IF NOT EXISTS lumina_schema_migrations (
			component text PRIMARY KEY, version integer NOT NULL, applied_at timestamptz NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS lumina_sessions (
			cluster_id text NOT NULL, tenant_id text NOT NULL, session_id text NOT NULL,
			cwd text NOT NULL DEFAULT '',
			created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
			message_count integer NOT NULL DEFAULT 0, turn_count integer NOT NULL DEFAULT 0,
			pinned boolean NOT NULL DEFAULT false, status text NOT NULL DEFAULT 'idle',
			last_event_seq bigint NOT NULL DEFAULT 0, fence_token bigint NOT NULL DEFAULT 0,
			PRIMARY KEY(cluster_id, tenant_id, session_id)
		)`,
		`ALTER TABLE lumina_sessions ADD COLUMN IF NOT EXISTS cwd text NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS lumina_sessions_updated_idx
			ON lumina_sessions(cluster_id, tenant_id, updated_at DESC)`,
		`CREATE TABLE IF NOT EXISTS lumina_session_streams (
			cluster_id text NOT NULL, tenant_id text NOT NULL, session_id text NOT NULL,
			stream_id text NOT NULL, kind text NOT NULL, parent_stream_id text,
			head_seq bigint NOT NULL DEFAULT 0, status text NOT NULL,
			created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
			PRIMARY KEY(cluster_id, tenant_id, session_id, stream_id),
			FOREIGN KEY(cluster_id, tenant_id, session_id)
				REFERENCES lumina_sessions(cluster_id, tenant_id, session_id) ON DELETE CASCADE
		)`,
		`CREATE TABLE IF NOT EXISTS lumina_session_events (
			cluster_id text NOT NULL, tenant_id text NOT NULL, session_id text NOT NULL,
			seq bigint NOT NULL, event_id text NOT NULL, stream_id text NOT NULL,
			stream_seq bigint NOT NULL, type text NOT NULL, schema_version integer NOT NULL,
			occurred_at timestamptz NOT NULL, causation_id text, correlation_id text,
			audience jsonb NOT NULL, payload jsonb NOT NULL, payload_blob_id text,
			PRIMARY KEY(cluster_id, tenant_id, session_id, seq),
			UNIQUE(cluster_id, tenant_id, event_id),
			UNIQUE(cluster_id, tenant_id, session_id, stream_id, stream_seq),
			FOREIGN KEY(cluster_id, tenant_id, session_id, stream_id)
				REFERENCES lumina_session_streams(cluster_id, tenant_id, session_id, stream_id) ON DELETE CASCADE
		)`,
		`CREATE INDEX IF NOT EXISTS lumina_session_events_type_idx
			ON lumina_session_events(cluster_id, tenant_id, session_id, type, seq)`,
		`CREATE TABLE IF NOT EXISTS lumina_session_checkpoints (
			cluster_id text NOT NULL, tenant_id text NOT NULL, session_id text NOT NULL,
			stream_id text NOT NULL, projector text NOT NULL, projector_version integer NOT NULL,
			upto_seq bigint NOT NULL, state_json bytea NOT NULL, created_at timestamptz NOT NULL,
			PRIMARY KEY(cluster_id, tenant_id, session_id, stream_id, projector)
		)`,
		`CREATE TABLE IF NOT EXISTS lumina_session_command_results (
			cluster_id text NOT NULL, tenant_id text NOT NULL, session_id text NOT NULL,
			command_id text NOT NULL, accepted_seq bigint NOT NULL, result_json jsonb NOT NULL,
			created_at timestamptz NOT NULL,
			PRIMARY KEY(cluster_id, tenant_id, session_id, command_id)
		)`,
		`CREATE TABLE IF NOT EXISTS lumina_session_consumer_offsets (
			cluster_id text NOT NULL, tenant_id text NOT NULL, session_id text NOT NULL,
			consumer text NOT NULL, upto_seq bigint NOT NULL, updated_at timestamptz NOT NULL,
			PRIMARY KEY(cluster_id, tenant_id, session_id, consumer)
		)`,
		`CREATE TABLE IF NOT EXISTS lumina_session_blobs (
			cluster_id text NOT NULL, tenant_id text NOT NULL, digest text NOT NULL,
			mime_type text NOT NULL, size_bytes bigint NOT NULL, data bytea NOT NULL,
			created_at timestamptz NOT NULL,
			PRIMARY KEY(cluster_id, tenant_id, digest)
		)`,
		`CREATE TABLE IF NOT EXISTS lumina_session_outbox (
			id bigserial PRIMARY KEY, cluster_id text NOT NULL, tenant_id text NOT NULL,
			session_id text NOT NULL, seq bigint NOT NULL, topic text NOT NULL,
			payload jsonb NOT NULL, created_at timestamptz NOT NULL, published_at timestamptz,
			claimed_by text, claim_until timestamptz,
			UNIQUE(cluster_id, tenant_id, session_id, seq, topic)
		)`,
		`ALTER TABLE lumina_session_outbox ADD COLUMN IF NOT EXISTS claimed_by text`,
		`ALTER TABLE lumina_session_outbox ADD COLUMN IF NOT EXISTS claim_until timestamptz`,
		`CREATE INDEX IF NOT EXISTS lumina_session_outbox_pending_idx
			ON lumina_session_outbox(cluster_id, published_at, id) WHERE published_at IS NULL`,
		`CREATE TABLE IF NOT EXISTS lumina_session_imports (
			cluster_id text NOT NULL,tenant_id text NOT NULL,session_id text NOT NULL,migration_id text NOT NULL,
			status text NOT NULL,event_count bigint NOT NULL DEFAULT 0,checksum text,
			created_at timestamptz NOT NULL,completed_at timestamptz,
			PRIMARY KEY(cluster_id,tenant_id,session_id),UNIQUE(cluster_id,tenant_id,migration_id))`,
	}
	for _, statement := range statements {
		if _, err := connection.Exec(ctx, statement); err != nil {
			return fmt.Errorf("migrate postgres session repository: %w", err)
		}
	}
	if _, err = connection.Exec(ctx, `INSERT INTO lumina_schema_migrations(component, version, applied_at)
		VALUES('session', $1, $2)
		ON CONFLICT(component) DO UPDATE SET version=GREATEST(lumina_schema_migrations.version,excluded.version),
		applied_at=excluded.applied_at`,
		postgresSessionSchemaVersion, r.now().UTC()); err != nil {
		return err
	}
	var version int
	if err := connection.QueryRow(ctx, `SELECT version FROM lumina_schema_migrations WHERE component='session'`).Scan(&version); err != nil {
		return err
	}
	if version != postgresSessionSchemaVersion {
		return fmt.Errorf("unsupported PostgreSQL session schema version %d (binary supports %d)",
			version, postgresSessionSchemaVersion)
	}
	return nil
}

func (r *PostgresRepository) OpenRuntime(ctx context.Context, tenantID, sessionID string,
	options RuntimeOpenOptions) (*RuntimeLoad, error) {
	tenantID = normalizedTenantID(tenantID)
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("session id is required")
	}
	created := false
	if options.CreateIfMissing {
		command, err := r.pool.Exec(ctx, `INSERT INTO lumina_sessions(
			cluster_id, tenant_id, session_id, cwd, created_at, updated_at, fence_token)
			VALUES($1,$2,$3,$4,$5,$5,$6) ON CONFLICT DO NOTHING`,
			r.clusterID, tenantID, sessionID, options.CWD, r.now().UTC(), options.FenceToken)
		if err != nil {
			return nil, err
		}
		created = command.RowsAffected() == 1
	}
	if !options.ReadOnly && options.FenceToken <= 0 {
		return nil, errors.New("writable PostgreSQL runtime requires a fencing token")
	}
	if err := r.bindFence(ctx, tenantID, sessionID, options.FenceToken, options.ReadOnly); err != nil {
		return nil, err
	}
	if !options.ReadOnly && strings.TrimSpace(options.CWD) != "" {
		command, err := r.pool.Exec(ctx, `UPDATE lumina_sessions SET cwd=$4, updated_at=$5
			WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3 AND fence_token=$6`,
			r.clusterID, tenantID, sessionID, options.CWD, r.now().UTC(), options.FenceToken)
		if err != nil {
			return nil, err
		}
		if command.RowsAffected() != 1 {
			return nil, ErrSessionFenceLost
		}
	}
	store := &PostgresRuntimeStore{repository: r, tenantID: tenantID, sessionID: sessionID,
		fenceToken: options.FenceToken, readOnly: options.ReadOnly}
	if !options.ReadOnly {
		if err := store.CreateStream(ctx, harness.StreamDescriptor{ID: sessionID, Kind: string(harness.ScopeSession), Status: "idle"}); err != nil {
			return nil, err
		}
	}
	if created {
		if _, err := store.Append(ctx, 0, harness.PendingEvent{Type: harness.EventSessionCreated,
			Payload: map[string]any{"session_id": sessionID, "format_version": 3}}); err != nil {
			return nil, err
		}
	}
	if !options.ReadOnly {
		if err := RecoverInterruptedRuntime(ctx, store); err != nil {
			return nil, err
		}
	}
	state, recovery, tasks, err := LoadRuntimeState(ctx, store)
	if err != nil {
		return nil, err
	}
	return &RuntimeLoad{Journal: store, State: state, SkillRecovery: recovery, Tasks: tasks}, nil
}

func (r *PostgresRepository) Exists(ctx context.Context, tenantID, sessionID string) (bool, error) {
	var exists bool
	err := r.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM lumina_sessions
		WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3)`,
		r.clusterID, normalizedTenantID(tenantID), sessionID).Scan(&exists)
	return exists, err
}

func (r *PostgresRepository) RuntimeInfo(ctx context.Context, tenantID, sessionID string) (RuntimeInfo, error) {
	info := RuntimeInfo{TenantID: normalizedTenantID(tenantID), SessionID: sessionID}
	err := r.pool.QueryRow(ctx, `SELECT cwd,status,fence_token FROM lumina_sessions
		WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3`, r.clusterID, info.TenantID, sessionID).
		Scan(&info.CWD, &info.Status, &info.FenceToken)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeInfo{}, osErrNotExist(sessionID)
	}
	return info, err
}

func (r *PostgresRepository) ResolveParentSession(ctx context.Context, tenantID, streamID string) (string, error) {
	var sessionID string
	err := r.pool.QueryRow(ctx, `SELECT session_id FROM lumina_session_streams
		WHERE cluster_id=$1 AND tenant_id=$2 AND stream_id=$3 ORDER BY updated_at DESC LIMIT 1`,
		r.clusterID, normalizedTenantID(tenantID), streamID).Scan(&sessionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", osErrNotExist(streamID)
	}
	return sessionID, err
}

func (r *PostgresRepository) LoadEvents(ctx context.Context, tenantID, sessionID string, afterSeq int64,
	limit int) ([]harness.Event, int64, error) {
	store := &PostgresRuntimeStore{repository: r, tenantID: normalizedTenantID(tenantID),
		sessionID: sessionID, readOnly: true}
	events, err := store.Load(ctx, afterSeq, limit)
	if err != nil {
		return nil, 0, err
	}
	head, err := store.Head(ctx)
	return events, head, err
}

func (r *PostgresRepository) ListSessions(ctx context.Context, tenantID string) ([]Meta, error) {
	rows, err := r.pool.Query(ctx, `SELECT session_id, created_at, updated_at, message_count, turn_count, pinned
		FROM lumina_sessions WHERE cluster_id=$1 AND tenant_id=$2 ORDER BY updated_at DESC, session_id`,
		r.clusterID, normalizedTenantID(tenantID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Meta
	for rows.Next() {
		var item Meta
		var createdAt, updatedAt time.Time
		if err := rows.Scan(&item.SessionID, &createdAt, &updatedAt, &item.MessageCount, &item.TurnCount, &item.Pinned); err != nil {
			return nil, err
		}
		item.CreatedAt = float64(createdAt.UnixNano()) / float64(time.Second)
		item.LastUpdated = float64(updatedAt.UnixNano()) / float64(time.Second)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (r *PostgresRepository) Pin(ctx context.Context, tenantID, sessionID string, pinned bool) (*Meta, error) {
	var item Meta
	var createdAt, updatedAt time.Time
	err := r.pool.QueryRow(ctx, `UPDATE lumina_sessions SET pinned=$4, updated_at=$5
		WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3
		RETURNING session_id, created_at, updated_at, message_count, turn_count, pinned`,
		r.clusterID, normalizedTenantID(tenantID), sessionID, pinned, r.now().UTC()).Scan(
		&item.SessionID, &createdAt, &updatedAt, &item.MessageCount, &item.TurnCount, &item.Pinned)
	if err != nil {
		return nil, err
	}
	item.CreatedAt = float64(createdAt.UnixNano()) / float64(time.Second)
	item.LastUpdated = float64(updatedAt.UnixNano()) / float64(time.Second)
	return &item, nil
}

func (r *PostgresRepository) UpdateMetaProjection(ctx context.Context, tenantID, sessionID string,
	fenceToken int64, messageCount, turnCount int) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := r.checkFenceTx(ctx, tx, normalizedTenantID(tenantID), sessionID, fenceToken); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE lumina_sessions SET message_count=$4, turn_count=$5, updated_at=$6
		WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3`, r.clusterID, normalizedTenantID(tenantID),
		sessionID, messageCount, turnCount, r.now().UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) bindFence(ctx context.Context, tenantID, sessionID string, fenceToken int64,
	readOnly bool) error {
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var current int64
	if err := tx.QueryRow(ctx, `SELECT fence_token FROM lumina_sessions
		WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3 FOR UPDATE`,
		r.clusterID, tenantID, sessionID).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return osErrNotExist(sessionID)
		}
		return err
	}
	if readOnly {
		return tx.Commit(ctx)
	}
	if current > fenceToken {
		return fmt.Errorf("%w: current=%d requested=%d", ErrSessionFenceLost, current, fenceToken)
	}
	if current < fenceToken {
		if _, err := tx.Exec(ctx, `UPDATE lumina_sessions SET fence_token=$4, updated_at=$5
			WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3`, r.clusterID, tenantID, sessionID,
			fenceToken, r.now().UTC()); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (r *PostgresRepository) checkFenceTx(ctx context.Context, tx pgx.Tx, tenantID, sessionID string,
	fenceToken int64) error {
	if fenceToken <= 0 {
		return errors.New("PostgreSQL runtime write requires a fencing token")
	}
	var current int64
	if err := tx.QueryRow(ctx, `SELECT fence_token FROM lumina_sessions
		WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3 FOR UPDATE`,
		r.clusterID, tenantID, sessionID).Scan(&current); err != nil {
		return err
	}
	if current != fenceToken {
		return fmt.Errorf("%w: current=%d handle=%d", ErrSessionFenceLost, current, fenceToken)
	}
	return nil
}

func (r *PostgresRepository) Close() error {
	if r != nil && r.pool != nil {
		r.pool.Close()
	}
	return nil
}

func (r *PostgresRepository) Health(ctx context.Context) error {
	if err := r.pool.Ping(ctx); err != nil {
		return err
	}
	var version int
	if err := r.pool.QueryRow(ctx, `SELECT version FROM lumina_schema_migrations WHERE component='session'`).Scan(&version); err != nil {
		return err
	}
	if version != postgresSessionSchemaVersion {
		return fmt.Errorf("postgres session schema version=%d, want %d", version, postgresSessionSchemaVersion)
	}
	return nil
}

func (r *PostgresRepository) ClaimOutbox(ctx context.Context, instanceID string, limit int,
	lease time.Duration) ([]OutboxRecord, error) {
	if strings.TrimSpace(instanceID) == "" {
		return nil, errors.New("outbox claimant instance id is required")
	}
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	if lease <= 0 {
		lease = 30 * time.Second
	}
	rows, err := r.pool.Query(ctx, `WITH candidates AS (
		SELECT id FROM lumina_session_outbox
		WHERE cluster_id=$1 AND published_at IS NULL AND (claim_until IS NULL OR claim_until<$2)
		ORDER BY id FOR UPDATE SKIP LOCKED LIMIT $3
	) UPDATE lumina_session_outbox o SET claimed_by=$4, claim_until=$5
	FROM candidates c WHERE o.id=c.id
	RETURNING o.id,o.tenant_id,o.session_id,o.seq,o.payload`, r.clusterID, r.now().UTC(), limit,
		instanceID, r.now().UTC().Add(lease))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []OutboxRecord
	for rows.Next() {
		var record OutboxRecord
		var payload []byte
		if err := rows.Scan(&record.ID, &record.TenantID, &record.SessionID, &record.Seq, &payload); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(payload, &record.Event); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}

func (r *PostgresRepository) MarkOutboxPublished(ctx context.Context, instanceID string, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	command, err := r.pool.Exec(ctx, `UPDATE lumina_session_outbox SET published_at=$1,claimed_by=NULL,claim_until=NULL
		WHERE cluster_id=$2 AND claimed_by=$3 AND id=ANY($4)`, r.now().UTC(), r.clusterID, instanceID, ids)
	if err != nil {
		return err
	}
	if command.RowsAffected() != int64(len(ids)) {
		return fmt.Errorf("outbox publish ownership changed: marked=%d requested=%d", command.RowsAffected(), len(ids))
	}
	return nil
}

type PostgresRuntimeStore struct {
	repository   *PostgresRepository
	tenantID     string
	sessionID    string
	fenceToken   int64
	readOnly     bool
	projectionMu sync.Mutex
}

func (s *PostgresRuntimeStore) SessionID() string { return s.sessionID }

func (s *PostgresRuntimeStore) WithProjectionLock(ctx context.Context, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.projectionMu.Lock()
	defer s.projectionMu.Unlock()
	connection, err := s.repository.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer connection.Release()
	lockKey := strings.Join([]string{s.repository.clusterID, s.tenantID, s.sessionID, "projection"}, "\x1f")
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, lockKey); err != nil {
		return err
	}
	defer connection.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, lockKey)
	return fn()
}

func (s *PostgresRuntimeStore) CreateStream(ctx context.Context, stream harness.StreamDescriptor) error {
	if s.readOnly {
		return errors.New("runtime store is read-only")
	}
	if strings.TrimSpace(stream.ID) == "" || strings.TrimSpace(stream.Kind) == "" {
		return errors.New("stream id and kind are required")
	}
	if stream.Status == "" {
		stream.Status = "idle"
	}
	now := s.repository.now().UTC()
	tx, err := s.repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := s.repository.checkFenceTx(ctx, tx, s.tenantID, s.sessionID, s.fenceToken); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO lumina_session_streams(
		cluster_id, tenant_id, session_id, stream_id, kind, parent_stream_id, status, created_at, updated_at)
		VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$8) ON CONFLICT DO NOTHING`,
		s.repository.clusterID, s.tenantID, s.sessionID, stream.ID, stream.Kind, stream.ParentID, stream.Status, now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *PostgresRuntimeStore) Append(ctx context.Context, expectedStreamSeq int64,
	pending ...harness.PendingEvent) ([]harness.Event, error) {
	if s.readOnly {
		return nil, errors.New("runtime store is read-only")
	}
	if len(pending) == 0 {
		return nil, nil
	}
	streamID := pending[0].StreamID
	if streamID == "" {
		streamID = s.sessionID
	}
	for index := range pending {
		if pending[index].StreamID == "" {
			pending[index].StreamID = streamID
		}
		if pending[index].StreamID != streamID {
			return nil, errors.New("one append batch must target one stream")
		}
		if strings.TrimSpace(pending[index].Type) == "" {
			return nil, errors.New("event type is required")
		}
	}
	tx, err := s.repository.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var sessionSeq, currentFence int64
	if err := tx.QueryRow(ctx, `SELECT last_event_seq, fence_token FROM lumina_sessions
		WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3 FOR UPDATE`,
		s.repository.clusterID, s.tenantID, s.sessionID).Scan(&sessionSeq, &currentFence); err != nil {
		return nil, err
	}
	if s.fenceToken > 0 && currentFence != s.fenceToken {
		return nil, fmt.Errorf("%w: current=%d handle=%d", ErrSessionFenceLost, currentFence, s.fenceToken)
	}
	var streamSeq int64
	if err := tx.QueryRow(ctx, `SELECT head_seq FROM lumina_session_streams
		WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3 AND stream_id=$4 FOR UPDATE`,
		s.repository.clusterID, s.tenantID, s.sessionID, streamID).Scan(&streamSeq); err != nil {
		return nil, err
	}
	if expectedStreamSeq != harness.AnyStreamSeq && expectedStreamSeq != streamSeq {
		return nil, fmt.Errorf("%w: stream %s expected %d, current %d", ErrStreamConflict,
			streamID, expectedStreamSeq, streamSeq)
	}
	created := make([]harness.Event, 0, len(pending))
	for _, item := range pending {
		payload, err := json.Marshal(item.Payload)
		if err != nil {
			return nil, err
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
			occurredAt = s.repository.now()
		}
		sessionSeq++
		streamSeq++
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_session_events(
			cluster_id, tenant_id, session_id, seq, event_id, stream_id, stream_seq, type,
			schema_version, occurred_at, causation_id, correlation_id, audience, payload)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),NULLIF($12,''),$13,$14)`,
			s.repository.clusterID, s.tenantID, s.sessionID, sessionSeq, id, streamID, streamSeq,
			item.Type, maxInt(item.SchemaVersion, 1), occurredAt.UTC(), item.CausationID,
			item.CorrelationID, audienceJSON, payload); err != nil {
			return nil, err
		}
		event := harness.Event{Seq: sessionSeq, StreamSeq: streamSeq, ID: id, SessionID: s.sessionID,
			StreamID: streamID, Type: item.Type, SchemaVersion: maxInt(item.SchemaVersion, 1),
			OccurredAt: occurredAt.UTC(), CausationID: item.CausationID, CorrelationID: item.CorrelationID,
			Audience: audience, Payload: payload}
		outboxPayload, _ := json.Marshal(event)
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_session_outbox(
			cluster_id, tenant_id, session_id, seq, topic, payload, created_at)
			VALUES($1,$2,$3,$4,'runtime.event',$5,$6) ON CONFLICT DO NOTHING`,
			s.repository.clusterID, s.tenantID, s.sessionID, sessionSeq, outboxPayload,
			s.repository.now().UTC()); err != nil {
			return nil, err
		}
		created = append(created, event)
	}
	if _, err := tx.Exec(ctx, `UPDATE lumina_session_streams SET head_seq=$5, updated_at=$6
		WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3 AND stream_id=$4`,
		s.repository.clusterID, s.tenantID, s.sessionID, streamID, streamSeq, s.repository.now().UTC()); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE lumina_sessions SET last_event_seq=$4, updated_at=$5
		WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3`,
		s.repository.clusterID, s.tenantID, s.sessionID, sessionSeq, s.repository.now().UTC()); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return created, nil
}

func (s *PostgresRuntimeStore) Load(ctx context.Context, afterSeq int64, limit int) ([]harness.Event, error) {
	return s.load(ctx, `SELECT seq,event_id,stream_id,stream_seq,type,schema_version,occurred_at,
		COALESCE(causation_id,''),COALESCE(correlation_id,''),audience,payload
		FROM lumina_session_events WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3 AND seq>$4
		ORDER BY seq LIMIT $5`, s.repository.clusterID, s.tenantID, s.sessionID, afterSeq, normalizedLimit(limit))
}

func (s *PostgresRuntimeStore) LoadStream(ctx context.Context, streamID string, afterStreamSeq int64,
	limit int) ([]harness.Event, error) {
	return s.load(ctx, `SELECT seq,event_id,stream_id,stream_seq,type,schema_version,occurred_at,
		COALESCE(causation_id,''),COALESCE(correlation_id,''),audience,payload
		FROM lumina_session_events WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3
		AND stream_id=$4 AND stream_seq>$5 ORDER BY stream_seq LIMIT $6`, s.repository.clusterID,
		s.tenantID, s.sessionID, streamID, afterStreamSeq, normalizedLimit(limit))
}

func (s *PostgresRuntimeStore) load(ctx context.Context, query string, args ...any) ([]harness.Event, error) {
	rows, err := s.repository.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []harness.Event
	for rows.Next() {
		var event harness.Event
		var audienceJSON []byte
		if err := rows.Scan(&event.Seq, &event.ID, &event.StreamID, &event.StreamSeq, &event.Type,
			&event.SchemaVersion, &event.OccurredAt, &event.CausationID, &event.CorrelationID,
			&audienceJSON, &event.Payload); err != nil {
			return nil, err
		}
		event.SessionID = s.sessionID
		if err := json.Unmarshal(audienceJSON, &event.Audience); err != nil {
			return nil, err
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

func (s *PostgresRuntimeStore) Head(ctx context.Context) (int64, error) {
	var head int64
	err := s.repository.pool.QueryRow(ctx, `SELECT last_event_seq FROM lumina_sessions
		WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3`,
		s.repository.clusterID, s.tenantID, s.sessionID).Scan(&head)
	return head, err
}

func (s *PostgresRuntimeStore) StreamHead(ctx context.Context, streamID string) (int64, error) {
	var head int64
	err := s.repository.pool.QueryRow(ctx, `SELECT head_seq FROM lumina_session_streams
		WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3 AND stream_id=$4`,
		s.repository.clusterID, s.tenantID, s.sessionID, streamID).Scan(&head)
	return head, err
}

func (s *PostgresRuntimeStore) SaveCheckpoint(ctx context.Context, checkpoint harness.Checkpoint) error {
	if s.readOnly {
		return errors.New("runtime store is read-only")
	}
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = s.repository.now()
	}
	return s.withFenceTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO lumina_session_checkpoints(
		cluster_id,tenant_id,session_id,stream_id,projector,projector_version,upto_seq,state_json,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)
		ON CONFLICT(cluster_id,tenant_id,session_id,stream_id,projector) DO UPDATE SET
		projector_version=excluded.projector_version,upto_seq=excluded.upto_seq,
		state_json=excluded.state_json,created_at=excluded.created_at`, s.repository.clusterID,
			s.tenantID, s.sessionID, checkpoint.StreamID, checkpoint.Projector, checkpoint.ProjectorVersion,
			checkpoint.UpToSeq, checkpoint.State, checkpoint.CreatedAt.UTC())
		return err
	})
}

func (s *PostgresRuntimeStore) LoadCheckpoint(ctx context.Context, streamID, projector string) (*harness.Checkpoint, error) {
	var checkpoint harness.Checkpoint
	err := s.repository.pool.QueryRow(ctx, `SELECT stream_id,projector,projector_version,upto_seq,state_json,created_at
		FROM lumina_session_checkpoints WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3
		AND stream_id=$4 AND projector=$5`, s.repository.clusterID, s.tenantID, s.sessionID,
		streamID, projector).Scan(&checkpoint.StreamID, &checkpoint.Projector, &checkpoint.ProjectorVersion,
		&checkpoint.UpToSeq, &checkpoint.State, &checkpoint.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &checkpoint, err
}

func (s *PostgresRuntimeStore) SaveCommandResult(ctx context.Context, result harness.CommandResult) error {
	if s.readOnly {
		return errors.New("runtime store is read-only")
	}
	if result.CreatedAt.IsZero() {
		result.CreatedAt = s.repository.now()
	}
	return s.withFenceTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO lumina_session_command_results(
		cluster_id,tenant_id,session_id,command_id,accepted_seq,result_json,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7)
		ON CONFLICT(cluster_id,tenant_id,session_id,command_id) DO NOTHING`,
			s.repository.clusterID, s.tenantID, s.sessionID, result.CommandID, result.AcceptedSeq,
			result.Result, result.CreatedAt.UTC())
		return err
	})
}

func (s *PostgresRuntimeStore) GetCommandResult(ctx context.Context, commandID string) (*harness.CommandResult, error) {
	var result harness.CommandResult
	err := s.repository.pool.QueryRow(ctx, `SELECT command_id,session_id,accepted_seq,result_json,created_at
		FROM lumina_session_command_results WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3 AND command_id=$4`,
		s.repository.clusterID, s.tenantID, s.sessionID, commandID).Scan(&result.CommandID, &result.SessionID,
		&result.AcceptedSeq, &result.Result, &result.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &result, err
}

func (s *PostgresRuntimeStore) ConsumerOffset(ctx context.Context, consumer string) (int64, error) {
	var offset int64
	err := s.repository.pool.QueryRow(ctx, `SELECT upto_seq FROM lumina_session_consumer_offsets
		WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3 AND consumer=$4`,
		s.repository.clusterID, s.tenantID, s.sessionID, consumer).Scan(&offset)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return offset, err
}

func (s *PostgresRuntimeStore) SaveConsumerOffset(ctx context.Context, consumer string, offset int64) error {
	if s.readOnly {
		return errors.New("runtime store is read-only")
	}
	return s.withFenceTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO lumina_session_consumer_offsets(
		cluster_id,tenant_id,session_id,consumer,upto_seq,updated_at) VALUES($1,$2,$3,$4,$5,$6)
		ON CONFLICT(cluster_id,tenant_id,session_id,consumer) DO UPDATE SET
		upto_seq=excluded.upto_seq,updated_at=excluded.updated_at`, s.repository.clusterID,
			s.tenantID, s.sessionID, consumer, offset, s.repository.now().UTC())
		return err
	})
}

func (s *PostgresRuntimeStore) PutBlob(ctx context.Context, mimeType string, data []byte) (string, error) {
	if s.readOnly {
		return "", errors.New("runtime store is read-only")
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	err := s.withFenceTx(ctx, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `INSERT INTO lumina_session_blobs(
			cluster_id,tenant_id,digest,mime_type,size_bytes,data,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT DO NOTHING`, s.repository.clusterID, s.tenantID, digest, mimeType, len(data), data,
			s.repository.now().UTC())
		return err
	})
	return digest, err
}

func (s *PostgresRuntimeStore) GetBlob(ctx context.Context, digest string) ([]byte, string, error) {
	var data []byte
	var mimeType string
	err := s.repository.pool.QueryRow(ctx, `SELECT data,mime_type FROM lumina_session_blobs
		WHERE cluster_id=$1 AND tenant_id=$2 AND digest=$3`, s.repository.clusterID,
		s.tenantID, digest).Scan(&data, &mimeType)
	return data, mimeType, err
}

func (s *PostgresRuntimeStore) IntegrityCheck(ctx context.Context) error {
	var version int
	if err := s.repository.pool.QueryRow(ctx, `SELECT version FROM lumina_schema_migrations
		WHERE component='session'`).Scan(&version); err != nil {
		return err
	}
	if version != postgresSessionSchemaVersion {
		return fmt.Errorf("postgres session schema version=%d, want %d", version, postgresSessionSchemaVersion)
	}
	return s.repository.pool.Ping(ctx)
}

func (s *PostgresRuntimeStore) withFenceTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.repository.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := s.repository.checkFenceTx(ctx, tx, s.tenantID, s.sessionID, s.fenceToken); err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (*PostgresRuntimeStore) Close() error { return nil }

func normalizedTenantID(value string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return LocalTenantID
}

func osErrNotExist(sessionID string) error {
	return fmt.Errorf("session %s: %w", sessionID, ErrRuntimeNotFound)
}

var _ SessionRepository = (*PostgresRepository)(nil)
var _ RuntimeStore = (*PostgresRuntimeStore)(nil)
