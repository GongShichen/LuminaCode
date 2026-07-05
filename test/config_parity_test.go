package test

import (
	"os"
	"path/filepath"
	"testing"

	"LuminaCode/agent"
	"LuminaCode/config"
)

func TestConfigLoadsLuminaDefaultsAndEnvOverrides(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".Lumina", "CONFIG"), 0o755); err != nil {
		t.Fatal(err)
	}
	defaults := `{
  "api_key": "file-key",
  "api_base_url": "https://config.example/v1",
  "api_model": "file-model",
  "api_type": "auto",
  "api_max_tokens": 1234,
  "mcp_enabled": false,
  "prompt_cache_ttl_seconds": 42.5,
  "session_dir": ".Lumina/sessions-local",
  "api_input_price_per_1k": 0.12,
  "api_output_price_per_1k": 0.34,
  "auto_memory_directory": ".Lumina/memory",
  "extraction_model": "extract-model",
  "skills_dir": ".Lumina/PROJECT_SKILLS",
  "bundled_skills_dir": ".Lumina/SKILLS",
  "system_prompt_path": ".Lumina/SYSTEM/system-prompt.md",
  "memory_extraction_prompt_path": ".Lumina/SYSTEM/extraction_system.md",
  "ui_backend": "legacy_terminal",
  "worktree_dir": ".Lumina/worktrees"
}`
	if err := os.WriteFile(filepath.Join(dir, ".Lumina", "CONFIG", "defaults.json"), []byte(defaults), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	t.Setenv("LUMINA_API_MODEL", "env-model")
	t.Setenv("LUMINA_API_TYPE", "openai-compatible")
	t.Setenv("LUMINA_PROMPT_CACHE_TTL_SECONDS", "77")
	t.Setenv("LUMINA_ANTHROPIC_CACHE_EDITS", "true")
	t.Setenv("LUMINA_UI_BACKEND", "legacy_terminal")

	cfg := config.NewConfig()
	if cfg.APIMaxTokens != 1234 || cfg.MCPEnabled {
		t.Fatalf("defaults were not applied: %#v", cfg)
	}
	if cfg.APIKey != "file-key" || cfg.APIBaseURL != "https://config.example/v1" || cfg.APIModel != "env-model" || cfg.PromptCacheTTLSeconds != 77 {
		t.Fatalf("env overrides were not applied: %#v", cfg)
	}
	if cfg.APIType != "openai-compatible" {
		t.Fatalf("api type override was not applied: %#v", cfg)
	}
	if !cfg.AnthropicCacheEditsEnabled {
		t.Fatalf("anthropic cache edit env override was not applied: %#v", cfg)
	}
	if cfg.SessionDir != filepath.Join(dir, ".Lumina", "sessions-local") {
		t.Fatalf("session dir was not resolved from defaults: %s", cfg.SessionDir)
	}
	if cfg.AutoMemoryDirectory == nil || *cfg.AutoMemoryDirectory != filepath.Join(dir, ".Lumina", "memory") {
		t.Fatalf("auto memory directory was not resolved from defaults: %#v", cfg.AutoMemoryDirectory)
	}
	if cfg.ExtractionModel == nil || *cfg.ExtractionModel != "extract-model" {
		t.Fatalf("extraction model was not applied: %#v", cfg.ExtractionModel)
	}
	if cfg.APIInputPricePer1K == nil || *cfg.APIInputPricePer1K != 0.12 || cfg.APIOutputPricePer1K == nil || *cfg.APIOutputPricePer1K != 0.34 {
		t.Fatalf("pricing defaults were not applied: input=%v output=%v", cfg.APIInputPricePer1K, cfg.APIOutputPricePer1K)
	}
	if cfg.BundledSkillsDir != filepath.Join(dir, ".Lumina", "SKILLS") {
		t.Fatalf("bundled skill path was not resolved: %s", cfg.BundledSkillsDir)
	}
	if cfg.WorktreeDir != ".Lumina/worktrees" {
		t.Fatalf("worktree dir should stay relative like Python config, got %s", cfg.WorktreeDir)
	}
	if cfg.UIBackend != "prompt_toolkit_fullscreen" {
		t.Fatalf("ui backend should be forced to fullscreen, got %s", cfg.UIBackend)
	}
}

