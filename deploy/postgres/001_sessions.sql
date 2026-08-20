-- Repeatable cluster Session Runtime schema. PostgreSQL 15+.
CREATE TABLE IF NOT EXISTS lumina_schema_migrations (
  component text PRIMARY KEY, version integer NOT NULL, applied_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS lumina_sessions (
  cluster_id text NOT NULL, tenant_id text NOT NULL, session_id text NOT NULL,
  cwd text NOT NULL DEFAULT '', created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
  message_count integer NOT NULL DEFAULT 0, turn_count integer NOT NULL DEFAULT 0,
  pinned boolean NOT NULL DEFAULT false, status text NOT NULL DEFAULT 'idle',
  last_event_seq bigint NOT NULL DEFAULT 0, fence_token bigint NOT NULL DEFAULT 0,
  PRIMARY KEY(cluster_id, tenant_id, session_id)
);
CREATE INDEX IF NOT EXISTS lumina_sessions_updated_idx
  ON lumina_sessions(cluster_id, tenant_id, updated_at DESC);
CREATE TABLE IF NOT EXISTS lumina_session_streams (
  cluster_id text NOT NULL, tenant_id text NOT NULL, session_id text NOT NULL,
  stream_id text NOT NULL, kind text NOT NULL, parent_stream_id text,
  head_seq bigint NOT NULL DEFAULT 0, status text NOT NULL,
  created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
  PRIMARY KEY(cluster_id, tenant_id, session_id, stream_id),
  FOREIGN KEY(cluster_id, tenant_id, session_id)
    REFERENCES lumina_sessions(cluster_id, tenant_id, session_id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS lumina_session_events (
  cluster_id text NOT NULL, tenant_id text NOT NULL, session_id text NOT NULL,
  seq bigint NOT NULL, event_id text NOT NULL, stream_id text NOT NULL, stream_seq bigint NOT NULL,
  type text NOT NULL, schema_version integer NOT NULL, occurred_at timestamptz NOT NULL,
  causation_id text, correlation_id text, audience jsonb NOT NULL, payload jsonb NOT NULL,
  payload_blob_id text, PRIMARY KEY(cluster_id, tenant_id, session_id, seq),
  UNIQUE(cluster_id, tenant_id, event_id),
  UNIQUE(cluster_id, tenant_id, session_id, stream_id, stream_seq),
  FOREIGN KEY(cluster_id, tenant_id, session_id, stream_id)
    REFERENCES lumina_session_streams(cluster_id, tenant_id, session_id, stream_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS lumina_session_events_type_idx
  ON lumina_session_events(cluster_id, tenant_id, session_id, type, seq);
CREATE TABLE IF NOT EXISTS lumina_session_checkpoints (
  cluster_id text NOT NULL, tenant_id text NOT NULL, session_id text NOT NULL,
  stream_id text NOT NULL, projector text NOT NULL, projector_version integer NOT NULL,
  upto_seq bigint NOT NULL, state_json bytea NOT NULL, created_at timestamptz NOT NULL,
  PRIMARY KEY(cluster_id, tenant_id, session_id, stream_id, projector)
);
CREATE TABLE IF NOT EXISTS lumina_session_command_results (
  cluster_id text NOT NULL, tenant_id text NOT NULL, session_id text NOT NULL,
  command_id text NOT NULL, accepted_seq bigint NOT NULL, result_json jsonb NOT NULL,
  created_at timestamptz NOT NULL, PRIMARY KEY(cluster_id, tenant_id, session_id, command_id)
);
CREATE TABLE IF NOT EXISTS lumina_session_consumer_offsets (
  cluster_id text NOT NULL, tenant_id text NOT NULL, session_id text NOT NULL,
  consumer text NOT NULL, upto_seq bigint NOT NULL, updated_at timestamptz NOT NULL,
  PRIMARY KEY(cluster_id, tenant_id, session_id, consumer)
);
CREATE TABLE IF NOT EXISTS lumina_session_blobs (
  cluster_id text NOT NULL, tenant_id text NOT NULL, digest text NOT NULL,
  mime_type text NOT NULL, size_bytes bigint NOT NULL, data bytea NOT NULL,
  created_at timestamptz NOT NULL, PRIMARY KEY(cluster_id, tenant_id, digest)
);
CREATE TABLE IF NOT EXISTS lumina_session_outbox (
  id bigserial PRIMARY KEY, cluster_id text NOT NULL, tenant_id text NOT NULL,
  session_id text NOT NULL, seq bigint NOT NULL, topic text NOT NULL,
  payload jsonb NOT NULL, created_at timestamptz NOT NULL, published_at timestamptz,
  claimed_by text, claim_until timestamptz,
  UNIQUE(cluster_id, tenant_id, session_id, seq, topic)
);
CREATE INDEX IF NOT EXISTS lumina_session_outbox_pending_idx
  ON lumina_session_outbox(cluster_id, published_at, id) WHERE published_at IS NULL;
CREATE TABLE IF NOT EXISTS lumina_session_imports (
  cluster_id text NOT NULL, tenant_id text NOT NULL, session_id text NOT NULL,
  migration_id text NOT NULL, status text NOT NULL, event_count bigint NOT NULL DEFAULT 0,
  checksum text, created_at timestamptz NOT NULL, completed_at timestamptz,
  PRIMARY KEY(cluster_id, tenant_id, session_id), UNIQUE(cluster_id, tenant_id, migration_id)
);
