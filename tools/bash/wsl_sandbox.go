package bash

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	wslProbeTimeout   = 15 * time.Second
	wslImportTimeout  = 15 * time.Minute
	wslSandboxTmpName = "LuminaSandbox.wsl"
)

type WSLSetupStatus struct {
	Needed             bool
	Reason             string
	Distro             string
	ImageURL           string
	ImagePath          string
	CanInstall         bool
	AllowLocalFallback bool
}

func (m *SandboxManager) WSLSetupStatus(command string, dangerouslyDisableSandbox bool) WSLSetupStatus {
	status := WSLSetupStatus{
		Distro:             m.WSLDistro(),
		ImageURL:           strings.TrimSpace(m.options.WSLImageURL),
		ImagePath:          strings.TrimSpace(m.options.WSLImagePath),
		CanInstall:         strings.TrimSpace(m.options.WSLImageURL) != "" || strings.TrimSpace(m.options.WSLImagePath) != "",
		AllowLocalFallback: m.AllowLocalFallback(),
	}
	if !m.NeedsSetup(command, dangerouslyDisableSandbox) {
		return status
	}
	status.Needed = true
	if !m.wslAvailable() {
		status.Reason = "wsl.exe was not found. Install WSL2 first, then retry."
		status.CanInstall = false
		return status
	}
	if !m.wslDistroExists(status.Distro) {
		status.Reason = "WSL sandbox distro is not installed."
		return status
	}
	status.Reason = "WSL sandbox distro exists but is missing bash or bwrap."
	return status
}

func (m *SandboxManager) InstallWSLSandbox(ctx context.Context) error {
	if m == nil || m.platform != "windows" {
		return fmt.Errorf("WSL sandbox installation is only supported on Windows")
	}
	if m.wslSandboxReady() {
		return nil
	}
	wsl, err := exec.LookPath("wsl.exe")
	if err != nil {
		if wsl, err = exec.LookPath("wsl"); err != nil {
			return fmt.Errorf("wsl.exe was not found. Install WSL2 first, then retry")
		}
	}
	imagePath, err := m.resolveWSLImage(ctx)
	if err != nil {
		return err
	}
	installDir := strings.TrimSpace(m.options.WSLInstallDir)
	if installDir == "" {
		return fmt.Errorf("wsl sandbox install directory is empty")
	}
	if err := os.MkdirAll(filepath.Dir(installDir), 0o755); err != nil {
		return fmt.Errorf("create WSL sandbox parent directory: %w", err)
	}
	importCtx, cancel := context.WithTimeout(ctx, wslImportTimeout)
	defer cancel()
	cmd := exec.CommandContext(importCtx, wsl, "--import", m.WSLDistro(), installDir, imagePath, "--version", "2")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("import WSL sandbox distro %q: %w\n%s", m.WSLDistro(), err, strings.TrimSpace(normalizeWSLOutput(output)))
	}
	if !m.wslSandboxReady() {
		return fmt.Errorf("WSL sandbox distro %q was imported, but bash or bwrap is not available inside it", m.WSLDistro())
	}
	return nil
}

func (m *SandboxManager) resolveWSLImage(ctx context.Context) (string, error) {
	if imagePath := strings.TrimSpace(m.options.WSLImagePath); imagePath != "" {
		if err := verifyFileSHA256(imagePath, m.options.WSLImageSHA256); err != nil {
			return "", err
		}
		return imagePath, nil
	}
	imageURL := strings.TrimSpace(m.options.WSLImageURL)
	if imageURL == "" {
		return "", fmt.Errorf("no WSL sandbox image is configured. Set wsl_sandbox_image_path or wsl_sandbox_image_url")
	}
	installDir := strings.TrimSpace(m.options.WSLInstallDir)
	if installDir == "" {
		return "", fmt.Errorf("wsl sandbox install directory is empty")
	}
	cacheDir := filepath.Join(filepath.Dir(installDir), "images")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("create WSL image cache directory: %w", err)
	}
	imagePath := filepath.Join(cacheDir, wslSandboxTmpName)
	if _, err := os.Stat(imagePath); err == nil {
		if err := verifyFileSHA256(imagePath, m.options.WSLImageSHA256); err == nil {
			return imagePath, nil
		}
		_ = os.Remove(imagePath)
	}
	if err := downloadFile(ctx, imageURL, imagePath); err != nil {
		return "", err
	}
	if err := verifyFileSHA256(imagePath, m.options.WSLImageSHA256); err != nil {
		_ = os.Remove(imagePath)
		return "", err
	}
	return imagePath, nil
}

