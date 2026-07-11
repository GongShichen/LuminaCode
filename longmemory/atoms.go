package longmemory

import (
	"context"
	"database/sql"
	"math"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/viant/sqlite-vec/vector"
)

const (
	defaultAtomTargetTokens = 96
	defaultAtomMaxTokens    = 160
)

func BuildEvidenceAtoms(chunk EvidenceChunk, targetTokens, maxTokens int) []EvidenceAtom {
	if targetTokens <= 0 {
		targetTokens = defaultAtomTargetTokens
	}
	if maxTokens < targetTokens {
		maxTokens = maxInt(targetTokens, defaultAtomMaxTokens)
	}
	text := strings.TrimSpace(chunk.Text)
	if text == "" {
		return nil
	}
	runes := []rune(text)
	targetRunes, maxRunes := targetTokens*3, maxTokens*3
	boundaries := atomBoundaries(runes, maxRunes)
	start := 0
	var atoms []EvidenceAtom
	for start < len(runes) {
		target := minInt(start+targetRunes, len(runes))
		end := minInt(start+maxRunes, len(runes))
		for _, boundary := range boundaries {
			if boundary <= start {
				continue
			}
			if boundary > end {
				break
			}
			if atomHardBoundary(runes, boundary) || boundary >= target || end == len(runes) {
				end = boundary
				break
			}
		}
		if end <= start {
			end = minInt(start+maxRunes, len(runes))
		}
		segmentStart, segmentEnd := trimRuneRange(runes, start, end)
		if segmentEnd > segmentStart {
			atomText := string(runes[segmentStart:segmentEnd])
			absoluteStart := chunk.StartRune + segmentStart
			absoluteEnd := chunk.StartRune + segmentEnd
			hash := StableID(chunk.ScopeType, chunk.ScopeKey, "atom-content", atomText)
			atomID := StableID(chunk.ScopeType, chunk.ScopeKey, "atom:"+chunk.ChunkID,
				strings.Join([]string{chunk.MessageID, itoa(absoluteStart), itoa(absoluteEnd), hash}, "\x00"))
			atoms = append(atoms, EvidenceAtom{AtomID: atomID, ChunkID: chunk.ChunkID,
				MessageID: chunk.MessageID, SessionID: chunk.SessionID, ScopeType: chunk.ScopeType,
				ScopeKey: chunk.ScopeKey, Role: chunk.Role, Text: atomText, StartRune: absoluteStart,
				EndRune: absoluteEnd, OccurredAt: chunk.OccurredAt, ValidFrom: chunk.ValidFrom,
				ValidUntil: chunk.ValidUntil, EpistemicStatus: defaultEpistemicStatus(chunk.Role, atomText),
				ContentHash: hash})
		}
		start = end
	}
	return atoms
}

func atomHardBoundary(runes []rune, boundary int) bool {
	if boundary <= 0 || boundary > len(runes) || runes[boundary-1] != '\n' {
		return false
	}
	lineStart := boundary
	for lineStart < len(runes) && unicode.IsSpace(runes[lineStart]) && runes[lineStart] != '\n' {
		lineStart++
	}
	if lineStart >= len(runes) {
		return true
	}
	if runes[lineStart] == '-' || runes[lineStart] == '*' || runes[lineStart] == '+' {
		return true
	}
	if unicode.IsDigit(runes[lineStart]) {
		for index := lineStart + 1; index < len(runes) && index < lineStart+5; index++ {
			if runes[index] == '.' || runes[index] == ')' {
				return true
			}
			if !unicode.IsDigit(runes[index]) {
				break
			}
		}
	}
	return false
}

