package memory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

func (f *Fabric) Forget(ctx context.Context, selector Selector, mode ForgetMode) error {
	if f == nil || f.ledger == nil || f.index == nil {
		return errors.New("memory fabric is closed")
	}
	selector.Space = normalizeSpace(selector.Space)
	selector.EventIDs = uniqueStrings(selector.EventIDs)
	selector.MemoryIDs = uniqueStrings(selector.MemoryIDs)
	selector.ContextIDs = uniqueStrings(selector.ContextIDs)
	if len(selector.EventIDs)+len(selector.MemoryIDs)+len(selector.ContextIDs) == 0 {
		return errors.New("forget selector is empty")
	}
	if mode == "" {
		mode = ForgetTombstone
	}
	if mode != ForgetTombstone && mode != ForgetPurge {
		return fmt.Errorf("unsupported forget mode %q", mode)
	}

	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	now := f.now()
	tx, err := f.ledger.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	eventIDs, nodeIDs, err := resolveForgetTargetsTx(ctx, tx, selector)
	if err != nil {
		return err
	}
	if mode == ForgetTombstone {
		if err := tombstoneTargetsTx(ctx, tx, selector.Space, eventIDs, nodeIDs, now); err != nil {
			return err
		}
	} else if err := purgeTargetsTx(ctx, tx, selector, eventIDs, nodeIDs, now); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if _, err := f.projectResources(ctx, "node", nodeIDs); err != nil {
		return IndexLagError{Cause: err}
	}
	if _, err := f.projectResources(ctx, "event", eventIDs); err != nil {
		return IndexLagError{Cause: err}
	}
	f.wakeWorker()
	return nil
}

func resolveForgetTargetsTx(ctx context.Context, tx *sql.Tx, selector Selector) ([]string, []string, error) {
	eventIDs, err := filterOwnedIDsTx(ctx, tx, "events", "event_id", selector.Space, selector.EventIDs)
	if err != nil {
		return nil, nil, err
	}
	if len(selector.ContextIDs) > 0 {
		args := []any{selector.Space}
		for _, id := range selector.ContextIDs {
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx, `SELECT event_id FROM events WHERE space=? AND context_id IN (`+
			placeholders(len(selector.ContextIDs))+`)`, args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, nil, err
			}
			eventIDs = append(eventIDs, id)
		}
		_ = rows.Close()
	}
	eventIDs = uniqueStrings(eventIDs)
	nodeIDs, err := filterOwnedIDsTx(ctx, tx, "memory_nodes", "node_id", selector.Space, selector.MemoryIDs)
	if err != nil {
		return nil, nil, err
	}
	if len(eventIDs) > 0 {
		args := []any{selector.Space}
		for _, id := range eventIDs {
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT s.node_id FROM node_sources s
			JOIN memory_nodes n ON n.node_id=s.node_id WHERE n.space=? AND s.event_id IN (`+
			placeholders(len(eventIDs))+`)`, args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, nil, err
			}
			nodeIDs = append(nodeIDs, id)
		}
		_ = rows.Close()
	}
	if len(selector.ContextIDs) > 0 {
		args := []any{selector.Space}
		for _, id := range selector.ContextIDs {
			args = append(args, id)
		}
		rows, err := tx.QueryContext(ctx, `SELECT node_id FROM memory_nodes WHERE space=? AND context_id IN (`+
			placeholders(len(selector.ContextIDs))+`)`, args...)
		if err != nil {
			return nil, nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return nil, nil, err
			}
			nodeIDs = append(nodeIDs, id)
		}
		_ = rows.Close()
	}
	return eventIDs, uniqueStrings(nodeIDs), nil
}

func filterOwnedIDsTx(ctx context.Context, tx *sql.Tx, table, idColumn, space string, ids []string) ([]string, error) {
	ids = uniqueStrings(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	allowed := map[string]bool{"events:event_id": true, "memory_nodes:node_id": true}
	if !allowed[table+":"+idColumn] {
		return nil, errors.New("unsupported memory ownership lookup")
	}
	args := []any{space}
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, `SELECT `+idColumn+` FROM `+table+` WHERE space=? AND `+idColumn+` IN (`+
		placeholders(len(ids))+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return uniqueStrings(result), rows.Err()
}

func tombstoneTargetsTx(ctx context.Context, tx *sql.Tx, space string, eventIDs, nodeIDs []string, now time.Time) error {
	stamp := formatFabricTime(now.UTC())
	if len(eventIDs) > 0 {
		args := []any{SemanticTombstoned, space}
		for _, id := range eventIDs {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE events SET tombstoned=1, semantic_status=?
			WHERE space=? AND event_id IN (`+placeholders(len(eventIDs))+`)`,
			args...); err != nil {
			return err
		}
	}
	if len(nodeIDs) > 0 {
		args := []any{SemanticTombstoned, stamp, space}
		for _, id := range nodeIDs {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_nodes SET tombstoned=1, status=?, updated_at=?
			WHERE space=? AND node_id IN (`+placeholders(len(nodeIDs))+`)`, args...); err != nil {
			return err
		}
		versionArgs := []any{SemanticTombstoned}
		for _, id := range nodeIDs {
			versionArgs = append(versionArgs, id)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE slot_versions SET status=? WHERE node_id IN (`+
			placeholders(len(nodeIDs))+`)`, versionArgs...); err != nil {
			return err
		}
	}
	return enqueueForgetOutboxTx(ctx, tx, eventIDs, nodeIDs, stamp)
}

