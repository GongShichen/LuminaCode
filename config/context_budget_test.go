package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLegacyInputBudgetMigrationFreezesOutputIndependentValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte(`{"api_max_tokens":8192,"vendor_option":{"keep":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{ContextInputTokenLimit: 16000}
	applyLuminaDefaults(&cfg, path, "", "", false)
	if cfg.ContextInputTokenLimit != 8192 {
		t.Fatal("legacy budget not imported")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err = json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["vendor_option"] == nil || settings["context_input_token_limit"] != float64(8192) {
		t.Fatal("migration lost unrelated config or input limit")
	}
	settings["api_max_tokens"] = 1024
	raw, _ = json.Marshal(settings)
	_ = os.WriteFile(path, raw, 0600)
	applyLuminaDefaults(&cfg, path, "", "", false)
	if cfg.APIMaxTokens != 1024 || cfg.CompressionContextLimit() != 8192 {
		t.Fatal("output changes affected the input budget")
	}
}
