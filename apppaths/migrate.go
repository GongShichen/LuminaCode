package apppaths

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"time"
)

type MigrationOptions struct {
	Apply              bool
	SourceRoot         string
	CurrentProjectRoot string
	InstalledVersion   string
	PackagedResources  string
	Now                time.Time
	BeforeLayoutCommit func(AppPaths, *MigrationReport) error
	AvailableDiskBytes func(string) (uint64, error)
}

type MigrationOperation struct {
	Kind               string `json:"kind"`
	Source             string `json:"source"`
	Destination        string `json:"destination"`
	Reason             string `json:"reason"`
	Bytes              int64  `json:"bytes"`
	Status             string `json:"status"`
	Error              string `json:"error,omitempty"`
	allowExternalLinks bool
}

type MigrationReport struct {
	LayoutVersion      int                  `json:"layout_version"`
	SourceRoot         string               `json:"source_root"`
	TargetRoot         string               `json:"target_root"`
	DryRun             bool                 `json:"dry_run"`
	RequiredCopyBytes  int64                `json:"required_copy_bytes"`
	AvailableBytes     uint64               `json:"available_bytes,omitempty"`
	SpaceCheck         string               `json:"space_check,omitempty"`
	LegacyBackup       string               `json:"legacy_backup,omitempty"`
	Operations         []MigrationOperation `json:"operations"`
	Conflicts          []string             `json:"conflicts,omitempty"`
	UnresolvedProjects []string             `json:"unresolved_projects,omitempty"`
	Unexplained        []string             `json:"unexplained,omitempty"`
	Warnings           []string             `json:"warnings,omitempty"`
	StartedAt          string               `json:"started_at"`
	FinishedAt         string               `json:"finished_at,omitempty"`
}

type completedOperation struct {
	operation MigrationOperation
	copied    bool
}

type migrationFileSnapshot struct {
	path   string
	data   []byte
	mode   os.FileMode
	exists bool
}

