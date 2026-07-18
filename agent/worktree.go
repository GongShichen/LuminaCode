package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"LuminaCode/apppaths"

	"github.com/google/uuid"
)

type WorktreeResult struct {
	RepoRoot     string
	WorktreePath string
	BaseRef      string
	BranchName   string
}

type WorktreeManager struct{}

func FindGitRoot(cwd string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "rev-parse", "--show-toplevel")
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return filepath.Clean(stringTrimSpace(out))
}

func CreateWorktree(ctx context.Context, repoRoot, baseRef, agentType, worktreesDir string) (WorktreeResult, error) {
	if baseRef == "" {
		baseRef = "HEAD"
	}
	if worktreesDir == "" {
		worktreesDir = filepath.Join(apppaths.ProjectLocalDirName, apppaths.ProjectWorktreesDirName)
	}
	result := WorktreeResult{RepoRoot: repoRoot, BaseRef: baseRef}
	if !filepath.IsAbs(worktreesDir) {
		worktreesDir = filepath.Join(repoRoot, worktreesDir)
	}
	if err := os.MkdirAll(worktreesDir, 0o755); err != nil {
		return result, nil
	}
	suffix := uuid.NewString()[:8]
	path := filepath.Join(worktreesDir, agentType+"-"+suffix)
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "git", "worktree", "add", path, baseRef)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = removeDirIfEmpty(path)
		_ = out
		return result, nil
	}
	result.WorktreePath = path
	return result, nil
}

func RemoveWorktree(ctx context.Context, path string) error {
	if path == "" {
		return nil
	}
	repoRoot := FindGitRoot(path)
	if repoRoot == "" {
		_ = os.RemoveAll(path)
		return nil
	}
	cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cmdCtx, "git", "worktree", "remove", "--force", path)
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		_ = os.RemoveAll(path)
		_ = out
		return nil
	}
	pruneCtx, pruneCancel := context.WithTimeout(ctx, 10*time.Second)
	defer pruneCancel()
	prune := exec.CommandContext(pruneCtx, "git", "worktree", "prune")
	prune.Dir = repoRoot
	_ = prune.Run()
	_ = os.RemoveAll(path)
	return nil
}

func stringTrimSpace(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

func removeDirIfEmpty(path string) error {
	return os.Remove(path)
}
