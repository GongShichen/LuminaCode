package memory

import (
	"strings"
	"testing"
)

func TestProjectSpaceIsStableAndNamespaced(t *testing.T) {
	space := ProjectSpace(t.TempDir())
	if !strings.HasPrefix(space, "project:") || strings.Contains(space, " ") {
		t.Fatalf("unexpected project space %q", space)
	}
	if again := ProjectSpace(strings.TrimPrefix(space, "project:")); again == "" {
		t.Fatal("project space normalization returned an empty namespace")
	}
}