func Migrate(paths AppPaths, opts MigrationOptions) (MigrationReport, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	sourceRoot := filepath.Clean(strings.TrimSpace(opts.SourceRoot))
	if sourceRoot == "." || sourceRoot == "" {
		sourceRoot = paths.Root
	}
	if !filepath.IsAbs(sourceRoot) {
		return MigrationReport{}, fmt.Errorf("migration source root must be absolute: %s", sourceRoot)
	}
	if err := validateMigrationRoot(sourceRoot); err != nil {
		return MigrationReport{}, err
	}
	if !samePath(sourceRoot, paths.Root) && (insidePath(paths.Root, sourceRoot) || insidePath(sourceRoot, paths.Root)) {
		return MigrationReport{}, fmt.Errorf("migration source and target roots must not be nested: %s -> %s", sourceRoot, paths.Root)
	}
	report := MigrationReport{
		LayoutVersion: LayoutVersion, SourceRoot: sourceRoot, TargetRoot: paths.Root, DryRun: !opts.Apply,
		Operations: []MigrationOperation{}, Unexplained: []string{}, StartedAt: now.UTC().Format(time.RFC3339Nano),
	}
	if opts.Apply {
		release, err := acquireMigrationLock(paths)
		if err != nil {
			return report, err
		}
		defer release()
	}
	if existing, err := ReadLayout(paths); err == nil {
		if existing.LayoutVersion != LayoutVersion {
			return report, fmt.Errorf("unsupported LuminaCode layout version %d", existing.LayoutVersion)
		}
		if !opts.Apply {
			report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
			return report, nil
		}
		if err := EnsureBaseDirs(paths); err != nil {
			return report, err
		}
		report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		installedVersion := strings.TrimSpace(opts.InstalledVersion)
		if installedVersion == "" {
			installedVersion = existing.InstalledVersion
		}
		if err := WriteLayout(paths, installedVersion); err != nil {
			return report, err
		}
		return report, nil
	}
	buildMigrationOperations(paths, opts, &report)
	validateMigrationOperations(&report)
	insufficientSpace := false
	if report.RequiredCopyBytes > 0 {
		diskAvailable := opts.AvailableDiskBytes
		if diskAvailable == nil {
			diskAvailable = availableDiskBytes
		}
		available, spaceErr := diskAvailable(paths.Root)
		if spaceErr != nil {
			report.SpaceCheck = "unavailable"
			report.Warnings = append(report.Warnings, "disk space check failed: "+spaceErr.Error())
		} else {
			report.AvailableBytes = available
			report.SpaceCheck = "sufficient"
			if uint64(report.RequiredCopyBytes) > available {
				report.SpaceCheck = "insufficient"
				insufficientSpace = true
			}
		}
	}
	if len(report.Conflicts) > 0 {
		report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return report, fmt.Errorf("layout migration has %d conflict(s)", len(report.Conflicts))
	}
	if insufficientSpace {
		report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return report, fmt.Errorf("insufficient disk space: migration needs %d bytes, %d bytes available", report.RequiredCopyBytes, report.AvailableBytes)
	}
	if !opts.Apply {
		report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		return report, nil
	}
	if err := EnsureBaseDirs(paths); err != nil {
		return report, err
	}

	preserveSource := !samePath(sourceRoot, paths.Root)
	backupPath := ""
	if preserveSource {
		backupPath = sourceRoot + ".legacy-" + now.UTC().Format("20060102-150405")
		if _, statErr := os.Stat(backupPath); statErr == nil {
			return report, fmt.Errorf("legacy backup already exists: %s", backupPath)
		} else if !os.IsNotExist(statErr) {
			return report, fmt.Errorf("inspect legacy backup target: %w", statErr)
		}
	}
	var completed []completedOperation
	for i := range report.Operations {
		op := &report.Operations[i]
		if op.Status == "identical" || op.Status == "missing" {
			continue
		}
		copied := op.Kind == "copy" || preserveSource
		if err := applyMigrationOperation(*op, copied); err != nil {
			op.Status = "failed"
			op.Error = err.Error()
			report.Conflicts = append(report.Conflicts, fmt.Sprintf("%s: %v", op.Source, err))
			rollbackMigration(completed)
			return report, err
		}
		op.Status = "applied"
		completed = append(completed, completedOperation{operation: *op, copied: copied})
	}
	if opts.BeforeLayoutCommit != nil {
		if err := opts.BeforeLayoutCommit(paths, &report); err != nil {
			rollbackMigration(completed)
			return report, fmt.Errorf("pre-commit migration check: %w", err)
		}
	}
	var snapshots []migrationFileSnapshot
	for _, project := range boundMigrationProjects(paths, opts, report) {
		manifestSnapshots, err := snapshotMigrationFiles(project.ManifestFile)
		if err != nil {
			restoreMigrationFiles(snapshots)
			rollbackMigration(completed)
			return report, err
		}
		snapshots = append(snapshots, manifestSnapshots...)
		if err := EnsureProjectManifest(project, now); err != nil {
			restoreMigrationFiles(snapshots)
			rollbackMigration(completed)
			return report, err
		}
	}
	configSnapshots, err := snapshotMigrationFiles(paths.SettingsFile, paths.ManagedMCPFile, paths.MCPConfigFile)
	if err != nil {
		restoreMigrationFiles(snapshots)
		rollbackMigration(completed)
		return report, err
	}
	snapshots = append(snapshots, configSnapshots...)
	if err := rewriteMigratedSettings(paths.SettingsFile); err != nil {
		restoreMigrationFiles(snapshots)
		rollbackMigration(completed)
		return report, err
	}
	if err := rewriteManagedMCP(paths, sourceRoot); err != nil {
		restoreMigrationFiles(snapshots)
		rollbackMigration(completed)
		return report, err
	}
	if projectConflicts := inspectProjectManifests(paths.ProjectsDataDir, paths.Platform); len(projectConflicts) > 0 {
		report.Conflicts = append(report.Conflicts, projectConflicts...)
		report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
		restoreMigrationFiles(snapshots)
		rollbackMigration(completed)
		return report, fmt.Errorf("post-migration project validation failed with %d conflict(s)", len(projectConflicts))
	}
	if preserveSource {
		if err := os.Rename(sourceRoot, backupPath); err != nil {
			restoreMigrationFiles(snapshots)
			rollbackMigration(completed)
			return report, fmt.Errorf("preserve legacy source: %w", err)
		}
		report.LegacyBackup = backupPath
	}
	report.FinishedAt = time.Now().UTC().Format(time.RFC3339Nano)
	reportPath := filepath.Join(paths.MigrationsDir, "migration-report.json")
	if err := writeJSONAtomic(reportPath, report, 0o600); err != nil {
		restoreMigrationSource(sourceRoot, backupPath)
		restoreMigrationFiles(snapshots)
		rollbackMigration(completed)
		return report, err
	}
	if err := WriteLayout(paths, opts.InstalledVersion); err != nil {
		_ = os.Remove(reportPath)
		restoreMigrationSource(sourceRoot, backupPath)
		restoreMigrationFiles(snapshots)
		rollbackMigration(completed)
		return report, err
	}
	retiredMemoryRoot := filepath.Join(sourceRoot, "memory")
	if preserveSource {
		retiredMemoryRoot = filepath.Join(backupPath, "memory")
	}
	if err := os.RemoveAll(retiredMemoryRoot); err != nil {
		report.Warnings = append(report.Warnings, "could not remove retired memory data: "+err.Error())
	}
	return report, nil
}

