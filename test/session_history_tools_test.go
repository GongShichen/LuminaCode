package test

import (
	"testing"

	"LuminaCode/agent"
	"LuminaCode/config"
)

func TestSessionHistoryToolsAreRegisteredWhenEnabled(t *testing.T) {
	cfg := config.NewConfigForCWD(t.TempDir())
	cfg.SessionMemoryEnabled = true
	engine := agent.NewCoreExecutionEngine(&cfg)

	if engine.Registry.Get("session_history_list") == nil {
		t.Fatal("session_history_list should be registered when session memory is enabled")
	}
	if engine.Registry.Get("session_history_get") == nil {
		t.Fatal("session_history_get should be registered when session memory is enabled")
	}
}

func TestSessionHistoryToolsAreNotRegisteredWhenDisabled(t *testing.T) {
	cfg := config.NewConfigForCWD(t.TempDir())
	cfg.SessionMemoryEnabled = false
	engine := agent.NewCoreExecutionEngine(&cfg)

	if engine.Registry.Get("session_history_list") != nil {
		t.Fatal("session_history_list should not be registered when session memory is disabled")
	}
	if engine.Registry.Get("session_history_get") != nil {
		t.Fatal("session_history_get should not be registered when session memory is disabled")
	}
}
