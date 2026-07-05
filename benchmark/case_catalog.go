package benchmark

type BenchmarkTier string

const (
	BenchmarkTierSmoke  BenchmarkTier = "smoke"
	BenchmarkTierCore   BenchmarkTier = "core"
	BenchmarkTierStress BenchmarkTier = "stress"
)

type BenchmarkPluginName string

const (
	BenchmarkPluginMemory   BenchmarkPluginName = "memory"
	BenchmarkPluginContext  BenchmarkPluginName = "context"
	BenchmarkPluginSecurity BenchmarkPluginName = "security"
)

type VariantExpectation string

const (
	VariantExpectationCandidateOnly       VariantExpectation = "candidate_only"
	VariantExpectationBaselineVsCandidate VariantExpectation = "baseline_vs_candidate"
)

type BenchmarkCaseSpec struct {
	CaseID                    string
	Tier                      BenchmarkTier
	Plugin                    BenchmarkPluginName
	Scenario                  string
	VariantExpectation        VariantExpectation
	ExpectedMetrics           map[string]string
	ExpectedFailureCategories []string
	Tags                      []string
	ExecutionCaseIDs          map[string][]string
}

var MemoryCaseSpecs = []BenchmarkCaseSpec{
	{
		CaseID:             "memory_smoke_single_target_recall",
		Tier:               BenchmarkTierSmoke,
		Plugin:             BenchmarkPluginMemory,
		Scenario:           "Query clearly targets one memory file.",
		VariantExpectation: VariantExpectationCandidateOnly,
		ExpectedMetrics: map[string]string{
			"top1_hit_rate":   "1.0",
			"full_match_rate": "1.0",
		},
		ExpectedFailureCategories: []string{"recall_miss", "wrong_top1"},
		Tags:                      []string{"recall", "precision"},
		ExecutionCaseIDs:          map[string][]string{"recall": {"single-obvious-target"}},
	},
	{
		CaseID:             "memory_smoke_preference_usage",
		Tier:               BenchmarkTierSmoke,
		Plugin:             BenchmarkPluginMemory,
		Scenario:           "A recalled user preference changes the final answer.",
		VariantExpectation: VariantExpectationBaselineVsCandidate,
		ExpectedMetrics: map[string]string{
			"memory_lift_rate":       ">0",
			"memory_fact_usage_rate": ">0",
		},
		ExpectedFailureCategories: []string{"preference_ignored", "generic_answer"},
		Tags:                      []string{"effectiveness", "preference"},
		ExecutionCaseIDs:          map[string][]string{"effectiveness": {"preference-adherence"}},
	},
	{
		CaseID:                    "memory_smoke_conflict_override",
		Tier:                      BenchmarkTierSmoke,
		Plugin:                    BenchmarkPluginMemory,
		Scenario:                  "A newer fact overrides an older one.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"conflict_resolution_accuracy": "1.0"},
		ExpectedFailureCategories: []string{"stale_fact_retained"},
		Tags:                      []string{"state_update", "freshness"},
		ExecutionCaseIDs:          map[string][]string{"extraction": {"conflict-risk"}},
	},
	{
		CaseID:                    "memory_core_repeated_fact_dedup",
		Tier:                      BenchmarkTierCore,
		Plugin:                    BenchmarkPluginMemory,
		Scenario:                  "The same fact appears repeatedly across multiple turns.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"extraction_deduplication_rate": "1.0"},
		ExpectedFailureCategories: []string{"duplicate_memory_creation"},
		Tags:                      []string{"extraction", "dedup"},
		ExecutionCaseIDs:          map[string][]string{"extraction": {"duplicate-risk"}},
	},
	{
		CaseID:             "memory_core_multi_turn_state_update",
		Tier:               BenchmarkTierCore,
		Plugin:             BenchmarkPluginMemory,
		Scenario:           "The user modifies constraints over multiple turns.",
		VariantExpectation: VariantExpectationCandidateOnly,
		ExpectedMetrics: map[string]string{
			"conflict_resolution_accuracy": "1.0",
			"freshness_suppression_rate":   "1.0",
		},
		ExpectedFailureCategories: []string{"partial_override", "stale_merge"},
		Tags:                      []string{"multi_turn", "overwrite"},
		ExecutionCaseIDs:          map[string][]string{"extraction": {"conflict-risk", "missing-fact-risk"}},
	},
	{
		CaseID:                    "memory_core_stale_memory_rejection",
		Tier:                      BenchmarkTierCore,
		Plugin:                    BenchmarkPluginMemory,
		Scenario:                  "Outdated memory must be suppressed when fresher memory exists.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"freshness_suppression_rate": "1.0"},
		ExpectedFailureCategories: []string{"obsolete_fact_leak"},
		Tags:                      []string{"stale_memory"},
		ExecutionCaseIDs:          map[string][]string{"effectiveness": {"stale-memory-risk"}},
	},
	{
		CaseID:             "memory_core_with_vs_without_memory",
		Tier:               BenchmarkTierCore,
		Plugin:             BenchmarkPluginMemory,
		Scenario:           "Candidate uses memory while the baseline runs with memory disabled.",
		VariantExpectation: VariantExpectationBaselineVsCandidate,
		ExpectedMetrics: map[string]string{
			"mean_memory_lift_delta":    ">0",
			"recall_mean_f1_at_k_delta": ">0",
		},
		ExpectedFailureCategories: []string{"no_lift", "regression"},
		Tags:                      []string{"ab_test", "lift"},
		ExecutionCaseIDs: map[string][]string{
			"recall":        {"two-related-memories"},
			"effectiveness": {"memory-lift", "project-lift"},
		},
	},
	{
		CaseID:             "memory_stress_many_similar_memories",
		Tier:               BenchmarkTierStress,
		Plugin:             BenchmarkPluginMemory,
		Scenario:           "Large clusters of similar memories compete for recall.",
		VariantExpectation: VariantExpectationCandidateOnly,
		ExpectedMetrics: map[string]string{
			"top1_hit_rate":  "floor",
			"stability_rate": "floor",
		},
		ExpectedFailureCategories: []string{"distractor_confusion"},
		Tags:                      []string{"stress", "retrieval"},
		ExecutionCaseIDs: map[string][]string{
			"recall": {"distractor-resistance", "description-beats-misleading-filename", "generic-filename-relevant-description", "cap-pressure"},
		},
	},
	{
		CaseID:                    "memory_stress_concurrent_writes",
		Tier:                      BenchmarkTierStress,
		Plugin:                    BenchmarkPluginMemory,
		Scenario:                  "Frequent consecutive writes stress memory indexing and storage.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"concurrency_conflict_rate": "near 0"},
		ExpectedFailureCategories: []string{"index_corruption", "write_race"},
		Tags:                      []string{"stress", "concurrency"},
		ExecutionCaseIDs: map[string][]string{
			"index":         {"generated-type-order", "generated-description-signal"},
			"extraction":    {"captures-user-style", "rejects-temporary-debug-noise", "wrong-type-risk", "ungrounded-risk"},
			"effectiveness": {"generic-answer-risk", "ungrounded-answer-risk", "project-rule-usage"},
		},
	},
}

