package config

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"

	"LuminaCode/apppaths"
)

const CompressionTriggerRatio = 0.80

type Config struct {
	Paths        apppaths.AppPaths
	ProjectPaths apppaths.ProjectPaths
	PathErrors   []string

	APIKey                      string
	APIBaseURL                  string
	APIModel                    string
	APIType                     string
	FallbackAPIEnabled          bool
	FallbackAPIKey              string
	FallbackAPIBaseURL          string
	FallbackAPIModel            string
	FallbackAPIType             string
	APIMaxTokens                int
	APIStreamIdleTimeoutSeconds float64
	PinnedFields                map[string]bool

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

	WebSearchEnabled        bool
	WebSearchProvider       string
	WebSearchBaseURL        string
	WebSearchMaxResults     int
	WebSearchTimeoutSeconds float64
	WebFetchEnabled         bool
	WebFetchRequireSearch   bool
	WebFetchMaxChars        int
	WebFetchTimeoutSeconds  float64
	WebFetchUserAgent       string
	WebSearchCacheScope     string

	ContextCompressThreshold   float64
	PromptCacheTTLSeconds      float64
	AnthropicCacheEditsEnabled bool
	MaxParentTurns             int

	SessionDir           string
	SessionArchiveDir    string
	SessionMemoryDir     string
	SessionMemoryAgentID string
	ProjectRuntimeDir    string

	SessionMemoryEnabled             bool
	SessionMemoryTurnInterval        int
	SessionMemorySummaryModel        string
	SessionMemorySummaryMaxTokens    int
	SessionHistoryGetMessageLimit    int
	SessionMemoryMaxCommits          int
	SessionMemoryMaxMessages         int
	SessionMemoryVacuumAfterEviction bool
	SessionMaintenanceEnabled        bool
	SessionMaintenanceMode           string
	SessionRetentionDays             int
	SessionMaxEntries                int
	SessionMaxDiskBytes              int64
	SessionHighWaterRatio            float64
	SessionArchiveBeforeDelete       bool
	SessionProtectPinned             bool
	TeamTimelineMaxEntries           int
	TeamDialogueMaxEntries           int
	TeamArtifactMaxBytes             int64

	LongTermMemoryEnabled bool
	MemoryBackend         string
	MemoryPath            string
	// MemoryArtifactPath is an internal runtime override for shared build
	// artifacts. It is intentionally not exposed as user config.
	MemoryArtifactPath               string
	MemoryRemoteProcessing           string
	MemoryCompilerModel              string
	MemoryConflictModel              string
	MemoryCompileBatchTokens         int
	MemorySearchLatencyMS            int
	MemoryCandidateLimit             int
	MemoryContextTargetTokens        int
	MemoryContextMaxTokens           int
	MemoryRecallMaxItems             int
	MemoryEmbeddingEnabled           bool
	MemoryEmbeddingModel             string
	MemoryEmbeddingModelDir          string
	MemoryEmbeddingBatchSize         int
	MemoryEmbeddingBatchWaitMS       int
	MemoryEmbeddingQueryCacheEntries int
	MemoryEmbeddingExecutionTimeout  float64
	MemoryBGEEnabled                 bool
	MemoryBGEModelDir                string
	MemoryConfigErrors               []string

	SkillsEnabled       bool
	SkillsDir           string
	UserSkillsDir       string
	BundledSkillsDir    string
	IsolatedSkillsOnly  bool
	TeamDir             string
	BundledTeamDir      string
	SystemPromptPath    string
	ProjectRootMarkers  []string
	ProjectDocFilenames []string
	ProjectDocMaxBytes  int

	UIBackend   string
	HarnessMode string

	WorktreeBaseRef string
	WorktreeDir     string

	CWD string
}

func NewConfig() Config {
	cwd, _ := os.Getwd()
	return NewConfigForCWD(cwd)
}

