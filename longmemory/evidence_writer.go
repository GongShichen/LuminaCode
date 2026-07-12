package longmemory

import (
	"context"
	"database/sql"
	"encoding/json"
)

// evidenceTxWriter prepares the high-volume evidence statements once per
// extraction transaction. It also avoids rebuilding FTS rows for unchanged
// evidence, which keeps replay and resume costs proportional to new content.
type evidenceTxWriter struct {
	chunkUpsert *sql.Stmt
	chunkFTSDel *sql.Stmt
	chunkFTSIns *sql.Stmt
	atomUpsert  *sql.Stmt
	atomFTSDel  *sql.Stmt
	atomFTSIns  *sql.Stmt
	edgeUpsert  *sql.Stmt
}

func newEvidenceTxWriter(ctx context.Context, tx *sql.Tx) (*evidenceTxWriter, error) {
	w := &evidenceTxWriter{}
	statements := []struct {
		dst **sql.Stmt
		sql string
	}{
		{&w.chunkUpsert, `INSERT INTO memory_evidence_chunks(chunk_id, span_id, parent_memory_id,
			scope_type, scope_key, session_id, message_id, role, text, start_rune, end_rune, occurred_at,
			valid_from, valid_until, content_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(chunk_id) DO UPDATE SET span_id=excluded.span_id,
			parent_memory_id=excluded.parent_memory_id, scope_type=excluded.scope_type,
			scope_key=excluded.scope_key, session_id=excluded.session_id, message_id=excluded.message_id,
			role=excluded.role, text=excluded.text, start_rune=excluded.start_rune,
			end_rune=excluded.end_rune, occurred_at=excluded.occurred_at,
			valid_from=excluded.valid_from, valid_until=excluded.valid_until,
			content_hash=excluded.content_hash
			WHERE memory_evidence_chunks.span_id IS NOT excluded.span_id
				OR memory_evidence_chunks.parent_memory_id IS NOT excluded.parent_memory_id
				OR memory_evidence_chunks.scope_type IS NOT excluded.scope_type
				OR memory_evidence_chunks.scope_key IS NOT excluded.scope_key
				OR memory_evidence_chunks.session_id IS NOT excluded.session_id
				OR memory_evidence_chunks.message_id IS NOT excluded.message_id
				OR memory_evidence_chunks.role IS NOT excluded.role
				OR memory_evidence_chunks.text IS NOT excluded.text
				OR memory_evidence_chunks.start_rune IS NOT excluded.start_rune
				OR memory_evidence_chunks.end_rune IS NOT excluded.end_rune
				OR memory_evidence_chunks.occurred_at IS NOT excluded.occurred_at
				OR memory_evidence_chunks.valid_from IS NOT excluded.valid_from
				OR memory_evidence_chunks.valid_until IS NOT excluded.valid_until
				OR memory_evidence_chunks.content_hash IS NOT excluded.content_hash`},
		{&w.chunkFTSDel, `DELETE FROM memory_chunk_fts WHERE chunk_id=?`},
		{&w.chunkFTSIns, `INSERT INTO memory_chunk_fts(chunk_id, session_id, message_id, role, text) VALUES (?, ?, ?, ?, ?)`},
		{&w.atomUpsert, `INSERT INTO memory_evidence_atoms(atom_id, chunk_id, message_id,
			session_id, scope_type, scope_key, role, text, start_rune, end_rune, sequence_no, container_id,
			container_kind, container_ordinal, parent_container_id, heading_path_json, occurred_at, valid_from,
			valid_until, epistemic_status, content_hash) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(atom_id) DO UPDATE SET chunk_id=excluded.chunk_id,
			message_id=excluded.message_id, session_id=excluded.session_id,
			scope_type=excluded.scope_type, scope_key=excluded.scope_key, role=excluded.role,
			text=excluded.text, start_rune=excluded.start_rune, end_rune=excluded.end_rune,
			sequence_no=excluded.sequence_no, container_id=excluded.container_id,
			container_kind=excluded.container_kind, container_ordinal=excluded.container_ordinal,
			parent_container_id=excluded.parent_container_id, heading_path_json=excluded.heading_path_json,
			occurred_at=excluded.occurred_at, valid_from=excluded.valid_from,
			valid_until=excluded.valid_until, epistemic_status=excluded.epistemic_status,
			content_hash=excluded.content_hash
			WHERE memory_evidence_atoms.chunk_id IS NOT excluded.chunk_id
				OR memory_evidence_atoms.message_id IS NOT excluded.message_id
				OR memory_evidence_atoms.session_id IS NOT excluded.session_id
				OR memory_evidence_atoms.scope_type IS NOT excluded.scope_type
				OR memory_evidence_atoms.scope_key IS NOT excluded.scope_key
				OR memory_evidence_atoms.role IS NOT excluded.role
				OR memory_evidence_atoms.text IS NOT excluded.text
				OR memory_evidence_atoms.start_rune IS NOT excluded.start_rune
				OR memory_evidence_atoms.end_rune IS NOT excluded.end_rune
				OR memory_evidence_atoms.sequence_no IS NOT excluded.sequence_no
				OR memory_evidence_atoms.container_id IS NOT excluded.container_id
				OR memory_evidence_atoms.container_kind IS NOT excluded.container_kind
				OR memory_evidence_atoms.container_ordinal IS NOT excluded.container_ordinal
				OR memory_evidence_atoms.parent_container_id IS NOT excluded.parent_container_id
				OR memory_evidence_atoms.heading_path_json IS NOT excluded.heading_path_json
				OR memory_evidence_atoms.occurred_at IS NOT excluded.occurred_at
				OR memory_evidence_atoms.valid_from IS NOT excluded.valid_from
				OR memory_evidence_atoms.valid_until IS NOT excluded.valid_until
				OR memory_evidence_atoms.epistemic_status IS NOT excluded.epistemic_status
				OR memory_evidence_atoms.content_hash IS NOT excluded.content_hash`},
		{&w.atomFTSDel, `DELETE FROM memory_atom_fts WHERE atom_id=?`},
		{&w.atomFTSIns, `INSERT INTO memory_atom_fts(atom_id, session_id, message_id, role, text) VALUES (?, ?, ?, ?, ?)`},
		{&w.edgeUpsert, `INSERT INTO memory_edges(edge_id, scope_type, scope_key, from_id, to_id,
			edge_type, weight, confidence, created_at, valid_until) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(edge_id) DO UPDATE SET weight=excluded.weight, confidence=excluded.confidence,
			valid_until=excluded.valid_until`},
	}
	for _, item := range statements {
		stmt, err := tx.PrepareContext(ctx, item.sql)
		if err != nil {
			w.close()
			return nil, err
		}
		*item.dst = stmt
	}
	return w, nil
}

