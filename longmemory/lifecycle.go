package longmemory

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type ValueWeights struct {
	Importance         float64 `json:"importance"`
	Confidence         float64 `json:"confidence"`
	AccessRecency      float64 `json:"access_recency"`
	AccessFrequency    float64 `json:"access_frequency"`
	Reinforcement      float64 `json:"reinforcement"`
	ProvenanceStrength float64 `json:"provenance_strength"`
	DependencyStrength float64 `json:"dependency_strength"`
}

type LifecyclePolicy struct {
	Enabled               bool
	HotAccessDays         int
	WarmAccessDays        int
	AccessRecencyHalfLife int
	ArchiveGraceDays      int
	ArchiveValueThreshold float64
	RetentionDays         RetentionPolicy
	Weights               ValueWeights
}

type MemoryLifecycle struct {
	MemoryID           string      `json:"memory_id"`
	Status             Status      `json:"status"`
	Temperature        Temperature `json:"temperature"`
	RetentionExpiresAt time.Time   `json:"retention_expires_at"`
	AccessCount        int64       `json:"access_count"`
	LastAccessedAt     time.Time   `json:"last_accessed_at"`
	LastReinforcedAt   time.Time   `json:"last_reinforced_at"`
	ValueScore         float64     `json:"value_score"`
	Pinned             bool        `json:"pinned"`
	ArchivedAt         time.Time   `json:"archived_at"`
	ArchiveReason      string      `json:"archive_reason"`
}

type LifecycleDecision struct {
	MemoryID       string             `json:"memory_id"`
	CurrentState   MemoryLifecycle    `json:"current_state"`
	ProposedState  MemoryLifecycle    `json:"proposed_state"`
	ScoreBreakdown map[string]float64 `json:"score_breakdown"`
	Reasons        []string           `json:"reasons"`
	Action         string             `json:"action"`
}

type LifecycleEvent struct {
	EventID        string             `json:"event_id"`
	ResourceKind   string             `json:"resource_kind"`
	ResourceID     string             `json:"resource_id"`
	EventType      string             `json:"event_type"`
	OldStatus      Status             `json:"old_status"`
	NewStatus      Status             `json:"new_status"`
	OldTemperature Temperature        `json:"old_temperature"`
	NewTemperature Temperature        `json:"new_temperature"`
	Score          float64            `json:"score"`
	ScoreBreakdown map[string]float64 `json:"score_breakdown"`
	Reasons        []string           `json:"reasons"`
	CreatedAt      time.Time          `json:"created_at"`
}

func DefaultLifecyclePolicy() LifecyclePolicy {
	return LifecyclePolicy{Enabled: true, HotAccessDays: 30, WarmAccessDays: 90,
		AccessRecencyHalfLife: 30, ArchiveGraceDays: 30, ArchiveValueThreshold: 0.45,
		Weights: ValueWeights{Importance: 0.30, Confidence: 0.20, AccessRecency: 0.15,
			AccessFrequency: 0.10, Reinforcement: 0.10, ProvenanceStrength: 0.10, DependencyStrength: 0.05}}
}