func purgeTargetsTx(ctx context.Context, tx *sql.Tx, selector Selector, eventIDs, nodeIDs []string,
	now time.Time) error {
	stamp := formatFabricTime(now.UTC())
	if err := enqueueForgetOutboxTx(ctx, tx, eventIDs, nodeIDs, stamp); err != nil {
		return err
	}
	if len(nodeIDs) > 0 {
		args := make([]any, 0, len(nodeIDs))
		for _, id := range nodeIDs {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE resource_id IN (`+placeholders(len(nodeIDs))+`)`, args...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_nodes WHERE node_id IN (`+placeholders(len(nodeIDs))+`)`, args...); err != nil {
			return err
		}
	}
	if len(eventIDs) > 0 {
		args := make([]any, 0, len(eventIDs))
		for _, id := range eventIDs {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM identity_aliases WHERE source_event_id IN (`+
			placeholders(len(eventIDs))+`)`, args...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM events WHERE event_id IN (`+placeholders(len(eventIDs))+`)`, args...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE resource_id IN (`+placeholders(len(eventIDs))+`)`, args...); err != nil {
			return err
		}
	}
	if len(selector.ContextIDs) > 0 {
		args := make([]any, 0, len(selector.ContextIDs))
		for _, id := range selector.ContextIDs {
			args = append(args, id)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM contexts WHERE context_id IN (`+
			placeholders(len(selector.ContextIDs))+`)`, args...); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM jobs WHERE resource_id IN (`+
			placeholders(len(selector.ContextIDs))+`)`, args...); err != nil {
			return err
		}
	}
	for _, statement := range []string{
		`DELETE FROM conflict_sets WHERE (SELECT COUNT(*) FROM conflict_members m WHERE m.conflict_id=conflict_sets.conflict_id) < 2`,
		`DELETE FROM slots WHERE NOT EXISTS (SELECT 1 FROM memory_nodes n WHERE n.slot_id=slots.slot_id)`,
		`DELETE FROM identities WHERE NOT EXISTS (SELECT 1 FROM memory_nodes n WHERE n.subject_identity_id=identities.identity_id)
			AND NOT EXISTS (SELECT 1 FROM identity_aliases a WHERE a.identity_id=identities.identity_id)`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func enqueueForgetOutboxTx(ctx context.Context, tx *sql.Tx, eventIDs, nodeIDs []string, stamp string) error {
	for _, resource := range []struct {
		kind string
		ids  []string
	}{{"event", eventIDs}, {"node", nodeIDs}} {
		for _, id := range uniqueStrings(resource.ids) {
			if _, err := tx.ExecContext(ctx, `UPDATE outbox SET status='done', updated_at=?
				WHERE resource_kind=? AND resource_id=? AND operation='upsert'`, stamp, resource.kind, id); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO outbox(
				resource_kind, resource_id, operation, status, created_at, updated_at)
				VALUES (?, ?, 'delete', 'pending', ?, ?)
				ON CONFLICT(resource_kind, resource_id, operation) DO UPDATE SET status='pending', updated_at=excluded.updated_at`,
				resource.kind, id, stamp, stamp); err != nil {
				return err
			}
		}
	}
	return nil
}

