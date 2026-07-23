package memory

import (
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"LuminaCode/apppaths"
)

var projectSpaceInvalid = regexp.MustCompile(`[^a-z0-9._/-]+`)
var projectSpaceSeparators = regexp.MustCompile(`-{2,}`)

// ProjectSpace returns the stable namespace used by Fabric for one canonical
// project. It deliberately contains no legacy store or scope semantics.
func ProjectSpace(projectRoot string) string {
	root, err := apppaths.CanonicalProjectRoot(projectRoot, runtime.GOOS)
	if err != nil || strings.TrimSpace(root) == "" {
		root = projectRoot
	}
	key := strings.ToLower(strings.TrimSpace(filepath.ToSlash(root)))
	key = projectSpaceInvalid.ReplaceAllString(key, "-")
	key = strings.ReplaceAll(key, "/", "--")
	key = projectSpaceSeparators.ReplaceAllString(key, "-")
	key = strings.Trim(key, ".-_")
	if key == "" {
		key = "default"
	}
	return "project:" + key
}
