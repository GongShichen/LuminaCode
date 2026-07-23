package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

type IndexLagError struct {
	Cause error
}

func (e IndexLagError) Error() string {
	return "memory event is durable but its search index is pending: " + e.Cause.Error()
}

func (e IndexLagError) Unwrap() error { return e.Cause }

func (f *Fabric) AppendEvents(ctx context.Context, events []RawEvent, options IngestOptions) (IngestResult, error) {
	result := IngestResult{SemanticStatus: SemanticEventDurable}
	if f == nil || f.ledger == nil || f.index == nil {
		return result, errors.New("memory fabric is closed")
	}
	if len(events) == 0 {
		result.Durable = true
		return result, nil
	}
	if options.SemanticPolicy == "" {
		options.SemanticPolicy = SemanticDeferred
	}

	f.writeMu.Lock()
	defer f.writeMu.Unlock()
	now := f.now()
	tx, err := f.ledger.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()

	normalized := make([]RawEvent, 0, len(events))
	for _, event := range events {
		event.Space = normalizeSpace(event.Space)
		event.Content = strings.TrimSpace(event.Content)
		event.Actor = strings.ToLower(strings.TrimSpace(event.Actor))
		if event.Actor == "" {
			event.Actor = "unknown"
		}
		if event.Content == "" {
			continue
		}
		if event.OccurredAt.IsZero() {
			event.OccurredAt = now
		} else {
			event.OccurredAt = event.OccurredAt.UTC()
		}
		if event.ID == "" {
			event.ID = stableFabricID("evt", event.Space, event.ContextID, event.SessionID, event.Actor,
				event.SourceKind, event.SourceRef, formatFabricTime(event.OccurredAt), event.Content)
		}
		if event.ContextID != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO contexts(
				context_id, space, context_type, opened_at) VALUES (?, ?, 'segment', ?)
				ON CONFLICT(context_id) DO NOTHING`, event.ContextID, event.Space, formatFabricTime(event.OccurredAt)); err != nil {
				return result, fmt.Errorf("ensure memory context: %w", err)
			}
		}
		checksum := eventChecksum(event)
		insert, err := tx.ExecContext(ctx, `INSERT INTO events(
			event_id, space, context_id, session_id, actor, source_kind, content, occurred_at,
			source_ref, checksum, metadata_json, semantic_status, token_estimate, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING`, event.ID, event.Space, event.ContextID, event.SessionID, event.Actor,
			event.SourceKind, event.Content, formatFabricTime(event.OccurredAt), event.SourceRef, checksum,
			marshalJSON(event.Metadata), SemanticEventDurable, estimateTokens(event.Content), formatFabricTime(now))
		if err != nil {
			return result, fmt.Errorf("append memory event: %w", err)
		}
		rows, _ := insert.RowsAffected()
		if rows == 0 {
			var existingID string
			if err := tx.QueryRowContext(ctx, `SELECT event_id FROM events WHERE space=? AND checksum=?`,
				event.Space, checksum).Scan(&existingID); err == nil {
				event.ID = existingID
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO outbox(
			resource_kind, resource_id, operation, payload_json, status, created_at, updated_at)
			VALUES ('event', ?, 'upsert', '{}', 'pending', ?, ?)
			ON CONFLICT(resource_kind, resource_id, operation) DO UPDATE SET
				status=CASE WHEN outbox.status='done' THEN outbox.status ELSE 'pending' END,
				updated_at=excluded.updated_at`, event.ID, formatFabricTime(now), formatFabricTime(now)); err != nil {
			return result, fmt.Errorf("enqueue memory event projection: %w", err)
		}
		normalized = append(normalized, event)
		result.EventIDs = append(result.EventIDs, event.ID)
	}
	if err := tx.Commit(); err != nil {
		return result, fmt.Errorf("commit memory events: %w", err)
	}
	result.Durable = true
	result.EventIDs = uniqueStrings(result.EventIDs)
	if len(result.EventIDs) == 0 {
		return result, nil
	}

	indexedThrough, indexErr := f.projectResources(ctx, "event", result.EventIDs)
	result.IndexedThrough = indexedThrough
	if indexErr == nil && f.sidecar != nil && f.options.RetrievalEncoder != nil {
		// The sidecar is an optional derived index. Keep event encodings current
		// without delaying or invalidating the durable ledger/primary index.
		_ = f.syncRetrievalSidecarEvents(ctx, result.EventIDs)
	}
	if options.SemanticPolicy == SemanticDeterministic {
		drafts := deterministicDrafts(normalized)
		if len(drafts) > 0 {
			commit, commitErr := f.commitDrafts(ctx, MemoryRequest{Space: normalized[0].Space,
				ContextID: normalized[0].ContextID, SourceEventIDs: result.EventIDs, Drafts: drafts,
				Mode: WriteCriticalResult}, drafts)
			result.SemanticStatus = commit.SemanticStatus
			if commitErr != nil && indexErr == nil {
				indexErr = commitErr
			}
		}
	} else if options.SemanticPolicy == SemanticRequired {
		commit, commitErr := f.rememberDurableEvents(ctx, MemoryRequest{Space: normalized[0].Space,
			ContextID: normalized[0].ContextID, SourceEventIDs: result.EventIDs,
			Mode: WriteExplicit, RequireSemantic: true})
		result.SemanticStatus = commit.SemanticStatus
		result.PendingJobID = commit.PendingJobID
		if commitErr != nil && indexErr == nil {
			indexErr = commitErr
		}
	} else if options.SemanticPolicy != SemanticDurableOnly {
		job, enqueueErr := f.enqueueCompileIfReady(ctx, normalized)
		if enqueueErr != nil && indexErr == nil {
			indexErr = enqueueErr
		}
		result.PendingJobID = job.ID
	}
	if options.SealContext && len(normalized) > 0 && normalized[0].ContextID != "" {
		job, sealErr := f.SealContext(ctx, ContextRef{ID: normalized[0].ContextID, Space: normalized[0].Space})
		if job.ID != "" {
			result.PendingJobID = job.ID
		}
		if sealErr != nil && indexErr == nil {
			indexErr = sealErr
		}
	}
	f.wakeWorker()
	if indexErr != nil {
		return result, IndexLagError{Cause: indexErr}
	}
	return result, nil
}

