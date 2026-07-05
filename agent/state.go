package agent

import (
	"LuminaCode/security"
	"LuminaCode/tools/file"
	"bytes"
	"encoding/json"

	mapset "github.com/deckarep/golang-set/v2"
)

type AgentState struct {
	Messages                       []map[string]any `json:"messages"`
	TotalInputTokens               int              `json:"total_input_tokens"`
	TotalOutputTokens              int              `json:"total_output_tokens"`
	CacheReadInputTokens           int              `json:"cache_read_input_tokens"`
	CacheCreateInputTokens         int              `json:"cache_creation_input_tokens"`
	ServerToolUseInputTokens       int              `json:"server_tool_use_input_tokens"`
	TurnCount                      int              `json:"turn_count"`
	ConsecutiveAutoCompactFailures int              `json:"consecutive_autocompact_failures"`
	LastContinueReason             string           `json:"last_continue_reason"`

	// permission tracking
	PermissionState *security.PermissionState `json:"permission_state"`
	// Denied tool calls
	DeniedToolCalls map[string]int `json:"denied_tool_calls"`

	ToolErrors map[string]int `json:"tool_errors"`

	SystemPrompt string `json:"system_prompt"`

	RecentApiErrors []string `json:"recent_api_errors"`

	ContentReplacements map[string]string `json:"content_replacements"`

	TotalTaskBudget     *int `json:"total_task_budget"`
	TaskBudgetRemaining *int `json:"task_budget_remaining"`

	CacheBreakPoints mapset.Set[int] `json:"cache_breakpoints"`

	ReadFileState map[string]file.FileStateEntry `json:"read_file_state"`

	LastExtractionTurn          int  `json:"last_extraction_turn"`
	UserTurnCount               int  `json:"user_turn_count"`
	LastExtractionUserTurn      int  `json:"last_extraction_user_turn"`
	MemoryWritesSinceExtraction bool `json:"memory_writes_since_extraction"`

	LastQuery string `json:"last_query"`
}

func NewAgentState() AgentState {
	return AgentState{
		Messages:                       []map[string]any{},
		TotalInputTokens:               0,
		TotalOutputTokens:              0,
		CacheReadInputTokens:           0,
		CacheCreateInputTokens:         0,
		ServerToolUseInputTokens:       0,
		TurnCount:                      0,
		ConsecutiveAutoCompactFailures: 0,
		LastContinueReason:             "",

		PermissionState: security.DefaultPermissionState(),

		DeniedToolCalls: map[string]int{},
		ToolErrors:      map[string]int{},

		SystemPrompt: "",

		RecentApiErrors: []string{},

		ContentReplacements: map[string]string{},

		TotalTaskBudget:     nil,
		TaskBudgetRemaining: nil,

		CacheBreakPoints: mapset.NewSet[int](),

		ReadFileState: map[string]file.FileStateEntry{},

		LastExtractionTurn:          0,
		UserTurnCount:               0,
		LastExtractionUserTurn:      0,
		MemoryWritesSinceExtraction: false,

		LastQuery: "",
	}
}

func (s AgentState) ToMap() (map[string]any, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(b))
	decoder.UseNumber()
	var result map[string]any
	if err := decoder.Decode(&result); err != nil {
		return nil, err
	}
	return result, nil
}

func GetAgentStateFromMap(m map[string]any) (AgentState, error) {
	rawCacheBreakpoints, hasCacheBreakpoints := m["cache_breakpoints"]
	if !hasCacheBreakpoints {
		rawCacheBreakpoints = m["cache_break_points"]
	}
	permissionRaw, _ := m["permission_state"].(map[string]any)
	m = copyAnyMap(m)
	delete(m, "cache_breakpoints")
	delete(m, "cache_break_points")
	delete(m, "permission_state")
	b, err := json.Marshal(m)
	if err != nil {
		return AgentState{}, err
	}

	result := NewAgentState()
	if err := json.Unmarshal(b, &result); err != nil {
		return AgentState{}, err
	}
	if permissionRaw != nil {
		if permissionState, err := security.GetPermissionStateFromMap(permissionRaw); err == nil {
			result.PermissionState = &permissionState
		}
	}
	result.CacheBreakPoints = intSetFromAny(rawCacheBreakpoints)

	return result, nil
}

func (s *AgentState) SetReadFileState(path string, entry file.FileStateEntry) {
	if s.ReadFileState == nil {
		s.ReadFileState = map[string]file.FileStateEntry{}
	}
	s.ReadFileState[path] = entry
}

func (s *AgentState) GetReadFileState(path string) (file.FileStateEntry, bool) {
	if s.ReadFileState == nil {
		return file.FileStateEntry{}, false
	}
	entry, ok := s.ReadFileState[path]
	return entry, ok
}

func (s *AgentState) TurnCountValue() int {
	if s == nil {
		return 0
	}
	return s.TurnCount
}

func (s *AgentState) TokenTotals() (int, int) {
	if s == nil {
		return 0, 0
	}
	return s.TotalInputTokens, s.TotalOutputTokens
}

func (s *AgentState) YoloEnabled() bool {
	return s != nil && s.PermissionState != nil && s.PermissionState.YoloMode
}

func copyAnyMap(m map[string]any) map[string]any {
	cp := make(map[string]any, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return cp
}

func intSetFromAny(raw any) mapset.Set[int] {
	out := mapset.NewSet[int]()
	switch values := raw.(type) {
	case []int:
		for _, value := range values {
			out.Add(value)
		}
	case []any:
		for _, value := range values {
			if parsed, ok := intFromStateAny(value); ok {
				out.Add(parsed)
			}
		}
	case []float64:
		for _, value := range values {
			out.Add(int(value))
		}
	case []json.Number:
		for _, value := range values {
			if n, err := value.Int64(); err == nil {
				out.Add(int(n))
			}
		}
	}
	return out
}

func intFromStateAny(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		if n, err := v.Int64(); err == nil {
			return int(n), true
		}
	}
	return 0, false
}
