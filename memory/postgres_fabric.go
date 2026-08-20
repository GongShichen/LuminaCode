package memory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvector "github.com/pgvector/pgvector-go"
	pgvectorpgx "github.com/pgvector/pgvector-go/pgx"
)

const (
	postgresFabricSchemaVersion = 3
	postgresFabricDimensions    = 1024
	postgresExactVectorLimit    = 50_000
)

func PostgresSchemaVersion() int { return postgresFabricSchemaVersion }

type PostgresFabricOptions struct {
	FabricOptions
	PostgresURL string
	ClusterID   string
	TenantID    string
	ProjectID   string
}

type PostgresFabric struct {
	options   FabricOptions
	pool      *pgxpool.Pool
	clusterID string
	tenantID  string
	projectID string

	workerCtx    context.Context
	workerCancel context.CancelFunc
	workerWG     sync.WaitGroup
	closeOnce    sync.Once
	jobWake      chan struct{}
}

var _ FabricEngine = (*PostgresFabric)(nil)

func OpenPostgresFabric(ctx context.Context, open PostgresFabricOptions) (*PostgresFabric, error) {
	if strings.TrimSpace(open.PostgresURL) == "" {
		return nil, errors.New("PostgreSQL Memory Fabric requires postgres_url")
	}
	open.FabricOptions = normalizeFabricOptions(open.FabricOptions)
	if open.FabricOptions.RetrievalEncoder != nil {
		// The cluster schema deliberately fixes BGE-M3 vectors to 1024
		// dimensions so every instance shares one pgvector index contract.
		if vectorizer := open.FabricOptions.Vectorizer; vectorizer != nil && vectorizer.Dimensions() != 0 &&
			vectorizer.Dimensions() != postgresFabricDimensions {
			return nil, fmt.Errorf("PostgreSQL Memory Fabric requires %d-dimensional embeddings, got %d",
				postgresFabricDimensions, vectorizer.Dimensions())
		}
	}
	poolConfig, err := pgxpool.ParseConfig(open.PostgresURL)
	if err != nil {
		return nil, errors.New("invalid postgres_url for PostgreSQL Memory Fabric")
	}
	bootstrapConfig := poolConfig.Copy()
	bootstrapConfig.AfterConnect = nil
	bootstrap, err := pgxpool.NewWithConfig(ctx, bootstrapConfig)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL Memory Fabric bootstrap connection: %w", err)
	}
	if _, err := bootstrap.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		bootstrap.Close()
		return nil, fmt.Errorf("enable pgvector extension: %w", err)
	}
	bootstrap.Close()
	poolConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgvectorpgx.RegisterTypes(ctx, conn)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("open PostgreSQL Memory Fabric: %w", err)
	}
	fabric := &PostgresFabric{options: open.FabricOptions, pool: pool,
		clusterID: firstNonEmptyMemory(strings.TrimSpace(open.ClusterID), "default"),
		tenantID:  firstNonEmptyMemory(strings.TrimSpace(open.TenantID), "local"),
		projectID: firstNonEmptyMemory(strings.TrimSpace(open.ProjectID), "default"),
		jobWake:   make(chan struct{}, 1)}
	if err := fabric.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := fabric.recoverJobs(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if err := fabric.prepareDerivedJobs(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	if open.StartWorkers {
		fabric.startWorkers()
	}
	return fabric, nil
}

func (f *PostgresFabric) migrate(ctx context.Context) error {
	connection, err := f.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock(hashtext('lumina_memory_schema'))`); err != nil {
		return err
	}
	defer connection.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext('lumina_memory_schema'))`)
	if _, err := connection.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS vector`); err != nil {
		return fmt.Errorf("enable pgvector extension: %w", err)
	}
	var extensionVersion string
	if err := connection.QueryRow(ctx, `SELECT extversion FROM pg_extension WHERE extname='vector'`).Scan(&extensionVersion); err != nil {
		return fmt.Errorf("inspect pgvector extension: %w", err)
	}
	if versionLess(extensionVersion, "0.8.6") {
		return fmt.Errorf("pgvector %s is unsupported; version 0.8.6 or newer is required", extensionVersion)
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS lumina_schema_migrations (
			component text PRIMARY KEY, version integer NOT NULL, applied_at timestamptz NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_contexts (
			cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL,
			space text NOT NULL, context_id text NOT NULL, parent_id text, context_type text,
			label text, opened_at timestamptz, closed_at timestamptz,
			PRIMARY KEY(cluster_id,tenant_id,project_id,space,context_id))`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_events (
			cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL,
			space text NOT NULL, event_id text NOT NULL, context_id text, session_id text,
			actor text NOT NULL, source_kind text, content text NOT NULL, occurred_at timestamptz NOT NULL,
			source_ref text, checksum text NOT NULL, metadata jsonb NOT NULL DEFAULT '{}',
			semantic_status text NOT NULL, token_estimate integer NOT NULL, tombstoned boolean NOT NULL DEFAULT false,
			created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
			PRIMARY KEY(cluster_id,tenant_id,project_id,space,event_id),
			UNIQUE(cluster_id,tenant_id,project_id,space,checksum))`,
		`CREATE INDEX IF NOT EXISTS lumina_memory_events_context_idx ON lumina_memory_events
			(cluster_id,tenant_id,project_id,space,context_id,occurred_at) WHERE tombstoned=false`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_identities (
			cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
			identity_id text NOT NULL,canonical text NOT NULL,identity_type text,display_name text,status text NOT NULL,
			created_at timestamptz NOT NULL,PRIMARY KEY(cluster_id,tenant_id,project_id,space,identity_id),
			UNIQUE(cluster_id,tenant_id,project_id,space,identity_type,canonical))`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_identity_aliases (
			cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
			normalized_alias text NOT NULL,identity_id text NOT NULL,source_event_id text,method text,status text NOT NULL,
			created_at timestamptz NOT NULL,PRIMARY KEY(cluster_id,tenant_id,project_id,space,normalized_alias,identity_id))`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_slots (
			cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
			slot_id text NOT NULL,subject_identity_id text,facet text,attribute_key text,scope_key text,
			created_at timestamptz NOT NULL,PRIMARY KEY(cluster_id,tenant_id,project_id,space,slot_id),
			UNIQUE(cluster_id,tenant_id,project_id,space,subject_identity_id,facet,attribute_key,scope_key))`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_nodes (
			cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL,
			space text NOT NULL, node_id text NOT NULL, context_id text, slot_id text, status text NOT NULL,
			statement text NOT NULL, node jsonb NOT NULL, content_hash text NOT NULL,
			created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
			PRIMARY KEY(cluster_id,tenant_id,project_id,space,node_id))`,
		`CREATE INDEX IF NOT EXISTS lumina_memory_nodes_slot_idx ON lumina_memory_nodes
			(cluster_id,tenant_id,project_id,space,slot_id,status)`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_claim_values (
			cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
			node_id text NOT NULL,value jsonb NOT NULL,PRIMARY KEY(cluster_id,tenant_id,project_id,space,node_id))`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_node_sources (
			cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL,
			space text NOT NULL, node_id text NOT NULL, event_id text NOT NULL,
			start_rune integer NOT NULL, end_rune integer NOT NULL, source_role text,
			PRIMARY KEY(cluster_id,tenant_id,project_id,space,node_id,event_id,start_rune,end_rune))`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_node_keys (
			cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
			node_id text NOT NULL,key_type text NOT NULL,key_text text NOT NULL,
			PRIMARY KEY(cluster_id,tenant_id,project_id,space,node_id,key_type,key_text))`,
		`CREATE INDEX IF NOT EXISTS lumina_memory_node_keys_lookup_idx ON lumina_memory_node_keys
			(cluster_id,tenant_id,project_id,space,key_text,key_type)`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_slot_versions (
			cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,space text NOT NULL,
			slot_id text NOT NULL,node_id text NOT NULL,valid_from timestamptz,valid_until timestamptz,status text NOT NULL,
			created_at timestamptz NOT NULL,PRIMARY KEY(cluster_id,tenant_id,project_id,space,slot_id,node_id))`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_conflicts (
			cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL,
			space text NOT NULL, conflict_id text NOT NULL, slot_id text NOT NULL,
			generation text NOT NULL, status text NOT NULL, conflict jsonb NOT NULL,
			created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
			PRIMARY KEY(cluster_id,tenant_id,project_id,space,conflict_id),
			UNIQUE(cluster_id,tenant_id,project_id,space,slot_id,generation))`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_resolutions (
			cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL,
			space text NOT NULL, resolution_id text NOT NULL, conflict_id text NOT NULL,
			generation text NOT NULL, resolution jsonb NOT NULL, created_at timestamptz NOT NULL,
			PRIMARY KEY(cluster_id,tenant_id,project_id,space,resolution_id))`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_documents (
			cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL,
			space text NOT NULL, doc_id text NOT NULL, resource_kind text NOT NULL, resource_id text NOT NULL,
			context_id text, actor text, occurred_at timestamptz, status text, content text NOT NULL,
			source_event_ids jsonb NOT NULL DEFAULT '[]', metadata jsonb NOT NULL DEFAULT '{}',
			embedding vector(1024),embedding_model text,
			search_vector tsvector GENERATED ALWAYS AS (to_tsvector('simple',coalesce(content,''))) STORED,
			PRIMARY KEY(cluster_id,tenant_id,project_id,space,doc_id))`,
		`ALTER TABLE lumina_memory_documents ADD COLUMN IF NOT EXISTS embedding vector(1024)`,
		`ALTER TABLE lumina_memory_documents ADD COLUMN IF NOT EXISTS embedding_model text`,
		`CREATE INDEX IF NOT EXISTS lumina_memory_documents_fts_idx ON lumina_memory_documents USING gin(search_vector)`,
		`CREATE INDEX IF NOT EXISTS lumina_memory_documents_hnsw_idx ON lumina_memory_documents
			USING hnsw (embedding vector_cosine_ops) WITH (m=16,ef_construction=64)`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_spans (
			cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL,
			space text NOT NULL, event_id text NOT NULL, ordinal integer NOT NULL, content text NOT NULL,
			embedding halfvec(1024), source_ref text, model_revision text NOT NULL, tokenizer_hash text NOT NULL,
			PRIMARY KEY(cluster_id,tenant_id,project_id,space,event_id,ordinal))`,
		`CREATE INDEX IF NOT EXISTS lumina_memory_spans_hnsw_idx ON lumina_memory_spans
			USING hnsw (embedding halfvec_cosine_ops) WITH (m=16,ef_construction=64)`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_sparse_postings (
			cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL,
			space text NOT NULL, event_id text NOT NULL, token_id bigint NOT NULL, weight real NOT NULL,
			PRIMARY KEY(cluster_id,tenant_id,project_id,space,event_id,token_id))`,
		`CREATE INDEX IF NOT EXISTS lumina_memory_sparse_lookup_idx ON lumina_memory_sparse_postings
			(cluster_id,tenant_id,project_id,space,token_id,event_id)`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_graph_edges (
			cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL,
			space text NOT NULL, source_id text NOT NULL, target_id text NOT NULL, weight real NOT NULL,
			PRIMARY KEY(cluster_id,tenant_id,project_id,space,source_id,target_id))`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_jobs (
			cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL,
			job_id text NOT NULL, kind text NOT NULL, space text NOT NULL, resource_id text NOT NULL,
			payload jsonb NOT NULL DEFAULT '{}', status text NOT NULL, attempts integer NOT NULL DEFAULT 0,
			available_at timestamptz NOT NULL, lease_owner text, lease_until timestamptz,
			last_error text, created_at timestamptz NOT NULL, updated_at timestamptz NOT NULL,
			PRIMARY KEY(cluster_id,tenant_id,project_id,job_id))`,
		`CREATE INDEX IF NOT EXISTS lumina_memory_jobs_claim_idx ON lumina_memory_jobs
			(cluster_id,tenant_id,project_id,status,available_at)`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_index_state (
			cluster_id text NOT NULL, tenant_id text NOT NULL, project_id text NOT NULL,
			key text NOT NULL, value text NOT NULL, updated_at timestamptz NOT NULL,
			PRIMARY KEY(cluster_id,tenant_id,project_id,key))`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_outbox (
			id bigserial PRIMARY KEY,cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,
			space text NOT NULL,resource_kind text NOT NULL,resource_id text NOT NULL,operation text NOT NULL,
			payload jsonb NOT NULL DEFAULT '{}',status text NOT NULL,attempts integer NOT NULL DEFAULT 0,
			created_at timestamptz NOT NULL,updated_at timestamptz NOT NULL,
			UNIQUE(cluster_id,tenant_id,project_id,space,resource_kind,resource_id,operation))`,
		`CREATE TABLE IF NOT EXISTS lumina_memory_imports (
			cluster_id text NOT NULL,tenant_id text NOT NULL,project_id text NOT NULL,migration_id text NOT NULL,
			space text NOT NULL,status text NOT NULL,checksum text NOT NULL,created_at timestamptz NOT NULL,
			completed_at timestamptz,PRIMARY KEY(cluster_id,tenant_id,project_id,migration_id))`,
	}
	for _, statement := range statements {
		if _, err := connection.Exec(ctx, statement); err != nil {
			return fmt.Errorf("migrate PostgreSQL Memory Fabric: %w", err)
		}
	}
	if _, err = connection.Exec(ctx, `INSERT INTO lumina_schema_migrations(component,version,applied_at)
		VALUES('memory',$1,$2) ON CONFLICT(component) DO UPDATE SET
		version=GREATEST(lumina_schema_migrations.version,excluded.version),applied_at=excluded.applied_at`,
		postgresFabricSchemaVersion, f.now()); err != nil {
		return err
	}
	var version int
	if err := connection.QueryRow(ctx, `SELECT version FROM lumina_schema_migrations WHERE component='memory'`).Scan(&version); err != nil {
		return err
	}
	if version != postgresFabricSchemaVersion {
		return fmt.Errorf("unsupported PostgreSQL Memory Fabric schema version %d (binary supports %d)",
			version, postgresFabricSchemaVersion)
	}
	return nil
}

func (f *PostgresFabric) now() time.Time { return f.options.Clock().UTC() }

func (f *PostgresFabric) Close() error {
	if f == nil {
		return nil
	}
	f.closeOnce.Do(func() {
		if f.workerCancel != nil {
			f.workerCancel()
		}
		f.workerWG.Wait()
		f.pool.Close()
		if f.options.Cleanup != nil {
			_ = f.options.Cleanup()
		}
	})
	return nil
}

func (f *PostgresFabric) RetrievalSidecarEnabled() bool {
	return f != nil && f.options.RetrievalEncoder != nil
}

func versionLess(left, right string) bool {
	parse := func(value string) [3]int {
		var result [3]int
		parts := strings.SplitN(value, ".", 4)
		for index := 0; index < len(parts) && index < 3; index++ {
			digits := strings.TrimLeftFunc(parts[index], func(r rune) bool { return r < '0' || r > '9' })
			digits = strings.TrimRightFunc(digits, func(r rune) bool { return r < '0' || r > '9' })
			result[index], _ = strconv.Atoi(digits)
		}
		return result
	}
	l, r := parse(left), parse(right)
	for index := range l {
		if l[index] != r[index] {
			return l[index] < r[index]
		}
	}
	return false
}

