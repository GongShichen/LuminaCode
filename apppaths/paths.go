package apppaths

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"
	"unicode"
)

const (
	LayoutVersion = 2
	AppName       = "LuminaCode"
)

type ResolveOptions struct {
	GOOS         string
	HomeDir      string
	LocalAppData string
	Env          map[string]string
}

type AppPaths struct {
	Platform            string `json:"platform"`
	Root                string `json:"root"`
	LayoutFile          string `json:"layout_file"`
	AppDir              string `json:"app_dir"`
	FrontendDir         string `json:"frontend_dir"`
	ResourcesDir        string `json:"resources_dir"`
	DefaultsResourceDir string `json:"defaults_resource_dir"`
	SystemResourceDir   string `json:"system_resource_dir"`
	BundledSkillsDir    string `json:"bundled_skills_dir"`
	BundledTeamsDir     string `json:"bundled_teams_dir"`
	ExtensionsDir       string `json:"extensions_dir"`
	ArxivMCPDir         string `json:"arxiv_mcp_dir"`
	ScriptsDir          string `json:"scripts_dir"`
	ConfigDir           string `json:"config_dir"`
	SettingsFile        string `json:"settings_file"`
	MCPConfigFile       string `json:"mcp_config_file"`
	InstructionsDir     string `json:"instructions_dir"`
	PromptsDir          string `json:"prompts_dir"`
	UserSkillsDir       string `json:"user_skills_dir"`
	UserTeamsDir        string `json:"user_teams_dir"`
	DataDir             string `json:"data_dir"`
	MemoryDir           string `json:"memory_dir"`
	SessionsDir         string `json:"sessions_dir"`
	ActiveSessionsDir   string `json:"active_sessions_dir"`
	ArchivedSessionsDir string `json:"archived_sessions_dir"`
	ProjectsDataDir     string `json:"projects_data_dir"`
	LegacyDataDir       string `json:"legacy_data_dir"`
	StateDir            string `json:"state_dir"`
	RunDir              string `json:"run_dir"`
	EndpointFile        string `json:"endpoint_file"`
	LogsDir             string `json:"logs_dir"`
	BackendLogFile      string `json:"backend_log_file"`
	ManagedDir          string `json:"managed_dir"`
	ManagedMCPFile      string `json:"managed_mcp_file"`
	ServicesDir         string `json:"services_dir"`
	SearxNGDir          string `json:"searxng_dir"`
	MigrationsDir       string `json:"migrations_dir"`
	ProjectsStateDir    string `json:"projects_state_dir"`
	CacheDir            string `json:"cache_dir"`
	ModelsDir           string `json:"models_dir"`
	MemoryModelDir      string `json:"memory_model_dir"`
	DownloadsDir        string `json:"downloads_dir"`
	TempDir             string `json:"temp_dir"`
}

type ProjectPaths struct {
	Platform       string `json:"platform"`
	ID             string `json:"id"`
	CanonicalRoot  string `json:"canonical_root"`
	DataDir        string `json:"data_dir"`
	ManifestFile   string `json:"manifest_file"`
	TrustDir       string `json:"trust_dir"`
	MCPTrustFile   string `json:"mcp_trust_file"`
	TeamsDir       string `json:"teams_dir"`
	StateDir       string `json:"state_dir"`
	ToolResultsDir string `json:"tool_results_dir"`
}

func ResolveCurrent() (AppPaths, error) {
	home, err := os.UserHomeDir()
	if runtime.GOOS == "windows" {
		if userProfile := strings.TrimSpace(os.Getenv("USERPROFILE")); userProfile != "" {
			home = userProfile
			err = nil
		}
	} else if envHome := strings.TrimSpace(os.Getenv("HOME")); envHome != "" {
		home = envHome
		err = nil
	}
	if err != nil {
		return AppPaths{}, err
	}
	return Resolve(ResolveOptions{
		GOOS:         runtime.GOOS,
		HomeDir:      home,
		LocalAppData: os.Getenv("LOCALAPPDATA"),
		Env:          environmentMap(),
	})
}

