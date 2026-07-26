package bash

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestSandboxBackendsAndPolicy(t *testing.T) {
	darwin := &SandboxManager{enabled: true, platform: "darwin", sandboxAvailable: true}
	if darwin.BackendName() != "sandbox-exec" || !ShouldUseSandbox("git status", darwin, false, nil) {
		t.Fatalf("darwin sandbox policy mismatch: backend=%q enabled=%t", darwin.BackendName(), darwin.IsSandboxingEnabled())
	}
	for _, command := range []string{"git status", "ssh host", "docker ps", "brew list"} {
		if !ShouldUseSandbox(command, darwin, false, nil) {
			t.Fatalf("%q must not bypass the sandbox", command)
		}
	}
	if ShouldUseSandbox("git status", darwin, true, nil) {
		t.Fatal("YOLO mode should bypass OS sandbox isolation")
	}

	linux := &SandboxManager{enabled: true, platform: "linux", sandboxAvailable: true}
	if linux.BackendName() != "bwrap" || !ShouldUseSandbox("ssh host", linux, false, nil) {
		t.Fatalf("linux sandbox policy mismatch: backend=%q enabled=%t", linux.BackendName(), linux.IsSandboxingEnabled())
	}

	unavailable := &SandboxManager{enabled: true, platform: "linux", sandboxAvailable: false}
	if unavailable.GetSandboxCommand("echo unsafe", SandboxConfig{Enabled: true}, t.TempDir()) != nil {
		t.Fatal("unavailable sandbox must fail closed instead of returning a shell command")
	}
}

func TestMacOSProfileContainsRequiredIsolationRules(t *testing.T) {
	manager := &SandboxManager{enabled: true, platform: "darwin", sandboxAvailable: true}
	cwd := filepath.Join(t.TempDir(), `project "quoted"`)
	runtimeDir := filepath.Join(t.TempDir(), "runtime")
	profile := manager.buildMacosProfile(SandboxConfig{
		Enabled: true, AllowRead: []string{runtimeDir}, AllowWrite: []string{runtimeDir}, AllowNetwork: false,
	}, cwd)
	for _, want := range []string{
		`(import "system.sb")`,
		"process-fork",
		"path-ancestors " + strconv.Quote(cwd),
		"subpath " + strconv.Quote(runtimeDir),
	} {
		if !strings.Contains(profile, want) {
			t.Fatalf("macOS profile missing %q:\n%s", want, profile)
		}
	}
	if strings.Contains(profile, "(allow network*)") {
		t.Fatal("network must be denied by default")
	}
	argv := manager.GetSandboxCommand("env", SandboxConfig{Enabled: true, AllowRead: []string{cwd}, AllowWrite: []string{cwd}}, cwd)
	joined := strings.Join(argv, "\x00")
	for _, want := range []string{"sandbox-exec", "/usr/bin/env", "-i", "HOME=" + cwd} {
		if !strings.Contains(joined, want) {
			t.Fatalf("macOS argv missing %q: %#v", want, argv)
		}
	}
}

func TestLinuxSandboxCommandUsesWorkspaceAndNetworkPolicy(t *testing.T) {
	manager := &SandboxManager{enabled: true, platform: "linux", sandboxAvailable: true}
	cwd := t.TempDir()
	runtimeDir := t.TempDir()
	resolvedCWD := linuxSandboxRoots([]string{cwd})[0]
	resolvedRuntimeDir := linuxSandboxRoots([]string{runtimeDir})[0]
	argv := manager.GetSandboxCommand("echo ok", SandboxConfig{
		Enabled: true, AllowRead: []string{cwd, runtimeDir}, AllowWrite: []string{cwd, runtimeDir}, AllowNetwork: false,
	}, cwd)
	joined := strings.Join(argv, "\x00")
	for _, want := range []string{"bwrap", "--unshare-all", "--clearenv", "--bind\x00" + resolvedCWD + "\x00" + resolvedCWD, "--bind\x00" + resolvedRuntimeDir + "\x00" + resolvedRuntimeDir} {
		if !strings.Contains(joined, want) {
			t.Fatalf("linux argv missing %q: %#v", want, argv)
		}
	}
	if strings.Contains(joined, "--share-net") {
		t.Fatal("network must remain unshared by default")
	}
}