func (f *PostgresFabric) AppendEvents(ctx context.Context, events []RawEvent,
	ingestOptions IngestOptions) (IngestResult, error) {
	result := IngestResult{SemanticStatus: SemanticEventDurable}
	if f != nil && f.options.WriteGuard != nil {
		if err := f.options.WriteGuard(ctx); err != nil {
			return result, err
		}
	}
	if f == nil || f.pool == nil {
		return result, errors.New("PostgreSQL Memory Fabric is closed")
	}
	if len(events) == 0 {
		result.Durable = true
		return result, nil
	}
	if ingestOptions.SemanticPolicy == "" {
		ingestOptions.SemanticPolicy = SemanticDeferred
	}
	now := f.now()
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return result, err
	}
	defer tx.Rollback(ctx)
	contextLocks := make([]string, 0, len(events))
	for _, event := range events {
		if strings.TrimSpace(event.ContextID) != "" {
			contextLocks = append(contextLocks, normalizeSpace(event.Space)+"\x1f"+event.ContextID)
		}
	}
	contextLocks = uniqueStrings(contextLocks)
	sort.Strings(contextLocks)
	for _, lockKey := range contextLocks {
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`,
			f.clusterID+"\x1f"+f.tenantID+"\x1f"+f.projectID+"\x1fcontext\x1f"+lockKey); err != nil {
			return result, err
		}
	}
	normalized := make([]RawEvent, 0, len(events))
	for _, event := range events {
		event.Space = normalizeSpace(event.Space)
		event.Content = strings.TrimSpace(event.Content)
		event.Actor = strings.ToLower(strings.TrimSpace(event.Actor))
		if event.Actor == "" {
			event.Actor = "unknown"
		}
		if event.Content == "" {
			continue
		}
		if event.OccurredAt.IsZero() {
			event.OccurredAt = now
		} else {
			event.OccurredAt = event.OccurredAt.UTC()
		}
		if event.ID == "" {
			event.ID = stableFabricID("evt", event.Space, event.ContextID, event.SessionID, event.Actor,
				event.SourceKind, event.SourceRef, formatFabricTime(event.OccurredAt), event.Content)
		}
		if event.ContextID != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_contexts(
				cluster_id,tenant_id,project_id,space,context_id,context_type,opened_at)
				VALUES($1,$2,$3,$4,$5,'segment',$6) ON CONFLICT DO NOTHING`, f.clusterID, f.tenantID,
				f.projectID, event.Space, event.ContextID, event.OccurredAt); err != nil {
				return result, err
			}
		}
		checksum := eventChecksum(event)
		metadata := marshalJSON(event.Metadata)
		command, err := tx.Exec(ctx, `INSERT INTO lumina_memory_events(
			cluster_id,tenant_id,project_id,space,event_id,context_id,session_id,actor,source_kind,
			content,occurred_at,source_ref,checksum,metadata,semantic_status,token_estimate,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8,NULLIF($9,''),$10,$11,NULLIF($12,''),
			$13,$14,$15,$16,$17,$17) ON CONFLICT DO NOTHING`, f.clusterID, f.tenantID, f.projectID,
			event.Space, event.ID, event.ContextID, event.SessionID, event.Actor, event.SourceKind,
			event.Content, event.OccurredAt, event.SourceRef, checksum, metadata, SemanticEventDurable,
			estimateTokens(event.Content), now)
		if err != nil {
			return result, fmt.Errorf("append PostgreSQL memory event: %w", err)
		}
		if command.RowsAffected() == 0 {
			if err := tx.QueryRow(ctx, `SELECT event_id FROM lumina_memory_events WHERE
				cluster_id=$1 AND tenant_id=$2 AND project_id=$3 AND space=$4 AND checksum=$5`,
				f.clusterID, f.tenantID, f.projectID, event.Space, checksum).Scan(&event.ID); err != nil {
				return result, err
			}
		}
		sourceIDs, _ := json.Marshal([]string{event.ID})
		metadataJSON, _ := json.Marshal(map[string]any{"actor": event.Actor, "source_kind": event.SourceKind,
			"source_ref": event.SourceRef, "session_id": event.SessionID})
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_documents(
			cluster_id,tenant_id,project_id,space,doc_id,resource_kind,resource_id,context_id,actor,
			occurred_at,status,content,source_event_ids,metadata) VALUES($1,$2,$3,$4,$5,'event',$5,
			NULLIF($6,''),$7,$8,$9,$10,$11,$12)
			ON CONFLICT(cluster_id,tenant_id,project_id,space,doc_id) DO UPDATE SET
			content=excluded.content,occurred_at=excluded.occurred_at,metadata=excluded.metadata`,
			f.clusterID, f.tenantID, f.projectID, event.Space, event.ID, event.ContextID, event.Actor,
			event.OccurredAt, SemanticEventDurable, event.Content, sourceIDs, metadataJSON); err != nil {
			return result, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_outbox(
			cluster_id,tenant_id,project_id,space,resource_kind,resource_id,operation,status,created_at,updated_at)
			VALUES($1,$2,$3,$4,'event',$5,'upsert','done',$6,$6)
			ON CONFLICT(cluster_id,tenant_id,project_id,space,resource_kind,resource_id,operation)
			DO UPDATE SET status='done',updated_at=excluded.updated_at`, f.clusterID, f.tenantID,
			f.projectID, event.Space, event.ID, now); err != nil {
			return result, err
		}
		if err := f.linkContextEventTx(ctx, tx, event); err != nil {
			return result, err
		}
		if f.options.RetrievalEncoder != nil {
			if err := f.enqueueJobTx(ctx, tx, "index_event", event.Space, event.ID,
				map[string]any{"event_id": event.ID}, now); err != nil {
				return result, err
			}
		}
		normalized = append(normalized, event)
		result.EventIDs = append(result.EventIDs, event.ID)
	}
	if err := tx.Commit(ctx); err != nil {
		return result, err
	}
	result.Durable = true
	result.EventIDs = uniqueStrings(result.EventIDs)
	if len(result.EventIDs) == 0 {
		return result, nil
	}
	if f.options.RetrievalEncoder != nil {
		if err := f.indexEvents(ctx, normalized); err != nil {
			f.wakeWorker()
			return result, IndexLagError{Cause: err}
		}
		if err := f.finishJobsByResource(ctx, "index_event", result.EventIDs); err != nil {
			return result, IndexLagError{Cause: err}
		}
	}
	if ingestOptions.SemanticPolicy == SemanticDeterministic {
		drafts := deterministicDrafts(normalized)
		if len(drafts) > 0 {
			commit, commitErr := f.commitDrafts(ctx, MemoryRequest{Space: normalized[0].Space,
				ContextID: normalized[0].ContextID, SourceEventIDs: result.EventIDs, Drafts: drafts,
				Mode: WriteCriticalResult}, drafts)
			result.SemanticStatus = commit.SemanticStatus
			if commitErr != nil {
				return result, commitErr
			}
		}
	} else if ingestOptions.SemanticPolicy != SemanticDurableOnly && len(normalized) > 0 &&
		normalized[0].ContextID != "" {
		job, enqueueErr := f.enqueueJob(ctx, "compile_context", normalized[0].Space,
			normalized[0].ContextID, map[string]any{"context_id": normalized[0].ContextID}, now)
		result.PendingJobID = job.ID
		if enqueueErr != nil {
			return result, enqueueErr
		}
	}
	if ingestOptions.SealContext && len(normalized) > 0 && normalized[0].ContextID != "" {
		job, sealErr := f.SealContext(ctx, ContextRef{ID: normalized[0].ContextID, Space: normalized[0].Space,
			ClosedAt: now})
		result.PendingJobID = job.ID
		if sealErr != nil {
			return result, sealErr
		}
	}
	f.wakeWorker()
	return result, nil
}

