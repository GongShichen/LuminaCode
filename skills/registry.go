package skills

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

type SkillRegistry struct {
	skills map[string]SkillSpec
	order  []string
	root   string
}

func NewSkillRegistry(root string) *SkillRegistry {
	if root != "" {
		root = resolveSkillPathBestEffort(root)
	}
	return &SkillRegistry{skills: map[string]SkillSpec{}, root: root}
}

func (r *SkillRegistry) Register(skill SkillSpec) {
	key := strings.ToLower(skill.CanonicalName)
	if _, exists := r.skills[key]; !exists {
		r.skills[key] = skill
		r.order = append(r.order, key)
	}
}

func (r *SkillRegistry) Find(name string) *SkillSpec {
	skill, ok := r.skills[strings.ToLower(name)]
	if !ok {
		return nil
	}
	return &skill
}

func (r *SkillRegistry) FindVisible(name, cwd string) *SkillSpec {
	skill := r.Find(name)
	if skill == nil || !r.IsVisible(*skill, cwd) {
		return nil
	}
	return skill
}

func (r *SkillRegistry) ListAll() []SkillSpec {
	out := make([]SkillSpec, 0, len(r.skills))
	for _, key := range r.order {
		if skill, ok := r.skills[key]; ok {
			out = append(out, skill)
		}
	}
	return out
}

func (r *SkillRegistry) ListVisible(cwd string) []SkillSpec {
	var visible []SkillSpec
	for _, key := range r.order {
		skill, ok := r.skills[key]
		if ok && r.IsVisible(skill, cwd) {
			visible = append(visible, skill)
		}
	}
	return visible
}

func (r *SkillRegistry) ListUserInvocable(cwd string) []SkillSpec {
	skills := r.ListAll()
	if cwd != "" {
		skills = r.ListVisible(cwd)
	}
	var out []SkillSpec
	for _, skill := range skills {
		if skill.Frontmatter.UserInvocable {
			out = append(out, skill)
		}
	}
	return out
}

func (r *SkillRegistry) ListModelInvocable(cwd string) []SkillSpec {
	skills := r.ListAll()
	if cwd != "" {
		skills = r.ListVisible(cwd)
	}
	var out []SkillSpec
	for _, skill := range skills {
		if !skill.Frontmatter.DisableModelInvocation {
			out = append(out, skill)
		}
	}
	return out
}

func (r *SkillRegistry) IsVisible(skill SkillSpec, cwd string) bool {
	patterns := skill.Frontmatter.Paths
	if patterns == nil {
		return true
	}
	cwdPath := resolveSkillPathBestEffort(cwd)
	visible := false
	for _, rawPattern := range patterns {
		spec, ok := parseSkillPattern(rawPattern)
		if !ok {
			continue
		}
		if matchesSkillPattern(cwdPath, r.root, spec) {
			visible = !spec.negated
		}
	}
	return visible
}

type skillPatternSpec struct {
	anchored      bool
	directoryOnly bool
	hasSlash      bool
	negated       bool
	parts         []string
}

func parseSkillPattern(pattern string) (skillPatternSpec, bool) {
	normalized := strings.TrimSpace(strings.ReplaceAll(pattern, "\\", "/"))
	if normalized == "" {
		return skillPatternSpec{}, false
	}
	negated := strings.HasPrefix(normalized, "!")
	if negated {
		normalized = strings.TrimSpace(strings.TrimPrefix(normalized, "!"))
	}
	normalized = strings.TrimPrefix(normalized, "./")
	anchored := strings.HasPrefix(normalized, "/")
	directoryOnly := strings.HasSuffix(normalized, "/")
	normalized = strings.Trim(normalized, "/")
	if normalized == "" {
		return skillPatternSpec{}, false
	}
	parts := strings.Split(normalized, "/")
	return skillPatternSpec{anchored: anchored, directoryOnly: directoryOnly, hasSlash: strings.Contains(normalized, "/"), negated: negated, parts: parts}, true
}

func matchesSkillPattern(path, root string, spec skillPatternSpec) bool {
	matchParts := pathPartsForSkill(path, root)
	if spec.directoryOnly {
		for _, ancestor := range directoryAncestors(path, root) {
			if matchesSkillParts(ancestor, spec) {
				return true
			}
		}
		return false
	}
	return matchesSkillParts(matchParts, spec)
}

func matchesSkillParts(pathParts []string, spec skillPatternSpec) bool {
	if spec.anchored {
		return segmentsMatch(pathParts, spec.parts)
	}
	if !spec.hasSlash {
		for _, segment := range pathParts {
			ok, _ := doublestar.Match(spec.parts[0], segment)
			if ok {
				return true
			}
		}
		return false
	}
	return segmentsMatch(pathParts, spec.parts)
}

func segmentsMatch(pathParts, patternParts []string) bool {
	pattern := strings.Join(patternParts, "/")
	path := strings.Join(pathParts, "/")
	ok, _ := doublestar.Match(pattern, path)
	return ok
}

func pathPartsForSkill(path, root string) []string {
	if root != "" {
		if rel, err := filepath.Rel(root, path); err == nil && isInsideRelative(rel) {
			return splitPathParts(rel)
		}
	}
	return splitPathParts(filepath.Clean(path))
}

func directoryAncestors(path, root string) [][]string {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	current := path
	if !info.IsDir() {
		current = filepath.Dir(path)
	}
	var ancestors [][]string
	for {
		if root != "" {
			rel, err := filepath.Rel(root, current)
			if err != nil || !isInsideRelative(rel) {
				break
			}
			if rel != "." {
				ancestors = append(ancestors, splitPathParts(rel))
			}
			if current == root {
				break
			}
		} else {
			parts := splitPathParts(current)
			if len(parts) > 0 {
				ancestors = append(ancestors, parts)
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return ancestors
}

func isInsideRelative(rel string) bool {
	return rel == "." || (rel != ".." && !strings.HasPrefix(filepath.ToSlash(rel), "../"))
}

func splitPathParts(path string) []string {
	path = filepath.ToSlash(filepath.Clean(path))
	var parts []string
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." {
			continue
		}
		if strings.HasSuffix(part, ":") {
			continue
		}
		parts = append(parts, part)
	}
	return parts
}

func resolveSkillPathBestEffort(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	cleaned := filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(resolved)
	}
	current := cleaned
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			parts := append([]string{resolved}, suffix...)
			return filepath.Clean(filepath.Join(parts...))
		}
		parent := filepath.Dir(current)
		if parent == current {
			return cleaned
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		current = parent
	}
}