func NewConfigForCWD(cwd string) Config {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	paths, pathsErr := apppaths.ResolveCurrent()
	if pathsErr != nil {
		homeDir := userHomeDir()
		paths, _ = apppaths.Resolve(apppaths.ResolveOptions{GOOS: runtime.GOOS, HomeDir: homeDir, Env: map[string]string{}})
	}
	projectRoot, projectRootErr := apppaths.DiscoverProjectRoot(cwd, []string{".git"})
	if projectRootErr != nil {
		projectRoot = cwd
	}
	projectPaths, projectErr := paths.ForProject(projectRoot)
	luminaRoot := FindLuminaRoot(cwd)
	if luminaRoot == "" {
		luminaRoot = paths.ResourcesDir
	}
	resourceDir := LuminaResourceDir(luminaRoot)
	bundledSystemDir := resourceSubdir(resourceDir, apppaths.LegacySystemDirName, "system")
	bundledSkillsDir := resourceSubdir(resourceDir, apppaths.LegacySkillsDirName, "skills")
	bundledTeamsDir := resourceSubdir(resourceDir, apppaths.LegacyTeamsDirName, "teams")

	cfg := Config{
		Paths:                       paths,
		ProjectPaths:                projectPaths,
		APIKey:                      "",
		APIBaseURL:                  "",
		APIModel:                    "",
		APIType:                     "openai_compatible",
		FallbackAPIEnabled:          false,
		FallbackAPIKey:              "",
		FallbackAPIBaseURL:          "",
		FallbackAPIModel:            "",
		FallbackAPIType:             "auto",
		APIMaxTokens:                16000,
		APIStreamIdleTimeoutSeconds: 600.0,

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

		WebSearchEnabled:        true,
		WebSearchProvider:       "searxng",
		WebSearchBaseURL:        "http://127.0.0.1:8888",
		WebSearchMaxResults:     10,
		WebSearchTimeoutSeconds: 20.0,
		WebFetchEnabled:         true,
		WebFetchRequireSearch:   true,
		WebFetchMaxChars:        80_000,
		WebFetchTimeoutSeconds:  20.0,
		WebFetchUserAgent:       "LuminaCode/1.0 (+https://github.com/bugcat9/LuminaCode)",

		ContextCompressThreshold:   CompressionTriggerRatio,
		PromptCacheTTLSeconds:      300,
		AnthropicCacheEditsEnabled: false,
		MaxParentTurns:             100,

		SessionDir:        paths.ActiveSessionsDir,
		SessionArchiveDir: paths.ArchivedSessionsDir,
		ProjectRuntimeDir: projectPaths.StateDir,

		SessionMemoryEnabled:             true,
		SessionMemoryTurnInterval:        5,
		SessionMemorySummaryModel:        "",
		SessionMemorySummaryMaxTokens:    800,
		SessionHistoryGetMessageLimit:    20,
		SessionMemoryMaxCommits:          200,
		SessionMemoryMaxMessages:         4000,
		SessionMemoryVacuumAfterEviction: false,
		SessionMaintenanceEnabled:        true,
		SessionMaintenanceMode:           "warn",
		SessionRetentionDays:             30,
		SessionMaxEntries:                500,
		SessionMaxDiskBytes:              0,
		SessionHighWaterRatio:            0.8,
		SessionArchiveBeforeDelete:       true,
		SessionProtectPinned:             true,
		TeamTimelineMaxEntries:           2000,
		TeamDialogueMaxEntries:           1000,
		TeamArtifactMaxBytes:             0,

		LongTermMemoryEnabled:            true,
		MemoryBackend:                    "fabric",
		MemoryPath:                       filepath.Join(paths.MemoryDir, "fabric"),
		MemoryRemoteProcessing:           "redacted",
		MemoryCompilerModel:              "inherit",
		MemoryConflictModel:              "inherit",
		MemoryCompileBatchTokens:         10_000,
		MemorySearchLatencyMS:            2_000,
		MemoryCandidateLimit:             64,
		MemoryContextTargetTokens:        2_500,
		MemoryContextMaxTokens:           6_000,
		MemoryRecallMaxItems:             24,
		MemoryEmbeddingEnabled:           true,
		MemoryEmbeddingModel:             "bge-m3",
		MemoryEmbeddingModelDir:          paths.MemoryModelDir,
		MemoryEmbeddingBatchSize:         32,
		MemoryEmbeddingBatchWaitMS:       20,
		MemoryEmbeddingQueryCacheEntries: 10_000,
		MemoryEmbeddingExecutionTimeout:  8,
		MemoryBGEEnabled:                 true,
		MemoryBGEModelDir:                paths.MemoryModelDir,

		SkillsEnabled:       true,
		SkillsDir:           filepath.Join(apppaths.ProjectLocalDirName, apppaths.ProjectSkillsDirName),
		UserSkillsDir:       paths.UserSkillsDir,
		BundledSkillsDir:    bundledSkillsDir,
		TeamDir:             paths.UserTeamsDir,
		BundledTeamDir:      bundledTeamsDir,
		SystemPromptPath:    filepath.Join(bundledSystemDir, "system-prompt.md"),
		ProjectRootMarkers:  []string{".git"},
		ProjectDocFilenames: []string{"LUMINA.md", "AGENTS.md"},
		ProjectDocMaxBytes:  64 * 1024,

		UIBackend:   "prompt_toolkit_fullscreen",
		HarnessMode: "",

		WorktreeBaseRef: "HEAD",
		WorktreeDir:     filepath.Join(apppaths.ProjectLocalDirName, apppaths.ProjectWorktreesDirName),

		CWD: cwd,
	}
	if pathsErr != nil {
		cfg.PathErrors = append(cfg.PathErrors, pathsErr.Error())
	}
	if projectErr != nil {
		cfg.PathErrors = append(cfg.PathErrors, projectErr.Error())
	}
	if projectRootErr != nil {
		cfg.PathErrors = append(cfg.PathErrors, projectRootErr.Error())
	}
	applyLuminaDefaults(&cfg, paths.SettingsFile, cwd, resourceDir)
	if projectDefaults := findProjectDefaults(cwd); projectDefaults != "" && filepath.Clean(projectDefaults) != filepath.Clean(paths.SettingsFile) {
		applyLuminaDefaults(&cfg, projectDefaults, cwd, resourceDir)
	}
	if err := refreshProjectPaths(&cfg, cwd); err != nil {
		cfg.PathErrors = append(cfg.PathErrors, err.Error())
	}
	applyEnvOverrides(&cfg)
	ApplyHarnessDefaults(&cfg)
	normalizeMemoryModelConfig(&cfg)
	return cfg
}

func normalizeMemoryModelConfig(cfg *Config) {
	if cfg == nil {
		return
	}
	// BGE-M3 is the single managed memory model. Legacy enable/model fields are
	// normalized so an older settings file cannot select a different vector
	// space or silently disable local memory encoding.
	cfg.MemoryBGEEnabled = true
	cfg.MemoryEmbeddingEnabled = true
	cfg.MemoryEmbeddingModel = "bge-m3"
	cfg.MemoryEmbeddingModelDir = cfg.MemoryBGEModelDir
}

