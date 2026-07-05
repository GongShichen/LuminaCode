package bash

import (
	"fmt"
	"strings"

	appsecurity "LuminaCode/security"
)

const (
	MaxSubcommandsForSecurityCheck = 50
	MaxSuggestedRulesForCompound   = 5
)

type ParseResult string

const (
	ParseSimple      ParseResult = "simple"
	ParseTooComplex  ParseResult = "too-complex"
	ParseUnavailable ParseResult = "parse-unavailable"
)

type Risk string

const (
	RiskSafe     Risk = "safe"
	RiskLow      Risk = "low"
	RiskNormal   Risk = "normal"
	RiskHigh     Risk = "high"
	RiskCritical Risk = "critical"
)

type PermissionResult struct {
	Allowed           bool
	Risk              Risk
	Reason            string
	SuggestedRules    []string
	NeedsUserDecision bool
	ParseResult       ParseResult
}

var blockedRulePrefixes = map[string]bool{
	"bash": true, "sh": true, "zsh": true, "dash": true, "ksh": true,
	"sudo": true, "doas": true, "pkexec": true, "env": true, "exec": true,
	"su": true, "nohup": true, "nice": true,
}

func init() {
	appsecurity.RegisterCommandRuleAnalyzer(appsecurity.CommandRuleAnalyzer{
		GetSimpleCommandPrefix: GetSimpleCommandPrefix,
		NeedsUserDecision: func(command string) bool {
			return AnalyzeCommandPermissions(command).NeedsUserDecision
		},
	})
}

func GetSimpleCommandPrefix(command string) string {
	cleaned := StripAllSafeEnvPrefixes(command)
	tokens := Tokenize(cleaned, false)
	if len(tokens) == 0 {
		return ""
	}
	base := tokens[0]
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}
	if blockedRulePrefixes[base] || len(tokens) < 2 || strings.HasPrefix(tokens[1], "-") {
		if base == "git" && len(tokens) >= 4 && tokens[1] == "-c" {
			return "git " + tokens[3]
		}
		return ""
	}
	return base + " " + tokens[1]
}

func AggregateCompoundPermissions(subResults []PermissionResult) PermissionResult {
	if len(subResults) == 0 {
		return PermissionResult{
			Allowed: false, Risk: RiskCritical, Reason: "Empty command",
			NeedsUserDecision: true, ParseResult: ParseUnavailable,
		}
	}
	if len(subResults) == 1 {
		return subResults[0]
	}
	var allRules []string
	for _, result := range subResults {
		allRules = append(allRules, result.SuggestedRules...)
	}
	if len(allRules) > MaxSuggestedRulesForCompound {
		allRules = allRules[:MaxSuggestedRulesForCompound]
	}
	for _, result := range subResults {
		if !result.Allowed && result.NeedsUserDecision {
			return PermissionResult{
				Allowed: false, Risk: RiskHigh,
				Reason:         "Compound command contains denied subcommand(s)",
				SuggestedRules: allRules, NeedsUserDecision: true,
				ParseResult: ParseUnavailable,
			}
		}
	}
	for _, result := range subResults {
		if result.NeedsUserDecision {
			return PermissionResult{
				Allowed: false, Risk: result.Risk,
				Reason:         "Compound command contains subcommand(s) requiring review",
				SuggestedRules: allRules, NeedsUserDecision: true,
				ParseResult: ParseUnavailable,
			}
		}
	}
	return PermissionResult{
		Allowed: true, Risk: RiskSafe, Reason: "All subcommands are safe",
		NeedsUserDecision: false, ParseResult: ParseUnavailable,
	}
}

func AnalyzeCommandPermissions(command string) PermissionResult {
	subcommands := SplitPipeline(command)
	if len(subcommands) > MaxSubcommandsForSecurityCheck {
		return PermissionResult{
			Allowed: false, Risk: RiskHigh,
			Reason:            fmt.Sprintf("Too many subcommands (%d > %d)", len(subcommands), MaxSubcommandsForSecurityCheck),
			NeedsUserDecision: true, ParseResult: ParseTooComplex,
		}
	}
	results := make([]PermissionResult, 0, len(subcommands))
	for _, sub := range subcommands {
		results = append(results, analyzeSingleCommand(sub))
	}
	result := AggregateCompoundPermissions(results)
	if result.NeedsUserDecision {
		result.SuggestedRules = generateRuleSuggestions(subcommands)
	}
	return result
}

func analyzeSingleCommand(command string) PermissionResult {
	cleaned := strings.TrimSpace(command)
	if cleaned == "" {
		return PermissionResult{
			Allowed: true, Risk: RiskSafe, Reason: "Empty command",
			NeedsUserDecision: false, ParseResult: ParseSimple,
		}
	}
	if appsecurity.IsDangerous(cleaned) {
		return PermissionResult{
			Allowed: false, Risk: RiskHigh, Reason: "Command matches dangerous pattern",
			NeedsUserDecision: true, ParseResult: ParseSimple,
		}
	}
	semantic := CheckSemantics(cleaned)
	if semantic == "dangerous" {
		return PermissionResult{
			Allowed: false, Risk: RiskHigh,
			Reason:            "Command contains dangerous patterns (AST)",
			NeedsUserDecision: true, ParseResult: ParseSimple,
		}
	}
	if semantic == "too-complex" {
		return PermissionResult{
			Allowed: false, Risk: RiskHigh,
			Reason:            "Command is too complex for automated analysis",
			NeedsUserDecision: true, ParseResult: ParseTooComplex,
		}
	}
	secResult := RunAllSecurityChecks(cleaned)
	if IsBlocking(secResult) {
		return PermissionResult{
			Allowed: false, Risk: RiskCritical,
			Reason:            "Security check failed: " + joinFindingDescriptions(secResult.Findings, 3),
			NeedsUserDecision: true, ParseResult: ParseSimple,
		}
	}
	if len(secResult.Findings) > 0 {
		return PermissionResult{
			Allowed: false, Risk: RiskNormal,
			Reason:            "Security warnings: " + joinFindingDescriptions(secResult.Findings, 3),
			NeedsUserDecision: true, ParseResult: ParseSimple,
		}
	}
	return PermissionResult{
		Allowed: true, Risk: RiskSafe, Reason: "All checks passed",
		NeedsUserDecision: false, ParseResult: ParseSimple,
	}
}

func generateRuleSuggestions(subcommands []string) []string {
	var suggestions []string
	seen := map[string]bool{}
	for _, sub := range subcommands {
		prefix := GetSimpleCommandPrefix(sub)
		if prefix != "" && !seen[prefix] {
			seen[prefix] = true
			suggestions = append(suggestions, fmt.Sprintf("Bash(%s:*)", prefix))
			if len(suggestions) >= MaxSuggestedRulesForCompound {
				return suggestions
			}
		}
	}
	if len(suggestions) != 0 {
		return suggestions
	}
	for _, sub := range subcommands {
		if len(suggestions) >= MaxSuggestedRulesForCompound {
			break
		}
		cleaned := strings.TrimSpace(sub)
		if len([]rune(cleaned)) > 80 {
			cleaned = firstCommandRunes(cleaned, 77) + "..."
		}
		if cleaned != "" && !seen[cleaned] {
			seen[cleaned] = true
			suggestions = append(suggestions, fmt.Sprintf("Bash(%s)", cleaned))
		}
	}
	return suggestions
}

func firstCommandRunes(text string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}