func boundMigrationProjects(paths AppPaths, opts MigrationOptions, report MigrationReport) []ProjectPaths {
	root := strings.TrimSpace(opts.CurrentProjectRoot)
	if root == "" {
		root, _ = os.Getwd()
	}
	project, err := paths.ForProject(root)
	if err != nil {
		return nil
	}
	for _, operation := range report.Operations {
		if operation.Status == "missing" || operation.Status == "failed" || operation.Status == "conflict" {
			continue
		}
		if samePath(operation.Destination, project.DataDir) || insidePath(operation.Destination, project.DataDir) ||
			samePath(operation.Destination, project.StateDir) || insidePath(operation.Destination, project.StateDir) {
			return []ProjectPaths{project}
		}
	}
	return nil
}

func snapshotMigrationFiles(paths ...string) ([]migrationFileSnapshot, error) {
	snapshots := make([]migrationFileSnapshot, 0, len(paths))
	for _, path := range paths {
		snapshot := migrationFileSnapshot{path: path}
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			snapshots = append(snapshots, snapshot)
			continue
		}
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		snapshot.data = data
		snapshot.mode = info.Mode().Perm()
		snapshot.exists = true
		snapshots = append(snapshots, snapshot)
	}
	return snapshots, nil
}

func restoreMigrationFiles(snapshots []migrationFileSnapshot) {
	for _, snapshot := range snapshots {
		if !snapshot.exists {
			_ = os.Remove(snapshot.path)
			continue
		}
		_ = WriteFileAtomic(snapshot.path, snapshot.data, snapshot.mode)
	}
}

func restoreMigrationSource(sourceRoot, backupPath string) {
	if backupPath == "" {
		return
	}
	if _, err := os.Stat(sourceRoot); os.IsNotExist(err) {
		_ = os.Rename(backupPath, sourceRoot)
	}
}

