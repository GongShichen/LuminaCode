-- Repeatable PostgreSQL/pgvector Memory Fabric schema. pgvector 0.8.6+.
CREATE EXTENSION IF NOT EXISTS vector;
CREATE TABLE IF NOT EXISTS lumina_memory_contexts (
  cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL, space text NOT NULL,
  context_id text NOT NULL, parent_id text, context_type text, label text,
  opened_at timestamptz, closed_at timestamptz,
  PRIMARY KEY(cluster_id,tenant_id,project_id,space,context_id)
);
CREATE TABLE IF NOT EXISTS lumina_memory_events (
  cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL, space text NOT NULL,
  event_id text NOT NULL, context_id text, session_id text, actor text NOT NULL, source_kind text,
  content text NOT NULL, occurred_at timestamptz NOT NULL, source_ref text, checksum text NOT NULL,
  metadata jsonb NOT NULL DEFAULT '{}', semantic_status text NOT NULL, token_estimate integer NOT NULL,
  tombstoned boolean NOT NULL DEFAULT false, created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
  PRIMARY KEY(cluster_id,tenant_id,project_id,space,event_id),
  UNIQUE(cluster_id,tenant_id,project_id,space,checksum)
);
CREATE INDEX IF NOT EXISTS lumina_memory_events_context_idx ON lumina_memory_events
  (cluster_id,tenant_id,project_id,space,context_id,occurred_at) WHERE tombstoned=false;
CREATE TABLE IF NOT EXISTS lumina_memory_identities (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
  identity_id text NOT NULL,canonical text NOT NULL,identity_type text,display_name text,status text NOT NULL,
  created_at timestamptz NOT NULL,PRIMARY KEY(cluster_id,tenant_id,project_id,space,identity_id),
  UNIQUE(cluster_id,tenant_id,project_id,space,identity_type,canonical)
);
CREATE TABLE IF NOT EXISTS lumina_memory_identity_aliases (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
  normalized_alias text NOT NULL,identity_id text NOT NULL,source_event_id text,method text,status text NOT NULL,
  created_at timestamptz NOT NULL,
  PRIMARY KEY(cluster_id,tenant_id,project_id,space,normalized_alias,identity_id)
);
CREATE TABLE IF NOT EXISTS lumina_memory_slots (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
  slot_id text NOT NULL,subject_identity_id text,facet text,attribute_key text,scope_key text,
  created_at timestamptz NOT NULL,PRIMARY KEY(cluster_id,tenant_id,project_id,space,slot_id),
  UNIQUE(cluster_id,tenant_id,project_id,space,subject_identity_id,facet,attribute_key,scope_key)
);
CREATE TABLE IF NOT EXISTS lumina_memory_nodes (
  cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL, space text NOT NULL,
  node_id text NOT NULL, context_id text, slot_id text, status text NOT NULL, statement text NOT NULL,
  node jsonb NOT NULL, content_hash text NOT NULL, created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
  PRIMARY KEY(cluster_id,tenant_id,project_id,space,node_id)
);
CREATE INDEX IF NOT EXISTS lumina_memory_nodes_slot_idx ON lumina_memory_nodes
  (cluster_id,tenant_id,project_id,space,slot_id,status);
CREATE TABLE IF NOT EXISTS lumina_memory_claim_values (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
  node_id text NOT NULL,value jsonb NOT NULL,PRIMARY KEY(cluster_id,tenant_id,project_id,space,node_id)
);
CREATE TABLE IF NOT EXISTS lumina_memory_node_sources (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
  node_id text NOT NULL,event_id text NOT NULL,start_rune integer NOT NULL,end_rune integer NOT NULL,source_role text,
  PRIMARY KEY(cluster_id,tenant_id,project_id,space,node_id,event_id,start_rune,end_rune)
);
CREATE TABLE IF NOT EXISTS lumina_memory_node_keys (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
  node_id text NOT NULL,key_type text NOT NULL,key_text text NOT NULL,
  PRIMARY KEY(cluster_id,tenant_id,project_id,space,node_id,key_type,key_text)
);
CREATE INDEX IF NOT EXISTS lumina_memory_node_keys_lookup_idx ON lumina_memory_node_keys
  (cluster_id,tenant_id,project_id,space,key_text,key_type);
