package session

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"LuminaCode/agent"
	"LuminaCode/harness"
)

type RuntimeLoad struct {
	Journal       *RuntimeJournal
	State         *agent.AgentState
	SkillRecovery map[string]any
	Tasks         []map[string]any
	Migrated      bool
}

type runtimeMigrationReport struct {
	SessionID     string   `json:"session_id"`
	SourceVersion int      `json:"source_version"`
	TargetVersion int      `json:"target_version"`
	Status        string   `json:"status"`
	Stage         string   `json:"stage,omitempty"`
	Error         string   `json:"error,omitempty"`
	Sources       []string `json:"sources,omitempty"`
	CompletedAt   string   `json:"completed_at,omitempty"`
}

func (s *Store) OpenRuntime(ctx context.Context, sessionID string) (*RuntimeLoad, error) {
	path := RuntimeJournalPath(s.dir, sessionID)
	if _, err := os.Stat(path); err == nil {
		journal, openErr := OpenRuntimeJournal(ctx, s.dir, sessionID)
		if openErr != nil {
			return nil, openErr
		}
		if recoverErr := RecoverInterruptedRuntime(ctx, journal); recoverErr != nil {
			_ = journal.Close()
			return nil, recoverErr
		}
		state, recovery, tasks, loadErr := LoadRuntimeState(ctx, journal)
		if loadErr != nil {
			_ = journal.Close()
			return nil, loadErr
		}
		return &RuntimeLoad{Journal: journal, State: state, SkillRecovery: recovery, Tasks: tasks}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	legacyState := s.LoadState(sessionID)
	legacyMessages := s.Load(sessionID)
	legacyRecovery := s.LoadSkillRecovery(sessionID)
	legacyTasks := s.LoadTaskRuntimeSnapshot(sessionID)
	if legacyState == nil && len(legacyMessages) == 0 {
		journal, err := OpenRuntimeJournal(ctx, s.dir, sessionID)
		if err != nil {
			return nil, err
		}
		if _, err := journal.Append(ctx, 0, harness.PendingEvent{
			Type:    harness.EventSessionCreated,
			Payload: map[string]any{"session_id": sessionID, "format_version": 2},
		}); err != nil {
			_ = journal.Close()
			return nil, err
		}
		return &RuntimeLoad{Journal: journal}, nil
	}
	if legacyState == nil {
		created := agent.NewAgentState()
		created.Messages = legacyMessages
		legacyState = &created
	}
	journal, err := s.migrateRuntime(ctx, sessionID, legacyState, legacyRecovery, legacyTasks)
	if err != nil {
		return nil, err
	}
	state, recovery, tasks, err := LoadRuntimeState(ctx, journal)
	if err != nil {
		_ = journal.Close()
		return nil, err
	}
	return &RuntimeLoad{Journal: journal, State: state, SkillRecovery: recovery, Tasks: tasks, Migrated: true}, nil
}

func (s *Store) migrateRuntime(ctx context.Context, sessionID string, state *agent.AgentState, recovery map[string]any, tasks []map[string]any) (*RuntimeJournal, error) {
	sessionRoot := s.sessionDir(sessionID)
	finalPath := RuntimeJournalPath(s.dir, sessionID)
	tempPath := finalPath + ".migrating"
	_ = os.Remove(tempPath)
	report := runtimeMigrationReport{SessionID: sessionID, SourceVersion: 1, TargetVersion: RuntimeJournalSchemaVersion, Status: "running", Stage: "backup"}
	backupRoot := filepath.Join(sessionRoot, ".migration-backup", "v1")
	if err := os.MkdirAll(backupRoot, 0o700); err != nil {
		return nil, err
	}
	for _, name := range []string{"transcript.jsonl", "meta.json", "state.json", "skill-recovery.json", "skill-recovery.commit.json", "tasks.json", "team.json"} {
		source := filepath.Join(sessionRoot, name)
		if _, err := os.Stat(source); err != nil {
			continue
		}
		if err := copyFile(source, filepath.Join(backupRoot, name)); err != nil {
			return nil, s.writeMigrationFailure(sessionID, report, "backup", err)
		}
		report.Sources = append(report.Sources, name)
	}

	report.Stage = "journal"
	journal, err := openRuntimeJournal(ctx, tempPath, sessionID, time.Now)
	if err != nil {
		return nil, s.writeMigrationFailure(sessionID, report, "journal", err)
	}
	failed := true
	defer func() {
		if failed {
			_ = journal.Close()
			_ = os.Remove(tempPath)
		}
	}()
	created, err := journal.Append(ctx, 0, harness.PendingEvent{
		Type:    harness.EventSessionCreated,
		Payload: map[string]any{"session_id": sessionID, "format_version": 2, "source": "legacy"},
	})
	if err != nil {
		return nil, s.writeMigrationFailure(sessionID, report, "session_event", err)
	}
	head := created[len(created)-1].StreamSeq
	userTurn := 0
	for _, message := range state.Messages {
		raw, marshalErr := json.Marshal(message)
		if marshalErr != nil {
			return nil, s.writeMigrationFailure(sessionID, report, "messages", marshalErr)
		}
		eventType := harness.EventMessageContextAppended
		switch role, _ := message["role"].(string); role {
		case "user":
			eventType = harness.EventMessageUserAppended
			userTurn++
		case "assistant":
			eventType = harness.EventMessageAssistantAppended
		}
		appended, appendErr := journal.Append(ctx, head, harness.PendingEvent{
			Type:     eventType,
			Audience: []harness.Audience{harness.AudienceModel, harness.AudienceUser},
			Payload:  harness.MessageAppendedPayload{Message: raw, UserTurn: userTurn},
		})
		if appendErr != nil {
			return nil, s.writeMigrationFailure(sessionID, report, "messages", appendErr)
		}
		head = appended[len(appended)-1].StreamSeq
	}
	stateEvents, err := appendRuntimeState(ctx, journal, head, state, recovery, tasks, harness.EventLegacySnapshotImported)
	if err != nil {
		return nil, s.writeMigrationFailure(sessionID, report, "state", err)
	}
	if len(stateEvents) > 0 {
		head = stateEvents[len(stateEvents)-1].StreamSeq
	}
	for _, task := range tasks {
		taskID := fmt.Sprint(task["task_id"])
		if taskID == "<nil>" || strings.TrimSpace(taskID) == "" {
			taskID = fmt.Sprint(task["id"])
		}
		if taskID == "<nil>" || strings.TrimSpace(taskID) == "" {
			continue
		}
		appended, appendErr := journal.Append(ctx, head, harness.PendingEvent{
			Type: harness.EventTaskCreated, Audience: []harness.Audience{harness.AudienceUser, harness.AudienceInternal},
			Payload: map[string]any{"task_id": taskID, "record": task, "source": "legacy"},
		})
		if appendErr != nil {
			return nil, s.writeMigrationFailure(sessionID, report, "tasks", appendErr)
		}
		head = appended[len(appended)-1].StreamSeq
	}
	if err := journal.IntegrityCheck(ctx); err != nil {
		return nil, s.writeMigrationFailure(sessionID, report, "integrity", err)
	}
	if err := journal.Close(); err != nil {
		return nil, s.writeMigrationFailure(sessionID, report, "close", err)
	}
	if err := os.Rename(tempPath, finalPath); err != nil {
		return nil, s.writeMigrationFailure(sessionID, report, "commit", err)
	}
	_ = os.Chmod(finalPath, 0o600)
	report.Status = "completed"
	report.Stage = "complete"
	report.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = atomicWriteJSON(filepath.Join(sessionRoot, "migration-v2.json"), report)
	failed = false
	return OpenRuntimeJournal(ctx, s.dir, sessionID)
}

func (s *Store) writeMigrationFailure(sessionID string, report runtimeMigrationReport, stage string, cause error) error {
	report.Status = "failed"
	report.Stage = stage
	report.Error = cause.Error()
	report.CompletedAt = time.Now().UTC().Format(time.RFC3339Nano)
	_ = atomicWriteJSON(filepath.Join(s.sessionDir(sessionID), "migration-error.json"), report)
	return fmt.Errorf("session %s runtime migration failed at %s: %w", sessionID, stage, cause)
}

func AppendRuntimeState(ctx context.Context, journal *RuntimeJournal, state *agent.AgentState, recovery map[string]any, tasks []map[string]any) error {
	journal.projectionMu.Lock()
	defer journal.projectionMu.Unlock()
	// Checkpoint facts can follow any concurrently committed lifecycle event;
	// their total order is the SQLite event sequence, so no stale read/write
	// decision is involved here.
	events, err := appendRuntimeState(ctx, journal, harness.AnyStreamSeq, state, recovery, tasks, harness.EventRuntimeStateCheckpointed)
	if err != nil {
		return err
	}
	if len(events) == 0 {
		return nil
	}
	projection := harness.NewSessionProjection()
	after := int64(0)
	checkpoint, err := journal.LoadCheckpoint(ctx, journal.sessionID, projection.Name())
	if err != nil {
		return err
	}
	if checkpoint != nil && checkpoint.ProjectorVersion == projection.Version() {
		if err := projection.UnmarshalState(checkpoint.State); err != nil {
			return err
		}
		after = checkpoint.UpToSeq
	}
	last, err := harness.Replay(ctx, journal, projection, after)
	if err != nil {
		return err
	}
	data, err := projection.MarshalState()
	if err != nil {
		return err
	}
	return journal.SaveCheckpoint(ctx, harness.Checkpoint{StreamID: journal.sessionID, Projector: projection.Name(), ProjectorVersion: projection.Version(), UpToSeq: last, State: data})
}

func appendRuntimeState(ctx context.Context, journal *RuntimeJournal, expectedStreamSeq int64, state *agent.AgentState, recovery map[string]any, tasks []map[string]any, eventType string) ([]harness.Event, error) {
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return nil, err
	}
	recoveryJSON, err := json.Marshal(recovery)
	if err != nil {
		return nil, err
	}
	tasksJSON, err := json.Marshal(tasks)
	if err != nil {
		return nil, err
	}
	return journal.Append(ctx, expectedStreamSeq, harness.PendingEvent{
		Type:    eventType,
		Payload: harness.RuntimeStateCheckpointedPayload{State: stateJSON, SkillRecovery: recoveryJSON, Tasks: tasksJSON},
	})
}

