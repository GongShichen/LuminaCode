package main

import (
	"fmt"
	"path/filepath"
	"runtime"
	"testing"

	"LuminaCode/apppaths"
)

func TestIsStaleBackendEndpointError(t *testing.T) {
	for _, message := range []string{
		"dial tcp 127.0.0.1:4451: connectex: No connection could be made because the target machine actively refused it.",
		"dial tcp 127.0.0.1:4451: connect: connection refused",
	} {
		if !isStaleBackendEndpointError(fmt.Errorf("%s", message)) {
			t.Fatalf("expected stale endpoint error for %q", message)
		}
	}
	if isStaleBackendEndpointError(fmt.Errorf("invalid backend endpoint file")) {
		t.Fatal("invalid endpoint files should still fail")
	}
}

func TestDefaultLegacySourceDoesNotMigrateHomeForCustomWindowsRoot(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows legacy source selection")
	}
	customRoot := filepath.Join(t.TempDir(), "custom-app-root")
	if got := defaultLegacySource(apppaths.AppPaths{Root: customRoot}); got != customRoot {
		t.Fatalf("custom Windows AppRoot should not default to home legacy source: got %q want %q", got, customRoot)
	}
}