func ReloadDynamicConfig(current Config) Config {
	fresh := NewConfigForCWD(current.CWD)
	updated := current
	teamDir := current.TeamDir
	systemPromptPath := current.SystemPromptPath
	skillsDir := current.SkillsDir
	userSkillsDir := current.UserSkillsDir
	bundledSkillsDir := current.BundledSkillsDir
	isolatedSkillsOnly := current.IsolatedSkillsOnly
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
	updated.FallbackAPIEnabled = fresh.FallbackAPIEnabled
	updated.FallbackAPIKey = fresh.FallbackAPIKey
	updated.FallbackAPIBaseURL = fresh.FallbackAPIBaseURL
	updated.FallbackAPIModel = fresh.FallbackAPIModel
	updated.FallbackAPIType = fresh.FallbackAPIType
	if !isPinned(current, "api_max_tokens") {
		updated.APIMaxTokens = fresh.APIMaxTokens
	}
	updated.APIStreamIdleTimeoutSeconds = fresh.APIStreamIdleTimeoutSeconds
	updated.MaxToolOutputChars = fresh.MaxToolOutputChars
	updated.MaxToolResultCharsAbsolute = fresh.MaxToolResultCharsAbsolute
	updated.MaxMessageToolResultsChars = fresh.MaxMessageToolResultsChars
	updated.ShellTimeoutSeconds = fresh.ShellTimeoutSeconds
	updated.ShellMaxOutputBytes = fresh.ShellMaxOutputBytes
	updated.ContextCompressThreshold = fresh.ContextCompressThreshold
	updated.PromptCacheTTLSeconds = fresh.PromptCacheTTLSeconds
	updated.AnthropicCacheEditsEnabled = fresh.AnthropicCacheEditsEnabled
	updated.MaxParentTurns = fresh.MaxParentTurns
	if !isPinned(current, "session_memory_enabled") {
		updated.SessionMemoryEnabled = fresh.SessionMemoryEnabled
	}
	updated.SessionMemoryTurnInterval = fresh.SessionMemoryTurnInterval
	updated.SessionMemorySummaryModel = fresh.SessionMemorySummaryModel
	updated.SessionMemorySummaryMaxTokens = fresh.SessionMemorySummaryMaxTokens
	updated.SessionHistoryGetMessageLimit = fresh.SessionHistoryGetMessageLimit
	updated.SessionMemoryMaxCommits = fresh.SessionMemoryMaxCommits
	updated.SessionMemoryMaxMessages = fresh.SessionMemoryMaxMessages
	updated.SessionMemoryVacuumAfterEviction = fresh.SessionMemoryVacuumAfterEviction
	updated.SessionMaintenanceEnabled = fresh.SessionMaintenanceEnabled
	updated.SessionMaintenanceMode = fresh.SessionMaintenanceMode
	updated.SessionRetentionDays = fresh.SessionRetentionDays
	updated.SessionMaxEntries = fresh.SessionMaxEntries
	updated.SessionMaxDiskBytes = fresh.SessionMaxDiskBytes
	updated.SessionHighWaterRatio = fresh.SessionHighWaterRatio
	updated.SessionArchiveBeforeDelete = fresh.SessionArchiveBeforeDelete
	updated.SessionProtectPinned = fresh.SessionProtectPinned
	updated.TeamTimelineMaxEntries = fresh.TeamTimelineMaxEntries
	updated.TeamDialogueMaxEntries = fresh.TeamDialogueMaxEntries
	updated.TeamArtifactMaxBytes = fresh.TeamArtifactMaxBytes
	if !isPinned(current, "long_term_memory_enabled") {
		updated.LongTermMemoryEnabled = fresh.LongTermMemoryEnabled
	}
	if !isPinned(current, "memory_backend") {
		updated.MemoryBackend = fresh.MemoryBackend
	}
	if !isPinned(current, "memory_path") {
		updated.MemoryPath = fresh.MemoryPath
	}
	updated.MemoryRemoteProcessing = fresh.MemoryRemoteProcessing
	updated.MemoryCompilerModel = fresh.MemoryCompilerModel
	updated.MemoryConflictModel = fresh.MemoryConflictModel
	updated.MemoryCompileBatchTokens = fresh.MemoryCompileBatchTokens
	updated.MemorySearchLatencyMS = fresh.MemorySearchLatencyMS
	updated.MemoryCandidateLimit = fresh.MemoryCandidateLimit
	updated.MemoryContextTargetTokens = fresh.MemoryContextTargetTokens
	if !isPinned(current, "memory_context_max_tokens") {
		updated.MemoryContextMaxTokens = fresh.MemoryContextMaxTokens
	}
	if !isPinned(current, "memory_recall_max_items") {
		updated.MemoryRecallMaxItems = fresh.MemoryRecallMaxItems
	}
	updated.MemoryEmbeddingEnabled = fresh.MemoryEmbeddingEnabled
	updated.MemoryEmbeddingModel = fresh.MemoryEmbeddingModel
	updated.MemoryEmbeddingModelDir = fresh.MemoryEmbeddingModelDir
	updated.MemoryEmbeddingBatchSize = fresh.MemoryEmbeddingBatchSize
	updated.MemoryEmbeddingBatchWaitMS = fresh.MemoryEmbeddingBatchWaitMS
	updated.MemoryEmbeddingQueryCacheEntries = fresh.MemoryEmbeddingQueryCacheEntries
	updated.MemoryEmbeddingExecutionTimeout = fresh.MemoryEmbeddingExecutionTimeout
	updated.MemoryBGEEnabled = fresh.MemoryBGEEnabled
	updated.MemoryBGEModelDir = fresh.MemoryBGEModelDir
	updated.MemoryConfigErrors = append([]string(nil), fresh.MemoryConfigErrors...)
	updated.WebSearchEnabled = fresh.WebSearchEnabled
	updated.WebSearchProvider = fresh.WebSearchProvider
	updated.WebSearchBaseURL = fresh.WebSearchBaseURL
	updated.WebSearchMaxResults = fresh.WebSearchMaxResults
	updated.WebSearchTimeoutSeconds = fresh.WebSearchTimeoutSeconds
	updated.WebFetchEnabled = fresh.WebFetchEnabled
	updated.WebFetchRequireSearch = fresh.WebFetchRequireSearch
	updated.WebFetchMaxChars = fresh.WebFetchMaxChars
	updated.WebFetchTimeoutSeconds = fresh.WebFetchTimeoutSeconds
	updated.WebFetchUserAgent = fresh.WebFetchUserAgent
	updated.ProjectRootMarkers = fresh.ProjectRootMarkers
	updated.ProjectDocFilenames = fresh.ProjectDocFilenames
	updated.ProjectDocMaxBytes = fresh.ProjectDocMaxBytes
	updated.ProjectPaths = fresh.ProjectPaths
	if !isPinned(current, "project_runtime_dir") {
		updated.ProjectRuntimeDir = fresh.ProjectRuntimeDir
	}
	if !isPinned(current, "harness_mode") {
		updated.HarnessMode = fresh.HarnessMode
	}
	updated.TeamDir = teamDir
	updated.BundledTeamDir = fresh.BundledTeamDir
	updated.SystemPromptPath = systemPromptPath
	updated.SkillsDir = skillsDir
	updated.UserSkillsDir = userSkillsDir
	updated.BundledSkillsDir = bundledSkillsDir
	updated.IsolatedSkillsOnly = isolatedSkillsOnly
	ApplyHarnessDefaults(&updated)
	return updated
}

func ProjectRuntimeName(projectRoot string) string {
	paths, err := apppaths.ResolveCurrent()
	if err == nil {
		discovered, discoverErr := apppaths.DiscoverProjectRoot(projectRoot, []string{".git"})
		if discoverErr == nil {
			projectRoot = discovered
		}
		if project, projectErr := paths.ForProject(projectRoot); projectErr == nil {
			return project.ID
		}
	}
	return LegacyProjectRuntimeName(projectRoot)
}

func LegacyProjectRuntimeName(projectRoot string) string {
	name := filepath.Base(filepath.Clean(projectRoot))
	name = strings.TrimSpace(name)
	if name == "." || name == string(filepath.Separator) || name == "" {
		name = "default"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('-')
	}
	cleaned := strings.Trim(b.String(), ".-_")
	if cleaned == "" {
		return "default"
	}
	return cleaned
}

func ProjectRuntimeDir(projectRoot string) string {
	paths, err := apppaths.ResolveCurrent()
	if err != nil {
		return ""
	}
	discovered, discoverErr := apppaths.DiscoverProjectRoot(projectRoot, []string{".git"})
	if discoverErr != nil {
		return ""
	}
	project, err := paths.ForProject(discovered)
	if err != nil {
		return ""
	}
	return project.StateDir
}

func SetCWD(cfg *Config, cwd string) error {
	if cfg == nil {
		return nil
	}
	cfg.CWD = cwd
	return refreshProjectPaths(cfg, cwd)
}

func refreshProjectPaths(cfg *Config, cwd string) error {
	if cfg == nil {
		return nil
	}
	root, err := apppaths.DiscoverProjectRoot(cwd, cfg.ProjectRootMarkersOrDefault())
	if err != nil {
		return err
	}
	project, err := cfg.Paths.ForProject(root)
	if err != nil {
		return err
	}
	cfg.ProjectPaths = project
	cfg.ProjectRuntimeDir = project.StateDir
	return nil
}

func (c Config) ToolResultsDir(sessionID string) string {
	if c.ProjectPaths.ToolResultsDir != "" && c.ProjectPaths.StateDir != "" &&
		filepath.Clean(c.ProjectRuntimeDir) == filepath.Clean(c.ProjectPaths.StateDir) {
		return c.ProjectPaths.ToolResultsForSession(sessionID)
	}
	root := c.ToolResultsRoot()
	if root == "" {
		return ""
	}
	return filepath.Join(root, LegacyProjectRuntimeName(sessionID))
}

