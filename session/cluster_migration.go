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
	"time"

	"github.com/jackc/pgx/v5"
)

const clusterMigrationMarker = "migrated-to-cluster.json"

type ClusterMigrationReport struct {
	MigrationID string `json:"migration_id"`
	TenantID    string `json:"tenant_id"`
	SessionID   string `json:"session_id"`
	EventCount  int64  `json:"event_count"`
	StreamCount int64  `json:"stream_count"`
	BlobCount   int64  `json:"blob_count"`
	Checksum    string `json:"checksum"`
	DryRun      bool   `json:"dry_run"`
	Status      string `json:"status"`
}

func LocalMigrationMarker(sessionDir, sessionID string) string {
	return filepath.Join(filepath.Dir(RuntimeJournalPath(sessionDir, sessionID)), clusterMigrationMarker)
}

func MarkLocalRuntimeMigrated(sessionDir string, report ClusterMigrationReport) error {
	marker := LocalMigrationMarker(sessionDir, report.SessionID)
	payload := map[string]any{"version": 1, "read_only": true, "tenant_id": report.TenantID,
		"migration_id": report.MigrationID, "event_count": report.EventCount, "checksum": report.Checksum,
		"completed_at": time.Now().UTC().Format(time.RFC3339Nano)}
	return atomicWriteJSON(marker, payload)
}

func LocalRuntimeMigrated(sessionDir, sessionID string) bool {
	_, err := os.Stat(LocalMigrationMarker(sessionDir, sessionID))
	return err == nil
}