func atomBoundaries(runes []rune, maxRunes int) []int {
	boundaries := make([]int, 0, len(runes)/32+1)
	inFence := false
	for index, value := range runes {
		if index+2 < len(runes) && string(runes[index:index+3]) == "```" {
			inFence = !inFence
		}
		if inFence {
			continue
		}
		if value == '\n' || value == '.' || value == '?' || value == '!' || value == ';' ||
			value == '。' || value == '？' || value == '！' || value == '；' {
			boundaries = append(boundaries, index+1)
		}
	}
	for forced := maxRunes; forced < len(runes); forced += maxRunes {
		boundaries = append(boundaries, forced)
	}
	boundaries = append(boundaries, len(runes))
	sort.Ints(boundaries)
	return uniqueInts(boundaries)
}

func trimRuneRange(runes []rune, start, end int) (int, int) {
	for start < end && unicode.IsSpace(runes[start]) {
		start++
	}
	for end > start && unicode.IsSpace(runes[end-1]) {
		end--
	}
	return start, end
}

func uniqueInts(values []int) []int {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func defaultEpistemicStatus(role, text string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case "tool", "system", "observation":
		return "observed"
	case "user":
		if strings.HasSuffix(strings.TrimSpace(text), "?") || strings.HasSuffix(strings.TrimSpace(text), "？") {
			return "questioned"
		}
		return "reported"
	case "assistant":
		if strings.HasSuffix(strings.TrimSpace(text), "?") || strings.HasSuffix(strings.TrimSpace(text), "？") {
			return "questioned"
		}
		return "suggested"
	default:
		return "derived"
	}
}

