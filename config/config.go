package config

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

const CompressionTriggerRatio = 0.80

type Config struct {
	APIKey       string
	APIBaseURL   string
	APIModel     string
	APIType      string
	APIMaxTokens int
	PinnedFields map[string]bool

	Yolo bool

	MaxToolOutputChars         int
	MaxToolResultCharsAbsolute int
	MaxMessageToolResultsChars int
	ShellTimeoutSeconds        float64
	ShellMaxOutputBytes        int

	MCPEnabled        bool
	MCPPingInterval   float64
	MCPConnectTimeout float64
	MCPRequestTimeout float64

	ContextCompressThreshold   float64
	PromptCacheTTLSeconds      float64
	AnthropicCacheEditsEnabled bool
	MaxParentTurns             int

	SessionDir string

	APIInputPricePer1K  *float64
	APIOutputPricePer1K *float64

	AutoMemoryEnabled                  bool
	AutoMemoryDirectory                *string
	ExtractionModel                    *string
	MemoryRecallPrefetchTimeoutSeconds float64

	SkillsEnabled              bool
	SkillsDir                  string
	UserSkillsDir              string
	BundledSkillsDir           string
	SystemPromptPath           string
	MemoryExtractionPromptPath string

	UIBackend string

	WorktreeBaseRef string
	WorktreeDir     string

	CWD string
}

func NewConfig() Config {
	cwd, _ := os.Getwd()
	return NewConfigForCWD(cwd)
}

func NewConfigForCWD(cwd string) Config {
	homeDir, _ := os.UserHomeDir()
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	luminaRoot := FindLuminaRoot(cwd)
	if luminaRoot == "" {
		luminaRoot = cwd
	}
	resourceDir := LuminaResourceDir(luminaRoot)

	cfg := Config{
		APIKey:       "",
		APIBaseURL:   "",
		APIModel:     "",
		APIType:      "openai_compatible",
		APIMaxTokens: 16000,

		Yolo: false,

		MaxToolOutputChars:         50_000,
		MaxToolResultCharsAbsolute: 400_000,
		MaxMessageToolResultsChars: 200_000,
		ShellTimeoutSeconds:        30.0,
		ShellMaxOutputBytes:        5 * 1024 * 1024,

		MCPEnabled:        true,
		MCPPingInterval:   30.0,
		MCPConnectTimeout: 10.0,
		MCPRequestTimeout: 30.0,

		ContextCompressThreshold:   CompressionTriggerRatio,
		PromptCacheTTLSeconds:      300,
		AnthropicCacheEditsEnabled: false,
		MaxParentTurns:             100,

		SessionDir: filepath.Join(homeDir, ".Lumina", "sessions"),

		APIInputPricePer1K:  nil,
		APIOutputPricePer1K: nil,

		AutoMemoryEnabled:                  true,
		AutoMemoryDirectory:                nil,
		ExtractionModel:                    nil,
		MemoryRecallPrefetchTimeoutSeconds: 0.25,

		SkillsEnabled:              true,
		SkillsDir:                  ".Lumina/PROJECT_SKILLS",
		UserSkillsDir:              filepath.Join(homeDir, ".Lumina", "skills"),
		BundledSkillsDir:           filepath.Join(resourceDir, "SKILLS"),
		SystemPromptPath:           filepath.Join(resourceDir, "SYSTEM", "system-prompt.md"),
		MemoryExtractionPromptPath: filepath.Join(resourceDir, "SYSTEM", "extraction_system.md"),

		UIBackend: "prompt_toolkit_fullscreen",

		WorktreeBaseRef: "HEAD",
		WorktreeDir:     ".Lumina/worktrees",

		CWD: cwd,
	}
	applyLuminaDefaults(&cfg, filepath.Join(resourceDir, "CONFIG", "defaults.json"), cwd, resourceDir)
	applyEnvOverrides(&cfg)
	return cfg
}