func Resolve(opts ResolveOptions) (AppPaths, error) {
	goos := strings.ToLower(strings.TrimSpace(opts.GOOS))
	if goos == "" {
		goos = runtime.GOOS
	}
	home := strings.TrimSpace(opts.HomeDir)
	root := strings.TrimSpace(envValue(opts.Env, "LUMINA_APP_ROOT"))
	if root != "" {
		root = expandHome(root, home, goos)
		if !platformIsAbs(root, goos) {
			return AppPaths{}, fmt.Errorf("LUMINA_APP_ROOT must be absolute: %q", root)
		}
	} else if goos == "windows" && strings.TrimSpace(firstNonEmptyPath(opts.LocalAppData, envValue(opts.Env, "LOCALAPPDATA"))) != "" {
		root = platformJoin(goos, strings.TrimSpace(firstNonEmptyPath(opts.LocalAppData, envValue(opts.Env, "LOCALAPPDATA"))), AppName)
	} else {
		if home == "" {
			return AppPaths{}, errors.New("cannot resolve LuminaCode app root: user home is empty")
		}
		root = platformJoin(goos, home, ".lumina")
	}
	if !platformIsAbs(root, goos) {
		return AppPaths{}, fmt.Errorf("resolved LuminaCode AppRoot must be absolute: %q", root)
	}
	root = platformClean(root, goos)
	if err := validateRoot(root, goos); err != nil {
		return AppPaths{}, err
	}

	join := func(elements ...string) string { return platformJoin(goos, elements...) }
	appDir := join(root, "app")
	resourcesDir := join(appDir, "resources")
	configDir := join(root, "config")
	dataDir := join(root, "data")
	stateDir := join(root, "state")
	cacheDir := join(root, "cache")
	memoryModelDir := join(cacheDir, "models", "memory", "bge-m3")
	return AppPaths{
		Platform: goos, Root: root, LayoutFile: join(root, "layout.json"),
		AppDir: appDir, FrontendDir: join(appDir, "frontend"), ResourcesDir: resourcesDir,
		DefaultsResourceDir: join(resourcesDir, "defaults"), SystemResourceDir: join(resourcesDir, "system"),
		BundledSkillsDir: join(resourcesDir, "skills"), BundledTeamsDir: join(resourcesDir, "teams"),
		ExtensionsDir: join(appDir, "extensions"), ArxivMCPDir: join(appDir, "extensions", "arxiv-mcp"),
		ScriptsDir: join(appDir, "scripts"),
		ConfigDir:  configDir, SettingsFile: join(configDir, "settings.json"), MCPConfigFile: join(configDir, "mcp.json"),
		InstructionsDir: join(configDir, "instructions"), PromptsDir: join(configDir, "prompts"),
		UserSkillsDir: join(configDir, "skills"), UserTeamsDir: join(configDir, "teams"),
		DataDir: dataDir, MemoryDir: join(dataDir, "memory"),
		SessionsDir: join(dataDir, "sessions"), ActiveSessionsDir: join(dataDir, "sessions", "active"),
		ArchivedSessionsDir: join(dataDir, "sessions", "archive"), ProjectsDataDir: join(dataDir, "projects"),
		LegacyDataDir: join(dataDir, "legacy", "layout"),
		StateDir:      stateDir, RunDir: join(stateDir, "run"), EndpointFile: join(stateDir, "run", "backend.json"),
		LogsDir: join(stateDir, "logs"), BackendLogFile: join(stateDir, "logs", "backend.log"),
		ManagedDir: join(stateDir, "managed"), ManagedMCPFile: join(stateDir, "managed", "mcp.json"),
		ServicesDir: join(stateDir, "services"), SearxNGDir: join(stateDir, "services", "searxng"),
		MigrationsDir: join(stateDir, "migrations"), ProjectsStateDir: join(stateDir, "projects"),
		CacheDir: cacheDir, ModelsDir: join(cacheDir, "models"), MemoryModelDir: memoryModelDir,
		DownloadsDir: join(cacheDir, "downloads"), TempDir: join(cacheDir, "tmp"),
	}, nil
}

func (p AppPaths) ForProject(root string) (ProjectPaths, error) {
	platform := p.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	canonical, err := CanonicalProjectRoot(root, platform)
	if err != nil {
		return ProjectPaths{}, err
	}
	id := ProjectIDFromCanonical(canonical)
	dataDir := platformJoin(platform, p.ProjectsDataDir, id)
	stateDir := platformJoin(platform, p.ProjectsStateDir, id)
	join := func(elements ...string) string { return platformJoin(platform, elements...) }
	return ProjectPaths{
		Platform: platform, ID: id, CanonicalRoot: canonical, DataDir: dataDir, ManifestFile: join(dataDir, "project.json"),
		TrustDir: join(dataDir, "trust"), MCPTrustFile: join(dataDir, "trust", "mcp.json"),
		TeamsDir: join(dataDir, "teams"), StateDir: stateDir, ToolResultsDir: join(stateDir, "tool-results"),
	}, nil
}

func CanonicalProjectRoot(root, goos string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return "", errors.New("project root is empty")
	}
	if strings.EqualFold(goos, "windows") && runtime.GOOS != "windows" && isWindowsAbsolute(root) {
		return canonicalWindowsPath(root), nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("resolve project root: %w", err)
	}
	abs = filepath.Clean(abs)
	if resolved, resolveErr := filepath.EvalSymlinks(abs); resolveErr == nil {
		abs = filepath.Clean(resolved)
	}
	if strings.EqualFold(goos, "windows") {
		abs = canonicalWindowsPath(abs)
	}
	return abs, nil
}

func isWindowsAbsolute(value string) bool {
	value = strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")
	if strings.HasPrefix(value, "//") {
		parts := strings.Split(strings.TrimPrefix(value, "//"), "/")
		return len(parts) >= 2 && parts[0] != "" && parts[1] != ""
	}
	return len(value) >= 3 && unicode.IsLetter(rune(value[0])) && value[1] == ':' && value[2] == '/'
}

