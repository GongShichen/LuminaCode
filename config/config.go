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
)

const CompressionTriggerRatio = 0.80

type Config struct {
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

	ExtractionModel                     *string
	LongTermMemoryEnabled               bool
	LongTermMemoryStore                 string
	MemoryContextMaxTokens              int
	MemoryRecallMaxItems                int
	MemoryEmbeddingEnabled              bool
	MemoryEmbeddingModel                string
	MemoryEmbeddingModelDir             string
	MemoryFTSCandidates                 int
	MemoryVectorCandidates              int
	MemoryGraphCandidates               int
	MemoryGraphMaxHops                  int
	MemoryRRFK                          int
	MemoryMMRLambda                     float64
	MemorySessionRetrievalEnabled       bool
	MemorySessionCandidates             int
	MemoryChunksPerSession              int
	MemorySessionChunkCandidates        int
	MemoryMMRRelevanceWeight            float64
	MemoryMMRNoveltyWeight              float64
	MemoryMMRFacetCoverageWeight        float64
	MemoryMMRSourceCoverageWeight       float64
	MemoryCoreContextTokens             int
	MemoryContextTargetTokens           int
	MemoryRetrievalLocalTimeoutSeconds  float64
	MemoryEvidenceNeighborChunks        int
	MemoryAdjacentChunkWindow           int
	MemoryRetrievalCacheEnabled         bool
	MemoryRetrievalCacheTTLSeconds      float64
	MemoryCanonicalEntityEnabled        bool
	MemoryCanonicalEventEnabled         bool
	MemoryCanonicalSimilarityThreshold  float64
	MemoryCanonicalConfirmationModel    string
	MemoryCanonicalConfirmationTimeout  float64
	MemoryConfigErrors                  []string
	MemoryQueryExpansionEnabled         bool
	MemoryQueryExpansionModel           string
	MemoryQueryExpansionTimeoutSeconds  float64
	MemoryQueryExpansionAdditionalWait  int
	MemoryQueryExpansionMaxContext      int
	MemoryQueryExpansionMaxQueries      int
	MemoryWriteConfirmUserScope         bool
	MemoryWriteConfirmProcedural        bool
	MemoryBackgroundExtractionEnabled   bool
	MemoryBackgroundExtractionInterval  int
	MemoryRetentionDays                 map[string]int
	MemoryLifecycleEnabled              bool
	MemoryMaintenanceIntervalSeconds    int
	MemoryHotAccessDays                 int
	MemoryWarmAccessDays                int
	MemoryAccessRecencyHalfLifeDays     int
	MemoryArchiveGraceDays              int
	MemoryArchiveValueThreshold         float64
	MemoryAutoHardDeleteEnabled         bool
	MemoryValueWeights                  map[string]float64
	MemoryAtomTargetTokens              int
	MemoryAtomMaxTokens                 int
	MemoryAtomMaxSelected               int
	MemoryCoverageMaxFacets             int
	MemoryCoverageCompletionRounds      int
	MemoryCoverageRelevanceWeight       float64
	MemoryCoverageFacetWeight           float64
	MemoryCoverageProvenanceWeight      float64
	MemoryCoverageSourceWeight          float64
	MemoryCoverageCoherenceWeight       float64
	MemoryCoverageSupportTarget         float64
	MemoryCoverageResidualTrigger       float64
	MemoryCoverageMinMarginalGain       float64
	MemoryAtomStructuralContextEnabled  bool
	MemoryAtomStructuralContextTokens   int
	MemoryEvidencePrimaryBudgetRatio    float64
	MemoryEvidenceCompletionBudgetRatio float64
	MemoryEvidenceContextBudgetRatio    float64
	MemoryEmbeddingBatchSize            int
	MemoryEmbeddingBatchWaitMS          int
	MemoryEmbeddingQueryCacheEntries    int
	MemoryEmbeddingExecutionTimeout     float64

	SkillsEnabled              bool
	SkillsDir                  string
	UserSkillsDir              string
	BundledSkillsDir           string
	IsolatedSkillsOnly         bool
	TeamDir                    string
	SystemPromptPath           string
	MemoryExtractionPromptPath string

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
	homeDir := userHomeDir()
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	luminaRoot := FindLuminaRoot(cwd)
	if luminaRoot == "" {
		luminaRoot = cwd
	}
	resourceDir := LuminaResourceDir(luminaRoot)

	cfg := Config{
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

		SessionDir:        filepath.Join(homeDir, ".lumina", "sessions"),
		ProjectRuntimeDir: filepath.Join(homeDir, ".lumina", "project", ProjectRuntimeName(cwd)),

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

		ExtractionModel:                    nil,
		LongTermMemoryEnabled:              true,
		LongTermMemoryStore:                filepath.Join(homeDir, ".lumina", "memory", "lumina-memory.sqlite"),
		MemoryContextMaxTokens:             6000,
		MemoryRecallMaxItems:               8,
		MemoryEmbeddingEnabled:             true,
		MemoryEmbeddingModel:               "multilingual-e5-small",
		MemoryEmbeddingModelDir:            filepath.Join(homeDir, ".lumina", "models", "memory", "multilingual-e5-small"),
		MemoryFTSCandidates:                40,
		MemoryVectorCandidates:             40,
		MemoryGraphCandidates:              20,
		MemoryGraphMaxHops:                 2,
		MemoryRRFK:                         60,
		MemoryMMRLambda:                    0.75,
		MemorySessionRetrievalEnabled:      true,
		MemorySessionCandidates:            12,
		MemoryChunksPerSession:             6,
		MemorySessionChunkCandidates:       64,
		MemoryMMRRelevanceWeight:           0.60,
		MemoryMMRNoveltyWeight:             0.20,
		MemoryMMRFacetCoverageWeight:       0.15,
		MemoryMMRSourceCoverageWeight:      0.05,
		MemoryCoreContextTokens:            512,
		MemoryContextTargetTokens:          2400,
		MemoryRetrievalLocalTimeoutSeconds: 3,
		MemoryEvidenceNeighborChunks:       1,
		MemoryAdjacentChunkWindow:          1,
		MemoryQueryExpansionEnabled:        true,
		MemoryQueryExpansionModel:          "inherit",
		MemoryQueryExpansionTimeoutSeconds: 0,
		MemoryQueryExpansionAdditionalWait: 750,
		MemoryQueryExpansionMaxContext:     3000,
		MemoryQueryExpansionMaxQueries:     5,
		MemoryRetrievalCacheEnabled:        true,
		MemoryRetrievalCacheTTLSeconds:     300,
		MemoryCanonicalEntityEnabled:       true,
		MemoryCanonicalEventEnabled:        true,
		MemoryCanonicalSimilarityThreshold: 0.82,
		MemoryCanonicalConfirmationModel:   "inherit",
		MemoryCanonicalConfirmationTimeout: 8,
		MemoryWriteConfirmUserScope:        true,
		MemoryWriteConfirmProcedural:       true,
		MemoryBackgroundExtractionEnabled:  true,
		MemoryBackgroundExtractionInterval: 3,
		MemoryRetentionDays: map[string]int{
			"semantic":   365,
			"episodic":   180,
			"procedural": 0,
			"preference": 0,
			"feedback":   0,
			"project":    365,
			"reference":  365,
		},
		MemoryLifecycleEnabled:           true,
		MemoryMaintenanceIntervalSeconds: 300,
		MemoryHotAccessDays:              30,
		MemoryWarmAccessDays:             90,
		MemoryAccessRecencyHalfLifeDays:  30,
		MemoryArchiveGraceDays:           30,
		MemoryArchiveValueThreshold:      0.45,
		MemoryAutoHardDeleteEnabled:      false,
		MemoryValueWeights: map[string]float64{
			"importance": 0.30, "confidence": 0.20, "access_recency": 0.15,
			"access_frequency": 0.10, "reinforcement": 0.10,
			"provenance_strength": 0.10, "dependency_strength": 0.05,
		},
		MemoryAtomTargetTokens:              96,
		MemoryAtomMaxTokens:                 160,
		MemoryAtomMaxSelected:               32,
		MemoryCoverageMaxFacets:             8,
		MemoryCoverageCompletionRounds:      1,
		MemoryCoverageRelevanceWeight:       0.45,
		MemoryCoverageFacetWeight:           0.25,
		MemoryCoverageProvenanceWeight:      0.15,
		MemoryCoverageSourceWeight:          0.10,
		MemoryCoverageCoherenceWeight:       0.05,
		MemoryCoverageSupportTarget:         0.82,
		MemoryCoverageResidualTrigger:       0.82,
		MemoryCoverageMinMarginalGain:       0,
		MemoryAtomStructuralContextEnabled:  true,
		MemoryAtomStructuralContextTokens:   384,
		MemoryEvidencePrimaryBudgetRatio:    0.70,
		MemoryEvidenceCompletionBudgetRatio: 0.20,
		MemoryEvidenceContextBudgetRatio:    0.10,
		MemoryEmbeddingBatchSize:            32,
		MemoryEmbeddingBatchWaitMS:          20,
		MemoryEmbeddingQueryCacheEntries:    10000,
		MemoryEmbeddingExecutionTimeout:     8,

		SkillsEnabled:              true,
		SkillsDir:                  ".Lumina/PROJECT_SKILLS",
		UserSkillsDir:              filepath.Join(homeDir, ".lumina", "skills"),
		BundledSkillsDir:           filepath.Join(resourceDir, "SKILLS"),
		TeamDir:                    filepath.Join(homeDir, ".lumina", "TEAM"),
		SystemPromptPath:           filepath.Join(resourceDir, "SYSTEM", "system-prompt.md"),
		MemoryExtractionPromptPath: filepath.Join(resourceDir, "SYSTEM", "extraction_system.md"),
		ProjectRootMarkers:         []string{".git"},
		ProjectDocFilenames:        []string{"LUMINA.md", "AGENTS.md"},
		ProjectDocMaxBytes:         64 * 1024,

		UIBackend:   "prompt_toolkit_fullscreen",
		HarnessMode: "",

		WorktreeBaseRef: "HEAD",
		WorktreeDir:     ".Lumina/worktrees",

		CWD: cwd,
	}
	applyLuminaDefaults(&cfg, UserDefaultsPath(homeDir), cwd, resourceDir)
	applyEnvOverrides(&cfg)
	ApplyHarnessDefaults(&cfg)
	return cfg
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
	updated.ExtractionModel = fresh.ExtractionModel
	if !isPinned(current, "long_term_memory_enabled") {
		updated.LongTermMemoryEnabled = fresh.LongTermMemoryEnabled
	}
	if !isPinned(current, "long_term_memory_store") {
		updated.LongTermMemoryStore = fresh.LongTermMemoryStore
	}
	if !isPinned(current, "memory_context_max_tokens") {
		updated.MemoryContextMaxTokens = fresh.MemoryContextMaxTokens
	}
	if !isPinned(current, "memory_recall_max_items") {
		updated.MemoryRecallMaxItems = fresh.MemoryRecallMaxItems
	}
	updated.MemoryEmbeddingEnabled = fresh.MemoryEmbeddingEnabled
	updated.MemoryEmbeddingModel = fresh.MemoryEmbeddingModel
	updated.MemoryEmbeddingModelDir = fresh.MemoryEmbeddingModelDir
	updated.MemoryFTSCandidates = fresh.MemoryFTSCandidates
	updated.MemoryVectorCandidates = fresh.MemoryVectorCandidates
	updated.MemoryGraphCandidates = fresh.MemoryGraphCandidates
	updated.MemoryGraphMaxHops = fresh.MemoryGraphMaxHops
	updated.MemoryRRFK = fresh.MemoryRRFK
	updated.MemoryMMRLambda = fresh.MemoryMMRLambda
	updated.MemorySessionRetrievalEnabled = fresh.MemorySessionRetrievalEnabled
	updated.MemorySessionCandidates = fresh.MemorySessionCandidates
	updated.MemoryChunksPerSession = fresh.MemoryChunksPerSession
	updated.MemorySessionChunkCandidates = fresh.MemorySessionChunkCandidates
	updated.MemoryMMRRelevanceWeight = fresh.MemoryMMRRelevanceWeight
	updated.MemoryMMRNoveltyWeight = fresh.MemoryMMRNoveltyWeight
	updated.MemoryMMRFacetCoverageWeight = fresh.MemoryMMRFacetCoverageWeight
	updated.MemoryMMRSourceCoverageWeight = fresh.MemoryMMRSourceCoverageWeight
	updated.MemoryCoreContextTokens = fresh.MemoryCoreContextTokens
	updated.MemoryContextTargetTokens = fresh.MemoryContextTargetTokens
	updated.MemoryRetrievalLocalTimeoutSeconds = fresh.MemoryRetrievalLocalTimeoutSeconds
	updated.MemoryEvidenceNeighborChunks = fresh.MemoryEvidenceNeighborChunks
	updated.MemoryAdjacentChunkWindow = fresh.MemoryAdjacentChunkWindow
	updated.MemoryRetrievalCacheEnabled = fresh.MemoryRetrievalCacheEnabled
	updated.MemoryRetrievalCacheTTLSeconds = fresh.MemoryRetrievalCacheTTLSeconds
	updated.MemoryCanonicalEntityEnabled = fresh.MemoryCanonicalEntityEnabled
	updated.MemoryCanonicalEventEnabled = fresh.MemoryCanonicalEventEnabled
	updated.MemoryCanonicalSimilarityThreshold = fresh.MemoryCanonicalSimilarityThreshold
	updated.MemoryCanonicalConfirmationModel = fresh.MemoryCanonicalConfirmationModel
	updated.MemoryCanonicalConfirmationTimeout = fresh.MemoryCanonicalConfirmationTimeout
	updated.MemoryConfigErrors = append([]string(nil), fresh.MemoryConfigErrors...)
	updated.MemoryQueryExpansionEnabled = fresh.MemoryQueryExpansionEnabled
	updated.MemoryQueryExpansionModel = fresh.MemoryQueryExpansionModel
	updated.MemoryQueryExpansionTimeoutSeconds = fresh.MemoryQueryExpansionTimeoutSeconds
	updated.MemoryQueryExpansionAdditionalWait = fresh.MemoryQueryExpansionAdditionalWait
	updated.MemoryQueryExpansionMaxContext = fresh.MemoryQueryExpansionMaxContext
	updated.MemoryQueryExpansionMaxQueries = fresh.MemoryQueryExpansionMaxQueries
	updated.MemoryWriteConfirmUserScope = fresh.MemoryWriteConfirmUserScope
	updated.MemoryWriteConfirmProcedural = fresh.MemoryWriteConfirmProcedural
	if !isPinned(current, "memory_background_extraction_enabled") {
		updated.MemoryBackgroundExtractionEnabled = fresh.MemoryBackgroundExtractionEnabled
	}
	updated.MemoryBackgroundExtractionInterval = fresh.MemoryBackgroundExtractionInterval
	updated.MemoryRetentionDays = cloneStringIntMap(fresh.MemoryRetentionDays)
	updated.MemoryLifecycleEnabled = fresh.MemoryLifecycleEnabled
	updated.MemoryMaintenanceIntervalSeconds = fresh.MemoryMaintenanceIntervalSeconds
	updated.MemoryHotAccessDays = fresh.MemoryHotAccessDays
	updated.MemoryWarmAccessDays = fresh.MemoryWarmAccessDays
	updated.MemoryAccessRecencyHalfLifeDays = fresh.MemoryAccessRecencyHalfLifeDays
	updated.MemoryArchiveGraceDays = fresh.MemoryArchiveGraceDays
	updated.MemoryArchiveValueThreshold = fresh.MemoryArchiveValueThreshold
	updated.MemoryAutoHardDeleteEnabled = fresh.MemoryAutoHardDeleteEnabled
	updated.MemoryValueWeights = cloneStringFloatMap(fresh.MemoryValueWeights)
	updated.MemoryAtomTargetTokens = fresh.MemoryAtomTargetTokens
	updated.MemoryAtomMaxTokens = fresh.MemoryAtomMaxTokens
	updated.MemoryAtomMaxSelected = fresh.MemoryAtomMaxSelected
	updated.MemoryCoverageMaxFacets = fresh.MemoryCoverageMaxFacets
	updated.MemoryCoverageCompletionRounds = fresh.MemoryCoverageCompletionRounds
	updated.MemoryCoverageRelevanceWeight = fresh.MemoryCoverageRelevanceWeight
	updated.MemoryCoverageFacetWeight = fresh.MemoryCoverageFacetWeight
	updated.MemoryCoverageProvenanceWeight = fresh.MemoryCoverageProvenanceWeight
	updated.MemoryCoverageSourceWeight = fresh.MemoryCoverageSourceWeight
	updated.MemoryCoverageCoherenceWeight = fresh.MemoryCoverageCoherenceWeight
	updated.MemoryCoverageSupportTarget = fresh.MemoryCoverageSupportTarget
	updated.MemoryCoverageResidualTrigger = fresh.MemoryCoverageResidualTrigger
	updated.MemoryCoverageMinMarginalGain = fresh.MemoryCoverageMinMarginalGain
	updated.MemoryAtomStructuralContextEnabled = fresh.MemoryAtomStructuralContextEnabled
	updated.MemoryAtomStructuralContextTokens = fresh.MemoryAtomStructuralContextTokens
	updated.MemoryEvidencePrimaryBudgetRatio = fresh.MemoryEvidencePrimaryBudgetRatio
	updated.MemoryEvidenceCompletionBudgetRatio = fresh.MemoryEvidenceCompletionBudgetRatio
	updated.MemoryEvidenceContextBudgetRatio = fresh.MemoryEvidenceContextBudgetRatio
	updated.MemoryEmbeddingBatchSize = fresh.MemoryEmbeddingBatchSize
	updated.MemoryEmbeddingBatchWaitMS = fresh.MemoryEmbeddingBatchWaitMS
	updated.MemoryEmbeddingQueryCacheEntries = fresh.MemoryEmbeddingQueryCacheEntries
	updated.MemoryEmbeddingExecutionTimeout = fresh.MemoryEmbeddingExecutionTimeout
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
	if !isPinned(current, "project_runtime_dir") {
		updated.ProjectRuntimeDir = fresh.ProjectRuntimeDir
	}
	if !isPinned(current, "harness_mode") {
		updated.HarnessMode = fresh.HarnessMode
	}
	updated.TeamDir = teamDir
	updated.SystemPromptPath = systemPromptPath
	updated.SkillsDir = skillsDir
	updated.UserSkillsDir = userSkillsDir
	updated.BundledSkillsDir = bundledSkillsDir
	updated.IsolatedSkillsOnly = isolatedSkillsOnly
	ApplyHarnessDefaults(&updated)
	return updated
}