func (r *PostgresRepository) ImportLocalRuntime(ctx context.Context, tenantID, sessionID, cwd,
	migrationID string, fenceToken int64, source *RuntimeJournal, dryRun bool) (ClusterMigrationReport, error) {
	report := ClusterMigrationReport{MigrationID: migrationID, TenantID: normalizedTenantID(tenantID),
		SessionID: sessionID, DryRun: dryRun, Status: "validated"}
	if source == nil || source.db == nil {
		return report, errors.New("local runtime journal is required")
	}
	if strings.TrimSpace(migrationID) == "" {
		migrationID = stableRuntimeMigrationID(source.path, report.TenantID, sessionID)
		report.MigrationID = migrationID
	}
	if fenceToken <= 0 && !dryRun {
		return report, errors.New("session import requires a Redis fencing token")
	}
	if err := source.IntegrityCheck(ctx); err != nil {
		return report, err
	}
	if err := source.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM streams`).Scan(&report.StreamCount); err != nil {
		return report, err
	}
	if err := source.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blobs`).Scan(&report.BlobCount); err != nil {
		return report, err
	}
	checksum, count, err := sqliteRuntimeChecksum(ctx, source)
	if err != nil {
		return report, err
	}
	report.EventCount, report.Checksum = count, checksum
	if dryRun {
		return report, nil
	}
	tx, err := r.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return report, err
	}
	defer tx.Rollback(ctx)
	var existingMigration, existingStatus, existingChecksum string
	var existingCount int64
	err = tx.QueryRow(ctx, `SELECT migration_id,status,event_count,COALESCE(checksum,'') FROM lumina_session_imports WHERE
		cluster_id=$1 AND tenant_id=$2 AND session_id=$3 FOR UPDATE`, r.clusterID, report.TenantID,
		sessionID).Scan(&existingMigration, &existingStatus, &existingCount, &existingChecksum)
	if err == nil {
		if existingMigration == migrationID && existingStatus == "completed" &&
			existingCount == report.EventCount && existingChecksum == report.Checksum {
			report.Status = "completed"
			return report, tx.Commit(ctx)
		}
		return report, fmt.Errorf("session %s already has migration %s in status %s",
			sessionID, existingMigration, existingStatus)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return report, err
	}
	var targetExists bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM lumina_sessions WHERE cluster_id=$1
		AND tenant_id=$2 AND session_id=$3)`, r.clusterID, report.TenantID, sessionID).Scan(&targetExists); err != nil {
		return report, err
	}
	if targetExists {
		return report, fmt.Errorf("target session %s already exists", sessionID)
	}
	started := r.now().UTC()
	if _, err := tx.Exec(ctx, `INSERT INTO lumina_session_imports(
		cluster_id,tenant_id,session_id,migration_id,status,event_count,checksum,created_at)
		VALUES($1,$2,$3,$4,'running',$5,$6,$7)`, r.clusterID, report.TenantID, sessionID,
		migrationID, report.EventCount, report.Checksum, started); err != nil {
		return report, err
	}
	createdAt, updatedAt := started, started
	_ = source.db.QueryRowContext(ctx, `SELECT COALESCE(MIN(occurred_at),''),COALESCE(MAX(occurred_at),'') FROM events`).
		Scan((*runtimeSQLiteTime)(&createdAt), (*runtimeSQLiteTime)(&updatedAt))
	state, _, _, _ := LoadRuntimeState(ctx, source)
	messageCount, turnCount := 0, 0
	if state != nil {
		messageCount, turnCount = len(state.Messages), state.TurnCount
	}
	var lastEventSeq int64
	if err := source.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq),0) FROM events`).Scan(&lastEventSeq); err != nil {
		return report, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO lumina_sessions(
		cluster_id,tenant_id,session_id,cwd,created_at,updated_at,message_count,turn_count,status,last_event_seq,fence_token)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'idle',$9,$10)`, r.clusterID, report.TenantID, sessionID,
		cwd, createdAt, updatedAt, messageCount, turnCount, lastEventSeq, fenceToken); err != nil {
		return report, err
	}
	if err := copySQLiteStreamsToPostgres(ctx, source, tx, r.clusterID, report.TenantID, sessionID); err != nil {
		return report, err
	}
	if err := copySQLiteBlobsToPostgres(ctx, source, tx, r.clusterID, report.TenantID); err != nil {
		return report, err
	}
	if err := copySQLiteEventsToPostgres(ctx, source, tx, r.clusterID, report.TenantID, sessionID); err != nil {
		return report, err
	}
	if err := copySQLiteRuntimeAuxToPostgres(ctx, source, tx, r.clusterID, report.TenantID, sessionID); err != nil {
		return report, err
	}
	var targetCount int64
	if err := tx.QueryRow(ctx, `SELECT COUNT(*) FROM lumina_session_events WHERE cluster_id=$1
		AND tenant_id=$2 AND session_id=$3`, r.clusterID, report.TenantID, sessionID).Scan(&targetCount); err != nil {
		return report, err
	}
	if targetCount != report.EventCount {
		return report, fmt.Errorf("session event count mismatch: source=%d target=%d", report.EventCount, targetCount)
	}
	targetChecksum, err := postgresRuntimeChecksumTx(ctx, tx, r.clusterID, report.TenantID, sessionID)
	if err != nil {
		return report, err
	}
	if targetChecksum != report.Checksum {
		return report, fmt.Errorf("session event checksum mismatch: source=%s target=%s",
			report.Checksum, targetChecksum)
	}
	if _, err := tx.Exec(ctx, `UPDATE lumina_session_imports SET status='completed',completed_at=$1
		WHERE cluster_id=$2 AND tenant_id=$3 AND session_id=$4`, r.now().UTC(), r.clusterID,
		report.TenantID, sessionID); err != nil {
		return report, err
	}
	if err := tx.Commit(ctx); err != nil {
		return report, err
	}
	report.Status = "completed"
	return report, nil
}

func postgresRuntimeChecksumTx(ctx context.Context, tx pgx.Tx, clusterID, tenantID,
	sessionID string) (string, error) {
	rows, err := tx.Query(ctx, `SELECT seq,event_id,stream_id,stream_seq,type,payload FROM lumina_session_events
		WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3 ORDER BY seq`, clusterID, tenantID, sessionID)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	hash := sha256.New()
	for rows.Next() {
		var seq, streamSeq int64
		var id, streamID, eventType string
		var payload []byte
		if err := rows.Scan(&seq, &id, &streamID, &streamSeq, &eventType, &payload); err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%d\x00%s\x00%s\x00%d\x00%s\x00", seq, id, streamID, streamSeq, eventType)
		hash.Write(canonicalRuntimeJSON(payload))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil)), rows.Err()
}

type runtimeSQLiteTime time.Time

func (t *runtimeSQLiteTime) Scan(value any) error {
	text, _ := value.(string)
	if text == "" {
		return nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, text)
	if err == nil {
		*t = runtimeSQLiteTime(parsed)
	}
	return err
}

func sqliteRuntimeChecksum(ctx context.Context, source *RuntimeJournal) (string, int64, error) {
	rows, err := source.db.QueryContext(ctx, `SELECT seq,event_id,stream_id,stream_seq,type,payload_json FROM events ORDER BY seq`)
	if err != nil {
		return "", 0, err
	}
	defer rows.Close()
	hash := sha256.New()
	var count int64
	for rows.Next() {
		var seq, streamSeq int64
		var id, streamID, eventType string
		var payload []byte
		if err := rows.Scan(&seq, &id, &streamID, &streamSeq, &eventType, &payload); err != nil {
			return "", count, err
		}
		fmt.Fprintf(hash, "%d\x00%s\x00%s\x00%d\x00%s\x00", seq, id, streamID, streamSeq, eventType)
		hash.Write(canonicalRuntimeJSON(payload))
		hash.Write([]byte{0})
		count++
	}
	return hex.EncodeToString(hash.Sum(nil)), count, rows.Err()
}

func canonicalRuntimeJSON(payload []byte) []byte {
	var value any
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if decoder.Decode(&value) != nil {
		return payload
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return payload
	}
	return canonical
}

func stableRuntimeMigrationID(path, tenantID, sessionID string) string {
	hash := sha256.Sum256([]byte(filepath.Clean(path) + "\x00" + tenantID + "\x00" + sessionID))
	return "session-" + hex.EncodeToString(hash[:16])
}

func copySQLiteStreamsToPostgres(ctx context.Context, source *RuntimeJournal, tx pgx.Tx,
	clusterID, tenantID, sessionID string) error {
	rows, err := source.db.QueryContext(ctx, `SELECT stream_id,kind,COALESCE(parent_stream_id,''),head_seq,status,created_at,updated_at FROM streams`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, kind, parent, status, created, updated string
		var head int64
		if err := rows.Scan(&id, &kind, &parent, &head, &status, &created, &updated); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_session_streams(
			cluster_id,tenant_id,session_id,stream_id,kind,parent_stream_id,head_seq,status,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10)`, clusterID, tenantID, sessionID,
			id, kind, parent, head, status, created, updated); err != nil {
			return err
		}
	}
	return rows.Err()
}

