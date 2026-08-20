package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"LuminaCode/apppaths"

	"github.com/jackc/pgx/v5"
)

const memoryMigrationMarker = "migrated-to-postgres.json"

type DurableSnapshot struct {
	Version       int                       `json:"version"`
	Space         string                    `json:"space"`
	Contexts      []ContextRef              `json:"contexts"`
	Events        []RawEvent                `json:"events"`
	EventStatuses map[string]SemanticStatus `json:"event_statuses,omitempty"`
	Identities    []DurableIdentity         `json:"identities,omitempty"`
	Nodes         []MemoryNode              `json:"nodes"`
	Conflicts     []Conflict                `json:"conflicts"`
	Resolutions   []Resolution              `json:"resolutions"`
	Checksum      string                    `json:"checksum"`
}

type DurableIdentity struct {
	Identity Identity `json:"identity"`
	Aliases  []string `json:"aliases,omitempty"`
}

type FabricMigrationReport struct {
	MigrationID string `json:"migration_id"`
	TenantID    string `json:"tenant_id"`
	ProjectID   string `json:"project_id"`
	Space       string `json:"space"`
	Events      int    `json:"events"`
	Identities  int    `json:"identities"`
	Nodes       int    `json:"nodes"`
	Conflicts   int    `json:"conflicts"`
	Resolutions int    `json:"resolutions"`
	Checksum    string `json:"checksum"`
	DryRun      bool   `json:"dry_run"`
	Status      string `json:"status"`
}

type DurableSnapshotExporter interface {
	ExportDurableSnapshot(context.Context, string) (DurableSnapshot, error)
}

type DurableSnapshotImporter interface {
	ImportDurableSnapshot(context.Context, DurableSnapshot, string, bool) (FabricMigrationReport, error)
}

func MemoryMigrationMarker(dir string) string { return filepath.Join(dir, memoryMigrationMarker) }

func MarkSQLiteFabricMigrated(dir string, report FabricMigrationReport) error {
	payload, err := json.MarshalIndent(map[string]any{"version": 1, "read_only": true,
		"migration_id": report.MigrationID, "tenant_id": report.TenantID, "project_id": report.ProjectID,
		"space": report.Space, "checksum": report.Checksum,
		"completed_at": time.Now().UTC().Format(time.RFC3339Nano)}, "", "  ")
	if err != nil {
		return err
	}
	return apppaths.WriteFileAtomic(MemoryMigrationMarker(dir), append(payload, '\n'), 0o600)
}

func SQLiteFabricMigrated(dir string) bool {
	_, err := os.Stat(MemoryMigrationMarker(dir))
	return err == nil
}

