package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"
)

type nodePersistResult struct {
	Node           MemoryNode
	MemoryIDs      []string
	ChangedNodeIDs []string
	ConflictID     string
	ResolutionID   string
	Critical       bool
}

func (f *Fabric) commitDrafts(ctx context.Context, request MemoryRequest, drafts []MemoryDraft) (MemoryCommitResult, error) {
	result := MemoryCommitResult{Durable: true, EventIDs: uniqueStrings(request.SourceEventIDs), SemanticStatus: SemanticActive}
	if len(drafts) == 0 {
		return result, nil
	}
	invalid := 0
	for _, original := range drafts {
		draft, sourceEvents, err := f.validateDraft(ctx, original)
		if err != nil {
			invalid++
			_ = f.persistQuarantine(ctx, request, original, err)
			continue
		}
		persisted, err := f.persistGroundedDraft(ctx, request, draft, sourceEvents)
		if err != nil {
			return result, err
		}
		result.MemoryIDs = append(result.MemoryIDs, persisted.MemoryIDs...)
		result.ConflictIDs = append(result.ConflictIDs, persisted.ConflictID)
		result.ResolutionIDs = append(result.ResolutionIDs, persisted.ResolutionID)
		if len(persisted.ChangedNodeIDs) > 0 {
			if _, err := f.projectResources(ctx, "node", persisted.ChangedNodeIDs); err != nil {
				return result, IndexLagError{Cause: err}
			}
		}
		if persisted.ConflictID == "" {
			continue
		}
		result.SemanticStatus = SemanticPendingResolution
		if request.Mode == WriteImport {
			// Historical imports batch unresolved conflicts after all compiler
			// jobs finish; never spend one remote call per semantic node.
			continue
		}
		if persisted.Critical && f.options.Adjudicator != nil && f.options.RemoteProcessing != RemoteProcessingOff {
			resolution, usage, resolveErr := f.resolveConflict(ctx, persisted.ConflictID)
			result.Usage = mergeAPIUsage(result.Usage, usage)
			if resolveErr != nil {
				if request.RequireSemantic {
					return result, resolveErr
				}
				continue
			}
			if resolution.ID != "" {
				result.ResolutionIDs = append(result.ResolutionIDs, resolution.ID)
			}
			if resolution.Decision != DecisionUncertain && resolution.Decision != DecisionNeedsCompile {
				result.SemanticStatus = SemanticActive
			}
		} else if f.options.Adjudicator != nil && f.options.RemoteProcessing != RemoteProcessingOff {
			job, enqueueErr := f.enqueueAdjudication(ctx, request.Space, persisted.ConflictID, f.now().Add(30*time.Second))
			if enqueueErr != nil {
				return result, enqueueErr
			}
			result.PendingJobID = job.ID
		}
	}
	result.MemoryIDs = uniqueStrings(result.MemoryIDs)
	result.ConflictIDs = uniqueStrings(result.ConflictIDs)
	result.ResolutionIDs = uniqueStrings(result.ResolutionIDs)
	if invalid == len(drafts) {
		result.SemanticStatus = SemanticQuarantined
		if request.RequireSemantic {
			return result, errors.New("all semantic memory candidates failed grounding")
		}
	}
	f.wakeWorker()
	return result, nil
}

func (f *Fabric) persistQuarantine(ctx context.Context, request MemoryRequest, draft MemoryDraft, cause error) error {
	now := f.now()
	id := stableFabricID("mem", request.Space, "quarantine", draft.Statement, marshalJSONArray(request.SourceEventIDs))
	payload := map[string]any{"draft": draft, "error": truncateMemoryError(cause.Error())}
	_, err := f.ledger.ExecContext(ctx, `INSERT INTO memory_nodes(
		node_id, space, context_id, node_kind, claim_type, statement, facet, attribute_key,
		status, payload_json, content_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(node_id) DO NOTHING`, id, normalizeSpace(request.Space), request.ContextID,
		draft.Kind, draft.ClaimType, firstNonEmptyMemory(draft.Statement, "quarantined semantic candidate"),
		draft.Facet, normalizeKey(draft.AttributeKey), SemanticQuarantined, marshalJSON(payload),
		contentHash(draft.Statement, cause.Error()), formatFabricTime(now), formatFabricTime(now))
	return err
}