func copySQLiteBlobsToPostgres(ctx context.Context, source *RuntimeJournal, tx pgx.Tx,
	clusterID, tenantID string) error {
	rows, err := source.db.QueryContext(ctx, `SELECT digest,mime_type,size_bytes,data,created_at FROM blobs`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var digest, mimeType, created string
		var size int64
		var data []byte
		if err := rows.Scan(&digest, &mimeType, &size, &data, &created); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_session_blobs(
			cluster_id,tenant_id,digest,mime_type,size_bytes,data,created_at) VALUES($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT DO NOTHING`, clusterID, tenantID, digest, mimeType, size, data, created); err != nil {
			return err
		}
	}
	return rows.Err()
}

func copySQLiteEventsToPostgres(ctx context.Context, source *RuntimeJournal, tx pgx.Tx,
	clusterID, tenantID, sessionID string) error {
	rows, err := source.db.QueryContext(ctx, `SELECT seq,event_id,stream_id,stream_seq,type,schema_version,
		occurred_at,COALESCE(causation_id,''),COALESCE(correlation_id,''),audience_json,payload_json,
		COALESCE(payload_blob_id,'') FROM events ORDER BY seq`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var seq, streamSeq int64
		var schema int
		var id, streamID, eventType, occurred, causation, correlation, audience, blobID string
		var payload []byte
		if err := rows.Scan(&seq, &id, &streamID, &streamSeq, &eventType, &schema, &occurred,
			&causation, &correlation, &audience, &payload, &blobID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_session_events(
			cluster_id,tenant_id,session_id,seq,event_id,stream_id,stream_seq,type,schema_version,
			occurred_at,causation_id,correlation_id,audience,payload,payload_blob_id)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,NULLIF($11,''),NULLIF($12,''),$13,$14,NULLIF($15,''))`,
			clusterID, tenantID, sessionID, seq, id, streamID, streamSeq, eventType, schema, occurred,
			causation, correlation, audience, payload, blobID); err != nil {
			return err
		}
	}
	return rows.Err()
}