func TestConfigFindsBundledResourcesOutsideLuminaRoot(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("LUMINA_RESOURCE_ROOT", "")
	t.Setenv("LUMINA_HOME", "")
	t.Setenv("LUMINA_API_KEY", "")
	t.Setenv("LUMINA_API_BASE_URL", "")
	t.Setenv("LUMINA_API_MODEL", "")
	t.Setenv("LUMINA_API_TYPE", "")

	root := repoRoot(t)
	cfg := config.NewConfig()
	if cfg.CWD != dir {
		t.Fatalf("CWD should remain the invocation directory, got %s", cfg.CWD)
	}
	if cfg.APIMaxTokens != 1000000 {
		t.Fatalf("defaults.json should load from bundled resources outside cwd, max_tokens=%d", cfg.APIMaxTokens)
	}
	if cfg.APIBaseURL != "https://api.deepseek.com/anthropic" || cfg.APIModel != "deepseek-v4-pro[1m]" || cfg.APIType != "anthropic" {
		t.Fatalf("bundled defaults should match DeepSeek Anthropic config, base=%q model=%q type=%q", cfg.APIBaseURL, cfg.APIModel, cfg.APIType)
	}
	if cfg.BundledSkillsDir != filepath.Join(root, ".Lumina", "SKILLS") {
		t.Fatalf("bundled skills should resolve from Lumina root, got %s", cfg.BundledSkillsDir)
	}
	if cfg.SystemPromptPath != filepath.Join(root, ".Lumina", "SYSTEM", "system-prompt.md") {
		t.Fatalf("system prompt should resolve from Lumina root, got %s", cfg.SystemPromptPath)
	}
	if cfg.MemoryExtractionPromptPath != filepath.Join(root, ".Lumina", "SYSTEM", "extraction_system.md") {
		t.Fatalf("extraction prompt should resolve from Lumina root, got %s", cfg.MemoryExtractionPromptPath)
	}
	if cfg.UIBackend != "prompt_toolkit_fullscreen" {
		t.Fatalf("bundled defaults should start fullscreen by default, got %s", cfg.UIBackend)
	}
}

