package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func FindCanonicalGitRoot(cwd string) string {
	if out := runGit([]string{"rev-parse", "--show-toplevel"}, cwd, 3*time.Second); out != "" {
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

func SanitizeGitRootForPath(gitRoot string) string {
	resolved, err := filepath.Abs(gitRoot)
	if err != nil {
		resolved = gitRoot
	}
	sum := sha256.Sum256([]byte(resolved))
	return hex.EncodeToString(sum[:])[:16]
}

func runGit(args []string, cwd string, timeout time.Duration) string {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func parseWorktreeGitdir(gitFile string) string {
	data, err := os.ReadFile(gitFile)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "gitdir:") {
			return strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		}
	}
	return ""
}
