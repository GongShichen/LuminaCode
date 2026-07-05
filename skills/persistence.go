package skills

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	PostCompactSkillsTokenBudget = 25000
	PostCompactMaxTokensPerSkill = 5000
	SkillRecoveryMetaKey         = "lumina_skill_recovery"
	SkillRecoverySnapshotVersion = 1
	recoveryHeader               = "<system-reminder>\nThe following skills were previously invoked and may still be relevant:\n\n"
	recoveryFooter               = "\n</system-reminder>"
	recoverySectionSeparator     = "\n\n---\n\n"
)

type InvokedSkillRecord struct {
	Name            string  `json:"name"`
	Path            string  `json:"path"`
	Content         string  `json:"content"`
	InvokedAt       float64 `json:"invoked_at"`
	AgentScope      string  `json:"agent_scope"`
	LastTurnIndex   int     `json:"last_turn_index"`
	InvocationCount int     `json:"invocation_count"`
}

type SkillPersistence struct {
	mu      sync.Mutex
	records map[string]map[string]InvokedSkillRecord
	order   map[string][]string
}

func NewSkillPersistence() *SkillPersistence {
	return &SkillPersistence{records: map[string]map[string]InvokedSkillRecord{}, order: map[string][]string{}}
}

func (p *SkillPersistence) RecordInvocation(agentScope, name, path, content string, turnCount int) {
	if p == nil || content == "" {
		return
	}
	if agentScope == "" {
		agentScope = "main"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	scopeRecords := p.records[agentScope]
	if scopeRecords == nil {
		scopeRecords = map[string]InvokedSkillRecord{}
		p.records[agentScope] = scopeRecords
	}
	now := float64(time.Now().UnixNano()) / float64(time.Second)
	record, exists := scopeRecords[name]
	if !exists {
		scopeRecords[name] = InvokedSkillRecord{
			Name:            name,
			Path:            path,
			Content:         content,
			InvokedAt:       now,
			AgentScope:      agentScope,
			LastTurnIndex:   turnCount,
			InvocationCount: 1,
		}
		p.order[agentScope] = append(p.order[agentScope], name)
		return
	}
	record.Path = path
	record.Content = content
	record.InvokedAt = now
	record.LastTurnIndex = turnCount
	record.InvocationCount++
	scopeRecords[name] = record
}

func (p *SkillPersistence) BuildRecoveryAttachment(agentScope string) *string {
	if p == nil {
		return nil
	}
	if agentScope == "" {
		agentScope = "main"
	}
	p.mu.Lock()
	scopeRecords := p.records[agentScope]
	records := make([]InvokedSkillRecord, 0, len(scopeRecords))
	for _, name := range p.order[agentScope] {
		if record, ok := scopeRecords[name]; ok {
			records = append(records, record)
		}
	}
	if len(records) < len(scopeRecords) {
		for name, record := range scopeRecords {
			if !stringInSlice(name, p.order[agentScope]) {
				records = append(records, record)
			}
		}
	}
	p.mu.Unlock()
	if len(records) == 0 {
		return nil
	}

	totalCharsBudget := PostCompactSkillsTokenBudget * CharsPerTokenEstimate
	maxCharsPerSkill := PostCompactMaxTokensPerSkill * CharsPerTokenEstimate
	candidates := buildRecoveryCandidates(records, maxCharsPerSkill)
	selected := selectRecoveryRecords(candidates, totalCharsBudget)
	if len(selected) == 0 {
		return nil
	}
	parts := make([]string, 0, len(selected))
	for _, candidate := range selected {
		parts = append(parts, candidate.section)
	}
	text := recoveryHeader + strings.Join(parts, recoverySectionSeparator) + recoveryFooter
	return &text
}

func (p *SkillPersistence) BuildRecoveryMessage(agentScope string) map[string]any {
	text := p.BuildRecoveryAttachment(agentScope)
	if text == nil || *text == "" {
		return nil
	}
	if agentScope == "" {
		agentScope = "main"
	}
	return map[string]any{
		"role":    "user",
		"content": []map[string]any{{"type": "text", "text": *text}},
		"isMeta":  true,
		"metadata": map[string]any{
			"source":             SkillRecoverySource,
			SkillRecoveryMetaKey: true,
			"agent_scope":        agentScope,
		},
	}
}

func (p *SkillPersistence) ClearForScope(agentScope string) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.records, agentScope)
	delete(p.order, agentScope)
}

