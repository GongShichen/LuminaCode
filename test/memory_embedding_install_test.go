package test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMemoryModelInstallUsesPinnedModelScopeAssets(t *testing.T) {
	repoRoot := repositoryRoot(t)
	unixScript := readRepositoryFile(t, repoRoot, "scripts/setup-memory-models.sh")
	windowsScript := readRepositoryFile(t, repoRoot, "scripts/setup-memory-models-windows.ps1")
	lock := readRepositoryFile(t, repoRoot, "scripts/memory-models.lock")

	for name, content := range map[string]string{
		"Unix":    unixScript,
		"Windows": windowsScript,
	} {
		if !strings.Contains(content, "modelscope.cn") || !strings.Contains(content, "resolve/") {
			t.Fatalf("%s model installer does not use the ModelScope resolve protocol", name)
		}
		if strings.Contains(content, "huggingface.co/") {
			t.Fatalf("%s model installer references Hugging Face", name)
		}
		if !strings.Contains(content, ".staging") || !strings.Contains(content, "5 GiB") {
			t.Fatalf("%s model installer lacks staging or disk-space checks", name)
		}
		if strings.Contains(content, "legacy_runtime_root") || strings.Contains(content, "$legacyRuntime") {
			t.Fatalf("%s model installer can still reuse the retired E5 runtime", name)
		}
	}
	for _, required := range []string{
		"BAAI/bge-m3|e44369c5623cc146f016da906583db4ee0e3488d",
		"Xenova/bge-m3|3fa3a927e7bc973ae751a8add34455b52d915ac0",
		"sentence_transformers_int8.onnx|onnx/model.onnx|568599071|e5b42096865cef734247dd8c7a44e35d305b70ba0c3736758065e1cad50f6453|cpu-int8",
		"sentence_transformers_fp16.onnx|onnx/model.onnx|1134137521|06ee3dee19cfc2e01a6cc44e3d97ac787dfa2ae554373638ff024f1ae4593970|accelerator-fp16",
		"mlx-community/bge-m3-mlx-8bit|8273f354c222397b0ffeb5f053d3e810aa485ce9",
		"model.safetensors|metal/model.safetensors|603620090|57b597e5aa8c102c2698cc0915760a839e4c1a30bff0a46d7883d27d3f010720|metal-int8",
		"colbert_linear.pt", "sparse_linear.pt",
	} {
		if !strings.Contains(lock, required) {
			t.Fatalf("model lock is missing %q", required)
		}
	}
	if strings.Contains(strings.ToLower(lock), "multilingual-e5") {
		t.Fatal("model lock still contains the retired E5 model")
	}
	if strings.Contains(lock, "model.onnx_data") {
		t.Fatal("model lock still downloads the retired external-data FP32 graph")
	}
}

