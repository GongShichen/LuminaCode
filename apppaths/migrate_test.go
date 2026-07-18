package apppaths

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestMigrateV1LayoutInPlace(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(root, "CONFIG", "defaults.json"), `{"api_model":"custom","session_dir":"~/.lumina/sessions","team_dir":"/custom/../custom/teams"}`)
	writeFixtureFile(t, filepath.Join(root, "CONFIG", "mcp.json"), `{"mcpServers":{}}`)
	writeFixtureFile(t, filepath.Join(root, "SYSTEM", "system-prompt.md"), "system")
	writeFixtureFile(t, filepath.Join(root, "SKILLS", "demo", "SKILL.md"), "skill")
	writeFixtureFile(t, filepath.Join(root, "TEAM", "demo", "team.yaml"), "name: demo")
	writeFixtureFile(t, filepath.Join(root, "memory", "lumina-memory.sqlite"), "sqlite")
	writeFixtureFile(t, filepath.Join(root, "sessions", "s1", "session.sqlite"), "session")
	writeFixtureFile(t, filepath.Join(root, "project", "repo", "CONFIG", "trusted_mcp.json"), `{"servers":{"demo":"fingerprint"}}`)
	writeFixtureFile(t, filepath.Join(root, "project", "repo", "background", "tool-results", "old.txt"), "output")
	paths := fixturePaths(t, root)

	dry, err := Migrate(paths, MigrationOptions{SourceRoot: root, CurrentProjectRoot: projectRoot})
	if err != nil {
		t.Fatalf("dry run: %v report=%#v", err, dry)
	}
	if !dry.DryRun || len(dry.Operations) == 0 {
		t.Fatalf("unexpected dry run report: %#v", dry)
	}
	if _, err := os.Stat(paths.LayoutFile); !os.IsNotExist(err) {
		t.Fatalf("dry run wrote layout: %v", err)
	}

	report, err := Migrate(paths, MigrationOptions{Apply: true, SourceRoot: root, CurrentProjectRoot: projectRoot, InstalledVersion: "test"})
	if err != nil {
		t.Fatalf("apply: %v report=%#v", err, report)
	}
	if err := CheckLayout(paths); err != nil {
		t.Fatal(err)
	}
	project, _ := paths.ForProject(projectRoot)
	for _, path := range []string{
		paths.SettingsFile, paths.MemoryDB, filepath.Join(paths.ActiveSessionsDir, "s1", "session.sqlite"),
		project.MCPTrustFile, filepath.Join(project.ToolResultsForSession("_legacy"), "old.txt"),
		filepath.Join(paths.SystemResourceDir, "system-prompt.md"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("missing migrated path %s: %v", path, err)
		}
	}
	if _, err := os.Stat(project.ManifestFile); err != nil {
		t.Fatalf("bound project manifest was not created: %v", err)
	}
	settingsData, err := os.ReadFile(paths.SettingsFile)
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := json.Unmarshal(settingsData, &settings); err != nil {
		t.Fatal(err)
	}
	if _, ok := settings["session_dir"]; ok || settings["api_model"] != "custom" || settings["team_dir"] != filepath.Clean("/custom/../custom/teams") {
		t.Fatalf("legacy defaults were not normalized: %#v", settings)
	}
	if _, err := Migrate(paths, MigrationOptions{Apply: true, SourceRoot: root, CurrentProjectRoot: projectRoot}); err != nil {
		t.Fatalf("repeat migration must be idempotent: %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var topLevel []string
	for _, entry := range entries {
		topLevel = append(topLevel, entry.Name())
	}
	sort.Strings(topLevel)
	wantTopLevel := []string{"app", "cache", "config", "data", "layout.json", "state"}
	if !reflect.DeepEqual(topLevel, wantTopLevel) {
		t.Fatalf("non-standard top-level entries remain: %#v", topLevel)
	}
}

func TestMigrateQuarantinesAmbiguousProjectTrust(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "project", "other", "CONFIG", "trusted_mcp.json"), `{"servers":{"danger":"fingerprint"}}`)
	paths := fixturePaths(t, root)
	projectRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	report, err := Migrate(paths, MigrationOptions{Apply: true, SourceRoot: root, CurrentProjectRoot: projectRoot})
	if err != nil {
		t.Fatalf("apply: %v report=%#v", err, report)
	}
	if len(report.UnresolvedProjects) != 1 || report.UnresolvedProjects[0] != "other" {
		t.Fatalf("unexpected unresolved projects: %#v", report.UnresolvedProjects)
	}
	project, _ := paths.ForProject(projectRoot)
	if _, err := os.Stat(project.MCPTrustFile); !os.IsNotExist(err) {
		t.Fatalf("ambiguous trust became active: %v", err)
	}
	legacyTrust := filepath.Join(paths.LegacyDataDir, "projects", "other", "CONFIG", "trusted_mcp.json")
	if _, err := os.Stat(legacyTrust); err != nil {
		t.Fatalf("legacy trust was not preserved: %v", err)
	}
}