func (w *evidenceTxWriter) close() {
	if w == nil {
		return
	}
	for _, stmt := range []*sql.Stmt{w.chunkUpsert, w.chunkFTSDel, w.chunkFTSIns, w.atomUpsert, w.atomFTSDel, w.atomFTSIns, w.edgeUpsert} {
		if stmt != nil {
			_ = stmt.Close()
		}
	}
}

func (w *evidenceTxWriter) upsertChunk(ctx context.Context, chunk EvidenceChunk) error {
	if chunk.ChunkID == "" || chunk.Text == "" {
		return nil
	}
	result, err := w.chunkUpsert.ExecContext(ctx, chunk.ChunkID, chunk.SpanID, chunk.ParentMemoryID,
		chunk.ScopeType, chunk.ScopeKey, chunk.SessionID, chunk.MessageID, chunk.Role, chunk.Text,
		chunk.StartRune, chunk.EndRune, formatTime(chunk.OccurredAt), formatTime(chunk.ValidFrom),
		formatTime(chunk.ValidUntil), chunk.ContentHash)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	if _, err = w.chunkFTSDel.ExecContext(ctx, chunk.ChunkID); err != nil {
		return err
	}
	_, err = w.chunkFTSIns.ExecContext(ctx, chunk.ChunkID, chunk.SessionID, chunk.MessageID, chunk.Role, chunk.Text)
	return err
}

func (w *evidenceTxWriter) upsertAtom(ctx context.Context, atom EvidenceAtom) error {
	if atom.AtomID == "" || atom.Text == "" {
		return nil
	}
	headingPath, _ := json.Marshal(atom.HeadingPath)
	result, err := w.atomUpsert.ExecContext(ctx, atom.AtomID, atom.ChunkID, atom.MessageID, atom.SessionID,
		atom.ScopeType, atom.ScopeKey, atom.Role, atom.Text, atom.StartRune, atom.EndRune, atom.SequenceNo,
		atom.ContainerID, atom.ContainerKind, atom.ContainerOrdinal, atom.ParentContainerID, string(headingPath),
		formatTime(atom.OccurredAt), formatTime(atom.ValidFrom), formatTime(atom.ValidUntil),
		atom.EpistemicStatus, atom.ContentHash)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	if _, err = w.atomFTSDel.ExecContext(ctx, atom.AtomID); err != nil {
		return err
	}
	_, err = w.atomFTSIns.ExecContext(ctx, atom.AtomID, atom.SessionID, atom.MessageID, atom.Role, atomSearchText(atom))
	return err
}

func (w *evidenceTxWriter) upsertEdge(ctx context.Context, edge Edge) error {
	_, err := w.edgeUpsert.ExecContext(ctx, edge.EdgeID, edge.ScopeType, edge.ScopeKey, edge.FromID, edge.ToID,
		edge.Type, edge.Weight, edge.Confidence, formatTime(edge.CreatedAt), formatTime(edge.ValidUntil))
	return err
}