func TestMakeInstallAndUninstallManageMemoryModels(t *testing.T) {
	repoRoot := repositoryRoot(t)
	makefile := readRepositoryFile(t, repoRoot, "Makefile")
	preflightScript := readRepositoryFile(t, repoRoot, "scripts/install-preflight.sh")
	installCall := `setup-memory-models.sh install`
	if !strings.Contains(makefile, installCall) {
		t.Fatalf("make install does not invoke %q", installCall)
	}
	if strings.Index(makefile, installCall) > strings.Index(makefile, `scripts/install-app-layout.sh "$(APP_ROOT)"`) {
		t.Fatal("make install downloads models after the app swap")
	}
	preflightCall := `scripts/install-preflight.sh`
	buildCall := `$(MAKE) build`
	if !strings.Contains(makefile, preflightCall) || !strings.Contains(makefile, buildCall) ||
		strings.Index(makefile, preflightCall) > strings.Index(makefile, buildCall) {
		t.Fatal("make install does not run hardware/model preflight before the build")
	}
	for _, required := range []string{
		"setup-memory-models.sh\" \"$model_action\"",
		"platform:",
		"accelerator:",
		"toolchain:",
		"xcodebuild -downloadComponent MetalToolchain",
		"xcrun --sdk macosx metal -v",
	} {
		if !strings.Contains(preflightScript, required) {
			t.Fatalf("Unix install preflight is missing %q", required)
		}
	}
	installWrapper := readRepositoryFile(t, repoRoot, "scripts/install.sh")
	for _, required := range []string{
		"hardware and model preflight",
		"application and native runtime build",
		"model publication and atomic deployment",
		"LuminaCode installation failed.",
		"exit code:",
		"error:",
		"log:",
	} {
		if !strings.Contains(installWrapper, required) {
			t.Fatalf("Unix install error reporting is missing %q", required)
		}
	}
	if !strings.Contains(makefile, `"$(APP_ROOT)/cache"`) {
		t.Fatal("make uninstall does not remove the rebuildable model cache layer")
	}
	if !strings.Contains(makefile, "CGO_ENABLED=$(CGO_ENABLED) $(GO) build") {
		t.Fatal("make build can silently compile a backend without local embedding support")
	}
	if !strings.Contains(makefile, "sh scripts/build-bge-metal.sh") {
		t.Fatal("make install does not build the native Metal BGE runtime")
	}
	if !strings.Contains(makefile, `SKIP_MEMORY_MODELS)" = "1" ]; then LUMINA_APP_ROOT`) ||
		!strings.Contains(makefile, `scripts/setup-memory-models.sh doctor; models_status=verified-preinstalled`) {
		t.Fatal("SKIP_MEMORY_MODELS can bypass validation of the mandatory preinstalled BGE model")
	}
	windowsInstaller := readRepositoryFile(t, repoRoot, "scripts/install-windows.ps1")
	if !strings.Contains(windowsInstaller, `$env:CGO_ENABLED = "1"`) ||
		!strings.Contains(windowsInstaller, "Assert-Command $cCompilerName") {
		t.Fatal("Windows installer does not enforce a local-embedding-capable backend build")
	}
	if strings.Index(windowsInstaller, `-Action preflight`) < 0 ||
		strings.Index(windowsInstaller, `-Action preflight`) > strings.Index(windowsInstaller, `build frontend`) {
		t.Fatal("Windows installer does not run model preflight before its first build")
	}
	for _, required := range []string{
		`$installStage`,
		`Start-Transcript`,
		`LuminaCode installation failed.`,
		`error: $($_.Exception.Message)`,
		`log: $installLog`,
	} {
		if !strings.Contains(windowsInstaller, required) {
			t.Fatalf("Windows install error reporting is missing %q", required)
		}
	}

	if runtime.GOOS == "windows" {
		t.Skip("the Unix uninstall action is covered on Unix platforms")
	}
	appRoot := t.TempDir()
	modelDir := filepath.Join(appRoot, "cache", "models", "memory", "bge-m3")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.onnx"), []byte("test"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("sh", filepath.Join(repoRoot, "scripts", "setup-memory-models.sh"), "uninstall")
	cmd.Env = append(os.Environ(), "LUMINA_APP_ROOT="+appRoot)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("model uninstall failed: %v\n%s", err, output)
	}
	if _, err := os.Stat(modelDir); !os.IsNotExist(err) {
		t.Fatalf("model directory still exists after uninstall: %s", modelDir)
	}
}

