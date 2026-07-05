package test

import (
	"testing"

	"LuminaCode/memory"
)

func TestAutoMemoryEnablePriorityMatchesPython(t *testing.T) {
	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "true")
	if memory.IsAutoMemoryEnabled(true, false, false) {
		t.Fatalf("env disable should take priority")
	}

	t.Setenv("CLAUDE_CODE_DISABLE_AUTO_MEMORY", "false")
	if memory.IsAutoMemoryEnabled(true, true, false) {
		t.Fatalf("bare mode should disable auto-memory")
	}
	if memory.IsAutoMemoryEnabled(true, false, true) {
		t.Fatalf("remote mode should disable auto-memory")
	}
	if memory.IsAutoMemoryEnabled(false, false, false) {
		t.Fatalf("config false should disable auto-memory")
	}
	if !memory.IsAutoMemoryEnabled(true, false, false) {
		t.Fatalf("default enabled path should enable auto-memory")
	}
}

func TestParseBoolEnvMatchesPython(t *testing.T) {
	t.Setenv("LUMINA_TEST_BOOL", "YES")
	parsed := memory.ParseBoolEnv("LUMINA_TEST_BOOL")
	if parsed == nil || !*parsed {
		t.Fatalf("YES parsed as %#v, want true", parsed)
	}
	t.Setenv("LUMINA_TEST_BOOL", "No")
	parsed = memory.ParseBoolEnv("LUMINA_TEST_BOOL")
	if parsed == nil || *parsed {
		t.Fatalf("No parsed as %#v, want false", parsed)
	}
	t.Setenv("LUMINA_TEST_BOOL", "maybe")
	if parsed = memory.ParseBoolEnv("LUMINA_TEST_BOOL"); parsed != nil {
		t.Fatalf("invalid boolean parsed as %#v, want nil", parsed)
	}
}
