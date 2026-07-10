package longmemory

import (
	"context"
	"sort"
	"strings"
)

type sessionRank struct {
	SessionID string
	ScopeType ScopeType
	ScopeKey  string
	Rank      float64
}

func (s *Store) InspectCatalog(ctx context.Context, scopes []Scope) (MemoryCatalog, error) {
	catalog := MemoryCatalog{ByType: map[string]int{}, ByScope: map[string]int{}}
	where, args := buildFilters(SearchOptions{Scopes: scopes, IncludeExpired: true}, false)
	rows, err := s.db.QueryContext(ctx, `SELECT memory_type, scope_type, scope_key, COUNT(*), MIN(valid_from), MAX(valid_from)
		FROM memories `+where+` GROUP BY memory_type, scope_type, scope_key`, args...)
	if err != nil {
		return catalog, err
	}
	for rows.Next() {
		var memoryType, scopeType, scopeKey, oldest, newest string
		var count int
		if err := rows.Scan(&memoryType, &scopeType, &scopeKey, &count, &oldest, &newest); err != nil {
			rows.Close()
			return catalog, err
		}
		catalog.TotalMemories += count
		catalog.ByType[memoryType] += count
		catalog.ByScope[scopeType+"/"+scopeKey] += count
		oldTime, newTime := parseTime(oldest), parseTime(newest)
		if !oldTime.IsZero() && (catalog.Oldest.IsZero() || oldTime.Before(catalog.Oldest)) {
			catalog.Oldest = oldTime
		}
		if newTime.After(catalog.Newest) {
			catalog.Newest = newTime
		}
	}
	if err := rows.Close(); err != nil {
		return catalog, err
	}
	if err := rows.Err(); err != nil {
		return catalog, err
	}
	for table, target := range map[string]*int{
		"memory_episodes": &catalog.TotalEpisodes,
		"memory_facts":    &catalog.TotalFacts,
		"memory_edges":    &catalog.TotalEdges,
	} {
		count, err := s.countScopedRows(ctx, table, scopes)
		if err != nil {
			return catalog, err
		}
		*target = count
	}
	return catalog, nil
}

func (s *Store) countScopedRows(ctx context.Context, table string, scopes []Scope) (int, error) {
	clauses, args := scopedClauses(scopes, "")
	query := "SELECT COUNT(*) FROM " + table
	if clauses != "" {
		query += " WHERE " + clauses
	}
	var count int
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&count)
	return count, err
}