func ReloadDynamicConfig(current Config) Config {
	fresh := NewConfigForCWD(current.CWD)
	updated := current
	if !isPinned(current, "api_key") {
		updated.APIKey = fresh.APIKey
	}
	if !isPinned(current, "api_base_url") {
		updated.APIBaseURL = fresh.APIBaseURL
	}
	if !isPinned(current, "api_model") {
		updated.APIModel = fresh.APIModel
	}
	if !isPinned(current, "api_type") {
		updated.APIType = fresh.APIType
	}
	if !isPinned(current, "api_max_tokens") {
		updated.APIMaxTokens = fresh.APIMaxTokens
	}
	updated.MaxToolOutputChars = fresh.MaxToolOutputChars
	updated.MaxToolResultCharsAbsolute = fresh.MaxToolResultCharsAbsolute
	updated.MaxMessageToolResultsChars = fresh.MaxMessageToolResultsChars
	updated.ShellTimeoutSeconds = fresh.ShellTimeoutSeconds
	updated.ShellMaxOutputBytes = fresh.ShellMaxOutputBytes
	updated.ContextCompressThreshold = fresh.ContextCompressThreshold
	updated.PromptCacheTTLSeconds = fresh.PromptCacheTTLSeconds
	updated.AnthropicCacheEditsEnabled = fresh.AnthropicCacheEditsEnabled
	updated.MaxParentTurns = fresh.MaxParentTurns
	updated.APIInputPricePer1K = fresh.APIInputPricePer1K
	updated.APIOutputPricePer1K = fresh.APIOutputPricePer1K
	updated.ExtractionModel = fresh.ExtractionModel
	updated.MemoryRecallPrefetchTimeoutSeconds = fresh.MemoryRecallPrefetchTimeoutSeconds
	return updated
}

func PinFields(cfg *Config, fields ...string) {
	if cfg == nil {
		return
	}
	if cfg.PinnedFields == nil {
		cfg.PinnedFields = map[string]bool{}
	}
	for _, field := range fields {
		cfg.PinnedFields[field] = true
	}
}

func isPinned(cfg Config, field string) bool {
	return cfg.PinnedFields != nil && cfg.PinnedFields[field]
}

func (c Config) CompressionContextLimit() int {
	if c.APIMaxTokens > 0 {
		return c.APIMaxTokens
	}
	return 16000
}

func (c Config) CompressionThreshold() float64 {
	if c.ContextCompressThreshold > 0 {
		return c.ContextCompressThreshold
	}
	return CompressionTriggerRatio
}

func (c Config) CompressionTriggerTokens() int {
	limit := c.CompressionContextLimit()
	if limit <= 0 {
		return 0
	}
	return int(math.Floor(float64(limit) * c.CompressionThreshold()))
}

type luminaDefaults struct {
	APIKey                             *string  `json:"api_key"`
	APIBaseURL                         *string  `json:"api_base_url"`
	APIModel                           *string  `json:"api_model"`
	APIType                            *string  `json:"api_type"`
	APIMaxTokens                       *int     `json:"api_max_tokens"`
	MaxToolOutputChars                 *int     `json:"max_tool_output_chars"`
	MaxToolResultCharsAbsolute         *int     `json:"max_tool_result_chars_absolute"`
	MaxMessageToolResultsChars         *int     `json:"max_message_tool_results_chars"`
	ShellTimeoutSeconds                *float64 `json:"shell_timeout_seconds"`
	ShellMaxOutputBytes                *int     `json:"shell_max_output_bytes"`
	MCPEnabled                         *bool    `json:"mcp_enabled"`
	MCPPingInterval                    *float64 `json:"mcp_ping_interval"`
	MCPConnectTimeout                  *float64 `json:"mcp_connect_timeout"`
	MCPRequestTimeout                  *float64 `json:"mcp_request_timeout"`
	ContextCompressThreshold           *float64 `json:"context_compress_threshold"`
	PromptCacheTTLSeconds              *float64 `json:"prompt_cache_ttl_seconds"`
	AnthropicCacheEditsEnabled         *bool    `json:"anthropic_cache_edits_enabled"`
	MaxParentTurns                     *int     `json:"max_parent_turns"`
	SessionDir                         *string  `json:"session_dir"`
	APIInputPricePer1K                 *float64 `json:"api_input_price_per_1k"`
	APIOutputPricePer1K                *float64 `json:"api_output_price_per_1k"`
	AutoMemoryEnabled                  *bool    `json:"auto_memory_enabled"`
	AutoMemoryDirectory                *string  `json:"auto_memory_directory"`
	ExtractionModel                    *string  `json:"extraction_model"`
	MemoryRecallPrefetchTimeoutSeconds *float64 `json:"memory_recall_prefetch_timeout_seconds"`
	SkillsEnabled                      *bool    `json:"skills_enabled"`
	SkillsDir                          *string  `json:"skills_dir"`
	UserSkillsDir                      *string  `json:"user_skills_dir"`
	BundledSkillsDir                   *string  `json:"bundled_skills_dir"`
	SystemPromptPath                   *string  `json:"system_prompt_path"`
	MemoryExtractionPromptPath         *string  `json:"memory_extraction_prompt_path"`
	UIBackend                          *string  `json:"ui_backend"`
	WorktreeBaseRef                    *string  `json:"worktree_base_ref"`
	WorktreeDir                        *string  `json:"worktree_dir"`
}

