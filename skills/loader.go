package skills

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"LuminaCode/apppaths"
	"LuminaCode/config"
)

const SkillFilename = "SKILL.md"

type SkillLoader struct {
	Config    config.Config
	seenPaths map[string]struct{}
}

func NewSkillLoader(cfg config.Config) *SkillLoader {
	return &SkillLoader{Config: cfg, seenPaths: map[string]struct{}{}}
}

func (l *SkillLoader) LoadFrontmatterOnly() []SkillSpec {
	l.seenPaths = map[string]struct{}{}
	var skills []SkillSpec
	if l.Config.IsolatedSkillsOnly {
		return append(skills, l.loadDirectory(l.UserSkillsDir(), SkillSourceUser)...)
	}
	for _, root := range l.ProjectSkillsDirs() {
		skills = append(skills, l.loadDirectory(root, SkillSourceProject)...)
	}
	skills = append(skills, l.loadDirectory(l.UserSkillsDir(), SkillSourceUser)...)
	skills = append(skills, l.loadDirectory(l.BundledSkillsDir(), SkillSourceBundled)...)
	return skills
}

func (l *SkillLoader) LoadFullContent(skill SkillSpec) SkillSpec {
	if skill.Content != nil || skill.SkillFile == "" {
		return skill
	}
	fm, content, err := ParseSkillMD(skill.SkillFile)
	if err != nil {
		return skill
	}
	skill.Frontmatter = fm
	skill.Content = &content
	skill.LoadedAt = loadedNow()
	return skill
}

func (l *SkillLoader) ProjectSkillsDir() string {
	return filepath.Join(l.projectRoot(), l.Config.SkillsDir)
}

func (l *SkillLoader) ProjectSkillsDirs() []string {
	root := l.projectRoot()
	roots := []string{filepath.Join(root, "skills")}
	if l.Config.SkillsDir != "" {
		roots = append(roots, l.ProjectSkillsDir())
	}
	return uniqueExistingOrder(roots)
}

func (l *SkillLoader) projectRoot() string {
	if strings.TrimSpace(l.Config.ProjectPaths.CanonicalRoot) != "" {
		return l.Config.ProjectPaths.CanonicalRoot
	}
	return l.Config.CWD
}

func (l *SkillLoader) UserSkillsDir() string {
	return expandSkillHome(l.Config.UserSkillsDir)
}

func (l *SkillLoader) BundledSkillsDir() string {
	if l.Config.BundledSkillsDir != "" {
		return l.Config.BundledSkillsDir
	}
	return apppaths.ProjectBundledSkillsDir(l.Config.CWD)
}

func (l *SkillLoader) loadDirectory(root string, source SkillSource) []SkillSpec {
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	sort.Slice(entries, func(i, j int) bool { return strings.ToLower(entries[i].Name()) < strings.ToLower(entries[j].Name()) })
	var skills []SkillSpec
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		skillFile := filepath.Join(dir, SkillFilename)
		if _, err := os.Stat(skillFile); err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(skillFile)
		if err != nil {
			resolved, err = filepath.Abs(skillFile)
			if err != nil {
				resolved = skillFile
			}
		}
		if _, seen := l.seenPaths[resolved]; seen {
			continue
		}
		fm, _, err := ParseSkillMD(skillFile)
		if err != nil {
			continue
		}
		l.seenPaths[resolved] = struct{}{}
		skills = append(skills, SkillSpec{
			Frontmatter: fm, Source: source, Directory: dir, SkillFile: skillFile, CanonicalName: entry.Name(),
		})
	}
	return skills
}

func expandSkillHome(path string) string {
	if path == "~" {
		if home := skillHomeDir(); home != "" {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") {
		if home := skillHomeDir(); home != "" {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func skillHomeDir() string {
	if home := strings.TrimSpace(os.Getenv("HOME")); home != "" {
		return home
	}
	home, _ := os.UserHomeDir()
	return home
}

func uniqueExistingOrder(paths []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		key, err := filepath.Abs(path)
		if err != nil {
			key = path
		}
		if resolved, err := filepath.EvalSymlinks(key); err == nil {
			key = resolved
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, path)
	}
	return out
}
