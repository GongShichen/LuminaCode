package test

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestLuminaAssetsLayoutMatchesRenamedPythonBundle(t *testing.T) {
	root := repoRoot(t)
	required := []string{
		"LUMINA.md",
		".Lumina/CONFIG/defaults.json.example",
		".Lumina/CONFIG/mcp.json",
		".Lumina/SKILLS/commit/SKILL.md",
		".Lumina/SKILLS/jupyter-notebook/SKILL.md",
		".Lumina/SKILLS/pdf/SKILL.md",
		".Lumina/SKILLS/review/SKILL.md",
		".Lumina/SKILLS/security-best-practices/SKILL.md",
		".Lumina/SKILLS/security-threat-model/SKILL.md",
		".Lumina/SYSTEM/extraction_system.md",
		".Lumina/SYSTEM/system-prompt.md",
	}
	for _, rel := range required {
		info, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("required Lumina asset %s missing: %v", rel, err)
		}
		if info.IsDir() {
			t.Fatalf("required Lumina asset %s should be a file", rel)
		}
	}

	var got []string
	err := filepath.WalkDir(filepath.Join(root, ".Lumina"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if relSlash == ".Lumina/CONFIG/defaults.json" {
			return nil
		}
		got = append(got, relSlash)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	want := required[1:]
	sort.Strings(want)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("unexpected .Lumina asset set:\nwant:\n%s\n got:\n%s", strings.Join(want, "\n"), strings.Join(got, "\n"))
	}
}

func TestLuminaBundledPromptsUseRenamedInstructionFile(t *testing.T) {
	root := repoRoot(t)
	for _, rel := range []string{
		".Lumina/SYSTEM/system-prompt.md",
		".Lumina/SYSTEM/extraction_system.md",
	} {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		text := string(data)
		oldNames := []string{"XX" + "CODE.md", "XX" + "Code", "Xx" + "Code"}
		for _, oldName := range oldNames {
			if strings.Contains(text, oldName) {
				t.Fatalf("%s still contains old project naming", rel)
			}
		}
		if !strings.Contains(text, "LUMINA.md") {
			t.Fatalf("%s should reference LUMINA.md", rel)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	return filepath.Dir(filepath.Dir(file))
}
