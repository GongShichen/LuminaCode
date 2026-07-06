package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"LuminaCode/agent"
	"LuminaCode/security"
	"LuminaCode/tools/file"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/google/uuid"
	orderedmap "github.com/pb33f/ordered-map/v2"
)

type Meta struct {
	SessionID    string  `json:"session_id"`
	CreatedAt    float64 `json:"created_at"`
	LastUpdated  float64 `json:"last_updated"`
	MessageCount int     `json:"message_count"`
	TurnCount    int     `json:"turn_count"`
}

type Store struct {
	dir string
}

func NewStore(sessionDir string) *Store {
	_ = os.MkdirAll(sessionDir, 0o755)
	return &Store{dir: sessionDir}
}

func (s *Store) Save(sessionID string, messages []map[string]any, turnCount int) error {
	if err := atomicWriteJSONL(s.sessionPath(sessionID), messages); err != nil {
		return err
	}
	_, err := s.upsertMeta(sessionID, len(messages), turnCount, nil)
	return err
}

func (s *Store) SaveWithMeta(sessionID string, messages []map[string]any, meta *Meta, turnCount int) error {
	if err := atomicWriteJSONL(s.sessionPath(sessionID), messages); err != nil {
		return err
	}
	_, err := s.upsertMeta(sessionID, len(messages), turnCount, meta)
	return err
}

func (s *Store) SaveState(sessionID string, state *agent.AgentState) error {
	return s.SaveStateWithRecovery(
		sessionID,
		state,
		s.LoadSkillRecovery(sessionID),
		s.LoadTaskRuntimeSnapshot(sessionID),
	)
}

func (s *Store) Load(sessionID string) []map[string]any {
	file, err := os.Open(s.sessionPath(sessionID))
	if err != nil {
		return nil
	}
	defer file.Close()
	var messages []map[string]any
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var message map[string]any
		if err := json.Unmarshal(line, &message); err == nil {
			messages = append(messages, message)
		}
	}
	return messages
}

func (s *Store) SaveStateWithRecovery(sessionID string, state *agent.AgentState, recovery map[string]any, tasks []map[string]any) error {
	if state == nil {
		return nil
	}
	generation := newGenerationID()
	stateMap := orderedAgentStateSnapshot(state)
	recoveryPayload := newOrderedJSONMap()
	recoveryPayload.Set("version", 1)
	recoveryPayload.Set("generation", generation)
	recoveryPayload.Set("agent_scopes", map[string]any{})
	setOrderedMapValues(recoveryPayload, recovery)
	recoveryPayload.Set("generation", generation)

	statePayload := newOrderedJSONMap()
	statePayload.Set("generation", generation)
	statePayload.Set("state", stateMap)
	if err := atomicWriteJSON(s.statePath(sessionID), statePayload); err != nil {
		return err
	}
	if err := atomicWriteJSON(s.skillRecoveryPath(sessionID), recoveryPayload); err != nil {
		return err
	}
	if tasks == nil {
		tasks = []map[string]any{}
	}
	taskPayload := newOrderedJSONMap()
	taskPayload.Set("version", 1)
	taskPayload.Set("generation", generation)
	taskPayload.Set("tasks", tasks)
	if err := atomicWriteJSON(s.taskRuntimePath(sessionID), taskPayload); err != nil {
		return err
	}
	commitPayload := newOrderedJSONMap()
	commitPayload.Set("generation", generation)
	commitPayload.Set("version", 1)
	if err := atomicWriteJSON(s.skillRecoveryCommitPath(sessionID), commitPayload); err != nil {
		return err
	}
	_, err := s.upsertMeta(sessionID, len(state.Messages), state.TurnCount, nil)
	return err
}

func (s *Store) SaveSnapshotWithRecovery(sessionID string, state *agent.AgentState, recovery map[string]any, tasks []map[string]any) error {
	if state == nil {
		return nil
	}
	if err := s.Save(sessionID, state.Messages, state.TurnCount); err != nil {
		return err
	}
	return s.SaveStateWithRecovery(sessionID, state, recovery, tasks)
}

func (s *Store) LoadState(sessionID string) *agent.AgentState {
	data := loadJSONMap(s.statePath(sessionID))
	if data == nil {
		return nil
	}
	payload, _ := data["state"].(map[string]any)
	if payload == nil {
		payload = data
	}
	permissionRaw, _ := payload["permission_state"].(map[string]any)
	cacheBreakpointsRaw := payload["cache_breakpoints"]
	if cacheBreakpointsRaw == nil {
		cacheBreakpointsRaw = payload["cache_break_points"]
	}
	delete(payload, "permission_state")
	delete(payload, "cache_breakpoints")
	delete(payload, "cache_break_points")
	state, err := agent.GetAgentStateFromMap(payload)
	if err != nil {
		return nil
	}
	if permissionRaw != nil {
		if permissionState, err := security.GetPermissionStateFromMap(permissionRaw); err == nil {
			state.PermissionState = &permissionState
		}
	}
	if state.PermissionState == nil {
		state.PermissionState = security.DefaultPermissionState()
	}
	state.CacheBreakPoints = mapset.NewSet[int]()
	for _, value := range anySlice(cacheBreakpointsRaw) {
		state.CacheBreakPoints.Add(intFromAny(value))
	}
	return &state
}

