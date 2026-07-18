package longmemory

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"LuminaCode/apppaths"
)

const DefaultStoreRelativePath = "data/memory/lumina-memory.sqlite"

func DefaultStorePath() string {
	paths, err := apppaths.ResolveCurrent()
	if err != nil {
		return filepath.FromSlash(DefaultStoreRelativePath)
	}
	return paths.MemoryDB
}

func DefaultModelPath(modelName string) string {
	paths, err := apppaths.ResolveCurrent()
	if err != nil {
		return filepath.Join("cache", "models", "memory", modelName)
	}
	if modelName == "multilingual-e5-small" || strings.TrimSpace(modelName) == "" {
		return paths.MemoryModelDir
	}
	return filepath.Join(paths.ModelsDir, "memory", modelName)
}

func ExpandPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return DefaultStorePath()
	}
	if path == "~" {
		if home := userHomeDir(); home != "" {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home := userHomeDir(); home != "" {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func UserScopeKey() string {
	return "default"
}

func ProjectScopeKey(projectRoot string) string {
	root := ResolveProjectRoot(projectRoot)
	if root == "" {
		root = projectRoot
	}
	return sanitizeKey(root)
}

func CanonicalProjectScopeKey(projectRoot string) string {
	root, err := apppaths.CanonicalProjectRoot(projectRoot, runtime.GOOS)
	if err != nil || root == "" {
		root = projectRoot
	}
	return sanitizeKey(root)
}

func ResolveProjectRoot(projectRoot string) string {
	resolved, err := filepath.Abs(projectRoot)
	if err != nil {
		resolved = projectRoot
	}
	if gitRoot := findCanonicalGitRoot(resolved); gitRoot != "" {
		if abs, err := filepath.Abs(gitRoot); err == nil {
			return abs
		}
		return gitRoot
	}
	return resolved
}

func findCanonicalGitRoot(cwd string) string {
	if out := runGitRoot(cwd, 3*time.Second); out != "" {
		top, _ := filepath.Abs(out)
		gitEntry := filepath.Join(top, ".git")
		if info, err := os.Stat(gitEntry); err == nil && !info.IsDir() {
			if gitDir := parseWorktreeGitdir(gitEntry); gitDir != "" {
				resolved, _ := filepath.Abs(filepath.Join(gitDir, "..", "..", ".."))
				return resolved
			}
		}
		return top
	}
	current, err := filepath.Abs(cwd)
	if err != nil {
		return ""
	}
	for {
		gitEntry := filepath.Join(current, ".git")
		if info, err := os.Stat(gitEntry); err == nil {
			if !info.IsDir() {
				if gitDir := parseWorktreeGitdir(gitEntry); gitDir != "" {
					resolved, _ := filepath.Abs(filepath.Join(gitDir, "..", "..", ".."))
					return resolved
				}
			}
			return current
		}
		parent := filepath.Dir(current)
		if parent == current {
			return ""
		}
		current = parent
	}
}

func runGitRoot(cwd string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func parseWorktreeGitdir(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(data))
	const prefix = "gitdir:"
	if !strings.HasPrefix(strings.ToLower(line), prefix) {
		return ""
	}
	target := strings.TrimSpace(line[len(prefix):])
	if target == "" {
		return ""
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return filepath.Clean(target)
}

func TeamScopeKey(teamName string) string {
	return sanitizeKey(teamName)
}

func AgentTypeScopeKey(projectRoot, agentType string) string {
	return ProjectScopeKey(projectRoot) + "::" + sanitizeKey(agentType)
}

func TeamAgentScopeKey(teamName, agentID string) string {
	return TeamScopeKey(teamName) + "::" + sanitizeKey(agentID)
}

func StableID(scopeType ScopeType, scopeKey string, title string, content string) string {
	h := sha1.Sum([]byte(string(scopeType) + "\x00" + scopeKey + "\x00" + strings.ToLower(strings.TrimSpace(title)) + "\x00" + strings.TrimSpace(content)))
	return "mem_" + hex.EncodeToString(h[:12])
}

func RuntimeScopes(projectRoot, agentType, teamName, teamAgentID string) []Scope {
	return runtimeScopes(ProjectScopeKey(projectRoot), agentType, teamName, teamAgentID)
}

func RuntimeScopesCanonical(projectRoot, agentType, teamName, teamAgentID string) []Scope {
	return runtimeScopes(CanonicalProjectScopeKey(projectRoot), agentType, teamName, teamAgentID)
}

func runtimeScopes(projectScopeKey, agentType, teamName, teamAgentID string) []Scope {
	scopes := []Scope{
		{Type: ScopeUser, Key: UserScopeKey()},
		{Type: ScopeProject, Key: projectScopeKey},
	}
	if strings.TrimSpace(teamName) != "" {
		scopes = append(scopes, Scope{Type: ScopeTeam, Key: TeamScopeKey(teamName)})
	}
	if strings.TrimSpace(agentType) != "" {
		scopes = append(scopes, Scope{Type: ScopeAgentType, Key: projectScopeKey + "::" + sanitizeKey(agentType)})
	}
	if strings.TrimSpace(teamName) != "" && strings.TrimSpace(teamAgentID) != "" {
		scopes = append(scopes, Scope{Type: ScopeTeamAgent, Key: TeamAgentScopeKey(teamName, teamAgentID)})
	}
	return scopes
}

func sanitizeKey(text string) string {
	text = strings.ToLower(strings.TrimSpace(filepath.ToSlash(text)))
	text = regexp.MustCompile(`[^a-z0-9._/-]+`).ReplaceAllString(text, "-")
	text = strings.ReplaceAll(text, "/", "--")
	text = regexp.MustCompile(`-{2,}`).ReplaceAllString(text, "-")
	text = strings.Trim(text, ".-_")
	if text == "" {
		return "default"
	}
	return text
}

func userHomeDir() string {
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return home
	}
	home, _ := os.UserHomeDir()
	return home
}