func (c Config) ToolResultsRoot() string {
	if c.ProjectPaths.ToolResultsDir != "" && c.ProjectPaths.StateDir != "" &&
		filepath.Clean(c.ProjectRuntimeDir) == filepath.Clean(c.ProjectPaths.StateDir) {
		return c.ProjectPaths.ToolResultsDir
	}
	if strings.TrimSpace(c.ProjectRuntimeDir) == "" {
		return ""
	}
	return filepath.Join(c.ProjectRuntimeDir, "tool-results")
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

func (c Config) ValidateMemoryConfig() error {
	if len(c.MemoryConfigErrors) == 0 {
		return nil
	}
	return fmt.Errorf("invalid memory configuration: %s", strings.Join(c.MemoryConfigErrors, "; "))
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

func IsTerminalBenchHarnessMode(mode string) bool {
	return strings.EqualFold(strings.TrimSpace(mode), "terminal-bench") ||
		strings.EqualFold(strings.TrimSpace(mode), "terminal_bench")
}

func ApplyHarnessDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	if strings.TrimSpace(cfg.HarnessMode) != "" {
		cfg.Yolo = true
	}
	if IsTerminalBenchHarnessMode(cfg.HarnessMode) && cfg.ShellTimeoutSeconds == 30.0 {
		cfg.ShellTimeoutSeconds = 120.0
	}
}

func (c Config) ProjectRootMarkersOrDefault() []string {
	return nonEmptyStringsOrDefault(c.ProjectRootMarkers, []string{".git"})
}

func (c Config) ProjectRoot() string {
	if strings.TrimSpace(c.ProjectPaths.CanonicalRoot) != "" {
		return c.ProjectPaths.CanonicalRoot
	}
	return c.CWD
}

func IsMemoryFabricBackend(backend string) bool {
	backend = strings.ToLower(strings.TrimSpace(backend))
	return backend == "fabric"
}

func (c Config) UsesMemoryFabric() bool {
	return c.LongTermMemoryEnabled && IsMemoryFabricBackend(c.MemoryBackend)
}

func (c Config) ProjectDocFilenamesOrDefault() []string {
	return nonEmptyStringsOrDefault(c.ProjectDocFilenames, []string{"LUMINA.md", "AGENTS.md"})
}

func (c Config) ProjectDocMaxBytesOrDefault() int {
	if c.ProjectDocMaxBytes > 0 {
		return c.ProjectDocMaxBytes
	}
	return 64 * 1024
}

func nonEmptyStringsOrDefault(values []string, fallback []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]struct{}{}
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) > 0 {
		return out
	}
	return append([]string(nil), fallback...)
}

type luminaDefaults struct {
	APIKey                           *string  `json:"api_key"`
	APIBaseURL                       *string  `json:"api_base_url"`
	APIModel                         *string  `json:"api_model"`
	APIType                          *string  `json:"api_type"`
	FallbackAPIEnabled               *bool    `json:"fallback_api_enabled"`
	FallbackAPIKey                   *string  `json:"fallback_api_key"`
	FallbackAPIBaseURL               *string  `json:"fallback_api_base_url"`
	FallbackAPIModel                 *string  `json:"fallback_api_model"`
	FallbackAPIType                  *string  `json:"fallback_api_type"`
	APIMaxTokens                     *int     `json:"api_max_tokens"`
	APIStreamIdleTimeoutSeconds      *float64 `json:"api_stream_idle_timeout_seconds"`
	MaxToolOutputChars               *int     `json:"max_tool_output_chars"`
	MaxToolResultCharsAbsolute       *int     `json:"max_tool_result_chars_absolute"`
	MaxMessageToolResultsChars       *int     `json:"max_message_tool_results_chars"`
	ShellTimeoutSeconds              *float64 `json:"shell_timeout_seconds"`
	ShellMaxOutputBytes              *int     `json:"shell_max_output_bytes"`
	MCPEnabled                       *bool    `json:"mcp_enabled"`
	MCPPingInterval                  *float64 `json:"mcp_ping_interval"`
	MCPConnectTimeout                *float64 `json:"mcp_connect_timeout"`
	MCPRequestTimeout                *float64 `json:"mcp_request_timeout"`
	WebSearchEnabled                 *bool    `json:"web_search_enabled"`
	WebSearchProvider                *string  `json:"web_search_provider"`
	WebSearchBaseURL                 *string  `json:"web_search_base_url"`
	WebSearchMaxResults              *int     `json:"web_search_max_results"`
	WebSearchTimeoutSeconds          *float64 `json:"web_search_timeout_seconds"`
	WebFetchEnabled                  *bool    `json:"web_fetch_enabled"`
	WebFetchRequireSearch            *bool    `json:"web_fetch_require_search_result"`
	WebFetchMaxChars                 *int     `json:"web_fetch_max_chars"`
	WebFetchTimeoutSeconds           *float64 `json:"web_fetch_timeout_seconds"`
	WebFetchUserAgent                *string  `json:"web_fetch_user_agent"`
	ContextCompressThreshold         *float64 `json:"context_compress_threshold"`
	PromptCacheTTLSeconds            *float64 `json:"prompt_cache_ttl_seconds"`
	AnthropicCacheEditsEnabled       *bool    `json:"anthropic_cache_edits_enabled"`
	MaxParentTurns                   *int     `json:"max_parent_turns"`
	SessionDir                       *string  `json:"session_dir"`
	SessionArchiveDir                *string  `json:"session_archive_dir"`
	SessionMemoryEnabled             *bool    `json:"session_memory_enabled"`
	SessionMemoryTurnInterval        *int     `json:"session_memory_turn_interval"`
	SessionMemorySummaryModel        *string  `json:"session_memory_summary_model"`
	SessionMemorySummaryMaxTokens    *int     `json:"session_memory_summary_max_tokens"`
	SessionHistoryGetMessageLimit    *int     `json:"session_history_get_message_limit"`
	SessionMemoryMaxCommits          *int     `json:"session_memory_max_commits"`
	SessionMemoryMaxMessages         *int     `json:"session_memory_max_messages"`
	SessionMemoryVacuumAfterEviction *bool    `json:"session_memory_vacuum_after_eviction"`
	SessionMaintenanceEnabled        *bool    `json:"session_maintenance_enabled"`
	SessionMaintenanceMode           *string  `json:"session_maintenance_mode"`
	SessionRetentionDays             *int     `json:"session_retention_days"`
	SessionMaxEntries                *int     `json:"session_max_entries"`
	SessionMaxDiskBytes              *int64   `json:"session_max_disk_bytes"`
	SessionHighWaterRatio            *float64 `json:"session_high_water_ratio"`
	SessionArchiveBeforeDelete       *bool    `json:"session_archive_before_delete"`
	SessionProtectPinned             *bool    `json:"session_protect_pinned"`
	TeamTimelineMaxEntries           *int     `json:"team_timeline_max_entries"`
	TeamDialogueMaxEntries           *int     `json:"team_dialogue_max_entries"`
	TeamArtifactMaxBytes             *int64   `json:"team_artifact_max_bytes"`
	LongTermMemoryEnabled            *bool    `json:"long_term_memory_enabled"`
	MemoryBackend                    *string  `json:"memory_backend"`
	MemoryPath                       *string  `json:"memory_path"`
	MemoryRemoteProcessing           *string  `json:"memory_remote_processing"`
	MemoryCompilerModel              *string  `json:"memory_compiler_model"`
	MemoryConflictModel              *string  `json:"memory_conflict_model"`
	MemoryCompileBatchTokens         *int     `json:"memory_compile_batch_tokens"`
	MemorySearchLatencyMS            *int     `json:"memory_search_latency_ms"`
	MemoryCandidateLimit             *int     `json:"memory_candidate_limit"`
	MemoryContextTargetTokens        *int     `json:"memory_context_target_tokens"`
	MemoryContextMaxTokens           *int     `json:"memory_context_max_tokens"`
	MemoryRecallMaxItems             *int     `json:"memory_recall_max_items"`
	MemoryEmbeddingEnabled           *bool    `json:"memory_embedding_enabled"`
	MemoryEmbeddingModel             *string  `json:"memory_embedding_model"`
	MemoryEmbeddingModelDir          *string  `json:"memory_embedding_model_dir"`
	MemoryEmbeddingBatchSize         *int     `json:"memory_embedding_batch_size"`
	MemoryEmbeddingBatchWaitMS       *int     `json:"memory_embedding_batch_wait_ms"`
	MemoryEmbeddingQueryCacheEntries *int     `json:"memory_embedding_query_cache_entries"`
	MemoryEmbeddingExecutionTimeout  *float64 `json:"memory_embedding_execution_timeout_seconds"`
	MemoryBGEEnabled                 *bool    `json:"memory_bge_enabled"`
	MemoryBGEModelDir                *string  `json:"memory_bge_model_dir"`
	SkillsEnabled                    *bool    `json:"skills_enabled"`
	SkillsDir                        *string  `json:"skills_dir"`
	UserSkillsDir                    *string  `json:"user_skills_dir"`
	BundledSkillsDir                 *string  `json:"bundled_skills_dir"`
	TeamDir                          *string  `json:"team_dir"`
	SystemPromptPath                 *string  `json:"system_prompt_path"`
	ProjectRootMarkers               []string `json:"project_root_markers"`
	ProjectDocFilenames              []string `json:"project_doc_filenames"`
	ProjectDocMaxBytes               *int     `json:"project_doc_max_bytes"`
	UIBackend                        *string  `json:"ui_backend"`
	HarnessMode                      *string  `json:"harness_mode"`
	WorktreeBaseRef                  *string  `json:"worktree_base_ref"`
	WorktreeDir                      *string  `json:"worktree_dir"`
}