func (f *PostgresFabric) linkContextEventTx(ctx context.Context, tx pgx.Tx, event RawEvent) error {
	if event.ContextID == "" {
		return nil
	}
	var previous string
	err := tx.QueryRow(ctx, `SELECT event_id FROM lumina_memory_events WHERE cluster_id=$1 AND tenant_id=$2
		AND project_id=$3 AND space=$4 AND context_id=$5 AND event_id<>$6 AND tombstoned=false
		ORDER BY occurred_at DESC,event_id DESC LIMIT 1`, f.clusterID, f.tenantID, f.projectID,
		event.Space, event.ContextID, event.ID).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, pair := range [][2]string{{previous, event.ID}, {event.ID, previous}} {
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_graph_edges(
			cluster_id,tenant_id,project_id,space,source_id,target_id,weight)
			VALUES($1,$2,$3,$4,$5,$6,1) ON CONFLICT DO NOTHING`, f.clusterID, f.tenantID,
			f.projectID, event.Space, pair[0], pair[1]); err != nil {
			return err
		}
	}
	return nil
}

func (f *PostgresFabric) indexEvents(ctx context.Context, events []RawEvent) error {
	if f.options.RetrievalEncoder == nil || len(events) == 0 {
		return nil
	}
	type spanInput struct {
		event   RawEvent
		ordinal int
		content string
	}
	var inputs []spanInput
	for _, event := range events {
		spans, err := f.options.RetrievalEncoder.Split(event.Content, 512, 64)
		if err != nil || len(spans) == 0 {
			spans = []string{event.Content}
		}
		for ordinal, content := range spans {
			if strings.TrimSpace(content) != "" {
				inputs = append(inputs, spanInput{event: event, ordinal: ordinal, content: content})
			}
		}
	}
	batchSize := f.options.EmbeddingBatchSize
	if batchSize <= 0 || batchSize > 20 {
		batchSize = 20
	}
	encodings := make([]RetrievalEncoding, 0, len(inputs))
	for start := 0; start < len(inputs); start += batchSize {
		end := minIntMemory(len(inputs), start+batchSize)
		texts := make([]string, 0, end-start)
		for _, input := range inputs[start:end] {
			texts = append(texts, input.content)
		}
		encoded, err := f.options.RetrievalEncoder.Encode(ctx, texts, RetrievalDocument)
		if err != nil {
			return err
		}
		if len(encoded) != len(texts) {
			return fmt.Errorf("retrieval encoder returned %d rows for %d spans", len(encoded), len(texts))
		}
		encodings = append(encodings, encoded...)
	}
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	seenEvents := map[string]struct{}{}
	for index, input := range inputs {
		encoding := encodings[index]
		if len(encoding.Dense) != postgresFabricDimensions {
			return fmt.Errorf("retrieval encoder returned %d dense dimensions, want %d",
				len(encoding.Dense), postgresFabricDimensions)
		}
		if _, exists := seenEvents[input.event.ID]; !exists {
			if _, err := tx.Exec(ctx, `DELETE FROM lumina_memory_spans WHERE cluster_id=$1 AND tenant_id=$2
				AND project_id=$3 AND space=$4 AND event_id=$5`, f.clusterID, f.tenantID, f.projectID,
				input.event.Space, input.event.ID); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM lumina_memory_sparse_postings WHERE cluster_id=$1 AND tenant_id=$2
				AND project_id=$3 AND space=$4 AND event_id=$5`, f.clusterID, f.tenantID, f.projectID,
				input.event.Space, input.event.ID); err != nil {
				return err
			}
			seenEvents[input.event.ID] = struct{}{}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_spans(
			cluster_id,tenant_id,project_id,space,event_id,ordinal,content,embedding,source_ref,model_revision,tokenizer_hash)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11)`, f.clusterID, f.tenantID,
			f.projectID, input.event.Space, input.event.ID, input.ordinal, input.content,
			pgvector.NewHalfVector(encoding.Dense), input.event.SourceRef,
			f.options.RetrievalEncoder.Revision(), f.options.RetrievalEncoder.TokenizerHash()); err != nil {
			return err
		}
		if input.ordinal == 0 {
			for tokenID, weight := range encoding.Sparse {
				if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_sparse_postings(
					cluster_id,tenant_id,project_id,space,event_id,token_id,weight)
					VALUES($1,$2,$3,$4,$5,$6,$7)
					ON CONFLICT(cluster_id,tenant_id,project_id,space,event_id,token_id)
					DO UPDATE SET weight=excluded.weight`,
					f.clusterID, f.tenantID, f.projectID, input.event.Space, input.event.ID, tokenID, weight); err != nil {
					return err
				}
			}
		}
	}
	for eventID := range seenEvents {
		if _, err := tx.Exec(ctx, `UPDATE lumina_memory_events SET updated_at=$1 WHERE cluster_id=$2 AND
			tenant_id=$3 AND project_id=$4 AND event_id=$5`, f.now(), f.clusterID, f.tenantID,
			f.projectID, eventID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

type postgresFabricJob struct {
	ID         string
	Kind       string
	Space      string
	ResourceID string
	Payload    json.RawMessage
	Attempts   int
}

func (f *PostgresFabric) enqueueJobTx(ctx context.Context, tx pgx.Tx, kind, space, resourceID string,
	payload any, availableAt time.Time) error {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	jobID := f.postgresJobID(kind, space, resourceID, encoded)
	_, err = tx.Exec(ctx, `INSERT INTO lumina_memory_jobs(
		cluster_id,tenant_id,project_id,job_id,kind,space,resource_id,payload,status,available_at,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,'pending',$9,$10,$10)
		ON CONFLICT(cluster_id,tenant_id,project_id,job_id) DO UPDATE SET
		status=CASE WHEN lumina_memory_jobs.status IN ('running','done') THEN lumina_memory_jobs.status ELSE 'pending' END,
		payload=excluded.payload,available_at=LEAST(lumina_memory_jobs.available_at,excluded.available_at),
		updated_at=excluded.updated_at`, f.clusterID, f.tenantID, f.projectID, jobID, kind,
		normalizeSpace(space), resourceID, encoded, availableAt, f.now())
	return err
}

func (f *PostgresFabric) postgresJobID(kind, space, resourceID string, payload []byte) string {
	return stableFabricID("job", f.tenantID, f.projectID, kind, normalizeSpace(space), resourceID,
		contentHash(string(payload)))
}

func (f *PostgresFabric) prepareDerivedJobs(ctx context.Context) error {
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	now := f.now()
	if f.options.RetrievalEncoder != nil {
		rows, err := tx.Query(ctx, `SELECT e.space,e.event_id FROM lumina_memory_events e WHERE
			e.cluster_id=$1 AND e.tenant_id=$2 AND e.project_id=$3 AND e.tombstoned=false AND NOT EXISTS(
			SELECT 1 FROM lumina_memory_spans s WHERE s.cluster_id=e.cluster_id AND s.tenant_id=e.tenant_id
			AND s.project_id=e.project_id AND s.space=e.space AND s.event_id=e.event_id
			AND s.model_revision=$4 AND s.tokenizer_hash=$5)`, f.clusterID, f.tenantID, f.projectID,
			f.options.RetrievalEncoder.Revision(), f.options.RetrievalEncoder.TokenizerHash())
		if err != nil {
			return err
		}
		var pending [][2]string
		for rows.Next() {
			var space, eventID string
			if err := rows.Scan(&space, &eventID); err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, [2]string{space, eventID})
		}
		rows.Close()
		for _, item := range pending {
			if err := f.enqueueJobTx(ctx, tx, "index_event", item[0], item[1],
				map[string]any{"event_id": item[1], "revision": f.options.RetrievalEncoder.Revision(),
					"tokenizer": f.options.RetrievalEncoder.TokenizerHash()}, now); err != nil {
				return err
			}
		}
	}
	if f.options.Vectorizer != nil {
		rows, err := tx.Query(ctx, `SELECT space,doc_id FROM lumina_memory_documents WHERE cluster_id=$1
			AND tenant_id=$2 AND project_id=$3 AND resource_kind='node'
			AND (embedding IS NULL OR COALESCE(embedding_model,'')<>$4)`, f.clusterID, f.tenantID,
			f.projectID, f.options.Vectorizer.Model())
		if err != nil {
			return err
		}
		var pending [][2]string
		for rows.Next() {
			var space, nodeID string
			if err := rows.Scan(&space, &nodeID); err != nil {
				rows.Close()
				return err
			}
			pending = append(pending, [2]string{space, nodeID})
		}
		rows.Close()
		for _, item := range pending {
			if err := f.enqueueJobTx(ctx, tx, "embed_node", item[0], item[1],
				map[string]any{"node_id": item[1], "model": f.options.Vectorizer.Model()}, now); err != nil {
				return err
			}
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_index_state(
		cluster_id,tenant_id,project_id,key,value,updated_at) VALUES($1,$2,$3,'retrieval_contract',$4,$5)
		ON CONFLICT(cluster_id,tenant_id,project_id,key) DO UPDATE SET value=excluded.value,updated_at=excluded.updated_at`,
		f.clusterID, f.tenantID, f.projectID, f.retrievalContract(), now); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (f *PostgresFabric) retrievalContract() string {
	parts := []string{"schema=3"}
	if f.options.RetrievalEncoder != nil {
		parts = append(parts, "retrieval="+f.options.RetrievalEncoder.Revision(),
			"tokenizer="+f.options.RetrievalEncoder.TokenizerHash())
	}
	if f.options.Vectorizer != nil {
		parts = append(parts, "semantic="+f.options.Vectorizer.Model())
	}
	return strings.Join(parts, "|")
}

func (f *PostgresFabric) enqueueJob(ctx context.Context, kind, space, resourceID string,
	payload any, availableAt time.Time) (JobRef, error) {
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return JobRef{}, err
	}
	defer tx.Rollback(ctx)
	if err := f.enqueueJobTx(ctx, tx, kind, space, resourceID, payload, availableAt); err != nil {
		return JobRef{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return JobRef{}, err
	}
	f.wakeWorker()
	encoded, _ := json.Marshal(payload)
	return JobRef{ID: f.postgresJobID(kind, space, resourceID, encoded),
		Kind: kind, Status: "pending"}, nil
}

func (f *PostgresFabric) finishJobsByResource(ctx context.Context, kind string, resourceIDs []string) error {
	if len(resourceIDs) == 0 {
		return nil
	}
	_, err := f.pool.Exec(ctx, `UPDATE lumina_memory_jobs SET status='done',lease_owner=NULL,lease_until=NULL,
		updated_at=$1,last_error=NULL WHERE cluster_id=$2 AND tenant_id=$3 AND project_id=$4
		AND kind=$5 AND resource_id=ANY($6)`, f.now(), f.clusterID, f.tenantID, f.projectID, kind, resourceIDs)
	return err
}

func (f *PostgresFabric) recoverJobs(ctx context.Context) error {
	_, err := f.pool.Exec(ctx, `UPDATE lumina_memory_jobs SET status='pending',lease_owner=NULL,lease_until=NULL,
		updated_at=$1 WHERE cluster_id=$2 AND tenant_id=$3 AND project_id=$4 AND status='running'
		AND (lease_until IS NULL OR lease_until<$1)`, f.now(), f.clusterID, f.tenantID, f.projectID)
	return err
}

func (f *PostgresFabric) claimJob(ctx context.Context) (*postgresFabricJob, error) {
	owner := uuid.NewString()
	var job postgresFabricJob
	err := f.pool.QueryRow(ctx, `WITH candidate AS (
		SELECT job_id FROM lumina_memory_jobs WHERE cluster_id=$1 AND tenant_id=$2 AND project_id=$3
		AND ((status='pending' AND available_at<=$4) OR (status='running' AND lease_until<$4))
		ORDER BY available_at,created_at
		FOR UPDATE SKIP LOCKED LIMIT 1
	) UPDATE lumina_memory_jobs j SET status='running',attempts=j.attempts+1,lease_owner=$5,
		lease_until=$6,updated_at=$4 FROM candidate c WHERE j.cluster_id=$1 AND j.tenant_id=$2
		AND j.project_id=$3 AND j.job_id=c.job_id
		RETURNING j.job_id,j.kind,j.space,j.resource_id,j.payload,j.attempts`, f.clusterID, f.tenantID,
		f.projectID, f.now(), owner, f.now().Add(60*time.Second)).Scan(&job.ID, &job.Kind, &job.Space,
		&job.ResourceID, &job.Payload, &job.Attempts)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &job, err
}

func (f *PostgresFabric) finishJob(ctx context.Context, job postgresFabricJob, executeErr error) error {
	status := "done"
	availableAt := f.now()
	message := ""
	if executeErr != nil {
		status = "pending"
		message = truncateMemoryError(executeErr.Error())
		backoff := time.Duration(1<<minIntMemory(job.Attempts, 6)) * time.Second
		availableAt = availableAt.Add(backoff)
	}
	_, err := f.pool.Exec(ctx, `UPDATE lumina_memory_jobs SET status=$1,available_at=$2,lease_owner=NULL,
		lease_until=NULL,last_error=NULLIF($3,''),updated_at=$4 WHERE cluster_id=$5 AND tenant_id=$6
		AND project_id=$7 AND job_id=$8`, status, availableAt, message, f.now(), f.clusterID,
		f.tenantID, f.projectID, job.ID)
	return err
}

func (f *PostgresFabric) processJob(ctx context.Context, job postgresFabricJob) error {
	switch job.Kind {
	case "index_event":
		events, err := f.loadEvents(ctx, job.Space, []string{job.ResourceID})
		if err != nil {
			return err
		}
		if len(events) == 0 {
			return nil
		}
		return f.indexEvents(ctx, events)
	case "compile_context":
		return f.compileContext(ctx, job.Space, job.ResourceID)
	case "embed_node":
		return f.embedNode(ctx, job.Space, job.ResourceID)
	case "adjudicate_conflict":
		_, _, err := f.resolveConflict(ctx, job.Space, job.ResourceID)
		return err
	default:
		return fmt.Errorf("unknown PostgreSQL Memory Fabric job kind %q", job.Kind)
	}
}

func (f *PostgresFabric) embedNode(ctx context.Context, space, nodeID string) error {
	if f.options.Vectorizer == nil {
		return nil
	}
	var statement string
	if err := f.pool.QueryRow(ctx, `SELECT statement FROM lumina_memory_nodes WHERE cluster_id=$1
		AND tenant_id=$2 AND project_id=$3 AND space=$4 AND node_id=$5`, f.clusterID, f.tenantID,
		f.projectID, normalizeSpace(space), nodeID).Scan(&statement); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		return err
	}
	vectors, err := f.options.Vectorizer.Embed(ctx, []string{statement}, VectorContent)
	if err != nil {
		return err
	}
	if len(vectors) != 1 || len(vectors[0]) != postgresFabricDimensions {
		return fmt.Errorf("semantic vectorizer returned invalid dimensions for node %s", nodeID)
	}
	_, err = f.pool.Exec(ctx, `UPDATE lumina_memory_documents SET embedding=$1,embedding_model=$2
		WHERE cluster_id=$3 AND tenant_id=$4 AND project_id=$5 AND space=$6 AND doc_id=$7`,
		pgvector.NewVector(vectors[0]), f.options.Vectorizer.Model(), f.clusterID, f.tenantID,
		f.projectID, normalizeSpace(space), nodeID)
	return err
}

func (f *PostgresFabric) processNextWork(ctx context.Context) (bool, error) {
	if f != nil && f.options.WriteGuard != nil {
		if err := f.options.WriteGuard(ctx); err != nil {
			return false, err
		}
	}
	job, err := f.claimJob(ctx)
	if err != nil || job == nil {
		return false, err
	}
	executeErr := f.processJob(ctx, *job)
	finishErr := f.finishJob(context.WithoutCancel(ctx), *job, executeErr)
	if executeErr != nil {
		return true, executeErr
	}
	return true, finishErr
}

func (f *PostgresFabric) startWorkers() {
	f.workerCtx, f.workerCancel = context.WithCancel(context.Background())
	for range f.options.WorkerCount {
		f.workerWG.Add(1)
		go func() {
			defer f.workerWG.Done()
			ticker := time.NewTicker(f.options.WorkerPollInterval)
			defer ticker.Stop()
			for {
				for {
					worked, _ := f.processNextWork(f.workerCtx)
					if !worked {
						break
					}
				}
				select {
				case <-f.workerCtx.Done():
					return
				case <-f.jobWake:
				case <-ticker.C:
				}
			}
		}()
	}
}

func (f *PostgresFabric) wakeWorker() {
	select {
	case f.jobWake <- struct{}{}:
	default:
	}
}

func (f *PostgresFabric) Flush(ctx context.Context) error {
	for {
		worked, err := f.processNextWork(ctx)
		if err != nil {
			return err
		}
		if worked {
			continue
		}
		var pending int
		if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM lumina_memory_jobs WHERE cluster_id=$1
			AND tenant_id=$2 AND project_id=$3 AND status IN ('pending','running')`, f.clusterID,
			f.tenantID, f.projectID).Scan(&pending); err != nil {
			return err
		}
		if pending == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (f *PostgresFabric) Remember(ctx context.Context, request MemoryRequest) (MemoryCommitResult, error) {
	result := MemoryCommitResult{SemanticStatus: SemanticEventDurable}
	if f != nil && f.options.WriteGuard != nil {
		if err := f.options.WriteGuard(ctx); err != nil {
			return result, err
		}
	}
	request.Space = normalizeSpace(request.Space)
	if request.Mode == "" {
		request.Mode = WriteExplicit
	}
	if len(request.Events) > 0 {
		for index := range request.Events {
			if request.Events[index].Space == "" {
				request.Events[index].Space = request.Space
			}
			if request.Events[index].ContextID == "" {
				request.Events[index].ContextID = request.ContextID
			}
		}
		ingested, err := f.AppendEvents(ctx, request.Events, IngestOptions{SemanticPolicy: SemanticDurableOnly})
		result.Durable = ingested.Durable
		result.EventIDs = append(result.EventIDs, ingested.EventIDs...)
		if err != nil {
			var lag IndexLagError
			if !errors.As(err, &lag) {
				return result, err
			}
		}
	}
	request.SourceEventIDs = uniqueStrings(append(request.SourceEventIDs, result.EventIDs...))
	result.Durable = result.Durable || len(request.SourceEventIDs) > 0
	if len(request.SourceEventIDs) == 0 && len(request.Drafts) == 0 {
		return result, errors.New("semantic memory requires source events or drafts")
	}
	if len(request.Drafts) > 0 {
		commit, err := f.commitDrafts(ctx, request, request.Drafts)
		commit.EventIDs = uniqueStrings(append(commit.EventIDs, result.EventIDs...))
		return commit, err
	}
	if f.options.Compiler == nil || f.options.RemoteProcessing == RemoteProcessingOff {
		if request.RequireSemantic {
			return result, errors.New("semantic compiler is unavailable while raw events remain durable")
		}
		job, err := f.enqueueJob(ctx, "compile_context", request.Space,
			firstNonEmptyMemory(request.ContextID, strings.Join(request.SourceEventIDs, ",")),
			map[string]any{"context_id": request.ContextID, "event_ids": request.SourceEventIDs}, f.now())
		result.PendingJobID = job.ID
		result.SemanticStatus = SemanticProposed
		return result, err
	}
	return f.compileEventIDs(ctx, request)
}

func (f *PostgresFabric) compileContext(ctx context.Context, space, contextID string) error {
	rows, err := f.pool.Query(ctx, `SELECT event_id FROM lumina_memory_events WHERE cluster_id=$1
		AND tenant_id=$2 AND project_id=$3 AND space=$4 AND context_id=$5 AND tombstoned=false
		AND semantic_status IN ($6,$7) ORDER BY occurred_at,event_id`, f.clusterID, f.tenantID,
		f.projectID, normalizeSpace(space), contextID, SemanticEventDurable, SemanticProposed)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) == 0 || f.options.Compiler == nil || f.options.RemoteProcessing == RemoteProcessingOff {
		return nil
	}
	_, err = f.compileEventIDs(ctx, MemoryRequest{Space: space, ContextID: contextID,
		SourceEventIDs: ids, Mode: WriteNormal})
	return err
}

func (f *PostgresFabric) compileEventIDs(ctx context.Context,
	request MemoryRequest) (MemoryCommitResult, error) {
	result := MemoryCommitResult{Durable: true, EventIDs: uniqueStrings(request.SourceEventIDs),
		SemanticStatus: SemanticEventDurable}
	events, err := f.loadEvents(ctx, request.Space, request.SourceEventIDs)
	if err != nil {
		return result, err
	}
	if len(events) == 0 {
		return result, errors.New("semantic memory source events do not exist")
	}
	sources := request.CompileSources
	ids := append([]string(nil), request.SourceEventIDs...)
	if len(sources) == 0 {
		plan, planErr := f.options.Planner.Plan(ctx, events, PlanningOptions{Mode: request.Mode,
			MaxSources: f.options.CompileMaxSources, MaxSourcesPerSession: f.options.CompileSourcesPerSession,
			MaxSourceRunes: f.options.CompileSourceRunes})
		if planErr != nil {
			return result, planErr
		}
		ids, sources = candidatesToCompileInput(plan.Candidates)
		if len(plan.SkippedEventIDs) > 0 {
			_, _ = f.pool.Exec(ctx, `UPDATE lumina_memory_events SET semantic_status=$1,updated_at=$2
				WHERE cluster_id=$3 AND tenant_id=$4 AND project_id=$5 AND space=$6 AND event_id=ANY($7)`,
				SemanticSkipped, f.now(), f.clusterID, f.tenantID, f.projectID, normalizeSpace(request.Space),
				plan.SkippedEventIDs)
		}
	}
	if len(sources) == 0 {
		result.SemanticStatus = SemanticSkipped
		return result, nil
	}
	remoteSources := append([]CompileSource(nil), sources...)
	if f.options.RemoteProcessing == RemoteProcessingRedacted {
		for index := range remoteSources {
			remoteSources[index].Text = redactSecrets(remoteSources[index].Text)
		}
	}
	compileRequest := CompileRequest{Mode: request.Mode, Instructions: request.Instructions,
		Sources: remoteSources, MaxInputTokens: f.options.CompileBatchTokens,
		MaxOutputTokens: f.options.CompileOutputTokens, MaxNodes: f.options.CompileMaxNodes}
	response, err := f.options.Compiler.Compile(ctx, compileRequest)
	response.Usage.Calls = maxIntMemory(1, response.Usage.Calls)
	if f.options.UsageObserver != nil {
		observeErr := f.options.UsageObserver(ctx, APIUsageEvent{Stage: APIStageSemanticCompile,
			Space: request.Space, ContextID: request.ContextID, Usage: response.Usage,
			Error: errorStringMemory(err), RecordedAt: f.now()})
		if err == nil && observeErr != nil {
			err = observeErr
		}
	}
	result.Usage = response.Usage
	if err != nil {
		return result, err
	}
	boundNodes, boundAliases, _, bindErr := bindCompilerSources(response.Nodes, response.Aliases, sources, ids)
	if bindErr != nil {
		return result, &CompileContractError{Reason: bindErr.Error()}
	}
	request.SourceEventIDs = ids
	if err := f.applyPostgresAliases(ctx, request.Space, boundAliases); err != nil {
		return result, err
	}
	commit, err := f.commitDrafts(ctx, request, boundNodes)
	commit.EventIDs = result.EventIDs
	commit.Usage = response.Usage
	if err == nil {
		_, _ = f.pool.Exec(ctx, `UPDATE lumina_memory_events SET semantic_status=$1,updated_at=$2
			WHERE cluster_id=$3 AND tenant_id=$4 AND project_id=$5 AND space=$6 AND event_id=ANY($7)`,
			commit.SemanticStatus, f.now(), f.clusterID, f.tenantID, f.projectID,
			normalizeSpace(request.Space), ids)
	}
	return commit, err
}

func (f *PostgresFabric) applyPostgresAliases(ctx context.Context, space string,
	proposals []IdentityAliasProposal) error {
	space = normalizeSpace(space)
	for _, proposal := range proposals {
		canonical := normalizeKey(proposal.Canonical)
		if canonical == "" || len(proposal.Aliases) == 0 {
			continue
		}
		identityType := normalizeKey(proposal.Type)
		identityID := stableFabricID("idn", space, identityType, canonical)
		tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_identities(
			cluster_id,tenant_id,project_id,space,identity_id,canonical,identity_type,display_name,status,created_at)
			VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,'active',$9) ON CONFLICT DO NOTHING`,
			f.clusterID, f.tenantID, f.projectID, space, identityID, canonical, identityType,
			strings.TrimSpace(proposal.Canonical), f.now()); err != nil {
			tx.Rollback(ctx)
			return err
		}
		sourceID := ""
		if len(proposal.Sources) > 0 {
			sourceID = proposal.Sources[0].EventID
		}
		for _, alias := range append(proposal.Aliases, proposal.Canonical) {
			normalized := normalizeClaim(alias)
			if normalized == "" {
				continue
			}
			if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_identity_aliases(
				cluster_id,tenant_id,project_id,space,normalized_alias,identity_id,source_event_id,method,status,created_at)
				VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),'compiler_grounded','active',$8)
				ON CONFLICT DO NOTHING`, f.clusterID, f.tenantID, f.projectID, space, normalized,
				identityID, sourceID, f.now()); err != nil {
				tx.Rollback(ctx)
				return err
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (f *PostgresFabric) commitDrafts(ctx context.Context, request MemoryRequest,
	drafts []MemoryDraft) (MemoryCommitResult, error) {
	result := MemoryCommitResult{Durable: true, EventIDs: uniqueStrings(request.SourceEventIDs),
		SemanticStatus: SemanticActive}
	events, err := f.loadEvents(ctx, request.Space, request.SourceEventIDs)
	if err != nil {
		return result, err
	}
	eventByID := make(map[string]RawEvent, len(events))
	for _, event := range events {
		eventByID[event.ID] = event
	}
	invalid := 0
	for _, draft := range drafts {
		node, normalizeErr := f.normalizeDraft(ctx, request, draft, eventByID)
		if normalizeErr != nil {
			invalid++
			_ = f.persistPostgresQuarantine(ctx, request, draft, normalizeErr)
			if request.RequireSemantic {
				return result, normalizeErr
			}
			continue
		}
		tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{})
		if err != nil {
			return result, err
		}
		if node.SlotID != "" {
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`,
				f.clusterID+"\x1f"+f.tenantID+"\x1f"+f.projectID+"\x1fslot\x1f"+node.SlotID); err != nil {
				tx.Rollback(ctx)
				return result, err
			}
		}
		var existingJSON []byte
		existingNodes := []MemoryNode{}
		if node.SlotID != "" {
			rows, queryErr := tx.Query(ctx, `SELECT node FROM lumina_memory_nodes WHERE cluster_id=$1
				AND tenant_id=$2 AND project_id=$3 AND space=$4 AND slot_id=$5
				AND status IN ($6,$7,$8,$9)`, f.clusterID, f.tenantID, f.projectID, node.Space,
				node.SlotID, SemanticActive, SemanticScoped, SemanticPendingResolution, SemanticGrounded)
			if queryErr != nil {
				tx.Rollback(ctx)
				return result, queryErr
			}
			for rows.Next() {
				if err := rows.Scan(&existingJSON); err != nil {
					rows.Close()
					tx.Rollback(ctx)
					return result, err
				}
				var existing MemoryNode
				if json.Unmarshal(existingJSON, &existing) == nil && existing.ID != node.ID {
					existingNodes = append(existingNodes, existing)
				}
			}
			rows.Close()
		}
		decision := DecisionCoexists
		winner := ""
		var conflictingNodes []MemoryNode
		if node.SlotID != "" {
			existingEvents := map[string]map[string]RawEvent{}
			for _, existing := range existingNodes {
				loaded, loadErr := f.loadPostgresNodeEventsTx(ctx, tx, node.Space, existing.ID)
				if loadErr != nil {
					tx.Rollback(ctx)
					return result, loadErr
				}
				existingEvents[existing.ID] = loaded
			}
			decision, winner, conflictingNodes = localSlotDecision(node, existingNodes, eventByID,
				func(id string) map[string]RawEvent { return existingEvents[id] })
		}
		switch decision {
		case DecisionDuplicate:
			node.Status = SemanticRejected
			if winner != "" {
				result.MemoryIDs = append(result.MemoryIDs, winner)
			}
		case DecisionSupersedes, DecisionCoexists, DecisionScoped:
			node.Status = SemanticActive
		default:
			node.Status = SemanticPendingResolution
		}
		nodeJSON, _ := json.Marshal(node)
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_nodes(
			cluster_id,tenant_id,project_id,space,node_id,context_id,slot_id,status,statement,node,
			content_hash,created_at,updated_at) VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),
			$8,$9,$10,$11,$12,$12) ON CONFLICT DO NOTHING`, f.clusterID, f.tenantID, f.projectID,
			node.Space, node.ID, node.ContextID, node.SlotID, node.Status, node.Statement, nodeJSON,
			contentHash(node.Statement, claimValueKey(node.Value)), node.CreatedAt); err != nil {
			tx.Rollback(ctx)
			return result, err
		}
		if err := f.persistPostgresNodeAuxTx(ctx, tx, node, draft.SubjectType); err != nil {
			tx.Rollback(ctx)
			return result, err
		}
		for _, source := range node.Sources {
			if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_node_sources(
				cluster_id,tenant_id,project_id,space,node_id,event_id,start_rune,end_rune,source_role)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,'')) ON CONFLICT DO NOTHING`, f.clusterID,
				f.tenantID, f.projectID, node.Space, node.ID, source.EventID, source.StartRune,
				source.EndRune, source.Role); err != nil {
				tx.Rollback(ctx)
				return result, err
			}
		}
		sourceIDs := sourceSpanEventIDs(node.Sources)
		sourceJSON, _ := json.Marshal(sourceIDs)
		metadataJSON, _ := json.Marshal(map[string]any{"slot_id": node.SlotID})
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_documents(
			cluster_id,tenant_id,project_id,space,doc_id,resource_kind,resource_id,context_id,status,
			content,source_event_ids,metadata) VALUES($1,$2,$3,$4,$5,'node',$5,NULLIF($6,''),$7,$8,$9,$10)
			ON CONFLICT(cluster_id,tenant_id,project_id,space,doc_id) DO UPDATE SET
			status=excluded.status,content=excluded.content,source_event_ids=excluded.source_event_ids`,
			f.clusterID, f.tenantID, f.projectID, node.Space, node.ID, node.ContextID, node.Status,
			node.Statement, sourceJSON, metadataJSON); err != nil {
			tx.Rollback(ctx)
			return result, err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_outbox(
			cluster_id,tenant_id,project_id,space,resource_kind,resource_id,operation,status,created_at,updated_at)
			VALUES($1,$2,$3,$4,'node',$5,'upsert','done',$6,$6)
			ON CONFLICT(cluster_id,tenant_id,project_id,space,resource_kind,resource_id,operation)
			DO UPDATE SET status='done',updated_at=excluded.updated_at`, f.clusterID, f.tenantID,
			f.projectID, node.Space, node.ID, f.now()); err != nil {
			tx.Rollback(ctx)
			return result, err
		}
		if f.options.Vectorizer != nil {
			if err := f.enqueueJobTx(ctx, tx, "embed_node", node.Space, node.ID,
				map[string]any{"node_id": node.ID}, f.now()); err != nil {
				tx.Rollback(ctx)
				return result, err
			}
		}
		if decision == DecisionSupersedes {
			members := append([]MemoryNode{node}, conflictingNodes...)
			conflict, resolution, err := f.createLocalPostgresResolutionTx(ctx, tx, members, node.ID)
			if err != nil {
				tx.Rollback(ctx)
				return result, err
			}
			result.ConflictIDs = append(result.ConflictIDs, conflict.ID)
			result.ResolutionIDs = append(result.ResolutionIDs, resolution.ID)
			for _, old := range conflictingNodes {
				old.Status = SemanticSuperseded
				oldJSON, _ := json.Marshal(old)
				if _, err := tx.Exec(ctx, `UPDATE lumina_memory_nodes SET status=$1,node=$2,updated_at=$3
					WHERE cluster_id=$4 AND tenant_id=$5 AND project_id=$6 AND space=$7 AND node_id=$8`,
					old.Status, oldJSON, f.now(), f.clusterID, f.tenantID, f.projectID, node.Space,
					old.ID); err != nil {
					tx.Rollback(ctx)
					return result, err
				}
				_, _ = tx.Exec(ctx, `UPDATE lumina_memory_documents SET status=$1 WHERE cluster_id=$2
					AND tenant_id=$3 AND project_id=$4 AND space=$5 AND doc_id=$6`, old.Status,
					f.clusterID, f.tenantID, f.projectID, node.Space, old.ID)
				_, _ = tx.Exec(ctx, `UPDATE lumina_memory_slot_versions SET status=$1 WHERE cluster_id=$2
					AND tenant_id=$3 AND project_id=$4 AND space=$5 AND node_id=$6`, old.Status,
					f.clusterID, f.tenantID, f.projectID, node.Space, old.ID)
			}
		} else if node.Status == SemanticPendingResolution {
			members := append([]MemoryNode{node}, conflictingNodes...)
			conflict, err := f.createConflictTx(ctx, tx, members)
			if err != nil {
				tx.Rollback(ctx)
				return result, err
			}
			result.ConflictIDs = append(result.ConflictIDs, conflict.ID)
			result.SemanticStatus = SemanticPendingResolution
			if f.options.Adjudicator != nil && f.options.RemoteProcessing != RemoteProcessingOff {
				if err := f.enqueueJobTx(ctx, tx, "adjudicate_conflict", node.Space, conflict.ID,
					map[string]any{"conflict_id": conflict.ID}, f.now()); err != nil {
					tx.Rollback(ctx)
					return result, err
				}
			}
		}
		if err := tx.Commit(ctx); err != nil {
			return result, err
		}
		result.MemoryIDs = append(result.MemoryIDs, node.ID)
	}
	result.MemoryIDs = uniqueStrings(result.MemoryIDs)
	result.ConflictIDs = uniqueStrings(result.ConflictIDs)
	result.ResolutionIDs = uniqueStrings(result.ResolutionIDs)
	if len(drafts) > 0 && invalid == len(drafts) {
		result.SemanticStatus = SemanticQuarantined
	}
	f.wakeWorker()
	return result, nil
}

func (f *PostgresFabric) persistPostgresQuarantine(ctx context.Context, request MemoryRequest,
	draft MemoryDraft, cause error) error {
	space := normalizeSpace(request.Space)
	statement := firstNonEmptyMemory(strings.TrimSpace(draft.Statement), "quarantined semantic candidate")
	nodeID := stableFabricID("mem", space, "quarantine", statement,
		marshalJSONArray(request.SourceEventIDs))
	quarantinePayload := map[string]any{"draft": draft, "error": truncateMemoryError(cause.Error())}
	payload, _ := json.Marshal(quarantinePayload)
	node := MemoryNode{ID: nodeID, Space: space, ContextID: request.ContextID, Kind: draft.Kind,
		ClaimType: draft.ClaimType, Statement: statement, Facet: draft.Facet,
		AttributeKey: normalizeKey(draft.AttributeKey), Status: SemanticQuarantined,
		Payload: quarantinePayload, CreatedAt: f.now()}
	nodeJSON, _ := json.Marshal(node)
	_, err := f.pool.Exec(ctx, `INSERT INTO lumina_memory_nodes(
		cluster_id,tenant_id,project_id,space,node_id,context_id,status,statement,node,content_hash,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10,$11,$11) ON CONFLICT DO NOTHING`,
		f.clusterID, f.tenantID, f.projectID, space, nodeID, request.ContextID, SemanticQuarantined,
		statement, nodeJSON, contentHash(statement, string(payload)), f.now())
	return err
}

func (f *PostgresFabric) loadPostgresNodeEventsTx(ctx context.Context, tx pgx.Tx, space,
	nodeID string) (map[string]RawEvent, error) {
	rows, err := tx.Query(ctx, `SELECT e.event_id,e.space,COALESCE(e.context_id,''),COALESCE(e.session_id,''),
		e.actor,COALESCE(e.source_kind,''),e.content,e.occurred_at,COALESCE(e.source_ref,''),e.metadata
		FROM lumina_memory_node_sources s JOIN lumina_memory_events e ON e.cluster_id=s.cluster_id
		AND e.tenant_id=s.tenant_id AND e.project_id=s.project_id AND e.space=s.space AND e.event_id=s.event_id
		WHERE s.cluster_id=$1 AND s.tenant_id=$2 AND s.project_id=$3 AND s.space=$4 AND s.node_id=$5`,
		f.clusterID, f.tenantID, f.projectID, normalizeSpace(space), nodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]RawEvent{}
	for rows.Next() {
		var event RawEvent
		var metadata []byte
		if err := rows.Scan(&event.ID, &event.Space, &event.ContextID, &event.SessionID, &event.Actor,
			&event.SourceKind, &event.Content, &event.OccurredAt, &event.SourceRef, &metadata); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(metadata, &event.Metadata)
		result[event.ID] = event
	}
	return result, rows.Err()
}

func (f *PostgresFabric) persistPostgresNodeAuxTx(ctx context.Context, tx pgx.Tx,
	node MemoryNode, identityType string) error {
	if node.SubjectID != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_identities(
			cluster_id,tenant_id,project_id,space,identity_id,canonical,identity_type,display_name,status,created_at)
			VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8,'active',$9) ON CONFLICT DO NOTHING`, f.clusterID,
			f.tenantID, f.projectID, node.Space, node.SubjectID, normalizeKey(node.Subject),
			normalizeKey(identityType), node.Subject, node.CreatedAt); err != nil {
			return err
		}
		if node.Subject != "" {
			if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_identity_aliases(
				cluster_id,tenant_id,project_id,space,normalized_alias,identity_id,method,status,created_at)
				VALUES($1,$2,$3,$4,$5,$6,'node','active',$7) ON CONFLICT DO NOTHING`, f.clusterID,
				f.tenantID, f.projectID, node.Space, normalizeClaim(node.Subject), node.SubjectID,
				node.CreatedAt); err != nil {
				return err
			}
		}
	}
	if node.SlotID != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_slots(
			cluster_id,tenant_id,project_id,space,slot_id,subject_identity_id,facet,attribute_key,scope_key,created_at)
			VALUES($1,$2,$3,$4,$5,NULLIF($6,''),$7,$8,$9,$10) ON CONFLICT DO NOTHING`, f.clusterID,
			f.tenantID, f.projectID, node.Space, node.SlotID, node.SubjectID, node.Facet,
			node.AttributeKey, node.ScopeKey, node.CreatedAt); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_slot_versions(
			cluster_id,tenant_id,project_id,space,slot_id,node_id,valid_from,valid_until,status,created_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING`, f.clusterID,
			f.tenantID, f.projectID, node.Space, node.SlotID, node.ID, nullTime(node.ValidFrom),
			nullTime(node.ValidUntil), node.Status, node.CreatedAt); err != nil {
			return err
		}
	}
	if node.Kind == NodeClaim {
		value, _ := json.Marshal(node.Value)
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_claim_values VALUES($1,$2,$3,$4,$5,$6)
			ON CONFLICT(cluster_id,tenant_id,project_id,space,node_id) DO UPDATE SET value=excluded.value`,
			f.clusterID, f.tenantID, f.projectID, node.Space, node.ID, value); err != nil {
			return err
		}
	}
	for _, key := range node.Keys {
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_node_keys VALUES($1,$2,$3,$4,$5,'anchor',$6)
			ON CONFLICT DO NOTHING`, f.clusterID, f.tenantID, f.projectID, node.Space, node.ID, key); err != nil {
			return err
		}
	}
	for _, cue := range node.RetrievalCues {
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_node_keys VALUES($1,$2,$3,$4,$5,'cue',$6)
			ON CONFLICT DO NOTHING`, f.clusterID, f.tenantID, f.projectID, node.Space, node.ID, cue); err != nil {
			return err
		}
	}
	return nil
}

