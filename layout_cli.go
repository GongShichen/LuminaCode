package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"LuminaCode/apppaths"
	"LuminaCode/backend"
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
				if err := stopLayoutBackend(endpoint, "migration"); err != nil {
					return err
				}
			}
		}
		report, migrateErr := apppaths.Migrate(paths, apppaths.MigrationOptions{
			Apply: *apply, SourceRoot: *source, CurrentProjectRoot: *projectRoot, InstalledVersion: *installedVersion,
			PackagedResources: *packagedResources,
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
		if err := stopLayoutBackend(paths.EndpointFile, "binding project"); err != nil {
			return err
		}
		return apppaths.BindLegacyProject(paths, *legacy, *root)
	default:
		return fmt.Errorf("unknown layout command %q", args[0])
	}
}

func stopLayoutBackend(endpoint, action string) error {
	if _, statErr := os.Stat(endpoint); os.IsNotExist(statErr) {
		return nil
	} else if statErr != nil {
		return fmt.Errorf("inspect backend endpoint before %s: %w", action, statErr)
	}
	if shutdownErr := backend.RunShutdownCLI([]string{"--endpoint", endpoint}); shutdownErr != nil {
		if isStaleBackendEndpointError(shutdownErr) {
			fmt.Fprintf(os.Stderr, "warning: ignoring stale backend endpoint %s: %v\n", endpoint, shutdownErr)
			_ = os.Remove(endpoint)
			return nil
		}
		return fmt.Errorf("stop backend before %s: %w", action, shutdownErr)
	}
	return nil
}

func isStaleBackendEndpointError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "connection refused") ||
		strings.Contains(message, "actively refused") ||
		strings.Contains(message, "connectex:")
}

func defaultLegacySource(paths apppaths.AppPaths) string {
	if runtime.GOOS != "windows" || apppaths.HasLegacyLayout(paths.Root) {
		return paths.Root
	}
	if !isDefaultWindowsAppRoot(paths.Root) {
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

func isDefaultWindowsAppRoot(root string) bool {
	localAppData := strings.TrimSpace(os.Getenv("LOCALAPPDATA"))
	if localAppData == "" {
		return false
	}
	defaultRoot := filepath.Join(localAppData, apppaths.AppName)
	return strings.EqualFold(filepath.Clean(root), filepath.Clean(defaultRoot))
}