func firstNonEmptyMemory(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func (f *Fabric) persistGroundedDraft(ctx context.Context, request MemoryRequest, draft MemoryDraft,
	sourceEvents map[string]RawEvent) (nodePersistResult, error) {
	var result nodePersistResult
	now := f.now()
	tx, err := f.ledger.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer func() { _ = tx.Rollback() }()

	space := normalizeSpace(request.Space)
	if space == "default" {
		for _, event := range sourceEvents {
			space = event.Space
			break
		}
	}
	var identity Identity
	if strings.TrimSpace(draft.Subject) != "" {
		identity, err = resolveOrCreateIdentityTx(ctx, tx, space, draft.Subject, draft.SubjectType, now)
		if err != nil {
			return result, err
		}
	}
	scope := scopeKey(draft.Scope)
	attribute := normalizeKey(draft.AttributeKey)
	slotID := ""
	if draft.Kind == NodeClaim {
		slotID = stableFabricID("slot", space, identity.ID, string(draft.Facet), attribute, scope)
		if _, err := tx.ExecContext(ctx, `INSERT INTO slots(
			slot_id, space, subject_identity_id, facet, attribute_key, scope_key, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, slotID, space, identity.ID,
			draft.Facet, attribute, scope, formatFabricTime(now)); err != nil {
			return result, err
		}
	}
	sourceIDs := make([]string, 0, len(draft.Sources))
	for _, source := range draft.Sources {
		sourceIDs = append(sourceIDs, source.EventID)
	}
	sourceIDs = uniqueStrings(sourceIDs)
	sort.Strings(sourceIDs)
	nodeID := stableFabricID("mem", space, string(draft.Kind), slotID, normalizeClaim(draft.Statement),
		claimValueKey(draft.Value), strings.Join(sourceIDs, "\x1f"))
	payload := draft.Payload
	if payload == nil {
		payload = map[string]any{}
	}
	node := MemoryNode{ID: nodeID, Space: space, ContextID: request.ContextID, Kind: draft.Kind,
		ClaimType: draft.ClaimType, Statement: draft.Statement, SubjectID: identity.ID, Subject: draft.Subject,
		Facet: draft.Facet, AttributeKey: attribute, ScopeKey: scope, SlotID: slotID, Value: draft.Value,
		ValidFrom: draft.ValidFrom, ValidUntil: draft.ValidUntil, EvidenceMode: draft.EvidenceMode,
		Status: SemanticGrounded, Sources: draft.Sources, Keys: draft.Keys, RetrievalCues: draft.RetrievalCues,
		Payload: payload, CreatedAt: now}
	existingSame, err := loadMemoryNodeTx(ctx, tx, nodeID)
	if err == nil {
		result.Node = existingSame
		result.MemoryIDs = []string{existingSame.ID}
		if err := tx.Commit(); err != nil {
			return result, err
		}
		return result, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO memory_nodes(
		node_id, space, context_id, node_kind, claim_type, statement, subject_identity_id, subject_text,
		facet, attribute_key, scope_key, slot_id, evidence_mode, valid_from, valid_until, status,
		payload_json, content_hash, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, node.ID, node.Space,
		node.ContextID, node.Kind, node.ClaimType, node.Statement, node.SubjectID, node.Subject, node.Facet,
		node.AttributeKey, node.ScopeKey, node.SlotID, node.EvidenceMode, formatFabricTime(node.ValidFrom),
		formatFabricTime(node.ValidUntil), node.Status, marshalJSON(node.Payload),
		contentHash(node.Statement, claimValueKey(node.Value), strings.Join(sourceIDs, "\x1f")),
		formatFabricTime(now), formatFabricTime(now)); err != nil {
		return result, err
	}
	if node.Kind == NodeClaim {
		if err := insertClaimValueTx(ctx, tx, node.ID, node.Value); err != nil {
			return result, err
		}
	}
	for _, source := range node.Sources {
		if _, err := tx.ExecContext(ctx, `INSERT INTO node_sources(
			node_id, event_id, start_rune, end_rune, source_role) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT DO NOTHING`, node.ID, source.EventID, source.StartRune, source.EndRune, source.Role); err != nil {
			return result, err
		}
	}
	if node.ContextID != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO node_contexts(node_id, context_id, relation)
			VALUES (?, ?, 'origin') ON CONFLICT DO NOTHING`, node.ID, node.ContextID); err != nil {
			return result, err
		}
	}
	for _, key := range node.Keys {
		if _, err := tx.ExecContext(ctx, `INSERT INTO node_keys(node_id, key_type, key_text)
			VALUES (?, 'anchor', ?) ON CONFLICT DO NOTHING`, node.ID, key); err != nil {
			return result, err
		}
	}
	for _, cue := range node.RetrievalCues {
		if _, err := tx.ExecContext(ctx, `INSERT INTO node_keys(node_id, key_type, key_text)
			VALUES (?, 'trigger', ?) ON CONFLICT DO NOTHING`, node.ID, cue); err != nil {
			return result, err
		}
	}
	if node.SlotID != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO slot_versions(
			slot_id, node_id, valid_from, valid_until, status, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			node.SlotID, node.ID, formatFabricTime(node.ValidFrom), formatFabricTime(node.ValidUntil),
			node.Status, formatFabricTime(now)); err != nil {
			return result, err
		}
	}

	var changed []string
	if node.SlotID == "" {
		node.Status = SemanticActive
	} else {
		existing, err := loadSlotNodesTx(ctx, tx, node.SlotID, node.ID)
		if err != nil {
			return result, err
		}
		decision, winner, conflicting := localSlotDecision(node, existing, sourceEvents, func(id string) map[string]RawEvent {
			return loadNodeEventsTx(ctx, tx, id)
		})
		switch decision {
		case DecisionDuplicate:
			node.Status = SemanticRejected
			if winner != "" {
				result.MemoryIDs = append(result.MemoryIDs, winner)
			}
		case DecisionSupersedes:
			node.Status = SemanticActive
			members := append([]MemoryNode{node}, conflicting...)
			conflict, resolution, err := createLocalResolutionTx(ctx, tx, members, node.ID, DecisionSupersedes, now)
			if err != nil {
				return result, err
			}
			result.ConflictID, result.ResolutionID = conflict.ID, resolution.ID
			for _, old := range conflicting {
				if _, err := tx.ExecContext(ctx, `UPDATE memory_nodes SET status=?, updated_at=? WHERE node_id=?`,
					SemanticSuperseded, formatFabricTime(now), old.ID); err != nil {
					return result, err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE slot_versions SET status=? WHERE node_id=?`, SemanticSuperseded, old.ID); err != nil {
					return result, err
				}
				changed = append(changed, old.ID)
			}
		case DecisionCoexists, DecisionScoped:
			node.Status = SemanticActive
		default:
			node.Status = SemanticPendingResolution
			members := append([]MemoryNode{node}, conflicting...)
			conflict, err := createConflictTx(ctx, tx, members, isCriticalDraft(request.Mode, node), now)
			if err != nil {
				return result, err
			}
			result.ConflictID, result.Critical = conflict.ID, isCriticalDraft(request.Mode, node)
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memory_nodes SET status=?, updated_at=? WHERE node_id=?`,
		node.Status, formatFabricTime(now), node.ID); err != nil {
		return result, err
	}
	if node.SlotID != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE slot_versions SET status=? WHERE node_id=?`, node.Status, node.ID); err != nil {
			return result, err
		}
	}
	changed = append(changed, node.ID)
	for _, changedID := range uniqueStrings(changed) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO outbox(
			resource_kind, resource_id, operation, status, created_at, updated_at)
			VALUES ('node', ?, 'upsert', 'pending', ?, ?)
			ON CONFLICT(resource_kind, resource_id, operation) DO UPDATE SET status='pending', updated_at=excluded.updated_at`,
			changedID, formatFabricTime(now), formatFabricTime(now)); err != nil {
			return result, err
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	result.Node = node
	result.MemoryIDs = append(result.MemoryIDs, node.ID)
	result.ChangedNodeIDs = uniqueStrings(changed)
	return result, nil
}

func insertClaimValueTx(ctx context.Context, tx *sql.Tx, nodeID string, value ClaimValue) error {
	listJSON := marshalJSONArray(value.List)
	var boolValue any
	if value.Bool != nil {
		if *value.Bool {
			boolValue = 1
		} else {
			boolValue = 0
		}
	}
	var number any
	if value.Kind == ValueNumber {
		number = value.Number
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO claim_values(
		node_id, value_kind, text_value, number_value, unit, time_value, list_json, bool_value)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, nodeID, value.Kind, value.Text, number, value.Unit,
		formatFabricTime(value.Time), listJSON, boolValue)
	return err
}

func resolveOrCreateIdentityTx(ctx context.Context, tx *sql.Tx, space, subject, identityType string, now time.Time) (Identity, error) {
	normalized := normalizeClaim(subject)
	var identity Identity
	err := tx.QueryRowContext(ctx, `SELECT i.identity_id, i.space, i.canonical, i.identity_type, i.display_name
		FROM identity_aliases a JOIN identities i ON i.identity_id=a.identity_id
		WHERE a.space=? AND a.normalized_alias=? AND a.status='active'
		ORDER BY CASE WHEN i.identity_type=? THEN 0 ELSE 1 END LIMIT 1`, space, normalized, identityType).
		Scan(&identity.ID, &identity.Space, &identity.Canonical, &identity.Type, &identity.DisplayName)
	if err == nil {
		return identity, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return identity, err
	}
	canonical := normalizeKey(subject)
	if canonical == "" {
		canonical = stableFabricID("subject", subject)
	}
	identity = Identity{ID: stableFabricID("idn", space, identityType, canonical), Space: space,
		Canonical: canonical, Type: normalizeKey(identityType), DisplayName: strings.TrimSpace(subject)}
	if _, err := tx.ExecContext(ctx, `INSERT INTO identities(
		identity_id, space, canonical, identity_type, display_name, created_at)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT(identity_id) DO NOTHING`, identity.ID, identity.Space,
		identity.Canonical, identity.Type, identity.DisplayName, formatFabricTime(now)); err != nil {
		return identity, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO identity_aliases(
		space, normalized_alias, identity_id, method, created_at) VALUES (?, ?, ?, 'exact_subject', ?)
		ON CONFLICT DO NOTHING`, space, normalized, identity.ID, formatFabricTime(now)); err != nil {
		return identity, err
	}
	return identity, nil
}

func localSlotDecision(candidate MemoryNode, existing []MemoryNode, candidateEvents map[string]RawEvent,
	loadEvents func(string) map[string]RawEvent) (ConflictDecision, string, []MemoryNode) {
	if len(existing) == 0 {
		return DecisionCoexists, "", nil
	}
	var overlapping []MemoryNode
	for _, old := range existing {
		if claimValueKey(candidate.Value) == claimValueKey(old.Value) {
			return DecisionDuplicate, old.ID, []MemoryNode{old}
		}
		if intervalsOverlap(candidate.ValidFrom, candidate.ValidUntil, old.ValidFrom, old.ValidUntil) {
			overlapping = append(overlapping, old)
		}
	}
	if len(overlapping) == 0 {
		return DecisionScoped, "", nil
	}
	if canLocallySupersede(candidate, overlapping, candidateEvents, loadEvents) {
		return DecisionSupersedes, candidate.ID, overlapping
	}
	return DecisionUncertain, "", overlapping
}

func canLocallySupersede(candidate MemoryNode, existing []MemoryNode, candidateEvents map[string]RawEvent,
	loadEvents func(string) map[string]RawEvent) bool {
	switch candidate.Facet {
	case FacetPreference, FacetConstraint:
		if candidate.EvidenceMode != EvidenceUserDeclared {
			return false
		}
	case FacetState, FacetConfiguration, FacetProcedureState:
		if candidate.EvidenceMode != EvidenceObserved {
			return false
		}
	default:
		return false
	}
	candidateAuthority := sourceAuthority(candidate, candidateEvents)
	for _, old := range existing {
		if candidateAuthority < sourceAuthority(old, loadEvents(old.ID)) {
			return false
		}
		oldTime := old.ValidFrom
		if oldTime.IsZero() {
			oldTime = old.CreatedAt
		}
		newTime := candidate.ValidFrom
		if newTime.IsZero() {
			newTime = candidate.CreatedAt
		}
		if !newTime.After(oldTime) {
			return false
		}
	}
	return true
}

func isCriticalDraft(mode MemoryWriteMode, node MemoryNode) bool {
	if mode == WriteImport {
		return false
	}
	if mode == WriteCorrection || mode == WritePreference || mode == WriteConstraint || mode == WriteCriticalResult {
		return true
	}
	switch node.Facet {
	case FacetState, FacetPreference, FacetConstraint, FacetConfiguration, FacetProcedureState:
		return true
	default:
		return false
	}
}

const nodeSelectColumns = `n.node_id, n.space, n.context_id, n.node_kind, n.claim_type, n.statement,
	n.subject_identity_id, n.subject_text, n.facet, n.attribute_key, n.scope_key, n.slot_id,
	n.evidence_mode, n.valid_from, n.valid_until, n.status, n.payload_json, n.created_at,
	COALESCE(v.value_kind,''), COALESCE(v.text_value,''), v.number_value, COALESCE(v.unit,''),
	COALESCE(v.time_value,''), COALESCE(v.list_json,'[]'), v.bool_value`

func loadMemoryNodeTx(ctx context.Context, tx *sql.Tx, id string) (MemoryNode, error) {
	return scanMemoryNode(tx.QueryRowContext(ctx, `SELECT `+nodeSelectColumns+`
		FROM memory_nodes n LEFT JOIN claim_values v ON v.node_id=n.node_id WHERE n.node_id=?`, id))
}

func scanMemoryNode(row interface{ Scan(...any) error }) (MemoryNode, error) {
	var node MemoryNode
	var validFrom, validUntil, payloadJSON, createdAt, valueTime, listJSON string
	var number sql.NullFloat64
	var boolValue sql.NullInt64
	err := row.Scan(&node.ID, &node.Space, &node.ContextID, &node.Kind, &node.ClaimType, &node.Statement,
		&node.SubjectID, &node.Subject, &node.Facet, &node.AttributeKey, &node.ScopeKey, &node.SlotID,
		&node.EvidenceMode, &validFrom, &validUntil, &node.Status, &payloadJSON, &createdAt,
		&node.Value.Kind, &node.Value.Text, &number, &node.Value.Unit, &valueTime, &listJSON, &boolValue)
	if err != nil {
		return node, err
	}
	node.ValidFrom, node.ValidUntil, node.CreatedAt = parseFabricTime(validFrom), parseFabricTime(validUntil), parseFabricTime(createdAt)
	node.Value.Time = parseFabricTime(valueTime)
	if number.Valid {
		node.Value.Number = number.Float64
	}
	if boolValue.Valid {
		value := boolValue.Int64 != 0
		node.Value.Bool = &value
	}
	_ = json.Unmarshal([]byte(listJSON), &node.Value.List)
	_ = json.Unmarshal([]byte(payloadJSON), &node.Payload)
	return node, nil
}

func loadSlotNodesTx(ctx context.Context, tx *sql.Tx, slotID, excludeID string) ([]MemoryNode, error) {
	rows, err := tx.QueryContext(ctx, `SELECT `+nodeSelectColumns+` FROM memory_nodes n
		LEFT JOIN claim_values v ON v.node_id=n.node_id
		WHERE n.slot_id=? AND n.node_id!=? AND n.status IN (?, ?, ?) AND n.tombstoned=0
		ORDER BY n.valid_from, n.created_at`, slotID, excludeID, SemanticActive, SemanticScoped, SemanticPendingResolution)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var nodes []MemoryNode
	for rows.Next() {
		node, err := scanMemoryNode(rows)
		if err != nil {
			return nil, err
		}
		node.Sources, _ = loadNodeSourcesTx(ctx, tx, node.ID)
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

func loadNodeSourcesTx(ctx context.Context, tx *sql.Tx, nodeID string) ([]SourceSpan, error) {
	rows, err := tx.QueryContext(ctx, `SELECT event_id, start_rune, end_rune, source_role
		FROM node_sources WHERE node_id=? ORDER BY event_id, start_rune`, nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SourceSpan
	for rows.Next() {
		var source SourceSpan
		if err := rows.Scan(&source.EventID, &source.StartRune, &source.EndRune, &source.Role); err != nil {
			return nil, err
		}
		result = append(result, source)
	}
	return result, rows.Err()
}

func loadNodeEventsTx(ctx context.Context, tx *sql.Tx, nodeID string) map[string]RawEvent {
	rows, err := tx.QueryContext(ctx, `SELECT e.event_id, e.space, e.context_id, e.session_id, e.actor,
		e.source_kind, e.content, e.occurred_at, e.source_ref, e.metadata_json
		FROM node_sources s JOIN events e ON e.event_id=s.event_id WHERE s.node_id=?`, nodeID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := map[string]RawEvent{}
	for rows.Next() {
		event, err := scanEvent(rows)
		if err == nil {
			result[event.ID] = event
		}
	}
	return result
}

func (f *Fabric) loadMemoryNodes(ctx context.Context, ids []string) ([]MemoryNode, error) {
	result := make([]MemoryNode, 0, len(ids))
	for _, id := range uniqueStrings(ids) {
		node, err := f.loadMemoryNode(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, nil
}

func (f *Fabric) loadMemoryNode(ctx context.Context, id string) (MemoryNode, error) {
	node, err := scanMemoryNode(f.ledger.QueryRowContext(ctx, `SELECT `+nodeSelectColumns+`
		FROM memory_nodes n LEFT JOIN claim_values v ON v.node_id=n.node_id WHERE n.node_id=?`, id))
	if err != nil {
		return node, err
	}
	rows, err := f.ledger.QueryContext(ctx, `SELECT event_id, start_rune, end_rune, source_role
		FROM node_sources WHERE node_id=? ORDER BY event_id, start_rune`, id)
	if err == nil {
		for rows.Next() {
			var source SourceSpan
			if scanErr := rows.Scan(&source.EventID, &source.StartRune, &source.EndRune, &source.Role); scanErr == nil {
				node.Sources = append(node.Sources, source)
			}
		}
		_ = rows.Close()
	}
	keyRows, err := f.ledger.QueryContext(ctx, `SELECT key_type, key_text FROM node_keys WHERE node_id=? ORDER BY key_type, key_text`, id)
	if err == nil {
		for keyRows.Next() {
			var kind, value string
			if scanErr := keyRows.Scan(&kind, &value); scanErr == nil {
				if kind == "trigger" {
					node.RetrievalCues = append(node.RetrievalCues, value)
				} else {
					node.Keys = append(node.Keys, value)
				}
			}
		}
		_ = keyRows.Close()
	}
	return node, nil
}

func (f *Fabric) projectNode(ctx context.Context, nodeID string, seq int64) error {
	node, err := f.loadMemoryNode(ctx, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return f.deleteIndexedResource(ctx, nodeID, seq)
	}
	if err != nil {
		return err
	}
	sourceIDs := make([]string, 0, len(node.Sources))
	for _, source := range node.Sources {
		sourceIDs = append(sourceIDs, source.EventID)
	}
	keys := append([]string{node.Subject, string(node.Facet), node.AttributeKey, node.Value.Text, node.Value.Unit}, node.Keys...)
	keys = append(keys, node.RetrievalCues...)
	vectorText := strings.TrimSpace(strings.Join([]string{node.Statement,
		strings.Join(node.Keys, " "), strings.Join(node.RetrievalCues, " ")}, "\n"))
	if err := f.upsertIndexDocument(ctx, indexedDocument{ID: node.ID, Space: node.Space, ResourceKind: "node",
		ResourceID: node.ID, Content: node.Statement, Keys: keys, ContextID: node.ContextID,
		OccurredAt: firstTimeMemory(node.ValidFrom, node.CreatedAt), SlotID: node.SlotID, Status: node.Status,
		SourceEventIDs: uniqueStrings(sourceIDs), LedgerSeq: seq, IndexFTS: true, IndexVector: true,
		VectorText: vectorText,
		Metadata: map[string]any{"kind": node.Kind, "claim_type": node.ClaimType, "facet": node.Facet,
			"attribute_key": node.AttributeKey, "value": node.Value}}); err != nil {
		return err
	}
	if _, err := f.index.ExecContext(ctx, `DELETE FROM active_slots WHERE node_id=?`, node.ID); err != nil {
		return err
	}
	if node.SlotID != "" && (node.Status == SemanticActive || node.Status == SemanticScoped) {
		resolutionID := ""
		_ = f.ledger.QueryRowContext(ctx, `SELECT r.resolution_id FROM conflict_members cm
			JOIN resolutions r ON r.conflict_id=cm.conflict_id WHERE cm.node_id=?
			ORDER BY r.created_at DESC LIMIT 1`, node.ID).Scan(&resolutionID)
		_, err = f.index.ExecContext(ctx, `INSERT INTO active_slots(
			slot_id, node_id, space, semantic_status, valid_from, valid_until, resolution_id)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(slot_id, node_id) DO UPDATE SET
				space=excluded.space, semantic_status=excluded.semantic_status,
				valid_from=excluded.valid_from, valid_until=excluded.valid_until,
				resolution_id=excluded.resolution_id`, node.SlotID, node.ID, node.Space, node.Status,
			formatFabricTime(node.ValidFrom), formatFabricTime(node.ValidUntil), resolutionID)
	}
	return err
}

func firstTimeMemory(values ...time.Time) time.Time {
	for _, value := range values {
		if !value.IsZero() {
			return value
		}
	}
	return time.Time{}
}