func DefaultJSONKeys() []string {
	typeOfDefaults := reflect.TypeOf(luminaDefaults{})
	keys := make([]string, 0, typeOfDefaults.NumField())
	for index := 0; index < typeOfDefaults.NumField(); index++ {
		name := strings.Split(typeOfDefaults.Field(index).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			keys = append(keys, name)
		}
	}
	sort.Strings(keys)
	return keys
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
	if defaults.FallbackAPIEnabled != nil {
		cfg.FallbackAPIEnabled = *defaults.FallbackAPIEnabled
	}
	if defaults.FallbackAPIKey != nil {
		cfg.FallbackAPIKey = *defaults.FallbackAPIKey
	}
	if defaults.FallbackAPIBaseURL != nil {
		cfg.FallbackAPIBaseURL = *defaults.FallbackAPIBaseURL
	}
	if defaults.FallbackAPIModel != nil {
		cfg.FallbackAPIModel = *defaults.FallbackAPIModel
	}
	if defaults.FallbackAPIType != nil {
		cfg.FallbackAPIType = *defaults.FallbackAPIType
	}
	if defaults.APIMaxTokens != nil {
		cfg.APIMaxTokens = *defaults.APIMaxTokens
	}
	if defaults.APIStreamIdleTimeoutSeconds != nil && *defaults.APIStreamIdleTimeoutSeconds > 0 {
		cfg.APIStreamIdleTimeoutSeconds = *defaults.APIStreamIdleTimeoutSeconds
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
	if defaults.WebSearchEnabled != nil {
		cfg.WebSearchEnabled = *defaults.WebSearchEnabled
	}
	if defaults.WebSearchProvider != nil {
		cfg.WebSearchProvider = strings.TrimSpace(*defaults.WebSearchProvider)
	}
	if defaults.WebSearchBaseURL != nil {
		cfg.WebSearchBaseURL = strings.TrimRight(strings.TrimSpace(*defaults.WebSearchBaseURL), "/")
	}
	if defaults.WebSearchMaxResults != nil && *defaults.WebSearchMaxResults > 0 {
		cfg.WebSearchMaxResults = *defaults.WebSearchMaxResults
	}
	if defaults.WebSearchTimeoutSeconds != nil && *defaults.WebSearchTimeoutSeconds > 0 {
		cfg.WebSearchTimeoutSeconds = *defaults.WebSearchTimeoutSeconds
	}
	if defaults.WebFetchEnabled != nil {
		cfg.WebFetchEnabled = *defaults.WebFetchEnabled
	}
	if defaults.WebFetchRequireSearch != nil {
		cfg.WebFetchRequireSearch = *defaults.WebFetchRequireSearch
	}
	if defaults.WebFetchMaxChars != nil && *defaults.WebFetchMaxChars > 0 {
		cfg.WebFetchMaxChars = *defaults.WebFetchMaxChars
	}
	if defaults.WebFetchTimeoutSeconds != nil && *defaults.WebFetchTimeoutSeconds > 0 {
		cfg.WebFetchTimeoutSeconds = *defaults.WebFetchTimeoutSeconds
	}
	if defaults.WebFetchUserAgent != nil {
		cfg.WebFetchUserAgent = strings.TrimSpace(*defaults.WebFetchUserAgent)
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
	if defaults.SessionDir != nil && !apppaths.IsLegacyDefaultSetting("session_dir", *defaults.SessionDir) {
		cfg.SessionDir = resolveProjectPath(cwd, *defaults.SessionDir)
	}
	if defaults.SessionArchiveDir != nil {
		cfg.SessionArchiveDir = resolveProjectPath(cwd, *defaults.SessionArchiveDir)
	}
	if defaults.SessionMemoryEnabled != nil {
		cfg.SessionMemoryEnabled = *defaults.SessionMemoryEnabled
	}
	if defaults.SessionMemoryTurnInterval != nil && *defaults.SessionMemoryTurnInterval > 0 {
		cfg.SessionMemoryTurnInterval = *defaults.SessionMemoryTurnInterval
	}
	if defaults.SessionMemorySummaryModel != nil {
		cfg.SessionMemorySummaryModel = strings.TrimSpace(*defaults.SessionMemorySummaryModel)
	}
	if defaults.SessionMemorySummaryMaxTokens != nil && *defaults.SessionMemorySummaryMaxTokens > 0 {
		cfg.SessionMemorySummaryMaxTokens = *defaults.SessionMemorySummaryMaxTokens
	}
	if defaults.SessionHistoryGetMessageLimit != nil && *defaults.SessionHistoryGetMessageLimit > 0 {
		cfg.SessionHistoryGetMessageLimit = *defaults.SessionHistoryGetMessageLimit
	}
	if defaults.SessionMemoryMaxCommits != nil && *defaults.SessionMemoryMaxCommits > 0 {
		cfg.SessionMemoryMaxCommits = *defaults.SessionMemoryMaxCommits
	}
	if defaults.SessionMemoryMaxMessages != nil && *defaults.SessionMemoryMaxMessages > 0 {
		cfg.SessionMemoryMaxMessages = *defaults.SessionMemoryMaxMessages
	}
	if defaults.SessionMemoryVacuumAfterEviction != nil {
		cfg.SessionMemoryVacuumAfterEviction = *defaults.SessionMemoryVacuumAfterEviction
	}
	if defaults.SessionMaintenanceEnabled != nil {
		cfg.SessionMaintenanceEnabled = *defaults.SessionMaintenanceEnabled
	}
	if defaults.SessionMaintenanceMode != nil {
		mode := strings.ToLower(strings.TrimSpace(*defaults.SessionMaintenanceMode))
		if mode == "warn" || mode == "enforce" {
			cfg.SessionMaintenanceMode = mode
		}
	}
	if defaults.SessionRetentionDays != nil && *defaults.SessionRetentionDays > 0 {
		cfg.SessionRetentionDays = *defaults.SessionRetentionDays
	}
	if defaults.SessionMaxEntries != nil && *defaults.SessionMaxEntries > 0 {
		cfg.SessionMaxEntries = *defaults.SessionMaxEntries
	}
	if defaults.SessionMaxDiskBytes != nil && *defaults.SessionMaxDiskBytes >= 0 {
		cfg.SessionMaxDiskBytes = *defaults.SessionMaxDiskBytes
	}
	if defaults.SessionHighWaterRatio != nil && *defaults.SessionHighWaterRatio > 0 && *defaults.SessionHighWaterRatio <= 1 {
		cfg.SessionHighWaterRatio = *defaults.SessionHighWaterRatio
	}
	if defaults.SessionArchiveBeforeDelete != nil {
		cfg.SessionArchiveBeforeDelete = *defaults.SessionArchiveBeforeDelete
	}
	if defaults.SessionProtectPinned != nil {
		cfg.SessionProtectPinned = *defaults.SessionProtectPinned
	}
	if defaults.TeamTimelineMaxEntries != nil && *defaults.TeamTimelineMaxEntries > 0 {
		cfg.TeamTimelineMaxEntries = *defaults.TeamTimelineMaxEntries
	}
	if defaults.TeamDialogueMaxEntries != nil && *defaults.TeamDialogueMaxEntries > 0 {
		cfg.TeamDialogueMaxEntries = *defaults.TeamDialogueMaxEntries
	}
	if defaults.TeamArtifactMaxBytes != nil && *defaults.TeamArtifactMaxBytes >= 0 {
		cfg.TeamArtifactMaxBytes = *defaults.TeamArtifactMaxBytes
	}
	if defaults.LongTermMemoryEnabled != nil {
		cfg.LongTermMemoryEnabled = *defaults.LongTermMemoryEnabled
	}
	if defaults.MemoryBackend != nil {
		backend := strings.ToLower(strings.TrimSpace(*defaults.MemoryBackend))
		switch backend {
		case "fabric":
			cfg.MemoryBackend = backend
		default:
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_backend must be fabric")
		}
	}
	if defaults.MemoryPath != nil && strings.TrimSpace(*defaults.MemoryPath) != "" {
		cfg.MemoryPath = resolveProjectPath(cwd, expandHome(*defaults.MemoryPath))
	}
	if defaults.MemoryRemoteProcessing != nil {
		policy := strings.ToLower(strings.TrimSpace(*defaults.MemoryRemoteProcessing))
		switch policy {
		case "off", "redacted", "allow":
			cfg.MemoryRemoteProcessing = policy
		default:
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_remote_processing must be off, redacted, or allow")
		}
	}
	if defaults.MemoryCompilerModel != nil && strings.TrimSpace(*defaults.MemoryCompilerModel) != "" {
		cfg.MemoryCompilerModel = strings.TrimSpace(*defaults.MemoryCompilerModel)
	}
	if defaults.MemoryConflictModel != nil && strings.TrimSpace(*defaults.MemoryConflictModel) != "" {
		cfg.MemoryConflictModel = strings.TrimSpace(*defaults.MemoryConflictModel)
	}
	applyPositiveMemoryInt(cfg, "memory_compile_batch_tokens", defaults.MemoryCompileBatchTokens, &cfg.MemoryCompileBatchTokens)
	applyPositiveMemoryInt(cfg, "memory_search_latency_ms", defaults.MemorySearchLatencyMS, &cfg.MemorySearchLatencyMS)
	applyPositiveMemoryInt(cfg, "memory_candidate_limit", defaults.MemoryCandidateLimit, &cfg.MemoryCandidateLimit)
	applyPositiveMemoryInt(cfg, "memory_context_target_tokens", defaults.MemoryContextTargetTokens, &cfg.MemoryContextTargetTokens)
	if defaults.MemoryContextMaxTokens != nil && *defaults.MemoryContextMaxTokens > 0 {
		cfg.MemoryContextMaxTokens = *defaults.MemoryContextMaxTokens
	}
	applyPositiveMemoryInt(cfg, "memory_recall_max_items", defaults.MemoryRecallMaxItems, &cfg.MemoryRecallMaxItems)
	if defaults.MemoryEmbeddingEnabled != nil {
		cfg.MemoryEmbeddingEnabled = *defaults.MemoryEmbeddingEnabled
	}
	if defaults.MemoryEmbeddingModel != nil && strings.TrimSpace(*defaults.MemoryEmbeddingModel) != "" {
		cfg.MemoryEmbeddingModel = strings.TrimSpace(*defaults.MemoryEmbeddingModel)
	}
	if defaults.MemoryEmbeddingModelDir != nil && strings.TrimSpace(*defaults.MemoryEmbeddingModelDir) != "" &&
		!apppaths.IsLegacyDefaultSetting("memory_embedding_model_dir", *defaults.MemoryEmbeddingModelDir) {
		cfg.MemoryEmbeddingModelDir = expandHome(*defaults.MemoryEmbeddingModelDir)
	}
	applyPositiveMemoryInt(cfg, "memory_embedding_batch_size", defaults.MemoryEmbeddingBatchSize, &cfg.MemoryEmbeddingBatchSize)
	applyPositiveMemoryInt(cfg, "memory_embedding_batch_wait_ms", defaults.MemoryEmbeddingBatchWaitMS, &cfg.MemoryEmbeddingBatchWaitMS)
	applyPositiveMemoryInt(cfg, "memory_embedding_query_cache_entries", defaults.MemoryEmbeddingQueryCacheEntries, &cfg.MemoryEmbeddingQueryCacheEntries)
	if defaults.MemoryEmbeddingExecutionTimeout != nil {
		if *defaults.MemoryEmbeddingExecutionTimeout <= 0 {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_embedding_execution_timeout_seconds must be positive")
		} else {
			cfg.MemoryEmbeddingExecutionTimeout = *defaults.MemoryEmbeddingExecutionTimeout
		}
	}
	if defaults.MemoryBGEEnabled != nil {
		cfg.MemoryBGEEnabled = *defaults.MemoryBGEEnabled
	}
	if defaults.MemoryBGEModelDir != nil && strings.TrimSpace(*defaults.MemoryBGEModelDir) != "" {
		cfg.MemoryBGEModelDir = expandHome(*defaults.MemoryBGEModelDir)
	}
	if cfg.MemoryContextTargetTokens > cfg.MemoryContextMaxTokens {
		cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors,
			"memory_context_target_tokens must not exceed memory_context_max_tokens")
	}
	if cfg.MemoryRecallMaxItems > cfg.MemoryCandidateLimit {
		cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors,
			"memory_recall_max_items must not exceed memory_candidate_limit")
	}
	if defaults.SkillsEnabled != nil {
		cfg.SkillsEnabled = *defaults.SkillsEnabled
	}
	if defaults.SkillsDir != nil && !apppaths.IsLegacyDefaultSetting("skills_dir", *defaults.SkillsDir) {
		cfg.SkillsDir = *defaults.SkillsDir
	}
	if defaults.UserSkillsDir != nil && !apppaths.IsLegacyDefaultSetting("user_skills_dir", *defaults.UserSkillsDir) {
		cfg.UserSkillsDir = expandHome(*defaults.UserSkillsDir)
	}
	if defaults.BundledSkillsDir != nil && !apppaths.IsLegacyDefaultSetting("bundled_skills_dir", *defaults.BundledSkillsDir) {
		cfg.BundledSkillsDir = resolveResourcePath(resourceDir, *defaults.BundledSkillsDir)
	}
	if defaults.TeamDir != nil && !apppaths.IsLegacyDefaultSetting("team_dir", *defaults.TeamDir) {
		cfg.TeamDir = expandHome(*defaults.TeamDir)
	}
	if defaults.SystemPromptPath != nil && !apppaths.IsLegacyDefaultSetting("system_prompt_path", *defaults.SystemPromptPath) {
		cfg.SystemPromptPath = resolveResourcePath(resourceDir, *defaults.SystemPromptPath)
	}
	if len(defaults.ProjectRootMarkers) > 0 {
		cfg.ProjectRootMarkers = nonEmptyStringsOrDefault(defaults.ProjectRootMarkers, cfg.ProjectRootMarkers)
	}
	if len(defaults.ProjectDocFilenames) > 0 {
		cfg.ProjectDocFilenames = nonEmptyStringsOrDefault(defaults.ProjectDocFilenames, cfg.ProjectDocFilenames)
	}
	if defaults.ProjectDocMaxBytes != nil && *defaults.ProjectDocMaxBytes > 0 {
		cfg.ProjectDocMaxBytes = *defaults.ProjectDocMaxBytes
	}
	if defaults.UIBackend != nil {
		cfg.UIBackend = *defaults.UIBackend
	}
	if defaults.HarnessMode != nil {
		cfg.HarnessMode = strings.TrimSpace(*defaults.HarnessMode)
	}
	if defaults.WorktreeBaseRef != nil {
		cfg.WorktreeBaseRef = *defaults.WorktreeBaseRef
	}
	if defaults.WorktreeDir != nil && !apppaths.IsLegacyDefaultSetting("worktree_dir", *defaults.WorktreeDir) {
		cfg.WorktreeDir = *defaults.WorktreeDir
	}
}