func (f *PostgresFabric) normalizeDraft(ctx context.Context, request MemoryRequest, draft MemoryDraft,
	events map[string]RawEvent) (MemoryNode, error) {
	draft.Statement = strings.TrimSpace(draft.Statement)
	normalizeDraftKind(&draft)
	if !validateNodeKind(draft.Kind) || draft.Statement == "" {
		return MemoryNode{}, errors.New("memory statement is required")
	}
	draft.Keys = normalizeStringList(draft.Keys, 4)
	draft.RetrievalCues = normalizeStringList(draft.RetrievalCues, 4)
	if len(draft.Sources) == 0 {
		for _, eventID := range request.SourceEventIDs {
			if event, ok := events[eventID]; ok {
				draft.Sources = append(draft.Sources, SourceSpan{EventID: eventID,
					SourceRef: event.SourceRef, StartRune: 0, EndRune: len([]rune(event.Content)), Role: "support"})
			}
		}
	}
	normalizedSources := make([]SourceSpan, 0, len(draft.Sources))
	for _, original := range draft.Sources {
		source := original
		event, exists := events[source.EventID]
		if !exists {
			return MemoryNode{}, fmt.Errorf("memory source event %q is not durable in this tenant/space", source.EventID)
		}
		if strings.TrimSpace(source.Text) != "" {
			ranges := findSourceQuoteRanges(event.Content, source.Text)
			if len(ranges) == 0 {
				ranges = findSourceQuoteRanges(redactSecrets(event.Content), source.Text)
			}
			if len(ranges) == 0 {
				return MemoryNode{}, fmt.Errorf("memory source quote for event %q does not occur", source.EventID)
			}
			for _, item := range ranges {
				grounded := source
				grounded.StartRune, grounded.EndRune, grounded.Text = item[0], item[1], ""
				grounded.SourceRef = firstNonEmptyMemory(grounded.SourceRef, event.SourceRef)
				normalizedSources = append(normalizedSources, grounded)
			}
			continue
		}
		var err error
		source, err = normalizeSourceSpanRange(source, event.Content)
		if err != nil {
			return MemoryNode{}, err
		}
		if source.SourceRef == "" {
			source.SourceRef = event.SourceRef
		}
		normalizedSources = append(normalizedSources, source)
	}
	draft.Sources = normalizedSources
	draft = normalizeGroundedDraftProjection(draft, events)
	if draft.Kind == NodeClaim {
		if strings.TrimSpace(draft.Subject) == "" || !validateFacet(draft.Facet) ||
			strings.TrimSpace(draft.AttributeKey) == "" {
			return MemoryNode{}, errors.New("claim subject, facet, and attribute_key are required")
		}
		if draft.ClaimType == "" {
			draft.ClaimType = claimTypeForFacet(draft.Facet)
		}
		if draft.Value.Kind == "" {
			draft.Value = ClaimValue{Kind: ValueText, Text: draft.Statement}
		}
	}
	if draft.EvidenceMode == "" {
		draft.EvidenceMode = EvidenceInferred
	}
	if draft.Value.Kind == ValueNumber && draft.EvidenceMode != EvidenceInferred {
		needle := strconv.FormatFloat(draft.Value.Number, 'g', -1, 64)
		if !sourcesContain(events, draft.Sources, needle) {
			return MemoryNode{}, fmt.Errorf("numeric claim value %s is not grounded", needle)
		}
	}
	if draft.Value.Kind == ValueTime && !draft.Value.Time.IsZero() && draft.EvidenceMode != EvidenceInferred {
		var grounded bool
		draft.Sources, grounded = groundTimeSources(events, draft.Sources, draft.Value.Time)
		if !grounded {
			return MemoryNode{}, fmt.Errorf("time claim value %s is not grounded",
				draft.Value.Time.UTC().Format("2006-01-02"))
		}
	}
	space := normalizeSpace(request.Space)
	identityID := ""
	if strings.TrimSpace(draft.Subject) != "" {
		resolved, err := f.resolvePostgresIdentityID(ctx, space, draft.Subject, draft.SubjectType)
		if err != nil {
			return MemoryNode{}, err
		}
		identityID = resolved
	}
	slotID := ""
	attribute := normalizeKey(draft.AttributeKey)
	if draft.Kind == NodeClaim {
		slotID = stableFabricID("slot", space, identityID, string(draft.Facet), attribute, scopeKey(draft.Scope))
	}
	sourceIDs := sourceSpanEventIDs(draft.Sources)
	sort.Strings(sourceIDs)
	nodeID := stableFabricID("mem", space, string(draft.Kind), slotID, normalizeClaim(draft.Statement),
		claimValueKey(draft.Value), strings.Join(sourceIDs, "\x1f"))
	return MemoryNode{ID: nodeID, Space: space, ContextID: request.ContextID, Kind: draft.Kind,
		ClaimType: draft.ClaimType, Statement: draft.Statement, SubjectID: identityID, Subject: draft.Subject,
		Facet: draft.Facet, AttributeKey: attribute, ScopeKey: scopeKey(draft.Scope), SlotID: slotID,
		Value: draft.Value, ValidFrom: draft.ValidFrom, ValidUntil: draft.ValidUntil,
		EvidenceMode: draft.EvidenceMode, Sources: draft.Sources, Keys: draft.Keys,
		RetrievalCues: draft.RetrievalCues, Payload: draft.Payload, CreatedAt: f.now()}, nil
}

