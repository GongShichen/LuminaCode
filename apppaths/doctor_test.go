package apppaths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorReportsHealthWithoutReadingSensitiveContent(t *testing.T) {
	paths := fixturePaths(t, t.TempDir())
	if err := WriteLayout(paths, "test"); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(paths.FrontendDir, "dist", "index.js"), "frontend")
	writeFixtureFile(t, filepath.Join(paths.SystemResourceDir, "system-prompt.md"), "system")
	secret := "doctor-must-not-print-this-token"
	writeFixtureFile(t, paths.SettingsFile, `{"api_key":"`+secret+`"}`)

	report := Doctor(paths)
	if !report.Healthy() {
		t.Fatalf("expected healthy report: %#v", report)
	}
	data, err := os.ReadFile(paths.SettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), secret) {
		t.Fatal("test fixture did not contain secret")
	}
	if strings.Contains(report.LayoutStatus+strings.Join(report.Warnings, "\n")+strings.Join(report.Unrecoverable, "\n"), secret) {
		t.Fatal("doctor exposed sensitive content")
	}
	writeFixtureFile(t, filepath.Join(paths.Root, ".layout-migration.lock"), "held")
	locked := Doctor(paths)
	if locked.Healthy() || !locked.Components["migration_lock"].Exists {
		t.Fatalf("doctor ignored migration lock: %#v", locked)
	}
}

func TestDoctorFindsProjectManifestCollision(t *testing.T) {
	paths := fixturePaths(t, t.TempDir())
	if err := WriteLayout(paths, "test"); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(paths.FrontendDir, "dist", "index.js"), "frontend")
	writeFixtureFile(t, filepath.Join(paths.SystemResourceDir, "system-prompt.md"), "system")
	manifest := `{"id":"wrong","canonical_root":"/same","display_name":"same"}`
	writeFixtureFile(t, filepath.Join(paths.ProjectsDataDir, "one", "project.json"), manifest)

	report := Doctor(paths)
	if report.Healthy() || len(report.ProjectConflicts) == 0 {
		t.Fatalf("invalid project manifest was not reported: %#v", report)
	}
}