func (f *Fabric) Doctor(ctx context.Context) (HealthReport, error) {
	report := HealthReport{LedgerPath: f.options.LedgerPath, IndexPath: f.options.IndexPath}
	if f == nil || f.ledger == nil || f.index == nil {
		return report, errors.New("memory fabric is closed")
	}
	if err := f.ledger.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&report.LedgerQuickCheck); err != nil {
		return report, err
	}
	if err := f.index.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&report.IndexQuickCheck); err != nil {
		return report, err
	}
	_ = f.ledger.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status IN ('pending','running')`).Scan(&report.PendingJobs)
	_ = f.ledger.QueryRowContext(ctx, `SELECT COUNT(*) FROM outbox WHERE status!='done'`).Scan(&report.PendingOutbox)
	var generation, indexed string
	_ = f.index.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key='index_generation'`).Scan(&generation)
	_ = f.index.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key='indexed_ledger_seq'`).Scan(&indexed)
	report.IndexGeneration, _ = strconv.ParseInt(generation, 10, 64)
	report.IndexedLedgerSeq, _ = strconv.ParseInt(indexed, 10, 64)
	var ledgerSchema, indexSchema string
	_ = f.ledger.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_revision'`).Scan(&ledgerSchema)
	_ = f.index.QueryRowContext(ctx, `SELECT value FROM index_meta WHERE key='schema_revision'`).Scan(&indexSchema)
	if ledgerSchema != fabricSchemaRevision || indexSchema != fabricSchemaRevision {
		report.Warnings = append(report.Warnings, "schema fingerprint mismatch")
	}
	var maxSeq int64
	_ = f.ledger.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM outbox`).Scan(&maxSeq)
	if maxSeq > report.IndexedLedgerSeq {
		report.Warnings = append(report.Warnings, fmt.Sprintf("index trails ledger by %d outbox records", maxSeq-report.IndexedLedgerSeq))
	}
	var failedJobs, criticalConflicts, orphanSources int
	_ = f.ledger.QueryRowContext(ctx, `SELECT COUNT(*) FROM jobs WHERE status='failed'`).Scan(&failedJobs)
	_ = f.ledger.QueryRowContext(ctx, `SELECT COUNT(*) FROM conflict_sets
		WHERE critical=1 AND status IN (?, ?)`, SemanticPendingResolution, SemanticUnresolved).Scan(&criticalConflicts)
	_ = f.ledger.QueryRowContext(ctx, `SELECT COUNT(*) FROM node_sources s
		LEFT JOIN events e ON e.event_id=s.event_id WHERE e.event_id IS NULL`).Scan(&orphanSources)
	if failedJobs > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%d memory jobs failed", failedJobs))
	}
	if criticalConflicts > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%d critical conflicts remain unresolved", criticalConflicts))
	}
	if orphanSources > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf("%d semantic sources are orphaned", orphanSources))
	}
	report.Healthy = strings.EqualFold(report.LedgerQuickCheck, "ok") &&
		strings.EqualFold(report.IndexQuickCheck, "ok") && ledgerSchema == fabricSchemaRevision &&
		indexSchema == fabricSchemaRevision && orphanSources == 0
	return report, nil
}