func normalizeEpistemicStatus(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "reported", "observed", "derived", "suggested", "hypothetical", "questioned":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func upsertEvidenceAtomTx(ctx context.Context, tx *sql.Tx, atom EvidenceAtom) error {
	if strings.TrimSpace(atom.AtomID) == "" || strings.TrimSpace(atom.Text) == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_evidence_atoms(atom_id, chunk_id, message_id,
		session_id, scope_type, scope_key, role, text, start_rune, end_rune, occurred_at, valid_from,
		valid_until, epistemic_status, content_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(atom_id) DO UPDATE SET text=excluded.text, role=excluded.role,
		epistemic_status=excluded.epistemic_status, content_hash=excluded.content_hash`, atom.AtomID, atom.ChunkID,
		atom.MessageID, atom.SessionID, atom.ScopeType, atom.ScopeKey, atom.Role, atom.Text, atom.StartRune,
		atom.EndRune, formatTime(atom.OccurredAt), formatTime(atom.ValidFrom), formatTime(atom.ValidUntil),
		atom.EpistemicStatus, atom.ContentHash)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_atom_fts WHERE atom_id=?`, atom.AtomID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_atom_fts(atom_id, session_id, message_id, role, text)
		VALUES (?, ?, ?, ?, ?)`, atom.AtomID, atom.SessionID, atom.MessageID, atom.Role, atom.Text)
	return err
}

func (s *Store) AppendEvidenceAtoms(ctx context.Context, atoms []EvidenceAtom) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, atom := range atoms {
		if err := upsertEvidenceAtomTx(ctx, tx, atom); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) BackfillEvidenceAtoms(ctx context.Context, limit, targetTokens, maxTokens int) (int, error) {
	query := `SELECT ` + chunkColumns + `, 0 FROM memory_evidence_chunks c
		WHERE NOT EXISTS (SELECT 1 FROM memory_evidence_atoms a WHERE a.chunk_id=c.chunk_id)
		ORDER BY c.occurred_at, c.chunk_id`
	var args []any
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	var chunks []EvidenceChunk
	for rows.Next() {
		chunk, _, scanErr := scanChunk(rows)
		if scanErr != nil {
			rows.Close()
			return 0, scanErr
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	count := 0
	for _, chunk := range chunks {
		for _, atom := range BuildEvidenceAtoms(chunk, targetTokens, maxTokens) {
			if err := upsertEvidenceAtomTx(ctx, tx, atom); err != nil {
				return count, err
			}
			if err := upsertEdgeTx(ctx, tx, normalizeEdge(Edge{ScopeType: atom.ScopeType, ScopeKey: atom.ScopeKey,
				FromID: chunk.ChunkID, ToID: atom.AtomID, Type: EdgeContains, Weight: 1, Confidence: 1})); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, tx.Commit()
}

const atomColumns = `a.atom_id, a.chunk_id, a.message_id, a.session_id, a.scope_type, a.scope_key,
	a.role, a.text, a.start_rune, a.end_rune, a.occurred_at, a.valid_from, a.valid_until,
	a.epistemic_status, a.content_hash`

const activeAtomParentClause = `EXISTS (SELECT 1 FROM memory_evidence_chunks parent_chunk
	WHERE parent_chunk.chunk_id=a.chunk_id AND parent_chunk.archived_at='')`

func scanAtom(scanner interface{ Scan(...any) error }) (EvidenceAtom, float64, error) {
	var atom EvidenceAtom
	var occurredAt, validFrom, validUntil string
	var score float64
	err := scanner.Scan(&atom.AtomID, &atom.ChunkID, &atom.MessageID, &atom.SessionID, &atom.ScopeType,
		&atom.ScopeKey, &atom.Role, &atom.Text, &atom.StartRune, &atom.EndRune, &occurredAt, &validFrom,
		&validUntil, &atom.EpistemicStatus, &atom.ContentHash, &score)
	atom.OccurredAt, atom.ValidFrom, atom.ValidUntil = parseTime(occurredAt), parseTime(validFrom), parseTime(validUntil)
	return atom, score, err
}

func atomEntry(atom EvidenceAtom, score float64, reason string) Entry {
	return Entry{MemoryID: atom.AtomID, ScopeType: atom.ScopeType, ScopeKey: atom.ScopeKey,
		MemoryType: TypeEpisodic, Title: "Message " + atom.MessageID, Content: atom.Text, Summary: atom.Text,
		SourceSessionID: atom.SessionID, SourceMessageIDs: []string{atom.MessageID}, CreatedAt: atom.OccurredAt,
		UpdatedAt: atom.OccurredAt, ValidFrom: atom.ValidFrom, ValidUntil: atom.ValidUntil, Status: StatusActive,
		Score: score, MatchReason: reason, DocumentKind: "atom", ParentID: atom.ChunkID, MessageID: atom.MessageID,
		Role: atom.Role, OccurredAt: atom.OccurredAt, EpistemicStatus: atom.EpistemicStatus}
}

func (s *Store) SearchAtomsKeyword(ctx context.Context, queries []string, scopes []Scope, sessionID string, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 40
	}
	best := map[string]Entry{}
	for _, query := range normalizeStrings(queries) {
		ftsQuery := sanitizeFTSQuery(query)
		if ftsQuery == "" {
			continue
		}
		clauses, args := []string{"memory_atom_fts MATCH ?", activeAtomParentClause}, []any{ftsQuery}
		if sessionID != "" {
			clauses = append(clauses, "a.session_id=?")
			args = append(args, sessionID)
		}
		if scopeSQL, scopeArgs := scopedClauses(scopes, "a."); scopeSQL != "" {
			clauses = append(clauses, "("+scopeSQL+")")
			args = append(args, scopeArgs...)
		}
		args = append(args, limit)
		rows, err := s.db.QueryContext(ctx, `SELECT `+atomColumns+`, bm25(memory_atom_fts)
			FROM memory_atom_fts JOIN memory_evidence_atoms a ON a.atom_id=memory_atom_fts.atom_id
			WHERE `+strings.Join(clauses, " AND ")+` ORDER BY bm25(memory_atom_fts) LIMIT ?`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			atom, rank, scanErr := scanAtom(rows)
			if scanErr != nil {
				rows.Close()
				return nil, scanErr
			}
			entry := atomEntry(atom, -rank, "atom_bm25")
			if prior, ok := best[entry.MemoryID]; !ok || entry.Score > prior.Score {
				best[entry.MemoryID] = entry
			}
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
	}
	return sortedEntryMap(best, limit), nil
}

func (s *Store) SearchAtomsSemantic(ctx context.Context, embedding []float32, model string, scopes []Scope, sessionID string, limit int) ([]Entry, error) {
	if len(embedding) == 0 || strings.TrimSpace(model) == "" {
		return nil, nil
	}
	blob, err := vector.EncodeEmbedding(embedding)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 40
	}
	clauses, args := []string{"e.model=?", "e.dimensions=?", activeAtomParentClause}, []any{model, len(embedding)}
	if sessionID != "" {
		clauses = append(clauses, "a.session_id=?")
		args = append(args, sessionID)
	}
	if scopeSQL, scopeArgs := scopedClauses(scopes, "a."); scopeSQL != "" {
		clauses = append(clauses, "("+scopeSQL+")")
		args = append(args, scopeArgs...)
	}
	args = append(args, blob, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT `+atomColumns+`, vec_cosine(e.embedding, ?)
		FROM memory_atom_embeddings e JOIN memory_evidence_atoms a ON a.atom_id=e.atom_id
		WHERE `+strings.Join(clauses, " AND ")+` ORDER BY vec_cosine(e.embedding, ?) DESC LIMIT ?`, reorderAtomVectorArgs(args)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		atom, score, scanErr := scanAtom(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		entries = append(entries, atomEntry(atom, score, "atom_vector"))
	}
	return entries, rows.Err()
}

func reorderAtomVectorArgs(args []any) []any {
	if len(args) < 2 {
		return args
	}
	blob := args[len(args)-2]
	limit := args[len(args)-1]
	base := append([]any(nil), args[:len(args)-2]...)
	return append([]any{blob}, append(base, blob, limit)...)
}

func (s *Store) SearchAtomsEntity(ctx context.Context, terms []string, scopes []Scope, sessionID string, limit int) ([]Entry, error) {
	terms = normalizeEntityTerms(terms)
	if len(terms) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 40
	}
	clauses, args := []string{activeAtomParentClause}, []any{}
	var matches []string
	for _, term := range terms {
		matches = append(matches, `(e.normalized_entity LIKE ? OR ? LIKE '%' || e.normalized_entity || '%')`)
		args = append(args, "%"+term+"%", term)
	}
	clauses = append(clauses, "("+strings.Join(matches, " OR ")+")")
	if sessionID != "" {
		clauses = append(clauses, "a.session_id=?")
		args = append(args, sessionID)
	}
	if scopeSQL, scopeArgs := scopedClauses(scopes, "a."); scopeSQL != "" {
		clauses = append(clauses, "("+scopeSQL+")")
		args = append(args, scopeArgs...)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT `+atomColumns+`, e.confidence
		FROM memory_atom_entities e JOIN memory_evidence_atoms a ON a.atom_id=e.atom_id
		WHERE `+strings.Join(clauses, " AND ")+` ORDER BY e.confidence DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		atom, score, scanErr := scanAtom(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		entries = append(entries, atomEntry(atom, score, "atom_entity"))
	}
	return entries, rows.Err()
}

func (s *Store) SearchAtomsTemporal(ctx context.Context, constraints []TemporalConstraint, scopes []Scope, sessionID string, limit int) ([]Entry, error) {
	if len(constraints) == 0 {
		return nil, nil
	}
	if limit <= 0 {
		limit = 40
	}
	clauses, args := []string{"a.occurred_at<>''", activeAtomParentClause}, []any{}
	if sessionID != "" {
		clauses = append(clauses, "a.session_id=?")
		args = append(args, sessionID)
	}
	if scopeSQL, scopeArgs := scopedClauses(scopes, "a."); scopeSQL != "" {
		clauses = append(clauses, "("+scopeSQL+")")
		args = append(args, scopeArgs...)
	}
	args = append(args, maxInt(limit*4, 64))
	rows, err := s.db.QueryContext(ctx, `SELECT `+atomColumns+`, 1 FROM memory_evidence_atoms a
		WHERE `+strings.Join(clauses, " AND ")+` ORDER BY a.occurred_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var entries []Entry
	for rows.Next() {
		atom, _, scanErr := scanAtom(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		entry := atomEntry(atom, 1, "atom_temporal")
		if temporalEntryAllowed(entry, constraints) {
			entries = append(entries, entry)
		}
	}
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries, rows.Err()
}

func sortedEntryMap(best map[string]Entry, limit int) []Entry {
	entries := make([]Entry, 0, len(best))
	for _, entry := range best {
		entries = append(entries, entry)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if math.Abs(entries[i].Score-entries[j].Score) < 1e-12 {
			return entries[i].MemoryID < entries[j].MemoryID
		}
		return entries[i].Score > entries[j].Score
	})
	if len(entries) > limit {
		entries = entries[:limit]
	}
	return entries
}

func (s *Store) GetAtoms(ctx context.Context, ids []string) ([]EvidenceAtom, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	marks := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for index := range ids {
		args[index] = ids[index]
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+atomColumns+`, 0 FROM memory_evidence_atoms a
		WHERE `+activeAtomParentClause+` AND a.atom_id IN (`+marks+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var atoms []EvidenceAtom
	for rows.Next() {
		atom, _, scanErr := scanAtom(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		atoms = append(atoms, atom)
	}
	return atoms, rows.Err()
}

func (s *Store) NeighborAtoms(ctx context.Context, atom EvidenceAtom, radius int) ([]EvidenceAtom, error) {
	if radius <= 0 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+atomColumns+`, 0 FROM memory_evidence_atoms a
		WHERE `+activeAtomParentClause+` AND a.scope_type=? AND a.scope_key=? AND a.session_id=? AND a.message_id=?
		ORDER BY a.start_rune, a.end_rune`, atom.ScopeType, atom.ScopeKey, atom.SessionID, atom.MessageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []EvidenceAtom
	selected := -1
	for rows.Next() {
		current, _, scanErr := scanAtom(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if current.AtomID == atom.AtomID {
			selected = len(all)
		}
		all = append(all, current)
	}
	if selected < 0 {
		return nil, rows.Err()
	}
	start, end := maxInt(0, selected-radius), minInt(len(all), selected+radius+1)
	return append([]EvidenceAtom(nil), all[start:end]...), rows.Err()
}

func (s *Store) AtomsMissingEmbedding(ctx context.Context, model string, limit int) ([]EvidenceAtom, error) {
	if limit <= 0 {
		limit = 32
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+atomColumns+`, 0 FROM memory_evidence_atoms a
		LEFT JOIN memory_atom_embeddings e ON e.atom_id=a.atom_id AND e.model=?
		WHERE `+activeAtomParentClause+` AND (e.atom_id IS NULL OR e.content_hash<>a.content_hash)
		ORDER BY a.occurred_at LIMIT ?`, model, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var atoms []EvidenceAtom
	for rows.Next() {
		atom, _, scanErr := scanAtom(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		atoms = append(atoms, atom)
	}
	return atoms, rows.Err()
}

func (s *Store) UpsertAtomEmbedding(ctx context.Context, atomID, model, contentHash string, embedding []float32) error {
	blob, err := vector.EncodeEmbedding(embedding)
	if err != nil {
		return err
	}
	now := formatTime(time.Now().UTC())
	_, err = s.db.ExecContext(ctx, `INSERT INTO memory_atom_embeddings(atom_id, model, dimensions, content_hash,
		embedding, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(atom_id, model) DO UPDATE SET dimensions=excluded.dimensions,
		content_hash=excluded.content_hash, embedding=excluded.embedding, updated_at=excluded.updated_at`, atomID,
		model, len(embedding), contentHash, blob, now, now)
	return err
}