func deterministicDrafts(events []RawEvent) []MemoryDraft {
	var drafts []MemoryDraft
	for _, event := range events {
		kind := strings.ToLower(strings.TrimSpace(event.SourceKind))
		if kind != "tool" && kind != "observation" && kind != "command" && kind != "file" {
			continue
		}
		facet := FacetState
		attribute := firstMetadataValue(event.Metadata, "path", "command", "tool")
		if kind == "command" || event.Metadata["command"] != "" {
			facet = FacetProcedureState
		}
		if attribute == "" {
			attribute = kind
		}
		drafts = append(drafts, MemoryDraft{
			Kind: NodeClaim, ClaimType: ClaimState, Statement: event.Content,
			Subject:     firstMetadataValue(event.Metadata, "path", "tool", "command"),
			SubjectType: kind, Facet: facet, AttributeKey: attribute,
			Scope: Scope{Project: event.Metadata["project"], Environment: event.Metadata["environment"], Actor: event.Actor},
			Value: ClaimValue{Kind: ValueText, Text: event.Content}, ValidFrom: event.OccurredAt,
			EvidenceMode: EvidenceObserved,
			Sources:      []SourceSpan{{EventID: event.ID, StartRune: 0, EndRune: len([]rune(event.Content)), Role: "support"}},
			Keys:         normalizeStringList([]string{attribute, event.Metadata["path"], event.Metadata["command"], event.Metadata["tool"]}, 4),
		})
	}
	return drafts
}

func firstMetadataValue(metadata map[string]string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(metadata[key]); value != "" {
			return value
		}
	}
	return ""
}

