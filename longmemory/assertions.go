package longmemory

import (
	"context"
	"sort"
	"strings"
	"time"
)

func (s *Store) BuildAssertionViews(ctx context.Context, selected []CandidateScore, reference time.Time) ([]AssertionView, error) {
	ids := make([]string, 0, len(selected)*2)
	var scopes []Scope
	seenScopes := map[string]struct{}{}
	for _, candidate := range selected {
		ids = append(ids, candidate.MemoryID, candidate.Entry.ParentID)
		key := string(candidate.Entry.ScopeType) + "\x00" + candidate.Entry.ScopeKey
		if candidate.Entry.ScopeType != "" && candidate.Entry.ScopeKey != "" {
			if _, ok := seenScopes[key]; !ok {
				seenScopes[key] = struct{}{}
				scopes = append(scopes, Scope{Type: candidate.Entry.ScopeType, Key: candidate.Entry.ScopeKey})
			}
		}
	}
	ids = normalizeStrings(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	supportSources := map[string][]string{}
	edgeArgs := make([]any, 0, len(ids)+1+len(scopes)*2)
	for _, id := range ids {
		edgeArgs = append(edgeArgs, id)
	}
	edgeArgs = append(edgeArgs, EdgeSupports)
	edgeWhere := `to_id IN (` + marks + `) AND edge_type=?`
	if scopeSQL, scopeArgs := scopedClauses(scopes, ""); scopeSQL != "" {
		edgeWhere += " AND (" + scopeSQL + ")"
		edgeArgs = append(edgeArgs, scopeArgs...)
	}
	edgeRows, edgeErr := s.db.QueryContext(ctx, `SELECT from_id, to_id FROM memory_edges WHERE `+edgeWhere, edgeArgs...)
	if edgeErr != nil {
		return nil, edgeErr
	}
	for edgeRows.Next() {
		var memoryID, sourceID string
		if err := edgeRows.Scan(&memoryID, &sourceID); err != nil {
			edgeRows.Close()
			return nil, err
		}
		supportSources[memoryID] = append(supportSources[memoryID], sourceID)
	}
	if err := edgeRows.Close(); err != nil {
		return nil, err
	}
	args := make([]any, 0, len(ids)*3+len(scopes)*2)
	for _, id := range ids {
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	for _, id := range ids {
		args = append(args, id)
	}
	where := `(f.memory_id IN (` + marks + `) OR f.fact_id IN (
		SELECT e.from_id FROM memory_edges e WHERE e.to_id IN (` + marks + `)
	) OR f.memory_id IN (
		SELECT e.from_id FROM memory_edges e WHERE e.to_id IN (` + marks + `)
	))`
	if scopeSQL, scopeArgs := scopedClauses(scopes, "f."); scopeSQL != "" {
		where += " AND (" + scopeSQL + ")"
		args = append(args, scopeArgs...)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT fact_id, memory_id, scope_type, scope_key, subject,
		predicate, object, qualifiers_json, fact_key, confidence, valid_from, valid_until, observed_at,
		invalidated_at, superseded_by, status FROM memory_facts f WHERE `+where+`
		ORDER BY fact_key, valid_from, observed_at`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	facts, err := scanFacts(rows)
	if err != nil {
		return nil, err
	}
	views := map[string]*AssertionView{}
	for _, fact := range facts {
		view := views[fact.FactKey]
		if view == nil {
			view = &AssertionView{FactKey: fact.FactKey}
			views[fact.FactKey] = view
		}
		version := AssertionVersion{FactID: fact.FactID, Subject: fact.Subject, Predicate: fact.Predicate,
			Object: fact.Object, ValidFrom: fact.ValidFrom, ValidUntil: fact.ValidUntil,
			ObservedAt: fact.ObservedAt, InvalidatedAt: fact.InvalidatedAt,
			SourceChunks: normalizeStrings(supportSources[fact.MemoryID])}
		if assertionCurrentAt(fact, reference) {
			view.Current = append(view.Current, version)
		} else {
			view.Historical = append(view.Historical, version)
		}
	}
	result := make([]AssertionView, 0, len(views))
	for _, view := range views {
		if len(view.Current) > 1 {
			view.Conflicting = append(view.Conflicting, view.Current...)
		}
		result = append(result, *view)
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].FactKey < result[j].FactKey })
	return result, nil
}

func assertionCurrentAt(fact Fact, reference time.Time) bool {
	if reference.IsZero() {
		return fact.Status == StatusActive && fact.InvalidatedAt.IsZero()
	}
	if !fact.ValidFrom.IsZero() && fact.ValidFrom.After(reference) {
		return false
	}
	if !fact.ValidUntil.IsZero() && !fact.ValidUntil.After(reference) {
		return false
	}
	if !fact.ObservedAt.IsZero() && fact.ObservedAt.After(reference) {
		return false
	}
	return fact.InvalidatedAt.IsZero() || fact.InvalidatedAt.After(reference)
}