func (s *Store) LoadSkillRecovery(sessionID string) map[string]any {
	stateData, recoveryData, commitData := s.loadGenerationTriplet(sessionID, s.skillRecoveryPath(sessionID))
	if stateData == nil || recoveryData == nil || commitData == nil || intFromAny(recoveryData["version"]) != 1 {
		return nil
	}
	return recoveryData
}

func (s *Store) LoadTaskRuntimeSnapshot(sessionID string) []map[string]any {
	stateData, taskData, commitData := s.loadGenerationTriplet(sessionID, s.taskRuntimePath(sessionID))
	if stateData == nil || taskData == nil || commitData == nil || intFromAny(taskData["version"]) != 1 {
		return nil
	}
	rawTasks, ok := taskData["tasks"].([]any)
	if !ok {
		return nil
	}
	tasks := make([]map[string]any, 0, len(rawTasks))
	for _, raw := range rawTasks {
		if task, ok := raw.(map[string]any); ok {
			tasks = append(tasks, task)
		}
	}
	return tasks
}

func (s *Store) ListSessions() []Meta {
	matches, _ := filepath.Glob(filepath.Join(s.dir, "*.meta.json"))
	sort.Slice(matches, func(i, j int) bool {
		ii, _ := os.Stat(matches[i])
		jj, _ := os.Stat(matches[j])
		if ii == nil || jj == nil {
			return matches[i] < matches[j]
		}
		return ii.ModTime().After(jj.ModTime())
	})
	var metas []Meta
	for _, path := range matches {
		id := filepath.Base(path)
		id = id[:len(id)-len(".meta.json")]
		if meta := s.LoadMeta(id); meta != nil {
			metas = append(metas, *meta)
		}
	}
	return metas
}

func (s *Store) LoadMeta(sessionID string) *Meta {
	data := loadJSONMap(s.metaPath(sessionID))
	if data == nil {
		return nil
	}
	meta := &Meta{
		SessionID:    stringFromAny(data["session_id"]),
		CreatedAt:    floatFromAny(data["created_at"]),
		LastUpdated:  floatFromAny(data["last_updated"]),
		MessageCount: intFromAny(data["message_count"]),
		TurnCount:    intFromAny(data["turn_count"]),
	}
	if meta.SessionID == "" {
		return nil
	}
	return meta
}

func (s *Store) Delete(sessionID string) {
	_ = os.Remove(s.sessionPath(sessionID))
	_ = os.Remove(s.metaPath(sessionID))
	_ = os.Remove(s.statePath(sessionID))
	_ = os.Remove(s.skillRecoveryPath(sessionID))
	_ = os.Remove(s.skillRecoveryCommitPath(sessionID))
	_ = os.Remove(s.taskRuntimePath(sessionID))
	_ = os.Remove(filepath.Join(s.dir, sessionID+".sqlite"))
}

func (s *Store) upsertMeta(sessionID string, messageCount, turnCount int, provided *Meta) (*Meta, error) {
	now := float64(time.Now().UnixNano()) / 1e9
	meta := provided
	if meta == nil {
		meta = s.LoadMeta(sessionID)
	}
	if meta == nil {
		meta = &Meta{SessionID: sessionID, CreatedAt: now}
	}
	meta.LastUpdated = now
	meta.MessageCount = messageCount
	meta.TurnCount = turnCount
	return meta, atomicWriteJSON(s.metaPath(sessionID), meta)
}

func (s *Store) loadGenerationTriplet(sessionID, payloadPath string) (map[string]any, map[string]any, map[string]any) {
	stateData := loadJSONMap(s.statePath(sessionID))
	payloadData := loadJSONMap(payloadPath)
	commitData := loadJSONMap(s.skillRecoveryCommitPath(sessionID))
	if stateData == nil || payloadData == nil || commitData == nil {
		return nil, nil, nil
	}
	stateGeneration := stringFromAny(stateData["generation"])
	payloadGeneration := stringFromAny(payloadData["generation"])
	commitGeneration := stringFromAny(commitData["generation"])
	if stateGeneration == "" || stateGeneration != payloadGeneration || stateGeneration != commitGeneration {
		return nil, nil, nil
	}
	return stateData, payloadData, commitData
}

func (s *Store) sessionPath(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".jsonl")
}

func (s *Store) metaPath(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".meta.json")
}

func (s *Store) statePath(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".state.json")
}

func (s *Store) skillRecoveryPath(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".skill-recovery.json")
}

func (s *Store) skillRecoveryCommitPath(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".skill-recovery.commit.json")
}

func (s *Store) taskRuntimePath(sessionID string) string {
	return filepath.Join(s.dir, sessionID+".tasks.json")
}

func atomicWriteJSON(path string, value any) error {
	data, err := marshalPythonJSON(value, true)
	if err != nil {
		return err
	}
	return atomicReplace(path, data)
}