func (f *PostgresFabric) resolvePostgresIdentityID(ctx context.Context, space, subject,
	identityType string) (string, error) {
	normalized := normalizeClaim(subject)
	var identityID string
	err := f.pool.QueryRow(ctx, `SELECT i.identity_id FROM lumina_memory_identity_aliases a
		JOIN lumina_memory_identities i ON i.cluster_id=a.cluster_id AND i.tenant_id=a.tenant_id
		AND i.project_id=a.project_id AND i.space=a.space AND i.identity_id=a.identity_id
		WHERE a.cluster_id=$1 AND a.tenant_id=$2 AND a.project_id=$3 AND a.space=$4
		AND a.normalized_alias=$5 AND a.status='active'
		ORDER BY CASE WHEN COALESCE(i.identity_type,'')=$6 THEN 0 ELSE 1 END LIMIT 1`, f.clusterID,
		f.tenantID, f.projectID, space, normalized, normalizeKey(identityType)).Scan(&identityID)
	if err == nil {
		return identityID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}
	canonical := normalizeKey(subject)
	if canonical == "" {
		canonical = stableFabricID("subject", subject)
	}
	return stableFabricID("idn", space, normalizeKey(identityType), canonical), nil
}

func sourceSpanEventIDs(sources []SourceSpan) []string {
	ids := make([]string, 0, len(sources))
	for _, source := range sources {
		ids = append(ids, source.EventID)
	}
	return uniqueStrings(ids)
}

func (f *PostgresFabric) createConflictTx(ctx context.Context, tx pgx.Tx,
	members []MemoryNode) (Conflict, error) {
	if len(members) < 2 {
		return Conflict{}, errors.New("a conflict requires at least two nodes")
	}
	parts := make([]string, 0, len(members))
	for _, member := range members {
		parts = append(parts, member.ID+"\x1f"+claimValueKey(member.Value))
	}
	sort.Strings(parts)
	generation := contentHash(members[0].SlotID, strings.Join(parts, "\x1e"))
	conflict := Conflict{ID: stableFabricID("conf", members[0].SlotID, generation),
		Space: members[0].Space, SlotID: members[0].SlotID, Generation: generation,
		Status: SemanticPendingResolution, Members: members, CreatedAt: f.now()}
	encoded, _ := json.Marshal(conflict)
	_, err := tx.Exec(ctx, `INSERT INTO lumina_memory_conflicts(
		cluster_id,tenant_id,project_id,space,conflict_id,slot_id,generation,status,conflict,created_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)
		ON CONFLICT(cluster_id,tenant_id,project_id,space,slot_id,generation) DO UPDATE SET
		conflict=excluded.conflict,updated_at=excluded.updated_at`, f.clusterID, f.tenantID, f.projectID,
		conflict.Space, conflict.ID, conflict.SlotID, conflict.Generation, conflict.Status, encoded, f.now())
	return conflict, err
}

func (f *PostgresFabric) createLocalPostgresResolutionTx(ctx context.Context, tx pgx.Tx,
	members []MemoryNode, winnerID string) (Conflict, Resolution, error) {
	conflict, err := f.createConflictTx(ctx, tx, members)
	if err != nil {
		return Conflict{}, Resolution{}, err
	}
	memberIDs := conflictMemberIDs(conflict)
	losers := make([]string, 0, len(memberIDs))
	for _, id := range memberIDs {
		if id != winnerID {
			losers = append(losers, id)
		}
	}
	resolution := Resolution{ID: stableFabricID("res", conflict.ID, conflict.Generation,
		string(DecisionSupersedes), winnerID), ConflictID: conflict.ID, Generation: conflict.Generation,
		Decision: DecisionSupersedes, WinnerIDs: []string{winnerID}, LoserIDs: losers,
		SupportIDs: conflictSourceIDs(conflict), Reason: "deterministic source authority and temporal ordering",
		PolicyID: localResolutionPolicy, CreatedAt: f.now()}
	encoded, _ := json.Marshal(resolution)
	if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_resolutions(
		cluster_id,tenant_id,project_id,space,resolution_id,conflict_id,generation,resolution,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, f.clusterID, f.tenantID,
		f.projectID, conflict.Space, resolution.ID, conflict.ID, conflict.Generation, encoded,
		resolution.CreatedAt); err != nil {
		return Conflict{}, Resolution{}, err
	}
	conflict.Status = SemanticActive
	conflictJSON, _ := json.Marshal(conflict)
	if _, err := tx.Exec(ctx, `UPDATE lumina_memory_conflicts SET status=$1,conflict=$2,updated_at=$3
		WHERE cluster_id=$4 AND tenant_id=$5 AND project_id=$6 AND space=$7 AND conflict_id=$8`,
		conflict.Status, conflictJSON, f.now(), f.clusterID, f.tenantID, f.projectID,
		conflict.Space, conflict.ID); err != nil {
		return Conflict{}, Resolution{}, err
	}
	return conflict, resolution, nil
}

func (f *PostgresFabric) loadConflict(ctx context.Context, space, conflictID string) (Conflict, error) {
	var encoded []byte
	err := f.pool.QueryRow(ctx, `SELECT conflict FROM lumina_memory_conflicts WHERE cluster_id=$1
		AND tenant_id=$2 AND project_id=$3 AND space=$4 AND conflict_id=$5`, f.clusterID,
		f.tenantID, f.projectID, normalizeSpace(space), conflictID).Scan(&encoded)
	if err != nil {
		return Conflict{}, err
	}
	var conflict Conflict
	err = json.Unmarshal(encoded, &conflict)
	return conflict, err
}