func validateMigrationRoot(root string) error {
	if strings.ContainsAny(root, "*?[]{}") {
		return fmt.Errorf("migration source contains an unresolved wildcard: %s", root)
	}
	clean := filepath.Clean(root)
	volume := filepath.VolumeName(clean)
	if clean == string(filepath.Separator) || (volume != "" && clean == volume+string(filepath.Separator)) {
		return fmt.Errorf("refusing unsafe migration source root: %s", root)
	}
	info, err := os.Lstat(clean)
	if err != nil {
		return fmt.Errorf("inspect migration source: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("migration source root must not be a symlink or junction: %s", root)
	}
	if !info.IsDir() {
		return fmt.Errorf("migration source root is not a directory: %s", root)
	}
	return nil
}

func buildMigrationOperations(paths AppPaths, opts MigrationOptions, report *MigrationReport) {
	source := report.SourceRoot
	add := func(kind, from, to, reason string) {
		path := filepath.Join(source, filepath.FromSlash(from))
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return
		}
		op := MigrationOperation{Kind: kind, Source: path, Destination: to, Reason: reason, Status: "planned"}
		op.allowExternalLinks = samePath(to, paths.AppDir) || insidePath(to, paths.AppDir) ||
			samePath(to, paths.LegacyDataDir) || insidePath(to, paths.LegacyDataDir)
		if err != nil {
			op.Status = "failed"
			op.Error = err.Error()
			report.Conflicts = append(report.Conflicts, path+": "+err.Error())
		} else if info.Mode()&os.ModeSymlink != 0 {
			op.Status = "failed"
			op.Error = "top-level symlinks are not migrated"
			report.Conflicts = append(report.Conflicts, path+": top-level symlinks are not migrated")
		} else if err := validateMigrationTree(path, op.allowExternalLinks); err != nil {
			op.Status = "failed"
			op.Error = err.Error()
			report.Conflicts = append(report.Conflicts, path+": "+err.Error())
		} else {
			op.Bytes, err = pathSize(path)
			if err != nil {
				op.Status = "failed"
				op.Error = err.Error()
				report.Conflicts = append(report.Conflicts, path+": "+err.Error())
			} else if kind == "copy" || !samePath(source, paths.Root) {
				report.RequiredCopyBytes += op.Bytes
			}
		}
		report.Operations = append(report.Operations, op)
	}

	resources := []struct{ old, target, legacy string }{
		{"SYSTEM", paths.SystemResourceDir, filepath.Join(paths.LegacyDataDir, "resources", "SYSTEM")},
		{"SKILLS", paths.BundledSkillsDir, filepath.Join(paths.LegacyDataDir, "resources", "SKILLS")},
		{"TEAM", paths.BundledTeamsDir, filepath.Join(paths.LegacyDataDir, "resources", "TEAM")},
	}
	for _, resource := range resources {
		add("copy", resource.old, resource.legacy, "preserve installed resources")
	}
	planUserResourceImports(paths, opts, report, add)
	for _, resource := range resources {
		add("move", resource.old, resource.target, "install resources into app layer")
	}
	add("move", "frontend", paths.FrontendDir, "move frontend payload into app layer")
	add("move", "setup-searxng.sh", filepath.Join(paths.ScriptsDir, "setup-searxng.sh"), "move managed script into app layer")
	add("move", "mcp/arxiv-mcp", paths.ArxivMCPDir, "move managed MCP extension into app layer")
	add("move", "models", paths.ModelsDir, "move regenerable models into cache layer")
	add("move", "sessions", paths.ActiveSessionsDir, "move active sessions")
	add("move", "session-archive", paths.ArchivedSessionsDir, "move archived sessions")
	add("move", "searxng", paths.SearxNGDir, "move managed service state")
	add("move", "run", paths.RunDir, "move daemon runtime state")
	add("move", "CONFIG/defaults.json", paths.SettingsFile, "move user settings")
	add("move", "CONFIG/mcp.json", paths.MCPConfigFile, "move user MCP configuration")
	add("move", "CONFIG/managed-mcp.json", paths.ManagedMCPFile, "move managed MCP ownership ledger")
	add("move", "CONFIG/defaults.json.example", filepath.Join(paths.DefaultsResourceDir, "settings.example.json"), "move settings template")

	projectRoot := strings.TrimSpace(opts.CurrentProjectRoot)
	if projectRoot == "" {
		projectRoot, _ = os.Getwd()
	}
	project, projectErr := paths.ForProject(projectRoot)
	legacyProjectsRoot := filepath.Join(source, "project")
	entries, _ := os.ReadDir(legacyProjectsRoot)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		legacyName := entry.Name()
		if projectErr == nil && legacyName == slugify(filepath.Base(project.CanonicalRoot), 0) {
			base := filepath.ToSlash(filepath.Join("project", legacyName))
			add("move", base+"/CONFIG/trusted_mcp.json", project.MCPTrustFile, "bind unambiguous project MCP trust")
			add("move", base+"/teams", project.TeamsDir, "move durable project team data")
			add("move", base+"/background/tool-results", project.ToolResultsForSession("_legacy"), "move legacy tool results without activating cleanup")
			add("move", base, filepath.Join(paths.LegacyDataDir, "projects", legacyName, "residual"), "preserve remaining legacy project files")
			continue
		}
		report.UnresolvedProjects = append(report.UnresolvedProjects, legacyName)
		add("move", filepath.ToSlash(filepath.Join("project", legacyName)), filepath.Join(paths.LegacyDataDir, "projects", legacyName), "quarantine unresolved project data and trust")
	}

	legacyConfigDir := filepath.Join(source, "CONFIG")
	configAliasesTarget := sameExistingObject(legacyConfigDir, paths.ConfigDir)
	known := map[string]struct{}{"defaults.json": {}, "mcp.json": {}, "managed-mcp.json": {}, "defaults.json.example": {}}
	entries, _ = os.ReadDir(legacyConfigDir)
	for _, entry := range entries {
		if _, ok := known[entry.Name()]; ok {
			continue
		}
		add("move", filepath.ToSlash(filepath.Join("CONFIG", entry.Name())), filepath.Join(paths.LegacyDataDir, "config", entry.Name()), "preserve unclassified legacy configuration")
	}
	if !configAliasesTarget {
		add("move", "CONFIG", filepath.Join(paths.LegacyDataDir, "config-residual"), "preserve emptied legacy configuration directory")
	}
	add("move", "mcp", filepath.Join(paths.LegacyDataDir, "mcp"), "preserve unclassified managed MCP files")
	add("move", "project", filepath.Join(paths.LegacyDataDir, "project-runtime"), "preserve unclassified project runtime files")
	planCanonicalTopLevelNames(paths, report)

	report.Unexplained = unexplainedTopLevel(source)
	sort.Strings(report.UnresolvedProjects)
	sort.Strings(report.Unexplained)
}

