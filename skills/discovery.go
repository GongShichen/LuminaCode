package skills

import (
	"fmt"
	"sort"
	"strings"
)

const (
	MaxListingDescChars   = 250
	SkillListingBudgetPct = 0.01
	CharsPerTokenEstimate = 4
	SkillListingMetaKey   = "lumina_skill_listing"
)

type SkillDiscovery struct {
	Registry *SkillRegistry
}

func NewSkillDiscovery(registry *SkillRegistry) *SkillDiscovery {
	return &SkillDiscovery{Registry: registry}
}

func (d *SkillDiscovery) GetNewSkillsAttachment(contextWindowTokens int, cwd string) *string {
	var skills []SkillSpec
	if cwd != "" {
		skills = d.Registry.ListModelInvocable(cwd)
	} else {
		skills = d.Registry.ListModelInvocable("")
	}
	if len(skills) == 0 {
		return nil
	}
	budgetChars := int(float64(contextWindowTokens) * SkillListingBudgetPct * CharsPerTokenEstimate)
	text := d.FormatListing(skills, budgetChars, true)
	return &text
}

func (d *SkillDiscovery) BuildListingMessage(contextWindowTokens int, cwd string) map[string]any {
	text := d.GetNewSkillsAttachment(contextWindowTokens, cwd)
	if text == nil || *text == "" {
		return nil
	}
	return map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": *text}},
		"isMeta":  true,
		"metadata": map[string]any{
			"source":            SkillListingSource,
			SkillListingMetaKey: true,
		},
	}
}

func (d *SkillDiscovery) FormatListing(skills []SkillSpec, budgetChars int, preserveBundled bool) string {
	if len(skills) == 0 {
		return ""
	}
	ordered := append([]SkillSpec(nil), skills...)
	sort.Slice(ordered, func(i, j int) bool {
		pi, pj := sourcePriority(ordered[i].Source), sourcePriority(ordered[j].Source)
		if pi == pj {
			return strings.ToLower(ordered[i].CanonicalName) < strings.ToLower(ordered[j].CanonicalName)
		}
		return pi < pj
	})
	header := "<system-reminder>\nThe following skills are available through the Skill tool. Invoke a skill only when the task clearly matches its description or when_to_use guidance; do not call skills just to demonstrate capability. Skill context is transient and serves only the current request unless it explicitly writes files or memory.\n\n"
	footer := "\n</system-reminder>"
	if budgetChars < runeLen(header)+runeLen(footer)+16 {
		budgetChars = runeLen(header) + runeLen(footer) + 16
	}
	fullEntries := make([]string, 0, len(ordered))
	for _, skill := range ordered {
		fullEntries = append(fullEntries, d.formatFullEntry(skill))
	}
	fullText := header + strings.Join(fullEntries, "\n") + footer
	if runeLen(fullText) <= budgetChars {
		return fullText
	}
	remaining := budgetChars - runeLen(header) - runeLen(footer) - 1
	var rendered []string
	protected, regular := partitionSkills(ordered, preserveBundled)
	var omitted []SkillSpec
	for _, skill := range append(protected, regular...) {
		entry := d.formatFullEntry(skill)
		if remaining >= runeLen(entry)+1 {
			rendered = append(rendered, entry)
			remaining -= runeLen(entry) + 1
		} else {
			omitted = append(omitted, skill)
		}
	}
	if len(omitted) > 0 && remaining > 0 {
		for _, line := range formatNamesOnly(omitted) {
			if remaining < runeLen(line)+1 {
				break
			}
			rendered = append(rendered, line)
			remaining -= runeLen(line) + 1
		}
	}
	if len(rendered) == 0 {
		rendered = formatNamesOnly(ordered)
	}
	body := strings.Join(rendered, "\n")
	text := header + body + footer
	if runeLen(text) > budgetChars {
		text = header + truncateRunes(body, max(0, budgetChars-runeLen(header)-runeLen(footer))) + footer
	}
	return text
}

func (d *SkillDiscovery) formatFullEntry(skill SkillSpec) string {
	description := clipWords(skill.Frontmatter.Description, MaxListingDescChars)
	lines := []string{fmt.Sprintf("- %s: %s", skill.CanonicalName, description)}
	if skill.Frontmatter.WhenToUse != nil {
		lines = append(lines, "  When to use: "+clipWords(*skill.Frontmatter.WhenToUse, MaxListingDescChars))
	}
	return strings.Join(lines, "\n")
}

func sourcePriority(source SkillSource) int {
	switch source {
	case SkillSourceUser:
		return 0
	case SkillSourceProject:
		return 1
	case SkillSourceBundled:
		return 2
	default:
		return 99
	}
}

func partitionSkills(skills []SkillSpec, preserveBundled bool) ([]SkillSpec, []SkillSpec) {
	if !preserveBundled {
		return nil, skills
	}
	var protected, regular []SkillSpec
	for _, skill := range skills {
		if skill.Source == SkillSourceBundled {
			protected = append(protected, skill)
		} else {
			regular = append(regular, skill)
		}
	}
	return protected, regular
}

func formatNamesOnly(skills []SkillSpec) []string {
	out := make([]string, 0, len(skills))
	for _, skill := range skills {
		out = append(out, "- "+skill.CanonicalName)
	}
	return out
}

func clipWords(text string, limit int) string {
	text = strings.Join(strings.Fields(text), " ")
	if limit <= 0 {
		return ""
	}
	if runeLen(text) <= limit {
		return text
	}
	if limit <= 3 {
		return strings.Repeat(".", limit)
	}
	return truncateRunes(text, limit-3) + "..."
}