func (m *SandboxManager) wslSandbox(command string, config SandboxConfig, cwd string) []string {
	wsl, err := exec.LookPath("wsl.exe")
	if err != nil {
		if wsl, err = exec.LookPath("wsl"); err != nil {
			return nil
		}
	}
	wslCWD, err := WindowsPathToWSLPath(cwd)
	if err != nil {
		return []string{"cmd", "/C", "echo Lumina WSL sandbox path conversion failed: " + err.Error() + " && exit 1"}
	}
	wslConfig := translateSandboxConfigToWSL(config)
	if len(wslConfig.AllowRead) == 0 {
		wslConfig.AllowRead = []string{wslCWD}
	}
	if len(wslConfig.AllowWrite) == 0 {
		wslConfig.AllowWrite = []string{wslCWD}
	}
	allowed := map[string]struct{}{wslCWD: {}}
	for _, path := range append(append([]string{}, wslConfig.AllowRead...), wslConfig.AllowWrite...) {
		allowed[path] = struct{}{}
	}
	allowedPaths := make([]string, 0, len(allowed))
	for path := range allowed {
		allowedPaths = append(allowedPaths, path)
	}
	allowedExistingPaths := m.wslExistingPaths(wsl, allowedPaths)
	exists := func(path string) bool {
		_, ok := allowedExistingPaths[path]
		return ok
	}
	args := []string{"-d", m.WSLDistro(), "--cd", wslCWD, "--"}
	args = append(args, linuxSandboxArgs(command, wslConfig, wslCWD, exists)...)
	return append([]string{wsl}, args...)
}

func (m *SandboxManager) wslExistingPaths(wsl string, paths []string) map[string]struct{} {
	result := map[string]struct{}{}
	if strings.TrimSpace(wsl) == "" || len(paths) == 0 {
		return result
	}
	requested := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path != "" {
			requested[path] = struct{}{}
		}
	}
	script := `for p in "$@"; do [ -e "$p" ] && printf '%s\n' "$p"; done`
	args := []string{"-d", m.WSLDistro(), "--", "bash", "-lc", script, "bash"}
	args = append(args, paths...)
	ctx, cancel := context.WithTimeout(context.Background(), wslProbeTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, wsl, args...).Output()
	if err != nil {
		for _, path := range []string{"/usr", "/bin", "/etc", "/lib"} {
			if _, ok := requested[path]; ok {
				result[path] = struct{}{}
			}
		}
		return result
	}
	for _, line := range strings.Split(normalizeWSLOutput(output), "\n") {
		path := strings.TrimSpace(line)
		if _, ok := requested[path]; ok {
			result[path] = struct{}{}
		}
	}
	return result
}

func translateSandboxConfigToWSL(config SandboxConfig) SandboxConfig {
	out := config
	out.AllowRead = translatePathListToWSL(config.AllowRead)
	out.AllowWrite = translatePathListToWSL(config.AllowWrite)
	return out
}

func translatePathListToWSL(paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		translated, err := WindowsPathToWSLPath(path)
		if err == nil && translated != "" {
			out = append(out, translated)
		}
	}
	return out
}

func (m *SandboxManager) wslSandboxReady() bool {
	if m == nil || m.platform != "windows" || !m.wslAvailable() {
		return false
	}
	distro := m.WSLDistro()
	if !m.wslDistroExists(distro) {
		return false
	}
	return m.wslCommandSucceeds(distro, "command -v bash >/dev/null 2>&1 && command -v bwrap >/dev/null 2>&1")
}

func (m *SandboxManager) wslAvailable() bool {
	if _, err := exec.LookPath("wsl.exe"); err == nil {
		return true
	}
	_, err := exec.LookPath("wsl")
	return err == nil
}

