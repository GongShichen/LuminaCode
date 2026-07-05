package test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"LuminaCode/config"
	coretools "LuminaCode/tools"
)

func TestApplyToolResultBudgetPersistsLikePython(t *testing.T) {
	dir := t.TempDir()
	raw := strings.Repeat("a", 2600)
	got, err := coretools.ApplyToolResultBudgetWithPreview(raw, "tool-1", dir, 1000, 20)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(dir, "tool-results", "tool-1.txt")
	if !strings.HasPrefix(got, "<persisted-output>\nOutput too large (2.5 KB). Full output saved to: "+wantPath+"\n\n") {
		t.Fatalf("unexpected persisted output header:\n%s", got)
	}
	if !strings.Contains(got, "Preview (first 20 characters):\n"+strings.Repeat("a", 20)+"\n...\n</persisted-output>") {
		t.Fatalf("unexpected persisted output preview:\n%s", got)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != raw {
		t.Fatalf("persisted output should contain full raw output, got len=%d", len(data))
	}
}

func TestApplyToolResultBudgetHonorsExplicitZeroLimitsLikePython(t *testing.T) {
	dir := t.TempDir()
	got, err := coretools.ApplyToolResultBudgetWithPreview("abc", "zero", dir, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(dir, "tool-results", "zero.txt")
	if !strings.Contains(got, "Full output saved to: "+wantPath) {
		t.Fatalf("max_chars=0 should still persist oversized non-empty output like Python, got:\n%s", got)
	}
	if !strings.Contains(got, "Preview (first 0 characters):\n\n...\n</persisted-output>") {
		t.Fatalf("preview_chars=0 should produce Python empty preview plus ellipsis, got:\n%s", got)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "abc" {
		t.Fatalf("persisted data=%q want abc", data)
	}
}

func TestClampToAbsoluteMaxMatchesPythonFormat(t *testing.T) {
	raw := strings.Repeat("a", 60)
	got := coretools.ClampToAbsoluteMax(raw, 20)
	if !strings.HasPrefix(got, strings.Repeat("a", 10)+"\n\n[OUTPUT TRUNCATED: 60 B, 40 characters removed — exceeds absolute limit of 20 B]\n\n") {
		t.Fatalf("unexpected clamp header: %q", got)
	}
	if !strings.HasSuffix(got, strings.Repeat("a", 10)) {
		t.Fatalf("unexpected clamp tail: %q", got)
	}
}

func TestClampToAbsoluteMaxHonorsExplicitZeroLikePython(t *testing.T) {
	got := coretools.ClampToAbsoluteMax("abc", 0)
	want := "\n\n[OUTPUT TRUNCATED: 3 B, 3 characters removed — exceeds absolute limit of 0 B]\n\nabc"
	if got != want {
		t.Fatalf("absolute_max=0 should match Python slicing semantics:\nwant %q\n got %q", want, got)
	}
}

func TestBaseToolFormatLargeResultClampsAbsoluteBeforePersisting(t *testing.T) {
	previous := config.GetConfig()
	cfg := previous
	cfg.MaxToolResultCharsAbsolute = 40
	config.SetConfig(cfg)
	t.Cleanup(func() { config.SetConfig(previous) })

	dir := t.TempDir()
	tool := coretools.NewReadFileTool()
	raw := strings.Repeat("a", 120)
	got, err := tool.FormatLargeResult(context.Background(), raw, 20, "tool-absolute", dir)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(dir, "tool-results", "tool-absolute.txt")
	if !strings.Contains(got, "Full output saved to: "+wantPath) {
		t.Fatalf("expected persisted output reference, got:\n%s", got)
	}
	data, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	persisted := string(data)
	if persisted == raw {
		t.Fatal("persisted content should be clamped to the absolute max before budget persistence")
	}
	if !strings.Contains(persisted, "80 characters removed") ||
		!strings.Contains(persisted, "exceeds absolute limit of 40 B") {
		t.Fatalf("persisted content should contain Python absolute clamp notice, got:\n%s", persisted)
	}
}

func TestApplyAggregateResultBudgetMatchesPythonLargestFirst(t *testing.T) {
	results := []map[string]any{
		{"tool_use_id": "small", "content": strings.Repeat("s", 10)},
		{"tool_use_id": "large", "content": strings.Repeat("l", 300)},
		{"tool_use_id": "medium", "content": strings.Repeat("m", 60)},
	}
	got := coretools.ApplyAggregateResultBudget(results, 250)
	if results[1]["content"].(string) != strings.Repeat("l", 300) {
		t.Fatalf("original results should not be modified: %#v", results)
	}
	if got[0]["content"].(string) != strings.Repeat("s", 10) {
		t.Fatalf("small result should be preserved, got %q", got[0]["content"])
	}
	if !strings.Contains(got[1]["content"].(string), "Aggregate budget: 120 characters truncated across 3 concurrent results") {
		t.Fatalf("largest result should be truncated first with Python notice, got %q", got[1]["content"])
	}
	if got[2]["content"].(string) != strings.Repeat("m", 60) {
		t.Fatalf("medium result should fit after largest truncation, got %q", got[2]["content"])
	}
	total := 0
	for _, result := range got {
		total += len([]rune(result["content"].(string)))
	}
	if total > 250 {
		t.Fatalf("aggregate output should fit budget, total=%d results=%#v", total, got)
	}
}
