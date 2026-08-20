package main

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"LuminaCode/config"
	"LuminaCode/session"
)

func wiringTestConfig(t *testing.T) config.Config {
	t.Helper()
	root := t.TempDir()
	cfg := config.NewConfigForCWD(root)
	cfg.SessionDir = filepath.Join(root, "sessions")
	cfg.LongTermMemoryEnabled = false
	cfg.SkillsEnabled = false
	cfg.MCPEnabled = false
	return cfg
}

func TestStaticRuntimeInjectorsWithoutExternalServices(t *testing.T) {
	cfg := wiringTestConfig(t)
	if _, err := initializeMemoryPreflight(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	prompt, cleanupPrompt, err := initializePromptRuntime(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if prompt.Engine == nil || prompt.Store == nil || prompt.Engine.Config.CWD != cfg.CWD {
		t.Fatalf("invalid prompt runtime: %#v", prompt)
	}
	cleanupPrompt()

	storeRuntime := initializeSessionStoreRuntime(SessionStoreOptions{Config: cfg, Mutable: true})
	if storeRuntime == nil || storeRuntime.Store == nil {
		t.Fatal("session store injector returned nil")
	}

	journal, err := session.OpenRuntimeJournal(context.Background(), cfg.SessionDir, "inspect-runtime")
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	inspection, cleanupInspection, err := initializeRuntimeInspectionRuntime(context.Background(), RuntimeInspectionOptions{
		Config: cfg, SessionID: "inspect-runtime",
	})
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Journal == nil || inspection.Engine == nil || inspection.Assembly == nil {
		t.Fatalf("invalid inspection runtime: %#v", inspection)
	}
	cleanupInspection()

	if _, cleanupMemory, err := initializeMemoryCommandRuntime(context.Background(), cfg); err == nil ||
		!strings.Contains(err.Error(), "unavailable") || cleanupMemory != nil {
		t.Fatalf("disabled memory injector result cleanup=%v err=%v", cleanupMemory != nil, err)
	}
}