func LoadRuntimeState(ctx context.Context, journal *RuntimeJournal) (*agent.AgentState, map[string]any, []map[string]any, error) {
	var latest *harness.RuntimeStateCheckpointedPayload
	taskRecords := map[string]map[string]any{}
	after := int64(0)
	for {
		events, err := journal.Load(ctx, after, 500)
		if err != nil {
			return nil, nil, nil, err
		}
		for _, event := range events {
			switch event.Type {
			case harness.EventRuntimeStateCheckpointed, harness.EventLegacySnapshotImported:
				var payload harness.RuntimeStateCheckpointedPayload
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					return nil, nil, nil, err
				}
				latest = &payload
			case harness.EventTaskCreated, harness.EventTaskStatusChanged, harness.EventTaskNotificationAppended:
				var payload struct {
					TaskID string         `json:"task_id"`
					Record map[string]any `json:"record"`
				}
				if err := json.Unmarshal(event.Payload, &payload); err != nil {
					return nil, nil, nil, err
				}
				if payload.TaskID != "" && len(payload.Record) > 0 {
					taskRecords[payload.TaskID] = payload.Record
				}
			}
		}
		if len(events) == 0 || len(events) < 500 {
			break
		}
		after = events[len(events)-1].Seq
	}
	projectedTasks := make([]map[string]any, 0, len(taskRecords))
	for _, record := range taskRecords {
		projectedTasks = append(projectedTasks, record)
	}
	sort.SliceStable(projectedTasks, func(i, j int) bool {
		left, _ := projectedTasks[i]["created_at"].(float64)
		right, _ := projectedTasks[j]["created_at"].(float64)
		if left == right {
			return fmt.Sprint(projectedTasks[i]["task_id"]) < fmt.Sprint(projectedTasks[j]["task_id"])
		}
		return left < right
	})
	if latest == nil {
		return nil, nil, projectedTasks, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(latest.State))
	decoder.UseNumber()
	var stateMap map[string]any
	if err := decoder.Decode(&stateMap); err != nil {
		return nil, nil, nil, err
	}
	state, err := agent.GetAgentStateFromMap(stateMap)
	if err != nil {
		return nil, nil, nil, err
	}
	if state.PermissionState == nil {
		fresh := agent.NewAgentState()
		state.PermissionState = fresh.PermissionState
	}
	var recovery map[string]any
	if len(latest.SkillRecovery) > 0 && string(latest.SkillRecovery) != "null" {
		if err := json.Unmarshal(latest.SkillRecovery, &recovery); err != nil {
			return nil, nil, nil, err
		}
	}
	var tasks []map[string]any
	if len(latest.Tasks) > 0 && string(latest.Tasks) != "null" {
		if err := json.Unmarshal(latest.Tasks, &tasks); err != nil {
			return nil, nil, nil, err
		}
	}
	if len(projectedTasks) > 0 {
		tasks = projectedTasks
	}
	return &state, recovery, tasks, nil
}
