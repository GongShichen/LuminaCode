package apppaths

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type PathStatus struct {
	Path      string `json:"path"`
	Exists    bool   `json:"exists"`
	SizeBytes int64  `json:"size_bytes"`
	Mode      string `json:"mode,omitempty"`
}

type DoctorReport struct {
	LayoutStatus     string                `json:"layout_status"`
	Layout           *Layout               `json:"layout,omitempty"`
	Paths            AppPaths              `json:"paths"`
	Layers           map[string]PathStatus `json:"layers"`
	Components       map[string]PathStatus `json:"components"`
	LegacyEntries    []string              `json:"legacy_entries,omitempty"`
	ProjectConflicts []string              `json:"project_conflicts,omitempty"`
	Unrecoverable    []string              `json:"unrecoverable_components,omitempty"`
	Warnings         []string              `json:"warnings,omitempty"`
}

func Doctor(paths AppPaths) DoctorReport {
	report := DoctorReport{Paths: paths, Layers: map[string]PathStatus{}, Components: map[string]PathStatus{}}
	if layout, err := ReadLayout(paths); err == nil {
		report.Layout = &layout
		if layout.LayoutVersion == LayoutVersion {
			report.LayoutStatus = "ready"
		} else {
			report.LayoutStatus = "unsupported"
			report.Warnings = append(report.Warnings, fmt.Sprintf("layout version %d is unsupported", layout.LayoutVersion))
		}
		if paths.Platform != "" && layout.Platform != "" && !strings.EqualFold(paths.Platform, layout.Platform) {
			report.Warnings = append(report.Warnings, fmt.Sprintf("layout platform is %s but runtime platform is %s", layout.Platform, paths.Platform))
		}
	} else if HasLegacyLayout(paths.Root) {
		report.LayoutStatus = "migration_required"
	} else if os.IsNotExist(err) {
		report.LayoutStatus = "not_initialized"
	} else {
		report.LayoutStatus = "invalid"
		report.Warnings = append(report.Warnings, err.Error())
	}
	for name, path := range map[string]string{
		"app": paths.AppDir, "config": paths.ConfigDir, "data": paths.DataDir, "state": paths.StateDir, "cache": paths.CacheDir,
	} {
		report.Layers[name] = inspectPath(path)
	}
	for name, path := range map[string]string{
		"frontend":       filepath.Join(paths.FrontendDir, "dist", "index.js"),
		"system_prompt":  filepath.Join(paths.SystemResourceDir, "system-prompt.md"),
		"settings":       paths.SettingsFile,
		"mcp_config":     paths.MCPConfigFile,
		"memory_db":      paths.MemoryDB,
		"memory_model":   paths.MemoryModelDir,
		"endpoint":       paths.EndpointFile,
		"arxiv_mcp":      paths.ArxivMCPDir,
		"searxng":        paths.SearxNGDir,
		"migration_lock": filepath.Join(paths.Root, ".layout-migration.lock"),
	} {
		report.Components[name] = inspectPath(path)
	}
	rootEntries, _ := os.ReadDir(paths.Root)
	actualRootEntries := make(map[string]struct{}, len(rootEntries))
	for _, entry := range rootEntries {
		actualRootEntries[entry.Name()] = struct{}{}
	}
	for _, name := range []string{"CONFIG", "SYSTEM", "SKILLS", "TEAM", "frontend", "mcp", "memory", "models", "project", "sessions", "session-archive", "searxng", "run"} {
		if _, exists := actualRootEntries[name]; exists {
			report.LegacyEntries = append(report.LegacyEntries, name)
		}
	}
	report.ProjectConflicts = inspectProjectManifests(paths.ProjectsDataDir, paths.Platform)
	if runtime.GOOS != "windows" {
		for name, path := range map[string]string{"AppRoot": paths.Root, "config": paths.ConfigDir, "data": paths.DataDir, "state": paths.StateDir} {
			if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
				report.Warnings = append(report.Warnings, fmt.Sprintf("%s directory is accessible to other local users", name))
			}
		}
		for name, path := range map[string]string{
			"layout": paths.LayoutFile, "settings": paths.SettingsFile, "MCP config": paths.MCPConfigFile,
			"memory database": paths.MemoryDB, "endpoint": paths.EndpointFile,
		} {
			if info, err := os.Stat(path); err == nil && info.Mode().Perm()&0o077 != 0 {
				report.Warnings = append(report.Warnings, fmt.Sprintf("%s file is accessible to other local users", name))
			}
		}
	}
	if report.LayoutStatus == "unsupported" || report.LayoutStatus == "invalid" {
		report.Unrecoverable = append(report.Unrecoverable, "layout.json")
	}
	if report.Components["migration_lock"].Exists {
		report.Warnings = append(report.Warnings, "migration lock is present; another migration may be running or recovery may be required")
	}
	if len(report.ProjectConflicts) > 0 {
		report.Unrecoverable = append(report.Unrecoverable, "project manifests")
	}
	if !report.Components["frontend"].Exists {
		report.Warnings = append(report.Warnings, "frontend payload is missing; reinstall the app layer")
	}
	if !report.Components["system_prompt"].Exists {
		report.Warnings = append(report.Warnings, "bundled system resources are missing; reinstall the app layer")
	}
	sort.Strings(report.LegacyEntries)
	sort.Strings(report.ProjectConflicts)
	sort.Strings(report.Unrecoverable)
	sort.Strings(report.Warnings)
	return report
}

func (r DoctorReport) Healthy() bool {
	return r.LayoutStatus == "ready" && r.Components["frontend"].Exists && r.Components["system_prompt"].Exists &&
		!r.Components["migration_lock"].Exists && len(r.ProjectConflicts) == 0
}

func inspectPath(path string) PathStatus {
	status := PathStatus{Path: path}
	info, err := os.Stat(path)
	if err != nil {
		return status
	}
	status.Exists = true
	status.Mode = info.Mode().Perm().String()
	status.SizeBytes, _ = pathSize(path)
	return status
}

func inspectProjectManifests(root, platform string) []string {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	seenRoots := map[string]string{}
	var conflicts []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(root, entry.Name(), "project.json")
		data, err := os.ReadFile(path)
		if err != nil {
			conflicts = append(conflicts, entry.Name()+": missing or unreadable project.json")
			continue
		}
		var manifest ProjectManifest
		if json.Unmarshal(data, &manifest) != nil || manifest.ID != entry.Name() || manifest.CanonicalRoot == "" {
			conflicts = append(conflicts, entry.Name()+": invalid project manifest")
			continue
		}
		canonical, canonicalErr := CanonicalProjectRoot(manifest.CanonicalRoot, platform)
		if canonicalErr != nil || canonical != manifest.CanonicalRoot || ProjectIDFromCanonical(canonical) != manifest.ID {
			conflicts = append(conflicts, entry.Name()+": project id does not match canonical root")
			continue
		}
		if prior, ok := seenRoots[canonical]; ok && prior != manifest.ID {
			conflicts = append(conflicts, fmt.Sprintf("%s and %s refer to the same project root", prior, manifest.ID))
		}
		seenRoots[canonical] = manifest.ID
	}
	return conflicts
}