func (s *Store) SearchEntities(ctx context.Context, queries, entities []string, scopes []Scope, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 40
	}
	terms := normalizeEntityTerms(append(append([]string(nil), entities...), queries...))
	if len(terms) == 0 {
		return nil, nil
	}
	scopeSQL, scopeArgs := scopedClauses(scopes, "e.")
	clauses := []string{"m.status=?"}
	args := []any{StatusActive}
	if scopeSQL != "" {
		clauses = append(clauses, scopeSQL)
		args = append(args, scopeArgs...)
	}
	var termClauses []string
	for _, term := range terms {
		termClauses = append(termClauses, `(e.normalized_entity LIKE ? OR ? LIKE '%' || e.normalized_entity || '%')`)
		args = append(args, "%"+term+"%", term)
	}
	clauses = append(clauses, "("+strings.Join(termClauses, " OR ")+")")
	args = append(args, limit*4)
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixedMemoryColumns("m")+`, 0 AS rank
		FROM memory_entities e JOIN memories m ON m.memory_id=e.memory_id
		WHERE `+strings.Join(clauses, " AND ")+` ORDER BY e.confidence DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries, err := scanEntriesWithRank(rows)
	if err != nil {
		return nil, err
	}
	entries = appendUniqueEntries(nil, entries)
	for index := range entries {
		entries[index].Score = float64(len(entries) - index)
		entries[index].MatchReason = "entity"
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

func (s *Store) SearchSessions(ctx context.Context, queries []string, scopes []Scope, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 40
	}
	ranked := map[string]sessionRank{}
	for _, query := range normalizeStrings(queries) {
		ftsQuery := sanitizeFTSQuery(query)
		if ftsQuery == "" {
			continue
		}
		searchScopes := append([]Scope(nil), scopes...)
		if len(searchScopes) == 0 {
			searchScopes = []Scope{{}}
		}
		for _, scope := range searchScopes {
			clauses := []string{"memory_session_fts MATCH ?"}
			args := []any{ftsQuery}
			if scope.Type != "" && strings.TrimSpace(scope.Key) != "" {
				clauses = append([]string{"s.scope_type=?", "s.scope_key=?"}, clauses...)
				args = append([]any{scope.Type, scope.Key}, args...)
			}
			args = append(args, limit)
			rows, err := s.db.QueryContext(ctx, `SELECT s.session_id, s.scope_type, s.scope_key, bm25(memory_session_fts)
				FROM memory_session_fts JOIN memory_session_index s ON s.index_id=memory_session_fts.index_id
				WHERE `+strings.Join(clauses, " AND ")+` ORDER BY bm25(memory_session_fts) LIMIT ?`, args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var item sessionRank
				if err := rows.Scan(&item.SessionID, &item.ScopeType, &item.ScopeKey, &item.Rank); err != nil {
					rows.Close()
					return nil, err
				}
				item.Rank = -item.Rank
				key := sessionScopeKey(item.ScopeType, item.ScopeKey, item.SessionID)
				if prior, ok := ranked[key]; !ok || item.Rank > prior.Rank {
					ranked[key] = item
				}
			}
			if err := rows.Close(); err != nil {
				return nil, err
			}
		}
	}
	if len(ranked) == 0 {
		return nil, nil
	}
	var sessions []sessionRank
	for _, item := range ranked {
		sessions = append(sessions, item)
	}
	sort.SliceStable(sessions, func(i, j int) bool { return sessions[i].Rank > sessions[j].Rank })
	if len(sessions) > limit {
		sessions = sessions[:limit]
	}
	return s.memoriesForSessions(ctx, sessions, scopes, limit)
}

func (s *Store) memoriesForSessions(ctx context.Context, sessions []sessionRank, scopes []Scope, limit int) ([]Entry, error) {
	if len(sessions) == 0 {
		return nil, nil
	}
	scopeSQL, scopeArgs := scopedClauses(scopes, "m.")
	clauses := []string{"m.status=?"}
	args := []any{StatusActive}
	if scopeSQL != "" {
		clauses = append(clauses, scopeSQL)
		args = append(args, scopeArgs...)
	}
	placeholders := make([]string, 0, len(sessions))
	ranks := make(map[string]float64, len(sessions))
	for _, session := range sessions {
		placeholders = append(placeholders, "?")
		args = append(args, session.SessionID)
		ranks[sessionScopeKey(session.ScopeType, session.ScopeKey, session.SessionID)] = session.Rank
	}
	clauses = append(clauses, "m.source_session_id IN ("+strings.Join(placeholders, ",")+")")
	args = append(args, maxInt(limit*4, limit))
	rows, err := s.db.QueryContext(ctx, `SELECT `+prefixedMemoryColumns("m")+`, 0 AS rank
		FROM memories m WHERE `+strings.Join(clauses, " AND ")+`
		ORDER BY m.importance DESC, m.updated_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries, err := scanEntriesWithRank(rows)
	if err != nil {
		return nil, err
	}
	for index := range entries {
		entries[index].Score = ranks[sessionScopeKey(entries[index].ScopeType, entries[index].ScopeKey, entries[index].SourceSessionID)]
		entries[index].MatchReason = "session"
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Score == entries[j].Score {
			return entries[i].Importance > entries[j].Importance
		}
		return entries[i].Score > entries[j].Score
	})
	entries = appendUniqueEntries(nil, entries)
	sessionEntries, err := s.sessionEntriesBySessionIDs(ctx, sessions, scopes)
	if err != nil {
		return nil, err
	}
	entries = appendUniqueEntries(entries, sessionEntries)
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Score == entries[j].Score {
			return entries[i].Importance > entries[j].Importance
		}
		return entries[i].Score > entries[j].Score
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

