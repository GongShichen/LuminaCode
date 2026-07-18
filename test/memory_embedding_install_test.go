package test

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMemoryEmbeddingInstallUsesModelScope(t *testing.T) {
	repoRoot := repositoryRoot(t)
	unixScript := readRepositoryFile(t, repoRoot, "scripts/setup-memory-embedding.sh")
	windowsScript := readRepositoryFile(t, repoRoot, "scripts/setup-memory-embedding-windows.ps1")

	for name, content := range map[string]string{
		"Unix":    unixScript,
		"Windows": windowsScript,
	} {
		if !strings.Contains(content, "https://modelscope.cn/models/AI-ModelScope/multilingual-e5-small/") {
			t.Fatalf("%s embedding installer does not use ModelScope", name)
		}
		if strings.Contains(content, "huggingface.co/") {
			t.Fatalf("%s embedding installer still references Hugging Face", name)
		}
		if !strings.Contains(content, "model.onnx") || !strings.Contains(content, "tokenizer.json") {
			t.Fatalf("%s embedding installer is missing model or tokenizer assets", name)
		}
	}
}

func TestMakeInstallAndUninstallManageMemoryEmbedding(t *testing.T) {
	repoRoot := repositoryRoot(t)
	makefile := readRepositoryFile(t, repoRoot, "Makefile")
	installCall := `setup-memory-embedding.sh" install`
	if !strings.Contains(makefile, installCall) {
		t.Fatalf("make install does not invoke %q", installCall)
	}
	if !strings.Contains(makefile, `"$(APP_ROOT)/cache"`) {
		t.Fatal("make uninstall does not remove the rebuildable v2 cache layer")
	}
	if !strings.Contains(makefile, "CGO_ENABLED=$(CGO_ENABLED) $(GO) build") {
		t.Fatal("make build can silently compile a backend without local embedding support")
	}
	windowsInstaller := readRepositoryFile(t, repoRoot, "scripts/install-windows.ps1")
	if !strings.Contains(windowsInstaller, `$env:CGO_ENABLED = "1"`) ||
		!strings.Contains(windowsInstaller, "Assert-Command $cCompilerName") {
		t.Fatal("Windows installer does not enforce a local-embedding-capable backend build")
	}

	if runtime.GOOS == "windows" {
		t.Skip("the Unix uninstall action is covered on Unix platforms")
	}
	appRoot := t.TempDir()
	modelDir := filepath.Join(appRoot, "cache", "models", "memory", "multilingual-e5-small")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.onnx"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", filepath.Join(repoRoot, "scripts", "setup-memory-embedding.sh"), "uninstall")
	cmd.Env = append(os.Environ(), "LUMINA_APP_ROOT="+appRoot)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("embedding uninstall failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(modelDir); !os.IsNotExist(err) {
		t.Fatalf("embedding directory still exists after uninstall: %s", modelDir)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(workingDir, ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("cannot locate repository root from %s: %v", workingDir, err)
	}
	return root
}

func readRepositoryFile(t *testing.T, repoRoot, relativePath string) string {
	t.Helper()
	content, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(relativePath)))
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}