func TestMigrationRefusesConflictingDestination(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "memory", "lumina-memory.sqlite"), "old")
	paths := fixturePaths(t, root)
	writeFixtureFile(t, paths.MemoryDB, "new")
	report, err := Migrate(paths, MigrationOptions{SourceRoot: root})
	if err == nil || len(report.Conflicts) == 0 {
		t.Fatalf("expected migration conflict, report=%#v err=%v", report, err)
	}
	data, readErr := os.ReadFile(paths.MemoryDB)
	if readErr != nil || string(data) != "new" {
		t.Fatalf("destination was modified: %q %v", data, readErr)
	}
}

func TestMigrationImportsOnlyUserAddedResources(t *testing.T) {
	root := t.TempDir()
	packaged := filepath.Join(t.TempDir(), "resources")
	writeFixtureFile(t, filepath.Join(root, "SKILLS", "builtin", "SKILL.md"), "same")
	writeFixtureFile(t, filepath.Join(root, "SKILLS", "custom", "SKILL.md"), "custom")
	writeFixtureFile(t, filepath.Join(root, "TEAM", "modified", "team.yaml"), "user edit")
	writeFixtureFile(t, filepath.Join(packaged, "skills", "builtin", "SKILL.md"), "same")
	writeFixtureFile(t, filepath.Join(packaged, "teams", "modified", "team.yaml"), "packaged")
	paths := fixturePaths(t, root)

	report, err := Migrate(paths, MigrationOptions{Apply: true, SourceRoot: root, PackagedResources: packaged})
	if err != nil {
		t.Fatalf("apply: %v report=%#v", err, report)
	}
	if _, err := os.Stat(filepath.Join(paths.UserSkillsDir, "custom", "SKILL.md")); err != nil {
		t.Fatalf("user-added skill was not imported: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.UserSkillsDir, "builtin")); !os.IsNotExist(err) {
		t.Fatalf("packaged skill must not be imported: %v", err)
	}
	if _, err := os.Stat(filepath.Join(paths.UserTeamsDir, "modified")); !os.IsNotExist(err) {
		t.Fatalf("modified packaged team must await confirmation: %v", err)
	}
	if len(report.Warnings) == 0 {
		t.Fatal("modified packaged resource must be reported")
	}
}

func TestMigrationDoesNotRescanReadyV2Layout(t *testing.T) {
	root := t.TempDir()
	paths := fixturePaths(t, root)
	if err := WriteLayout(paths, "old"); err != nil {
		t.Fatal(err)
	}
	reportPath := filepath.Join(paths.MigrationsDir, "v2-report.json")
	writeFixtureFile(t, reportPath, `{"operations":[{"status":"applied"}]}`)
	userFile := filepath.Join(paths.PromptsDir, "user.txt")
	writeFixtureFile(t, userFile, "keep")
	report, err := Migrate(paths, MigrationOptions{Apply: true, SourceRoot: root, InstalledVersion: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Operations) != 0 {
		t.Fatalf("v2 upgrade planned legacy operations: %#v", report.Operations)
	}
	data, err := os.ReadFile(userFile)
	if err != nil || string(data) != "keep" {
		t.Fatalf("v2 user config was moved or modified: %q %v", data, err)
	}
	layout, err := ReadLayout(paths)
	if err != nil || layout.InstalledVersion != "new" {
		t.Fatalf("installed version was not updated: %#v %v", layout, err)
	}
	if data, err := os.ReadFile(reportPath); err != nil || string(data) != `{"operations":[{"status":"applied"}]}` {
		t.Fatalf("one-time migration report was overwritten: %q %v", data, err)
	}
}

func TestReadyV2TargetIgnoresSeparateLegacySource(t *testing.T) {
	target := t.TempDir()
	source := t.TempDir()
	paths := fixturePaths(t, target)
	if err := WriteLayout(paths, "old"); err != nil {
		t.Fatal(err)
	}
	legacySettings := filepath.Join(source, "CONFIG", "defaults.json")
	writeFixtureFile(t, legacySettings, `{"api_model":"must-not-import"}`)

	report, err := Migrate(paths, MigrationOptions{Apply: true, SourceRoot: source, InstalledVersion: "new"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Operations) != 0 {
		t.Fatalf("ready v2 target rescanned legacy source: %#v", report.Operations)
	}
	if _, err := os.Stat(legacySettings); err != nil {
		t.Fatalf("legacy source was modified: %v", err)
	}
	if _, err := os.Stat(paths.SettingsFile); !os.IsNotExist(err) {
		t.Fatalf("legacy settings were imported into ready v2 target: %v", err)
	}
}

func TestMigrationDoesNotCommitLayoutWhenPreCommitCheckFails(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "CONFIG", "defaults.json")
	writeFixtureFile(t, legacy, `{"api_model":"keep"}`)
	paths := fixturePaths(t, root)

	report, err := Migrate(paths, MigrationOptions{
		Apply: true, SourceRoot: root,
		BeforeLayoutCommit: func(AppPaths, *MigrationReport) error {
			return os.ErrInvalid
		},
	})
	if err == nil {
		t.Fatalf("expected pre-commit failure: %#v", report)
	}
	if _, statErr := os.Stat(paths.LayoutFile); !os.IsNotExist(statErr) {
		t.Fatalf("failed migration committed layout: %v", statErr)
	}
	data, readErr := os.ReadFile(legacy)
	if readErr != nil || string(data) != `{"api_model":"keep"}` {
		t.Fatalf("legacy source was not restored: %q %v", data, readErr)
	}
}

func TestMigrationDoesNotCommitLayoutWithInvalidProjectManifest(t *testing.T) {
	root := t.TempDir()
	legacy := filepath.Join(root, "CONFIG", "defaults.json")
	writeFixtureFile(t, legacy, `{"api_model":"keep"}`)
	paths := fixturePaths(t, root)
	writeFixtureFile(t, filepath.Join(paths.ProjectsDataDir, "001-test", "teams", "demo", "state.json"), `{}`)

	report, err := Migrate(paths, MigrationOptions{Apply: true, SourceRoot: root})
	if err == nil {
		t.Fatalf("expected project validation failure: %#v", report)
	}
	if len(report.Conflicts) != 1 || !strings.Contains(report.Conflicts[0], "missing or unreadable project.json") {
		t.Fatalf("project conflict was not reported: %#v", report.Conflicts)
	}
	if _, statErr := os.Stat(paths.LayoutFile); !os.IsNotExist(statErr) {
		t.Fatalf("failed migration committed layout: %v", statErr)
	}
	data, readErr := os.ReadFile(legacy)
	if readErr != nil || string(data) != `{"api_model":"keep"}` {
		t.Fatalf("legacy source was not restored: %q %v", data, readErr)
	}
}

func TestCanonicalNamePlanningSkipsDirectoryConsumedByMigration(t *testing.T) {
	root := t.TempDir()
	legacyConfig := filepath.Join(root, "CONFIG")
	writeFixtureFile(t, filepath.Join(legacyConfig, "defaults.json"), `{}`)
	paths := fixturePaths(t, root)
	report := MigrationReport{
		SourceRoot: root,
		Operations: []MigrationOperation{{
			Kind: "move", Source: legacyConfig,
			Destination: filepath.Join(paths.LegacyDataDir, "config-residual"),
		}},
	}

	planCanonicalTopLevelNames(paths, &report)
	if len(report.Operations) != 1 {
		t.Fatalf("consumed CONFIG directory also received a case rename: %#v", report.Operations)
	}
}

func TestCrossRootMigrationCopiesVerifiesAndBacksUpSource(t *testing.T) {
	source := t.TempDir()
	target := t.TempDir()
	writeFixtureFile(t, filepath.Join(source, "CONFIG", "defaults.json"), `{"api_model":"copied"}`)
	paths := fixturePaths(t, target)
	now := time.Date(2026, time.July, 18, 12, 30, 0, 0, time.UTC)

	report, err := Migrate(paths, MigrationOptions{Apply: true, SourceRoot: source, Now: now, InstalledVersion: "test"})
	if err != nil {
		t.Fatalf("cross-root migration failed: %v report=%#v", err, report)
	}
	wantBackup := source + ".legacy-v1-20260718-123000"
	if report.LegacyBackup != wantBackup {
		t.Fatalf("backup=%q want %q", report.LegacyBackup, wantBackup)
	}
	if _, statErr := os.Stat(source); !os.IsNotExist(statErr) {
		t.Fatalf("source should have been renamed: %v", statErr)
	}
	if data, readErr := os.ReadFile(filepath.Join(wantBackup, "CONFIG", "defaults.json")); readErr != nil || string(data) != `{"api_model":"copied"}` {
		t.Fatalf("legacy backup mismatch: %q %v", data, readErr)
	}
	if data, readErr := os.ReadFile(paths.SettingsFile); readErr != nil || !strings.Contains(string(data), `"api_model": "copied"`) {
		t.Fatalf("target settings mismatch: %q %v", data, readErr)
	}
	if err := CheckLayout(paths); err != nil {
		t.Fatal(err)
	}
}

func TestBindLegacyProjectPreflightsBeforeMovingTrust(t *testing.T) {
	paths := fixturePaths(t, t.TempDir())
	if err := WriteLayout(paths, "test"); err != nil {
		t.Fatal(err)
	}
	legacyTrust := filepath.Join(paths.LegacyDataDir, "projects", "repo", "CONFIG", "trusted_mcp.json")
	writeFixtureFile(t, legacyTrust, `{"servers":{"legacy":"hash"}}`)
	projectRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	project, err := paths.ForProject(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, project.MCPTrustFile, `{"servers":{"existing":"hash"}}`)

	if err := BindLegacyProject(paths, "repo", projectRoot); err == nil {
		t.Fatal("expected destination conflict")
	}
	if data, err := os.ReadFile(legacyTrust); err != nil || !strings.Contains(string(data), "legacy") {
		t.Fatalf("preflight failure moved legacy trust: %q %v", data, err)
	}
}

func TestBindLegacyProjectRequiresExistingRoot(t *testing.T) {
	paths := fixturePaths(t, t.TempDir())
	writeFixtureFile(t, filepath.Join(paths.LegacyDataDir, "projects", "repo", "teams", "state.json"), `{}`)
	if err := BindLegacyProject(paths, "repo", filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing project root should be rejected")
	}
}

func TestBindLegacyProjectHonorsMigrationLock(t *testing.T) {
	paths := fixturePaths(t, t.TempDir())
	writeFixtureFile(t, filepath.Join(paths.LegacyDataDir, "projects", "repo", "teams", "state.json"), `{}`)
	projectRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(paths.Root, ".layout-migration.lock"), "held")

	err := BindLegacyProject(paths, "repo", projectRoot)
	if err == nil || !strings.Contains(err.Error(), "acquire migration lock") {
		t.Fatalf("existing migration lock was ignored: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(paths.LegacyDataDir, "projects", "repo", "teams", "state.json")); statErr != nil {
		t.Fatalf("locked bind modified legacy data: %v", statErr)
	}
}

func TestMigrationPreservesLegacySymlinkWithoutFollowingIt(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.md")
	writeFixtureFile(t, outside, "outside")
	link := filepath.Join(root, "memory", "nested", "escape.md")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	paths := fixturePaths(t, root)
	report, err := Migrate(paths, MigrationOptions{SourceRoot: root})
	if err != nil || len(report.Conflicts) != 0 {
		t.Fatalf("legacy symlink object should be safe to archive: err=%v report=%#v", err, report)
	}
	if _, statErr := os.Lstat(link); statErr != nil {
		t.Fatalf("dry-run modified source link: %v", statErr)
	}
	if _, err := Migrate(paths, MigrationOptions{Apply: true, SourceRoot: root}); err != nil {
		t.Fatal(err)
	}
	archivedLink := filepath.Join(paths.LegacyDataDir, "memory", "nested", "escape.md")
	if target, err := os.Readlink(archivedLink); err != nil || target != outside {
		t.Fatalf("legacy link was followed instead of preserved: target=%q err=%v", target, err)
	}
	if data, err := os.ReadFile(outside); err != nil || string(data) != "outside" {
		t.Fatalf("external link target was modified: %q %v", data, err)
	}
}

func TestMigrationRejectsEscapingSymlinkInActiveProjectData(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	writeFixtureFile(t, outside, "outside")
	link := filepath.Join(root, "project", "repo", "teams", "escape")
	if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink creation is unavailable: %v", err)
	}
	paths := fixturePaths(t, root)
	report, err := Migrate(paths, MigrationOptions{SourceRoot: root, CurrentProjectRoot: projectRoot})
	if err == nil || len(report.Conflicts) == 0 || !strings.Contains(strings.Join(report.Conflicts, "\n"), "symlink escapes") {
		t.Fatalf("escaping active-data symlink was not rejected: err=%v report=%#v", err, report)
	}
}

func TestMigrationChecksDiskSpaceBeforeWriting(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "SYSTEM", "system-prompt.md"), "requires copy space")
	paths := fixturePaths(t, root)
	report, err := Migrate(paths, MigrationOptions{
		SourceRoot:         root,
		AvailableDiskBytes: func(string) (uint64, error) { return 0, nil },
	})
	if err == nil || report.RequiredCopyBytes == 0 || report.SpaceCheck != "insufficient" || !strings.Contains(err.Error(), "insufficient disk space") {
		t.Fatalf("disk exhaustion was not reported: err=%v report=%#v", err, report)
	}
	if _, statErr := os.Stat(paths.LayoutFile); !os.IsNotExist(statErr) {
		t.Fatalf("disk preflight wrote layout: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, "SYSTEM", "system-prompt.md")); statErr != nil {
		t.Fatalf("disk preflight moved source: %v", statErr)
	}
}