func planCanonicalTopLevelNames(paths AppPaths, report *MigrationReport) {
	if !samePath(report.SourceRoot, paths.Root) {
		return
	}
	entries, err := os.ReadDir(paths.Root)
	if err != nil {
		return
	}
	canonicalNames := []string{"app", "config", "data", "state", "cache"}
	for _, entry := range entries {
		for _, canonical := range canonicalNames {
			if entry.Name() == canonical || !strings.EqualFold(entry.Name(), canonical) {
				continue
			}
			source := filepath.Join(paths.Root, entry.Name())
			if migrationConsumesSource(report.Operations, source) {
				continue
			}
			report.Operations = append(report.Operations, MigrationOperation{
				Kind: "rename-case", Source: source, Destination: filepath.Join(paths.Root, canonical),
				Reason: "normalize AppRoot top-level directory casing", Status: "planned",
			})
		}
	}
}

func migrationConsumesSource(operations []MigrationOperation, source string) bool {
	for _, operation := range operations {
		if operation.Kind == "move" && samePath(operation.Source, source) {
			return true
		}
	}
	return false
}

func planUserResourceImports(paths AppPaths, opts MigrationOptions, report *MigrationReport, add func(string, string, string, string)) {
	packaged := strings.TrimSpace(opts.PackagedResources)
	if packaged == "" {
		return
	}
	for _, resource := range []struct {
		legacyDir string
		packaged  string
		userDir   string
		kind      string
	}{
		{legacyDir: "SKILLS", packaged: "skills", userDir: paths.UserSkillsDir, kind: "skill"},
		{legacyDir: "TEAM", packaged: "teams", userDir: paths.UserTeamsDir, kind: "team"},
	} {
		legacyRoot := filepath.Join(report.SourceRoot, resource.legacyDir)
		entries, err := os.ReadDir(legacyRoot)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			legacyPath := filepath.Join(legacyRoot, entry.Name())
			packagedPath := filepath.Join(packaged, resource.packaged, entry.Name())
			if _, err := os.Lstat(packagedPath); os.IsNotExist(err) {
				from := filepath.ToSlash(filepath.Join(resource.legacyDir, entry.Name()))
				add("copy", from, filepath.Join(resource.userDir, entry.Name()), "import user-added "+resource.kind)
				continue
			}
			identical, err := pathsIdentical(legacyPath, packagedPath)
			if err != nil {
				report.Warnings = append(report.Warnings, fmt.Sprintf("could not compare legacy %s %s: %v", resource.kind, entry.Name(), err))
				continue
			}
			if !identical {
				report.Warnings = append(report.Warnings, fmt.Sprintf("legacy %s %q differs from the packaged resource; preserved under data/legacy/layout/resources for manual confirmation", resource.kind, entry.Name()))
			}
		}
	}
}

