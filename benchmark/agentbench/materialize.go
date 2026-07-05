package agentbench

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

func MaterializeCase(ctx context.Context, c CaseSpec, baseWorkDir string) (string, error) {
	if baseWorkDir == "" {
		return "", fmt.Errorf("work dir is required")
	}
	caseDir := filepath.Join(baseWorkDir, sanitizeCaseID(c.ID))
	if err := os.RemoveAll(caseDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(caseDir, 0o755); err != nil {
		return "", err
	}
	switch {
	case c.Repo != "":
		if isGitRemote(c.Repo) {
			result := RunShellCommand(ctx, baseWorkDir, fmt.Sprintf("git clone %s %s", shellQuote(c.Repo), shellQuote(caseDir)), time.Duration(c.TimeoutSeconds)*time.Second)
			if result.ExitCode != 0 {
				return "", fmt.Errorf("git clone failed: %s%s", result.Stdout, result.Stderr)
			}
		} else if err := copyTree(c.Repo, caseDir); err != nil {
			return "", err
		}
	case c.WorkDir != "":
		if err := copyTree(c.WorkDir, caseDir); err != nil {
			return "", err
		}
	default:
		if err := materializeBuiltinCase(c, caseDir); err != nil {
			return "", err
		}
	}
	if err := ensureGitBaseline(ctx, caseDir); err != nil {
		return "", err
	}
	return caseDir, nil
}

func materializeBuiltinCase(c CaseSpec, dir string) error {
	switch c.ID {
	case "aider-polyglot-go-add":
		files := map[string]string{
			"go.mod":       "module toy\n\ngo 1.22\n",
			"calc.go":      "package toy\n\nfunc Add(a int, b int) int {\n\treturn a - b\n}\n",
			"calc_test.go": "package toy\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatalf(\"Add(2, 3) should equal 5\")\n\t}\n}\n",
		}
		return writeCaseFiles(dir, files)
	case "aider-polyglot-python-factorial":
		files := map[string]string{
			"math_utils.py":      "def factorial(n):\n    if n <= 1:\n        return 0\n    return n * factorial(n - 1)\n",
			"test_math_utils.py": "import unittest\nfrom math_utils import factorial\n\nclass MathUtilsTest(unittest.TestCase):\n    def test_factorial(self):\n        self.assertEqual(factorial(0), 1)\n        self.assertEqual(factorial(5), 120)\n\nif __name__ == '__main__':\n    unittest.main()\n",
		}
		return writeCaseFiles(dir, files)
	case "aider-polyglot-js-title":
		files := map[string]string{
			"title.js":      "function titleCase(value) {\n  return value.toLowerCase();\n}\n\nmodule.exports = { titleCase };\n",
			"title.test.js": "const assert = require('assert');\nconst { titleCase } = require('./title');\n\nassert.strictEqual(titleCase('hello world'), 'Hello World');\nassert.strictEqual(titleCase('LUMINA code'), 'Lumina Code');\n",
		}
		return writeCaseFiles(dir, files)
	case "terminal-bench-create-artifact":
		files := map[string]string{
			"README.md": "# Terminal Bench Smoke\n\nCreate result.txt with the requested contents.\n",
		}
		return writeCaseFiles(dir, files)
	default:
		return writeCaseFiles(dir, map[string]string{"README.md": "# Agent Bench Case\n"})
	}
}

func writeCaseFiles(dir string, files map[string]string) error {
	for name, content := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func copyTree(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	return filepath.WalkDir(src, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() && entry.Name() == ".git" {
			return filepath.SkipDir
		}
		target := filepath.Join(dst, rel)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.MkdirAll(target, info.Mode())
		}
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			return os.Symlink(link, target)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, info.Mode())
	})
}

func ensureGitBaseline(ctx context.Context, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		return nil
	}
	commands := []string{
		"git init",
		"git config user.name LuminaCodeAgentBench",
		"git config user.email agentbench@example.invalid",
		"git add .",
		"git commit -m init",
	}
	for _, command := range commands {
		result := RunShellCommand(ctx, dir, command, 30*time.Second)
		if result.ExitCode != 0 {
			return fmt.Errorf("%s failed: %s%s", command, result.Stdout, result.Stderr)
		}
	}
	return nil
}

func isGitRemote(value string) bool {
	return strings.Contains(value, "://") || strings.HasPrefix(value, "git@")
}

var unsafeCaseID = regexp.MustCompile(`[^A-Za-z0-9_.-]+`)

func sanitizeCaseID(id string) string {
	clean := unsafeCaseID.ReplaceAllString(id, "-")
	clean = strings.Trim(clean, "-")
	if clean == "" {
		return "case"
	}
	return clean
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