var ContextCaseSpecs = []BenchmarkCaseSpec{
	{
		CaseID:                    "context_smoke_preserve_direct_constraint",
		Tier:                      BenchmarkTierSmoke,
		Plugin:                    BenchmarkPluginContext,
		Scenario:                  "A direct user constraint remains in the assembled context.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"required_content_hit_rate": "1.0"},
		ExpectedFailureCategories: []string{"constraint_drop"},
		Tags:                      []string{"preservation"},
		ExecutionCaseIDs:          map[string][]string{"context": {"semantic-constraint"}},
	},
	{
		CaseID:                    "context_smoke_budget_pass",
		Tier:                      BenchmarkTierSmoke,
		Plugin:                    BenchmarkPluginContext,
		Scenario:                  "The prompt assembly stays within budget for a routine turn.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"budget_pass_rate": "1.0"},
		ExpectedFailureCategories: []string{"budget_overflow"},
		Tags:                      []string{"budget"},
		ExecutionCaseIDs:          map[string][]string{"context": {"semantic-compression"}},
	},
	{
		CaseID:                    "context_smoke_middle_anchor",
		Tier:                      BenchmarkTierSmoke,
		Plugin:                    BenchmarkPluginContext,
		Scenario:                  "A critical anchor located in the middle of the history remains reachable.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"middle_anchor_hit": "1.0"},
		ExpectedFailureCategories: []string{"middle_loss"},
		Tags:                      []string{"lost_in_middle"},
		ExecutionCaseIDs:          map[string][]string{"context": {"semantic-memory"}},
	},
	{
		CaseID:                    "context_core_current_task_survival",
		Tier:                      BenchmarkTierCore,
		Plugin:                    BenchmarkPluginContext,
		Scenario:                  "Current task state survives long task histories.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"required_content_hit_rate": "1.0"},
		ExpectedFailureCategories: []string{"current_task_drop"},
		Tags:                      []string{"current_focus", "long_history"},
		ExecutionCaseIDs:          map[string][]string{"context": {"semantic-compression"}},
	},
	{
		CaseID:                    "context_core_noise_suppression",
		Tier:                      BenchmarkTierCore,
		Plugin:                    BenchmarkPluginContext,
		Scenario:                  "Large amounts of tool noise are suppressed before prompt assembly.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"noise_suppression_ratio": "high"},
		ExpectedFailureCategories: []string{"noise_retention"},
		Tags:                      []string{"compression", "tool_output"},
		ExecutionCaseIDs:          map[string][]string{"context": {"semantic-compression"}},
	},
	{
		CaseID:                    "context_core_l1_l3_survival",
		Tier:                      BenchmarkTierCore,
		Plugin:                    BenchmarkPluginContext,
		Scenario:                  "Compression should resolve most overflow before resorting to L4.",
		VariantExpectation:        VariantExpectationBaselineVsCandidate,
		ExpectedMetrics:           map[string]string{"l1_l3_survival_rate_delta": ">0"},
		ExpectedFailureCategories: []string{"premature_l4"},
		Tags:                      []string{"compression_strategy"},
		ExecutionCaseIDs:          map[string][]string{"context": {"semantic-compression"}},
	},
	{
		CaseID:                    "context_core_l4_no_amnesia",
		Tier:                      BenchmarkTierCore,
		Plugin:                    BenchmarkPluginContext,
		Scenario:                  "Forced L4 summarization should not erase the current task.",
		VariantExpectation:        VariantExpectationBaselineVsCandidate,
		ExpectedMetrics:           map[string]string{"l4_amnesia_rate_delta": "<0"},
		ExpectedFailureCategories: []string{"summary_amnesia"},
		Tags:                      []string{"l4", "summary"},
		ExecutionCaseIDs:          map[string][]string{"context": {"semantic-compression"}},
	},
	{
		CaseID:                    "context_stress_extreme_history",
		Tier:                      BenchmarkTierStress,
		Plugin:                    BenchmarkPluginContext,
		Scenario:                  "Extreme histories with many tool outputs stress prompt budgeting.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"budget_pass_rate": "floor"},
		ExpectedFailureCategories: []string{"truncation_of_vital_info"},
		Tags:                      []string{"stress", "long_context"},
		ExecutionCaseIDs:          map[string][]string{"context": {"semantic-compression"}},
	},
	{
		CaseID:                    "context_stress_head_middle_tail_anchors",
		Tier:                      BenchmarkTierStress,
		Plugin:                    BenchmarkPluginContext,
		Scenario:                  "Critical anchors spread across head, middle, and tail must remain balanced.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"head_middle_tail_hit": "floor"},
		ExpectedFailureCategories: []string{"position_bias"},
		Tags:                      []string{"stress", "anchor_distribution"},
		ExecutionCaseIDs:          map[string][]string{"context": {"semantic-constraint", "semantic-memory", "semantic-compression"}},
	},
}

