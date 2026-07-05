package agentContext

import (
	"fmt"
	"sort"
	"strings"

	mapset "github.com/deckarep/golang-set/v2"
	"github.com/mohae/deepcopy"
)

const (
	defaultKeepRecent              = 5
	defaultKeepRecentMessage       = defaultKeepRecent * 2
	defaultCollapseThresholdTokens = 90000
	minRegionSize                  = 3
)

func getL3CollapseThreshold(contextLimit, softLimit int) int {
	l4Threshold := softLimit
	return MaxInt(1, MinInt(MinInt(defaultCollapseThresholdTokens, softLimit), l4Threshold-1))
}

func GetL3CollapseThreshold(contextLimit, softLimit int) int {
	return getL3CollapseThreshold(contextLimit, softLimit)
}

type CollapsedRegion struct {
	StartIdx int
	EndIdx   int
	Summary  string
}

type ProjectedExchange struct {
	Role       string   `json:"role"`
	Summary    string   `json:"summary"`
	ToolCalls  []string `json:"tool_calls"`
	KeyOutputs []string `json:"key_outputs"`
}

func partitionExchanges(messages []map[string]any) [][]map[string]any {
	if len(messages) == 0 {
		return [][]map[string]any{}
	}
	var exchanges [][]map[string]any
	current := []map[string]any{messages[0]}
	lastRole, ok := messages[0]["role"].(string)
	if !ok {
		lastRole = ""
	}
	for _, msg := range messages[1:] {
		role, ok := msg["role"].(string)
		if !ok {
			role = ""
		}
		if role != lastRole && current != nil {
			exchanges = append(exchanges, current)
			current = []map[string]any{msg}
		} else {
			current = append(current, msg)
		}
	}
	if current != nil && len(current) > 0 {
		exchanges = append(exchanges, current)
	}
	return exchanges
}

func projectExchange(exchange []map[string]any) *ProjectedExchange {
	if len(exchange) == 0 {
		return nil
	}

	proj := &ProjectedExchange{
		ToolCalls:  []string{},
		KeyOutputs: []string{},
	}

	for _, msg := range exchange {
		role, _ := msg["role"].(string)
		if proj.Role == "" {
			proj.Role = role
		}

		content, ok := contentBlocks(msg["content"])
		if !ok {
			continue
		}

		for _, block := range content {
			blockType, _ := block["type"].(string)

			switch blockType {
			case "text":
				text, _ := block["text"].(string)
				if text != "" && len([]rune(text)) > 10 {
					firstSent := strings.TrimSpace(strings.Split(text, ".")[0])
					firstSent = TruncateRunes(firstSent, 200)

					if proj.Summary == "" {
						proj.Summary = firstSent
					}
				}

			case "tool_use":
				toolName, _ := block["name"].(string)
				if toolName != "" {
					proj.ToolCalls = append(proj.ToolCalls, toolName)
				}

			case "tool_result":
				result, _ := block["content"].(string)
				if result != "" {
					firstLine := strings.Split(strings.TrimSpace(result), "\n")[0]
					firstLine = TruncateRunes(firstLine, 150)

					if firstLine != "" {
						proj.KeyOutputs = append(proj.KeyOutputs, firstLine)
					}
				}
			}
		}
	}

	if proj.Summary == "" && len(proj.ToolCalls) == 0 {
		return nil
	}

	return proj
}

func buildProjectionText(projections []ProjectedExchange) string {
	lines := []string{"[Earlier conversation — summarized]\n"}
	for i, proj := range projections {
		parts := []string{fmt.Sprintf("Turn %d", i+1)}
		if proj.Summary != "" {
			parts = append(parts, fmt.Sprintf("  Summary: %s", proj.Summary))
		}
		if len(proj.ToolCalls) > 0 {
			parts = append(parts, fmt.Sprintf("  Tools called: %s", strings.Join(proj.ToolCalls, ", ")))
		}
		if len(proj.KeyOutputs) > 0 {
			outputs := strings.Join(proj.KeyOutputs[:MinInt(3, len(proj.KeyOutputs))], "; ")
			parts = append(parts, fmt.Sprintf("  Key outputs: %s", outputs))
		}
		lines = append(lines, strings.Join(parts, " "))
	}
	return strings.Join(lines, "\n")
}