func validateMigrationOperations(report *MigrationReport) {
	for i := range report.Operations {
		op := &report.Operations[i]
		if op.Status == "failed" || op.Status == "missing" {
			continue
		}
		if op.Kind == "rename-case" {
			if sameExistingObject(op.Source, op.Destination) {
				continue
			}
			if _, err := os.Lstat(op.Destination); err == nil {
				op.Status = "conflict"
				report.Conflicts = append(report.Conflicts, fmt.Sprintf("case-normalized destination exists with different content: %s", op.Destination))
			} else if !os.IsNotExist(err) {
				op.Status = "failed"
				report.Conflicts = append(report.Conflicts, op.Destination+": "+err.Error())
			}
			continue
		}
		if insidePath(op.Destination, op.Source) {
			op.Status = "failed"
			report.Conflicts = append(report.Conflicts, "destination is inside source: "+op.Destination)
			continue
		}
		if _, err := os.Lstat(op.Destination); os.IsNotExist(err) {
			continue
		} else if err != nil {
			report.Conflicts = append(report.Conflicts, op.Destination+": "+err.Error())
			continue
		}
		identical, err := pathsIdentical(op.Source, op.Destination)
		if err == nil && identical {
			op.Status = "identical"
			continue
		}
		if empty, _ := isEmptyDir(op.Destination); empty {
			continue
		}
		op.Status = "conflict"
		report.Conflicts = append(report.Conflicts, fmt.Sprintf("destination exists with different content: %s", op.Destination))
	}
}