func TestConfigDoesNotProvideBuiltInModelDefault(t *testing.T) {
	resourceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(resourceRoot, "CONFIG"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resourceRoot, "CONFIG", "defaults.json"), []byte(`{"api_max_tokens": 1000}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	t.Setenv("LUMINA_RESOURCE_ROOT", resourceRoot)
	t.Setenv("LUMINA_API_MODEL", "")
	t.Setenv("ANTHROPIC_MODEL", "")

	cfg := config.NewConfig()
	if cfg.APIModel != "" {
		t.Fatalf("model should only come from user config/env/flag, got %q", cfg.APIModel)
	}
}

func TestConfigLoadsDirectLuminaResourceRoot(t *testing.T) {
	dir := t.TempDir()
	resourceRoot := filepath.Join(dir, ".lumina")
	if err := os.MkdirAll(filepath.Join(resourceRoot, "CONFIG"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(resourceRoot, "SYSTEM"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(resourceRoot, "SKILLS"), 0o755); err != nil {
		t.Fatal(err)
	}
	defaults := `{
  "api_max_tokens": 4321,
  "bundled_skills_dir": ".Lumina/SKILLS",
  "system_prompt_path": ".Lumina/SYSTEM/system-prompt.md",
  "memory_extraction_prompt_path": ".Lumina/SYSTEM/extraction_system.md"
}`
	if err := os.WriteFile(filepath.Join(resourceRoot, "CONFIG", "defaults.json"), []byte(defaults), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resourceRoot, "SYSTEM", "system-prompt.md"), []byte("system"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Chdir(t.TempDir())
	t.Setenv("LUMINA_RESOURCE_ROOT", resourceRoot)

	cfg := config.NewConfig()
	if cfg.APIMaxTokens != 4321 {
		t.Fatalf("direct resource defaults were not applied: %#v", cfg)
	}
	if cfg.BundledSkillsDir != filepath.Join(resourceRoot, "SKILLS") {
		t.Fatalf("direct bundled skills path mismatch: %s", cfg.BundledSkillsDir)
	}
	if cfg.SystemPromptPath != filepath.Join(resourceRoot, "SYSTEM", "system-prompt.md") {
		t.Fatalf("direct system prompt path mismatch: %s", cfg.SystemPromptPath)
	}
	if cfg.MemoryExtractionPromptPath != filepath.Join(resourceRoot, "SYSTEM", "extraction_system.md") {
		t.Fatalf("direct extraction prompt path mismatch: %s", cfg.MemoryExtractionPromptPath)
	}
}

func TestCompressionTriggerUsesConfiguredMaxTokensAndThreshold(t *testing.T) {
	cfg := config.NewConfig()
	cfg.APIMaxTokens = 1000

	if cfg.CompressionContextLimit() != 1000 {
		t.Fatalf("compression context limit=%d want 1000", cfg.CompressionContextLimit())
	}
	if cfg.CompressionThreshold() != 0.8 {
		t.Fatalf("default compression threshold=%v want 0.8", cfg.CompressionThreshold())
	}
	if cfg.CompressionTriggerTokens() != 800 {
		t.Fatalf("default compression trigger=%d want 800", cfg.CompressionTriggerTokens())
	}

	cfg.ContextCompressThreshold = 0.6
	if cfg.CompressionThreshold() != 0.6 {
		t.Fatalf("configured compression threshold=%v want 0.6", cfg.CompressionThreshold())
	}
	if cfg.CompressionTriggerTokens() != 600 {
		t.Fatalf("configured compression trigger=%d want 600", cfg.CompressionTriggerTokens())
	}
}

func TestConfigReloadDynamicConfigUpdatesDefaultsWithoutClobberingRuntimeFields(t *testing.T) {
	resourceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(resourceRoot, "CONFIG"), 0o755); err != nil {
		t.Fatal(err)
	}
	defaultsPath := filepath.Join(resourceRoot, "CONFIG", "defaults.json")
	if err := os.WriteFile(defaultsPath, []byte(`{
  "api_key": "key-one",
  "api_base_url": "https://one.example",
  "api_model": "model-one",
  "api_type": "anthropic",
  "api_max_tokens": 1000,
  "api_input_price_per_1k": 0.01,
  "api_output_price_per_1k": 0.02
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	t.Setenv("LUMINA_RESOURCE_ROOT", resourceRoot)
	t.Setenv("LUMINA_API_MODEL", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	current := config.NewConfigForCWD(workDir)
	current.CWD = filepath.Join(workDir, "runtime-cwd")
	current.Yolo = true
	current.AutoMemoryEnabled = false
	current.AutoMemoryDirectory = nil

	if err := os.WriteFile(defaultsPath, []byte(`{
  "api_key": "key-two",
  "api_base_url": "https://two.example",
  "api_model": "model-two",
  "api_type": "openai_compatible",
  "api_max_tokens": 2000,
  "api_input_price_per_1k": 0.03,
  "api_output_price_per_1k": 0.04
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	reloaded := config.ReloadDynamicConfig(current)
	if reloaded.APIKey != "key-two" || reloaded.APIBaseURL != "https://two.example" || reloaded.APIModel != "model-two" || reloaded.APIType != "openai_compatible" || reloaded.APIMaxTokens != 2000 {
		t.Fatalf("dynamic API defaults were not reloaded: %#v", reloaded)
	}
	if reloaded.CWD != current.CWD || !reloaded.Yolo || reloaded.AutoMemoryEnabled {
		t.Fatalf("runtime fields should be preserved: %#v", reloaded)
	}
	if reloaded.APIInputPricePer1K == nil || *reloaded.APIInputPricePer1K != 0.03 || reloaded.APIOutputPricePer1K == nil || *reloaded.APIOutputPricePer1K != 0.04 {
		t.Fatalf("pricing should reload: input=%v output=%v", reloaded.APIInputPricePer1K, reloaded.APIOutputPricePer1K)
	}
}

func TestQueryEngineRefreshRuntimeConfigUpdatesCoreEngine(t *testing.T) {
	resourceRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(resourceRoot, "CONFIG"), 0o755); err != nil {
		t.Fatal(err)
	}
	defaultsPath := filepath.Join(resourceRoot, "CONFIG", "defaults.json")
	if err := os.WriteFile(defaultsPath, []byte(`{
  "api_key": "key-one",
  "api_base_url": "https://one.example",
  "api_model": "model-one",
  "api_max_tokens": 1000
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	workDir := t.TempDir()
	t.Setenv("LUMINA_RESOURCE_ROOT", resourceRoot)
	t.Setenv("LUMINA_API_MODEL", "")
	t.Setenv("ANTHROPIC_MODEL", "")
	previous := config.GetConfig()
	t.Cleanup(func() { config.SetConfig(previous) })

	cfg := config.NewConfigForCWD(workDir)
	engine := agent.NewQueryEngine(&cfg)
	defer engine.Shutdown()
	if err := os.WriteFile(defaultsPath, []byte(`{
  "api_key": "key-two",
  "api_base_url": "https://two.example",
  "api_model": "model-two",
  "api_max_tokens": 2000
}`), 0o644); err != nil {
		t.Fatal(err)
	}
	engine.RefreshRuntimeConfig()
	if engine.Config.APIModel != "model-two" || engine.Config.APIMaxTokens != 2000 {
		t.Fatalf("query engine config did not refresh: %#v", engine.Config)
	}
	if engine.CoreEngine.Config.APIModel != "model-two" || engine.CoreEngine.Config.APIMaxTokens != 2000 {
		t.Fatalf("core engine config did not refresh: %#v", engine.CoreEngine.Config)
	}
	if global := config.GetConfig(); global.APIModel != "model-two" {
		t.Fatalf("global config was not refreshed: %#v", global)
	}
}
