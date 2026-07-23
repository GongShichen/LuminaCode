package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	localResolutionPolicy = "fabric/local-authority"
	apiResolutionPolicy   = "fabric/api-adjudicator"
)

func createConflictTx(ctx context.Context, tx *sql.Tx, members []MemoryNode, critical bool, now time.Time) (Conflict, error) {
	var conflict Conflict
	if len(members) < 2 {
		return conflict, errors.New("a conflict set requires at least two claims")
	}
	space := normalizeSpace(members[0].Space)
	slotID := members[0].SlotID
	if slotID == "" {
		return conflict, errors.New("a conflict set requires a slot")
	}
	memberParts := make([]string, 0, len(members))
	for _, member := range members {
		if normalizeSpace(member.Space) != space || member.SlotID != slotID {
			return conflict, errors.New("conflict members must share a space and slot")
		}
		memberParts = append(memberParts, member.ID+"\x1f"+claimValueKey(member.Value))
	}
	sort.Strings(memberParts)
	generation := contentHash(slotID, strings.Join(memberParts, "\x1e"))
	conflictID := stableFabricID("conf", slotID, generation)
	criticalValue := 0
	if critical {
		criticalValue = 1
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO conflict_sets(
		conflict_id, space, slot_id, generation, content_hash, status, critical, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(slot_id, generation) DO UPDATE SET
			critical=MAX(conflict_sets.critical, excluded.critical), updated_at=excluded.updated_at`,
		conflictID, space, slotID, generation, generation, SemanticPendingResolution, criticalValue,
		formatFabricTime(now), formatFabricTime(now))
	if err != nil {
		return conflict, err
	}
	for _, member := range members {
		if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_members(conflict_id, node_id)
			VALUES (?, ?) ON CONFLICT DO NOTHING`, conflictID, member.ID); err != nil {
			return conflict, err
		}
	}
	conflict = Conflict{ID: conflictID, Space: space, SlotID: slotID, Generation: generation,
		Status: SemanticPendingResolution, Members: append([]MemoryNode(nil), members...), CreatedAt: now}
	return conflict, nil
}

