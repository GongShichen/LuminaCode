package team

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"LuminaCode/apppaths"
	"LuminaCode/config"

	"gopkg.in/yaml.v3"
)

const (
	TeamConfigFile        = "team.yaml"
	AgentConfigFile       = "agent.yaml"
	AgentSystemPromptFile = "system.md"
	TeamSystemFile        = "team-system.md"
	SharedPromptFile      = "shared-prompt.md"
	CompletionPolicyFile  = "completion-policy.md"
)

type Loader struct {
	Config config.Config
}

func NewLoader(cfg config.Config) Loader {
	return Loader{Config: cfg}
}

func (l Loader) TeamDir() string {
	if strings.TrimSpace(l.Config.TeamDir) != "" {
		return expandHome(l.Config.TeamDir)
	}
	return l.Config.Paths.UserTeamsDir
}

func (l Loader) BundledTeamDir() string {
	if strings.TrimSpace(l.Config.BundledTeamDir) != "" {
		return l.Config.BundledTeamDir
	}
	return l.Config.Paths.BundledTeamsDir
}

func (l Loader) ProjectTeamDir() string {
	start := l.Config.CWD
	if strings.TrimSpace(l.Config.ProjectPaths.CanonicalRoot) != "" {
		start = l.Config.ProjectPaths.CanonicalRoot
	}
	current, err := filepath.Abs(start)
	if err != nil {
		current = start
	}
	if info, statErr := os.Stat(current); statErr == nil && !info.IsDir() {
		current = filepath.Dir(current)
	}
	for current != "" {
		candidate := apppaths.ProjectTeamsDir(current)
		if info, statErr := os.Stat(candidate); statErr == nil && info.IsDir() {
			return candidate
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ""
}

func (l Loader) teamRoots() []string {
	return uniqueTeamRoots([]string{
		l.ProjectTeamDir(),
		l.TeamDir(),
		l.BundledTeamDir(),
		l.Config.Paths.BundledTeamsDir,
	})
}

func (l Loader) teamRoot(name string) string {
	for _, root := range l.teamRoots() {
		if strings.TrimSpace(root) == "" {
			continue
		}
		candidate := filepath.Join(root, name)
		if info, err := os.Stat(filepath.Join(candidate, TeamConfigFile)); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return filepath.Join(l.TeamDir(), name)
}

func (l Loader) ListTeams() []TeamListItem {
	specs, _ := l.LoadAll()
	items := make([]TeamListItem, 0, len(specs))
	for _, spec := range specs {
		items = append(items, TeamListItem{
			Name:        spec.Name,
			DisplayName: spec.DisplayName,
			Description: spec.Description,
			AgentCount:  len(spec.AgentSpecs),
			RootDir:     spec.RootDir,
		})
	}
	return items
}

func (l Loader) LoadAll() ([]TeamSpec, []error) {
	names := map[string]string{}
	roots := l.teamRoots()
	for i := len(roots) - 1; i >= 0; i-- {
		root := roots[i]
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				names[strings.ToLower(entry.Name())] = entry.Name()
			}
		}
	}
	ordered := make([]string, 0, len(names))
	for _, name := range names {
		ordered = append(ordered, name)
	}
	sort.Slice(ordered, func(i, j int) bool { return strings.ToLower(ordered[i]) < strings.ToLower(ordered[j]) })
	var specs []TeamSpec
	var errs []error
	for _, name := range ordered {
		spec, err := l.Load(name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		specs = append(specs, spec)
	}
	return specs, errs
}

func uniqueTeamRoots(roots []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(roots))
	for _, root := range roots {
		root = strings.TrimSpace(root)
		if root == "" {
			continue
		}
		key, err := filepath.Abs(root)
		if err != nil {
			key = filepath.Clean(root)
		}
		if resolved, resolveErr := filepath.EvalSymlinks(key); resolveErr == nil {
			key = resolved
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, root)
	}
	return result
}

func (l Loader) Load(name string) (TeamSpec, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return TeamSpec{}, fmt.Errorf("team name is required")
	}
	root := l.teamRoot(name)
	data, err := os.ReadFile(filepath.Join(root, TeamConfigFile))
	if err != nil {
		return TeamSpec{}, fmt.Errorf("team %q config not found: %w", name, err)
	}
	var spec TeamSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return TeamSpec{}, fmt.Errorf("invalid team.yaml for %q: %w", name, err)
	}
	if spec.Name == "" {
		spec.Name = name
	}
	if spec.DisplayName == "" {
		spec.DisplayName = spec.Name
	}
	if spec.Loop.StopPolicy == "" {
		spec.Loop.StopPolicy = "user_interrupt_or_task_complete_only"
	}
	if spec.Loop.MaxParallelAgents <= 0 {
		spec.Loop.MaxParallelAgents = 4
	}
	if spec.Loop.A2ADefaultTimeoutSeconds <= 0 {
		spec.Loop.A2ADefaultTimeoutSeconds = 300
	}
	if spec.Loop.WaitForPendingA2ABeforeNextIteration == nil {
		wait := true
		spec.Loop.WaitForPendingA2ABeforeNextIteration = &wait
	}
	spec.RootDir = root
	spec.TeamSystemPath = filepath.Join(root, TeamSystemFile)
	if sharedPath := filepath.Join(root, SharedPromptFile); fileExists(sharedPath) {
		spec.SharedPromptPath = sharedPath
	}
	spec.CompletionPolicy = filepath.Join(root, CompletionPolicyFile)
	spec.Transcript.ShowMemberDialogue = true
	spec.LoadedAt = time.Now()

	if _, err := os.Stat(spec.TeamSystemPath); err != nil {
		return TeamSpec{}, fmt.Errorf("team %q missing %s", name, TeamSystemFile)
	}
	if _, err := os.Stat(spec.CompletionPolicy); err != nil {
		return TeamSpec{}, fmt.Errorf("team %q missing %s", name, CompletionPolicyFile)
	}
	agents, err := l.loadAgents(root, spec.Agents)
	if err != nil {
		return TeamSpec{}, err
	}
	spec.AgentSpecs = agents
	spec.AgentMap = map[string]int{}
	for i, agent := range agents {
		spec.AgentMap[agent.Name] = i
	}
	if _, ok := spec.AgentMap[spec.EntryAgent]; !ok {
		return TeamSpec{}, fmt.Errorf("team %q entry_agent %q is not defined", name, spec.EntryAgent)
	}
	spec.Gates = normalizeGateSpec(spec.Gates)
	for _, check := range spec.Gates.Checks {
		if check.Agent == "" {
			return TeamSpec{}, fmt.Errorf("team %q gate check %q missing agent", name, check.Name)
		}
		if _, ok := spec.AgentMap[check.Agent]; !ok {
			return TeamSpec{}, fmt.Errorf("team %q gate check %q agent %q is not defined", name, check.Name, check.Agent)
		}
	}
	return spec, nil
}