func TestUnixInstallWrapperReportsTheFailedStage(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix install wrapper")
	}
	repoRoot := repositoryRoot(t)
	temp := t.TempDir()
	fakeMake := filepath.Join(temp, "fake-make")
	script := `#!/bin/sh
case "$2" in
  _install-preflight) exit 0 ;;
  _install-build)
    echo "synthetic build failure: checksum mismatch" >&2
    echo "make: *** [Makefile:77: _install-build] Error 23" >&2
    exit 23
    ;;
  *) exit 99 ;;
esac
`
	if err := os.WriteFile(fakeMake, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", filepath.Join(repoRoot, "scripts", "install.sh"))
	cmd.Env = append(os.Environ(),
		"MAKE="+fakeMake,
		"LUMINA_INSTALL_LOG_DIR="+filepath.Join(temp, "logs"),
	)
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("install wrapper unexpectedly succeeded:\n%s", output)
	}
	text := string(output)
	for _, required := range []string{
		"synthetic build failure: checksum mismatch",
		"stage: application and native runtime build",
		"exit code: 23",
		"error: synthetic build failure: checksum mismatch",
		"log:",
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("install failure output is missing %q:\n%s", required, text)
		}
	}
	logs, globErr := filepath.Glob(filepath.Join(temp, "logs", "install-*.log"))
	if globErr != nil || len(logs) != 1 {
		t.Fatalf("expected one install log, got %v (err=%v)", logs, globErr)
	}
	logData, readErr := os.ReadFile(logs[0])
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(logData), "synthetic build failure: checksum mismatch") {
		t.Fatalf("install log omitted the original error:\n%s", logData)
	}
	temporaryLogs, globErr := filepath.Glob(filepath.Join(temp, "logs", ".install-*"))
	if globErr != nil {
		t.Fatal(globErr)
	}
	if len(temporaryLogs) != 0 {
		t.Fatalf("install wrapper left temporary status files behind: %v", temporaryLogs)
	}
}

func TestMemoryModelInstallerResumesAndPublishesAtomically(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix installer fixture")
	}
	repoRoot := repositoryRoot(t)
	payload := bytes.Repeat([]byte("modelscope-fixture-"), 64*1024)
	var mu sync.Mutex
	requests := 0
	ranged := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/models/fixture/bge/resolve/rev/onnx/model.onnx" {
			http.NotFound(writer, request)
			return
		}
		mu.Lock()
		requests++
		if request.Header.Get("Range") != "" {
			ranged = true
		}
		mu.Unlock()
		http.ServeContent(writer, request, "model.onnx", time.Unix(0, 0), bytes.NewReader(payload))
	}))
	defer server.Close()

	appRoot := t.TempDir()
	lock, backend := writeModelInstallerFixture(t, appRoot, payload)
	partial := filepath.Join(appRoot, "cache", "models", ".staging", "bge-m3-rev", "onnx", "model.onnx.partial")
	if err := os.MkdirAll(filepath.Dir(partial), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partial, payload[:64*1024], 0o600); err != nil {
		t.Fatal(err)
	}
	runModelInstaller(t, repoRoot, appRoot, lock, backend, server.URL, "1", true)
	installed := filepath.Join(appRoot, "cache", "models", "memory", "bge-m3", "onnx", "model.onnx")
	content, err := os.ReadFile(installed)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(content, payload) {
		t.Fatal("published fixture does not match the locked payload")
	}
	mu.Lock()
	firstRequests, sawRange := requests, ranged
	mu.Unlock()
	if firstRequests != 1 || !sawRange {
		t.Fatalf("resume requests=%d ranged=%t, want one ranged request", firstRequests, sawRange)
	}
	runModelInstaller(t, repoRoot, appRoot, lock, backend, server.URL, "1", true)
	mu.Lock()
	secondRequests := requests
	mu.Unlock()
	if secondRequests != firstRequests {
		t.Fatalf("verified reinstall downloaded again: before=%d after=%d", firstRequests, secondRequests)
	}
	if mode := fileMode(t, filepath.Dir(installed)); mode.Perm() != 0o700 {
		t.Fatalf("model directory mode = %o, want 700", mode.Perm())
	}
	if mode := fileMode(t, installed); mode.Perm() != 0o600 {
		t.Fatalf("model file mode = %o, want 600", mode.Perm())
	}
}

