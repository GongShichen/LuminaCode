package test

import (
	"path/filepath"
	"testing"

	"LuminaCode/config"
	luminateam "LuminaCode/team"
)

func TestTeamSummaryReturnsExpectedFields(t *testing.T) {
	root := repoRoot(t)
	cfg := config.NewConfigForCWD(root)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", root)
	if err != nil {
		t.Fatal(err)
	}
	summary := session.Summary()
	if summary.ActiveTeamName != "Product Development Team" {
		t.Fatalf("ActiveTeamName mismatch: got %q want %q", summary.ActiveTeamName, "Product Development Team")
	}
	if summary.Running {
		t.Fatal("expected Running=false for fresh session")
	}
	if summary.ActivityCount == 0 {
		t.Fatal("expected ActivityCount > 0 for registered agents")
	}
	if summary.LoopIteration != 0 {
		t.Fatalf("expected LoopIteration=0 for fresh session, got %d", summary.LoopIteration)
	}
	if summary.DialogueCount != 0 {
		t.Fatalf("expected DialogueCount=0 for fresh session, got %d", summary.DialogueCount)
	}
	if summary.ArtifactCount != 0 {
		t.Fatalf("expected ArtifactCount=0 for fresh session, got %d", summary.ArtifactCount)
	}
	if len(summary.GateStatus) != 0 {
		t.Fatalf("expected empty GateStatus for fresh session, got %#v", summary.GateStatus)
	}
}

func TestTeamSummaryFromManagerSession(t *testing.T) {
	root := repoRoot(t)
	cfg := config.NewConfigForCWD(root)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	cfg.SessionDir = t.TempDir()
	manager := luminateam.NewManager(cfg, nil, nil)
	session, err := manager.Start("parent-session", "product-development", root)
	if err != nil {
		t.Fatal(err)
	}
	retrieved, err := manager.Get(session.ID)
	if err != nil {
		t.Fatal(err)
	}
	summary := retrieved.Summary()
	if summary.ActiveTeamName != "Product Development Team" {
		t.Fatalf("retrieved session summary ActiveTeamName mismatch: got %q", summary.ActiveTeamName)
	}
}

func TestProductDevelopmentTeamBundleUnchanged(t *testing.T) {
	root := repoRoot(t)
	cfg := config.NewConfigForCWD(root)
	cfg.TeamDir = filepath.Join(root, ".Lumina", "TEAM")
	spec, err := luminateam.NewLoader(cfg).Load("product-development")
	if err != nil {
		t.Fatal(err)
	}
	if spec.DisplayName != "Product Development Team" {
		t.Fatalf("display name mismatch: %s", spec.DisplayName)
	}
	if len(spec.AgentSpecs) < 5 {
		t.Fatalf("expected at least 5 agents, got %d", len(spec.AgentSpecs))
	}
}