func (f *PostgresFabric) resolveConflict(ctx context.Context, space, conflictID string) (Resolution, APIUsage, error) {
	connection, err := f.pool.Acquire(ctx)
	if err != nil {
		return Resolution{}, APIUsage{}, err
	}
	defer connection.Release()
	lockKey := f.clusterID + "\x1f" + f.tenantID + "\x1f" + f.projectID + "\x1fconflict\x1f" + conflictID
	if _, err := connection.Exec(ctx, `SELECT pg_advisory_lock(hashtext($1))`, lockKey); err != nil {
		return Resolution{}, APIUsage{}, err
	}
	defer connection.Exec(context.Background(), `SELECT pg_advisory_unlock(hashtext($1))`, lockKey)
	conflict, err := f.loadConflict(ctx, space, conflictID)
	if err != nil {
		return Resolution{}, APIUsage{}, err
	}
	var existingJSON []byte
	err = connection.QueryRow(ctx, `SELECT resolution FROM lumina_memory_resolutions WHERE cluster_id=$1
		AND tenant_id=$2 AND project_id=$3 AND space=$4 AND conflict_id=$5 AND generation=$6
		ORDER BY created_at DESC LIMIT 1`, f.clusterID, f.tenantID, f.projectID, conflict.Space,
		conflict.ID, conflict.Generation).Scan(&existingJSON)
	if err == nil {
		var existing Resolution
		if json.Unmarshal(existingJSON, &existing) == nil {
			return existing, APIUsage{}, nil
		}
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return Resolution{}, APIUsage{}, err
	}
	if f.options.Adjudicator == nil || f.options.RemoteProcessing == RemoteProcessingOff {
		return Resolution{}, APIUsage{}, errors.New("conflict adjudicator is unavailable")
	}
	response, err := f.options.Adjudicator.Adjudicate(ctx, AdjudicationRequest{Conflict: conflict,
		AuthorityPolicy: authorityPolicyForConflict(conflict)})
	usage := response.Usage
	usage.Calls = maxIntMemory(1, usage.Calls)
	if f.options.UsageObserver != nil {
		observeErr := f.options.UsageObserver(ctx, APIUsageEvent{Stage: APIStageConflictAdjudication,
			Space: conflict.Space, ResourceID: conflict.ID, Usage: usage, Error: errorStringMemory(err),
			RecordedAt: f.now()})
		if err == nil && observeErr != nil {
			err = observeErr
		}
	}
	if err != nil {
		return Resolution{}, usage, err
	}
	response, err = validateAdjudication(conflict, response)
	if err != nil {
		return Resolution{}, usage, err
	}
	resolution := Resolution{ID: stableFabricID("res", conflict.ID, conflict.Generation,
		string(response.Decision), strings.Join(response.WinnerIDs, ",")), ConflictID: conflict.ID,
		Generation: conflict.Generation, Decision: response.Decision, WinnerIDs: response.WinnerIDs,
		LoserIDs: response.LoserIDs, Conditions: response.Conditions, ValidFrom: response.ValidFrom,
		ValidUntil: response.ValidUntil, SupportIDs: response.SupportIDs, Reason: response.Reason,
		PolicyID: apiResolutionPolicy, CreatedAt: f.now()}
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Resolution{}, usage, err
	}
	defer tx.Rollback(ctx)
	encoded, _ := json.Marshal(resolution)
	if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_resolutions(
		cluster_id,tenant_id,project_id,space,resolution_id,conflict_id,generation,resolution,created_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9) ON CONFLICT DO NOTHING`, f.clusterID, f.tenantID,
		f.projectID, conflict.Space, resolution.ID, conflict.ID, conflict.Generation, encoded,
		resolution.CreatedAt); err != nil {
		return Resolution{}, usage, err
	}
	winners := map[string]struct{}{}
	for _, id := range resolution.WinnerIDs {
		winners[id] = struct{}{}
	}
	for _, member := range conflict.Members {
		status := SemanticSuperseded
		if _, ok := winners[member.ID]; ok || response.Decision == DecisionCoexists {
			status = SemanticActive
		}
		member.Status = status
		nodeJSON, _ := json.Marshal(member)
		if _, err := tx.Exec(ctx, `UPDATE lumina_memory_nodes SET status=$1,node=$2,updated_at=$3
			WHERE cluster_id=$4 AND tenant_id=$5 AND project_id=$6 AND space=$7 AND node_id=$8`,
			status, nodeJSON, f.now(), f.clusterID, f.tenantID, f.projectID, conflict.Space, member.ID); err != nil {
			return Resolution{}, usage, err
		}
		if _, err := tx.Exec(ctx, `UPDATE lumina_memory_documents SET status=$1 WHERE cluster_id=$2
			AND tenant_id=$3 AND project_id=$4 AND space=$5 AND doc_id=$6`, status, f.clusterID,
			f.tenantID, f.projectID, conflict.Space, member.ID); err != nil {
			return Resolution{}, usage, err
		}
	}
	conflict.Status = SemanticActive
	conflictJSON, _ := json.Marshal(conflict)
	if _, err := tx.Exec(ctx, `UPDATE lumina_memory_conflicts SET status=$1,conflict=$2,updated_at=$3
		WHERE cluster_id=$4 AND tenant_id=$5 AND project_id=$6 AND space=$7 AND conflict_id=$8`,
		conflict.Status, conflictJSON, f.now(), f.clusterID, f.tenantID, f.projectID,
		conflict.Space, conflict.ID); err != nil {
		return Resolution{}, usage, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Resolution{}, usage, err
	}
	return resolution, usage, nil
}

func (f *PostgresFabric) PrioritizeConflicts(ctx context.Context,
	selector ConflictSelector) (JobRef, error) {
	if f != nil && f.options.WriteGuard != nil {
		if err := f.options.WriteGuard(ctx); err != nil {
			return JobRef{}, err
		}
	}
	selector.Space = normalizeSpace(selector.Space)
	ids := append([]string(nil), selector.ConflictIDs...)
	if len(selector.SlotIDs) > 0 {
		rows, err := f.pool.Query(ctx, `SELECT conflict_id FROM lumina_memory_conflicts WHERE
			cluster_id=$1 AND tenant_id=$2 AND project_id=$3 AND space=$4 AND slot_id=ANY($5)
			AND status=$6`, f.clusterID, f.tenantID, f.projectID, selector.Space, selector.SlotIDs,
			SemanticPendingResolution)
		if err != nil {
			return JobRef{}, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return JobRef{}, err
			}
			ids = append(ids, id)
		}
		rows.Close()
	}
	ids = uniqueStrings(ids)
	if len(ids) == 0 {
		return JobRef{}, errors.New("no matching conflicts")
	}
	var first JobRef
	for _, id := range ids {
		job, err := f.enqueueJob(ctx, "adjudicate_conflict", selector.Space, id,
			map[string]any{"conflict_id": id}, f.now())
		if err != nil {
			return first, err
		}
		if first.ID == "" {
			first = job
		}
	}
	return first, nil
}

func (f *PostgresFabric) ResolvePendingConflicts(ctx context.Context, space string,
	limit int) (APIUsage, error) {
	if f != nil && f.options.WriteGuard != nil {
		if err := f.options.WriteGuard(ctx); err != nil {
			return APIUsage{}, err
		}
	}
	if limit <= 0 || limit > 100 {
		limit = 8
	}
	rows, err := f.pool.Query(ctx, `SELECT conflict_id FROM lumina_memory_conflicts WHERE
		cluster_id=$1 AND tenant_id=$2 AND project_id=$3 AND space=$4 AND status=$5
		ORDER BY created_at LIMIT $6`, f.clusterID, f.tenantID, f.projectID, normalizeSpace(space),
		SemanticPendingResolution, limit)
	if err != nil {
		return APIUsage{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return APIUsage{}, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var total APIUsage
	for _, id := range ids {
		_, usage, err := f.resolveConflict(ctx, space, id)
		total = mergeAPIUsage(total, usage)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (f *PostgresFabric) Search(ctx context.Context, request SearchRequest) (SearchResult, error) {
	started := time.Now()
	result := SearchResult{}
	request.Space = normalizeSpace(request.Space)
	request.Query = strings.TrimSpace(request.Query)
	if request.Query == "" {
		return result, errors.New("memory search query is required")
	}
	if request.ReferenceTime.IsZero() {
		request.ReferenceTime = f.now()
	}
	if request.MaxEvidence <= 0 {
		request.MaxEvidence = f.options.MaxEvidence
	}
	if request.MaxContextTokens <= 0 {
		request.MaxContextTokens = f.options.TargetContextTokens
	}
	request.MaxContextTokens = minIntMemory(request.MaxContextTokens, f.options.MaxContextTokens)
	searchCtx := ctx
	cancel := func() {}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline && f.options.SearchLatencyBudget > 0 {
		searchCtx, cancel = context.WithTimeout(ctx, f.options.SearchLatencyBudget)
	}
	defer cancel()
	analysis := analyzeMemoryQuery(request.Query)
	result.Diagnostics.QueryTerms = searchTermDiagnostics(analysis)
	candidates := map[string]*rankedCandidate{}

	stage := time.Now()
	fts, ftsErr := f.searchPostgresFTS(searchCtx, request.Space, request.Query, f.options.CandidateLimit)
	if ftsErr != nil && searchCtx.Err() == nil {
		return result, ftsErr
	}
	mergeSearchChannel(candidates, "fts", fts)
	result.Diagnostics.FTSCandidates = len(fts)
	result.Diagnostics.BGEFTSCandidates = len(fts)
	result.Diagnostics.StageLatency = map[string]time.Duration{"fts": time.Since(stage)}
	result.Route = append(result.Route, "postgres-fts")

	var queryEncoding RetrievalEncoding
	if f.options.RetrievalEncoder != nil && searchCtx.Err() == nil {
		stage = time.Now()
		encoded, err := f.options.RetrievalEncoder.Encode(searchCtx, []string{request.Query}, RetrievalQuery)
		if err == nil && len(encoded) == 1 {
			queryEncoding = encoded[0]
			dense, denseErr := f.searchPostgresDense(searchCtx, request.Space, queryEncoding.Dense,
				f.options.CandidateLimit)
			if denseErr == nil {
				mergeSearchChannel(candidates, "dense-window", dense)
				result.Diagnostics.VectorCandidates = len(dense)
				result.Diagnostics.BGEDenseCandidates = len(dense)
			}
			semantic, semanticErr := f.searchPostgresSemanticVectors(searchCtx, request.Space,
				queryEncoding.Dense, f.options.CandidateLimit/2)
			if semanticErr == nil {
				mergeSearchChannel(candidates, "semantic-vector", semantic)
			}
			sparse, sparseErr := f.searchPostgresSparse(searchCtx, request.Space, queryEncoding.Sparse,
				f.options.CandidateLimit)
			if sparseErr == nil {
				mergeSearchChannel(candidates, "sparse", sparse)
				result.Diagnostics.BGESparseCandidates = len(sparse)
			}
			result.Diagnostics.StageLatency["dense_sparse"] = time.Since(stage)
			result.Route = append(result.Route, "pgvector-window", "bge-sparse")
		} else {
			result.Diagnostics.FallbackReason = errorStringMemory(err)
		}
	}

	stage = time.Now()
	ppr, pprErr := f.searchPostgresPPR(searchCtx, request.Space, candidates, f.options.CandidateLimit/2)
	if pprErr == nil && len(ppr) > 0 {
		mergeSearchChannel(candidates, "ppr", ppr)
		result.Diagnostics.PPRCandidates = len(ppr)
		result.Route = append(result.Route, "ppr")
	}
	result.Diagnostics.StageLatency["ppr"] = time.Since(stage)
	for _, candidate := range candidates {
		if candidate.document.ResourceKind == "node" {
			candidate.stateOnly = true
		}
	}

	ordered := orderSearchCandidates(candidates, analysis)
	ordered = filterSearchTimeAndState(ordered, analysis, request.ReferenceTime)
	ordered = prioritizeContextDiversity(ordered)
	if len(ordered) > 0 {
		stage = time.Now()
		expanded, expandedCount, err := f.expandPostgresContexts(searchCtx, request.Space, ordered, 12)
		if err == nil {
			ordered = prioritizeContextDiversity(expanded)
			result.Diagnostics.ContextExpandedEvents = expandedCount
			if expandedCount > 0 {
				result.Route = append(result.Route, "context-expand")
			}
		}
		result.Diagnostics.StageLatency["context_expand"] = time.Since(stage)
	}
	if f.options.RetrievalEncoder != nil && len(ordered) > 0 && searchCtx.Err() == nil {
		stage = time.Now()
		exactCount, batchCount, exactErr := f.applyPostgresExactScores(searchCtx, queryEncoding, ordered)
		if exactErr == nil && exactCount > 0 {
			ordered = orderSearchCandidates(candidateMap(ordered), analysis)
			result.Diagnostics.ExactScoredEvents = exactCount
			result.Diagnostics.ExactScoredSpans = exactCount
			result.Diagnostics.MaxSimCandidates = exactCount
			result.Diagnostics.DocumentEncodeBatches = batchCount
			result.Route = append(result.Route, "bge-exact-maxsim")
		} else if exactErr != nil {
			result.Diagnostics.FallbackReason = "exact-score: " + exactErr.Error()
		}
		result.Diagnostics.StageLatency["exact_score"] = time.Since(stage)
	}
	if f.options.Reranker != nil && len(ordered) > 1 && searchCtx.Err() == nil {
		stage = time.Now()
		limit := minIntMemory(len(ordered), f.options.CandidateLimit)
		texts := make([]string, limit)
		for index := range texts {
			texts[index] = ordered[index].document.Content
		}
		scores, err := f.options.Reranker.Rerank(searchCtx, request.Query, texts)
		if err == nil && len(scores) == limit && finiteScores(scores) {
			indices := make([]int, limit)
			for index := range indices {
				indices[index] = index
			}
			sort.SliceStable(indices, func(i, j int) bool { return scores[indices[i]] > scores[indices[j]] })
			for rank, candidateIndex := range indices {
				ordered[candidateIndex].ranks["reranker"] = rank + 1
				ordered[candidateIndex].channelScores["reranker"] = scores[candidateIndex]
			}
			ordered = orderSearchCandidates(candidateMap(ordered), analysis)
			result.Diagnostics.RerankerCandidates = limit
			result.Diagnostics.RerankerModelRevision = f.options.Reranker.Revision()
			result.Route = append(result.Route, "remote-reranker")
		} else if err != nil {
			result.Diagnostics.FallbackReason = "reranker: " + err.Error()
		}
		result.Diagnostics.StageLatency["reranker"] = time.Since(stage)
	}

	stage = time.Now()
	evidence, nodeIDs, deduplicated := selectSubmodularEvidence(ordered, analysis,
		request.MaxEvidence, request.MaxContextTokens)
	if len(evidence) == 0 && len(ordered) > 0 {
		evidence, nodeIDs, deduplicated = selectEvidence(ordered, analysis,
			request.MaxEvidence, request.MaxContextTokens)
	}
	evidence = orderEvidenceByContextAndTime(evidence)
	result.Evidence = evidence
	result.Diagnostics.Deduplicated = deduplicated
	result.Diagnostics.SelectedSourceEvents = sortedSourceEvents(evidence)
	result.Diagnostics.SelectedContextIDs = selectedEvidenceContextIDs(evidence, ordered)
	for _, item := range evidence {
		result.Diagnostics.EvidenceTokens += maxIntMemory(1, estimateTokens(item.Content))
	}
	result.Diagnostics.StageLatency["selection"] = time.Since(stage)
	nodes, err := f.loadNodes(searchCtx, request.Space, nodeIDs)
	if err != nil {
		return result, err
	}
	result.CurrentView = filterCurrentNodes(nodes, request.ReferenceTime)
	result.Conflicts, _ = f.loadConflictsForNodes(searchCtx, request.Space, result.CurrentView)
	result.Insufficient = memorySearchInsufficient(analysis, result)
	result.Route = uniqueStrings(append(result.Route, "rrf", "submodular-evidence"))
	result.Diagnostics.Route = append([]string(nil), result.Route...)
	result.Diagnostics.Duration = time.Since(started)
	result.Diagnostics.RRFCandidates = len(ordered)
	result.Diagnostics.RetrievalSidecarSchema = "postgres-pgvector-v1"
	if f.options.RetrievalEncoder != nil {
		result.Diagnostics.RetrievalModelRevision = f.options.RetrievalEncoder.Revision()
		result.Diagnostics.RetrievalTokenizerHash = f.options.RetrievalEncoder.TokenizerHash()
	}
	if request.IncludeDiagnostics {
		result.Diagnostics.Candidates = summarizeSearchCandidates(ordered, evidence, 96, 600)
	} else {
		result.Diagnostics = SearchDiagnostics{}
	}
	return result, nil
}

func (f *PostgresFabric) applyPostgresExactScores(ctx context.Context, query RetrievalEncoding,
	candidates []*rankedCandidate) (int, int, error) {
	limit := minIntMemory(len(candidates), f.options.CandidateLimit)
	selected := make([]*rankedCandidate, 0, limit)
	for _, candidate := range candidates {
		if candidate == nil || candidate.stateOnly || candidate.document.ResourceKind != "event" {
			continue
		}
		selected = append(selected, candidate)
		if len(selected) >= limit {
			break
		}
	}
	if len(selected) == 0 {
		return 0, 0, nil
	}
	batchSize := f.options.EmbeddingBatchSize
	if batchSize <= 0 || batchSize > 20 {
		batchSize = 20
	}
	type exactScore struct {
		candidate *rankedCandidate
		score     float64
	}
	scores := make([]exactScore, 0, len(selected))
	batches := 0
	for start := 0; start < len(selected); start += batchSize {
		end := minIntMemory(len(selected), start+batchSize)
		texts := make([]string, 0, end-start)
		for _, candidate := range selected[start:end] {
			texts = append(texts, candidate.document.Content)
		}
		encodings, err := f.options.RetrievalEncoder.Encode(ctx, texts, RetrievalDocument)
		if err != nil {
			return len(scores), batches, err
		}
		if len(encodings) != len(texts) {
			return len(scores), batches, fmt.Errorf("retrieval encoder returned %d rows for %d candidates",
				len(encodings), len(texts))
		}
		batches++
		for index, encoding := range encodings {
			candidate := selected[start+index]
			dense := 0.0
			if len(query.Dense) == len(encoding.Dense) {
				dense = dotFloat32(query.Dense, encoding.Dense)
			}
			sparse := sparseDotProduct(query.Sparse, encoding.Sparse)
			colbert := multiVectorMaxSim(query.Multi, encoding.Multi)
			score := fusedBGEExactScore(dense, sparse, colbert)
			if candidate.channelScores == nil {
				candidate.channelScores = map[string]float64{}
			}
			candidate.channelScores["bge_dense_exact"] = dense
			candidate.channelScores["bge_sparse_exact"] = sparse
			candidate.channelScores["bge_colbert_maxsim"] = colbert
			candidate.channelScores["bge_exact"] = score
			scores = append(scores, exactScore{candidate: candidate, score: score})
		}
	}
	sort.SliceStable(scores, func(i, j int) bool { return scores[i].score > scores[j].score })
	for rank, scored := range scores {
		if scored.candidate.ranks == nil {
			scored.candidate.ranks = map[string]int{}
		}
		scored.candidate.ranks["bge-exact"] = rank + 1
	}
	return len(scores), batches, nil
}

func (f *PostgresFabric) searchPostgresSemanticVectors(ctx context.Context, space string,
	dense []float32, limit int) ([]searchDocument, error) {
	if len(dense) != postgresFabricDimensions || limit <= 0 {
		return nil, nil
	}
	rows, err := f.pool.Query(ctx, `SELECT doc_id,resource_kind,resource_id,content,
		COALESCE(context_id,''),COALESCE(actor,''),occurred_at,COALESCE(status,''),source_event_ids,
		COALESCE(metadata->>'slot_id','')
		FROM lumina_memory_documents WHERE cluster_id=$1 AND tenant_id=$2 AND project_id=$3 AND space=$4
		AND resource_kind='node' AND embedding IS NOT NULL
		ORDER BY embedding <=> $5 LIMIT $6`, f.clusterID, f.tenantID, f.projectID,
		normalizeSpace(space), pgvector.NewVector(dense), postgresSearchLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPostgresDocuments(rows)
}

func finiteScores(scores []float64) bool {
	for _, score := range scores {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return false
		}
	}
	return true
}

func candidateMap(candidates []*rankedCandidate) map[string]*rankedCandidate {
	result := make(map[string]*rankedCandidate, len(candidates))
	for _, candidate := range candidates {
		result[candidate.document.ID] = candidate
	}
	return result
}

func (f *PostgresFabric) searchPostgresFTS(ctx context.Context, space, query string,
	limit int) ([]searchDocument, error) {
	rows, err := f.pool.Query(ctx, `SELECT doc_id,resource_kind,resource_id,content,COALESCE(context_id,''),
		COALESCE(actor,''),occurred_at,COALESCE(status,''),source_event_ids,COALESCE(metadata->>'slot_id','')
		FROM lumina_memory_documents WHERE cluster_id=$1 AND tenant_id=$2 AND project_id=$3 AND space=$4
		AND search_vector @@ plainto_tsquery('simple',$5)
		ORDER BY ts_rank_cd(search_vector,plainto_tsquery('simple',$5)) DESC,occurred_at DESC NULLS LAST
		LIMIT $6`, f.clusterID, f.tenantID, f.projectID, normalizeSpace(space), query, postgresSearchLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPostgresDocuments(rows)
}

func (f *PostgresFabric) searchPostgresDense(ctx context.Context, space string, dense []float32,
	limit int) ([]searchDocument, error) {
	if len(dense) != postgresFabricDimensions {
		return nil, nil
	}
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM lumina_memory_spans WHERE cluster_id=$1
		AND tenant_id=$2 AND project_id=$3 AND space=$4 AND embedding IS NOT NULL`, f.clusterID,
		f.tenantID, f.projectID, normalizeSpace(space)).Scan(&count); err != nil {
		return nil, err
	}
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if count < postgresExactVectorLimit {
		_, _ = tx.Exec(ctx, `SET LOCAL enable_indexscan=off`)
		_, _ = tx.Exec(ctx, `SET LOCAL enable_bitmapscan=off`)
	} else {
		efSearch := maxIntMemory(100, 4*f.options.CandidateLimit)
		_, _ = tx.Exec(ctx, `SELECT set_config('hnsw.ef_search',$1,true)`, strconv.Itoa(efSearch))
	}
	rows, err := tx.Query(ctx, `SELECT s.event_id,'event',s.event_id,s.content,
		COALESCE(e.context_id,''),e.actor,e.occurred_at,e.semantic_status,jsonb_build_array(e.event_id),''
		FROM lumina_memory_spans s JOIN lumina_memory_events e ON
		e.cluster_id=s.cluster_id AND e.tenant_id=s.tenant_id AND e.project_id=s.project_id
		AND e.space=s.space AND e.event_id=s.event_id WHERE s.cluster_id=$1 AND s.tenant_id=$2
		AND s.project_id=$3 AND s.space=$4 AND e.tombstoned=false AND s.embedding IS NOT NULL
		ORDER BY s.embedding <=> $5 LIMIT $6`, f.clusterID, f.tenantID, f.projectID,
		normalizeSpace(space), pgvector.NewHalfVector(dense), postgresSearchLimit(limit)*3)
	if err != nil {
		return nil, err
	}
	documents, err := scanPostgresDocuments(rows)
	rows.Close()
	if err != nil {
		return nil, err
	}
	_ = tx.Commit(ctx)
	seen := map[string]struct{}{}
	unique := make([]searchDocument, 0, minIntMemory(limit, len(documents)))
	for _, document := range documents {
		if _, exists := seen[document.ID]; exists {
			continue
		}
		seen[document.ID] = struct{}{}
		unique = append(unique, document)
		if len(unique) >= limit {
			break
		}
	}
	return unique, nil
}

