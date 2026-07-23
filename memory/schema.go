package memory

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
)

const fabricSchemaRevision = "ledger-semantic-artifact-chunk-index"

func (f *Fabric) migrate(ctx context.Context) error {
	ledgerStatements := []string{
		`PRAGMA journal_mode=WAL;`,
		`PRAGMA foreign_keys=ON;`,
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
		`CREATE TABLE IF NOT EXISTS contexts (
			context_id TEXT PRIMARY KEY, space TEXT NOT NULL, parent_id TEXT NOT NULL DEFAULT '',
			context_type TEXT NOT NULL DEFAULT '', label TEXT NOT NULL DEFAULT '',
			opened_at TEXT NOT NULL DEFAULT '', closed_at TEXT NOT NULL DEFAULT ''
		);`,
		`CREATE INDEX IF NOT EXISTS idx_contexts_space_parent ON contexts(space, parent_id);`,
		`CREATE TABLE IF NOT EXISTS events (
			event_id TEXT PRIMARY KEY, space TEXT NOT NULL, context_id TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '', actor TEXT NOT NULL, source_kind TEXT NOT NULL DEFAULT '',
			content TEXT NOT NULL, occurred_at TEXT NOT NULL, source_ref TEXT NOT NULL DEFAULT '',
			checksum TEXT NOT NULL, metadata_json TEXT NOT NULL DEFAULT '{}', semantic_status TEXT NOT NULL DEFAULT 'event_durable',
			token_estimate INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, tombstoned INTEGER NOT NULL DEFAULT 0
		);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_events_space_checksum ON events(space, checksum);`,
		`CREATE INDEX IF NOT EXISTS idx_events_context_semantic ON events(space, context_id, semantic_status, occurred_at);`,
		`CREATE TABLE IF NOT EXISTS identities (
			identity_id TEXT PRIMARY KEY, space TEXT NOT NULL, canonical TEXT NOT NULL, identity_type TEXT NOT NULL DEFAULT '',
			display_name TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'active', created_at TEXT NOT NULL
		);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_identities_canonical ON identities(space, identity_type, canonical);`,
		`CREATE TABLE IF NOT EXISTS identity_aliases (
			space TEXT NOT NULL, normalized_alias TEXT NOT NULL, identity_id TEXT NOT NULL,
			source_event_id TEXT NOT NULL DEFAULT '', method TEXT NOT NULL DEFAULT '', status TEXT NOT NULL DEFAULT 'active',
			created_at TEXT NOT NULL, PRIMARY KEY(space, normalized_alias, identity_id),
			FOREIGN KEY(identity_id) REFERENCES identities(identity_id) ON DELETE CASCADE
		);`,
		`CREATE INDEX IF NOT EXISTS idx_identity_alias_lookup ON identity_aliases(space, normalized_alias, status);`,
		`CREATE TABLE IF NOT EXISTS slots (
			slot_id TEXT PRIMARY KEY, space TEXT NOT NULL, subject_identity_id TEXT NOT NULL DEFAULT '', facet TEXT NOT NULL,
			attribute_key TEXT NOT NULL, scope_key TEXT NOT NULL, created_at TEXT NOT NULL,
			UNIQUE(space, subject_identity_id, facet, attribute_key, scope_key)
		);`,
		`CREATE TABLE IF NOT EXISTS memory_nodes (
			node_id TEXT PRIMARY KEY, space TEXT NOT NULL, context_id TEXT NOT NULL DEFAULT '', node_kind TEXT NOT NULL,
			claim_type TEXT NOT NULL DEFAULT '', statement TEXT NOT NULL, subject_identity_id TEXT NOT NULL DEFAULT '',
			subject_text TEXT NOT NULL DEFAULT '', facet TEXT NOT NULL DEFAULT '', attribute_key TEXT NOT NULL DEFAULT '',
			scope_key TEXT NOT NULL DEFAULT '', slot_id TEXT NOT NULL DEFAULT '', evidence_mode TEXT NOT NULL DEFAULT '',
			valid_from TEXT NOT NULL DEFAULT '', valid_until TEXT NOT NULL DEFAULT '', status TEXT NOT NULL,
			payload_json TEXT NOT NULL DEFAULT '{}', content_hash TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			tombstoned INTEGER NOT NULL DEFAULT 0
		);`,
		`CREATE INDEX IF NOT EXISTS idx_nodes_space_status ON memory_nodes(space, status, node_kind);`,
		`CREATE INDEX IF NOT EXISTS idx_nodes_slot_status ON memory_nodes(slot_id, status, valid_from);`,
		`CREATE TABLE IF NOT EXISTS claim_values (
			node_id TEXT PRIMARY KEY, value_kind TEXT NOT NULL DEFAULT '', text_value TEXT NOT NULL DEFAULT '',
			number_value REAL, unit TEXT NOT NULL DEFAULT '', time_value TEXT NOT NULL DEFAULT '',
			list_json TEXT NOT NULL DEFAULT '[]', bool_value INTEGER,
			FOREIGN KEY(node_id) REFERENCES memory_nodes(node_id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS node_sources (
			node_id TEXT NOT NULL, event_id TEXT NOT NULL, start_rune INTEGER NOT NULL, end_rune INTEGER NOT NULL,
			source_role TEXT NOT NULL DEFAULT '', PRIMARY KEY(node_id, event_id, start_rune, end_rune),
			FOREIGN KEY(node_id) REFERENCES memory_nodes(node_id) ON DELETE CASCADE,
			FOREIGN KEY(event_id) REFERENCES events(event_id) ON DELETE CASCADE
		);`,
		`CREATE INDEX IF NOT EXISTS idx_node_sources_event ON node_sources(event_id);`,
		`CREATE TABLE IF NOT EXISTS node_contexts (
			node_id TEXT NOT NULL, context_id TEXT NOT NULL, relation TEXT NOT NULL DEFAULT 'origin',
			PRIMARY KEY(node_id, context_id, relation), FOREIGN KEY(node_id) REFERENCES memory_nodes(node_id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS node_keys (
			node_id TEXT NOT NULL, key_type TEXT NOT NULL, key_text TEXT NOT NULL,
			PRIMARY KEY(node_id, key_type, key_text), FOREIGN KEY(node_id) REFERENCES memory_nodes(node_id) ON DELETE CASCADE
		);`,
		`CREATE INDEX IF NOT EXISTS idx_node_keys_lookup ON node_keys(key_text, key_type);`,
		`CREATE TABLE IF NOT EXISTS slot_versions (
			slot_id TEXT NOT NULL, node_id TEXT NOT NULL, valid_from TEXT NOT NULL DEFAULT '', valid_until TEXT NOT NULL DEFAULT '',
			status TEXT NOT NULL, created_at TEXT NOT NULL, PRIMARY KEY(slot_id, node_id),
			FOREIGN KEY(node_id) REFERENCES memory_nodes(node_id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS conflict_sets (
			conflict_id TEXT PRIMARY KEY, space TEXT NOT NULL, slot_id TEXT NOT NULL, generation TEXT NOT NULL,
			content_hash TEXT NOT NULL, status TEXT NOT NULL, critical INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL, UNIQUE(slot_id, generation)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_conflicts_status ON conflict_sets(space, status, critical, created_at);`,
		`CREATE TABLE IF NOT EXISTS conflict_members (
			conflict_id TEXT NOT NULL, node_id TEXT NOT NULL, PRIMARY KEY(conflict_id, node_id),
			FOREIGN KEY(conflict_id) REFERENCES conflict_sets(conflict_id) ON DELETE CASCADE,
			FOREIGN KEY(node_id) REFERENCES memory_nodes(node_id) ON DELETE CASCADE
		);`,
		`CREATE TABLE IF NOT EXISTS resolutions (
			resolution_id TEXT PRIMARY KEY, conflict_id TEXT NOT NULL, generation TEXT NOT NULL, decision TEXT NOT NULL,
			winner_ids_json TEXT NOT NULL DEFAULT '[]', loser_ids_json TEXT NOT NULL DEFAULT '[]',
			conditions TEXT NOT NULL DEFAULT '', valid_from TEXT NOT NULL DEFAULT '', valid_until TEXT NOT NULL DEFAULT '',
			support_ids_json TEXT NOT NULL DEFAULT '[]', reason TEXT NOT NULL DEFAULT '', policy_id TEXT NOT NULL,
			created_at TEXT NOT NULL, FOREIGN KEY(conflict_id) REFERENCES conflict_sets(conflict_id) ON DELETE CASCADE
		);`,
		`CREATE INDEX IF NOT EXISTS idx_resolutions_conflict ON resolutions(conflict_id, generation, created_at);`,
		`CREATE TABLE IF NOT EXISTS jobs (
			job_id TEXT PRIMARY KEY, job_kind TEXT NOT NULL, space TEXT NOT NULL DEFAULT '', resource_id TEXT NOT NULL DEFAULT '',
			content_hash TEXT NOT NULL, payload_json TEXT NOT NULL DEFAULT '{}', status TEXT NOT NULL,
			attempts INTEGER NOT NULL DEFAULT 0, available_at TEXT NOT NULL, lease_until TEXT NOT NULL DEFAULT '',
			last_error TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			UNIQUE(job_kind, content_hash)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_jobs_ready ON jobs(status, available_at, job_kind);`,
		`CREATE TABLE IF NOT EXISTS outbox (
			seq INTEGER PRIMARY KEY AUTOINCREMENT, resource_kind TEXT NOT NULL, resource_id TEXT NOT NULL,
			operation TEXT NOT NULL, payload_json TEXT NOT NULL DEFAULT '{}', status TEXT NOT NULL DEFAULT 'pending',
			attempts INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
			UNIQUE(resource_kind, resource_id, operation)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_outbox_status_seq ON outbox(status, seq);`,
	}
	for _, statement := range ledgerStatements {
		if _, err := f.ledger.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate memory ledger: %w", err)
		}
	}
	if _, err := f.ledger.ExecContext(ctx, `INSERT INTO meta(key, value) VALUES ('schema_revision', ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, fabricSchemaRevision); err != nil {
		return fmt.Errorf("record memory ledger schema: %w", err)
	}

	indexStatements := []string{
		`PRAGMA journal_mode=WAL;`,
		`CREATE TABLE IF NOT EXISTS index_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);`,
		`CREATE TABLE IF NOT EXISTS documents (
			doc_id TEXT PRIMARY KEY, space TEXT NOT NULL, resource_kind TEXT NOT NULL, resource_id TEXT NOT NULL,
			content TEXT NOT NULL, keys_text TEXT NOT NULL DEFAULT '', context_id TEXT NOT NULL DEFAULT '',
			occurred_at TEXT NOT NULL DEFAULT '', slot_id TEXT NOT NULL DEFAULT '', semantic_status TEXT NOT NULL DEFAULT '',
			source_event_ids_json TEXT NOT NULL DEFAULT '[]', ledger_seq INTEGER NOT NULL DEFAULT 0, metadata_json TEXT NOT NULL DEFAULT '{}'
		);`,
		`CREATE INDEX IF NOT EXISTS idx_documents_space_kind ON documents(space, resource_kind, semantic_status);`,
		`CREATE INDEX IF NOT EXISTS idx_documents_slot ON documents(space, slot_id, semantic_status);`,
		`CREATE VIRTUAL TABLE IF NOT EXISTS document_fts USING fts5(
			doc_id UNINDEXED, space UNINDEXED, resource_kind UNINDEXED, content, keys_text
		);`,
		`CREATE TABLE IF NOT EXISTS key_postings (
			space TEXT NOT NULL, key_text TEXT NOT NULL, doc_id TEXT NOT NULL, weight REAL NOT NULL DEFAULT 1,
			PRIMARY KEY(space, key_text, doc_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_key_postings_lookup ON key_postings(space, key_text, weight);`,
		`CREATE TABLE IF NOT EXISTS active_slots (
			slot_id TEXT NOT NULL, node_id TEXT NOT NULL, space TEXT NOT NULL, semantic_status TEXT NOT NULL,
			valid_from TEXT NOT NULL DEFAULT '', valid_until TEXT NOT NULL DEFAULT '', resolution_id TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(slot_id, node_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_active_slots_space ON active_slots(space, semantic_status, slot_id);`,
		`CREATE TABLE IF NOT EXISTS _vec_memory_vectors (
			dataset_id TEXT NOT NULL, id TEXT NOT NULL, content TEXT NOT NULL DEFAULT '',
			meta TEXT NOT NULL DEFAULT '{}', embedding BLOB NOT NULL,
			PRIMARY KEY(dataset_id, id)
		);`,
	}
	for _, statement := range indexStatements {
		if _, err := f.index.ExecContext(ctx, statement); err != nil {
			return fmt.Errorf("migrate memory index: %w", err)
		}
	}
	if err := f.ensureVectorVirtualTable(ctx); err != nil {
		return fmt.Errorf("migrate memory vector index: %w", err)
	}
	for key, value := range map[string]string{
		"schema_revision":    fabricSchemaRevision,
		"index_generation":   "1",
		"indexed_ledger_seq": "0",
	} {
		if _, err := f.index.ExecContext(ctx, `INSERT INTO index_meta(key, value) VALUES (?, ?)
			ON CONFLICT(key) DO NOTHING`, key, value); err != nil {
			return fmt.Errorf("record memory index metadata: %w", err)
		}
	}
	return nil
}

func (f *Fabric) ensureVectorVirtualTable(ctx context.Context) error {
	escapedPath := strings.ReplaceAll(f.options.IndexPath, "'", "''")
	desired := fmt.Sprintf(`CREATE VIRTUAL TABLE memory_vectors USING vec(
		doc_id, dbpath='%s', index=auto, cover_distance=cosine, cover_parallel=auto
	)`, escapedPath)
	var existing string
	err := f.index.QueryRowContext(ctx, `SELECT sql FROM sqlite_master
		WHERE type='table' AND name='memory_vectors'`).Scan(&existing)
	switch {
	case errorsIsNoRowsMemory(err):
		_, err = f.index.ExecContext(ctx, desired)
		return err
	case err != nil:
		return err
	case vectorVirtualTableUsesPath(existing, f.options.IndexPath):
		return nil
	default:
		if _, err := f.index.ExecContext(ctx, `DROP TABLE memory_vectors`); err != nil {
			return err
		}
		_, err = f.index.ExecContext(ctx, desired)
		return err
	}
}

func vectorVirtualTableUsesPath(statement, path string) bool {
	escapedPath := strings.ReplaceAll(filepath.Clean(path), "'", "''")
	return strings.Contains(statement, "dbpath='"+escapedPath+"'")
}

func errorsIsNoRowsMemory(err error) bool {
	return err == sql.ErrNoRows
}