func (s *Store) sessionEntriesBySessionIDs(ctx context.Context, sessions []sessionRank, scopes []Scope) ([]Entry, error) {
	if len(sessions) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(sessions))
	ranks := make(map[string]float64, len(sessions))
	for _, session := range sessions {
		ids = append(ids, session.SessionID)
		ranks[sessionScopeKey(session.ScopeType, session.ScopeKey, session.SessionID)] = session.Rank
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, 0, len(ids)+len(scopes)*2)
	for _, id := range ids {
		args = append(args, id)
	}
	scopeSQL, scopeArgs := scopedClauses(scopes, "")
	query := `SELECT index_id, scope_type, scope_key, session_id, summary, keyphrases_json, entities_json,
		roles_json, started_at, ended_at FROM memory_session_index WHERE session_id IN (` + marks + `)`
	if scopeSQL != "" {
		query += " AND (" + scopeSQL + ")"
		args = append(args, scopeArgs...)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		entry, sessionID, err := scanSessionEntry(rows)
		if err != nil {
			return nil, err
		}
		entry.Score = ranks[sessionScopeKey(entry.ScopeType, entry.ScopeKey, sessionID)]
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func sessionScopeKey(scopeType ScopeType, scopeKey, sessionID string) string {
	return string(scopeType) + "\x00" + scopeKey + "\x00" + sessionID
}

func (s *Store) sessionEntriesByIndexIDs(ctx context.Context, ids []string) ([]Entry, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for index, id := range ids {
		args[index] = id
	}
	rows, err := s.db.QueryContext(ctx, `SELECT index_id, scope_type, scope_key, session_id, summary,
		keyphrases_json, entities_json, roles_json, started_at, ended_at FROM memory_session_index
		WHERE index_id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		entry, _, err := scanSessionEntry(rows)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, rows.Err()
}

func scanSessionEntry(scanner interface{ Scan(...any) error }) (Entry, string, error) {
	var entry Entry
	var sessionID, tags, entities, roles, startedAt, endedAt string
	if err := scanner.Scan(&entry.MemoryID, &entry.ScopeType, &entry.ScopeKey, &sessionID, &entry.Content,
		&tags, &entities, &roles, &startedAt, &endedAt); err != nil {
		return Entry{}, "", err
	}
	entry.MemoryType = TypeEpisodic
	entry.Title = "Session " + sessionID
	entry.Summary = clipRunes(entry.Content, 800)
	entry.Tags = append(fromJSONList(tags), fromJSONList(roles)...)
	entry.Entities = fromJSONList(entities)
	entry.Importance = 0.5
	entry.Confidence = 1
	entry.SourceSessionID = sessionID
	entry.ValidFrom = parseTime(startedAt)
	entry.CreatedAt = entry.ValidFrom
	entry.UpdatedAt = parseTime(endedAt)
	entry.Status = StatusActive
	entry.MatchReason = "session"
	return entry, sessionID, nil
}

func (s *Store) SearchTemporal(ctx context.Context, queries []string, constraints []TemporalConstraint, scopes []Scope, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 40
	}
	combined := map[string]Entry{}
	for _, query := range normalizeStrings(queries) {
		entries, err := s.Search(ctx, SearchOptions{Query: query, Scopes: scopes, IncludeExpired: true,
			Limit: limit, MaxCandidates: limit * 2})
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if temporalEntryAllowed(entry, constraints) {
				entry.MatchReason = "temporal"
				combined[entry.MemoryID] = entry
			}
		}
	}
	entries := make([]Entry, 0, len(combined))
	for _, entry := range combined {
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		left, right := entries[i].ValidFrom, entries[j].ValidFrom
		if left.Equal(right) {
			return entries[i].Score > entries[j].Score
		}
		return left.After(right)
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, nil
}

func temporalEntryAllowed(entry Entry, constraints []TemporalConstraint) bool {
	if len(constraints) == 0 {
		return !entry.ValidFrom.IsZero() || !entry.ValidUntil.IsZero()
	}
	for _, constraint := range constraints {
		point := entry.ValidFrom
		if point.IsZero() {
			point = entry.CreatedAt
		}
		if !constraint.At.IsZero() {
			if (entry.ValidFrom.IsZero() || !entry.ValidFrom.After(constraint.At)) &&
				(entry.ValidUntil.IsZero() || entry.ValidUntil.After(constraint.At)) {
				return true
			}
			continue
		}
		if !constraint.From.IsZero() && point.Before(constraint.From) {
			continue
		}
		if !constraint.To.IsZero() && point.After(constraint.To) {
			continue
		}
		return true
	}
	return false
}

func scopedClauses(scopes []Scope, prefix string) (string, []any) {
	var clauses []string
	var args []any
	for _, scope := range scopes {
		if scope.Type == "" || strings.TrimSpace(scope.Key) == "" {
			continue
		}
		clauses = append(clauses, "("+prefix+"scope_type=? AND "+prefix+"scope_key=?)")
		args = append(args, scope.Type, scope.Key)
	}
	return strings.Join(clauses, " OR "), args
}

func normalizeEntityTerms(values []string) []string {
	var terms []string
	for _, value := range values {
		value = normalizeEntityValue(value)
		if value == "" {
			continue
		}
		terms = append(terms, value)
		for _, token := range strings.FieldsFunc(value, func(r rune) bool {
			return !(r == '_' || r == '-' || r == '.' || r == '/' || r == ':' || r == '@' ||
				(r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r > 127)
		}) {
			if len([]rune(token)) >= 2 {
				terms = append(terms, token)
			}
		}
	}
	terms = normalizeStrings(terms)
	if len(terms) > 24 {
		terms = terms[:24]
	}
	return terms
}