func (f *PostgresFabric) searchPostgresSparse(ctx context.Context, space string,
	query map[int64]float32, limit int) ([]searchDocument, error) {
	if len(query) == 0 {
		return nil, nil
	}
	tokenIDs := make([]int64, 0, len(query))
	for tokenID := range query {
		tokenIDs = append(tokenIDs, tokenID)
	}
	rows, err := f.pool.Query(ctx, `SELECT e.event_id,'event',e.event_id,e.content,
		COALESCE(e.context_id,''),e.actor,e.occurred_at,e.semantic_status,jsonb_build_array(e.event_id),''
		FROM lumina_memory_sparse_postings p JOIN lumina_memory_events e ON
		e.cluster_id=p.cluster_id AND e.tenant_id=p.tenant_id AND e.project_id=p.project_id
		AND e.space=p.space AND e.event_id=p.event_id WHERE p.cluster_id=$1 AND p.tenant_id=$2
		AND p.project_id=$3 AND p.space=$4 AND p.token_id=ANY($5) AND e.tombstoned=false
		GROUP BY e.event_id,e.content,e.context_id,e.actor,e.occurred_at,e.semantic_status
		ORDER BY SUM(p.weight) DESC LIMIT $6`, f.clusterID, f.tenantID, f.projectID,
		normalizeSpace(space), tokenIDs, postgresSearchLimit(limit))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPostgresDocuments(rows)
}

func scanPostgresDocuments(rows pgx.Rows) ([]searchDocument, error) {
	var result []searchDocument
	for rows.Next() {
		var document searchDocument
		var occurredAt *time.Time
		var status string
		var sources []byte
		if err := rows.Scan(&document.ID, &document.ResourceKind, &document.ResourceID,
			&document.Content, &document.ContextID, &document.Actor, &occurredAt, &status, &sources,
			&document.SlotID); err != nil {
			return nil, err
		}
		if occurredAt != nil {
			document.OccurredAt = occurredAt.UTC()
		}
		document.Status = SemanticStatus(status)
		_ = json.Unmarshal(sources, &document.SourceEventIDs)
		result = append(result, document)
	}
	return result, rows.Err()
}

func postgresSearchLimit(limit int) int {
	if limit <= 0 {
		return 64
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}

func (f *PostgresFabric) searchPostgresPPR(ctx context.Context, space string,
	candidates map[string]*rankedCandidate, limit int) ([]searchDocument, error) {
	if len(candidates) == 0 || limit <= 0 {
		return nil, nil
	}
	seedScores := map[string]float64{}
	for id, candidate := range candidates {
		if candidate != nil && candidate.document.ResourceKind == "event" {
			seedScores[id] = 1
			for _, rank := range candidate.ranks {
				seedScores[id] += 1 / float64(1+rank)
			}
		}
	}
	if len(seedScores) == 0 {
		return nil, nil
	}
	seedIDs := make([]string, 0, len(seedScores))
	for id := range seedScores {
		seedIDs = append(seedIDs, id)
	}
	rows, err := f.pool.Query(ctx, `SELECT source_id,target_id,weight FROM lumina_memory_graph_edges
		WHERE cluster_id=$1 AND tenant_id=$2 AND project_id=$3 AND space=$4
		AND (source_id=ANY($5) OR target_id=ANY($5))`, f.clusterID, f.tenantID, f.projectID,
		normalizeSpace(space), seedIDs)
	if err != nil {
		return nil, err
	}
	graph := map[string]map[string]float64{}
	for rows.Next() {
		var source, target string
		var weight float64
		if err := rows.Scan(&source, &target, &weight); err != nil {
			rows.Close()
			return nil, err
		}
		if graph[source] == nil {
			graph[source] = map[string]float64{}
		}
		graph[source][target] += weight
	}
	rows.Close()
	scores := map[string]float64{}
	totalSeed := 0.0
	for _, score := range seedScores {
		totalSeed += score
	}
	for id, score := range seedScores {
		scores[id] = score / totalSeed
	}
	const damping = 0.85
	for range 12 {
		next := map[string]float64{}
		for id, seed := range seedScores {
			next[id] += (1 - damping) * seed / totalSeed
		}
		for source, score := range scores {
			neighbors := graph[source]
			totalWeight := 0.0
			for _, weight := range neighbors {
				totalWeight += weight
			}
			if totalWeight == 0 {
				continue
			}
			for target, weight := range neighbors {
				next[target] += damping * score * weight / totalWeight
			}
		}
		scores = next
	}
	type scoredID struct {
		id    string
		score float64
	}
	var ranked []scoredID
	for id, score := range scores {
		if _, seed := seedScores[id]; !seed {
			ranked = append(ranked, scoredID{id: id, score: score})
		}
	}
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })
	if len(ranked) > limit {
		ranked = ranked[:limit]
	}
	ids := make([]string, len(ranked))
	for index := range ranked {
		ids[index] = ranked[index].id
	}
	documents, err := f.loadEventDocuments(ctx, space, ids)
	if err != nil {
		return nil, err
	}
	byID := map[string]searchDocument{}
	for _, document := range documents {
		byID[document.ID] = document
	}
	ordered := make([]searchDocument, 0, len(ids))
	for _, id := range ids {
		if document, ok := byID[id]; ok {
			ordered = append(ordered, document)
		}
	}
	return ordered, nil
}