func (f *Fabric) ExportDurableSnapshot(ctx context.Context, space string) (DurableSnapshot, error) {
	snapshot := DurableSnapshot{Version: 1, Space: normalizeSpace(space),
		EventStatuses: map[string]SemanticStatus{}}
	contextRows, err := f.ledger.QueryContext(ctx, `SELECT context_id,space,parent_id,context_type,label,
		opened_at,closed_at FROM contexts WHERE space=? ORDER BY context_id`, snapshot.Space)
	if err != nil {
		return snapshot, err
	}
	for contextRows.Next() {
		var ref ContextRef
		var opened, closed string
		if err := contextRows.Scan(&ref.ID, &ref.Space, &ref.ParentID, &ref.Type, &ref.Label,
			&opened, &closed); err != nil {
			contextRows.Close()
			return snapshot, err
		}
		ref.OpenedAt, ref.ClosedAt = parseFabricTime(opened), parseFabricTime(closed)
		snapshot.Contexts = append(snapshot.Contexts, ref)
	}
	contextRows.Close()
	eventRows, err := f.ledger.QueryContext(ctx, `SELECT event_id,space,context_id,session_id,actor,
		source_kind,content,occurred_at,source_ref,metadata_json,semantic_status FROM events
		WHERE space=? AND tombstoned=0 ORDER BY occurred_at,event_id`, snapshot.Space)
	if err != nil {
		return snapshot, err
	}
	for eventRows.Next() {
		var event RawEvent
		var occurred, metadata, status string
		if err := eventRows.Scan(&event.ID, &event.Space, &event.ContextID, &event.SessionID, &event.Actor,
			&event.SourceKind, &event.Content, &occurred, &event.SourceRef, &metadata, &status); err != nil {
			eventRows.Close()
			return snapshot, err
		}
		event.OccurredAt = parseFabricTime(occurred)
		_ = json.Unmarshal([]byte(metadata), &event.Metadata)
		snapshot.Events = append(snapshot.Events, event)
		snapshot.EventStatuses[event.ID] = SemanticStatus(status)
	}
	eventRows.Close()
	identityRows, err := f.ledger.QueryContext(ctx, `SELECT identity_id,space,canonical,identity_type,display_name
		FROM identities WHERE space=? ORDER BY identity_id`, snapshot.Space)
	if err != nil {
		return snapshot, err
	}
	for identityRows.Next() {
		var item DurableIdentity
		if err := identityRows.Scan(&item.Identity.ID, &item.Identity.Space, &item.Identity.Canonical,
			&item.Identity.Type, &item.Identity.DisplayName); err != nil {
			identityRows.Close()
			return snapshot, err
		}
		aliasRows, aliasErr := f.ledger.QueryContext(ctx, `SELECT normalized_alias FROM identity_aliases
			WHERE space=? AND identity_id=? AND status='active' ORDER BY normalized_alias`, snapshot.Space,
			item.Identity.ID)
		if aliasErr != nil {
			identityRows.Close()
			return snapshot, aliasErr
		}
		for aliasRows.Next() {
			var alias string
			if aliasRows.Scan(&alias) == nil {
				item.Aliases = append(item.Aliases, alias)
			}
		}
		aliasRows.Close()
		snapshot.Identities = append(snapshot.Identities, item)
	}
	identityRows.Close()
	nodeRows, err := f.ledger.QueryContext(ctx, `SELECT node_id FROM memory_nodes WHERE space=? AND tombstoned=0 ORDER BY node_id`, snapshot.Space)
	if err != nil {
		return snapshot, err
	}
	var nodeIDs []string
	for nodeRows.Next() {
		var id string
		if err := nodeRows.Scan(&id); err != nil {
			nodeRows.Close()
			return snapshot, err
		}
		nodeIDs = append(nodeIDs, id)
	}
	nodeRows.Close()
	snapshot.Nodes, err = f.loadMemoryNodes(ctx, nodeIDs)
	if err != nil {
		return snapshot, err
	}
	conflictRows, err := f.ledger.QueryContext(ctx, `SELECT conflict_id FROM conflict_sets WHERE space=? ORDER BY conflict_id`, snapshot.Space)
	if err != nil {
		return snapshot, err
	}
	for conflictRows.Next() {
		var id string
		if err := conflictRows.Scan(&id); err != nil {
			conflictRows.Close()
			return snapshot, err
		}
		conflict, err := f.loadConflict(ctx, id, false)
		if err != nil {
			conflictRows.Close()
			return snapshot, err
		}
		snapshot.Conflicts = append(snapshot.Conflicts, conflict)
	}
	conflictRows.Close()
	resolutionRows, err := f.ledger.QueryContext(ctx, `SELECT r.conflict_id,r.generation FROM resolutions r
		JOIN conflict_sets c ON c.conflict_id=r.conflict_id WHERE c.space=? ORDER BY r.resolution_id`, snapshot.Space)
	if err != nil {
		return snapshot, err
	}
	for resolutionRows.Next() {
		var conflictID, generation string
		if err := resolutionRows.Scan(&conflictID, &generation); err != nil {
			resolutionRows.Close()
			return snapshot, err
		}
		resolution, err := f.loadResolution(ctx, conflictID, generation)
		if err != nil {
			resolutionRows.Close()
			return snapshot, err
		}
		snapshot.Resolutions = append(snapshot.Resolutions, resolution)
	}
	resolutionRows.Close()
	snapshot.Checksum = durableSnapshotChecksum(snapshot)
	return snapshot, nil
}