func (p *SkillPersistence) ClearAll() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.records = map[string]map[string]InvokedSkillRecord{}
	p.order = map[string][]string{}
}

func (p *SkillPersistence) ExportSnapshot() map[string]any {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	scopes := map[string]any{}
	for scope, records := range p.records {
		scopeRecords := map[string]any{}
		for name, record := range records {
			scopeRecords[name] = map[string]any{
				"name":             record.Name,
				"path":             record.Path,
				"content":          record.Content,
				"invoked_at":       record.InvokedAt,
				"agent_scope":      record.AgentScope,
				"last_turn_index":  record.LastTurnIndex,
				"invocation_count": record.InvocationCount,
			}
		}
		scopes[scope] = scopeRecords
	}
	return map[string]any{
		"version":      SkillRecoverySnapshotVersion,
		"agent_scopes": scopes,
	}
}

func (p *SkillPersistence) ImportSnapshot(snapshot map[string]any) {
	if p == nil {
		return
	}
	if snapshot == nil {
		p.ClearAll()
		return
	}
	if !snapshotVersionMatchesPython(snapshot["version"]) {
		return
	}
	rawScopes, ok := snapshot["agent_scopes"].(map[string]any)
	if !ok {
		return
	}
	restored := map[string]map[string]InvokedSkillRecord{}
	restoredOrder := map[string][]string{}
	for scope, rawRecords := range rawScopes {
		recordMap, ok := rawRecords.(map[string]any)
		if !ok {
			continue
		}
		scopeRecords := map[string]InvokedSkillRecord{}
		for name, rawRecord := range recordMap {
			recordFields, ok := rawRecord.(map[string]any)
			if !ok {
				continue
			}
			content := truthyStringFromSnapshot(recordFields["content"])
			if content == "" {
				continue
			}
			invokedAt, ok := floatFromSnapshot(recordFields["invoked_at"])
			if !ok {
				continue
			}
			lastTurnIndex, ok := intFromSnapshot(recordFields["last_turn_index"])
			if !ok {
				continue
			}
			invocationCount, ok := intFromSnapshot(recordFields["invocation_count"])
			if !ok {
				continue
			}
			if invocationCount < 1 {
				invocationCount = 1
			}
			recordName := truthyStringFromSnapshot(recordFields["name"])
			if recordName == "" {
				recordName = name
			}
			recordScope := truthyStringFromSnapshot(recordFields["agent_scope"])
			if recordScope == "" {
				recordScope = scope
			}
			scopeRecords[name] = InvokedSkillRecord{
				Name:            recordName,
				Path:            truthyStringFromSnapshot(recordFields["path"]),
				Content:         content,
				InvokedAt:       invokedAt,
				AgentScope:      recordScope,
				LastTurnIndex:   lastTurnIndex,
				InvocationCount: invocationCount,
			}
			restoredOrder[scope] = append(restoredOrder[scope], name)
		}
		if len(scopeRecords) > 0 {
			restored[scope] = scopeRecords
		}
	}
	p.mu.Lock()
	p.records = restored
	p.order = restoredOrder
	p.mu.Unlock()
}

type recoveryCandidate struct {
	record  InvokedSkillRecord
	section string
}

func buildRecoveryCandidates(records []InvokedSkillRecord, maxCharsPerSkill int) []recoveryCandidate {
	candidates := make([]recoveryCandidate, 0, len(records))
	for _, record := range records {
		header := "## Skill: " + record.Name + "\nPath: " + record.Path + "\n\n"
		maxContentChars := max(0, maxCharsPerSkill-runeLen(header))
		content := truncateToCharLimit(record.Content, maxContentChars)
		section := header + content
		if runeLen(section) > maxCharsPerSkill {
			section = truncateRunes(section, maxCharsPerSkill)
		}
		candidates = append(candidates, recoveryCandidate{record: record, section: section})
	}
	return candidates
}