func copySQLiteRuntimeAuxToPostgres(ctx context.Context, source *RuntimeJournal, tx pgx.Tx,
	clusterID, tenantID, sessionID string) error {
	checkpointRows, err := source.db.QueryContext(ctx, `SELECT stream_id,projector,projector_version,upto_seq,state_json,created_at FROM checkpoints`)
	if err != nil {
		return err
	}
	for checkpointRows.Next() {
		var streamID, projector, created string
		var version int
		var upto int64
		var state []byte
		if err := checkpointRows.Scan(&streamID, &projector, &version, &upto, &state, &created); err != nil {
			checkpointRows.Close()
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_session_checkpoints VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			clusterID, tenantID, sessionID, streamID, projector, version, upto, state, created); err != nil {
			checkpointRows.Close()
			return err
		}
	}
	checkpointRows.Close()
	commandRows, err := source.db.QueryContext(ctx, `SELECT command_id,accepted_seq,result_json,created_at FROM command_results`)
	if err != nil {
		return err
	}
	for commandRows.Next() {
		var commandID, created string
		var accepted int64
		var result []byte
		if err := commandRows.Scan(&commandID, &accepted, &result, &created); err != nil {
			commandRows.Close()
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_session_command_results VALUES($1,$2,$3,$4,$5,$6,$7)`,
			clusterID, tenantID, sessionID, commandID, accepted, result, created); err != nil {
			commandRows.Close()
			return err
		}
	}
	commandRows.Close()
	offsetRows, err := source.db.QueryContext(ctx, `SELECT consumer,upto_seq,updated_at FROM consumer_offsets`)
	if err != nil {
		return err
	}
	defer offsetRows.Close()
	for offsetRows.Next() {
		var consumer, updated string
		var upto int64
		if err := offsetRows.Scan(&consumer, &upto, &updated); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_session_consumer_offsets VALUES($1,$2,$3,$4,$5,$6)`,
			clusterID, tenantID, sessionID, consumer, upto, updated); err != nil {
			return err
		}
	}
	return offsetRows.Err()
}