func normalizeGateSpec(gates TeamGateSpec) TeamGateSpec {
	if gates.NonblockingFindings == "" {
		gates.NonblockingFindings = "allow_complete"
	}
	checks := make([]TeamGateCheckSpec, 0, len(gates.Checks))
	for _, check := range gates.Checks {
		check.Name = strings.TrimSpace(check.Name)
		check.Agent = strings.TrimSpace(check.Agent)
		if check.Name == "" {
			check.Name = check.Agent
		}
		if check.Agent == "" {
			check.Agent = check.Name
		}
		if check.Name == "" {
			continue
		}
		check = normalizeGateCheck(check)
		checks = append(checks, check)
	}
	gates.Checks = checks
	return gates
}

func normalizeGateCheck(check TeamGateCheckSpec) TeamGateCheckSpec {
	if len(check.PassStatuses) == 0 {
		check.PassStatuses = []string{"pass"}
	}
	if len(check.AllowedStatuses) == 0 {
		check.AllowedStatuses = append([]string(nil), check.PassStatuses...)
	}
	check.PassStatuses = nonEmptyStrings(check.PassStatuses)
	check.AllowedStatuses = uniqueStrings(append(check.AllowedStatuses, check.PassStatuses...))
	check.EvidenceRequiredStatuses = nonEmptyStrings(check.EvidenceRequiredStatuses)
	check.FindingsRequiredStatuses = nonEmptyStrings(check.FindingsRequiredStatuses)
	return check
}

func (l Loader) loadAgents(root string, declared []string) ([]TeamAgentSpec, error) {
	names := append([]string(nil), declared...)
	if len(names) == 0 {
		entries, err := os.ReadDir(root)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			names = append(names, entry.Name())
		}
		sort.Strings(names)
	}
	var specs []TeamAgentSpec
	seen := map[string]struct{}{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			return nil, fmt.Errorf("duplicate team agent %q", name)
		}
		seen[name] = struct{}{}
		agentRoot := filepath.Join(root, name)
		spec, err := loadAgentSpec(agentRoot, name)
		if err != nil {
			return nil, err
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

func loadAgentSpec(root, id string) (TeamAgentSpec, error) {
	data, err := os.ReadFile(filepath.Join(root, AgentConfigFile))
	if err != nil {
		return TeamAgentSpec{}, fmt.Errorf("agent %q missing agent.yaml: %w", id, err)
	}
	var spec TeamAgentSpec
	if err := yaml.Unmarshal(data, &spec); err != nil {
		return TeamAgentSpec{}, fmt.Errorf("invalid agent.yaml for %q: %w", id, err)
	}
	if spec.Name == "" {
		spec.Name = id
	}
	if spec.Name != id {
		return TeamAgentSpec{}, fmt.Errorf("agent directory %q does not match name %q", id, spec.Name)
	}
	if spec.DisplayName == "" {
		spec.DisplayName = spec.Name
	}
	if spec.Tools == "" {
		spec.Tools = "inherit"
	}
	if spec.Model == "" {
		spec.Model = "inherit"
	}
	spec.RootDir = root
	spec.SystemPromptPath = filepath.Join(root, AgentSystemPromptFile)
	spec.SkillsDir = filepath.Join(root, "skills")
	if _, err := os.Stat(spec.SystemPromptPath); err != nil {
		return TeamAgentSpec{}, fmt.Errorf("agent %q missing system.md", id)
	}
	spec.AllowedAgents = normalizeCommunicatesWith(spec.CommunicatesWith)
	return spec, nil
}

func normalizeCommunicatesWith(value any) []string {
	if value == nil {
		return []string{"all"}
	}
	switch v := value.(type) {
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return []string{"all"}
		}
		return []string{text}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			text := strings.TrimSpace(fmt.Sprint(item))
			if text != "" {
				out = append(out, text)
			}
		}
		if len(out) == 0 {
			return []string{"all"}
		}
		return out
	case []string:
		if len(v) == 0 {
			return []string{"all"}
		}
		return append([]string(nil), v...)
	default:
		return []string{"all"}
	}
}

func expandHome(path string) string {
	if path == "~" {
		home, _ := os.UserHomeDir()
		return home
	}
	if strings.HasPrefix(path, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, path[2:])
	}
	return path
}
