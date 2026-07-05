package skills

import (
	"fmt"
	"strings"
)

const (
	SkillInlineSource              = "skill_inline"
	SkillListingSource             = "skill_listing"
	SkillRecoverySource            = "skill_recovery"
	SkillInlineAllowedToolsKey     = "lumina_skill_allowed_tools"
	SkillInlineDisableSkillToolKey = "lumina_skill_disable_skill_tool"
)

var SkillTransientSources = map[string]struct{}{
	SkillInlineSource:   {},
	SkillListingSource:  {},
	SkillRecoverySource: {},
}

type InlineSkillRuntime struct {
	HasAllowedTools  bool
	AllowedToolNames []string
	ModelOverride    *string
	Effort           any
	DisableSkillTool bool
}

func StripSkillContextMessages(messages []map[string]any, sources map[string]struct{}) []map[string]any {
	if sources == nil {
		sources = SkillTransientSources
	}
	var kept []map[string]any
	for _, message := range messages {
		metadata, _ := message["metadata"].(map[string]any)
		source, _ := metadata["source"].(string)
		if _, drop := sources[source]; !drop {
			kept = append(kept, message)
		}
	}
	return kept
}

func ResolveSkillContextCWD(defaultCWD string, context map[string]any) string {
	if context != nil {
		if cwd, ok := context["cwd"].(string); ok && cwd != "" {
			return resolveSkillPathBestEffort(cwd)
		}
	}
	return resolveSkillPathBestEffort(defaultCWD)
}

func CollectInlineSkillRuntime(messages []map[string]any) InlineSkillRuntime {
	var effectiveAllow map[string]struct{}
	var hasAllow bool
	var model *string
	var effort any
	disable := false
	for _, message := range messages {
		metadata, _ := message["metadata"].(map[string]any)
		if metadata["source"] != SkillInlineSource {
			continue
		}
		if rawAllowed, exists := metadata[SkillInlineAllowedToolsKey]; exists && rawAllowed != nil {
			allowed := map[string]struct{}{}
			for _, name := range stringSlice(rawAllowed) {
				if name != "" {
					allowed[name] = struct{}{}
				}
			}
			if !hasAllow {
				effectiveAllow = allowed
				hasAllow = true
			} else {
				for name := range effectiveAllow {
					if _, ok := allowed[name]; !ok {
						delete(effectiveAllow, name)
					}
				}
			}
		}
		if rawModel, ok := metadata["lumina_skill_model"].(string); ok && rawModel != "" {
			model = &rawModel
		}
		if rawEffort, ok := metadata["lumina_skill_effort"]; ok && rawEffort != nil {
			effort = rawEffort
		}
		if metadata[SkillInlineDisableSkillToolKey] == true {
			disable = true
		}
	}
	var allowedList []string
	if hasAllow {
		for name := range effectiveAllow {
			allowedList = append(allowedList, name)
		}
	}
	return InlineSkillRuntime{HasAllowedTools: hasAllow, AllowedToolNames: allowedList, ModelOverride: model, Effort: effort, DisableSkillTool: disable}
}

func stringSlice(raw any) []string {
	switch values := raw.(type) {
	case []string:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if text := strings.TrimSpace(value); text != "" {
				out = append(out, text)
			}
		}
		return out
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if text := strings.TrimSpace(fmt.Sprint(value)); text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}
