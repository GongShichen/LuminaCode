package benchmark

import "context"

type BuildReportOptions struct {
	Variant         *VariantOverride
	BaselinePlugins []BenchmarkPlugin
	BaselineVariant *VariantOverride
	BaselineProfile *string
	Tiers           []string
}

func BuildBenchmarkReport(ctx context.Context, plugins []BenchmarkPlugin, options BuildReportOptions) (BenchmarkReport, error) {
	core := BenchmarkCore{}
	activeVariant := withTiers(variantOrDefault(options.Variant, VariantOverride{Name: "candidate", Description: "benchmark run"}), options.Tiers)
	pluginResults := map[string]PluginRunResult{}
	pluginRules := map[string][]PluginSLA{}
	pluginOrder := make([]string, 0, len(plugins))
	for _, plugin := range plugins {
		result, err := core.RunSuite(ctx, plugin, &activeVariant)
		if err != nil {
			return BenchmarkReport{}, err
		}
		name := plugin.Name()
		pluginResults[name] = result
		pluginRules[name] = plugin.BuildSLARules()
		pluginOrder = append(pluginOrder, name)
	}
	run := core.BuildRunWithOrder(activeVariant, pluginResults, pluginOrder)
	verdict, err := core.Evaluate(run, pluginRules)
	if err != nil {
		return BenchmarkReport{}, err
	}

	var comparison *BenchmarkComparison
	if options.BaselinePlugins != nil {
		var baselineVariant VariantOverride
		if options.BaselineProfile != nil {
			profile, err := GetProfile(*options.BaselineProfile)
			if err != nil {
				return BenchmarkReport{}, err
			}
			baselineVariant = profile
		} else {
			baselineVariant = variantOrDefault(options.BaselineVariant, VariantOverride{Name: "baseline", Description: "benchmark baseline"})
		}
		activeBaselineVariant := withTiers(baselineVariant, options.Tiers)
		baselineResults := map[string]PluginRunResult{}
		baselineOrder := make([]string, 0, len(options.BaselinePlugins))
		for _, plugin := range options.BaselinePlugins {
			result, err := core.RunSuite(ctx, plugin, &activeBaselineVariant)
			if err != nil {
				return BenchmarkReport{}, err
			}
			name := plugin.Name()
			baselineResults[name] = result
			baselineOrder = append(baselineOrder, name)
		}
		baselineRun := core.BuildRunWithOrder(activeBaselineVariant, baselineResults, baselineOrder)
		comparison = new(core.ComputeDeltas(baselineRun, run))
	}

	return core.BuildBenchmarkReport(comparison, verdict, run, pluginRules), nil
}

func variantOrDefault(variant *VariantOverride, fallback VariantOverride) VariantOverride {
	if variant == nil {
		return cloneVariant(fallback)
	}
	return cloneVariant(*variant)
}

func withTiers(variant VariantOverride, tiers []string) VariantOverride {
	if tiers == nil {
		return variant
	}
	merged := map[string]any{}
	for key, value := range variant.ConfigOverrides {
		merged[key] = value
	}
	merged["tiers"] = uniqueStrings(tiers)
	variant.ConfigOverrides = merged
	return variant
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out
}
