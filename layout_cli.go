package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime"
	"strings"

	"LuminaCode/apppaths"
	"LuminaCode/backend"
	"LuminaCode/longmemory"
)

func runLayoutCLI(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lumina-backend layout <paths|doctor|migrate|bind-project>")
	}
	paths, err := apppaths.ResolveCurrent()
	if err != nil {
		return err
	}
	switch args[0] {
	case "paths":
		flags := flag.NewFlagSet("layout paths", flag.ContinueOnError)
		jsonOutput := flags.Bool("json", false, "write resolved paths as JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *jsonOutput {
			return writeJSON(os.Stdout, paths)
		}
		fmt.Fprintln(os.Stdout, paths.Root)
		return nil
	case "doctor":
		flags := flag.NewFlagSet("layout doctor", flag.ContinueOnError)
		jsonOutput := flags.Bool("json", false, "write doctor report as JSON")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		report := apppaths.Doctor(paths)
		if *jsonOutput {
			if err := writeJSON(os.Stdout, report); err != nil {
				return err
			}
			if !report.Healthy() {
				return fmt.Errorf("AppRoot health check failed")
			}
			return nil
		}
		fmt.Fprintf(os.Stdout, "Layout: %s\nAppRoot: %s\n", report.LayoutStatus, paths.Root)
		for _, name := range []string{"app", "config", "data", "state", "cache"} {
			status := report.Layers[name]
			fmt.Fprintf(os.Stdout, "%-7s %10d bytes  %s\n", name+":", status.SizeBytes, status.Path)
		}
		for _, warning := range report.Warnings {
			fmt.Fprintln(os.Stdout, "warning:", warning)
		}
		if !report.Healthy() {
			return fmt.Errorf("AppRoot health check failed")
		}
		return nil
	case "migrate":
		flags := flag.NewFlagSet("layout migrate", flag.ContinueOnError)
		apply := flags.Bool("apply", false, "apply the migration")
		dryRun := flags.Bool("dry-run", false, "inspect without changing files")
		source := flags.String("source", "", "legacy AppRoot source")
		projectRoot := flags.String("project-root", "", "project root used to bind one legacy project")
		installedVersion := flags.String("installed-version", "", "installed LuminaCode version")
		packagedResources := flags.String("packaged-resources", "", "staged app/resources directory used to identify user additions")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *apply && *dryRun {
			return fmt.Errorf("choose either --apply or --dry-run")
		}
		if strings.TrimSpace(*source) == "" {
			*source = defaultLegacySource(paths)
		}
		if *apply {
			for _, endpoint := range []string{paths.EndpointFile, apppaths.LegacyEndpointFile(*source)} {
				if _, statErr := os.Stat(endpoint); statErr == nil {
					if shutdownErr := backend.RunShutdownCLI([]string{"--endpoint", endpoint}); shutdownErr != nil {
						return fmt.Errorf("stop backend before migration: %w", shutdownErr)
					}
				}
			}
		}
		report, migrateErr := apppaths.Migrate(paths, apppaths.MigrationOptions{
			Apply: *apply, SourceRoot: *source, CurrentProjectRoot: *projectRoot, InstalledVersion: *installedVersion,
			PackagedResources:  *packagedResources,
			BeforeLayoutCommit: migrateLegacyMemory,
		})
		if err := writeJSON(os.Stdout, report); err != nil {
			return err
		}
		return migrateErr
	case "bind-project":
		flags := flag.NewFlagSet("layout bind-project", flag.ContinueOnError)
		legacy := flags.String("legacy", "", "legacy project directory name")
		root := flags.String("root", "", "canonical project root")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if *legacy == "" || *root == "" {
			return fmt.Errorf("layout bind-project requires --legacy and --root")
		}
		if err := apppaths.CheckLayout(paths); err != nil {
			return err
		}
		if _, statErr := os.Stat(paths.EndpointFile); statErr == nil {
			if shutdownErr := backend.RunShutdownCLI([]string{"--endpoint", paths.EndpointFile}); shutdownErr != nil {
				return fmt.Errorf("stop backend before binding project: %w", shutdownErr)
			}
		}
		return apppaths.BindLegacyProject(paths, *legacy, *root)
	default:
		return fmt.Errorf("unknown layout command %q", args[0])
	}
}

func migrateLegacyMemory(paths apppaths.AppPaths, _ *apppaths.MigrationReport) error {
	existed := false
	if _, err := os.Stat(paths.MemoryDB); err == nil {
		existed = true
	}
	store, err := longmemory.Open(context.Background(), paths.MemoryDB)
	if err != nil {
		if !existed {
			removeSQLiteFiles(paths.MemoryDB)
		}
		return err
	}
	if err := store.Close(); err != nil {
		if !existed {
			removeSQLiteFiles(paths.MemoryDB)
		}
		return err
	}
	return nil
}

func removeSQLiteFiles(path string) {
	for _, suffix := range []string{"", "-shm", "-wal"} {
		_ = os.Remove(path + suffix)
	}
}

func defaultLegacySource(paths apppaths.AppPaths) string {
	if runtime.GOOS != "windows" || apppaths.HasLegacyLayout(paths.Root) {
		return paths.Root
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return paths.Root
	}
	legacy := apppaths.LegacyDefaultRoot(home)
	if apppaths.HasLegacyLayout(legacy) {
		return legacy
	}
	return paths.Root
}
