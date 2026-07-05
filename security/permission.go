package security

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	mapset "github.com/deckarep/golang-set/v2"
)

const (
	shellRulePrefix = "Bash("
)

type PermissionState struct {
	ConfirmedPaths        mapset.Set[string] `json:"confirmed_paths"`
	ConfirmedTools        mapset.Set[string] `json:"confirmed_tools"`
	ConfirmedCommandRules mapset.Set[string] `json:"confirmed_command_rules"`
	YoloMode              bool               `json:"yolo_mode"`
}

type CommandRuleAnalyzer struct {
	GetSimpleCommandPrefix func(string) string
	NeedsUserDecision      func(string) bool
}

var (
	commandRuleAnalyzerMu sync.RWMutex
	commandRuleAnalyzer   CommandRuleAnalyzer
)

func RegisterCommandRuleAnalyzer(analyzer CommandRuleAnalyzer) {
	commandRuleAnalyzerMu.Lock()
	defer commandRuleAnalyzerMu.Unlock()
	commandRuleAnalyzer = analyzer
}

func DefaultPermissionState() *PermissionState {
	return &PermissionState{
		ConfirmedPaths:        mapset.NewSet[string](),
		ConfirmedTools:        mapset.NewSet[string](),
		ConfirmedCommandRules: mapset.NewSet[string](),
		YoloMode:              false,
	}
}

func (ps *PermissionState) IsPathConfirmed(path string) bool {
	if ps.YoloMode {
		return true
	}
	normalized := normalizePermissionPath(path)
	confirmedNormalized := make(map[string]struct{}, ps.ConfirmedPaths.Cardinality())
	for confirmedPath := range ps.ConfirmedPaths.Iter() {
		confirmedNormalized[normalizePermissionStoredPath(confirmedPath)] = struct{}{}
	}
	parts := strings.Split(normalized, "/")
	for i := 0; i <= len(parts); i++ {
		prefix := strings.Join(parts[:i], "/")
		if prefix == "" {
			prefix = "/"
		}
		if _, ok := confirmedNormalized[prefix]; ok {
			return true
		}
	}
	return false
}

func (ps *PermissionState) ConfirmPath(path string) {
	ps.ConfirmedPaths.Add(normalizePermissionPath(path))
}

func (ps *PermissionState) IsToolConfirmed(toolName string) bool {
	if ps.YoloMode {
		return true
	}
	return ps.ConfirmedTools.Contains(toolName)
}

func (ps *PermissionState) ConfirmTool(toolName string) {
	ps.ConfirmedTools.Add(toolName)
}

func (ps *PermissionState) ConfirmCommandPrefix(prefix string) {
	normalized := strings.TrimSpace(prefix)
	if normalized == "" {
		return
	}
	ps.ConfirmedCommandRules.Add(commandRuleForPrefix(normalized))
}

func (ps *PermissionState) IsCommandPrefixConfirmed(prefix string) bool {
	if ps == nil {
		return false
	}
	if ps.YoloMode {
		return true
	}
	normalized := strings.TrimSpace(prefix)
	if normalized == "" {
		return false
	}
	return ps.ConfirmedCommandRules.Contains(commandRuleForPrefix(normalized))
}

func (ps *PermissionState) IsCommandRuleConfirmed(command string) bool {
	if ps == nil {
		return false
	}
	if ps.YoloMode {
		return true
	}
	commandRuleAnalyzerMu.RLock()
	analyzer := commandRuleAnalyzer
	commandRuleAnalyzerMu.RUnlock()
	if analyzer.GetSimpleCommandPrefix == nil || analyzer.NeedsUserDecision == nil {
		return false
	}
	prefix := analyzer.GetSimpleCommandPrefix(command)
	if prefix == "" {
		return false
	}
	if analyzer.NeedsUserDecision(command) {
		return false
	}
	return ps.ConfirmedCommandRules.Contains(commandRuleForPrefix(prefix))
}

func commandRuleForPrefix(prefix string) string {
	return fmt.Sprintf("%s%s:*)", shellRulePrefix, prefix)
}

func (ps *PermissionState) ToMap() (map[string]any, error) {
	if ps == nil {
		ps = DefaultPermissionState()
	}
	return map[string]any{
		"confirmed_paths":         setToSortedSlice(ps.ConfirmedPaths),
		"confirmed_tools":         setToSortedSlice(ps.ConfirmedTools),
		"confirmed_command_rules": setToSortedSlice(ps.ConfirmedCommandRules),
		"yolo_mode":               ps.YoloMode,
	}, nil
}

func GetPermissionStateFromMap(m map[string]any) (PermissionState, error) {
	state := DefaultPermissionState()
	for _, path := range stringSliceFromAny(m["confirmed_paths"]) {
		state.ConfirmedPaths.Add(path)
	}
	for _, tool := range stringSliceFromAny(m["confirmed_tools"]) {
		state.ConfirmedTools.Add(tool)
	}
	rules := stringSliceFromAny(m["confirmed_command_rules"])
	if len(rules) == 0 {
		rules = stringSliceFromAny(m["confitment_commad_rules"])
	}
	for _, rule := range rules {
		state.ConfirmedCommandRules.Add(rule)
	}
	if yolo, ok := m["yolo_mode"].(bool); ok {
		state.YoloMode = yolo
	}
	return *state, nil
}

func NeedUserPermission(toolName string, toolInput any, state *PermissionState) bool {
	if state == nil {
		return false
	}
	if state.YoloMode {
		return false
	}
	if state.IsToolConfirmed(toolName) {
		return false
	}
	filePath := extractFilePath(toolInput)
	if filePath != "" && state.IsPathConfirmed(filePath) {
		return false
	}
	return true
}

func setToSortedSlice(set mapset.Set[string]) []string {
	if set == nil {
		return nil
	}
	out := make([]string, 0, set.Cardinality())
	for value := range set.Iter() {
		out = append(out, value)
	}
	sortStrings(out)
	return out
}

func stringSliceFromAny(raw any) []string {
	switch values := raw.(type) {
	case []string:
		return values
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if s, ok := value.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func extractFilePath(toolInput any) string {
	switch v := toolInput.(type) {
	case map[string]any:
		if filePath, ok := v["file_path"].(string); ok {
			return filePath
		}

	case map[string]string:
		if filePath := v["file_path"]; filePath != "" {
			return filePath
		}

	case interface{ GetFilePath() string }:
		return v.GetFilePath()

	case interface{ FilePath() string }:
		return v.FilePath()
	}
	return ""
}

func normalizePermissionPath(path string) string {
	absPath, err := filepath.Abs(path)
	if err != nil {
		absPath = path
	}
	return filepath.ToSlash(resolvePermissionPathBestEffort(absPath))
}

func normalizePermissionStoredPath(path string) string {
	return filepath.ToSlash(filepath.Clean(path))
}

func resolvePermissionPathBestEffort(path string) string {
	cleaned := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(resolved)
	}
	current := cleaned
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			parts := append([]string{resolved}, suffix...)
			return filepath.Clean(filepath.Join(parts...))
		}
		parent := filepath.Dir(current)
		if parent == current {
			return cleaned
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		current = parent
	}
}