func selectRecoveryRecords(candidates []recoveryCandidate, totalCharsBudget int) []recoveryCandidate {
	if len(candidates) == 0 {
		return nil
	}
	fixedOverhead := runeLen(recoveryHeader) + runeLen(recoveryFooter)
	if fixedOverhead >= totalCharsBudget {
		return nil
	}
	latestTurn := 0
	for _, candidate := range candidates {
		if candidate.record.LastTurnIndex > latestTurn {
			latestTurn = candidate.record.LastTurnIndex
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return recoverySelectionLess(candidates[j], candidates[i], latestTurn)
	})
	var selected []recoveryCandidate
	totalChars := fixedOverhead
	for _, candidate := range candidates {
		separatorChars := 0
		if len(selected) > 0 {
			separatorChars = runeLen(recoverySectionSeparator)
		}
		nextTotal := totalChars + separatorChars + runeLen(candidate.section)
		if nextTotal > totalCharsBudget {
			continue
		}
		selected = append(selected, candidate)
		totalChars = nextTotal
	}
	return selected
}

func recoverySelectionLess(a, b recoveryCandidate, latestTurn int) bool {
	ad := recoveryDensity(a, latestTurn)
	bd := recoveryDensity(b, latestTurn)
	if ad != bd {
		return ad < bd
	}
	if a.record.LastTurnIndex != b.record.LastTurnIndex {
		return a.record.LastTurnIndex < b.record.LastTurnIndex
	}
	if a.record.InvocationCount != b.record.InvocationCount {
		return a.record.InvocationCount < b.record.InvocationCount
	}
	return a.record.InvokedAt < b.record.InvokedAt
}

func recoveryDensity(candidate recoveryCandidate, latestTurn int) float64 {
	turnGap := latestTurn - candidate.record.LastTurnIndex
	if turnGap < 0 {
		turnGap = 0
	}
	recencyScore := 1.0 / float64(1+turnGap)
	frequencyScore := candidate.record.InvocationCount
	if frequencyScore > 5 {
		frequencyScore = 5
	}
	valueScore := 4*recencyScore + float64(frequencyScore)
	estimatedTokens := estimateTokens(candidate.section)
	return valueScore / float64(estimatedTokens)
}

func truncateToCharLimit(content string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	if runeLen(content) <= maxChars {
		return content
	}
	if maxChars <= 3 {
		return strings.Repeat(".", maxChars)
	}
	return truncateRunes(content, maxChars-3) + "..."
}

func estimateTokens(content string) int {
	tokens := (runeLen(content) + CharsPerTokenEstimate - 1) / CharsPerTokenEstimate
	if tokens < 1 {
		return 1
	}
	return tokens
}

func runeLen(content string) int {
	return len([]rune(content))
}

func truncateRunes(content string, maxChars int) string {
	if maxChars <= 0 {
		return ""
	}
	runes := []rune(content)
	if len(runes) <= maxChars {
		return content
	}
	return string(runes[:maxChars])
}

func truthyStringFromSnapshot(value any) string {
	if !snapshotTruthy(value) {
		return ""
	}
	return fmt.Sprint(value)
}

func intFromSnapshot(value any) (int, bool) {
	if !snapshotTruthy(value) {
		return 0, true
	}
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case float32:
		return int(v), true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case string:
		parsed, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func snapshotVersionMatchesPython(value any) bool {
	switch v := value.(type) {
	case int:
		return v == SkillRecoverySnapshotVersion
	case int64:
		return v == SkillRecoverySnapshotVersion
	case float64:
		return v == SkillRecoverySnapshotVersion
	case float32:
		return v == SkillRecoverySnapshotVersion
	case bool:
		if v {
			return SkillRecoverySnapshotVersion == 1
		}
		return SkillRecoverySnapshotVersion == 0
	default:
		return false
	}
}

func floatFromSnapshot(value any) (float64, bool) {
	if !snapshotTruthy(value) {
		return 0, true
	}
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case bool:
		if v {
			return 1, true
		}
		return 0, true
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func snapshotTruthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case int:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	case float32:
		return v != 0
	default:
		return true
	}
}

func stringInSlice(needle string, haystack []string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}