func applyPositiveMemoryInt(cfg *Config, name string, value *int, target *int) {
	if value == nil {
		return
	}
	if *value <= 0 {
		cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, name+" must be positive")
		return
	}
	*target = *value
}

func FindLuminaRoot(start string) string {
	if root := normalizeLuminaRoot(os.Getenv("LUMINA_RESOURCE_ROOT")); root != "" {
		return root
	}
	if root := normalizeLuminaRoot(os.Getenv("LUMINA_HOME")); root != "" {
		warnLegacyLuminaHome.Do(func() {
			fmt.Fprintln(os.Stderr, "lumina warning: LUMINA_HOME is deprecated; use LUMINA_RESOURCE_ROOT for bundled resources")
		})
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
	if home := userHomeDir(); home != "" {
		candidates = append(candidates, apppaths.LegacyDefaultRoot(home))
	}
	if paths, err := apppaths.ResolveCurrent(); err == nil {
		candidates = append(candidates, paths.ResourcesDir)
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
		return apppaths.ProjectLocalRoot(root)
	}
	return apppaths.ProjectLocalRoot(root)
}

func LuminaResourcePath(root string, elems ...string) string {
	resourceDir := LuminaResourceDir(root)
	if strings.EqualFold(filepath.Base(resourceDir), "resources") && len(elems) > 0 {
		elems = append([]string(nil), elems...)
		elems[0] = normalizedResourceName(elems[0])
	}
	parts := append([]string{resourceDir}, elems...)
	return filepath.Join(parts...)
}

func UserDefaultsPath(homeDir string) string {
	if homeDir != "" {
		paths, err := apppaths.Resolve(apppaths.ResolveOptions{GOOS: runtime.GOOS, HomeDir: homeDir, Env: map[string]string{}})
		if err == nil {
			return paths.SettingsFile
		}
	}
	paths, err := apppaths.ResolveCurrent()
	if err != nil {
		return ""
	}
	return paths.SettingsFile
}

func resourceSubdir(root, legacyName, v2Name string) string {
	if exactPathExists(root, v2Name) || strings.EqualFold(filepath.Base(root), "resources") {
		return filepath.Join(root, v2Name)
	}
	return filepath.Join(root, legacyName)
}

func hasDirectLuminaResources(root string) bool {
	if exactPathExists(root, filepath.Join("system", "system-prompt.md")) {
		return true
	}
	for _, rel := range []string{
		filepath.Join(apppaths.LegacyConfigDirName, apppaths.ProjectDefaultsFileName),
		filepath.Join(apppaths.LegacySystemDirName, apppaths.SystemPromptFileName),
		apppaths.LegacySkillsDirName,
	} {
		if exactPathExists(root, rel) {
			return true
		}
	}
	return false
}

func hasNestedLuminaResources(root string) bool {
	for _, rel := range []string{
		filepath.Join(apppaths.ProjectLocalDirName, apppaths.LegacyConfigDirName, apppaths.ProjectDefaultsFileName),
		filepath.Join(apppaths.ProjectLocalDirName, apppaths.LegacySystemDirName, apppaths.SystemPromptFileName),
		filepath.Join(apppaths.ProjectLocalDirName, apppaths.LegacySkillsDirName),
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
	cfg.APIKey = firstNonEmpty(os.Getenv("LUMINA_API_KEY"), os.Getenv("LLM_API_KEY"), os.Getenv("ANTHROPIC_API_KEY"), cfg.APIKey)
	cfg.APIBaseURL = firstNonEmpty(os.Getenv("LUMINA_API_BASE_URL"), os.Getenv("LLM_BASE_URL"), os.Getenv("ANTHROPIC_BASE_URL"), cfg.APIBaseURL)
	cfg.APIModel = firstNonEmpty(os.Getenv("LUMINA_API_MODEL"), os.Getenv("LLM_DEFAULT_MODEL"), os.Getenv("ANTHROPIC_MODEL"), cfg.APIModel)
	cfg.APIType = firstNonEmpty(os.Getenv("LUMINA_API_TYPE"), os.Getenv("LLM_API_TYPE"), cfg.APIType)
	cfg.FallbackAPIEnabled = envBool("LUMINA_FALLBACK_API_ENABLED", cfg.FallbackAPIEnabled)
	cfg.FallbackAPIKey = firstNonEmpty(os.Getenv("LUMINA_FALLBACK_API_KEY"), cfg.FallbackAPIKey)
	cfg.FallbackAPIBaseURL = firstNonEmpty(os.Getenv("LUMINA_FALLBACK_API_BASE_URL"), cfg.FallbackAPIBaseURL)
	cfg.FallbackAPIModel = firstNonEmpty(os.Getenv("LUMINA_FALLBACK_API_MODEL"), cfg.FallbackAPIModel)
	cfg.FallbackAPIType = firstNonEmpty(os.Getenv("LUMINA_FALLBACK_API_TYPE"), cfg.FallbackAPIType)
	cfg.APIStreamIdleTimeoutSeconds = envFloat("LUMINA_API_STREAM_IDLE_TIMEOUT_SECONDS", cfg.APIStreamIdleTimeoutSeconds)
	cfg.Yolo = envBool("YOLO_MODE", cfg.Yolo)
	cfg.PromptCacheTTLSeconds = envFloat("LUMINA_PROMPT_CACHE_TTL_SECONDS", cfg.PromptCacheTTLSeconds)
	cfg.AnthropicCacheEditsEnabled = envBool("LUMINA_ANTHROPIC_CACHE_EDITS", cfg.AnthropicCacheEditsEnabled)
	if backend := strings.ToLower(strings.TrimSpace(os.Getenv("LUMINA_MEMORY_BACKEND"))); backend != "" {
		if IsMemoryFabricBackend(backend) {
			cfg.MemoryBackend = backend
		} else {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "LUMINA_MEMORY_BACKEND must be fabric")
		}
	}
	cfg.MemoryPath = firstNonEmpty(strings.TrimSpace(os.Getenv("LUMINA_MEMORY_PATH")), cfg.MemoryPath)
	cfg.MemoryBGEModelDir = firstNonEmpty(strings.TrimSpace(os.Getenv("LUMINA_MEMORY_BGE_MODEL_DIR")), cfg.MemoryBGEModelDir)
	if policy := strings.ToLower(strings.TrimSpace(os.Getenv("LUMINA_MEMORY_REMOTE_PROCESSING"))); policy != "" {
		switch policy {
		case "off", "redacted", "allow":
			cfg.MemoryRemoteProcessing = policy
		default:
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors,
				"LUMINA_MEMORY_REMOTE_PROCESSING must be off, redacted, or allow")
		}
	}
	cfg.MaxParentTurns = envInt("LUMINA_MAX_PARENT_TURNS", cfg.MaxParentTurns)
	cfg.WebSearchEnabled = envBool("LUMINA_WEB_SEARCH_ENABLED", cfg.WebSearchEnabled)
	cfg.WebSearchProvider = firstNonEmpty(strings.TrimSpace(os.Getenv("LUMINA_WEB_SEARCH_PROVIDER")), cfg.WebSearchProvider)
	cfg.WebSearchBaseURL = strings.TrimRight(firstNonEmpty(strings.TrimSpace(os.Getenv("LUMINA_WEB_SEARCH_BASE_URL")), cfg.WebSearchBaseURL), "/")
	cfg.WebSearchMaxResults = positiveIntOrDefault(envInt("LUMINA_WEB_SEARCH_MAX_RESULTS", cfg.WebSearchMaxResults), cfg.WebSearchMaxResults)
	cfg.WebSearchTimeoutSeconds = envFloat("LUMINA_WEB_SEARCH_TIMEOUT_SECONDS", cfg.WebSearchTimeoutSeconds)
	cfg.WebFetchEnabled = envBool("LUMINA_WEB_FETCH_ENABLED", cfg.WebFetchEnabled)
	cfg.WebFetchRequireSearch = envBool("LUMINA_WEB_FETCH_REQUIRE_SEARCH_RESULT", cfg.WebFetchRequireSearch)
	cfg.WebFetchMaxChars = positiveIntOrDefault(envInt("LUMINA_WEB_FETCH_MAX_CHARS", cfg.WebFetchMaxChars), cfg.WebFetchMaxChars)
	cfg.WebFetchTimeoutSeconds = envFloat("LUMINA_WEB_FETCH_TIMEOUT_SECONDS", cfg.WebFetchTimeoutSeconds)
	cfg.WebFetchUserAgent = firstNonEmpty(strings.TrimSpace(os.Getenv("LUMINA_WEB_FETCH_USER_AGENT")), cfg.WebFetchUserAgent)
	cfg.SessionMemoryTurnInterval = positiveIntOrDefault(envInt("SESSION_MEM_TURN", cfg.SessionMemoryTurnInterval), cfg.SessionMemoryTurnInterval)
	cfg.HarnessMode = firstNonEmpty(strings.TrimSpace(os.Getenv("LUMINA_HARNESS_MODE")), cfg.HarnessMode)
	cfg.UIBackend = "prompt_toolkit_fullscreen"
}

