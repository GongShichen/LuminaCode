package test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"LuminaCode/agent"
	"LuminaCode/session"
)

func TestSessionStoreStateRecoveryAndTaskGeneration(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	state := agent.NewAgentState()
	state.Messages = append(state.Messages, map[string]any{"role": "user", "content": "hello"})
	state.TurnCount = 3
	state.SystemPrompt = "<>&雪"
	state.CacheBreakPoints.Add(4)
	recovery := map[string]any{"version": 1, "agent_scopes": map[string]any{"main": map[string]any{}}}
	tasks := []map[string]any{{"task_id": "task-1", "status": "completed"}}
	if err := store.SaveStateWithRecovery("sess", &state, recovery, tasks); err != nil {
		t.Fatal(err)
	}
	stateData, err := os.ReadFile(filepath.Join(dir, "sess.state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(stateData), `"cache_breakpoints"`) || strings.Contains(string(stateData), `"cache_break_points"`) {
		t.Fatalf("state snapshot should use Python cache_breakpoints key, got:\n%s", stateData)
	}
	if stateData[len(stateData)-1] == '\n' {
		t.Fatalf("state snapshot should match Python json.dumps without trailing newline, got:\n%s", stateData)
	}
	if strings.Contains(string(stateData), `\u003c`) || strings.Contains(string(stateData), `\u003e`) || strings.Contains(string(stateData), `\u0026`) || !strings.Contains(string(stateData), "<>&雪") {
		t.Fatalf("state snapshot should match Python ensure_ascii=False text escaping, got:\n%s", stateData)
	}
	requireSubstringsInOrder(t, string(stateData),
		`"generation"`,
		`"state"`,
		`"messages"`,
		`"total_input_tokens"`,
		`"total_output_tokens"`,
		`"cache_read_input_tokens"`,
		`"cache_creation_input_tokens"`,
		`"server_tool_use_input_tokens"`,
		`"turn_count"`,
		`"consecutive_autocompact_failures"`,
		`"last_continue_reason"`,
		`"permission_state"`,
		`"confirmed_paths"`,
		`"confirmed_tools"`,
		`"confirmed_command_rules"`,
		`"yolo_mode"`,
		`"system_prompt"`,
		`"recent_api_errors"`,
		`"content_replacements"`,
		`"denied_tool_calls"`,
		`"tool_errors"`,
		`"total_task_budget"`,
		`"task_budget_remaining"`,
		`"cache_breakpoints"`,
		`"read_file_state"`,
		`"last_query"`,
		`"last_extraction_turn"`,
		`"user_turn_count"`,
		`"last_extraction_user_turn"`,
		`"memory_writes_since_extraction"`,
	)
	var persisted map[string]any
	if err := json.Unmarshal(stateData, &persisted); err != nil {
		t.Fatal(err)
	}
	generation, _ := persisted["generation"].(string)
	if !regexp.MustCompile(`^[0-9a-f]{32}$`).MatchString(generation) {
		t.Fatalf("generation should match Python uuid.uuid4().hex, got %q", generation)
	}
	if loaded := store.LoadState("sess"); loaded == nil || loaded.TurnCount != 3 || len(loaded.Messages) != 1 || !loaded.CacheBreakPoints.Contains(4) {
		t.Fatalf("unexpected loaded state: %#v", loaded)
	}
	if loadedRecovery := store.LoadSkillRecovery("sess"); loadedRecovery == nil || loadedRecovery["generation"] == "" {
		t.Fatalf("expected aligned recovery snapshot, got %#v", loadedRecovery)
	}
	if loadedTasks := store.LoadTaskRuntimeSnapshot("sess"); len(loadedTasks) != 1 || loadedTasks[0]["task_id"] != "task-1" {
		t.Fatalf("unexpected task snapshot: %#v", loadedTasks)
	}
	recoveryData, err := os.ReadFile(filepath.Join(dir, "sess.skill-recovery.json"))
	if err != nil {
		t.Fatal(err)
	}
	requireSubstringsInOrder(t, string(recoveryData), `"version"`, `"generation"`, `"agent_scopes"`)

	if err := os.WriteFile(filepath.Join(dir, "sess.skill-recovery.commit.json"), []byte(`{"generation":"wrong","version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if store.LoadSkillRecovery("sess") != nil || store.LoadTaskRuntimeSnapshot("sess") != nil {
		t.Fatal("expected generation mismatch to suppress recovery and task snapshots")
	}
}

func TestSessionStoreSaveStatePreservesExistingRecoverySnapshots(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	state := agent.NewAgentState()
	state.Messages = []map[string]any{{"role": "user", "content": "one"}}
	recovery := map[string]any{"version": 1, "agent_scopes": map[string]any{"main": map[string]any{"ready": true}}}
	tasks := []map[string]any{{"task_id": "task-1", "status": "running"}}
	if err := store.SaveStateWithRecovery("sess", &state, recovery, tasks); err != nil {
		t.Fatal(err)
	}

	state.Messages = append(state.Messages, map[string]any{"role": "assistant", "content": "two"})
	state.TurnCount = 2
	if err := store.SaveState("sess", &state); err != nil {
		t.Fatal(err)
	}

	loaded := store.LoadState("sess")
	if loaded == nil || len(loaded.Messages) != 2 || loaded.TurnCount != 2 {
		t.Fatalf("unexpected saved state: %#v", loaded)
	}
	loadedRecovery := store.LoadSkillRecovery("sess")
	if loadedRecovery == nil || loadedRecovery["generation"] == "" {
		t.Fatalf("expected preserved recovery snapshot, got %#v", loadedRecovery)
	}
	scopes, _ := loadedRecovery["agent_scopes"].(map[string]any)
	if scopes == nil || scopes["main"] == nil {
		t.Fatalf("expected preserved recovery scopes, got %#v", loadedRecovery)
	}
	loadedTasks := store.LoadTaskRuntimeSnapshot("sess")
	if len(loadedTasks) != 1 || loadedTasks[0]["task_id"] != "task-1" {
		t.Fatalf("expected preserved task snapshot, got %#v", loadedTasks)
	}
}

func TestSessionStoreSaveSnapshotWritesJSONLBeforeStateStyleSnapshot(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	state := agent.NewAgentState()
	state.Messages = []map[string]any{{"role": "user", "content": "hello"}}
	state.TurnCount = 7
	recovery := map[string]any{"agent_scopes": map[string]any{"main": map[string]any{"ok": true}}}
	tasks := []map[string]any{{"task_id": "task-1", "status": "running"}}

	if err := store.SaveSnapshotWithRecovery("sess", &state, recovery, tasks); err != nil {
		t.Fatal(err)
	}

	if data, err := os.ReadFile(filepath.Join(dir, "sess.jsonl")); err != nil || !strings.Contains(string(data), `"role": "user"`) {
		t.Fatalf("expected JSONL transcript to be saved like Python before/with state snapshot, data=%q err=%v", data, err)
	}
	if loaded := store.LoadState("sess"); loaded == nil || len(loaded.Messages) != 1 || loaded.TurnCount != 7 {
		t.Fatalf("expected state snapshot, got %#v", loaded)
	}
	meta := store.LoadMeta("sess")
	if meta == nil || meta.MessageCount != 1 || meta.TurnCount != 7 {
		t.Fatalf("expected final Python-style metadata counters, got %#v", meta)
	}
	if loadedRecovery := store.LoadSkillRecovery("sess"); loadedRecovery == nil {
		t.Fatal("expected recovery snapshot to align with state generation")
	}
	if loadedTasks := store.LoadTaskRuntimeSnapshot("sess"); len(loadedTasks) != 1 || loadedTasks[0]["task_id"] != "task-1" {
		t.Fatalf("expected task runtime snapshot, got %#v", loadedTasks)
	}
}

func TestSessionStoreNilTaskSnapshotPersistsEmptyList(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	state := agent.NewAgentState()
	if err := store.SaveStateWithRecovery("sess", &state, nil, nil); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sess.tasks.json"))
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatal(err)
	}
	if tasks, ok := payload["tasks"].([]any); !ok || len(tasks) != 0 {
		t.Fatalf("nil task snapshot should persist as Python empty list, got %s", data)
	}
	requireSubstringsInOrder(t, string(data), `"version"`, `"generation"`, `"tasks"`)
	if loaded := store.LoadTaskRuntimeSnapshot("sess"); loaded == nil || len(loaded) != 0 {
		t.Fatalf("expected aligned empty task snapshot, got %#v", loaded)
	}
}

func TestSessionStoreSaveWithMetaPreservesCreatedAt(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	meta := &session.Meta{
		SessionID:    "sess",
		CreatedAt:    123,
		LastUpdated:  456,
		MessageCount: 9,
		TurnCount:    8,
	}
	if err := store.SaveWithMeta("sess", []map[string]any{{"role": "user", "content": "hello"}}, meta, 4); err != nil {
		t.Fatal(err)
	}
	loaded := store.LoadMeta("sess")
	if loaded == nil {
		t.Fatal("expected saved metadata")
	}
	if loaded.CreatedAt != 123 || loaded.LastUpdated <= 456 || loaded.MessageCount != 1 || loaded.TurnCount != 4 {
		t.Fatalf("metadata should preserve created_at and update counters, got %#v", loaded)
	}
}

func TestSessionStoreLoadersIgnoreMalformedFilesLikePython(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)

	if err := os.WriteFile(filepath.Join(dir, "broken.meta.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if store.LoadMeta("broken") != nil {
		t.Fatal("malformed metadata should be ignored")
	}

	if err := os.WriteFile(filepath.Join(dir, "broken.state.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if store.LoadState("broken") != nil {
		t.Fatal("malformed state should be ignored")
	}

	state := agent.NewAgentState()
	if err := store.SaveStateWithRecovery("sess", &state, map[string]any{"version": 1, "agent_scopes": map[string]any{}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sess.skill-recovery.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if store.LoadSkillRecovery("sess") != nil {
		t.Fatal("malformed skill recovery should be ignored")
	}
	if err := store.SaveStateWithRecovery("sess", &state, map[string]any{"version": 1, "agent_scopes": map[string]any{}}, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sess.tasks.json"), []byte("{broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	if store.LoadTaskRuntimeSnapshot("sess") != nil {
		t.Fatal("malformed task runtime snapshot should be ignored")
	}
}

func TestSessionStoreLoadSkipsTruncatedJSONLLinesLikePython(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	path := filepath.Join(dir, "sess.jsonl")
	line := `{"role": "user", "content": []}` + "\n{broken\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded := store.Load("sess")
	if len(loaded) != 1 || loaded[0]["role"] != "user" {
		t.Fatalf("expected only valid JSONL records to load, got %#v", loaded)
	}
}

func TestSessionStoreDeleteRemovesRecoveryAndTaskFilesLikePython(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	state := agent.NewAgentState()
	if err := store.SaveStateWithRecovery("sess", &state, map[string]any{"version": 1, "agent_scopes": map[string]any{}}, []map[string]any{{"task_id": "task-1"}}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"sess.skill-recovery.json",
		"sess.skill-recovery.commit.json",
		"sess.tasks.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("expected %s before delete: %v", name, err)
		}
	}

	store.Delete("sess")

	for _, name := range []string{
		"sess.skill-recovery.json",
		"sess.skill-recovery.commit.json",
		"sess.tasks.json",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be removed, stat err=%v", name, err)
		}
	}
}

func TestSessionStoreFullResumeRoundTripPreservesPythonStateFields(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	state := agent.NewAgentState()
	state.Messages = []map[string]any{
		{"role": "user", "content": []map[string]any{{"type": "text", "text": "hello"}}},
		{"role": "assistant", "content": []map[string]any{{"type": "text", "text": "hi there"}}},
	}
	state.TurnCount = 3
	state.TotalInputTokens = 100
	state.TotalOutputTokens = 50
	state.LastQuery = "hello"
	state.PermissionState.YoloMode = true

	if err := store.SaveState("sess", &state); err != nil {
		t.Fatal(err)
	}
	loaded := store.LoadState("sess")
	if loaded == nil {
		t.Fatal("expected state to load")
	}
	if len(loaded.Messages) != 2 ||
		loaded.TurnCount != 3 ||
		loaded.TotalInputTokens != 100 ||
		loaded.TotalOutputTokens != 50 ||
		loaded.LastQuery != "hello" ||
		loaded.PermissionState == nil ||
		!loaded.PermissionState.YoloMode {
		t.Fatalf("resume state did not round trip Python fields: %#v", loaded)
	}
}

func TestSessionStoreJSONLMatchesPythonTextConventions(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStore(dir)
	messages := []map[string]any{{"role": "user", "content": "<>&雪"}}
	if err := store.Save("sess", messages, 1); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "sess.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	if !strings.HasSuffix(got, "\n") {
		t.Fatalf("JSONL should end each record with a newline, got %q", got)
	}
	if strings.Contains(got, `\u003c`) || strings.Contains(got, `\u003e`) || strings.Contains(got, `\u0026`) || !strings.Contains(got, "<>&雪") {
		t.Fatalf("JSONL should match Python ensure_ascii=False text escaping, got %q", got)
	}
	if !strings.Contains(got, `": "`) || !strings.Contains(got, `, "`) {
		t.Fatalf("JSONL should use Python json.dumps default separators, got %q", got)
	}
}

func requireSubstringsInOrder(t *testing.T, text string, substrings ...string) {
	t.Helper()
	offset := 0
	for _, substring := range substrings {
		index := strings.Index(text[offset:], substring)
		if index < 0 {
			t.Fatalf("expected %q after byte %d in:\n%s", substring, offset, text)
		}
		offset += index + len(substring)
	}
}