CREATE TABLE IF NOT EXISTS lumina_memory_slot_versions (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
  slot_id text NOT NULL,node_id text NOT NULL,valid_from timestamptz,valid_until timestamptz,status text NOT NULL,
  created_at timestamptz NOT NULL,PRIMARY KEY(cluster_id,tenant_id,project_id,space,slot_id,node_id)
);
CREATE TABLE IF NOT EXISTS lumina_memory_conflicts (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
  conflict_id text NOT NULL,slot_id text NOT NULL,generation text NOT NULL,status text NOT NULL,
  conflict jsonb NOT NULL,created_at timestamptz NOT NULL,updated_at timestamptz NOT NULL,
  PRIMARY KEY(cluster_id,tenant_id,project_id,space,conflict_id),
  UNIQUE(cluster_id,tenant_id,project_id,space,slot_id,generation)
);
CREATE TABLE IF NOT EXISTS lumina_memory_resolutions (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
  resolution_id text NOT NULL,conflict_id text NOT NULL,generation text NOT NULL,resolution jsonb NOT NULL,
  created_at timestamptz NOT NULL,PRIMARY KEY(cluster_id,tenant_id,project_id,space,resolution_id)
);
CREATE TABLE IF NOT EXISTS lumina_memory_documents (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
  doc_id text NOT NULL,resource_kind text NOT NULL,resource_id text NOT NULL,context_id text,actor text,
  occurred_at timestamptz,status text,content text NOT NULL,source_event_ids jsonb NOT NULL DEFAULT '[]',
  metadata jsonb NOT NULL DEFAULT '{}',embedding vector(1024),embedding_model text,
  search_vector tsvector GENERATED ALWAYS AS (to_tsvector('simple',coalesce(content,''))) STORED,
  PRIMARY KEY(cluster_id,tenant_id,project_id,space,doc_id)
);
CREATE INDEX IF NOT EXISTS lumina_memory_documents_fts_idx ON lumina_memory_documents USING gin(search_vector);
CREATE INDEX IF NOT EXISTS lumina_memory_documents_hnsw_idx ON lumina_memory_documents
  USING hnsw (embedding vector_cosine_ops) WITH (m=16,ef_construction=64);
CREATE TABLE IF NOT EXISTS lumina_memory_spans (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
  event_id text NOT NULL,ordinal integer NOT NULL,content text NOT NULL,embedding halfvec(1024),source_ref text,
  model_revision text NOT NULL,tokenizer_hash text NOT NULL,
  PRIMARY KEY(cluster_id,tenant_id,project_id,space,event_id,ordinal)
);
CREATE INDEX IF NOT EXISTS lumina_memory_spans_hnsw_idx ON lumina_memory_spans
  USING hnsw (embedding halfvec_cosine_ops) WITH (m=16,ef_construction=64);
CREATE TABLE IF NOT EXISTS lumina_memory_sparse_postings (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
  event_id text NOT NULL,token_id bigint NOT NULL,weight real NOT NULL,
  PRIMARY KEY(cluster_id,tenant_id,project_id,space,event_id,token_id)
);
CREATE INDEX IF NOT EXISTS lumina_memory_sparse_lookup_idx ON lumina_memory_sparse_postings
  (cluster_id,tenant_id,project_id,space,token_id,event_id);
CREATE TABLE IF NOT EXISTS lumina_memory_graph_edges (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
  source_id text NOT NULL,target_id text NOT NULL,weight real NOT NULL,
  PRIMARY KEY(cluster_id,tenant_id,project_id,space,source_id,target_id)
);
CREATE TABLE IF NOT EXISTS lumina_memory_jobs (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,job_id text NOT NULL,
  kind text NOT NULL,space text NOT NULL,resource_id text NOT NULL,payload jsonb NOT NULL DEFAULT '{}',
  status text NOT NULL,attempts integer NOT NULL DEFAULT 0,available_at timestamptz NOT NULL,
  lease_owner text,lease_until timestamptz,last_error text,created_at timestamptz NOT NULL,updated_at timestamptz NOT NULL,
  PRIMARY KEY(cluster_id,tenant_id,project_id,job_id)
);
CREATE INDEX IF NOT EXISTS lumina_memory_jobs_claim_idx ON lumina_memory_jobs
  (cluster_id,tenant_id,project_id,status,available_at);
CREATE TABLE IF NOT EXISTS lumina_memory_outbox (
  id bigserial PRIMARY KEY,cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,
  space text NOT NULL,resource_kind text NOT NULL,resource_id text NOT NULL,operation text NOT NULL,
  payload jsonb NOT NULL DEFAULT '{}',status text NOT NULL,attempts integer NOT NULL DEFAULT 0,
  created_at timestamptz NOT NULL,updated_at timestamptz NOT NULL,
  UNIQUE(cluster_id,tenant_id,project_id,space,resource_kind,resource_id,operation)
);
CREATE TABLE IF NOT EXISTS lumina_memory_index_state (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,key text NOT NULL,value text NOT NULL,
  updated_at timestamptz NOT NULL,PRIMARY KEY(cluster_id,tenant_id,project_id,key)
);
CREATE TABLE IF NOT EXISTS lumina_memory_imports (
  cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,migration_id text NOT NULL,
  space text NOT NULL,status text NOT NULL,checksum text NOT NULL,created_at timestamptz NOT NULL,
  completed_at timestamptz,PRIMARY KEY(cluster_id,tenant_id,project_id,migration_id)
);
