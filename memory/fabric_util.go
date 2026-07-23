package memory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

func stableFabricID(prefix string, parts ...string) string {
	hash := sha256.New()
	_, _ = hash.Write([]byte(prefix))
	for _, part := range parts {
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write([]byte(part))
	}
	return prefix + "_" + hex.EncodeToString(hash.Sum(nil))[:32]
}

func contentHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func normalizeSpace(value string) string {
	value = normalizeKey(value)
	if value == "" {
		return "default"
	}
	return value
}

func normalizeKey(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	lastSeparator := false
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("./@:+#", r) {
			out.WriteRune(r)
			lastSeparator = false
			continue
		}
		if !lastSeparator {
			out.WriteByte('-')
			lastSeparator = true
		}
	}
	return strings.Trim(out.String(), "-")
}

func normalizeClaim(value string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(value))), " ")
}

func normalizeStringList(values []string, limit int) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = normalizeClaim(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
		if limit > 0 && len(result) >= limit {
			break
		}
	}
	return result
}

func marshalJSON(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "{}"
	}
	return string(data)
}

func marshalJSONArray(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func formatFabricTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339Nano)
}

func parseFabricTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func estimateTokens(text string) int {
	runes := len([]rune(strings.TrimSpace(text)))
	if runes == 0 {
		return 0
	}
	return maxIntMemory(1, (runes+2)/3)
}

func maxIntMemory(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func boolIntMemory(value bool) int {
	if value {
		return 1
	}
	return 0
}

func errorStringMemory(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func minIntMemory(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func intervalsOverlap(leftStart, leftEnd, rightStart, rightEnd time.Time) bool {
	if !leftEnd.IsZero() && !rightStart.IsZero() && leftEnd.Before(rightStart) {
		return false
	}
	if !rightEnd.IsZero() && !leftStart.IsZero() && rightEnd.Before(leftStart) {
		return false
	}
	return true
}

func claimValueKey(value ClaimValue) string {
	switch value.Kind {
	case ValueNumber:
		return string(value.Kind) + ":" + strconv.FormatFloat(value.Number, 'g', -1, 64) + ":" + normalizeKey(value.Unit)
	case ValueTime:
		return string(value.Kind) + ":" + formatFabricTime(value.Time)
	case ValueList:
		items := normalizeStringList(value.List, 0)
		sort.Strings(items)
		return string(value.Kind) + ":" + strings.Join(items, "\x1f")
	case ValueBool:
		if value.Bool == nil {
			return string(value.Kind) + ":unknown"
		}
		return string(value.Kind) + ":" + strconv.FormatBool(*value.Bool)
	default:
		return string(value.Kind) + ":" + normalizeClaim(value.Text)
	}
}

func validateFacet(value Facet) bool {
	switch value {
	case FacetProfile, FacetState, FacetPreference, FacetConstraint, FacetConfiguration,
		FacetLocation, FacetRelationship, FacetGoal, FacetProcedureState:
		return true
	default:
		return false
	}
}

func validateNodeKind(value NodeKind) bool {
	return value == NodeClaim || value == NodeEpisode || value == NodeProcedure
}

func scopeKey(scope Scope) string {
	return normalizeKey(strings.Join([]string{scope.Project, scope.Environment, scope.Actor, scope.Condition}, "|"))
}

func eventChecksum(event RawEvent) string {
	return contentHash(normalizeSpace(event.Space), event.ContextID, event.SessionID, event.Actor,
		event.SourceKind, event.SourceRef, formatFabricTime(event.OccurredAt), event.Content, marshalJSON(event.Metadata))
}

func sourceAuthority(node MemoryNode, events map[string]RawEvent) int {
	score := 0
	for _, source := range node.Sources {
		event := events[source.EventID]
		actor := strings.ToLower(strings.TrimSpace(event.Actor))
		kind := strings.ToLower(strings.TrimSpace(event.SourceKind))
		switch node.Facet {
		case FacetPreference, FacetConstraint:
			if actor == "user" {
				score = maxIntMemory(score, 100)
			}
		case FacetState, FacetConfiguration, FacetProcedureState:
			if actor == "tool" || kind == "tool" || kind == "observation" {
				score = maxIntMemory(score, 100)
			} else if actor == "user" {
				score = maxIntMemory(score, 70)
			}
		default:
			if kind == "document" || kind == "tool" || kind == "observation" {
				score = maxIntMemory(score, 90)
			} else if actor == "user" {
				score = maxIntMemory(score, 80)
			}
		}
	}
	switch node.EvidenceMode {
	case EvidenceObserved:
		score += 10
	case EvidenceUserDeclared:
		score += 5
	case EvidenceInferred:
		score -= 20
	}
	return score
}

var secretPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:api[_-]?key|access[_-]?token|secret|password)\s*[:=]\s*[^\s,;]+`),
	regexp.MustCompile(`(?i)\bbearer\s+[a-z0-9._~+/=-]{12,}`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{12,}\b`),
	regexp.MustCompile(`\bAKIA[A-Z0-9]{16}\b`),
}

func redactSecrets(text string) string {
	redacted := text
	for _, pattern := range secretPatterns {
		redacted = pattern.ReplaceAllStringFunc(redacted, func(match string) string {
			return strings.Repeat("*", utf8.RuneCountInString(match))
		})
	}
	return redacted
}

func uniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func mustJSONString(value any) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(fmt.Sprintf("marshal memory value: %v", err))
	}
	return string(data)
}
