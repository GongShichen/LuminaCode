package longmemory

import (
	"context"
	"database/sql"
	"encoding/json"
	"math"
	"sort"
	"strconv"
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
	structures := atomStructureSpans(chunk, runes)
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
			if atomHardBoundary(runes, boundary) || atomSpeechActBoundary(runes, start, boundary) || boundary >= target {
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
			atomID := StableID(chunk.ScopeType, chunk.ScopeKey, "atom:"+chunk.MessageID,
				strings.Join([]string{chunk.MessageID, itoa(absoluteStart), itoa(absoluteEnd), hash}, "\x00"))
			atom := EvidenceAtom{AtomID: atomID, ChunkID: chunk.ChunkID,
				MessageID: chunk.MessageID, SessionID: chunk.SessionID, ScopeType: chunk.ScopeType,
				ScopeKey: chunk.ScopeKey, Role: chunk.Role, Text: atomText, StartRune: absoluteStart,
				EndRune: absoluteEnd, OccurredAt: chunk.OccurredAt, ValidFrom: chunk.ValidFrom,
				ValidUntil: chunk.ValidUntil, EpistemicStatus: defaultEpistemicStatus(chunk.Role, atomText),
				ContentHash: hash, SequenceNo: len(atoms)}
			applyAtomStructure(&atom, structures, segmentStart)
			atom.ContentHash = StableID(chunk.ScopeType, chunk.ScopeKey, "atom-search-content",
				strings.Join([]string{hash, atomStructurePrefix(atom)}, "\x00"))
			atoms = append(atoms, atom)
		}
		start = end
	}
	return atoms
}

// atomSpeechActBoundary keeps a declarative assertion from being merged with
// a following question (and vice versa). Epistemic status is assigned per
// atom, so mixing both speech acts would misclassify the entire span.
func atomSpeechActBoundary(runes []rune, start, boundary int) bool {
	if boundary <= start || boundary >= len(runes) {
		return false
	}
	currentQuestion := runes[boundary-1] == '?' || runes[boundary-1] == '？'
	nextEnd := boundary
	for nextEnd < len(runes) {
		value := runes[nextEnd]
		if value == '.' || value == '?' || value == '!' || value == '。' || value == '？' || value == '！' {
			nextQuestion := value == '?' || value == '？'
			return currentQuestion != nextQuestion
		}
		nextEnd++
	}
	return false
}

// BuildEvidenceAtomsForSpan cuts the original message once, then assigns each
// atom to the chunk that contains it. Atomizing overlapping chunks separately
// creates duplicate and mid-word evidence, so ingestion and backfill must use
// this entry point whenever the source span is available.
func BuildEvidenceAtomsForSpan(span EvidenceSpan, chunks []EvidenceChunk, targetTokens, maxTokens int) []EvidenceAtom {
	span = normalizeEvidenceSpan(span)
	if strings.TrimSpace(span.Text) == "" {
		return nil
	}
	ordered := append([]EvidenceChunk(nil), chunks...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].StartRune == ordered[j].StartRune {
			return ordered[i].EndRune < ordered[j].EndRune
		}
		return ordered[i].StartRune < ordered[j].StartRune
	})
	virtual := EvidenceChunk{ChunkID: "", SpanID: span.SpanID, ParentMemoryID: span.MemoryID,
		ScopeType: span.ScopeType, ScopeKey: span.ScopeKey, SessionID: span.SessionID,
		MessageID: span.MessageID, Role: span.Role, Text: span.Text, StartRune: span.StartRune,
		EndRune: span.EndRune, OccurredAt: span.OccurredAt, ValidFrom: span.OccurredAt}
	atoms := BuildEvidenceAtoms(virtual, targetTokens, maxTokens)
	for index := range atoms {
		for _, chunk := range ordered {
			if atoms[index].StartRune >= chunk.StartRune && atoms[index].EndRune <= chunk.EndRune {
				atoms[index].ChunkID = chunk.ChunkID
				break
			}
		}
		if atoms[index].ChunkID == "" {
			for _, chunk := range ordered {
				if atoms[index].StartRune >= chunk.StartRune && atoms[index].StartRune < chunk.EndRune {
					atoms[index].ChunkID = chunk.ChunkID
					break
				}
			}
		}
	}
	return atoms
}