func (r *PostgresRepository) ExportRuntimeToLocal(ctx context.Context, tenantID, sessionID,
	targetSessionDir string) (ClusterMigrationReport, error) {
	tenantID = normalizedTenantID(tenantID)
	report := ClusterMigrationReport{TenantID: tenantID, SessionID: sessionID, Status: "running"}
	if exists, err := r.Exists(ctx, tenantID, sessionID); err != nil || !exists {
		if err == nil {
			err = ErrRuntimeNotFound
		}
		return report, err
	}
	targetPath := RuntimeJournalPath(targetSessionDir, sessionID)
	if _, err := os.Stat(targetPath); err == nil {
		return report, fmt.Errorf("local export target already exists: %s", targetPath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return report, err
	}
	journal, err := OpenRuntimeJournal(ctx, targetSessionDir, sessionID)
	if err != nil {
		return report, err
	}
	failed := true
	defer func() {
		_ = journal.Close()
		if failed {
			_ = os.Remove(targetPath)
		}
	}()
	tx, err := journal.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	for _, statement := range []string{"DELETE FROM events", "DELETE FROM checkpoints", "DELETE FROM streams",
		"DELETE FROM command_results", "DELETE FROM consumer_offsets", "DELETE FROM blobs"} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return report, err
		}
	}
	streamRows, err := r.pool.Query(ctx, `SELECT stream_id,kind,COALESCE(parent_stream_id,''),head_seq,status,
		created_at,updated_at FROM lumina_session_streams WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3
		ORDER BY (parent_stream_id IS NOT NULL),created_at,stream_id`,
		r.clusterID, tenantID, sessionID)
	if err != nil {
		return report, err
	}
	for streamRows.Next() {
		var id, kind, parent, status string
		var head int64
		var created, updated time.Time
		if err := streamRows.Scan(&id, &kind, &parent, &head, &status, &created, &updated); err != nil {
			streamRows.Close()
			return report, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO streams VALUES(?,?,?,?,?,?,?)`, id, kind,
			nullString(parent), head, status, created.UTC().Format(time.RFC3339Nano),
			updated.UTC().Format(time.RFC3339Nano)); err != nil {
			streamRows.Close()
			return report, err
		}
		report.StreamCount++
	}
	streamRows.Close()
	blobRows, err := r.pool.Query(ctx, `SELECT digest,mime_type,size_bytes,data,created_at FROM lumina_session_blobs
		WHERE cluster_id=$1 AND tenant_id=$2`, r.clusterID, tenantID)
	if err != nil {
		return report, err
	}
	for blobRows.Next() {
		var digest, mime string
		var size int64
		var data []byte
		var created time.Time
		if err := blobRows.Scan(&digest, &mime, &size, &data, &created); err != nil {
			blobRows.Close()
			return report, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO blobs VALUES(?,?,?,?,?)`, digest, mime, size,
			data, created.UTC().Format(time.RFC3339Nano)); err != nil {
			blobRows.Close()
			return report, err
		}
		report.BlobCount++
	}
	blobRows.Close()
	eventRows, err := r.pool.Query(ctx, `SELECT seq,event_id,stream_id,stream_seq,type,schema_version,
		occurred_at,COALESCE(causation_id,''),COALESCE(correlation_id,''),audience,payload,
		COALESCE(payload_blob_id,'') FROM lumina_session_events WHERE cluster_id=$1 AND tenant_id=$2
		AND session_id=$3 ORDER BY seq`, r.clusterID, tenantID, sessionID)
	if err != nil {
		return report, err
	}
	hash := sha256.New()
	var maxEventSeq int64
	for eventRows.Next() {
		var seq, streamSeq int64
		var schema int
		var id, streamID, eventType, causation, correlation, blobID string
		var occurred time.Time
		var audience, payload []byte
		if err := eventRows.Scan(&seq, &id, &streamID, &streamSeq, &eventType, &schema, &occurred,
			&causation, &correlation, &audience, &payload, &blobID); err != nil {
			eventRows.Close()
			return report, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO events(
			seq,event_id,stream_id,stream_seq,type,schema_version,occurred_at,causation_id,correlation_id,
			audience_json,payload_json,payload_blob_id) VALUES(?,?,?,?,?,?,?,NULLIF(?,''),NULLIF(?,''),?,?,NULLIF(?,''))`,
			seq, id, streamID, streamSeq, eventType, schema, occurred.UTC().Format(time.RFC3339Nano),
			causation, correlation, string(audience), payload, blobID); err != nil {
			eventRows.Close()
			return report, err
		}
		fmt.Fprintf(hash, "%d\x00%s\x00%s\x00%d\x00%s\x00", seq, id, streamID, streamSeq, eventType)
		hash.Write(canonicalRuntimeJSON(payload))
		hash.Write([]byte{0})
		report.EventCount++
		if seq > maxEventSeq {
			maxEventSeq = seq
		}
	}
	eventRows.Close()
	report.Checksum = hex.EncodeToString(hash.Sum(nil))
	if err := exportPostgresAuxToSQLite(ctx, r, tx, tenantID, sessionID); err != nil {
		return report, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sqlite_sequence SET seq=? WHERE name='events'`, maxEventSeq); err != nil {
		return report, err
	}
	if err := tx.Commit(); err != nil {
		return report, err
	}
	if err := journal.IntegrityCheck(ctx); err != nil {
		return report, err
	}
	store := NewStore(targetSessionDir)
	state, _, _, _ := LoadRuntimeState(ctx, journal)
	if state != nil {
		_ = store.UpdateMetaProjection(sessionID, len(state.Messages), state.TurnCount)
	}
	report.Status = "completed"
	failed = false
	return report, nil
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func exportPostgresAuxToSQLite(ctx context.Context, repository *PostgresRepository, tx *sql.Tx,
	tenantID, sessionID string) error {
	checkpointRows, err := repository.pool.Query(ctx, `SELECT stream_id,projector,projector_version,
		upto_seq,state_json,created_at FROM lumina_session_checkpoints WHERE cluster_id=$1 AND tenant_id=$2
		AND session_id=$3`, repository.clusterID, tenantID, sessionID)
	if err != nil {
		return err
	}
	for checkpointRows.Next() {
		var streamID, projector string
		var version int
		var upto int64
		var state []byte
		var created time.Time
		if err := checkpointRows.Scan(&streamID, &projector, &version, &upto, &state, &created); err != nil {
			checkpointRows.Close()
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO checkpoints VALUES(?,?,?,?,?,?)`, streamID,
			projector, version, upto, state, created.UTC().Format(time.RFC3339Nano)); err != nil {
			checkpointRows.Close()
			return err
		}
	}
	checkpointRows.Close()
	commandRows, err := repository.pool.Query(ctx, `SELECT command_id,accepted_seq,result_json,created_at
		FROM lumina_session_command_results WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3`,
		repository.clusterID, tenantID, sessionID)
	if err != nil {
		return err
	}
	for commandRows.Next() {
		var commandID string
		var accepted int64
		var result []byte
		var created time.Time
		if err := commandRows.Scan(&commandID, &accepted, &result, &created); err != nil {
			commandRows.Close()
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO command_results VALUES(?,?,?,?,?)`, commandID,
			sessionID, accepted, result, created.UTC().Format(time.RFC3339Nano)); err != nil {
			commandRows.Close()
			return err
		}
	}
	commandRows.Close()
	offsetRows, err := repository.pool.Query(ctx, `SELECT consumer,upto_seq,updated_at
		FROM lumina_session_consumer_offsets WHERE cluster_id=$1 AND tenant_id=$2 AND session_id=$3`,
		repository.clusterID, tenantID, sessionID)
	if err != nil {
		return err
	}
	defer offsetRows.Close()
	for offsetRows.Next() {
		var consumer string
		var upto int64
		var updated time.Time
		if err := offsetRows.Scan(&consumer, &upto, &updated); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO consumer_offsets VALUES(?,?,?)`, consumer,
			upto, updated.UTC().Format(time.RFC3339Nano)); err != nil {
			return err
		}
	}
	return offsetRows.Err()
}