func TestLinuxSandboxSkipsMissingOptionalSystemPaths(t *testing.T) {
	cwd := "/mnt/d/File/work_space/go/LuminaCode"
	missingRuntime := "/mnt/c/Users/zhouning/.lumina/project/luminacode"
	argv := linuxSandboxArgs("echo ok", SandboxConfig{
		Enabled: true, AllowRead: []string{cwd, missingRuntime}, AllowWrite: []string{cwd, missingRuntime}, AllowNetwork: false,
	}, cwd, func(path string) bool {
		switch path {
		case "/usr", "/bin", "/etc", "/lib", cwd:
			return true
		default:
			return false
		}
	})
	joined := strings.Join(argv, "\x00")
	if strings.Contains(joined, "--ro-bind\x00/opt/homebrew\x00/opt/homebrew") {
		t.Fatalf("missing optional system path must not be bound: %#v", argv)
	}
	if !strings.Contains(joined, "--bind\x00"+cwd+"\x00"+cwd) {
		t.Fatalf("WSL cwd should stay in POSIX form, got %#v", argv)
	}
	if strings.Contains(joined, missingRuntime) {
		t.Fatalf("missing runtime path must not be bound: %#v", argv)
	}
	for i, arg := range argv {
		if arg == "--" && i+1 < len(argv) {
			if runtime.GOOS == "windows" && argv[i+1] != "/usr/bin/bash" {
				t.Fatalf("WSL sandbox command must use Linux bash, got %#v", argv[i+1:])
			}
			return
		}
	}
	t.Fatalf("sandbox argv missing command separator: %#v", argv)
}

func TestMacOSSandboxIntegration(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS sandbox integration test")
	}
	if _, err := exec.LookPath("sandbox-exec"); err != nil {
		t.Skip("sandbox-exec unavailable")
	}
	manager := &SandboxManager{enabled: true, platform: "darwin", sandboxAvailable: true}
	cwd := t.TempDir()
	runtimeDir := t.TempDir()
	outsideDir := t.TempDir()
	workspaceFile := filepath.Join(cwd, "workspace.txt")
	runtimeFile := filepath.Join(runtimeDir, "runtime.txt")
	outsideFile := filepath.Join(outsideDir, "outside.txt")
	command := fmt.Sprintf(
		"echo workspace > %s && echo runtime > %s && if echo blocked > %s; then exit 42; fi",
		shellTestQuote(workspaceFile), shellTestQuote(runtimeFile), shellTestQuote(outsideFile),
	)
	argv := manager.GetSandboxCommand(command, SandboxConfig{
		Enabled: true, AllowRead: []string{cwd, runtimeDir}, AllowWrite: []string{cwd, runtimeDir}, AllowNetwork: false,
	}, cwd)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("sandboxed command failed: %v\n%s", err, output)
	}
	for _, path := range []string{workspaceFile, runtimeFile} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("allowed write missing at %s: %v", path, err)
		}
	}
	if _, err := os.Stat(outsideFile); !os.IsNotExist(err) {
		t.Fatalf("outside write should be denied, stat err=%v", err)
	}

	if _, err := exec.LookPath("nc"); err != nil {
		t.Skip("nc unavailable for network containment probe")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	networkCommand := fmt.Sprintf("nc -z 127.0.0.1 %d", port)
	argv = manager.GetSandboxCommand(networkCommand, SandboxConfig{Enabled: true, AllowRead: []string{cwd}, AllowWrite: []string{cwd}}, cwd)
	cmd = exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	if err := cmd.Run(); err == nil {
		t.Fatal("sandboxed command unexpectedly reached a local TCP listener")
	}
}

func TestManualWSLSandboxProbe(t *testing.T) {
	if runtime.GOOS != "windows" || os.Getenv("LUMINA_RUN_WSL_SANDBOX_PROBE") != "1" {
		t.Skip("manual WSL sandbox probe")
	}
	distro := strings.TrimSpace(os.Getenv("LUMINA_WSL_SANDBOX_DISTRO"))
	if distro == "" {
		distro = "Ubuntu-22.04"
	}
	manager := &SandboxManager{
		enabled:          true,
		platform:         "windows",
		sandboxAvailable: true,
		options:          SandboxOptions{Backend: "wsl-bwrap", WSLDistro: distro, WSLAllowLocalFallback: false},
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Dir(filepath.Dir(cwd))
	t.Logf("ready=%t exists=%t command=%t", manager.wslSandboxReady(), manager.wslDistroExists(distro), manager.wslCommandSucceeds(distro, "command -v bash >/dev/null 2>&1 && command -v bwrap >/dev/null 2>&1"))
	argv := manager.wslSandbox(
		"pwd; echo HOME=$HOME; uname -s; command -v bash || true; ls -ld /bin /usr /usr/bin /usr/bin/bash || true",
		SandboxConfig{Enabled: true, AllowRead: []string{repo}, AllowWrite: []string{repo}, AllowNetwork: false},
		repo,
	)
	t.Logf("argv=%q", argv)
	if len(argv) == 0 {
		t.Fatal("empty argv")
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = repo
	output, err := cmd.CombinedOutput()
	t.Logf("output:\n%s", normalizeWSLOutput(output))
	if err != nil {
		t.Fatal(err)
	}
}

func shellTestQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