func CollapseMessages(messages []map[string]any, keepRecent int) []map[string]any {
	if len(messages) == 0 {
		return messages
	}
	exchanges := partitionExchanges(messages)
	if len(exchanges) <= keepRecent {
		return messages
	}
	var older [][]map[string]any
	var recent [][]map[string]any
	if keepRecent == 0 {
		older = [][]map[string]any{}
		recent = exchanges
	} else {
		older = exchanges[:len(exchanges)-keepRecent]
		recent = exchanges[len(exchanges)-keepRecent:]
	}
	var projections []ProjectedExchange
	for _, exchange := range older {
		proj := projectExchange(exchange)
		if proj != nil {
			projections = append(projections, *proj)
		}
	}

	if len(projections) == 0 {
		return messages
	}
	collapsedText := buildProjectionText(projections)
	result := []map[string]any{
		map[string]any{
			"role": "user",
			"content": []map[string]any{
				map[string]any{
					"type": "text",
					"text": collapsedText,
				},
			},
		},
	}

	for _, exchange := range recent {
		result = append(result, exchange...)
	}
	return result
}

func CollapseMessagesDefault(messages []map[string]any) []map[string]any {
	return CollapseMessages(messages, defaultKeepRecent)
}

func findUncollapsedSpan(messages []map[string]any, existingRegions []CollapsedRegion, minSize, keepRecent int) (int, int) {
	if minSize <= 0 {
		minSize = minRegionSize
	}
	if keepRecent <= 0 {
		keepRecent = defaultKeepRecentMessage
	}
	collapsed := mapset.NewSet[int]()
	for _, region := range existingRegions {
		for i := region.StartIdx; i < region.EndIdx; i++ {
			collapsed.Add(i)
		}
	}
	searchLimit := MaxInt(0, len(messages)-keepRecent)
	runStart := -1
	for i := 0; i < searchLimit; i++ {
		if !collapsed.Contains(i) {
			if runStart == -1 {
				runStart = i
			}
		} else {
			if runStart != -1 && (i-runStart) >= minSize {
				return runStart, i
			}
			runStart = -1
		}
	}
	if runStart != -1 && searchLimit-runStart >= minSize {
		return runStart, searchLimit
	}
	return -1, -1
}

func ProjectCollapsedView(messages []map[string]any, regions []CollapsedRegion) []map[string]any {
	if regions == nil || len(regions) == 0 {
		return deepcopy.Copy(messages).([]map[string]any)
	}
	sortedRegions := append([]CollapsedRegion(nil), regions...)
	sort.Slice(sortedRegions, func(i, j int) bool {
		return sortedRegions[i].StartIdx < sortedRegions[j].StartIdx
	})
	insertMap := map[int]string{}
	skipIndices := mapset.NewSet[int]()
	for _, region := range sortedRegions {
		insertMap[region.StartIdx] = region.Summary
		for i := region.StartIdx; i < region.EndIdx; i++ {
			skipIndices.Add(i)
		}
	}

	var result []map[string]any
	for i, msg := range messages {
		if skipIndices.Contains(i) {
			if _, ok := insertMap[i]; ok {
				result = append(result, map[string]any{
					"role": "user",
					"content": []map[string]any{
						map[string]any{
							"type": "text",
							"text": insertMap[i],
						},
					},
				})
			}
			continue
		}
		result = append(result, deepcopy.Copy(msg).(map[string]any))
	}
	return result
}

func ApplyCollapseIfNeed(messages []map[string]any, currentTokens, collapseThresholdTokens int, existingRegions []CollapsedRegion) (bool, []CollapsedRegion) {
	var regions []CollapsedRegion
	if existingRegions == nil || len(existingRegions) == 0 {
		regions = []CollapsedRegion{}
	} else {
		regions = existingRegions
	}
	if currentTokens < collapseThresholdTokens {
		return false, regions
	}

	start, end := findUncollapsedSpan(messages, regions, 0, defaultKeepRecentMessage)
	if start == -1 || end == -1 || end <= start || end > len(messages) {
		return false, regions
	}
	msgCount := end - start
	summary := fmt.Sprintf(
		"[System: %d turns of debugging logs collapsed "+
			"for context efficiency. Original messages %d–%d "+
			"contained verbose tool output that has been folded.]", msgCount, start, end-1)
	spanMessages := messages[start:end]
	var projections []ProjectedExchange
	for _, exchange := range partitionExchanges(spanMessages) {
		projection := projectExchange(exchange)
		if projection != nil {
			projections = append(projections, *projection)
		}
	}
	if projections != nil && len(projections) > 0 {
		summary = buildProjectionText(projections)
	}
	regions = append(regions, CollapsedRegion{start, end, summary})
	return true, regions
}