type atomStructureSpan struct {
	start, end        int
	containerID, kind string
	parentID          string
	ordinal           int
	headingPath       []string
}

func atomStructureSpans(chunk EvidenceChunk, runes []rune) []atomStructureSpan {
	var result []atomStructureSpan
	headings := []string{}
	lineStart, listStart, tableStart := 0, -1, -1
	inCode, codeStart := false, -1
	listOrdinal, tableOrdinal := 0, 0
	for lineStart < len(runes) {
		lineEnd := lineStart
		for lineEnd < len(runes) && runes[lineEnd] != '\n' {
			lineEnd++
		}
		next := lineEnd
		if next < len(runes) {
			next++
		}
		line := strings.TrimSpace(string(runes[lineStart:lineEnd]))
		if strings.HasPrefix(line, "```") {
			if !inCode {
				inCode, codeStart = true, lineStart
			} else {
				result = append(result, structuralSpan(chunk, codeStart, next, "code_block", 0, "", headings))
				inCode, codeStart = false, -1
			}
			lineStart = next
			continue
		}
		if inCode {
			lineStart = next
			continue
		}
		if level, title := headingLine(line); level > 0 {
			if level <= len(headings) {
				headings = headings[:level-1]
			}
			for len(headings) < level-1 {
				headings = append(headings, "")
			}
			headings = append(headings, title)
			result = append(result, structuralSpan(chunk, lineStart, next, "heading", level, "", headings))
			listStart, tableStart, listOrdinal, tableOrdinal = -1, -1, 0, 0
			lineStart = next
			continue
		}
		if ordinal, ok := orderedListOrdinal(line); ok {
			if listStart < 0 {
				listStart, listOrdinal = lineStart, 0
			}
			listOrdinal++
			if ordinal == 0 {
				ordinal = listOrdinal
			}
			listID := StableID(chunk.ScopeType, chunk.ScopeKey, "atom-list",
				strings.Join([]string{chunk.MessageID, itoa(chunk.StartRune + listStart)}, "\x00"))
			result = append(result, structuralSpan(chunk, lineStart, next, "list_item", ordinal, listID, headings))
			tableStart, tableOrdinal = -1, 0
		} else if isUnorderedListLine(line) {
			if listStart < 0 {
				listStart, listOrdinal = lineStart, 0
			}
			listOrdinal++
			listID := StableID(chunk.ScopeType, chunk.ScopeKey, "atom-list",
				strings.Join([]string{chunk.MessageID, itoa(chunk.StartRune + listStart)}, "\x00"))
			result = append(result, structuralSpan(chunk, lineStart, next, "list_item", listOrdinal, listID, headings))
			tableStart, tableOrdinal = -1, 0
		} else if strings.Count(line, "|") >= 2 {
			if tableStart < 0 {
				tableStart, tableOrdinal = lineStart, 0
			}
			tableOrdinal++
			tableID := StableID(chunk.ScopeType, chunk.ScopeKey, "atom-table",
				strings.Join([]string{chunk.MessageID, itoa(chunk.StartRune + tableStart)}, "\x00"))
			result = append(result, structuralSpan(chunk, lineStart, next, "table_row", tableOrdinal, tableID, headings))
			listStart, listOrdinal = -1, 0
		} else {
			kind := "paragraph"
			if strings.HasPrefix(line, ">") {
				kind = "quote"
			}
			result = append(result, structuralSpan(chunk, lineStart, next, kind, 0, "", headings))
			listStart, tableStart, listOrdinal, tableOrdinal = -1, -1, 0, 0
		}
		lineStart = next
	}
	if inCode && codeStart >= 0 {
		result = append(result, structuralSpan(chunk, codeStart, len(runes), "code_block", 0, "", headings))
	}
	return result
}

func structuralSpan(chunk EvidenceChunk, start, end int, kind string, ordinal int, parent string, headings []string) atomStructureSpan {
	id := StableID(chunk.ScopeType, chunk.ScopeKey, "atom-container",
		strings.Join([]string{chunk.MessageID, kind, itoa(chunk.StartRune + start), itoa(ordinal)}, "\x00"))
	return atomStructureSpan{start: start, end: end, containerID: id, kind: kind,
		parentID: parent, ordinal: ordinal, headingPath: append([]string(nil), headings...)}
}

