package team

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"LuminaCode/config"

	"gopkg.in/yaml.v3"
)

const (
	TeamConfigFile        = "team.yaml"
	AgentConfigFile       = "agent.yaml"
	AgentSystemPromptFile = "system.md"
	TeamSystemFile        = "team-system.md"
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
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lumina", "TEAM")
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
	root := l.TeamDir()
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, nil
	}
	sort.Slice(entries, func(i, j int) bool { return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name()) })
	var specs []TeamSpec
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		spec, err := l.Load(entry.Name())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		specs = append(specs, spec)
	}
	return specs, errs
}

func (l Loader) Load(name string) (TeamSpec, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return TeamSpec{}, fmt.Errorf("team name is required")
	}
	root := filepath.Join(l.TeamDir(), name)
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
	if spec.EntryAgent == "" {
		spec.EntryAgent = "team-leader"
	}
	if spec.Loop.StopPolicy == "" {
		spec.Loop.StopPolicy = "user_interrupt_or_task_complete_only"
	}
	if spec.Loop.MaxParallelAgents <= 0 {
		spec.Loop.MaxParallelAgents = 4
	}
	spec.RootDir = root
	spec.TeamSystemPath = filepath.Join(root, TeamSystemFile)
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
	spec.Gates = normalizeGateSpec(spec.Gates, spec)
	if spec.Gates.QAAgent != "" {
		if _, ok := spec.AgentMap[spec.Gates.QAAgent]; !ok {
			return TeamSpec{}, fmt.Errorf("team %q gates.qa_agent %q is not defined", name, spec.Gates.QAAgent)
		}
	}
	if spec.Gates.ReviewerAgent != "" {
		if _, ok := spec.AgentMap[spec.Gates.ReviewerAgent]; !ok {
			return TeamSpec{}, fmt.Errorf("team %q gates.reviewer_agent %q is not defined", name, spec.Gates.ReviewerAgent)
		}
	}
	return spec, nil
}

func normalizeGateSpec(gates TeamGateSpec, spec TeamSpec) TeamGateSpec {
	if gates.NonblockingFindings == "" {
		gates.NonblockingFindings = "allow_complete"
	}
	legacyReviewerQA := spec.Loop.CompletionPolicy == "leader_with_reviewer_and_qa"
	if legacyReviewerQA {
		if gates.QAAgent == "" {
			gates.QAAgent = "qa"
		}
		if gates.ReviewerAgent == "" {
			gates.ReviewerAgent = "reviewer"
		}
		gates.RequireContract = true
	}
	return gates
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