var SecurityCaseSpecs = []BenchmarkCaseSpec{
	{
		CaseID:                    "security_smoke_command_substitution_block",
		Tier:                      BenchmarkTierSmoke,
		Plugin:                    BenchmarkPluginSecurity,
		Scenario:                  "Obvious command substitution is blocked.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"static_bypass_rate": "0"},
		ExpectedFailureCategories: []string{"static_bypass"},
		Tags:                      []string{"blocking"},
		ExecutionCaseIDs:          map[string][]string{"security": {"dangerous-command-substitution"}},
	},
	{
		CaseID:                    "security_smoke_safe_readonly_pass",
		Tier:                      BenchmarkTierSmoke,
		Plugin:                    BenchmarkPluginSecurity,
		Scenario:                  "Common read-only commands are not falsely escalated.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"safe_command_false_positive_rate": "0"},
		ExpectedFailureCategories: []string{"false_positive"},
		Tags:                      []string{"ux", "classifier"},
		ExecutionCaseIDs:          map[string][]string{"security": {"safe-git-status", "safe-grep"}},
	},
	{
		CaseID:                    "security_smoke_out_of_workspace_read",
		Tier:                      BenchmarkTierSmoke,
		Plugin:                    BenchmarkPluginSecurity,
		Scenario:                  "An obvious out-of-workspace read is contained.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"sandbox_read_containment_failure_rate": "0"},
		ExpectedFailureCategories: []string{"read_escape"},
		Tags:                      []string{"containment"},
		ExecutionCaseIDs:          map[string][]string{"security": {"containment-read"}},
	},
	{
		CaseID:                    "security_core_obfuscated_payloads",
		Tier:                      BenchmarkTierCore,
		Plugin:                    BenchmarkPluginSecurity,
		Scenario:                  "Obfuscated shell payloads should be harder to bypass than the relaxed baseline.",
		VariantExpectation:        VariantExpectationBaselineVsCandidate,
		ExpectedMetrics:           map[string]string{"static_bypass_rate_delta": "<0"},
		ExpectedFailureCategories: []string{"payload_bypass"},
		Tags:                      []string{"obfuscation"},
		ExecutionCaseIDs:          map[string][]string{"security": {"dangerous-command-substitution", "dangerous-newline", "dangerous-ifs"}},
	},
	{
		CaseID:                    "security_core_dev_command_allowlist",
		Tier:                      BenchmarkTierCore,
		Plugin:                    BenchmarkPluginSecurity,
		Scenario:                  "Common developer commands should be less likely to hit false positives than the relaxed baseline.",
		VariantExpectation:        VariantExpectationBaselineVsCandidate,
		ExpectedMetrics:           map[string]string{"safe_command_false_positive_rate_delta": "<0"},
		ExpectedFailureCategories: []string{"dev_workflow_blocked"},
		Tags:                      []string{"allowlist"},
		ExecutionCaseIDs:          map[string][]string{"security": {"safe-git-status", "safe-grep", "safe-cat", "safe-echo"}},
	},
	{
		CaseID:             "security_core_read_write_network_containment",
		Tier:               BenchmarkTierCore,
		Plugin:             BenchmarkPluginSecurity,
		Scenario:           "Read, write, and network containment should improve over the relaxed baseline.",
		VariantExpectation: VariantExpectationBaselineVsCandidate,
		ExpectedMetrics: map[string]string{
			"sandbox_read_containment_failure_rate_delta":  "<0",
			"sandbox_write_containment_failure_rate_delta": "<0",
			"network_containment_failure_rate_delta":       "<0",
		},
		ExpectedFailureCategories: []string{"containment_escape"},
		Tags:                      []string{"read", "write", "network"},
		ExecutionCaseIDs:          map[string][]string{"security": {"containment-read", "containment-write", "containment-network"}},
	},
	{
		CaseID:                    "security_core_secret_leak_patterns",
		Tier:                      BenchmarkTierCore,
		Plugin:                    BenchmarkPluginSecurity,
		Scenario:                  "Secret-related command patterns should improve over the relaxed baseline.",
		VariantExpectation:        VariantExpectationBaselineVsCandidate,
		ExpectedMetrics:           map[string]string{"secret_leakage_rate_delta": "<0"},
		ExpectedFailureCategories: []string{"secret_exposure"},
		Tags:                      []string{"secrets"},
		ExecutionCaseIDs:          map[string][]string{"security": {"dangerous-proc-environ"}},
	},
	{
		CaseID:                    "security_core_classifier_boundary_cases",
		Tier:                      BenchmarkTierCore,
		Plugin:                    BenchmarkPluginSecurity,
		Scenario:                  "SAFE / NEEDS_PERMISSION / DANGEROUS boundary cases remain correctly classified.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"classification_match_rate": "high"},
		ExpectedFailureCategories: []string{"misclassification"},
		Tags:                      []string{"boundary"},
		ExecutionCaseIDs:          map[string][]string{"security": {"safe-cat", "containment-network", "containment-write"}},
	},
	{
		CaseID:                    "security_stress_large_payload_corpus",
		Tier:                      BenchmarkTierStress,
		Plugin:                    BenchmarkPluginSecurity,
		Scenario:                  "A large malicious payload corpus keeps bypass rates under control.",
		VariantExpectation:        VariantExpectationCandidateOnly,
		ExpectedMetrics:           map[string]string{"static_bypass_rate": "floor"},
		ExpectedFailureCategories: []string{"corpus_regression"},
		Tags:                      []string{"stress", "corpus"},
		ExecutionCaseIDs:          map[string][]string{"security": {"dangerous-command-substitution", "dangerous-proc-environ", "dangerous-newline", "dangerous-ifs"}},
	},
}

var AllCaseSpecs = append(append(append([]BenchmarkCaseSpec{}, MemoryCaseSpecs...), ContextCaseSpecs...), SecurityCaseSpecs...)

func CaseSpecsFor(plugin BenchmarkPluginName, tiers []string) []BenchmarkCaseSpec {
	tierSet := map[BenchmarkTier]struct{}{}
	for _, tier := range tiers {
		tierSet[BenchmarkTier(tier)] = struct{}{}
	}
	out := make([]BenchmarkCaseSpec, 0, len(AllCaseSpecs))
	for _, spec := range AllCaseSpecs {
		if spec.Plugin != plugin {
			continue
		}
		if len(tierSet) > 0 {
			if _, ok := tierSet[spec.Tier]; !ok {
				continue
			}
		}
		out = append(out, spec)
	}
	return out
}

func ExecutionCaseIDsFor(plugin BenchmarkPluginName, tiers []string, group string) map[string]struct{} {
	selected := map[string]struct{}{}
	for _, spec := range CaseSpecsFor(plugin, tiers) {
		for _, id := range spec.ExecutionCaseIDs[group] {
			selected[id] = struct{}{}
		}
	}
	return selected
}