func applyLuminaDefaults(cfg *Config, path string, cwd string, resourceDir string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var defaults luminaDefaults
	if err := json.Unmarshal(data, &defaults); err != nil {
		return
	}
	if defaults.APIKey != nil {
		cfg.APIKey = *defaults.APIKey
	}
	if defaults.APIBaseURL != nil {
		cfg.APIBaseURL = *defaults.APIBaseURL
	}
	if defaults.APIModel != nil {
		cfg.APIModel = *defaults.APIModel
	}
	if defaults.APIType != nil {
		cfg.APIType = *defaults.APIType
	}
	if defaults.APIMaxTokens != nil {
		cfg.APIMaxTokens = *defaults.APIMaxTokens
	}
	if defaults.MaxToolOutputChars != nil {
		cfg.MaxToolOutputChars = *defaults.MaxToolOutputChars
	}
	if defaults.MaxToolResultCharsAbsolute != nil {
		cfg.MaxToolResultCharsAbsolute = *defaults.MaxToolResultCharsAbsolute
	}
	if defaults.MaxMessageToolResultsChars != nil {
		cfg.MaxMessageToolResultsChars = *defaults.MaxMessageToolResultsChars
	}
	if defaults.ShellTimeoutSeconds != nil {
		cfg.ShellTimeoutSeconds = *defaults.ShellTimeoutSeconds
	}
	if defaults.ShellMaxOutputBytes != nil {
		cfg.ShellMaxOutputBytes = *defaults.ShellMaxOutputBytes
	}
	if defaults.MCPEnabled != nil {
		cfg.MCPEnabled = *defaults.MCPEnabled
	}
	if defaults.MCPPingInterval != nil {
		cfg.MCPPingInterval = *defaults.MCPPingInterval
	}
	if defaults.MCPConnectTimeout != nil {
		cfg.MCPConnectTimeout = *defaults.MCPConnectTimeout
	}
	if defaults.MCPRequestTimeout != nil {
		cfg.MCPRequestTimeout = *defaults.MCPRequestTimeout
	}
	if defaults.ContextCompressThreshold != nil {
		cfg.ContextCompressThreshold = *defaults.ContextCompressThreshold
	}
	if defaults.PromptCacheTTLSeconds != nil {
		cfg.PromptCacheTTLSeconds = *defaults.PromptCacheTTLSeconds
	}
	if defaults.AnthropicCacheEditsEnabled != nil {
		cfg.AnthropicCacheEditsEnabled = *defaults.AnthropicCacheEditsEnabled
	}
	if defaults.MaxParentTurns != nil {
		cfg.MaxParentTurns = *defaults.MaxParentTurns
	}
	if defaults.SessionDir != nil {
		cfg.SessionDir = resolveProjectPath(cwd, *defaults.SessionDir)
	}
	if defaults.APIInputPricePer1K != nil {
		cfg.APIInputPricePer1K = defaults.APIInputPricePer1K
	}
	if defaults.APIOutputPricePer1K != nil {
		cfg.APIOutputPricePer1K = defaults.APIOutputPricePer1K
	}
	if defaults.AutoMemoryEnabled != nil {
		cfg.AutoMemoryEnabled = *defaults.AutoMemoryEnabled
	}
	if defaults.AutoMemoryDirectory != nil {
		resolved := resolveProjectPath(cwd, *defaults.AutoMemoryDirectory)
		cfg.AutoMemoryDirectory = &resolved
	}
	if defaults.ExtractionModel != nil {
		cfg.ExtractionModel = defaults.ExtractionModel
	}
	if defaults.MemoryRecallPrefetchTimeoutSeconds != nil {
		cfg.MemoryRecallPrefetchTimeoutSeconds = *defaults.MemoryRecallPrefetchTimeoutSeconds
	}
	if defaults.SkillsEnabled != nil {
		cfg.SkillsEnabled = *defaults.SkillsEnabled
	}
	if defaults.SkillsDir != nil {
		cfg.SkillsDir = *defaults.SkillsDir
	}
	if defaults.UserSkillsDir != nil {
		cfg.UserSkillsDir = expandHome(*defaults.UserSkillsDir)
	}
	if defaults.BundledSkillsDir != nil {
		cfg.BundledSkillsDir = resolveResourcePath(resourceDir, *defaults.BundledSkillsDir)
	}
	if defaults.SystemPromptPath != nil {
		cfg.SystemPromptPath = resolveResourcePath(resourceDir, *defaults.SystemPromptPath)
	}
	if defaults.MemoryExtractionPromptPath != nil {
		cfg.MemoryExtractionPromptPath = resolveResourcePath(resourceDir, *defaults.MemoryExtractionPromptPath)
	}
	if defaults.UIBackend != nil {
		cfg.UIBackend = *defaults.UIBackend
	}
	if defaults.WorktreeBaseRef != nil {
		cfg.WorktreeBaseRef = *defaults.WorktreeBaseRef
	}
	if defaults.WorktreeDir != nil {
		cfg.WorktreeDir = *defaults.WorktreeDir
	}
}