func (m *SandboxManager) wslDistroExists(distro string) bool {
	if strings.TrimSpace(distro) == "" {
		return false
	}
	wsl, err := exec.LookPath("wsl.exe")
	if err != nil {
		if wsl, err = exec.LookPath("wsl"); err != nil {
			return false
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), wslProbeTimeout)
	defer cancel()
	output, err := exec.CommandContext(ctx, wsl, "--list", "--quiet").Output()
	if err != nil {
		return false
	}
	want := strings.ToLower(strings.TrimSpace(distro))
	for _, line := range strings.Split(normalizeWSLOutput(output), "\n") {
		if strings.ToLower(strings.TrimSpace(strings.TrimSuffix(line, "*"))) == want {
			return true
		}
	}
	return false
}

func (m *SandboxManager) wslCommandSucceeds(distro, command string) bool {
	wsl, err := exec.LookPath("wsl.exe")
	if err != nil {
		if wsl, err = exec.LookPath("wsl"); err != nil {
			return false
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), wslProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, wsl, "-d", distro, "--", "bash", "-lc", command)
	return cmd.Run() == nil
}

func WindowsPathToWSLPath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", fmt.Errorf("empty path")
	}
	if strings.HasPrefix(path, "/") {
		return filepath.ToSlash(path), nil
	}
	unc := strings.TrimPrefix(path, `\\`)
	uncParts := strings.Split(unc, `\`)
	if len(uncParts) >= 3 && (strings.EqualFold(uncParts[0], "wsl$") || strings.EqualFold(uncParts[0], "wsl.localhost")) {
		return "/" + strings.Join(uncParts[2:], "/"), nil
	}
	normalized := strings.ReplaceAll(path, "/", `\`)
	if len(normalized) >= 2 && normalized[1] == ':' {
		drive := strings.ToLower(string(normalized[0]))
		rest := strings.TrimLeft(normalized[2:], `\`)
		if rest == "" {
			return "/mnt/" + drive, nil
		}
		return "/mnt/" + drive + "/" + strings.ReplaceAll(rest, `\`, "/"), nil
	}
	return "", fmt.Errorf("unsupported Windows path %q", path)
}

func normalizeWSLOutput(data []byte) string {
	if len(data) >= 2 && looksUTF16LE(data) {
		u16 := make([]uint16, 0, len(data)/2)
		for i := 0; i+1 < len(data); i += 2 {
			u16 = append(u16, uint16(data[i])|uint16(data[i+1])<<8)
		}
		return cleanupWSLText(string(utf16.Decode(u16)))
	}
	return cleanupWSLText(string(data))
}

func looksUTF16LE(data []byte) bool {
	if len(data) >= 2 && data[0] == 0xff && data[1] == 0xfe {
		return true
	}
	if len(data) < 4 {
		return false
	}
	samples := 0
	nuls := 0
	for i := 1; i < len(data) && samples < 32; i += 2 {
		samples++
		if data[i] == 0 {
			nuls++
		}
	}
	return samples > 0 && nuls*2 >= samples
}

func cleanupWSLText(text string) string {
	text = strings.ReplaceAll(text, "\ufeff", "")
	text = strings.ReplaceAll(text, "\x00", "")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return text
}

func downloadFile(ctx context.Context, url, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("create WSL sandbox image request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("download WSL sandbox image: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download WSL sandbox image: unexpected HTTP status %s", resp.Status)
	}
	tmp := path + ".part"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create WSL sandbox image file: %w", err)
	}
	_, copyErr := io.Copy(out, resp.Body)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write WSL sandbox image file: %w", copyErr)
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close WSL sandbox image file: %w", closeErr)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("save WSL sandbox image file: %w", err)
	}
	return nil
}

func verifyFileSHA256(path, want string) error {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return nil
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open WSL sandbox image for checksum: %w", err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return fmt.Errorf("hash WSL sandbox image: %w", err)
	}
	got := hex.EncodeToString(hash.Sum(nil))
	if !bytes.Equal([]byte(got), []byte(want)) {
		return fmt.Errorf("WSL sandbox image sha256 mismatch: got %s, want %s", got, want)
	}
	return nil
}
