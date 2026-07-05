package test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"LuminaCode/agent"
)

func TestFindGitRootMatchesPythonWorktreeManager(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	runGit(t, repo, "init")
	subdir := filepath.Join(repo, "sub", "deep")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}
	wantRoot := canonicalTestPath(t, repo)
	if got := canonicalTestPath(t, agent.FindGitRoot(repo)); got != wantRoot {
		t.Fatalf("repo root lookup mismatch: got %q want %q", got, repo)
	}
	if got := canonicalTestPath(t, agent.FindGitRoot(subdir)); got != wantRoot {
		t.Fatalf("subdir root lookup mismatch: got %q want %q", got, repo)
	}
	nonGit := t.TempDir()
	if got := agent.FindGitRoot(nonGit); got != "" {
		t.Fatalf("non-git directory should not have a git root, got %q", got)
	}
}

func TestWorktreeRemoveIsIdempotentAndIndependentLikePython(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	repo := t.TempDir()
	runGit(t, repo, "init")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test User")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("# Test"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", "README.md")
	runGit(t, repo, "commit", "-m", "initial")

	first, err := agent.CreateWorktree(context.Background(), repo, "HEAD", "agent-a", ".Lumina/worktrees")
	if err != nil {
		t.Fatal(err)
	}
	second, err := agent.CreateWorktree(context.Background(), repo, "HEAD", "agent-b", ".Lumina/worktrees")
	if err != nil {
		t.Fatal(err)
	}
	if first.WorktreePath == "" || second.WorktreePath == "" || first.WorktreePath == second.WorktreePath {
		t.Fatalf("expected two independent worktrees, got %#v %#v", first, second)
	}
	defer agent.RemoveWorktree(context.Background(), first.WorktreePath)
	defer agent.RemoveWorktree(context.Background(), second.WorktreePath)

	if err := os.WriteFile(filepath.Join(first.WorktreePath, "from_a.txt"), []byte("hello from A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second.WorktreePath, "from_b.txt"), []byte("hello from B"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(first.WorktreePath, "from_b.txt")); !os.IsNotExist(err) {
		t.Fatalf("first worktree should not see second worktree file, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(second.WorktreePath, "from_a.txt")); !os.IsNotExist(err) {
		t.Fatalf("second worktree should not see first worktree file, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "from_a.txt")); !os.IsNotExist(err) {
		t.Fatalf("parent repo should not see first worktree file, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, "from_b.txt")); !os.IsNotExist(err) {
		t.Fatalf("parent repo should not see second worktree file, err=%v", err)
	}

	if err := agent.RemoveWorktree(context.Background(), first.WorktreePath); err != nil {
		t.Fatal(err)
	}
	if err := agent.RemoveWorktree(context.Background(), first.WorktreePath); err != nil {
		t.Fatalf("second remove should be idempotent: %v", err)
	}
	if err := agent.RemoveWorktree(context.Background(), ""); err != nil {
		t.Fatalf("empty remove should be a no-op: %v", err)
	}
	if _, err := os.Stat(first.WorktreePath); !os.IsNotExist(err) {
		t.Fatalf("first worktree should be removed, err=%v", err)
	}
	if list := string(runGitOutput(t, repo, "worktree", "list", "--porcelain")); strings.Contains(list, first.WorktreePath) {
		t.Fatalf("removed worktree should not remain in git worktree list:\n%s", list)
	}
}

func runGitOutput(t *testing.T, dir string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v failed: %v\n%s", args, err, out)
	}
	return out
}

func canonicalTestPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return filepath.Clean(path)
	}
	return filepath.Clean(resolved)
}