func TestMigrationApplyHonorsExistingLock(t *testing.T) {
	root := t.TempDir()
	settings := filepath.Join(root, "CONFIG", "defaults.json")
	writeFixtureFile(t, settings, `{"api_model":"keep"}`)
	paths := fixturePaths(t, root)
	writeFixtureFile(t, filepath.Join(root, ".layout-migration.lock"), "held")

	report, err := Migrate(paths, MigrationOptions{Apply: true, SourceRoot: root})
	if err == nil || !strings.Contains(err.Error(), "acquire migration lock") {
		t.Fatalf("existing migration lock was ignored: err=%v report=%#v", err, report)
	}
	if data, readErr := os.ReadFile(settings); readErr != nil || string(data) != `{"api_model":"keep"}` {
		t.Fatalf("locked migration modified source: %q %v", data, readErr)
	}
	if _, statErr := os.Stat(paths.LayoutFile); !os.IsNotExist(statErr) {
		t.Fatalf("locked migration wrote layout: %v", statErr)
	}
}

func TestMigrationRecoversOwnedPartialCopyStaging(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "SYSTEM", "system-prompt.md"), "complete")
	paths := fixturePaths(t, root)
	staging := filepath.Join(paths.LegacyDataDir, "resources", "SYSTEM") + ".layout-copy"
	writeFixtureFile(t, filepath.Join(staging, "partial.txt"), "partial")

	report, err := Migrate(paths, MigrationOptions{Apply: true, SourceRoot: root})
	if err != nil {
		t.Fatalf("migration did not recover partial staging: %v report=%#v", err, report)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("owned copy staging remains: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(paths.LegacyDataDir, "resources", "SYSTEM", "system-prompt.md"))
	if err != nil || string(data) != "complete" {
		t.Fatalf("verified copy was not committed: %q %v", data, err)
	}
}

func fixturePaths(t *testing.T, root string) AppPaths {
	t.Helper()
	paths, err := Resolve(ResolveOptions{GOOS: runtime.GOOS, HomeDir: t.TempDir(), Env: map[string]string{"LUMINA_APP_ROOT": root}})
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func writeFixtureFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