func applyAtomStructure(atom *EvidenceAtom, structures []atomStructureSpan, relativeStart int) {
	for _, structure := range structures {
		if relativeStart < structure.start || relativeStart >= structure.end {
			continue
		}
		atom.ContainerID, atom.ContainerKind = structure.containerID, structure.kind
		atom.ContainerOrdinal, atom.ParentContainerID = structure.ordinal, structure.parentID
		atom.HeadingPath = append([]string(nil), structure.headingPath...)
		return
	}
}

func headingLine(line string) (int, string) {
	level := 0
	for level < len(line) && line[level] == '#' {
		level++
	}
	if level == 0 || level > 6 || level >= len(line) || line[level] != ' ' {
		return 0, ""
	}
	return level, strings.TrimSpace(line[level:])
}

func orderedListOrdinal(line string) (int, bool) {
	index := 0
	for index < len(line) && line[index] >= '0' && line[index] <= '9' {
		index++
	}
	if index == 0 || index >= len(line) || (line[index] != '.' && line[index] != ')') {
		return 0, false
	}
	value, err := strconv.Atoi(line[:index])
	return value, err == nil
}

func isUnorderedListLine(line string) bool {
	return len(line) >= 2 && (line[0] == '-' || line[0] == '*' || line[0] == '+') && unicode.IsSpace(rune(line[1]))
}

func atomStructurePrefix(atom EvidenceAtom) string {
	var parts []string
	if len(atom.HeadingPath) > 0 {
		parts = append(parts, "Under "+strings.Join(atom.HeadingPath, " / "))
	}
	if atom.ContainerKind == "list_item" && atom.ContainerOrdinal > 0 {
		parts = append(parts, "List item "+itoa(atom.ContainerOrdinal))
	} else if atom.ContainerKind != "" && atom.ContainerKind != "paragraph" {
		parts = append(parts, strings.ReplaceAll(atom.ContainerKind, "_", " "))
	}
	return strings.Join(parts, ". ")
}