func (s *Store) BackfillLifecycle(ctx context.Context, policy LifecyclePolicy, now time.Time) (int, error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	entries, err := s.List(ctx, SearchOptions{IncludeInactive: true, IncludeExpired: true, Limit: 1_000_000})
	if err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	updated := 0
	for _, entry := range entries {
		retention := entry.RetentionExpiresAt
		if retention.IsZero() {
			if days := policy.RetentionDays[entry.MemoryType]; days > 0 {
				retention = entry.CreatedAt.AddDate(0, 0, days)
			}
		}
		temperature := entry.Temperature
		if temperature == "" || entry.ValueScoreUpdatedAt.IsZero() {
			temperature = temperatureAt(entry.LastAccessedAt, entry.LastReinforcedAt, now, policy)
		}
		if entry.RetentionExpiresAt.Equal(retention) && entry.Temperature == temperature && !entry.ValueScoreUpdatedAt.IsZero() {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memories SET retention_expires_at=?, temperature=?, value_score_updated_at=? WHERE memory_id=?`,
			formatTime(retention), temperature, formatTime(now), entry.MemoryID); err != nil {
			return 0, err
		}
		updated++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	if updated > 0 {
		_ = appendLifecycleMigrationLog(filepath.Join(filepath.Dir(s.Path()), "migration-log.jsonl"), updated, now)
	}
	return updated, nil
}

func (s *Store) CalculateLifecycle(ctx context.Context, memoryID string, policy LifecyclePolicy, now time.Time) (LifecycleDecision, error) {
	entry, err := s.Get(ctx, memoryID)
	if err != nil {
		return LifecycleDecision{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	provenance, dependency, protected, err := s.lifecycleSignals(ctx, *entry, now)
	if err != nil {
		return LifecycleDecision{}, err
	}
	recency := exponentialRecency(entry.LastAccessedAt, now, policy.AccessRecencyHalfLife)
	frequency := math.Min(1, math.Log1p(float64(entry.AccessCount))/math.Log(101))
	reinforcement := exponentialRecency(entry.LastReinforcedAt, now, policy.AccessRecencyHalfLife)
	breakdown := map[string]float64{
		"importance": clamp01(entry.Importance), "confidence": clamp01(entry.Confidence),
		"access_recency": recency, "access_frequency": frequency, "reinforcement": reinforcement,
		"provenance_strength": provenance, "dependency_strength": dependency,
	}
	score := clamp01(breakdown["importance"]*policy.Weights.Importance + breakdown["confidence"]*policy.Weights.Confidence +
		breakdown["access_recency"]*policy.Weights.AccessRecency + breakdown["access_frequency"]*policy.Weights.AccessFrequency +
		breakdown["reinforcement"]*policy.Weights.Reinforcement + breakdown["provenance_strength"]*policy.Weights.ProvenanceStrength +
		breakdown["dependency_strength"]*policy.Weights.DependencyStrength)
	current := lifecycleFromEntry(*entry)
	proposed := current
	proposed.ValueScore = score
	proposed.Temperature = temperatureAt(entry.LastAccessedAt, entry.LastReinforcedAt, now, policy)
	action := "none"
	var reasons []string
	if proposed.Temperature != current.Temperature {
		action = "temperature"
		reasons = append(reasons, "access_temperature_changed")
	}
	if entry.Status == StatusActive && !entry.RetentionExpiresAt.IsZero() &&
		now.After(entry.RetentionExpiresAt.AddDate(0, 0, policy.ArchiveGraceDays)) && score < policy.ArchiveValueThreshold {
		if entry.Pinned {
			reasons = append(reasons, "pinned")
		} else if len(protected) > 0 {
			reasons = append(reasons, protected...)
		} else {
			action = "archive"
			proposed.Status = StatusArchived
			proposed.ArchivedAt = now
			proposed.ArchiveReason = "retention_expired_low_value"
			reasons = append(reasons, proposed.ArchiveReason)
		}
	}
	if action == "none" && math.Abs(current.ValueScore-score) > 0.001 {
		action = "score"
		reasons = append(reasons, "value_score_updated")
	}
	return LifecycleDecision{MemoryID: memoryID, CurrentState: current, ProposedState: proposed,
		ScoreBreakdown: breakdown, Reasons: normalizeStrings(reasons), Action: action}, nil
}

func (s *Store) PreviewMaintenance(ctx context.Context, policy LifecyclePolicy, now time.Time) ([]LifecycleDecision, error) {
	if !policy.Enabled {
		return nil, nil
	}
	entries, err := s.List(ctx, SearchOptions{IncludeInactive: true, IncludeExpired: true, Limit: 1_000_000})
	if err != nil {
		return nil, err
	}
	decisions := make([]LifecycleDecision, 0, len(entries))
	for _, entry := range entries {
		if entry.Status != StatusActive {
			continue
		}
		decision, err := s.CalculateLifecycle(ctx, entry.MemoryID, policy, now)
		if err != nil {
			return nil, err
		}
		if decision.Action != "none" || math.Abs(decision.CurrentState.ValueScore-decision.ProposedState.ValueScore) > 0.001 {
			decisions = append(decisions, decision)
		}
	}
	sort.SliceStable(decisions, func(i, j int) bool { return decisions[i].MemoryID < decisions[j].MemoryID })
	return decisions, nil
}

func (s *Store) ApplyMaintenance(ctx context.Context, decisions []LifecycleDecision) (int, error) {
	applied := 0
	for _, decision := range decisions {
		if decision.Action == "none" && math.Abs(decision.CurrentState.ValueScore-decision.ProposedState.ValueScore) <= 0.001 {
			continue
		}
		if err := s.applyLifecycleDecision(ctx, decision); err != nil {
			return applied, err
		}
		applied++
	}
	return applied, nil
}

func (s *Store) RecordAccess(ctx context.Context, ids []string) error {
	return s.updateMemoryActivity(ctx, ids, false)
}

func (s *Store) RecordReinforcement(ctx context.Context, ids []string) error {
	return s.updateMemoryActivity(ctx, ids, true)
}

func (s *Store) Pin(ctx context.Context, id string, pinned bool) error {
	entry, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	result, err := s.db.ExecContext(ctx, `UPDATE memories SET pinned=?, temperature=?, updated_at=? WHERE memory_id=?`,
		pinned, TemperatureHot, formatTime(now), id)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return sql.ErrNoRows
	}
	return s.recordLifecycleEvent(ctx, LifecycleEvent{ResourceKind: "memory", ResourceID: id,
		EventType: map[bool]string{true: "pin", false: "unpin"}[pinned], OldStatus: entry.Status, NewStatus: entry.Status,
		OldTemperature: entry.Temperature, NewTemperature: TemperatureHot, Score: entry.ValueScore, CreatedAt: now})
}

func (s *Store) RestoreLifecycle(ctx context.Context, id string) error {
	entry, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	if _, err := s.db.ExecContext(ctx, `UPDATE memories SET status=?, temperature=?, archived_at='', archive_reason='',
		last_accessed_at=?, access_count=access_count+1, updated_at=? WHERE memory_id=?`, StatusActive, TemperatureHot,
		formatTime(now), formatTime(now), id); err != nil {
		return err
	}
	if err := s.reindexMemory(ctx, id); err != nil {
		return err
	}
	return s.recordLifecycleEvent(ctx, LifecycleEvent{ResourceKind: "memory", ResourceID: id, EventType: "restore",
		OldStatus: entry.Status, NewStatus: StatusActive, OldTemperature: entry.Temperature,
		NewTemperature: TemperatureHot, Score: entry.ValueScore, CreatedAt: now})
}

func (s *Store) Archive(ctx context.Context, id, reason string) error {
	entry, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		reason = "manual_archive"
	}
	now := time.Now().UTC()
	decision := LifecycleDecision{MemoryID: id, CurrentState: lifecycleFromEntry(*entry),
		ProposedState: lifecycleFromEntry(*entry), Action: "archive", Reasons: []string{reason}}
	decision.ProposedState.Status = StatusArchived
	decision.ProposedState.Temperature = TemperatureCold
	decision.ProposedState.ArchivedAt = now
	decision.ProposedState.ArchiveReason = reason
	return s.applyLifecycleDecision(ctx, decision)
}

func (s *Store) ListLifecycleEvents(ctx context.Context, memoryID string, limit int) ([]LifecycleEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, resource_kind, resource_id, event_type, old_status,
		new_status, old_temperature, new_temperature, score, score_breakdown_json, reasons_json, created_at
		FROM memory_lifecycle_events WHERE resource_id=? ORDER BY created_at DESC LIMIT ?`, memoryID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []LifecycleEvent
	for rows.Next() {
		var event LifecycleEvent
		var breakdown, reasons, created string
		if err := rows.Scan(&event.EventID, &event.ResourceKind, &event.ResourceID, &event.EventType, &event.OldStatus,
			&event.NewStatus, &event.OldTemperature, &event.NewTemperature, &event.Score, &breakdown, &reasons, &created); err != nil {
			return nil, err
		}
		_ = jsonUnmarshal([]byte(breakdown), &event.ScoreBreakdown)
		event.Reasons = fromJSONList(reasons)
		event.CreatedAt = parseTime(created)
		events = append(events, event)
	}
	return events, rows.Err()
}

func (s *Store) updateMemoryActivity(ctx context.Context, ids []string, reinforced bool) error {
	if len(ids) == 0 {
		return nil
	}
	now := time.Now().UTC()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range normalizeStrings(ids) {
		targets := []string{id}
		var parentID string
		if err := tx.QueryRowContext(ctx, `SELECT parent_memory_id FROM memory_evidence_chunks WHERE chunk_id=?`, id).Scan(&parentID); err == nil && parentID != "" {
			targets = append(targets, parentID)
		} else if err != nil && err != sql.ErrNoRows {
			return err
		}
		for _, targetID := range normalizeStrings(targets) {
			entry, getErr := getEntryTx(ctx, tx, targetID)
			if getErr == sql.ErrNoRows {
				continue
			}
			if getErr != nil {
				return getErr
			}
			query := `UPDATE memories SET last_accessed_at=?, access_count=access_count+1, temperature=?, updated_at=? WHERE memory_id=?`
			args := []any{formatTime(now), TemperatureHot, formatTime(now), targetID}
			eventType := "temperature"
			if reinforced {
				query = `UPDATE memories SET last_accessed_at=?, last_reinforced_at=?, access_count=access_count+1, temperature=?, updated_at=? WHERE memory_id=?`
				args = []any{formatTime(now), formatTime(now), TemperatureHot, formatTime(now), targetID}
				eventType = "reinforcement"
			}
			if _, err := tx.ExecContext(ctx, query, args...); err != nil {
				return err
			}
			if reinforced || entry.Temperature != TemperatureHot {
				if err := insertLifecycleEventTx(ctx, tx, LifecycleEvent{ResourceKind: "memory", ResourceID: targetID,
					EventType: eventType, OldStatus: entry.Status, NewStatus: entry.Status,
					OldTemperature: entry.Temperature, NewTemperature: TemperatureHot, Score: entry.ValueScore,
					Reasons: []string{"memory_activity"}, CreatedAt: now}); err != nil {
					return err
				}
			}
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_evidence_chunks SET last_accessed_at=?, access_count=access_count+1,
			temperature=? WHERE chunk_id=?`, formatTime(now), TemperatureHot, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_session_index SET last_accessed_at=?, access_count=access_count+1,
			temperature=? WHERE index_id=?`, formatTime(now), TemperatureHot, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) applyLifecycleDecision(ctx context.Context, decision LifecycleDecision) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC()
	result, err := tx.ExecContext(ctx, `UPDATE memories SET status=?, temperature=?, value_score=?, value_score_updated_at=?,
		archived_at=?, archive_reason=?, updated_at=? WHERE memory_id=? AND status=? AND temperature=?`, decision.ProposedState.Status,
		decision.ProposedState.Temperature, decision.ProposedState.ValueScore, formatTime(now),
		formatTime(decision.ProposedState.ArchivedAt), decision.ProposedState.ArchiveReason, formatTime(now),
		decision.MemoryID, decision.CurrentState.Status, decision.CurrentState.Temperature)
	if err != nil {
		return err
	}
	if affected, _ := result.RowsAffected(); affected == 0 {
		return nil
	}
	if decision.ProposedState.Status == StatusArchived {
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id=?`, decision.MemoryID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM memory_chunk_fts WHERE chunk_id IN
			(SELECT chunk_id FROM memory_evidence_chunks WHERE parent_memory_id=?)`, decision.MemoryID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE memory_evidence_chunks SET archived_at=?, archive_reason=?, temperature=?
			WHERE parent_memory_id=?`, formatTime(now), decision.ProposedState.ArchiveReason, TemperatureCold, decision.MemoryID); err != nil {
			return err
		}
	}
	if err := insertLifecycleEventTx(ctx, tx, LifecycleEvent{ResourceKind: "memory", ResourceID: decision.MemoryID,
		EventType: decision.Action, OldStatus: decision.CurrentState.Status, NewStatus: decision.ProposedState.Status,
		OldTemperature: decision.CurrentState.Temperature, NewTemperature: decision.ProposedState.Temperature,
		Score: decision.ProposedState.ValueScore, ScoreBreakdown: decision.ScoreBreakdown, Reasons: decision.Reasons, CreatedAt: now}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.bumpIndexGeneration(ctx)
	return nil
}

func (s *Store) lifecycleSignals(ctx context.Context, entry Entry, now time.Time) (float64, float64, []string, error) {
	var spans, chunks, facts, edges, conflicts, events, coreBlocks int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_evidence_spans WHERE memory_id=?`, entry.MemoryID).Scan(&spans); err != nil {
		return 0, 0, nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_evidence_chunks WHERE parent_memory_id=?`, entry.MemoryID).Scan(&chunks); err != nil {
		return 0, 0, nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_facts WHERE memory_id=? AND status=?
		AND (valid_until='' OR valid_until>?)`, entry.MemoryID, StatusActive, formatTime(now)).Scan(&facts); err != nil {
		return 0, 0, nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_edges WHERE (from_id=? OR to_id=?) AND (valid_until='' OR valid_until>?)`,
		entry.MemoryID, entry.MemoryID, formatTime(now)).Scan(&edges); err != nil {
		return 0, 0, nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_edges WHERE (from_id=? OR to_id=?) AND edge_type=? AND (valid_until='' OR valid_until>?)`,
		entry.MemoryID, entry.MemoryID, EdgeContradicts, formatTime(now)).Scan(&conflicts); err != nil {
		return 0, 0, nil, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_event_sources es JOIN memory_evidence_chunks c
		ON c.chunk_id=es.chunk_id WHERE c.parent_memory_id=?`, entry.MemoryID).Scan(&events); err != nil {
		return 0, 0, nil, err
	}
	for _, messageID := range entry.SourceMessageIDs {
		var count int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_core_blocks WHERE scope_type=? AND scope_key=?
			AND source_message_ids_json LIKE ?`, entry.ScopeType, entry.ScopeKey, "%\""+messageID+"\"%").Scan(&count); err != nil {
			return 0, 0, nil, err
		}
		coreBlocks += count
	}
	provenance := math.Min(1, float64(spans+chunks)/5)
	dependency := math.Min(1, float64(facts+edges+events+coreBlocks)/5)
	var protected []string
	if facts > 0 {
		protected = append(protected, "active_fact_dependency")
	}
	if conflicts > 0 {
		protected = append(protected, "unresolved_conflict")
	}
	if events > 0 {
		protected = append(protected, "canonical_event_dependency")
	}
	if coreBlocks > 0 {
		protected = append(protected, "core_block_dependency")
	}
	return provenance, dependency, protected, nil
}

func lifecycleFromEntry(entry Entry) MemoryLifecycle {
	return MemoryLifecycle{MemoryID: entry.MemoryID, Status: entry.Status, Temperature: entry.Temperature,
		RetentionExpiresAt: entry.RetentionExpiresAt, AccessCount: entry.AccessCount, LastAccessedAt: entry.LastAccessedAt,
		LastReinforcedAt: entry.LastReinforcedAt, ValueScore: entry.ValueScore, Pinned: entry.Pinned,
		ArchivedAt: entry.ArchivedAt, ArchiveReason: entry.ArchiveReason}
}

func temperatureAt(accessed, reinforced, now time.Time, policy LifecyclePolicy) Temperature {
	if !reinforced.IsZero() && now.Sub(reinforced) <= 14*24*time.Hour {
		return TemperatureHot
	}
	if accessed.IsZero() {
		return TemperatureWarm
	}
	age := now.Sub(accessed)
	if age <= time.Duration(policy.HotAccessDays)*24*time.Hour {
		return TemperatureHot
	}
	if age <= time.Duration(policy.WarmAccessDays)*24*time.Hour {
		return TemperatureWarm
	}
	return TemperatureCold
}

func exponentialRecency(value, now time.Time, halfLifeDays int) float64 {
	if value.IsZero() || halfLifeDays <= 0 {
		return 0
	}
	ageDays := math.Max(0, now.Sub(value).Hours()/24)
	return math.Exp(-math.Ln2 * ageDays / float64(halfLifeDays))
}

func (s *Store) reindexMemory(ctx context.Context, id string) error {
	entry, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_fts WHERE memory_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_fts(memory_id, title, content, summary, tags, entities)
		VALUES (?, ?, ?, ?, ?, ?)`, id, entry.Title, entry.Content, entry.Summary, strings.Join(entry.Tags, " "), strings.Join(entry.Entities, " ")); err != nil {
		return err
	}
	if err := reindexMemoryChunksTx(ctx, tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

func reindexMemoryChunksTx(ctx context.Context, tx *sql.Tx, id string) error {
	rows, err := tx.QueryContext(ctx, `SELECT chunk_id, text FROM memory_evidence_chunks WHERE parent_memory_id=?`, id)
	if err != nil {
		return err
	}
	var chunks [][2]string
	for rows.Next() {
		var chunkID, text string
		if err := rows.Scan(&chunkID, &text); err != nil {
			rows.Close()
			return err
		}
		chunks = append(chunks, [2]string{chunkID, text})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, chunk := range chunks {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO memory_chunk_fts(chunk_id, text) VALUES (?, ?)`, chunk[0], chunk[1]); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_evidence_chunks SET archived_at='', archive_reason='', temperature=? WHERE parent_memory_id=?`, TemperatureHot, id); err != nil {
		return err
	}
	return nil
}

func (s *Store) recordLifecycleEvent(ctx context.Context, event LifecycleEvent) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertLifecycleEventTx(ctx, tx, event); err != nil {
		return err
	}
	return tx.Commit()
}

func insertLifecycleEventTx(ctx context.Context, tx *sql.Tx, event LifecycleEvent) error {
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	if event.EventID == "" {
		event.EventID = StableID(ScopeProject, "lifecycle", event.ResourceID, event.EventType+"\x00"+formatTime(event.CreatedAt))
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_lifecycle_events(event_id, resource_kind, resource_id,
		event_type, old_status, new_status, old_temperature, new_temperature, score, score_breakdown_json,
		reasons_json, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, event.EventID, event.ResourceKind,
		event.ResourceID, event.EventType, event.OldStatus, event.NewStatus, event.OldTemperature, event.NewTemperature,
		event.Score, marshalJSON(event.ScoreBreakdown), toJSON(event.Reasons), formatTime(event.CreatedAt))
	return err
}

func jsonUnmarshal(data []byte, target any) error {
	return json.Unmarshal(data, target)
}

func appendLifecycleMigrationLog(path string, updated int, now time.Time) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	data, _ := json.Marshal(map[string]any{"created_at": now, "migration": "memory_lifecycle", "updated": updated})
	_, err = file.Write(append(data, '\n'))
	return err
}