func applyMigrationOperation(op MigrationOperation, copyOnly bool) error {
	if op.Kind == "rename-case" {
		return renamePathCase(op.Source, op.Destination)
	}
	if err := validateMigrationTree(op.Source, op.allowExternalLinks); err != nil {
		return err
	}
	if empty, _ := isEmptyDir(op.Destination); empty {
		if err := os.Remove(op.Destination); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(filepath.Dir(op.Destination), 0o700); err != nil {
		return err
	}
	if copyOnly {
		return copyPathVerified(op.Source, op.Destination)
	}
	if err := os.Rename(op.Source, op.Destination); err == nil {
		return nil
	}
	if err := copyPathVerified(op.Source, op.Destination); err != nil {
		return err
	}
	return os.RemoveAll(op.Source)
}

func rollbackMigration(completed []completedOperation) {
	for i := len(completed) - 1; i >= 0; i-- {
		item := completed[i]
		if item.operation.Kind == "rename-case" {
			_ = renamePathCase(item.operation.Destination, item.operation.Source)
			continue
		}
		if item.copied {
			_ = os.RemoveAll(item.operation.Destination)
			continue
		}
		_ = os.MkdirAll(filepath.Dir(item.operation.Source), 0o700)
		if err := os.Rename(item.operation.Destination, item.operation.Source); err != nil {
			if copyPathVerified(item.operation.Destination, item.operation.Source) == nil {
				_ = os.RemoveAll(item.operation.Destination)
			}
		}
	}
}

func renamePathCase(source, destination string) error {
	if filepath.Clean(source) == filepath.Clean(destination) && source == destination {
		return nil
	}
	if !sameExistingObject(source, destination) {
		return os.Rename(source, destination)
	}
	temporary := filepath.Join(filepath.Dir(source), ".layout-case-"+filepath.Base(destination))
	if _, err := os.Lstat(temporary); err == nil {
		return fmt.Errorf("case-normalization temporary path exists: %s", temporary)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(source, temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, destination); err != nil {
		_ = os.Rename(temporary, source)
		return err
	}
	return nil
}

func copyPathVerified(source, destination string) error {
	staging := destination + ".layout-copy"
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := copyPath(source, staging); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	identical, err := pathsIdentical(source, staging)
	if err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	if !identical {
		_ = os.RemoveAll(staging)
		return fmt.Errorf("copy verification failed: %s", source)
	}
	if err := os.Rename(staging, destination); err != nil {
		_ = os.RemoveAll(staging)
		return err
	}
	return nil
}

func copyPath(source, destination string) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(source)
		if err != nil {
			return err
		}
		return os.Symlink(target, destination)
	}
	if !info.IsDir() {
		in, err := os.Open(source)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		if err := out.Sync(); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
	if err := os.Mkdir(destination, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyPath(filepath.Join(source, entry.Name()), filepath.Join(destination, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func pathsIdentical(a, b string) (bool, error) {
	ha, err := pathDigest(a)
	if err != nil {
		return false, err
	}
	hb, err := pathDigest(b)
	return ha == hb, err
}

func pathDigest(root string) (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, err := os.Readlink(path)
			if err != nil {
				return err
			}
			_, _ = io.WriteString(h, "symlink\x00"+target)
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		_, _ = io.WriteString(h, filepath.ToSlash(rel)+"\x00")
		if entry.IsDir() {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(h, file)
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		return closeErr
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func pathSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			total += int64(len(target))
			return nil
		}
		if !entry.IsDir() {
			info, infoErr := entry.Info()
			if infoErr != nil {
				return infoErr
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func validateMigrationTree(root string, allowExternalLinks bool) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			target, readErr := os.Readlink(path)
			if readErr != nil {
				return readErr
			}
			if allowExternalLinks {
				return nil
			}
			resolved := target
			if !filepath.IsAbs(resolved) {
				resolved = filepath.Join(filepath.Dir(path), resolved)
			}
			resolved = filepath.Clean(resolved)
			if evaluated, evalErr := filepath.EvalSymlinks(resolved); evalErr == nil {
				resolved = evaluated
			}
			if !samePath(resolved, rootAbs) && !insidePath(resolved, rootAbs) {
				return fmt.Errorf("symlink escapes migration source: %s -> %s", path, target)
			}
		}
		return nil
	})
}

func isEmptyDir(path string) (bool, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return false, err
	}
	return len(entries) == 0, nil
}

func samePath(a, b string) bool {
	aa, _ := filepath.Abs(a)
	bb, _ := filepath.Abs(b)
	aa = filepath.Clean(aa)
	bb = filepath.Clean(bb)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(aa, bb)
	}
	return aa == bb
}

func sameExistingObject(a, b string) bool {
	aInfo, aErr := os.Stat(a)
	bInfo, bErr := os.Stat(b)
	return aErr == nil && bErr == nil && os.SameFile(aInfo, bInfo)
}

func insidePath(path, parent string) bool {
	rel, err := filepath.Rel(parent, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func unexplainedTopLevel(root string) []string {
	known := map[string]struct{}{
		"CONFIG": {}, "SYSTEM": {}, "SKILLS": {}, "TEAM": {}, "frontend": {}, "setup-searxng.sh": {},
		"mcp": {}, "models": {}, "memory": {}, "project": {}, "sessions": {}, "session-archive": {}, "searxng": {}, "run": {},
		"layout.json": {}, "app": {}, "config": {}, "data": {}, "state": {}, "cache": {}, ".layout-migration.lock": {},
		"app.new": {}, "app.old": {},
	}
	entries, _ := os.ReadDir(root)
	var out []string
	for _, entry := range entries {
		if _, ok := known[entry.Name()]; !ok {
			out = append(out, entry.Name())
		}
	}
	return out
}

func rewriteMigratedSettings(path string) error {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		return fmt.Errorf("parse migrated settings: %w", err)
	}
	for key := range legacyDefaultSettings {
		value, ok := settings[key].(string)
		if !ok {
			continue
		}
		if IsLegacyDefaultSetting(key, value) {
			delete(settings, key)
			continue
		}
		settings[key] = normalizeMigratedPath(value)
	}
	return writeJSONAtomic(path, settings, 0o600)
}

func normalizeMigratedPath(value string) string {
	value = strings.TrimSpace(value)
	if value == "~" || strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			value = expandHome(value, home, runtime.GOOS)
		}
	}
	if value == "" {
		return value
	}
	return filepath.Clean(value)
}

func rewriteManagedMCP(paths AppPaths, legacyRoot string) error {
	managedData, err := os.ReadFile(paths.ManagedMCPFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	configData, err := os.ReadFile(paths.MCPConfigFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var managed, config map[string]any
	if json.Unmarshal(managedData, &managed) != nil || json.Unmarshal(configData, &config) != nil {
		return nil
	}
	managedServers, _ := managed["mcpServers"].(map[string]any)
	configServers, _ := config["mcpServers"].(map[string]any)
	oldPath := filepath.Join(legacyRoot, "mcp", "arxiv-mcp")
	newPath := paths.ArxivMCPDir
	changed := false
	for name, managedServer := range managedServers {
		current, ok := configServers[name]
		if !ok || !reflect.DeepEqual(current, managedServer) {
			continue
		}
		rewritten := replaceStrings(managedServer, oldPath, newPath)
		managedServers[name] = rewritten
		configServers[name] = rewritten
		changed = true
	}
	if !changed {
		return nil
	}
	if err := writeJSONAtomic(paths.ManagedMCPFile, managed, 0o600); err != nil {
		return err
	}
	return writeJSONAtomic(paths.MCPConfigFile, config, 0o600)
}

func replaceStrings(value any, oldValue, newValue string) any {
	switch typed := value.(type) {
	case string:
		return strings.ReplaceAll(typed, oldValue, newValue)
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = replaceStrings(typed[i], oldValue, newValue)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			out[key] = replaceStrings(item, oldValue, newValue)
		}
		return out
	default:
		return value
	}
}

func BindLegacyProject(paths AppPaths, legacyName, projectRoot string) error {
	if legacyName == "" || filepath.Base(legacyName) != legacyName || strings.ContainsAny(legacyName, `/\\`) {
		return errors.New("legacy project name must be a single path component")
	}
	release, err := acquireMigrationLock(paths)
	if err != nil {
		return err
	}
	defer release()

	legacy := filepath.Join(paths.LegacyDataDir, "projects", legacyName)
	if info, err := os.Stat(legacy); err != nil || !info.IsDir() {
		return fmt.Errorf("legacy project not found: %s", legacy)
	}
	rootInfo, err := os.Stat(projectRoot)
	if err != nil {
		return fmt.Errorf("inspect project root: %w", err)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("project root is not a directory: %s", projectRoot)
	}
	project, err := paths.ForProject(projectRoot)
	if err != nil {
		return err
	}
	var operations []MigrationOperation
	destinations := map[string]string{}
	for _, move := range []struct{ from, to string }{
		{filepath.Join(legacy, "CONFIG", "trusted_mcp.json"), project.MCPTrustFile},
		{filepath.Join(legacy, "trust", "mcp.json"), project.MCPTrustFile},
		{filepath.Join(legacy, "teams"), project.TeamsDir},
		{filepath.Join(legacy, "background", "tool-results"), project.ToolResultsForSession("_legacy")},
	} {
		if _, err := os.Lstat(move.from); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		key := filepath.Clean(move.to)
		if prior, duplicate := destinations[key]; duplicate {
			return fmt.Errorf("legacy project contains multiple sources for %s: %s and %s", move.to, prior, move.from)
		}
		destinations[key] = move.from
		if _, err := os.Lstat(move.to); err == nil {
			return fmt.Errorf("bind destination already exists: %s", move.to)
		} else if !os.IsNotExist(err) {
			return err
		}
		if err := validateMigrationTree(move.from, false); err != nil {
			return err
		}
		operations = append(operations, MigrationOperation{Source: move.from, Destination: move.to})
	}
	manifestSnapshot, err := snapshotMigrationFiles(project.ManifestFile)
	if err != nil {
		return err
	}
	if err := EnsureProjectManifest(project, time.Now()); err != nil {
		return err
	}
	var completed []completedOperation
	for _, operation := range operations {
		if err := applyMigrationOperation(operation, false); err != nil {
			rollbackMigration(completed)
			restoreMigrationFiles(manifestSnapshot)
			return err
		}
		completed = append(completed, completedOperation{operation: operation})
	}
	return nil
}

func acquireMigrationLock(paths AppPaths) (func(), error) {
	if err := os.MkdirAll(paths.Root, 0o700); err != nil {
		return nil, err
	}
	_ = chmodPath(paths.Root, 0o700)
	lockPath := filepath.Join(paths.Root, ".layout-migration.lock")
	lock, err := os.OpenFile(lockPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	if err := lock.Close(); err != nil {
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("close migration lock: %w", err)
	}
	return func() { _ = os.Remove(lockPath) }, nil
}
