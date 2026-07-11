package longmemory

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

func (s *Store) SearchCanonicalEntities(ctx context.Context, query string, scopes []Scope) ([]CanonicalEntity, error) {
	terms := normalizeEntityTerms([]string{query})
	if len(terms) == 0 || len(scopes) == 0 {
		return nil, nil
	}
	scopeSQL, scopeArgs := scopedClauses(scopes, "e.")
	var termClauses []string
	args := append([]any(nil), scopeArgs...)
	for _, term := range terms {
		termClauses = append(termClauses, "(LOWER(e.name) LIKE ? OR EXISTS (SELECT 1 FROM memory_entity_aliases a WHERE a.entity_id=e.entity_id AND a.normalized_alias LIKE ?))")
		pattern := "%" + normalizeEntityValue(term) + "%"
		args = append(args, pattern, pattern)
	}
	where := "(" + scopeSQL + ") AND (" + strings.Join(termClauses, " OR ") + ")"
	rows, err := s.db.QueryContext(ctx, `SELECT e.entity_id, e.scope_type, e.scope_key, e.name, e.entity_type,
		e.confidence, e.source_chunks_json, e.created_at, e.updated_at FROM memory_canonical_entities e
		WHERE `+where+` ORDER BY e.confidence DESC, e.updated_at DESC LIMIT 40`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CanonicalEntity
	for rows.Next() {
		var item CanonicalEntity
		var sources, created, updated string
		if err := rows.Scan(&item.EntityID, &item.ScopeType, &item.ScopeKey, &item.Name, &item.EntityType,
			&item.Confidence, &sources, &created, &updated); err != nil {
			return nil, err
		}
		item.SourceChunks = fromJSONList(sources)
		item.CreatedAt, item.UpdatedAt = parseTime(created), parseTime(updated)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) SearchCanonicalEvents(ctx context.Context, query string, scopes []Scope) ([]CanonicalEvent, error) {
	terms := normalizeEntityTerms([]string{query})
	if len(terms) == 0 || len(scopes) == 0 {
		return nil, nil
	}
	scopeSQL, scopeArgs := scopedClauses(scopes, "e.")
	var termClauses []string
	args := append([]any(nil), scopeArgs...)
	for _, term := range terms {
		termClauses = append(termClauses, "(LOWER(e.title) LIKE ? OR LOWER(e.summary) LIKE ?)")
		pattern := "%" + strings.ToLower(term) + "%"
		args = append(args, pattern, pattern)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT e.event_id, e.scope_type, e.scope_key, e.title, e.summary,
		e.occurred_at, e.valid_from, e.valid_until, e.confidence FROM memory_events e WHERE (`+scopeSQL+
		`) AND (`+strings.Join(termClauses, " OR ")+`) ORDER BY e.occurred_at, e.confidence DESC LIMIT 40`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []CanonicalEvent
	for rows.Next() {
		var event CanonicalEvent
		var occurred, validFrom, validUntil string
		if err := rows.Scan(&event.EventID, &event.ScopeType, &event.ScopeKey, &event.Title, &event.Summary,
			&occurred, &validFrom, &validUntil, &event.Confidence); err != nil {
			return nil, err
		}
		event.OccurredAt, event.ValidFrom, event.ValidUntil = parseTime(occurred), parseTime(validFrom), parseTime(validUntil)
		sources, sourceErr := s.eventSources(ctx, event.EventID)
		if sourceErr != nil {
			return nil, sourceErr
		}
		event.SourceChunks = sources
		result = append(result, event)
	}
	return result, rows.Err()
}

func (s *Store) CommitCanonicalMerge(ctx context.Context, merge CanonicalMerge) error {
	if merge.Entity == nil && merge.Event == nil {
		return fmt.Errorf("canonical merge has no entity or event")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	if merge.Entity != nil {
		entity := *merge.Entity
		entity.Name = strings.TrimSpace(entity.Name)
		if entity.ScopeType == "" || entity.ScopeKey == "" || entity.Name == "" {
			return fmt.Errorf("canonical entity requires scope and name")
		}
		if entity.EntityID == "" {
			entity.EntityID = StableID(entity.ScopeType, entity.ScopeKey, "canonical-entity", normalizeEntityValue(entity.Name)+"\x00"+entity.EntityType)
		}
		if entity.CreatedAt.IsZero() {
			entity.CreatedAt = now
		}
		entity.UpdatedAt = now
		entity.SourceChunks = normalizeStrings(append(entity.SourceChunks, merge.SourceChunks...))
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_canonical_entities(entity_id, scope_type, scope_key, name,
			entity_type, confidence, source_chunks_json, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(entity_id) DO UPDATE SET name=excluded.name, entity_type=excluded.entity_type,
			confidence=MAX(memory_canonical_entities.confidence, excluded.confidence),
			source_chunks_json=excluded.source_chunks_json, updated_at=excluded.updated_at`, entity.EntityID,
			entity.ScopeType, entity.ScopeKey, entity.Name, entity.EntityType, clamp01(entity.Confidence),
			toJSON(entity.SourceChunks), formatTime(entity.CreatedAt), formatTime(entity.UpdatedAt)); err != nil {
			return err
		}
		for _, alias := range normalizeStrings(append(merge.Aliases, entity.Name)) {
			if _, err := tx.ExecContext(ctx, `INSERT INTO memory_entity_aliases(entity_id, scope_type, scope_key, alias,
				normalized_alias, confidence, source_chunk_id) VALUES (?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(entity_id, normalized_alias) DO UPDATE SET confidence=MAX(memory_entity_aliases.confidence, excluded.confidence)`,
				entity.EntityID, entity.ScopeType, entity.ScopeKey, alias, normalizeEntityValue(alias), clamp01(entity.Confidence), firstString(merge.SourceChunks)); err != nil {
				return err
			}
		}
	}
	if merge.Event != nil {
		event := *merge.Event
		if event.ScopeType == "" || event.ScopeKey == "" || strings.TrimSpace(event.Title) == "" {
			return fmt.Errorf("canonical event requires scope and title")
		}
		if event.EventID == "" {
			event.EventID = StableID(event.ScopeType, event.ScopeKey, "canonical-event", event.Title+"\x00"+formatTime(event.OccurredAt))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_events(event_id, scope_type, scope_key, title, summary,
			occurred_at, valid_from, valid_until, confidence, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(event_id) DO UPDATE SET title=excluded.title, summary=excluded.summary,
			occurred_at=excluded.occurred_at, valid_from=excluded.valid_from, valid_until=excluded.valid_until,
			confidence=MAX(memory_events.confidence, excluded.confidence), updated_at=excluded.updated_at`, event.EventID,
			event.ScopeType, event.ScopeKey, event.Title, event.Summary, formatTime(event.OccurredAt),
			formatTime(event.ValidFrom), formatTime(event.ValidUntil), clamp01(event.Confidence), formatTime(now), formatTime(now)); err != nil {
			return err
		}
		for _, chunkID := range normalizeStrings(append(event.SourceChunks, merge.SourceChunks...)) {
			var scopeType ScopeType
			var scopeKey string
			if err := tx.QueryRowContext(ctx, `SELECT scope_type, scope_key FROM memory_evidence_chunks WHERE chunk_id=?`, chunkID).Scan(&scopeType, &scopeKey); err != nil {
				if err == sql.ErrNoRows {
					return fmt.Errorf("unknown event source chunk %s", chunkID)
				}
				return err
			}
			if scopeType != event.ScopeType || scopeKey != event.ScopeKey {
				return fmt.Errorf("event source chunk crosses scope")
			}
			if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_event_sources(event_id, chunk_id) VALUES (?, ?)`, event.EventID, chunkID); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bumpIndexGeneration(ctx)
	return nil
}

func (s *Store) ScheduleBackfill(ctx context.Context, jobType string) error {
	allowed := map[string]bool{"canonical_entity_backfill": true, "canonical_event_backfill": true,
		"session_chunk_index_backfill": true, "chunk_embedding_backfill": true}
	if !allowed[jobType] {
		return fmt.Errorf("unsupported memory backfill job %q", jobType)
	}
	return s.EnqueueJob(ctx, Job{JobID: StableID(ScopeProject, "memory-backfill", "job", jobType), Kind: jobType,
		ScopeType: ScopeProject, ScopeKey: "all", Payload: "{}", Status: "pending"})
}

func (s *Store) RunBackfillJob(ctx context.Context, job Job) error {
	switch job.Kind {
	case "canonical_entity_backfill":
		rows, err := s.db.QueryContext(ctx, `SELECT scope_type, scope_key, normalized_entity,
			MIN(original_text), MAX(confidence), json_group_array(memory_id) FROM memory_entities
			GROUP BY scope_type, scope_key, normalized_entity`)
		if err != nil {
			return err
		}
		var merges []CanonicalMerge
		for rows.Next() {
			var scopeType ScopeType
			var scopeKey, normalized, original, sourcesJSON string
			var confidence float64
			if err := rows.Scan(&scopeType, &scopeKey, &normalized, &original, &confidence, &sourcesJSON); err != nil {
				rows.Close()
				return err
			}
			_ = sourcesJSON
			merges = append(merges, CanonicalMerge{Entity: &CanonicalEntity{ScopeType: scopeType,
				ScopeKey: scopeKey, Name: original, Confidence: confidence}, Aliases: []string{normalized, original}})
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, merge := range merges {
			if err := s.CommitCanonicalMerge(ctx, merge); err != nil {
				return err
			}
		}
		return nil
	case "canonical_event_backfill":
		rows, err := s.db.QueryContext(ctx, `SELECT episode_id, scope_type, scope_key, content, occurred_at
			FROM memory_episodes ORDER BY occurred_at`)
		if err != nil {
			return err
		}
		var merges []CanonicalMerge
		for rows.Next() {
			var id string
			var scopeType ScopeType
			var scopeKey, content, occurred string
			if err := rows.Scan(&id, &scopeType, &scopeKey, &content, &occurred); err != nil {
				rows.Close()
				return err
			}
			event := CanonicalEvent{EventID: StableID(scopeType, scopeKey, "episode-event", id), ScopeType: scopeType,
				ScopeKey: scopeKey, Title: clipRunes(content, 160), Summary: clipRunes(content, 1200),
				OccurredAt: parseTime(occurred), Confidence: 1}
			merges = append(merges, CanonicalMerge{Event: &event})
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, merge := range merges {
			if err := s.CommitCanonicalMerge(ctx, merge); err != nil {
				return err
			}
		}
		return nil
	case "session_chunk_index_backfill":
		_, err := s.BackfillEvidenceChunks(ctx, 0)
		return err
	case "chunk_embedding_backfill":
		return nil // RunMaintenance consumes the missing chunk embedding index.
	default:
		return fmt.Errorf("unsupported memory backfill job %q", job.Kind)
	}
}

func (s *Store) eventSources(ctx context.Context, eventID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT chunk_id FROM memory_event_sources WHERE event_id=? ORDER BY chunk_id`, eventID)
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
	return result, rows.Err()
}