func durableSnapshotChecksum(snapshot DurableSnapshot) string {
	snapshot.Checksum = ""
	sort.Slice(snapshot.Contexts, func(i, j int) bool { return snapshot.Contexts[i].ID < snapshot.Contexts[j].ID })
	sort.Slice(snapshot.Events, func(i, j int) bool { return snapshot.Events[i].ID < snapshot.Events[j].ID })
	sort.Slice(snapshot.Identities, func(i, j int) bool {
		return snapshot.Identities[i].Identity.ID < snapshot.Identities[j].Identity.ID
	})
	sort.Slice(snapshot.Nodes, func(i, j int) bool { return snapshot.Nodes[i].ID < snapshot.Nodes[j].ID })
	sort.Slice(snapshot.Conflicts, func(i, j int) bool { return snapshot.Conflicts[i].ID < snapshot.Conflicts[j].ID })
	sort.Slice(snapshot.Resolutions, func(i, j int) bool { return snapshot.Resolutions[i].ID < snapshot.Resolutions[j].ID })
	encoded, _ := json.Marshal(snapshot)
	hash := sha256.Sum256(encoded)
	return hex.EncodeToString(hash[:])
}

func (f *PostgresFabric) ExportDurableSnapshot(ctx context.Context, space string) (DurableSnapshot, error) {
	snapshot := DurableSnapshot{Version: 1, Space: normalizeSpace(space),
		EventStatuses: map[string]SemanticStatus{}}
	rows, err := f.pool.Query(ctx, `SELECT context_id,space,COALESCE(parent_id,''),COALESCE(context_type,''),
		COALESCE(label,''),opened_at,closed_at FROM lumina_memory_contexts WHERE cluster_id=$1 AND
		tenant_id=$2 AND project_id=$3 AND space=$4 ORDER BY context_id`, f.clusterID, f.tenantID,
		f.projectID, snapshot.Space)
	if err != nil {
		return snapshot, err
	}
	for rows.Next() {
		var ref ContextRef
		var opened, closed *time.Time
		if err := rows.Scan(&ref.ID, &ref.Space, &ref.ParentID, &ref.Type, &ref.Label, &opened, &closed); err != nil {
			rows.Close()
			return snapshot, err
		}
		if opened != nil {
			ref.OpenedAt = opened.UTC()
		}
		if closed != nil {
			ref.ClosedAt = closed.UTC()
		}
		snapshot.Contexts = append(snapshot.Contexts, ref)
	}
	rows.Close()
	eventRows, err := f.pool.Query(ctx, `SELECT event_id,space,COALESCE(context_id,''),COALESCE(session_id,''),
		actor,COALESCE(source_kind,''),content,occurred_at,COALESCE(source_ref,''),metadata,semantic_status
		FROM lumina_memory_events WHERE cluster_id=$1 AND tenant_id=$2 AND project_id=$3 AND space=$4
		AND tombstoned=false ORDER BY occurred_at,event_id`, f.clusterID, f.tenantID, f.projectID, snapshot.Space)
	if err != nil {
		return snapshot, err
	}
	for eventRows.Next() {
		var event RawEvent
		var metadata []byte
		var status string
		if err := eventRows.Scan(&event.ID, &event.Space, &event.ContextID, &event.SessionID, &event.Actor,
			&event.SourceKind, &event.Content, &event.OccurredAt, &event.SourceRef, &metadata, &status); err != nil {
			eventRows.Close()
			return snapshot, err
		}
		_ = json.Unmarshal(metadata, &event.Metadata)
		event.OccurredAt = event.OccurredAt.UTC()
		snapshot.Events = append(snapshot.Events, event)
		snapshot.EventStatuses[event.ID] = SemanticStatus(status)
	}
	eventRows.Close()
	identityRows, err := f.pool.Query(ctx, `SELECT identity_id,space,canonical,COALESCE(identity_type,''),
		COALESCE(display_name,'') FROM lumina_memory_identities WHERE cluster_id=$1 AND tenant_id=$2
		AND project_id=$3 AND space=$4 ORDER BY identity_id`, f.clusterID, f.tenantID, f.projectID,
		snapshot.Space)
	if err != nil {
		return snapshot, err
	}
	for identityRows.Next() {
		var item DurableIdentity
		if err := identityRows.Scan(&item.Identity.ID, &item.Identity.Space, &item.Identity.Canonical,
			&item.Identity.Type, &item.Identity.DisplayName); err != nil {
			identityRows.Close()
			return snapshot, err
		}
		aliasRows, aliasErr := f.pool.Query(ctx, `SELECT normalized_alias FROM lumina_memory_identity_aliases
			WHERE cluster_id=$1 AND tenant_id=$2 AND project_id=$3 AND space=$4 AND identity_id=$5
			AND status='active' ORDER BY normalized_alias`, f.clusterID, f.tenantID, f.projectID,
			snapshot.Space, item.Identity.ID)
		if aliasErr != nil {
			identityRows.Close()
			return snapshot, aliasErr
		}
		for aliasRows.Next() {
			var alias string
			if aliasRows.Scan(&alias) == nil {
				item.Aliases = append(item.Aliases, alias)
			}
		}
		aliasRows.Close()
		snapshot.Identities = append(snapshot.Identities, item)
	}
	identityRows.Close()
	nodeRows, err := f.pool.Query(ctx, `SELECT node FROM lumina_memory_nodes WHERE cluster_id=$1 AND
		tenant_id=$2 AND project_id=$3 AND space=$4 ORDER BY node_id`, f.clusterID, f.tenantID,
		f.projectID, snapshot.Space)
	if err != nil {
		return snapshot, err
	}
	for nodeRows.Next() {
		var encoded []byte
		var node MemoryNode
		if err := nodeRows.Scan(&encoded); err != nil || json.Unmarshal(encoded, &node) != nil {
			nodeRows.Close()
			return snapshot, errors.New("decode PostgreSQL memory node")
		}
		snapshot.Nodes = append(snapshot.Nodes, node)
	}
	nodeRows.Close()
	conflictRows, err := f.pool.Query(ctx, `SELECT conflict FROM lumina_memory_conflicts WHERE cluster_id=$1
		AND tenant_id=$2 AND project_id=$3 AND space=$4 ORDER BY conflict_id`, f.clusterID, f.tenantID,
		f.projectID, snapshot.Space)
	if err != nil {
		return snapshot, err
	}
	for conflictRows.Next() {
		var encoded []byte
		var conflict Conflict
		if err := conflictRows.Scan(&encoded); err != nil || json.Unmarshal(encoded, &conflict) != nil {
			conflictRows.Close()
			return snapshot, errors.New("decode PostgreSQL memory conflict")
		}
		snapshot.Conflicts = append(snapshot.Conflicts, conflict)
	}
	conflictRows.Close()
	resolutionRows, err := f.pool.Query(ctx, `SELECT resolution FROM lumina_memory_resolutions WHERE cluster_id=$1
		AND tenant_id=$2 AND project_id=$3 AND space=$4 ORDER BY resolution_id`, f.clusterID, f.tenantID,
		f.projectID, snapshot.Space)
	if err != nil {
		return snapshot, err
	}
	for resolutionRows.Next() {
		var encoded []byte
		var resolution Resolution
		if err := resolutionRows.Scan(&encoded); err != nil || json.Unmarshal(encoded, &resolution) != nil {
			resolutionRows.Close()
			return snapshot, errors.New("decode PostgreSQL memory resolution")
		}
		snapshot.Resolutions = append(snapshot.Resolutions, resolution)
	}
	resolutionRows.Close()
	snapshot.Checksum = durableSnapshotChecksum(snapshot)
	return snapshot, nil
}