func canonicalWindowsPath(value string) string {
	return cleanWindowsPath(value, true)
}

func cleanWindowsPath(value string, foldCase bool) string {
	value = strings.ReplaceAll(strings.TrimSpace(value), `\`, "/")
	unc := strings.HasPrefix(value, "//")
	if unc {
		value = strings.TrimPrefix(value, "//")
	}
	parts := strings.Split(value, "/")
	cleaned := make([]string, 0, len(parts))
	minimumDepth := 0
	if unc {
		minimumDepth = 2
	} else if len(parts) > 0 && strings.HasSuffix(parts[0], ":") {
		minimumDepth = 1
	}
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(cleaned) > minimumDepth && cleaned[len(cleaned)-1] != ".." {
				cleaned = cleaned[:len(cleaned)-1]
			}
			continue
		}
		cleaned = append(cleaned, part)
	}
	canonical := strings.Join(cleaned, "/")
	if foldCase {
		canonical = strings.ToLower(canonical)
	}
	if unc {
		return "//" + canonical
	}
	if len(cleaned) == 1 && strings.HasSuffix(cleaned[0], ":") {
		return canonical + "/"
	}
	return canonical
}

func ProjectIDFromCanonical(canonical string) string {
	base := strings.TrimRight(strings.ReplaceAll(canonical, `\`, "/"), "/")
	if index := strings.LastIndex(base, "/"); index >= 0 {
		base = base[index+1:]
	}
	slug := slugify(base, 48)
	sum := sha256.Sum256([]byte(canonical))
	return slug + "-" + hex.EncodeToString(sum[:8])
}

func (p ProjectPaths) ToolResultsForSession(sessionID string) string {
	platform := p.Platform
	if platform == "" {
		platform = runtime.GOOS
	}
	return platformJoin(platform, p.ToolResultsDir, safeComponent(sessionID, "_legacy"))
}

func slugify(value string, maxLen int) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		valid := unicode.IsLetter(r) || unicode.IsDigit(r) || r == '.' || r == '_' || r == '-'
		if !valid {
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
			continue
		}
		b.WriteRune(r)
		lastDash = r == '-'
	}
	out := strings.Trim(b.String(), ".-_ ")
	if out == "" {
		out = "default"
	}
	if maxLen > 0 {
		runes := []rune(out)
		if len(runes) > maxLen {
			out = strings.Trim(string(runes[:maxLen]), ".-_ ")
		}
	}
	if out == "" {
		return "default"
	}
	return out
}

func safeComponent(value, fallback string) string {
	value = slugify(value, 96)
	if value == "default" && strings.TrimSpace(fallback) != "" {
		return fallback
	}
	return value
}

func validateRoot(root, goos string) error {
	if strings.TrimSpace(root) == "" || root == "." {
		return errors.New("LuminaCode app root is empty")
	}
	if goos == "windows" {
		canonical := canonicalWindowsPath(root)
		if len(canonical) == 3 && canonical[1:] == ":/" {
			return fmt.Errorf("refusing unsafe LuminaCode app root: %s", root)
		}
		if strings.HasPrefix(canonical, "//") && len(strings.Split(strings.TrimPrefix(canonical, "//"), "/")) <= 2 {
			return fmt.Errorf("refusing unsafe LuminaCode app root: %s", root)
		}
		return nil
	}
	if platformClean(root, goos) == "/" {
		return fmt.Errorf("refusing unsafe LuminaCode app root: %s", root)
	}
	return nil
}

func expandHome(value, home, goos string) string {
	if value == "~" {
		return home
	}
	if strings.HasPrefix(value, "~/") || strings.HasPrefix(value, `~\`) {
		return platformJoin(goos, home, value[2:])
	}
	return value
}

func platformIsAbs(value, goos string) bool {
	if goos == "windows" {
		return isWindowsAbsolute(value)
	}
	return strings.HasPrefix(value, "/")
}

func platformClean(value, goos string) string {
	if goos == "windows" {
		return strings.ReplaceAll(cleanWindowsPath(value, false), "/", `\`)
	}
	return pathpkg.Clean(value)
}

func platformJoin(goos string, elements ...string) string {
	if goos != "windows" {
		return pathpkg.Join(elements...)
	}
	var joined string
	for _, element := range elements {
		if strings.TrimSpace(element) == "" {
			continue
		}
		if joined == "" {
			joined = element
			continue
		}
		joined = strings.TrimRight(joined, `/\`) + "/" + strings.TrimLeft(element, `/\`)
	}
	return platformClean(joined, goos)
}

func envValue(env map[string]string, key string) string {
	if env == nil {
		return os.Getenv(key)
	}
	return env[key]
}

func firstNonEmptyPath(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func environmentMap() map[string]string {
	out := map[string]string{}
	for _, entry := range os.Environ() {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}
