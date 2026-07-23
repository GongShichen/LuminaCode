package apppaths

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

var (
	ErrMigrationRequired    = errors.New("legacy LuminaCode AppRoot requires layout migration")
	ErrLayoutNotInitialized = errors.New("LuminaCode AppRoot layout is not initialized; run 'lumina-backend layout migrate --dry-run' then '--apply'")
)

type Layout struct {
	LayoutVersion    int    `json:"layout_version"`
	InstalledVersion string `json:"installed_version"`
	Platform         string `json:"platform"`
	UpdatedAt        string `json:"updated_at"`
}

type ProjectManifest struct {
	ID            string `json:"id"`
	CanonicalRoot string `json:"canonical_root"`
	DisplayName   string `json:"display_name"`
	FirstSeenAt   string `json:"first_seen_at"`
	LastSeenAt    string `json:"last_seen_at"`
}

func ReadLayout(paths AppPaths) (Layout, error) {
	data, err := os.ReadFile(paths.LayoutFile)
	if err != nil {
		return Layout{}, err
	}
	var layout Layout
	if err := json.Unmarshal(data, &layout); err != nil {
		return Layout{}, fmt.Errorf("parse %s: %w", paths.LayoutFile, err)
	}
	return layout, nil
}

func WriteLayout(paths AppPaths, installedVersion string) error {
	if err := EnsureBaseDirs(paths); err != nil {
		return err
	}
	if err := EnsurePrivatePermissions(paths); err != nil {
		return err
	}
	platform := paths.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	layout := Layout{
		LayoutVersion: LayoutVersion, InstalledVersion: strings.TrimSpace(installedVersion),
		Platform: platform, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	return writeJSONAtomic(paths.LayoutFile, layout, 0o600)
}

func CheckLayout(paths AppPaths) error {
	layout, err := ReadLayout(paths)
	if err != nil {
		if os.IsNotExist(err) {
			if HasLegacyLayout(paths.Root) {
				return ErrMigrationRequired
			}
			return ErrLayoutNotInitialized
		}
		return err
	}
	if layout.LayoutVersion != LayoutVersion {
		return fmt.Errorf("unsupported LuminaCode layout version %d; expected %d", layout.LayoutVersion, LayoutVersion)
	}
	return nil
}

func PrepareRuntime(paths AppPaths, _ string) error {
	if err := CheckLayout(paths); err != nil {
		return err
	}
	if err := EnsureBaseDirs(paths); err != nil {
		return err
	}
	return EnsurePrivatePermissions(paths)
}

func HasLegacyLayout(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	actual := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		actual[entry.Name()] = struct{}{}
	}
	for _, name := range []string{"CONFIG", "SYSTEM", "SKILLS", "TEAM", "frontend", "mcp", "memory", "models", "project", "sessions", "session-archive", "searxng", "run"} {
		if _, exists := actual[name]; exists {
			return true
		}
	}
	return false
}

func EnsureBaseDirs(paths AppPaths) error {
	for _, dir := range []string{
		paths.Root, paths.ConfigDir, paths.InstructionsDir, paths.PromptsDir, paths.UserSkillsDir, paths.UserTeamsDir,
		paths.DataDir, paths.MemoryDir, paths.ActiveSessionsDir, paths.ArchivedSessionsDir, paths.ProjectsDataDir,
		paths.StateDir, paths.RunDir, paths.LogsDir, paths.ManagedDir, paths.ServicesDir, paths.MigrationsDir, paths.ProjectsStateDir,
		paths.CacheDir, paths.ModelsDir, paths.DownloadsDir, paths.TempDir,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		_ = chmodPath(dir, 0o700)
	}
	return nil
}

func EnsurePrivatePermissions(paths AppPaths) error {
	if privatePathInsideRoot(paths.Root, paths.Root) {
		if err := chmodPath(paths.Root, 0o700); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	for _, root := range []string{paths.ConfigDir, paths.DataDir, paths.StateDir} {
		if !privatePathInsideRoot(paths.Root, root) {
			continue
		}
		if err := chmodExistingDirectories(root, 0o700); err != nil {
			return err
		}
	}
	files := []string{
		paths.SettingsFile, paths.MCPConfigFile,
		filepath.Join(paths.MemoryDir, "fabric", "ledger.sqlite"),
		filepath.Join(paths.MemoryDir, "fabric", "ledger.sqlite-shm"),
		filepath.Join(paths.MemoryDir, "fabric", "ledger.sqlite-wal"),
		filepath.Join(paths.MemoryDir, "fabric", "index.sqlite"),
		filepath.Join(paths.MemoryDir, "fabric", "index.sqlite-shm"),
		filepath.Join(paths.MemoryDir, "fabric", "index.sqlite-wal"),
		paths.EndpointFile, paths.BackendLogFile, paths.ManagedMCPFile,
	}
	for _, path := range files {
		if !privatePathInsideRoot(paths.Root, path) {
			continue
		}
		if err := chmodExistingFile(path, 0o600); err != nil {
			return err
		}
	}
	for _, root := range []string{paths.ActiveSessionsDir, paths.ArchivedSessionsDir, paths.ProjectsDataDir, paths.ProjectsStateDir} {
		if !privatePathInsideRoot(paths.Root, root) {
			continue
		}
		if err := chmodExistingFiles(root, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func chmodExistingDirectories(root string, mode os.FileMode) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		if entry.IsDir() {
			return chmodPath(path, mode)
		}
		return nil
	})
}

func privatePathInsideRoot(root, path string) bool {
	if strings.TrimSpace(root) == "" || strings.TrimSpace(path) == "" {
		return false
	}
	return samePath(root, path) || insidePath(path, root)
}

func chmodExistingFiles(root string, mode os.FileMode) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type().IsRegular() {
			return chmodPath(path, mode)
		}
		return nil
	})
}

func chmodExistingFile(path string, mode os.FileMode) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() {
		return chmodPath(path, mode)
	}
	return nil
}

func EnsureProjectManifest(project ProjectPaths, now time.Time) error {
	if now.IsZero() {
		now = time.Now()
	}
	if err := os.MkdirAll(project.TrustDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(project.TeamsDir, 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(project.ToolResultsDir, 0o700); err != nil {
		return err
	}
	manifest := ProjectManifest{
		ID: project.ID, CanonicalRoot: project.CanonicalRoot, DisplayName: filepath.Base(filepath.FromSlash(project.CanonicalRoot)),
		FirstSeenAt: now.UTC().Format(time.RFC3339Nano), LastSeenAt: now.UTC().Format(time.RFC3339Nano),
	}
	if data, err := os.ReadFile(project.ManifestFile); err == nil {
		var existing ProjectManifest
		if err := json.Unmarshal(data, &existing); err != nil {
			return fmt.Errorf("parse project manifest %s: %w", project.ManifestFile, err)
		}
		if existing.ID != project.ID || existing.CanonicalRoot != project.CanonicalRoot {
			return fmt.Errorf("project id collision at %s", project.ManifestFile)
		}
		if existing.FirstSeenAt != "" {
			manifest.FirstSeenAt = existing.FirstSeenAt
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeJSONAtomic(project.ManifestFile, manifest, 0o600)
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return WriteFileAtomic(path, data, mode)
}