func atomSearchText(atom EvidenceAtom) string {
	prefix := atomStructurePrefix(atom)
	if prefix == "" {
		return atom.Text
	}
	return prefix + ". " + atom.Text
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
		if value == '.' && isListOrdinalPeriod(runes, index) {
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

func isListOrdinalPeriod(runes []rune, index int) bool {
	if index <= 0 || index+1 >= len(runes) || !unicode.IsDigit(runes[index-1]) || !unicode.IsSpace(runes[index+1]) {
		return false
	}
	start := index - 1
	for start > 0 && unicode.IsDigit(runes[start-1]) {
		start--
	}
	return start == 0 || runes[start-1] == '\n'
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
	var existingHash string
	if err := tx.QueryRowContext(ctx, `SELECT content_hash FROM memory_evidence_atoms WHERE atom_id=?`, atom.AtomID).Scan(&existingHash); err == nil && existingHash == atom.ContentHash {
		return nil
	} else if err != nil && err != sql.ErrNoRows {
		return err
	}
	headingPath, _ := json.Marshal(atom.HeadingPath)
	_, err := tx.ExecContext(ctx, `INSERT INTO memory_evidence_atoms(atom_id, chunk_id, message_id,
		session_id, scope_type, scope_key, role, text, start_rune, end_rune, sequence_no, container_id,
		container_kind, container_ordinal, parent_container_id, heading_path_json, occurred_at, valid_from,
		valid_until, epistemic_status, content_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(atom_id) DO UPDATE SET text=excluded.text, role=excluded.role,
		epistemic_status=excluded.epistemic_status, content_hash=excluded.content_hash,
		sequence_no=excluded.sequence_no, container_id=excluded.container_id, container_kind=excluded.container_kind,
		container_ordinal=excluded.container_ordinal, parent_container_id=excluded.parent_container_id,
		heading_path_json=excluded.heading_path_json`, atom.AtomID, atom.ChunkID,
		atom.MessageID, atom.SessionID, atom.ScopeType, atom.ScopeKey, atom.Role, atom.Text, atom.StartRune,
		atom.EndRune, atom.SequenceNo, atom.ContainerID, atom.ContainerKind, atom.ContainerOrdinal,
		atom.ParentContainerID, string(headingPath), formatTime(atom.OccurredAt), formatTime(atom.ValidFrom), formatTime(atom.ValidUntil),
		atom.EpistemicStatus, atom.ContentHash)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM memory_atom_fts WHERE atom_id=?`, atom.AtomID); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO memory_atom_fts(atom_id, session_id, message_id, role, text)
		VALUES (?, ?, ?, ?, ?)`, atom.AtomID, atom.SessionID, atom.MessageID, atom.Role, atomSearchText(atom))
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
	query := `SELECT s.span_id, s.memory_id, s.scope_type, s.scope_key, s.session_id, s.message_id,
		s.role, s.source_path, s.text, s.start_rune, s.end_rune, s.occurred_at, s.content_hash
		FROM memory_evidence_spans s WHERE NOT EXISTS (
			SELECT 1 FROM memory_evidence_atoms a WHERE a.message_id=s.message_id
			AND a.scope_type=s.scope_type AND a.scope_key=s.scope_key)
		ORDER BY s.occurred_at, s.span_id`
	var args []any
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	var spans []EvidenceSpan
	for rows.Next() {
		var span EvidenceSpan
		var occurredAt string
		if scanErr := rows.Scan(&span.SpanID, &span.MemoryID, &span.ScopeType, &span.ScopeKey,
			&span.SessionID, &span.MessageID, &span.Role, &span.SourcePath, &span.Text, &span.StartRune,
			&span.EndRune, &occurredAt, &span.ContentHash); scanErr != nil {
			rows.Close()
			return 0, scanErr
		}
		span.OccurredAt = parseTime(occurredAt)
		spans = append(spans, span)
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
	for _, span := range spans {
		chunks := BuildEvidenceChunks(span)
		for _, atom := range BuildEvidenceAtomsForSpan(span, chunks, targetTokens, maxTokens) {
			if err := upsertEvidenceAtomTx(ctx, tx, atom); err != nil {
				return count, err
			}
			if err := upsertEdgeTx(ctx, tx, normalizeEdge(Edge{ScopeType: atom.ScopeType, ScopeKey: atom.ScopeKey,
				FromID: atom.ChunkID, ToID: atom.AtomID, Type: EdgeContains, Weight: 1, Confidence: 1})); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, tx.Commit()
}

func (s *Store) BackfillAtomStructure(ctx context.Context, limit, targetTokens, maxTokens int) (int, error) {
	query := `SELECT ` + chunkColumns + `, 0 FROM memory_evidence_chunks c
		WHERE EXISTS (SELECT 1 FROM memory_evidence_atoms a WHERE a.chunk_id=c.chunk_id AND a.container_id='')
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
			count++
		}
	}
	return count, tx.Commit()
}

// RepairOverlappingEvidenceAtoms rebuilds only messages whose derived atoms
// overlap. Raw spans and chunks remain untouched; the atom layer is a
// reproducible index and can be repaired safely and idempotently.
func (s *Store) RepairOverlappingEvidenceAtoms(ctx context.Context, limit, targetTokens, maxTokens int) (int, error) {
	query := `SELECT DISTINCT s.span_id, s.memory_id, s.scope_type, s.scope_key, s.session_id, s.message_id,
		s.role, s.source_path, s.text, s.start_rune, s.end_rune, s.occurred_at, s.content_hash
		FROM memory_evidence_spans s WHERE EXISTS (
			SELECT 1 FROM memory_evidence_atoms a1 JOIN memory_evidence_atoms a2
			ON a1.scope_type=a2.scope_type AND a1.scope_key=a2.scope_key AND a1.message_id=a2.message_id
			AND a1.atom_id<a2.atom_id AND a1.start_rune<a2.end_rune AND a2.start_rune<a1.end_rune
			WHERE a1.scope_type=s.scope_type AND a1.scope_key=s.scope_key AND a1.message_id=s.message_id)
		ORDER BY s.occurred_at, s.span_id`
	var args []any
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	var spans []EvidenceSpan
	for rows.Next() {
		var span EvidenceSpan
		var occurredAt string
		if err := rows.Scan(&span.SpanID, &span.MemoryID, &span.ScopeType, &span.ScopeKey, &span.SessionID,
			&span.MessageID, &span.Role, &span.SourcePath, &span.Text, &span.StartRune, &span.EndRune,
			&occurredAt, &span.ContentHash); err != nil {
			rows.Close()
			return 0, err
		}
		span.OccurredAt = parseTime(occurredAt)
		spans = append(spans, span)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(spans) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	count := 0
	for _, span := range spans {
		var oldIDs []string
		atomRows, queryErr := tx.QueryContext(ctx, `SELECT atom_id FROM memory_evidence_atoms
			WHERE scope_type=? AND scope_key=? AND message_id=?`, span.ScopeType, span.ScopeKey, span.MessageID)
		if queryErr != nil {
			return count, queryErr
		}
		for atomRows.Next() {
			var atomID string
			if err := atomRows.Scan(&atomID); err != nil {
				atomRows.Close()
				return count, err
			}
			oldIDs = append(oldIDs, atomID)
		}
		if err := atomRows.Close(); err != nil {
			return count, err
		}
		for _, atomID := range oldIDs {
			for _, statement := range []string{
				`DELETE FROM memory_atom_embeddings WHERE atom_id=?`,
				`DELETE FROM memory_atom_entities WHERE atom_id=?`,
				`DELETE FROM memory_atom_fts WHERE atom_id=?`,
				`DELETE FROM memory_edges WHERE from_id=? OR to_id=?`,
				`DELETE FROM memory_evidence_atoms WHERE atom_id=?`,
			} {
				statementArgs := []any{atomID}
				if strings.Contains(statement, " OR ") {
					statementArgs = append(statementArgs, atomID)
				}
				if _, err := tx.ExecContext(ctx, statement, statementArgs...); err != nil {
					return count, err
				}
			}
		}
		chunks := BuildEvidenceChunks(span)
		for _, atom := range BuildEvidenceAtomsForSpan(span, chunks, targetTokens, maxTokens) {
			if err := upsertEvidenceAtomTx(ctx, tx, atom); err != nil {
				return count, err
			}
			if err := upsertEdgeTx(ctx, tx, normalizeEdge(Edge{ScopeType: atom.ScopeType, ScopeKey: atom.ScopeKey,
				FromID: atom.ChunkID, ToID: atom.AtomID, Type: EdgeContains, Weight: 1, Confidence: 1})); err != nil {
				return count, err
			}
			count++
		}
	}
	return count, tx.Commit()
}

// RepairMixedSpeechActAtoms rebuilds derived atoms that combine declarative
// statements with a trailing question. Source spans and chunks are immutable.
func (s *Store) RepairMixedSpeechActAtoms(ctx context.Context, limit, targetTokens, maxTokens int) (int, error) {
	query := `SELECT DISTINCT s.span_id, s.memory_id, s.scope_type, s.scope_key, s.session_id, s.message_id,
		s.role, s.source_path, s.text, s.start_rune, s.end_rune, s.occurred_at, s.content_hash
		FROM memory_evidence_spans s WHERE EXISTS (
			SELECT 1 FROM memory_evidence_atoms a WHERE a.scope_type=s.scope_type AND a.scope_key=s.scope_key
			AND a.message_id=s.message_id AND a.epistemic_status='questioned'
			AND (instr(a.text, '.')>0 OR instr(a.text, '。')>0 OR instr(a.text, '!')>0 OR instr(a.text, '！')>0))
		ORDER BY s.occurred_at, s.span_id`
	var args []any
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	var spans []EvidenceSpan
	for rows.Next() {
		var span EvidenceSpan
		var occurredAt string
		if err := rows.Scan(&span.SpanID, &span.MemoryID, &span.ScopeType, &span.ScopeKey, &span.SessionID,
			&span.MessageID, &span.Role, &span.SourcePath, &span.Text, &span.StartRune, &span.EndRune,
			&occurredAt, &span.ContentHash); err != nil {
			rows.Close()
			return 0, err
		}
		span.OccurredAt = parseTime(occurredAt)
		spans = append(spans, span)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(spans) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	count := 0
	for _, span := range spans {
		rebuilt, rebuildErr := replaceSpanAtomsTx(ctx, tx, span, targetTokens, maxTokens)
		if rebuildErr != nil {
			return count, rebuildErr
		}
		count += rebuilt
	}
	return count, tx.Commit()
}

func replaceSpanAtomsTx(ctx context.Context, tx *sql.Tx, span EvidenceSpan, targetTokens, maxTokens int) (int, error) {
	rows, err := tx.QueryContext(ctx, `SELECT atom_id FROM memory_evidence_atoms
		WHERE scope_type=? AND scope_key=? AND message_id=?`, span.ScopeType, span.ScopeKey, span.MessageID)
	if err != nil {
		return 0, err
	}
	var oldIDs []string
	for rows.Next() {
		var atomID string
		if err := rows.Scan(&atomID); err != nil {
			rows.Close()
			return 0, err
		}
		oldIDs = append(oldIDs, atomID)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, atomID := range oldIDs {
		statements := []string{
			`DELETE FROM memory_atom_embeddings WHERE atom_id=?`,
			`DELETE FROM memory_atom_entities WHERE atom_id=?`,
			`DELETE FROM memory_atom_fts WHERE atom_id=?`,
			`DELETE FROM memory_edges WHERE from_id=? OR to_id=?`,
			`DELETE FROM memory_evidence_atoms WHERE atom_id=?`,
		}
		for _, statement := range statements {
			statementArgs := []any{atomID}
			if strings.Contains(statement, " OR ") {
				statementArgs = append(statementArgs, atomID)
			}
			if _, err := tx.ExecContext(ctx, statement, statementArgs...); err != nil {
				return 0, err
			}
		}
	}
	count := 0
	chunks := BuildEvidenceChunks(span)
	for _, atom := range BuildEvidenceAtomsForSpan(span, chunks, targetTokens, maxTokens) {
		if err := upsertEvidenceAtomTx(ctx, tx, atom); err != nil {
			return count, err
		}
		if err := upsertEdgeTx(ctx, tx, normalizeEdge(Edge{ScopeType: atom.ScopeType, ScopeKey: atom.ScopeKey,
			FromID: atom.ChunkID, ToID: atom.AtomID, Type: EdgeContains, Weight: 1, Confidence: 1})); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

const atomColumns = `a.atom_id, a.chunk_id, a.message_id, a.session_id, a.scope_type, a.scope_key,
	a.role, a.text, a.start_rune, a.end_rune, a.sequence_no, a.container_id, a.container_kind,
	a.container_ordinal, a.parent_container_id, a.heading_path_json, a.occurred_at, a.valid_from, a.valid_until,
	a.epistemic_status, a.content_hash`

const activeAtomParentClause = `EXISTS (SELECT 1 FROM memory_evidence_chunks parent_chunk
	WHERE parent_chunk.chunk_id=a.chunk_id AND parent_chunk.archived_at='')`

func scanAtom(scanner interface{ Scan(...any) error }) (EvidenceAtom, float64, error) {
	var atom EvidenceAtom
	var occurredAt, validFrom, validUntil, headingPath string
	var score float64
	err := scanner.Scan(&atom.AtomID, &atom.ChunkID, &atom.MessageID, &atom.SessionID, &atom.ScopeType,
		&atom.ScopeKey, &atom.Role, &atom.Text, &atom.StartRune, &atom.EndRune, &atom.SequenceNo,
		&atom.ContainerID, &atom.ContainerKind, &atom.ContainerOrdinal, &atom.ParentContainerID, &headingPath,
		&occurredAt, &validFrom,
		&validUntil, &atom.EpistemicStatus, &atom.ContentHash, &score)
	_ = json.Unmarshal([]byte(headingPath), &atom.HeadingPath)
	atom.OccurredAt, atom.ValidFrom, atom.ValidUntil = parseTime(occurredAt), parseTime(validFrom), parseTime(validUntil)
	return atom, score, err
}

func atomEntry(atom EvidenceAtom, score float64, reason string) Entry {
	title := "Message " + atom.MessageID
	if prefix := atomStructurePrefix(atom); prefix != "" {
		title += " (" + prefix + ")"
	}
	return Entry{MemoryID: atom.AtomID, ScopeType: atom.ScopeType, ScopeKey: atom.ScopeKey,
		MemoryType: TypeEpisodic, Title: title, Content: atom.Text, Summary: atom.Text,
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
	constraints = effectiveTemporalConstraints(constraints)
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

func effectiveTemporalConstraints(constraints []TemporalConstraint) []TemporalConstraint {
	result := make([]TemporalConstraint, 0, len(constraints))
	for _, constraint := range constraints {
		if !constraint.From.IsZero() || !constraint.To.IsZero() || !constraint.At.IsZero() {
			result = append(result, constraint)
		}
	}
	return result
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