func ProjectRuntimeName(projectRoot string) string {
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
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".lumina", "project", ProjectRuntimeName(projectRoot))
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

func cloneStringIntMap(values map[string]int) map[string]int {
	if values == nil {
		return nil
	}
	out := make(map[string]int, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func cloneStringFloatMap(values map[string]float64) map[string]float64 {
	if values == nil {
		return nil
	}
	out := make(map[string]float64, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

type luminaDefaults struct {
	APIKey                              *string            `json:"api_key"`
	APIBaseURL                          *string            `json:"api_base_url"`
	APIModel                            *string            `json:"api_model"`
	APIType                             *string            `json:"api_type"`
	FallbackAPIEnabled                  *bool              `json:"fallback_api_enabled"`
	FallbackAPIKey                      *string            `json:"fallback_api_key"`
	FallbackAPIBaseURL                  *string            `json:"fallback_api_base_url"`
	FallbackAPIModel                    *string            `json:"fallback_api_model"`
	FallbackAPIType                     *string            `json:"fallback_api_type"`
	APIMaxTokens                        *int               `json:"api_max_tokens"`
	APIStreamIdleTimeoutSeconds         *float64           `json:"api_stream_idle_timeout_seconds"`
	MaxToolOutputChars                  *int               `json:"max_tool_output_chars"`
	MaxToolResultCharsAbsolute          *int               `json:"max_tool_result_chars_absolute"`
	MaxMessageToolResultsChars          *int               `json:"max_message_tool_results_chars"`
	ShellTimeoutSeconds                 *float64           `json:"shell_timeout_seconds"`
	ShellMaxOutputBytes                 *int               `json:"shell_max_output_bytes"`
	MCPEnabled                          *bool              `json:"mcp_enabled"`
	MCPPingInterval                     *float64           `json:"mcp_ping_interval"`
	MCPConnectTimeout                   *float64           `json:"mcp_connect_timeout"`
	MCPRequestTimeout                   *float64           `json:"mcp_request_timeout"`
	WebSearchEnabled                    *bool              `json:"web_search_enabled"`
	WebSearchProvider                   *string            `json:"web_search_provider"`
	WebSearchBaseURL                    *string            `json:"web_search_base_url"`
	WebSearchMaxResults                 *int               `json:"web_search_max_results"`
	WebSearchTimeoutSeconds             *float64           `json:"web_search_timeout_seconds"`
	WebFetchEnabled                     *bool              `json:"web_fetch_enabled"`
	WebFetchRequireSearch               *bool              `json:"web_fetch_require_search_result"`
	WebFetchMaxChars                    *int               `json:"web_fetch_max_chars"`
	WebFetchTimeoutSeconds              *float64           `json:"web_fetch_timeout_seconds"`
	WebFetchUserAgent                   *string            `json:"web_fetch_user_agent"`
	ContextCompressThreshold            *float64           `json:"context_compress_threshold"`
	PromptCacheTTLSeconds               *float64           `json:"prompt_cache_ttl_seconds"`
	AnthropicCacheEditsEnabled          *bool              `json:"anthropic_cache_edits_enabled"`
	MaxParentTurns                      *int               `json:"max_parent_turns"`
	SessionDir                          *string            `json:"session_dir"`
	SessionMemoryEnabled                *bool              `json:"session_memory_enabled"`
	SessionMemoryTurnInterval           *int               `json:"session_memory_turn_interval"`
	SessionMemorySummaryModel           *string            `json:"session_memory_summary_model"`
	SessionMemorySummaryMaxTokens       *int               `json:"session_memory_summary_max_tokens"`
	SessionHistoryGetMessageLimit       *int               `json:"session_history_get_message_limit"`
	SessionMemoryMaxCommits             *int               `json:"session_memory_max_commits"`
	SessionMemoryMaxMessages            *int               `json:"session_memory_max_messages"`
	SessionMemoryVacuumAfterEviction    *bool              `json:"session_memory_vacuum_after_eviction"`
	SessionMaintenanceEnabled           *bool              `json:"session_maintenance_enabled"`
	SessionMaintenanceMode              *string            `json:"session_maintenance_mode"`
	SessionRetentionDays                *int               `json:"session_retention_days"`
	SessionMaxEntries                   *int               `json:"session_max_entries"`
	SessionMaxDiskBytes                 *int64             `json:"session_max_disk_bytes"`
	SessionHighWaterRatio               *float64           `json:"session_high_water_ratio"`
	SessionArchiveBeforeDelete          *bool              `json:"session_archive_before_delete"`
	SessionProtectPinned                *bool              `json:"session_protect_pinned"`
	TeamTimelineMaxEntries              *int               `json:"team_timeline_max_entries"`
	TeamDialogueMaxEntries              *int               `json:"team_dialogue_max_entries"`
	TeamArtifactMaxBytes                *int64             `json:"team_artifact_max_bytes"`
	ExtractionModel                     *string            `json:"extraction_model"`
	LongTermMemoryEnabled               *bool              `json:"long_term_memory_enabled"`
	LongTermMemoryStore                 *string            `json:"long_term_memory_store"`
	MemoryContextMaxTokens              *int               `json:"memory_context_max_tokens"`
	MemoryRecallMaxItems                *int               `json:"memory_recall_max_items"`
	MemoryEmbeddingEnabled              *bool              `json:"memory_embedding_enabled"`
	MemoryEmbeddingModel                *string            `json:"memory_embedding_model"`
	MemoryEmbeddingModelDir             *string            `json:"memory_embedding_model_dir"`
	MemoryFTSCandidates                 *int               `json:"memory_fts_candidates"`
	MemoryVectorCandidates              *int               `json:"memory_vector_candidates"`
	MemoryGraphCandidates               *int               `json:"memory_graph_candidates"`
	MemoryGraphMaxHops                  *int               `json:"memory_graph_max_hops"`
	MemoryRRFK                          *int               `json:"memory_rrf_k"`
	MemoryMMRLambda                     *float64           `json:"memory_mmr_lambda"`
	MemorySessionRetrievalEnabled       *bool              `json:"memory_session_retrieval_enabled"`
	MemorySessionCandidates             *int               `json:"memory_session_candidates"`
	MemoryChunksPerSession              *int               `json:"memory_chunks_per_session"`
	MemorySessionChunkCandidates        *int               `json:"memory_session_chunk_candidates"`
	MemoryMMRRelevanceWeight            *float64           `json:"memory_mmr_relevance_weight"`
	MemoryMMRNoveltyWeight              *float64           `json:"memory_mmr_novelty_weight"`
	MemoryMMRFacetCoverageWeight        *float64           `json:"memory_mmr_facet_coverage_weight"`
	MemoryMMRSourceCoverageWeight       *float64           `json:"memory_mmr_source_coverage_weight"`
	MemoryCoreContextTokens             *int               `json:"memory_core_context_tokens"`
	MemoryContextTargetTokens           *int               `json:"memory_context_target_tokens"`
	MemoryRetrievalLocalTimeoutSeconds  *float64           `json:"memory_retrieval_local_timeout_seconds"`
	MemoryAdjacentChunkWindow           *int               `json:"memory_adjacent_chunk_window"`
	MemoryRetrievalCacheEnabled         *bool              `json:"memory_retrieval_cache_enabled"`
	MemoryRetrievalCacheTTLSeconds      *float64           `json:"memory_retrieval_cache_ttl_seconds"`
	MemoryCanonicalEntityEnabled        *bool              `json:"memory_canonical_entity_enabled"`
	MemoryCanonicalEventEnabled         *bool              `json:"memory_canonical_event_enabled"`
	MemoryCanonicalSimilarityThreshold  *float64           `json:"memory_canonical_similarity_threshold"`
	MemoryCanonicalConfirmationModel    *string            `json:"memory_canonical_confirmation_model"`
	MemoryCanonicalConfirmationTimeout  *float64           `json:"memory_canonical_confirmation_timeout_seconds"`
	MemoryQueryExpansionEnabled         *bool              `json:"memory_query_expansion_enabled"`
	MemoryQueryExpansionModel           *string            `json:"memory_query_expansion_model"`
	MemoryQueryExpansionTimeoutSeconds  *float64           `json:"memory_query_expansion_timeout_seconds"`
	MemoryQueryExpansionAdditionalWait  *int               `json:"memory_query_expansion_max_additional_wait_ms"`
	MemoryQueryExpansionMaxContext      *int               `json:"memory_query_expansion_max_context_tokens"`
	MemoryQueryExpansionMaxQueries      *int               `json:"memory_query_expansion_max_queries"`
	MemoryWriteConfirmUserScope         *bool              `json:"memory_write_requires_confirmation_for_user_scope"`
	MemoryWriteConfirmProcedural        *bool              `json:"memory_write_requires_confirmation_for_procedural"`
	MemoryBackgroundExtractionEnabled   *bool              `json:"memory_background_extraction_enabled"`
	MemoryBackgroundExtractionInterval  *int               `json:"memory_background_extraction_turn_interval"`
	MemoryRetentionDays                 map[string]int     `json:"memory_retention_days"`
	MemoryLifecycleEnabled              *bool              `json:"memory_lifecycle_enabled"`
	MemoryMaintenanceIntervalSeconds    *int               `json:"memory_maintenance_interval_seconds"`
	MemoryHotAccessDays                 *int               `json:"memory_hot_access_days"`
	MemoryWarmAccessDays                *int               `json:"memory_warm_access_days"`
	MemoryAccessRecencyHalfLifeDays     *int               `json:"memory_access_recency_half_life_days"`
	MemoryArchiveGraceDays              *int               `json:"memory_archive_grace_days"`
	MemoryArchiveValueThreshold         *float64           `json:"memory_archive_value_threshold"`
	MemoryAutoHardDeleteEnabled         *bool              `json:"memory_auto_hard_delete_enabled"`
	MemoryValueWeights                  map[string]float64 `json:"memory_value_weights"`
	MemoryAtomTargetTokens              *int               `json:"memory_atom_target_tokens"`
	MemoryAtomMaxTokens                 *int               `json:"memory_atom_max_tokens"`
	MemoryAtomMaxSelected               *int               `json:"memory_atom_max_selected"`
	MemoryCoverageMaxFacets             *int               `json:"memory_coverage_max_facets"`
	MemoryCoverageCompletionRounds      *int               `json:"memory_coverage_completion_rounds"`
	MemoryCoverageRelevanceWeight       *float64           `json:"memory_coverage_relevance_weight"`
	MemoryCoverageFacetWeight           *float64           `json:"memory_coverage_facet_weight"`
	MemoryCoverageProvenanceWeight      *float64           `json:"memory_coverage_provenance_weight"`
	MemoryCoverageSourceWeight          *float64           `json:"memory_coverage_source_weight"`
	MemoryCoverageCoherenceWeight       *float64           `json:"memory_coverage_coherence_weight"`
	MemoryCoverageSupportTarget         *float64           `json:"memory_coverage_support_target"`
	MemoryCoverageResidualTrigger       *float64           `json:"memory_coverage_residual_trigger"`
	MemoryCoverageMinMarginalGain       *float64           `json:"memory_coverage_min_marginal_gain"`
	MemoryAtomStructuralContextEnabled  *bool              `json:"memory_atom_structural_context_enabled"`
	MemoryAtomStructuralContextTokens   *int               `json:"memory_atom_structural_context_max_tokens"`
	MemoryEvidencePrimaryBudgetRatio    *float64           `json:"memory_evidence_primary_budget_ratio"`
	MemoryEvidenceCompletionBudgetRatio *float64           `json:"memory_evidence_completion_budget_ratio"`
	MemoryEvidenceContextBudgetRatio    *float64           `json:"memory_evidence_context_budget_ratio"`
	MemoryEmbeddingBatchSize            *int               `json:"memory_embedding_batch_size"`
	MemoryEmbeddingBatchWaitMS          *int               `json:"memory_embedding_batch_wait_ms"`
	MemoryEmbeddingQueryCacheEntries    *int               `json:"memory_embedding_query_cache_entries"`
	MemoryEmbeddingExecutionTimeout     *float64           `json:"memory_embedding_execution_timeout_seconds"`
	SkillsEnabled                       *bool              `json:"skills_enabled"`
	SkillsDir                           *string            `json:"skills_dir"`
	UserSkillsDir                       *string            `json:"user_skills_dir"`
	BundledSkillsDir                    *string            `json:"bundled_skills_dir"`
	TeamDir                             *string            `json:"team_dir"`
	SystemPromptPath                    *string            `json:"system_prompt_path"`
	MemoryExtractionPromptPath          *string            `json:"memory_extraction_prompt_path"`
	ProjectRootMarkers                  []string           `json:"project_root_markers"`
	ProjectDocFilenames                 []string           `json:"project_doc_filenames"`
	ProjectDocMaxBytes                  *int               `json:"project_doc_max_bytes"`
	UIBackend                           *string            `json:"ui_backend"`
	HarnessMode                         *string            `json:"harness_mode"`
	WorktreeBaseRef                     *string            `json:"worktree_base_ref"`
	WorktreeDir                         *string            `json:"worktree_dir"`
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
	if defaults.SessionDir != nil {
		cfg.SessionDir = resolveProjectPath(cwd, *defaults.SessionDir)
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
	if defaults.ExtractionModel != nil {
		cfg.ExtractionModel = defaults.ExtractionModel
	}
	if defaults.LongTermMemoryEnabled != nil {
		cfg.LongTermMemoryEnabled = *defaults.LongTermMemoryEnabled
	}
	if defaults.LongTermMemoryStore != nil {
		cfg.LongTermMemoryStore = resolveProjectPath(cwd, expandHome(*defaults.LongTermMemoryStore))
	}
	if defaults.MemoryContextMaxTokens != nil && *defaults.MemoryContextMaxTokens > 0 {
		cfg.MemoryContextMaxTokens = *defaults.MemoryContextMaxTokens
	}
	if defaults.MemoryEmbeddingEnabled != nil {
		cfg.MemoryEmbeddingEnabled = *defaults.MemoryEmbeddingEnabled
	}
	if defaults.MemoryEmbeddingModel != nil && strings.TrimSpace(*defaults.MemoryEmbeddingModel) != "" {
		cfg.MemoryEmbeddingModel = strings.TrimSpace(*defaults.MemoryEmbeddingModel)
	}
	if defaults.MemoryEmbeddingModelDir != nil && strings.TrimSpace(*defaults.MemoryEmbeddingModelDir) != "" {
		cfg.MemoryEmbeddingModelDir = expandHome(*defaults.MemoryEmbeddingModelDir)
	}
	if defaults.MemoryFTSCandidates != nil && *defaults.MemoryFTSCandidates > 0 {
		cfg.MemoryFTSCandidates = *defaults.MemoryFTSCandidates
	}
	if defaults.MemoryVectorCandidates != nil && *defaults.MemoryVectorCandidates > 0 {
		cfg.MemoryVectorCandidates = *defaults.MemoryVectorCandidates
	}
	if defaults.MemoryGraphCandidates != nil && *defaults.MemoryGraphCandidates > 0 {
		cfg.MemoryGraphCandidates = *defaults.MemoryGraphCandidates
	}
	if defaults.MemoryGraphMaxHops != nil && *defaults.MemoryGraphMaxHops >= -1 {
		cfg.MemoryGraphMaxHops = *defaults.MemoryGraphMaxHops
	}
	if defaults.MemoryRRFK != nil && *defaults.MemoryRRFK > 0 {
		cfg.MemoryRRFK = *defaults.MemoryRRFK
	}
	if defaults.MemorySessionRetrievalEnabled != nil {
		cfg.MemorySessionRetrievalEnabled = *defaults.MemorySessionRetrievalEnabled
	}
	applyPositiveMemoryInt(cfg, "memory_session_candidates", defaults.MemorySessionCandidates, &cfg.MemorySessionCandidates)
	applyPositiveMemoryInt(cfg, "memory_chunks_per_session", defaults.MemoryChunksPerSession, &cfg.MemoryChunksPerSession)
	applyPositiveMemoryInt(cfg, "memory_session_chunk_candidates", defaults.MemorySessionChunkCandidates, &cfg.MemorySessionChunkCandidates)
	if defaults.MemoryCoreContextTokens != nil && *defaults.MemoryCoreContextTokens > 0 {
		cfg.MemoryCoreContextTokens = *defaults.MemoryCoreContextTokens
	}
	if defaults.MemoryContextTargetTokens != nil && *defaults.MemoryContextTargetTokens > 0 {
		cfg.MemoryContextTargetTokens = *defaults.MemoryContextTargetTokens
	}
	if defaults.MemoryRetrievalLocalTimeoutSeconds != nil && *defaults.MemoryRetrievalLocalTimeoutSeconds > 0 {
		cfg.MemoryRetrievalLocalTimeoutSeconds = *defaults.MemoryRetrievalLocalTimeoutSeconds
	}
	if defaults.MemoryAdjacentChunkWindow != nil {
		if *defaults.MemoryAdjacentChunkWindow < 0 {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_adjacent_chunk_window must be non-negative")
		} else {
			cfg.MemoryAdjacentChunkWindow = *defaults.MemoryAdjacentChunkWindow
			cfg.MemoryEvidenceNeighborChunks = *defaults.MemoryAdjacentChunkWindow
		}
	}
	if defaults.MemoryRetrievalCacheEnabled != nil {
		cfg.MemoryRetrievalCacheEnabled = *defaults.MemoryRetrievalCacheEnabled
	}
	if defaults.MemoryRetrievalCacheTTLSeconds != nil {
		if *defaults.MemoryRetrievalCacheTTLSeconds < 0 {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_retrieval_cache_ttl_seconds must be non-negative")
		} else {
			cfg.MemoryRetrievalCacheTTLSeconds = *defaults.MemoryRetrievalCacheTTLSeconds
		}
	}
	if defaults.MemoryCanonicalEntityEnabled != nil {
		cfg.MemoryCanonicalEntityEnabled = *defaults.MemoryCanonicalEntityEnabled
	}
	if defaults.MemoryCanonicalEventEnabled != nil {
		cfg.MemoryCanonicalEventEnabled = *defaults.MemoryCanonicalEventEnabled
	}
	if defaults.MemoryCanonicalSimilarityThreshold != nil {
		applyMemoryWeight(cfg, "memory_canonical_similarity_threshold", defaults.MemoryCanonicalSimilarityThreshold, &cfg.MemoryCanonicalSimilarityThreshold)
	}
	if defaults.MemoryCanonicalConfirmationModel != nil && strings.TrimSpace(*defaults.MemoryCanonicalConfirmationModel) != "" {
		cfg.MemoryCanonicalConfirmationModel = strings.TrimSpace(*defaults.MemoryCanonicalConfirmationModel)
	}
	if defaults.MemoryCanonicalConfirmationTimeout != nil {
		if *defaults.MemoryCanonicalConfirmationTimeout <= 0 {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_canonical_confirmation_timeout_seconds must be positive")
		} else {
			cfg.MemoryCanonicalConfirmationTimeout = *defaults.MemoryCanonicalConfirmationTimeout
		}
	}
	if defaults.MemoryQueryExpansionEnabled != nil {
		cfg.MemoryQueryExpansionEnabled = *defaults.MemoryQueryExpansionEnabled
	}
	if defaults.MemoryQueryExpansionModel != nil && strings.TrimSpace(*defaults.MemoryQueryExpansionModel) != "" {
		cfg.MemoryQueryExpansionModel = strings.TrimSpace(*defaults.MemoryQueryExpansionModel)
	}
	if defaults.MemoryQueryExpansionTimeoutSeconds != nil && *defaults.MemoryQueryExpansionTimeoutSeconds >= 0 {
		cfg.MemoryQueryExpansionTimeoutSeconds = *defaults.MemoryQueryExpansionTimeoutSeconds
	}
	if defaults.MemoryQueryExpansionAdditionalWait != nil {
		if *defaults.MemoryQueryExpansionAdditionalWait < 0 {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_query_expansion_max_additional_wait_ms must be non-negative")
		} else {
			cfg.MemoryQueryExpansionAdditionalWait = *defaults.MemoryQueryExpansionAdditionalWait
		}
	}
	if defaults.MemoryQueryExpansionMaxContext != nil && *defaults.MemoryQueryExpansionMaxContext > 0 {
		cfg.MemoryQueryExpansionMaxContext = *defaults.MemoryQueryExpansionMaxContext
	}
	if defaults.MemoryQueryExpansionMaxQueries != nil && *defaults.MemoryQueryExpansionMaxQueries > 0 {
		cfg.MemoryQueryExpansionMaxQueries = *defaults.MemoryQueryExpansionMaxQueries
	}
	if defaults.MemoryWriteConfirmUserScope != nil {
		cfg.MemoryWriteConfirmUserScope = *defaults.MemoryWriteConfirmUserScope
	}
	if defaults.MemoryWriteConfirmProcedural != nil {
		cfg.MemoryWriteConfirmProcedural = *defaults.MemoryWriteConfirmProcedural
	}
	if defaults.MemoryBackgroundExtractionEnabled != nil {
		cfg.MemoryBackgroundExtractionEnabled = *defaults.MemoryBackgroundExtractionEnabled
	}
	if defaults.MemoryBackgroundExtractionInterval != nil && *defaults.MemoryBackgroundExtractionInterval > 0 {
		cfg.MemoryBackgroundExtractionInterval = *defaults.MemoryBackgroundExtractionInterval
	}
	if len(defaults.MemoryRetentionDays) > 0 {
		cfg.MemoryRetentionDays = cloneStringIntMap(defaults.MemoryRetentionDays)
	}
	if defaults.MemoryLifecycleEnabled != nil {
		cfg.MemoryLifecycleEnabled = *defaults.MemoryLifecycleEnabled
	}
	if defaults.MemoryMaintenanceIntervalSeconds != nil {
		if *defaults.MemoryMaintenanceIntervalSeconds <= 0 {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_maintenance_interval_seconds must be positive")
		} else {
			cfg.MemoryMaintenanceIntervalSeconds = *defaults.MemoryMaintenanceIntervalSeconds
		}
	}
	if defaults.MemoryHotAccessDays != nil {
		if *defaults.MemoryHotAccessDays < 0 {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_hot_access_days must be non-negative")
		} else {
			cfg.MemoryHotAccessDays = *defaults.MemoryHotAccessDays
		}
	}
	if defaults.MemoryWarmAccessDays != nil {
		if *defaults.MemoryWarmAccessDays < 0 {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_warm_access_days must be non-negative")
		} else {
			cfg.MemoryWarmAccessDays = *defaults.MemoryWarmAccessDays
		}
	}
	if cfg.MemoryHotAccessDays > cfg.MemoryWarmAccessDays {
		cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_hot_access_days must not exceed memory_warm_access_days")
	}
	if defaults.MemoryAccessRecencyHalfLifeDays != nil {
		if *defaults.MemoryAccessRecencyHalfLifeDays <= 0 {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_access_recency_half_life_days must be positive")
		} else {
			cfg.MemoryAccessRecencyHalfLifeDays = *defaults.MemoryAccessRecencyHalfLifeDays
		}
	}
	if defaults.MemoryArchiveGraceDays != nil {
		if *defaults.MemoryArchiveGraceDays < 0 {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_archive_grace_days must be non-negative")
		} else {
			cfg.MemoryArchiveGraceDays = *defaults.MemoryArchiveGraceDays
		}
	}
	if defaults.MemoryArchiveValueThreshold != nil {
		if *defaults.MemoryArchiveValueThreshold < 0 || *defaults.MemoryArchiveValueThreshold > 1 {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_archive_value_threshold must be between 0 and 1")
		} else {
			cfg.MemoryArchiveValueThreshold = *defaults.MemoryArchiveValueThreshold
		}
	}
	if defaults.MemoryAutoHardDeleteEnabled != nil {
		cfg.MemoryAutoHardDeleteEnabled = *defaults.MemoryAutoHardDeleteEnabled
		if *defaults.MemoryAutoHardDeleteEnabled {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_auto_hard_delete_enabled must be false; automatic hard deletion is not supported")
		}
	}
	if len(defaults.MemoryValueWeights) > 0 {
		allowed := map[string]bool{"importance": true, "confidence": true, "access_recency": true,
			"access_frequency": true, "reinforcement": true, "provenance_strength": true, "dependency_strength": true}
		total := 0.0
		valid := true
		for key, value := range defaults.MemoryValueWeights {
			if !allowed[key] {
				cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_value_weights contains unknown key "+key)
				valid = false
			}
			if value < 0 || value > 1 {
				cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_value_weights."+key+" must be between 0 and 1")
				valid = false
			}
			total += value
		}
		if len(defaults.MemoryValueWeights) != len(allowed) {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_value_weights must define all lifecycle signals")
			valid = false
		}
		if math.Abs(total-1) > 1e-9 {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_value_weights must sum to 1")
			valid = false
		}
		if valid {
			cfg.MemoryValueWeights = cloneStringFloatMap(defaults.MemoryValueWeights)
		}
	}
	applyPositiveMemoryInt(cfg, "memory_atom_target_tokens", defaults.MemoryAtomTargetTokens, &cfg.MemoryAtomTargetTokens)
	applyPositiveMemoryInt(cfg, "memory_atom_max_tokens", defaults.MemoryAtomMaxTokens, &cfg.MemoryAtomMaxTokens)
	applyPositiveMemoryInt(cfg, "memory_atom_max_selected", defaults.MemoryAtomMaxSelected, &cfg.MemoryAtomMaxSelected)
	applyPositiveMemoryInt(cfg, "memory_coverage_max_facets", defaults.MemoryCoverageMaxFacets, &cfg.MemoryCoverageMaxFacets)
	if defaults.MemoryCoverageCompletionRounds != nil {
		if *defaults.MemoryCoverageCompletionRounds < 0 {
			cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_coverage_completion_rounds must be non-negative")
		} else {
			cfg.MemoryCoverageCompletionRounds = *defaults.MemoryCoverageCompletionRounds
		}
	}
	applyMemoryWeight(cfg, "memory_coverage_relevance_weight", defaults.MemoryCoverageRelevanceWeight, &cfg.MemoryCoverageRelevanceWeight)
	applyMemoryWeight(cfg, "memory_coverage_facet_weight", defaults.MemoryCoverageFacetWeight, &cfg.MemoryCoverageFacetWeight)
	applyMemoryWeight(cfg, "memory_coverage_provenance_weight", defaults.MemoryCoverageProvenanceWeight, &cfg.MemoryCoverageProvenanceWeight)
	applyMemoryWeight(cfg, "memory_coverage_source_weight", defaults.MemoryCoverageSourceWeight, &cfg.MemoryCoverageSourceWeight)
	applyMemoryWeight(cfg, "memory_coverage_coherence_weight", defaults.MemoryCoverageCoherenceWeight, &cfg.MemoryCoverageCoherenceWeight)
	applyMemoryWeight(cfg, "memory_coverage_support_target", defaults.MemoryCoverageSupportTarget, &cfg.MemoryCoverageSupportTarget)
	applyMemoryWeight(cfg, "memory_coverage_residual_trigger", defaults.MemoryCoverageResidualTrigger, &cfg.MemoryCoverageResidualTrigger)
	applyMemoryWeight(cfg, "memory_coverage_min_marginal_gain", defaults.MemoryCoverageMinMarginalGain, &cfg.MemoryCoverageMinMarginalGain)
	if defaults.MemoryAtomStructuralContextEnabled != nil {
		cfg.MemoryAtomStructuralContextEnabled = *defaults.MemoryAtomStructuralContextEnabled
	}
	applyPositiveMemoryInt(cfg, "memory_atom_structural_context_max_tokens", defaults.MemoryAtomStructuralContextTokens, &cfg.MemoryAtomStructuralContextTokens)
	applyMemoryWeight(cfg, "memory_evidence_primary_budget_ratio", defaults.MemoryEvidencePrimaryBudgetRatio, &cfg.MemoryEvidencePrimaryBudgetRatio)
	applyMemoryWeight(cfg, "memory_evidence_completion_budget_ratio", defaults.MemoryEvidenceCompletionBudgetRatio, &cfg.MemoryEvidenceCompletionBudgetRatio)
	applyMemoryWeight(cfg, "memory_evidence_context_budget_ratio", defaults.MemoryEvidenceContextBudgetRatio, &cfg.MemoryEvidenceContextBudgetRatio)
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
	if cfg.MemoryAtomTargetTokens > cfg.MemoryAtomMaxTokens {
		cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_atom_target_tokens must not exceed memory_atom_max_tokens")
	}
	if cfg.MemoryCoverageResidualTrigger > cfg.MemoryCoverageSupportTarget {
		cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_coverage_residual_trigger must not exceed memory_coverage_support_target")
	}
	if cfg.MemoryAtomStructuralContextTokens > cfg.MemoryContextMaxTokens {
		cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_atom_structural_context_max_tokens must not exceed memory_context_max_tokens")
	}
	if cfg.MemoryQueryExpansionTimeoutSeconds > 0 &&
		float64(cfg.MemoryQueryExpansionAdditionalWait) > cfg.MemoryQueryExpansionTimeoutSeconds*1000 {
		cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory_query_expansion_max_additional_wait_ms must not exceed memory_query_expansion_timeout_seconds")
	}
	validateMemoryWeights(cfg)
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
	if defaults.TeamDir != nil {
		cfg.TeamDir = expandHome(*defaults.TeamDir)
	}
	if defaults.SystemPromptPath != nil {
		cfg.SystemPromptPath = resolveResourcePath(resourceDir, *defaults.SystemPromptPath)
	}
	if defaults.MemoryExtractionPromptPath != nil {
		cfg.MemoryExtractionPromptPath = resolveResourcePath(resourceDir, *defaults.MemoryExtractionPromptPath)
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
	if defaults.WorktreeDir != nil {
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

func applyMemoryWeight(cfg *Config, name string, value *float64, target *float64) {
	if value == nil {
		return
	}
	if *value < 0 || *value > 1 {
		cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, name+" must be between 0 and 1")
		return
	}
	*target = *value
}

func validateMemoryWeights(cfg *Config) {
	coverage := cfg.MemoryCoverageRelevanceWeight + cfg.MemoryCoverageFacetWeight +
		cfg.MemoryCoverageProvenanceWeight + cfg.MemoryCoverageSourceWeight + cfg.MemoryCoverageCoherenceWeight
	if math.Abs(coverage-1) > 1e-9 {
		cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory coverage weights must sum to 1")
	}
	budget := cfg.MemoryEvidencePrimaryBudgetRatio + cfg.MemoryEvidenceCompletionBudgetRatio + cfg.MemoryEvidenceContextBudgetRatio
	if math.Abs(budget-1) > 1e-9 {
		cfg.MemoryConfigErrors = append(cfg.MemoryConfigErrors, "memory evidence budget ratios must sum to 1")
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
	if home := userHomeDir(); home != "" {
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

func UserDefaultsPath(homeDir string) string {
	if homeDir == "" {
		homeDir = userHomeDir()
	}
	return filepath.Join(homeDir, ".lumina", "CONFIG", "defaults.json")
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