func atomicWriteJSONL(path string, records []map[string]any) error {
	var data []byte
	for _, record := range records {
		line, err := marshalPythonJSON(record, false)
		if err != nil {
			return err
		}
		line = spacePythonJSONSeparators(line)
		data = append(data, line...)
		data = append(data, '\n')
	}
	return atomicReplace(path, data)
}

func atomicReplace(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), filepath.Base(path)+"."+newGenerationID()+".tmp")
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func newGenerationID() string {
	return strings.ReplaceAll(uuid.NewString(), "-", "")
}

func newOrderedJSONMap() *orderedmap.OrderedMap[string, any] {
	return orderedmap.New[string, any](orderedmap.WithDisableHTMLEscape[string, any]())
}

func setOrderedMapValues(target *orderedmap.OrderedMap[string, any], values map[string]any) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		target.Set(key, values[key])
	}
}

func orderedAgentStateSnapshot(state *agent.AgentState) *orderedmap.OrderedMap[string, any] {
	out := newOrderedJSONMap()
	out.Set("messages", state.Messages)
	out.Set("total_input_tokens", state.TotalInputTokens)
	out.Set("total_output_tokens", state.TotalOutputTokens)
	out.Set("cache_read_input_tokens", state.CacheReadInputTokens)
	out.Set("cache_creation_input_tokens", state.CacheCreateInputTokens)
	out.Set("server_tool_use_input_tokens", state.ServerToolUseInputTokens)
	out.Set("turn_count", state.TurnCount)
	out.Set("consecutive_autocompact_failures", state.ConsecutiveAutoCompactFailures)
	out.Set("last_continue_reason", state.LastContinueReason)
	out.Set("permission_state", orderedPermissionStateSnapshot(state.PermissionState))
	out.Set("system_prompt", state.SystemPrompt)
	out.Set("recent_api_errors", state.RecentApiErrors)
	out.Set("content_replacements", state.ContentReplacements)
	out.Set("denied_tool_calls", state.DeniedToolCalls)
	out.Set("tool_errors", state.ToolErrors)
	out.Set("total_task_budget", state.TotalTaskBudget)
	out.Set("task_budget_remaining", state.TaskBudgetRemaining)
	out.Set("cache_breakpoints", intSetToSlice(state.CacheBreakPoints))
	out.Set("read_file_state", orderedReadFileStateSnapshot(state.ReadFileState))
	out.Set("last_query", state.LastQuery)
	out.Set("last_extraction_turn", state.LastExtractionTurn)
	out.Set("user_turn_count", state.UserTurnCount)
	out.Set("last_extraction_user_turn", state.LastExtractionUserTurn)
	out.Set("memory_writes_since_extraction", state.MemoryWritesSinceExtraction)
	return out
}

func orderedPermissionStateSnapshot(permissionState *security.PermissionState) *orderedmap.OrderedMap[string, any] {
	if permissionState == nil {
		permissionState = security.DefaultPermissionState()
	}
	out := newOrderedJSONMap()
	out.Set("confirmed_paths", sortedStringSet(permissionState.ConfirmedPaths))
	out.Set("confirmed_tools", sortedStringSet(permissionState.ConfirmedTools))
	out.Set("confirmed_command_rules", sortedStringSet(permissionState.ConfirmedCommandRules))
	out.Set("yolo_mode", permissionState.YoloMode)
	return out
}

func orderedReadFileStateSnapshot(entries map[string]file.FileStateEntry) *orderedmap.OrderedMap[string, any] {
	out := newOrderedJSONMap()
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		entry := entries[key]
		item := newOrderedJSONMap()
		item.Set("content", entry.Content)
		item.Set("timestamp", entry.TimeStamp)
		item.Set("is_partial_view", entry.IsPartialView)
		item.Set("line_endings", entry.LineEndings)
		out.Set(key, item)
	}
	return out
}

func sortedStringSet(values mapset.Set[string]) []string {
	if values == nil {
		return []string{}
	}
	result := make([]string, 0, values.Cardinality())
	for value := range values.Iter() {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func intSetToSlice(values mapset.Set[int]) []int {
	if values == nil {
		return []int{}
	}
	return values.ToSlice()
}

func marshalPythonJSON(value any, indent bool) ([]byte, error) {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if indent {
		encoder.SetIndent("", "  ")
	}
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

func spacePythonJSONSeparators(data []byte) []byte {
	out := make([]byte, 0, len(data)+8)
	inString := false
	escaped := false
	for _, b := range data {
		out = append(out, b)
		if escaped {
			escaped = false
			continue
		}
		if inString {
			if b == '\\' {
				escaped = true
			} else if b == '"' {
				inString = false
			}
			continue
		}
		if b == '"' {
			inString = true
			continue
		}
		if b == ':' || b == ',' {
			out = append(out, ' ')
		}
	}
	return out
}

func loadJSONMap(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil
	}
	return result
}

func stringFromAny(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return ""
}

func intFromAny(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case float64:
		return int(v)
	default:
		return 0
	}
}

func floatFromAny(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int:
		return float64(v)
	default:
		return 0
	}
}

func anySlice(value any) []any {
	if values, ok := value.([]any); ok {
		return values
	}
	return nil
}