func (f *PostgresFabric) ImportDurableSnapshot(ctx context.Context, snapshot DurableSnapshot,
	migrationID string, dryRun bool) (FabricMigrationReport, error) {
	report := FabricMigrationReport{MigrationID: migrationID, TenantID: f.tenantID, ProjectID: f.projectID,
		Space: normalizeSpace(snapshot.Space), Events: len(snapshot.Events), Nodes: len(snapshot.Nodes),
		Identities: len(snapshot.Identities), Conflicts: len(snapshot.Conflicts), Resolutions: len(snapshot.Resolutions), DryRun: dryRun,
		Checksum: durableSnapshotChecksum(snapshot), Status: "validated"}
	if f != nil && f.options.WriteGuard != nil {
		if err := f.options.WriteGuard(ctx); err != nil {
			return report, err
		}
	}
	if snapshot.Checksum != "" && snapshot.Checksum != report.Checksum {
		return report, errors.New("memory snapshot checksum mismatch")
	}
	if dryRun {
		return report, nil
	}
	tx, err := f.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return report, err
	}
	defer tx.Rollback(ctx)
	var status, checksum string
	err = tx.QueryRow(ctx, `SELECT status,checksum FROM lumina_memory_imports WHERE cluster_id=$1
		AND tenant_id=$2 AND project_id=$3 AND migration_id=$4 FOR UPDATE`, f.clusterID, f.tenantID,
		f.projectID, migrationID).Scan(&status, &checksum)
	if err == nil {
		if status == "completed" && checksum == report.Checksum {
			report.Status = status
			return report, tx.Commit(ctx)
		}
		return report, fmt.Errorf("memory migration %s already exists with status %s", migrationID, status)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return report, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_imports VALUES($1,$2,$3,$4,$5,'running',$6,$7,NULL)`,
		f.clusterID, f.tenantID, f.projectID, migrationID, report.Space, report.Checksum, f.now()); err != nil {
		return report, err
	}
	for _, ref := range snapshot.Contexts {
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_contexts(
			cluster_id,tenant_id,project_id,space,context_id,parent_id,context_type,label,opened_at,closed_at)
			VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),NULLIF($8,''),$9,$10) ON CONFLICT DO NOTHING`,
			f.clusterID, f.tenantID, f.projectID, report.Space, ref.ID, ref.ParentID, ref.Type, ref.Label,
			nullTime(ref.OpenedAt), nullTime(ref.ClosedAt)); err != nil {
			return report, err
		}
	}
	for _, item := range snapshot.Identities {
		identity := item.Identity
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_identities(
			cluster_id,tenant_id,project_id,space,identity_id,canonical,identity_type,display_name,status,created_at)
			VALUES($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''),'active',$9) ON CONFLICT DO NOTHING`,
			f.clusterID, f.tenantID, f.projectID, report.Space, identity.ID, identity.Canonical,
			identity.Type, identity.DisplayName, f.now()); err != nil {
			return report, err
		}
		for _, alias := range item.Aliases {
			if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_identity_aliases(
				cluster_id,tenant_id,project_id,space,normalized_alias,identity_id,method,status,created_at)
				VALUES($1,$2,$3,$4,$5,$6,'migration','active',$7) ON CONFLICT DO NOTHING`, f.clusterID,
				f.tenantID, f.projectID, report.Space, alias, identity.ID, f.now()); err != nil {
				return report, err
			}
		}
	}
	for _, event := range snapshot.Events {
		metadata, _ := json.Marshal(event.Metadata)
		status := snapshot.EventStatuses[event.ID]
		if status == "" {
			status = SemanticEventDurable
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_events(
			cluster_id,tenant_id,project_id,space,event_id,context_id,session_id,actor,source_kind,content,
			occurred_at,source_ref,checksum,metadata,semantic_status,token_estimate,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8,NULLIF($9,''),$10,$11,NULLIF($12,''),
			$13,$14,$15,$16,$17,$17)`, f.clusterID, f.tenantID, f.projectID, report.Space, event.ID,
			event.ContextID, event.SessionID, event.Actor, event.SourceKind, event.Content, event.OccurredAt,
			event.SourceRef, eventChecksum(event), metadata, status, estimateTokens(event.Content), f.now()); err != nil {
			return report, err
		}
		sources, _ := json.Marshal([]string{event.ID})
		metadataDoc, _ := json.Marshal(map[string]any{"source_ref": event.SourceRef, "actor": event.Actor})
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_documents(
			cluster_id,tenant_id,project_id,space,doc_id,resource_kind,resource_id,context_id,actor,occurred_at,
			status,content,source_event_ids,metadata) VALUES($1,$2,$3,$4,$5,'event',$5,NULLIF($6,''),$7,$8,$9,$10,$11,$12)`,
			f.clusterID, f.tenantID, f.projectID, report.Space, event.ID, event.ContextID, event.Actor,
			event.OccurredAt, status, event.Content, sources, metadataDoc); err != nil {
			return report, err
		}
		if err := f.enqueueJobTx(ctx, tx, "index_event", report.Space, event.ID,
			map[string]any{"event_id": event.ID}, f.now()); err != nil {
			return report, err
		}
	}
	if _, err := tx.Exec(ctx, `WITH ordered AS (
		SELECT event_id,context_id,LAG(event_id) OVER (PARTITION BY context_id ORDER BY occurred_at,event_id) AS previous_id
		FROM lumina_memory_events WHERE cluster_id=$1 AND tenant_id=$2 AND project_id=$3 AND space=$4
		AND context_id IS NOT NULL AND tombstoned=false
	), pairs AS (
		SELECT previous_id AS source_id,event_id AS target_id FROM ordered WHERE previous_id IS NOT NULL
		UNION ALL SELECT event_id,previous_id FROM ordered WHERE previous_id IS NOT NULL
	) INSERT INTO lumina_memory_graph_edges(cluster_id,tenant_id,project_id,space,source_id,target_id,weight)
	SELECT $1,$2,$3,$4,source_id,target_id,1 FROM pairs ON CONFLICT DO NOTHING`, f.clusterID,
		f.tenantID, f.projectID, report.Space); err != nil {
		return report, err
	}
	for _, node := range snapshot.Nodes {
		nodeJSON, _ := json.Marshal(node)
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_nodes(
			cluster_id,tenant_id,project_id,space,node_id,context_id,slot_id,status,statement,node,content_hash,created_at,updated_at)
			VALUES($1,$2,$3,$4,$5,NULLIF($6,''),NULLIF($7,''),$8,$9,$10,$11,$12,$12)`, f.clusterID,
			f.tenantID, f.projectID, report.Space, node.ID, node.ContextID, node.SlotID, node.Status,
			node.Statement, nodeJSON, contentHash(node.Statement, claimValueKey(node.Value)), node.CreatedAt); err != nil {
			return report, err
		}
		if err := f.persistPostgresNodeAuxTx(ctx, tx, node, ""); err != nil {
			return report, err
		}
		for _, source := range node.Sources {
			if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_node_sources VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
				f.clusterID, f.tenantID, f.projectID, report.Space, node.ID, source.EventID,
				source.StartRune, source.EndRune, source.Role); err != nil {
				return report, err
			}
		}
		sourceIDs, _ := json.Marshal(sourceSpanEventIDs(node.Sources))
		metadataDoc, _ := json.Marshal(map[string]any{"slot_id": node.SlotID})
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_documents(
			cluster_id,tenant_id,project_id,space,doc_id,resource_kind,resource_id,context_id,status,content,source_event_ids,metadata)
			VALUES($1,$2,$3,$4,$5,'node',$5,NULLIF($6,''),$7,$8,$9,$10)`, f.clusterID, f.tenantID,
			f.projectID, report.Space, node.ID, node.ContextID, node.Status, node.Statement, sourceIDs, metadataDoc); err != nil {
			return report, err
		}
		if f.options.Vectorizer != nil {
			if err := f.enqueueJobTx(ctx, tx, "embed_node", report.Space, node.ID,
				map[string]any{"node_id": node.ID}, f.now()); err != nil {
				return report, err
			}
		}
	}
	for _, conflict := range snapshot.Conflicts {
		encoded, _ := json.Marshal(conflict)
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_conflicts VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$10)`,
			f.clusterID, f.tenantID, f.projectID, report.Space, conflict.ID, conflict.SlotID,
			conflict.Generation, conflict.Status, encoded, conflict.CreatedAt); err != nil {
			return report, err
		}
	}
	for _, resolution := range snapshot.Resolutions {
		encoded, _ := json.Marshal(resolution)
		if _, err := tx.Exec(ctx, `INSERT INTO lumina_memory_resolutions VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			f.clusterID, f.tenantID, f.projectID, report.Space, resolution.ID, resolution.ConflictID,
			resolution.Generation, encoded, resolution.CreatedAt); err != nil {
			return report, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE lumina_memory_imports SET status='completed',completed_at=$1
		WHERE cluster_id=$2 AND tenant_id=$3 AND project_id=$4 AND migration_id=$5`, f.now(),
		f.clusterID, f.tenantID, f.projectID, migrationID); err != nil {
		return report, err
	}
	if err := tx.Commit(ctx); err != nil {
		return report, err
	}
	report.Status = "completed"
	f.wakeWorker()
	return report, nil
}

func (f *Fabric) ImportDurableSnapshot(ctx context.Context, snapshot DurableSnapshot,
	migrationID string, dryRun bool) (FabricMigrationReport, error) {
	report := FabricMigrationReport{MigrationID: migrationID, TenantID: "local", ProjectID: "local",
		Space: normalizeSpace(snapshot.Space), Events: len(snapshot.Events), Nodes: len(snapshot.Nodes),
		Identities: len(snapshot.Identities), Conflicts: len(snapshot.Conflicts), Resolutions: len(snapshot.Resolutions), DryRun: dryRun,
		Checksum: durableSnapshotChecksum(snapshot), Status: "validated"}
	if snapshot.Checksum != "" && snapshot.Checksum != report.Checksum {
		return report, errors.New("memory snapshot checksum mismatch")
	}
	if dryRun {
		return report, nil
	}
	for _, ref := range snapshot.Contexts {
		if err := f.upsertContext(ctx, ref); err != nil {
			return report, err
		}
	}
	if _, err := f.AppendEvents(ctx, snapshot.Events, IngestOptions{SemanticPolicy: SemanticDurableOnly}); err != nil {
		var lag IndexLagError
		if !errors.As(err, &lag) {
			return report, err
		}
	}
	tx, err := f.ledger.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer tx.Rollback()
	now := f.now()
	for eventID, status := range snapshot.EventStatuses {
		if status == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE events SET semantic_status=? WHERE space=? AND event_id=?`,
			status, report.Space, eventID); err != nil {
			return report, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO outbox(resource_kind,resource_id,operation,payload_json,status,
			created_at,updated_at) VALUES('event',?,'upsert','{}','pending',?,?)
			ON CONFLICT(resource_kind,resource_id,operation) DO UPDATE SET status='pending',updated_at=excluded.updated_at`,
			eventID, formatFabricTime(now), formatFabricTime(now)); err != nil {
			return report, err
		}
	}
	for _, item := range snapshot.Identities {
		identity := item.Identity
		if _, err := tx.ExecContext(ctx, `INSERT INTO identities(
			identity_id,space,canonical,identity_type,display_name,status,created_at)
			VALUES(?,?,?,?,?,'active',?) ON CONFLICT DO NOTHING`, identity.ID, report.Space,
			identity.Canonical, identity.Type, identity.DisplayName, formatFabricTime(now)); err != nil {
			return report, err
		}
		for _, alias := range item.Aliases {
			if _, err := tx.ExecContext(ctx, `INSERT INTO identity_aliases(
				space,normalized_alias,identity_id,method,status,created_at)
				VALUES(?,?,?,'migration','active',?) ON CONFLICT DO NOTHING`, report.Space, alias,
				identity.ID, formatFabricTime(now)); err != nil {
				return report, err
			}
		}
	}
	for _, node := range snapshot.Nodes {
		if node.SubjectID != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO identities(
				identity_id,space,canonical,identity_type,display_name,status,created_at)
				VALUES(?,?,?,?,?,'active',?) ON CONFLICT DO NOTHING`, node.SubjectID, report.Space,
				normalizeClaim(node.Subject), "", node.Subject, formatFabricTime(node.CreatedAt)); err != nil {
				return report, err
			}
		}
		if node.SlotID != "" {
			if _, err := tx.ExecContext(ctx, `INSERT INTO slots(
				slot_id,space,subject_identity_id,facet,attribute_key,scope_key,created_at)
				VALUES(?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`, node.SlotID, report.Space, node.SubjectID,
				node.Facet, node.AttributeKey, node.ScopeKey, formatFabricTime(node.CreatedAt)); err != nil {
				return report, err
			}
		}
		payload, _ := json.Marshal(node.Payload)
		if _, err := tx.ExecContext(ctx, `INSERT INTO memory_nodes(
			node_id,space,context_id,node_kind,claim_type,statement,subject_identity_id,subject_text,
			facet,attribute_key,scope_key,slot_id,evidence_mode,valid_from,valid_until,status,payload_json,
			content_hash,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, node.ID,
			report.Space, node.ContextID, node.Kind, node.ClaimType, node.Statement, node.SubjectID, node.Subject,
			node.Facet, node.AttributeKey, node.ScopeKey, node.SlotID, node.EvidenceMode,
			formatFabricTime(node.ValidFrom), formatFabricTime(node.ValidUntil), node.Status, payload,
			contentHash(node.Statement, claimValueKey(node.Value)), formatFabricTime(node.CreatedAt),
			formatFabricTime(node.CreatedAt)); err != nil {
			return report, err
		}
		if node.Kind == NodeClaim {
			if err := insertClaimValueTx(ctx, tx, node.ID, node.Value); err != nil {
				return report, err
			}
		}
		for _, source := range node.Sources {
			if _, err := tx.ExecContext(ctx, `INSERT INTO node_sources VALUES(?,?,?,?,?) ON CONFLICT DO NOTHING`,
				node.ID, source.EventID, source.StartRune, source.EndRune, source.Role); err != nil {
				return report, err
			}
		}
		if node.ContextID != "" {
			_, _ = tx.ExecContext(ctx, `INSERT INTO node_contexts VALUES(?,?,'origin') ON CONFLICT DO NOTHING`,
				node.ID, node.ContextID)
		}
		for _, key := range node.Keys {
			_, _ = tx.ExecContext(ctx, `INSERT INTO node_keys VALUES(?,'anchor',?) ON CONFLICT DO NOTHING`, node.ID, key)
		}
		for _, cue := range node.RetrievalCues {
			_, _ = tx.ExecContext(ctx, `INSERT INTO node_keys VALUES(?,'cue',?) ON CONFLICT DO NOTHING`, node.ID, cue)
		}
		if node.SlotID != "" {
			_, _ = tx.ExecContext(ctx, `INSERT INTO slot_versions VALUES(?,?,?,?,?,?) ON CONFLICT DO NOTHING`,
				node.SlotID, node.ID, formatFabricTime(node.ValidFrom), formatFabricTime(node.ValidUntil),
				node.Status, formatFabricTime(node.CreatedAt))
		}
		_, _ = tx.ExecContext(ctx, `INSERT INTO outbox(resource_kind,resource_id,operation,payload_json,status,
			created_at,updated_at) VALUES('node',?,'upsert','{}','pending',?,?) ON CONFLICT DO NOTHING`,
			node.ID, formatFabricTime(now), formatFabricTime(now))
	}
	for _, conflict := range snapshot.Conflicts {
		if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_sets(
			conflict_id,space,slot_id,generation,content_hash,status,critical,created_at,updated_at)
			VALUES(?,?,?,?,?,?,0,?,?)`, conflict.ID, report.Space, conflict.SlotID, conflict.Generation,
			conflict.Generation, conflict.Status, formatFabricTime(conflict.CreatedAt),
			formatFabricTime(conflict.CreatedAt)); err != nil {
			return report, err
		}
		for _, member := range conflict.Members {
			if _, err := tx.ExecContext(ctx, `INSERT INTO conflict_members VALUES(?,?) ON CONFLICT DO NOTHING`,
				conflict.ID, member.ID); err != nil {
				return report, err
			}
		}
	}
	for _, resolution := range snapshot.Resolutions {
		if err := insertResolutionTx(ctx, tx, resolution); err != nil {
			return report, err
		}
	}
	if err := tx.Commit(); err != nil {
		return report, err
	}
	nodeIDs := make([]string, 0, len(snapshot.Nodes))
	for _, node := range snapshot.Nodes {
		nodeIDs = append(nodeIDs, node.ID)
	}
	if _, err := f.projectResources(ctx, "node", nodeIDs); err != nil {
		return report, err
	}
	eventIDs := make([]string, 0, len(snapshot.Events))
	for _, event := range snapshot.Events {
		eventIDs = append(eventIDs, event.ID)
	}
	if _, err := f.projectResources(ctx, "event", eventIDs); err != nil {
		return report, err
	}
	if err := f.SyncRetrievalSidecar(ctx); err != nil {
		return report, err
	}
	report.Status = "completed"
	return report, nil
}