func positiveIntOrDefault(value, fallback int) int {
	if value > 0 {
		return value
	}
	return fallback
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
	if clean == apppaths.ProjectLocalDirName {
		return resourceDir
	}
	prefix := apppaths.ProjectLocalDirName + string(filepath.Separator)
	if strings.HasPrefix(clean, prefix) {
		clean = strings.TrimPrefix(clean, prefix)
	}
	if filepath.Separator != '/' {
		slashPrefix := apppaths.ProjectLocalDirName + "/"
		if strings.HasPrefix(clean, slashPrefix) {
			clean = strings.TrimPrefix(clean, slashPrefix)
		}
	}
	if strings.EqualFold(filepath.Base(resourceDir), "resources") {
		parts := strings.Split(filepath.ToSlash(clean), "/")
		if len(parts) > 0 {
			parts[0] = normalizedResourceName(parts[0])
			clean = filepath.FromSlash(strings.Join(parts, "/"))
		}
	}
	return filepath.Join(resourceDir, clean)
}

func normalizedResourceName(name string) string {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case apppaths.LegacyConfigDirName:
		return "defaults"
	case apppaths.LegacySystemDirName:
		return "system"
	case apppaths.LegacySkillsDirName:
		return "skills"
	case apppaths.LegacyTeamsDirName, "TEAMS":
		return "teams"
	default:
		return name
	}
}

func findProjectDefaults(cwd string) string {
	current, err := filepath.Abs(cwd)
	if err != nil {
		current = cwd
	}
	for current != "" {
		candidate := apppaths.ProjectDefaultsFile(current)
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ""
}

func expandHome(path string) string {
	if path == "~" {
		return userHomeDir()
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(userHomeDir(), path[2:])
	}
	return path
}

func userHomeDir() string {
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return home
	}
	home, _ := os.UserHomeDir()
	return home
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

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

var (
	configMu             sync.RWMutex
	config               *Config
	warnLegacyLuminaHome sync.Once
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