func TestMemoryModelInstallerRejectsChecksumNetworkAndSpaceFailures(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix installer fixture")
	}
	repoRoot := repositoryRoot(t)
	payload := []byte("locked-model-payload")
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(writer, "corrupt-payload")
	}))

	appRoot := t.TempDir()
	lock, backend := writeModelInstallerFixture(t, appRoot, payload)
	old := filepath.Join(appRoot, "cache", "models", "memory", "bge-m3", "old-install.marker")
	if err := os.MkdirAll(filepath.Dir(old), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	runModelInstaller(t, repoRoot, appRoot, lock, backend, server.URL, "1", false)
	if content, err := os.ReadFile(old); err != nil || string(content) != "preserve" {
		t.Fatalf("checksum failure replaced the prior model: content=%q err=%v", content, err)
	}
	server.Close()

	unreachableRoot := t.TempDir()
	unreachableLock, unreachableBackend := writeModelInstallerFixture(t, unreachableRoot, payload)
	runModelInstaller(t, repoRoot, unreachableRoot, unreachableLock, unreachableBackend, server.URL, "1", false)
	if _, err := os.Stat(filepath.Join(unreachableRoot, "cache", "models", "memory", "bge-m3")); !os.IsNotExist(err) {
		t.Fatal("unreachable endpoint published a BGE model")
	}

	spaceRoot := t.TempDir()
	spaceLock, spaceBackend := writeModelInstallerFixture(t, spaceRoot, payload)
	runModelInstaller(t, repoRoot, spaceRoot, spaceLock, spaceBackend, "http://127.0.0.1:1", strconv.FormatInt(1<<62, 10), false)
	if _, err := os.Stat(filepath.Join(spaceRoot, "cache", "models", ".staging", "bge-m3-rev")); !os.IsNotExist(err) {
		t.Fatal("space preflight modified BGE staging")
	}
}

func writeModelInstallerFixture(t *testing.T, appRoot string, bgePayload []byte) (string, string) {
	t.Helper()
	lock := filepath.Join(t.TempDir(), "models.lock")
	content := fmt.Sprintf("bge-m3|fixture/bge|rev|onnx/model.onnx|onnx/model.onnx|%d|%s\n",
		len(bgePayload), contentSHA(bgePayload))
	if err := os.WriteFile(lock, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	e5Root := filepath.Join(appRoot, "cache", "models", "memory", "multilingual-e5-small")
	if err := os.MkdirAll(filepath.Join(e5Root, "runtime", "lib"), 0o700); err != nil {
		t.Fatal(err)
	}
	for path, value := range map[string][]byte{
		filepath.Join(e5Root, "runtime", "provider"):                    []byte("cpu\n"),
		filepath.Join(e5Root, "runtime", "lib", "libonnxruntime.dylib"): []byte("fixture"),
		filepath.Join(e5Root, "runtime", "lib", "libonnxruntime.so.1"):  []byte("fixture"),
	} {
		if err := os.WriteFile(path, value, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	backend := filepath.Join(t.TempDir(), "fake-backend")
	script := `#!/bin/sh
set -eu
command="$2"
model_dir="$4"
case "$command" in
  prepare-bge-heads) mkdir -p "$model_dir/heads"; : > "$model_dir/heads/ready" ;;
  verify-bge-heads) test -f "$model_dir/heads/ready" ;;
  probe-bge) test -f "$model_dir/heads/ready" ;;
  *) exit 2 ;;
esac
`
	if err := os.WriteFile(backend, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return lock, backend
}

func runModelInstaller(t *testing.T, repoRoot, appRoot, lock, backend, endpoint, minimumFree string, wantSuccess bool) {
	t.Helper()
	cmd := exec.Command("sh", filepath.Join(repoRoot, "scripts", "setup-memory-models.sh"), "install")
	cmd.Env = append(os.Environ(),
		"LUMINA_APP_ROOT="+appRoot,
		"LUMINA_MEMORY_MODELS_LOCK="+lock,
		"LUMINA_BACKEND_BIN="+backend,
		"LUMINA_MODELSCOPE_ENDPOINT="+endpoint,
		"LUMINA_MEMORY_MODELS_MIN_FREE_KIB="+minimumFree,
		"LUMINA_MODEL_DOWNLOAD_RETRIES=0",
		"LUMINA_MODEL_DOWNLOAD_RETRY_DELAY=0",
		"LUMINA_MEMORY_MODEL_VARIANT=cpu-int8",
	)
	output, err := cmd.CombinedOutput()
	if wantSuccess && err != nil {
		t.Fatalf("fixture install failed: %v\n%s", err, output)
	}
	if !wantSuccess && err == nil {
		t.Fatalf("fixture install unexpectedly succeeded:\n%s", output)
	}
}

func contentSHA(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func fileMode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode()
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