func FindLuminaRoot(start string) string {
	if root := normalizeLuminaRoot(os.Getenv("LUMINA_RESOURCE_ROOT")); root != "" {
		return root
	}
	if root := normalizeLuminaRoot(os.Getenv("LUMINA_HOME")); root != "" {
		return root
	}
	for _, candidate := range luminaRootCandidates(start) {
		if root := findLuminaRootFrom(candidate); root != "" {
			return root
		}
	}
	return ""
}

func luminaRootCandidates(start string) []string {
	var candidates []string
	if start != "" {
		candidates = append(candidates, start)
	}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		candidates = append(candidates, cwd)
	}
	if exe, err := os.Executable(); err == nil && exe != "" {
		candidates = append(candidates, filepath.Dir(exe))
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		candidates = append(candidates, filepath.Join(home, ".lumina"))
	}
	if _, file, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Dir(file))
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		abs, err := filepath.Abs(candidate)
		if err != nil {
			abs = candidate
		}
		if _, exists := seen[abs]; exists {
			continue
		}
		seen[abs] = struct{}{}
		out = append(out, abs)
	}
	return out
}

func findLuminaRootFrom(start string) string {
	if start == "" {
		return ""
	}
	current, err := filepath.Abs(start)
	if err != nil {
		current = start
	}
	if info, err := os.Stat(current); err == nil && !info.IsDir() {
		current = filepath.Dir(current)
	}
	for {
		if hasLuminaResources(current) {
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

func normalizeLuminaRoot(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	path = expandHome(path)
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	if hasLuminaResources(path) {
		return path
	}
	return ""
}

func hasLuminaResources(root string) bool {
	if hasDirectLuminaResources(root) {
		return true
	}
	return hasNestedLuminaResources(root)
}

func LuminaResourceDir(root string) string {
	if hasDirectLuminaResources(root) {
		return root
	}
	if hasNestedLuminaResources(root) {
		return filepath.Join(root, ".Lumina")
	}
	return filepath.Join(root, ".Lumina")
}

func LuminaResourcePath(root string, elems ...string) string {
	parts := append([]string{LuminaResourceDir(root)}, elems...)
	return filepath.Join(parts...)
}

func hasDirectLuminaResources(root string) bool {
	for _, rel := range []string{
		filepath.Join("CONFIG", "defaults.json"),
		filepath.Join("SYSTEM", "system-prompt.md"),
		"SKILLS",
	} {
		if exactPathExists(root, rel) {
			return true
		}
	}
	return false
}

func hasNestedLuminaResources(root string) bool {
	for _, rel := range []string{
		filepath.Join(".Lumina", "CONFIG", "defaults.json"),
		filepath.Join(".Lumina", "SYSTEM", "system-prompt.md"),
		filepath.Join(".Lumina", "SKILLS"),
	} {
		if exactPathExists(root, rel) {
			return true
		}
	}
	return false
}

func exactPathExists(root, rel string) bool {
	current := root
	for _, part := range strings.Split(filepath.Clean(rel), string(filepath.Separator)) {
		if part == "." || part == "" {
			continue
		}
		entries, err := os.ReadDir(current)
		if err != nil {
			return false
		}
		found := false
		for _, entry := range entries {
			if entry.Name() == part {
				found = true
				break
			}
		}
		if !found {
			return false
		}
		current = filepath.Join(current, part)
	}
	_, err := os.Stat(current)
	return err == nil
}

func applyEnvOverrides(cfg *Config) {
	cfg.APIKey = firstNonEmpty(os.Getenv("LUMINA_API_KEY"), os.Getenv("ANTHROPIC_API_KEY"), cfg.APIKey)
	cfg.APIBaseURL = firstNonEmpty(os.Getenv("LUMINA_API_BASE_URL"), os.Getenv("ANTHROPIC_BASE_URL"), cfg.APIBaseURL)
	cfg.APIModel = firstNonEmpty(os.Getenv("LUMINA_API_MODEL"), os.Getenv("ANTHROPIC_MODEL"), cfg.APIModel)
	cfg.APIType = firstNonEmpty(os.Getenv("LUMINA_API_TYPE"), cfg.APIType)
	cfg.Yolo = envBool("YOLO_MODE", cfg.Yolo)
	cfg.PromptCacheTTLSeconds = envFloat("LUMINA_PROMPT_CACHE_TTL_SECONDS", cfg.PromptCacheTTLSeconds)
	cfg.AnthropicCacheEditsEnabled = envBool("LUMINA_ANTHROPIC_CACHE_EDITS", cfg.AnthropicCacheEditsEnabled)
	if inputPrice := envOptionalFloat("LUMINA_INPUT_PRICE_PER_1K"); inputPrice != nil {
		cfg.APIInputPricePer1K = inputPrice
	}
	if outputPrice := envOptionalFloat("LUMINA_OUTPUT_PRICE_PER_1K"); outputPrice != nil {
		cfg.APIOutputPricePer1K = outputPrice
	}
	cfg.UIBackend = "prompt_toolkit_fullscreen"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func resolveProjectPath(cwd, path string) string {
	path = expandHome(path)
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(cwd, path)
}

func resolveResourcePath(resourceDir, path string) string {
	path = expandHome(path)
	if filepath.IsAbs(path) {
		return path
	}
	clean := filepath.Clean(path)
	if clean == ".Lumina" {
		return resourceDir
	}
	prefix := ".Lumina" + string(filepath.Separator)
	if strings.HasPrefix(clean, prefix) {
		clean = strings.TrimPrefix(clean, prefix)
	}
	if filepath.Separator != '/' {
		slashPrefix := ".Lumina/"
		if strings.HasPrefix(clean, slashPrefix) {
			clean = strings.TrimPrefix(clean, slashPrefix)
		}
	}
	return filepath.Join(resourceDir, clean)
}

func expandHome(path string) string {
	if path == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}

func envBool(key string, fallback bool) bool {
	value := strings.ToLower(strings.TrimSpace(os.Getenv(key)))
	if value == "" {
		return fallback
	}

	switch value {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	default:
		return fallback
	}
}

func envFloat(key string, fallback float64) float64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}

	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}

	return parsed
}

func envOptionalFloat(key string) *float64 {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return nil
	}

	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return nil
	}

	return &parsed
}

var (
	configMu sync.RWMutex
	config   *Config
)

func GetConfig() Config {
	configMu.RLock()
	if config != nil {
		defer configMu.RUnlock()
		return *config
	}
	configMu.RUnlock()

	configMu.Lock()
	defer configMu.Unlock()

	if config == nil {
		cfg := NewConfig()
		config = &cfg
	}

	return *config
}

func SetConfig(c Config) {
	configMu.Lock()
	defer configMu.Unlock()

	config = &c
}

func GetConfigPtr() *Config {
	configMu.RLock()
	if config != nil {
		defer configMu.RUnlock()
		return config
	}
	configMu.RUnlock()

	configMu.Lock()
	defer configMu.Unlock()

	if config == nil {
		cfg := NewConfig()
		config = &cfg
	}

	return config
}