func (f *Fabric) upsertContext(ctx context.Context, ref ContextRef) error {
	if strings.TrimSpace(ref.ID) == "" {
		return errors.New("memory context id is required")
	}
	ref.Space = normalizeSpace(ref.Space)
	_, err := f.ledger.ExecContext(ctx, `INSERT INTO contexts(
		context_id, space, parent_id, context_type, label, opened_at, closed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(context_id) DO UPDATE SET
			parent_id=CASE WHEN excluded.parent_id='' THEN contexts.parent_id ELSE excluded.parent_id END,
			context_type=CASE WHEN excluded.context_type='' THEN contexts.context_type ELSE excluded.context_type END,
			label=CASE WHEN excluded.label='' THEN contexts.label ELSE excluded.label END,
			closed_at=CASE WHEN excluded.closed_at='' THEN contexts.closed_at ELSE excluded.closed_at END`,
		ref.ID, ref.Space, ref.ParentID, ref.Type, ref.Label, formatFabricTime(ref.OpenedAt), formatFabricTime(ref.ClosedAt))
	return err
}

func (f *Fabric) enqueueCompileIfReady(ctx context.Context, events []RawEvent) (JobRef, error) {
	if len(events) == 0 || events[0].ContextID == "" {
		return JobRef{}, nil
	}
	var tokens int
	err := f.ledger.QueryRowContext(ctx, `SELECT COALESCE(SUM(token_estimate), 0) FROM events
		WHERE space=? AND context_id=? AND semantic_status=? AND tombstoned=0`,
		events[0].Space, events[0].ContextID, SemanticEventDurable).Scan(&tokens)
	if err != nil || tokens < f.options.CompileBatchTokens {
		return JobRef{}, err
	}
	return f.enqueueCompileJob(ctx, events[0].Space, events[0].ContextID, WriteNormal)
}

func (f *Fabric) loadEvents(ctx context.Context, ids []string) ([]RawEvent, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for index, id := range ids {
		args[index] = id
	}
	rows, err := f.ledger.QueryContext(ctx, `SELECT event_id, space, context_id, session_id, actor, source_kind,
		content, occurred_at, source_ref, metadata_json FROM events WHERE event_id IN (`+placeholders+`) AND tombstoned=0`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []RawEvent
	for rows.Next() {
		var event RawEvent
		var occurredAt, metadataJSON string
		if err := rows.Scan(&event.ID, &event.Space, &event.ContextID, &event.SessionID, &event.Actor,
			&event.SourceKind, &event.Content, &occurredAt, &event.SourceRef, &metadataJSON); err != nil {
			return nil, err
		}
		event.OccurredAt = parseFabricTime(occurredAt)
		_ = json.Unmarshal([]byte(metadataJSON), &event.Metadata)
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(events, func(i, j int) bool {
		if !events[i].OccurredAt.Equal(events[j].OccurredAt) {
			return events[i].OccurredAt.Before(events[j].OccurredAt)
		}
		return events[i].ID < events[j].ID
	})
	return events, nil
}

func (f *Fabric) contextEventIDs(ctx context.Context, space, contextID string, onlyPending bool) ([]string, error) {
	query := `SELECT event_id FROM events WHERE space=? AND context_id=? AND tombstoned=0`
	args := []any{normalizeSpace(space), contextID}
	if onlyPending {
		// Proposed means the source was assigned to a compiler job but has not
		// reached a terminal semantic state. Include it when a context is sealed
		// again so a process restart can re-enqueue failed or interrupted batches.
		query += ` AND semantic_status IN (?, ?)`
		args = append(args, SemanticEventDurable, SemanticProposed)
	}
	query += ` ORDER BY occurred_at, event_id`
	rows, err := f.ledger.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func scanEvent(row interface{ Scan(...any) error }) (RawEvent, error) {
	var event RawEvent
	var occurredAt, metadataJSON string
	err := row.Scan(&event.ID, &event.Space, &event.ContextID, &event.SessionID, &event.Actor,
		&event.SourceKind, &event.Content, &occurredAt, &event.SourceRef, &metadataJSON)
	if err != nil {
		return event, err
	}
	event.OccurredAt = parseFabricTime(occurredAt)
	_ = json.Unmarshal([]byte(metadataJSON), &event.Metadata)
	return event, nil
}

func eventByIDTx(ctx context.Context, tx *sql.Tx, id string) (RawEvent, error) {
	return scanEvent(tx.QueryRowContext(ctx, `SELECT event_id, space, context_id, session_id, actor,
		source_kind, content, occurred_at, source_ref, metadata_json FROM events WHERE event_id=? AND tombstoned=0`, id))
}
