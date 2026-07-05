package benchmark

import (
	"fmt"
	"sort"
	"strings"
)

var profiles = map[string]VariantOverride{
	"memory_off": {
		Name:        "baseline",
		Description: "memory mechanisms disabled",
		ConfigOverrides: map[string]any{
			"disable_memory_recall":        true,
			"disable_memory_extraction":    true,
			"disable_memory_index":         true,
			"disable_memory_effectiveness": true,
		},
	},
	"context_off": {
		Name:        "baseline",
		Description: "context optimizations disabled",
		ConfigOverrides: map[string]any{
			"disable_context_optimizations": true,
		},
	},
	"security_relaxed": {
		Name:        "baseline",
		Description: "security protections relaxed",
		ConfigOverrides: map[string]any{
			"disable_static_checks": true,
			"disable_classifier":    true,
			"disable_sandbox":       true,
			"disable_secret_guard":  true,
		},
	},
}

func AvailableProfiles() []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func GetProfile(name string) (VariantOverride, error) {
	profile, ok := profiles[name]
	if ok {
		return cloneVariant(profile), nil
	}
	return VariantOverride{}, fmt.Errorf("unknown benchmark profile: %s. available=%s", name, strings.Join(AvailableProfiles(), ", "))
}

func cloneVariant(variant VariantOverride) VariantOverride {
	cloned := variant
	if variant.ConfigOverrides != nil {
		cloned.ConfigOverrides = map[string]any{}
		for key, value := range variant.ConfigOverrides {
			cloned.ConfigOverrides[key] = value
		}
	}
	return cloned
}