func (f *PostgresFabric) loadEventDocuments(ctx context.Context, space string,
	ids []string) ([]searchDocument, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := f.pool.Query(ctx, `SELECT event_id,'event',event_id,content,COALESCE(context_id,''),
		actor,occurred_at,semantic_status,jsonb_build_array(event_id),'' FROM lumina_memory_events
		WHERE cluster_id=$1 AND tenant_id=$2 AND project_id=$3 AND space=$4 AND event_id=ANY($5)
		AND tombstoned=false`, f.clusterID, f.tenantID, f.projectID, normalizeSpace(space), ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPostgresDocuments(rows)
}

func (f *PostgresFabric) expandPostgresContexts(ctx context.Context, space string,
	ordered []*rankedCandidate, maxContexts int) ([]*rankedCandidate, int, error) {
	contexts := []string{}
	seenContexts := map[string]struct{}{}
	for _, candidate := range ordered {
		if candidate.document.ContextID == "" {
			continue
		}
		if _, seen := seenContexts[candidate.document.ContextID]; seen {
			continue
		}
		seenContexts[candidate.document.ContextID] = struct{}{}
		contexts = append(contexts, candidate.document.ContextID)
		if len(contexts) >= maxContexts {
			break
		}
	}
	if len(contexts) == 0 {
		return ordered, 0, nil
	}
	rows, err := f.pool.Query(ctx, `SELECT event_id,'event',event_id,content,COALESCE(context_id,''),
		actor,occurred_at,semantic_status,jsonb_build_array(event_id),'' FROM lumina_memory_events
		WHERE cluster_id=$1 AND tenant_id=$2 AND project_id=$3 AND space=$4 AND context_id=ANY($5)
		AND tombstoned=false ORDER BY context_id,occurred_at,event_id`, f.clusterID, f.tenantID,
		f.projectID, normalizeSpace(space), contexts)
	if err != nil {
		return ordered, 0, err
	}
	documents, err := scanPostgresDocuments(rows)
	rows.Close()
	if err != nil {
		return ordered, 0, err
	}
	byID := candidateMap(ordered)
	added := 0
	for _, document := range documents {
		if _, exists := byID[document.ID]; exists {
			continue
		}
		candidate := &rankedCandidate{document: document, ranks: map[string]int{"context": added + 1},
			channelScores: map[string]float64{"context": 1}, reasons: []string{"context-companion"},
			stateOnly: document.ResourceKind == "node"}
		ordered = append(ordered, candidate)
		byID[document.ID] = candidate
		added++
	}
	return ordered, added, nil
}

func (f *PostgresFabric) loadNodes(ctx context.Context, space string, ids []string) ([]MemoryNode, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := f.pool.Query(ctx, `SELECT node FROM lumina_memory_nodes WHERE cluster_id=$1 AND
		tenant_id=$2 AND project_id=$3 AND space=$4 AND node_id=ANY($5)`, f.clusterID, f.tenantID,
		f.projectID, normalizeSpace(space), ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []MemoryNode
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var node MemoryNode
		if err := json.Unmarshal(encoded, &node); err != nil {
			return nil, err
		}
		result = append(result, node)
	}
	return result, rows.Err()
}

func (f *PostgresFabric) loadConflictsForNodes(ctx context.Context, space string,
	nodes []MemoryNode) ([]Conflict, error) {
	slots := []string{}
	for _, node := range nodes {
		if node.SlotID != "" {
			slots = append(slots, node.SlotID)
		}
	}
	slots = uniqueStrings(slots)
	if len(slots) == 0 {
		return nil, nil
	}
	rows, err := f.pool.Query(ctx, `SELECT conflict FROM lumina_memory_conflicts WHERE cluster_id=$1
		AND tenant_id=$2 AND project_id=$3 AND space=$4 AND slot_id=ANY($5) AND status=$6`,
		f.clusterID, f.tenantID, f.projectID, normalizeSpace(space), slots, SemanticPendingResolution)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Conflict
	for rows.Next() {
		var encoded []byte
		if err := rows.Scan(&encoded); err != nil {
			return nil, err
		}
		var conflict Conflict
		if err := json.Unmarshal(encoded, &conflict); err != nil {
			return nil, err
		}
		result = append(result, conflict)
	}
	return result, rows.Err()
}

func (f *PostgresFabric) loadEvents(ctx context.Context, space string, ids []string) ([]RawEvent, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := f.pool.Query(ctx, `SELECT event_id,space,COALESCE(context_id,''),COALESCE(session_id,''),
		actor,COALESCE(source_kind,''),content,occurred_at,COALESCE(source_ref,''),metadata
		FROM lumina_memory_events WHERE cluster_id=$1 AND tenant_id=$2 AND project_id=$3 AND space=$4
		AND event_id=ANY($5) AND tombstoned=false`, f.clusterID, f.tenantID, f.projectID,
		normalizeSpace(space), ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[string]RawEvent{}
	for rows.Next() {
		var event RawEvent
		var metadata []byte
		if err := rows.Scan(&event.ID, &event.Space, &event.ContextID, &event.SessionID, &event.Actor,
			&event.SourceKind, &event.Content, &event.OccurredAt, &event.SourceRef, &metadata); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(metadata, &event.Metadata)
		byID[event.ID] = event
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]RawEvent, 0, len(ids))
	for _, id := range ids {
		if event, ok := byID[id]; ok {
			result = append(result, event)
		}
	}
	return result, nil
}

func (f *PostgresFabric) SealContext(ctx context.Context, ref ContextRef) (JobRef, error) {
	if f != nil && f.options.WriteGuard != nil {
		if err := f.options.WriteGuard(ctx); err != nil {
			return JobRef{}, err
		}
	}
	ref.Space = normalizeSpace(ref.Space)
	if strings.TrimSpace(ref.ID) == "" {
		return JobRef{}, errors.New("memory context id is required")
	}
	if ref.ClosedAt.IsZero() {
		ref.ClosedAt = f.now()
	}
	_, err := f.pool.Exec(ctx, `INSERT INTO lumina_memory_contexts(
		cluster_id,tenant_id,project_id,space,context_id,parent_id,context_type,label,opened_at,closed_at)
		VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9,$10)
		ON CONFLICT(cluster_id,tenant_id,project_id,space,context_id) DO UPDATE SET
		parent_id=COALESCE(excluded.parent_id,lumina_memory_contexts.parent_id),
		context_type=COALESCE(excluded.context_type,lumina_memory_contexts.context_type),
		label=COALESCE(excluded.label,lumina_memory_contexts.label),closed_at=excluded.closed_at`,
		f.clusterID, f.tenantID, f.projectID, ref.Space, ref.ID, ref.ParentID, ref.Type, ref.Label,
		nullTime(ref.OpenedAt), ref.ClosedAt)
	if err != nil {
		return JobRef{}, err
	}
	return f.enqueueJob(ctx, "compile_context", ref.Space, ref.ID,
		map[string]any{"context_id": ref.ID}, f.now())
}

func nullTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value.UTC()
}

func (f *PostgresFabric) SealImport(ctx context.Context, refs []ContextRef,
	_ ImportPlanningOptions) ([]JobRef, error) {
	jobs := make([]JobRef, 0, len(refs))
	for _, ref := range refs {
		job, err := f.SealContext(ctx, ref)
		if err != nil {
			return jobs, err
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func (f *PostgresFabric) SyncRetrievalSidecar(ctx context.Context) error {
	if f != nil && f.options.WriteGuard != nil {
		if err := f.options.WriteGuard(ctx); err != nil {
			return err
		}
	}
	if f.options.RetrievalEncoder == nil {
		return nil
	}
	rows, err := f.pool.Query(ctx, `SELECT e.event_id,e.space,COALESCE(e.context_id,''),
		COALESCE(e.session_id,''),e.actor,COALESCE(e.source_kind,''),e.content,e.occurred_at,
		COALESCE(e.source_ref,''),e.metadata FROM lumina_memory_events e
		WHERE e.cluster_id=$1 AND e.tenant_id=$2 AND e.project_id=$3 AND e.tombstoned=false
		AND NOT EXISTS(SELECT 1 FROM lumina_memory_spans s WHERE s.cluster_id=e.cluster_id
		AND s.tenant_id=e.tenant_id AND s.project_id=e.project_id AND s.space=e.space
		AND s.event_id=e.event_id AND s.model_revision=$4 AND s.tokenizer_hash=$5)
		ORDER BY e.created_at,e.event_id`, f.clusterID, f.tenantID, f.projectID,
		f.options.RetrievalEncoder.Revision(), f.options.RetrievalEncoder.TokenizerHash())
	if err != nil {
		return err
	}
	defer rows.Close()
	var batch []RawEvent
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if err := f.indexEvents(ctx, batch); err != nil {
			return err
		}
		batch = batch[:0]
		return nil
	}
	for rows.Next() {
		var event RawEvent
		var metadata []byte
		if err := rows.Scan(&event.ID, &event.Space, &event.ContextID, &event.SessionID, &event.Actor,
			&event.SourceKind, &event.Content, &event.OccurredAt, &event.SourceRef, &metadata); err != nil {
			return err
		}
		_ = json.Unmarshal(metadata, &event.Metadata)
		batch = append(batch, event)
		if len(batch) >= 20 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return flush()
}

func (f *PostgresFabric) Forget(ctx context.Context, selector Selector, mode ForgetMode) error {
	if f != nil && f.options.WriteGuard != nil {
		if err := f.options.WriteGuard(ctx); err != nil {
			return err
		}
	}
	selector.Space = normalizeSpace(selector.Space)
	eventIDs := append([]string(nil), selector.EventIDs...)
	if len(selector.ContextIDs) > 0 {
		rows, err := f.pool.Query(ctx, `SELECT event_id FROM lumina_memory_events WHERE cluster_id=$1
			AND tenant_id=$2 AND project_id=$3 AND space=$4 AND context_id=ANY($5)`, f.clusterID,
			f.tenantID, f.projectID, selector.Space, selector.ContextIDs)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			eventIDs = append(eventIDs, id)
		}
		rows.Close()
	}
	eventIDs = uniqueStrings(eventIDs)
	nodeIDs := uniqueStrings(selector.MemoryIDs)
	if len(eventIDs) > 0 {
		rows, err := f.pool.Query(ctx, `SELECT DISTINCT node_id FROM lumina_memory_node_sources WHERE
			cluster_id=$1 AND tenant_id=$2 AND project_id=$3 AND space=$4 AND event_id=ANY($5)`,
			f.clusterID, f.tenantID, f.projectID, selector.Space, eventIDs)
		if err != nil {
			return err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			nodeIDs = append(nodeIDs, id)
		}
		rows.Close()
	}
	nodeIDs = uniqueStrings(nodeIDs)
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if mode == ForgetPurge {
		if len(nodeIDs) > 0 {
			_, _ = tx.Exec(ctx, `DELETE FROM lumina_memory_documents WHERE cluster_id=$1 AND tenant_id=$2
				AND project_id=$3 AND space=$4 AND doc_id=ANY($5)`, f.clusterID, f.tenantID,
				f.projectID, selector.Space, nodeIDs)
			_, _ = tx.Exec(ctx, `DELETE FROM lumina_memory_node_sources WHERE cluster_id=$1 AND tenant_id=$2
				AND project_id=$3 AND space=$4 AND node_id=ANY($5)`, f.clusterID, f.tenantID,
				f.projectID, selector.Space, nodeIDs)
			_, _ = tx.Exec(ctx, `DELETE FROM lumina_memory_nodes WHERE cluster_id=$1 AND tenant_id=$2
				AND project_id=$3 AND space=$4 AND node_id=ANY($5)`, f.clusterID, f.tenantID,
				f.projectID, selector.Space, nodeIDs)
		}
		if len(eventIDs) > 0 {
			for _, table := range []string{"lumina_memory_spans", "lumina_memory_sparse_postings"} {
				if _, err := tx.Exec(ctx, `DELETE FROM `+table+` WHERE cluster_id=$1 AND tenant_id=$2
					AND project_id=$3 AND space=$4 AND event_id=ANY($5)`, f.clusterID, f.tenantID,
					f.projectID, selector.Space, eventIDs); err != nil {
					return err
				}
			}
			_, _ = tx.Exec(ctx, `DELETE FROM lumina_memory_documents WHERE cluster_id=$1 AND tenant_id=$2
				AND project_id=$3 AND space=$4 AND doc_id=ANY($5)`, f.clusterID, f.tenantID,
				f.projectID, selector.Space, eventIDs)
			_, _ = tx.Exec(ctx, `DELETE FROM lumina_memory_graph_edges WHERE cluster_id=$1 AND tenant_id=$2
				AND project_id=$3 AND space=$4 AND (source_id=ANY($5) OR target_id=ANY($5))`,
				f.clusterID, f.tenantID, f.projectID, selector.Space, eventIDs)
			if _, err := tx.Exec(ctx, `DELETE FROM lumina_memory_events WHERE cluster_id=$1 AND tenant_id=$2
				AND project_id=$3 AND space=$4 AND event_id=ANY($5)`, f.clusterID, f.tenantID,
				f.projectID, selector.Space, eventIDs); err != nil {
				return err
			}
		}
	} else {
		if len(eventIDs) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE lumina_memory_events SET tombstoned=true,updated_at=$1
				WHERE cluster_id=$2 AND tenant_id=$3 AND project_id=$4 AND space=$5 AND event_id=ANY($6)`,
				f.now(), f.clusterID, f.tenantID, f.projectID, selector.Space, eventIDs); err != nil {
				return err
			}
			_, _ = tx.Exec(ctx, `DELETE FROM lumina_memory_documents WHERE cluster_id=$1 AND tenant_id=$2
				AND project_id=$3 AND space=$4 AND doc_id=ANY($5)`, f.clusterID, f.tenantID,
				f.projectID, selector.Space, eventIDs)
		}
		if len(nodeIDs) > 0 {
			if _, err := tx.Exec(ctx, `UPDATE lumina_memory_nodes SET status=$1,updated_at=$2 WHERE
				cluster_id=$3 AND tenant_id=$4 AND project_id=$5 AND space=$6 AND node_id=ANY($7)`,
				SemanticTombstoned, f.now(), f.clusterID, f.tenantID, f.projectID, selector.Space,
				nodeIDs); err != nil {
				return err
			}
			_, _ = tx.Exec(ctx, `DELETE FROM lumina_memory_documents WHERE cluster_id=$1 AND tenant_id=$2
				AND project_id=$3 AND space=$4 AND doc_id=ANY($5)`, f.clusterID, f.tenantID,
				f.projectID, selector.Space, nodeIDs)
		}
	}
	return tx.Commit(ctx)
}

func (f *PostgresFabric) Doctor(ctx context.Context) (HealthReport, error) {
	report := HealthReport{Healthy: true, LedgerPath: "postgres:" + f.clusterID + "/" + f.tenantID + "/" + f.projectID,
		IndexPath: "postgres:pgvector", LedgerQuickCheck: "ok", IndexQuickCheck: "ok"}
	if err := f.pool.Ping(ctx); err != nil {
		report.Healthy = false
		report.LedgerQuickCheck = err.Error()
		return report, err
	}
	var schemaVersion int
	if err := f.pool.QueryRow(ctx, `SELECT version FROM lumina_schema_migrations WHERE component='memory'`).
		Scan(&schemaVersion); err != nil {
		report.Healthy = false
		return report, err
	}
	if schemaVersion != postgresFabricSchemaVersion {
		report.Healthy = false
		report.Warnings = append(report.Warnings, fmt.Sprintf("schema version %d, want %d",
			schemaVersion, postgresFabricSchemaVersion))
	}
	if err := f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM lumina_memory_jobs WHERE cluster_id=$1
		AND tenant_id=$2 AND project_id=$3 AND status IN ('pending','running')`, f.clusterID,
		f.tenantID, f.projectID).Scan(&report.PendingJobs); err != nil {
		return report, err
	}
	report.PendingOutbox = 0
	var indexed int64
	_ = f.pool.QueryRow(ctx, `SELECT COUNT(*) FROM lumina_memory_spans WHERE cluster_id=$1
		AND tenant_id=$2 AND project_id=$3`, f.clusterID, f.tenantID, f.projectID).Scan(&indexed)
	report.IndexedLedgerSeq = indexed
	report.IndexGeneration = int64(postgresFabricSchemaVersion)
	return report, nil
}
