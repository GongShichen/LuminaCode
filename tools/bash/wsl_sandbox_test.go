package bash

import "testing"

func TestWindowsPathToWSLPath(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "drive root", in: `C:\`, want: "/mnt/c"},
		{name: "drive path", in: `D:\File\work space\LuminaCode`, want: "/mnt/d/File/work space/LuminaCode"},
		{name: "wsl unc", in: `\\wsl.localhost\Ubuntu\home\me\repo`, want: "/home/me/repo"},
		{name: "already linux", in: `/home/me/repo`, want: "/home/me/repo"},
	}
	for _, tc := range cases {
		got, err := WindowsPathToWSLPath(tc.in)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", tc.name, err)
		}
		if got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestNormalizeSandboxBackend(t *testing.T) {
	cases := map[string]string{
		"":           "auto",
		"auto":       "auto",
		"bwrap":      "local-bwrap",
		"wsl_bwrap":  "wsl-bwrap",
		"wsl2-bwrap": "wsl-bwrap",
		"disabled":   "none",
		"unexpected": "auto",
	}
	for raw, want := range cases {
		if got := normalizeSandboxBackend(raw); got != want {
			t.Fatalf("normalizeSandboxBackend(%q)=%q want %q", raw, got, want)
		}
	}
}

func TestWSLSandboxFailsClosedWhenWSLMissing(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	manager := &SandboxManager{
		enabled:          true,
		platform:         "windows",
		sandboxAvailable: true,
		options:          SandboxOptions{Backend: "wsl-bwrap", WSLDistro: "LuminaSandbox"},
	}
	if argv := manager.wslSandbox("echo unsafe", SandboxConfig{Enabled: true}, `C:\repo`); argv != nil {
		t.Fatalf("missing wsl.exe must fail closed, got %#v", argv)
	}
}