func createLocalResolutionTx(ctx context.Context, tx *sql.Tx, members []MemoryNode, winnerID string,
	decision ConflictDecision, now time.Time) (Conflict, Resolution, error) {
	conflict, err := createConflictTx(ctx, tx, members, true, now)
	if err != nil {
		return conflict, Resolution{}, err
	}
	memberIDs := conflictMemberIDs(conflict)
	if !containsString(memberIDs, winnerID) {
		return conflict, Resolution{}, fmt.Errorf("local resolution winner %q is not a conflict member", winnerID)
	}
	losers := make([]string, 0, len(memberIDs)-1)
	for _, id := range memberIDs {
		if id != winnerID {
			losers = append(losers, id)
		}
	}
	resolution := Resolution{ID: stableFabricID("res", conflict.ID, conflict.Generation, string(decision), winnerID),
		ConflictID: conflict.ID, Generation: conflict.Generation, Decision: decision,
		WinnerIDs: []string{winnerID}, LoserIDs: losers, SupportIDs: conflictSourceIDs(conflict),
		Reason: "deterministic source authority and temporal ordering", PolicyID: localResolutionPolicy, CreatedAt: now}
	if err := insertResolutionTx(ctx, tx, resolution); err != nil {
		return conflict, Resolution{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conflict_sets SET status=?, updated_at=? WHERE conflict_id=?`,
		SemanticActive, formatFabricTime(now), conflict.ID); err != nil {
		return conflict, Resolution{}, err
	}
	conflict.Status = SemanticActive
	return conflict, resolution, nil
}

func insertResolutionTx(ctx context.Context, tx *sql.Tx, resolution Resolution) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO resolutions(
		resolution_id, conflict_id, generation, decision, winner_ids_json, loser_ids_json,
		conditions, valid_from, valid_until, support_ids_json, reason, policy_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(resolution_id) DO NOTHING`,
		resolution.ID, resolution.ConflictID, resolution.Generation, resolution.Decision,
		marshalJSONArray(resolution.WinnerIDs), marshalJSONArray(resolution.LoserIDs), resolution.Conditions,
		formatFabricTime(resolution.ValidFrom), formatFabricTime(resolution.ValidUntil),
		marshalJSONArray(resolution.SupportIDs), resolution.Reason, resolution.PolicyID,
		formatFabricTime(resolution.CreatedAt))
	return err
}

func (f *Fabric) enqueueAdjudication(ctx context.Context, space, conflictID string, availableAt time.Time) (JobRef, error) {
	var generation string
	if err := f.ledger.QueryRowContext(ctx, `SELECT generation FROM conflict_sets WHERE conflict_id=?`, conflictID).Scan(&generation); err != nil {
		return JobRef{}, err
	}
	return f.enqueueJob(ctx, jobAdjudicateConflict, normalizeSpace(space), conflictID,
		contentHash(conflictID, generation), map[string]string{"conflict_id": conflictID}, availableAt)
}

func (f *Fabric) adjudicateConflictJob(ctx context.Context, job fabricJob) error {
	resolution, _, err := f.resolveConflict(ctx, job.ResourceID)
	if err != nil {
		return err
	}
	if resolution.Decision == DecisionNeedsCompile && f.options.Compiler != nil && f.options.RemoteProcessing != RemoteProcessingOff {
		conflict, loadErr := f.loadConflict(ctx, job.ResourceID, false)
		if loadErr == nil {
			_, _ = f.enqueueCompileEvents(ctx, conflict.Space, "", conflictSourceIDs(conflict), WriteCorrection)
		}
	}
	return nil
}

func (f *Fabric) resolveConflict(ctx context.Context, conflictID string) (Resolution, APIUsage, error) {
	lockValue, _ := f.conflictLocks.LoadOrStore(conflictID, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	conflict, err := f.loadConflict(ctx, conflictID, true)
	if err != nil {
		return Resolution{}, APIUsage{}, err
	}
	if existing, existingErr := f.loadResolution(ctx, conflict.ID, conflict.Generation); existingErr == nil {
		return existing, APIUsage{}, nil
	} else if !errors.Is(existingErr, sql.ErrNoRows) {
		return Resolution{}, APIUsage{}, existingErr
	}
	if f.options.Adjudicator == nil || f.options.RemoteProcessing == RemoteProcessingOff {
		return Resolution{}, APIUsage{}, errors.New("conflict adjudicator is unavailable; active view was not changed")
	}
	prior, err := f.loadPriorResolutions(ctx, conflict)
	if err != nil {
		return Resolution{}, APIUsage{}, err
	}
	response, err := f.options.Adjudicator.Adjudicate(ctx, AdjudicationRequest{
		Conflict: conflict, AuthorityPolicy: authorityPolicyForConflict(conflict), PriorResolutions: prior,
	})
	usage := response.Usage
	usage.Calls = maxIntMemory(1, usage.Calls)
	usageErr := f.observeAPIUsage(ctx, APIUsageEvent{Stage: APIStageConflictAdjudication, Space: conflict.Space,
		ResourceID: conflict.ID, Usage: usage, Error: errorStringMemory(err)})
	if usageErr != nil && err == nil {
		err = usageErr
	}
	if err != nil {
		return Resolution{}, usage, err
	}
	response, err = validateAdjudication(conflict, response)
	if err != nil {
		return Resolution{}, usage, err
	}
	resolution, changed, err := f.persistAdjudication(ctx, conflict, response)
	if err != nil {
		return Resolution{}, usage, err
	}
	if len(changed) > 0 {
		if _, err := f.projectResources(ctx, "node", changed); err != nil {
			return resolution, usage, IndexLagError{Cause: err}
		}
	}
	return resolution, usage, nil
}

// ResolvePendingConflicts performs at most one remote batch call. It is used
// after historical import compiler jobs finish so conflict cost is bounded by
// the case rather than by the number of semantic nodes.
func (f *Fabric) ResolvePendingConflicts(ctx context.Context, space string, limit int) (APIUsage, error) {
	batcher, ok := f.options.Adjudicator.(BatchConflictAdjudicator)
	if !ok || f.options.RemoteProcessing == RemoteProcessingOff {
		return APIUsage{}, nil
	}
	if limit <= 0 || limit > 8 {
		limit = 8
	}
	rows, err := f.ledger.QueryContext(ctx, `SELECT conflict_id FROM conflict_sets
		WHERE space=? AND status IN (?, ?) ORDER BY critical DESC, created_at LIMIT ?`,
		normalizeSpace(space), SemanticPendingResolution, SemanticUnresolved, limit)
	if err != nil {
		return APIUsage{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return APIUsage{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return APIUsage{}, err
	}
	if len(ids) == 0 {
		return APIUsage{}, nil
	}
	request := AdjudicationBatchRequest{}
	conflicts := make(map[string]Conflict, len(ids))
	for _, id := range ids {
		conflict, err := f.loadConflict(ctx, id, true)
		if err != nil {
			return APIUsage{}, err
		}
		if _, err := f.loadResolution(ctx, conflict.ID, conflict.Generation); err == nil {
			continue
		} else if !errors.Is(err, sql.ErrNoRows) {
			return APIUsage{}, err
		}
		prior, err := f.loadPriorResolutions(ctx, conflict)
		if err != nil {
			return APIUsage{}, err
		}
		conflicts[conflict.ID] = conflict
		request.Items = append(request.Items, AdjudicationRequest{Conflict: conflict,
			AuthorityPolicy: authorityPolicyForConflict(conflict), PriorResolutions: prior})
	}
	if len(request.Items) == 0 {
		return APIUsage{}, nil
	}
	response, callErr := batcher.AdjudicateBatch(ctx, request)
	usage := response.Usage
	usage.Calls = maxIntMemory(1, usage.Calls)
	usageErr := f.observeAPIUsage(ctx, APIUsageEvent{Stage: APIStageConflictAdjudication,
		Space: normalizeSpace(space), ResourceID: strings.Join(ids, ","), Usage: usage,
		Error: errorStringMemory(callErr)})
	if usageErr != nil && callErr == nil {
		callErr = usageErr
	}
	if callErr != nil {
		if isPermanentMemoryAPIError(callErr) {
			f.tripRemoteCircuit(callErr)
		}
		return usage, callErr
	}
	seen := map[string]struct{}{}
	for _, item := range response.Results {
		conflict, exists := conflicts[item.ConflictID]
		if !exists {
			return usage, fmt.Errorf("batch adjudicator returned unknown conflict %q", item.ConflictID)
		}
		if _, duplicate := seen[item.ConflictID]; duplicate {
			return usage, fmt.Errorf("batch adjudicator returned conflict %q more than once", item.ConflictID)
		}
		seen[item.ConflictID] = struct{}{}
		item, err = validateAdjudication(conflict, item)
		if err != nil {
			return usage, err
		}
		_, changed, err := f.persistAdjudication(ctx, conflict, item)
		if err != nil {
			return usage, err
		}
		if len(changed) > 0 {
			if _, err := f.projectResources(ctx, "node", changed); err != nil {
				return usage, IndexLagError{Cause: err}
			}
		}
	}
	return usage, nil
}

func (f *Fabric) persistAdjudication(ctx context.Context, conflict Conflict,
	response AdjudicationResponse) (Resolution, []string, error) {
	now := f.now()
	resolution := Resolution{ID: stableFabricID("res", conflict.ID, conflict.Generation, string(response.Decision),
		strings.Join(response.WinnerIDs, "\x1f"), strings.Join(response.LoserIDs, "\x1f"), response.Conditions),
		ConflictID: conflict.ID, Generation: conflict.Generation, Decision: response.Decision,
		WinnerIDs: response.WinnerIDs, LoserIDs: response.LoserIDs, Conditions: response.Conditions,
		ValidFrom: response.ValidFrom, ValidUntil: response.ValidUntil, SupportIDs: response.SupportIDs,
		Reason: response.Reason, PolicyID: apiResolutionPolicy, CreatedAt: now}
	tx, err := f.ledger.BeginTx(ctx, nil)
	if err != nil {
		return Resolution{}, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertResolutionTx(ctx, tx, resolution); err != nil {
		return Resolution{}, nil, err
	}
	memberIDs := conflictMemberIDs(conflict)
	status := SemanticActive
	changed := []string{}
	updates := map[string]SemanticStatus{}
	switch response.Decision {
	case DecisionSupersedes:
		for _, id := range response.WinnerIDs {
			updates[id] = SemanticActive
		}
		for _, id := range response.LoserIDs {
			updates[id] = SemanticSuperseded
		}
	case DecisionDuplicate:
		for _, id := range response.WinnerIDs {
			updates[id] = SemanticActive
		}
		for _, id := range response.LoserIDs {
			updates[id] = SemanticRejected
		}
	case DecisionCoexists:
		for _, id := range memberIDs {
			updates[id] = SemanticActive
		}
	case DecisionScoped:
		for _, id := range memberIDs {
			updates[id] = SemanticScoped
		}
	case DecisionUncertain, DecisionNeedsCompile:
		status = SemanticUnresolved
	}
	for id, next := range updates {
		if _, err := tx.ExecContext(ctx, `UPDATE memory_nodes SET status=?, updated_at=?
			WHERE node_id=? AND tombstoned=0`, next, formatFabricTime(now), id); err != nil {
			return Resolution{}, nil, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE slot_versions SET status=? WHERE node_id=?`, next, id); err != nil {
			return Resolution{}, nil, err
		}
		changed = append(changed, id)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE conflict_sets SET status=?, updated_at=? WHERE conflict_id=?`,
		status, formatFabricTime(now), conflict.ID); err != nil {
		return Resolution{}, nil, err
	}
	for _, id := range uniqueStrings(changed) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO outbox(
			resource_kind, resource_id, operation, status, created_at, updated_at)
			VALUES ('node', ?, 'upsert', 'pending', ?, ?)
			ON CONFLICT(resource_kind, resource_id, operation) DO UPDATE SET status='pending', updated_at=excluded.updated_at`,
			id, formatFabricTime(now), formatFabricTime(now)); err != nil {
			return Resolution{}, nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Resolution{}, nil, err
	}
	return resolution, uniqueStrings(changed), nil
}

func validateAdjudication(conflict Conflict, response AdjudicationResponse) (AdjudicationResponse, error) {
	allowed := make(map[string]struct{}, len(conflict.Members))
	supports := map[string]struct{}{}
	for _, member := range conflict.Members {
		allowed[member.ID] = struct{}{}
		supports[member.ID] = struct{}{}
		for _, source := range member.Sources {
			supports[source.EventID] = struct{}{}
		}
	}
	response.WinnerIDs = sortedUniqueStrings(response.WinnerIDs)
	response.LoserIDs = sortedUniqueStrings(response.LoserIDs)
	response.SupportIDs = sortedUniqueStrings(response.SupportIDs)
	for _, id := range append(append([]string{}, response.WinnerIDs...), response.LoserIDs...) {
		if _, ok := allowed[id]; !ok {
			return response, fmt.Errorf("adjudicator selected unknown claim %q", id)
		}
	}
	for _, id := range response.SupportIDs {
		if _, ok := supports[id]; !ok {
			return response, fmt.Errorf("adjudicator cited unknown support %q", id)
		}
	}
	winners := stringSet(response.WinnerIDs)
	for _, id := range response.LoserIDs {
		if _, ok := winners[id]; ok {
			return response, fmt.Errorf("adjudicator selected claim %q as both winner and loser", id)
		}
	}
	switch response.Decision {
	case DecisionSupersedes, DecisionDuplicate:
		if len(response.WinnerIDs) == 0 || len(response.LoserIDs) == 0 ||
			len(response.WinnerIDs)+len(response.LoserIDs) != len(allowed) {
			return response, errors.New("adjudicator must partition every conflict member for supersedes/duplicate")
		}
	case DecisionCoexists, DecisionScoped:
		response.WinnerIDs = conflictMemberIDs(conflict)
		response.LoserIDs = nil
	case DecisionUncertain, DecisionNeedsCompile:
		response.WinnerIDs, response.LoserIDs = nil, nil
	default:
		return response, fmt.Errorf("unsupported adjudication decision %q", response.Decision)
	}
	return response, nil
}

func (f *Fabric) loadConflict(ctx context.Context, conflictID string, includeExcerpts bool) (Conflict, error) {
	var conflict Conflict
	var created string
	if err := f.ledger.QueryRowContext(ctx, `SELECT conflict_id, space, slot_id, generation, status, created_at
		FROM conflict_sets WHERE conflict_id=?`, conflictID).Scan(&conflict.ID, &conflict.Space, &conflict.SlotID,
		&conflict.Generation, &conflict.Status, &created); err != nil {
		return conflict, err
	}
	conflict.CreatedAt = parseFabricTime(created)
	rows, err := f.ledger.QueryContext(ctx, `SELECT node_id FROM conflict_members WHERE conflict_id=? ORDER BY node_id`, conflictID)
	if err != nil {
		return conflict, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return conflict, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return conflict, err
	}
	conflict.Members, err = f.loadMemoryNodes(ctx, ids)
	if err != nil || !includeExcerpts {
		return conflict, err
	}
	for memberIndex := range conflict.Members {
		for sourceIndex := range conflict.Members[memberIndex].Sources {
			source := &conflict.Members[memberIndex].Sources[sourceIndex]
			var content string
			if err := f.ledger.QueryRowContext(ctx, `SELECT content FROM events WHERE event_id=?`, source.EventID).Scan(&content); err != nil {
				continue
			}
			runes := []rune(content)
			if source.StartRune >= 0 && source.EndRune <= len(runes) && source.StartRune < source.EndRune {
				source.Text = string(runes[source.StartRune:source.EndRune])
				if f.options.RemoteProcessing == RemoteProcessingRedacted {
					source.Text = redactSecrets(source.Text)
				}
			}
		}
	}
	return conflict, nil
}

func (f *Fabric) loadResolution(ctx context.Context, conflictID, generation string) (Resolution, error) {
	return scanResolution(f.ledger.QueryRowContext(ctx, `SELECT resolution_id, conflict_id, generation, decision,
		winner_ids_json, loser_ids_json, conditions, valid_from, valid_until, support_ids_json,
		reason, policy_id, created_at FROM resolutions WHERE conflict_id=? AND generation=?
		ORDER BY created_at DESC LIMIT 1`, conflictID, generation))
}

func scanResolution(row interface{ Scan(...any) error }) (Resolution, error) {
	var resolution Resolution
	var winners, losers, supports, validFrom, validUntil, created string
	err := row.Scan(&resolution.ID, &resolution.ConflictID, &resolution.Generation, &resolution.Decision,
		&winners, &losers, &resolution.Conditions, &validFrom, &validUntil, &supports,
		&resolution.Reason, &resolution.PolicyID, &created)
	if err != nil {
		return resolution, err
	}
	_ = json.Unmarshal([]byte(winners), &resolution.WinnerIDs)
	_ = json.Unmarshal([]byte(losers), &resolution.LoserIDs)
	_ = json.Unmarshal([]byte(supports), &resolution.SupportIDs)
	resolution.ValidFrom, resolution.ValidUntil = parseFabricTime(validFrom), parseFabricTime(validUntil)
	resolution.CreatedAt = parseFabricTime(created)
	return resolution, nil
}

func (f *Fabric) loadPriorResolutions(ctx context.Context, conflict Conflict) ([]Resolution, error) {
	rows, err := f.ledger.QueryContext(ctx, `SELECT r.resolution_id, r.conflict_id, r.generation, r.decision,
		r.winner_ids_json, r.loser_ids_json, r.conditions, r.valid_from, r.valid_until,
		r.support_ids_json, r.reason, r.policy_id, r.created_at
		FROM resolutions r JOIN conflict_sets c ON c.conflict_id=r.conflict_id
		WHERE c.slot_id=? AND r.generation!=? ORDER BY r.created_at DESC LIMIT 8`, conflict.SlotID, conflict.Generation)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Resolution
	for rows.Next() {
		resolution, err := scanResolution(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, resolution)
	}
	return result, rows.Err()
}

func (f *Fabric) PrioritizeConflicts(ctx context.Context, selector ConflictSelector) (JobRef, error) {
	space := normalizeSpace(selector.Space)
	clauses := []string{"space=?", "status IN (?, ?)"}
	args := []any{space, SemanticPendingResolution, SemanticUnresolved}
	var selected []string
	if len(selector.ConflictIDs) > 0 {
		clauses = append(clauses, "conflict_id IN ("+placeholders(len(selector.ConflictIDs))+")")
		for _, id := range selector.ConflictIDs {
			args = append(args, id)
		}
	}
	if len(selector.SlotIDs) > 0 {
		clauses = append(clauses, "slot_id IN ("+placeholders(len(selector.SlotIDs))+")")
		for _, id := range selector.SlotIDs {
			args = append(args, id)
		}
	}
	rows, err := f.ledger.QueryContext(ctx, `SELECT conflict_id FROM conflict_sets WHERE `+
		strings.Join(clauses, " AND ")+` ORDER BY critical DESC, created_at`, args...)
	if err != nil {
		return JobRef{}, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return JobRef{}, err
		}
		selected = append(selected, id)
	}
	if err := rows.Close(); err != nil {
		return JobRef{}, err
	}
	if len(selected) == 0 {
		return JobRef{Kind: jobAdjudicateConflict, Status: "complete"}, nil
	}
	if _, err := f.ledger.ExecContext(ctx, `UPDATE conflict_sets SET critical=1, updated_at=? WHERE conflict_id IN (`+
		placeholders(len(selected))+`)`, appendAny([]any{formatFabricTime(f.now())}, selected)...); err != nil {
		return JobRef{}, err
	}
	if f.options.Adjudicator == nil || f.options.RemoteProcessing == RemoteProcessingOff {
		return JobRef{ID: stableFabricID("jobset", strings.Join(selected, "\x1f")), Kind: jobAdjudicateConflict, Status: "disabled"}, nil
	}
	for _, id := range selected {
		if _, err := f.enqueueAdjudication(ctx, space, id, f.now()); err != nil {
			return JobRef{}, err
		}
	}
	f.wakeWorker()
	return JobRef{ID: stableFabricID("jobset", strings.Join(selected, "\x1f")), Kind: "adjudicate_conflicts", Status: "pending"}, nil
}

func authorityPolicyForConflict(conflict Conflict) string {
	facet := Facet("")
	if len(conflict.Members) > 0 {
		facet = conflict.Members[0].Facet
	}
	switch facet {
	case FacetPreference, FacetConstraint:
		return "Later explicit user declarations outrank inferred claims; different conditions or scopes coexist."
	case FacetState, FacetConfiguration:
		return "Direct tool observations outrank declarations and inference; different environments or valid times coexist."
	case FacetProcedureState:
		return "Successful execution traces outrank proposed procedures; environment and preconditions define scope."
	default:
		return "Direct document/tool evidence outranks inference. Do not use recency alone when intent or scope is ambiguous."
	}
}

func conflictMemberIDs(conflict Conflict) []string {
	ids := make([]string, 0, len(conflict.Members))
	for _, member := range conflict.Members {
		ids = append(ids, member.ID)
	}
	return sortedUniqueStrings(ids)
}

func conflictSourceIDs(conflict Conflict) []string {
	var ids []string
	for _, member := range conflict.Members {
		for _, source := range member.Sources {
			ids = append(ids, source.EventID)
		}
	}
	return sortedUniqueStrings(ids)
}

func sortedUniqueStrings(values []string) []string {
	values = uniqueStrings(values)
	sort.Strings(values)
	return values
}

func stringSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func containsString(values []string, target string) bool {
	_, ok := stringSet(values)[target]
	return ok
}

func mergeAPIUsage(left, right APIUsage) APIUsage {
	models := uniqueStrings([]string{left.Model, right.Model})
	return APIUsage{Calls: left.Calls + right.Calls, InputTokens: left.InputTokens + right.InputTokens,
		CacheReadInputTokens:     left.CacheReadInputTokens + right.CacheReadInputTokens,
		CacheCreationInputTokens: left.CacheCreationInputTokens + right.CacheCreationInputTokens,
		OutputTokens:             left.OutputTokens + right.OutputTokens, Model: strings.Join(models, ",")}
}
