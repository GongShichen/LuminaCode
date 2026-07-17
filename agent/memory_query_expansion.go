package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"LuminaCode/api"
	"LuminaCode/config"
	"LuminaCode/longmemory"

	"github.com/araddon/dateparse"
)

type MemoryExpansionClientFactory func(context.Context, string) (api.LLMClient, error)

func expandMemoryQuery(ctx context.Context, cfg config.Config, query longmemory.MemoryQuery, catalog longmemory.MemoryCatalog, factory MemoryExpansionClientFactory) (longmemory.QueryExpansion, string, string) {
	if !cfg.MemoryQueryExpansionEnabled || factory == nil {
		return longmemory.QueryExpansion{}, "", ""
	}
	model := strings.TrimSpace(cfg.MemoryQueryExpansionModel)
	if model == "" || strings.EqualFold(model, "inherit") {
		model = cfg.APIModel
	}
	client, err := factory(ctx, model)
	if err != nil {
		return longmemory.QueryExpansion{}, model, err.Error()
	}
	expansionCtx := ctx
	cancel := func() {}
	if cfg.MemoryQueryExpansionTimeoutSeconds > 0 {
		expansionCtx, cancel = context.WithTimeout(ctx,
			time.Duration(cfg.MemoryQueryExpansionTimeoutSeconds*float64(time.Second)))
	}
	defer cancel()
	expansionCtx = api.ContextWithStreamIdleTimeout(expansionCtx,
		time.Duration(cfg.APIStreamIdleTimeoutSeconds*float64(time.Second)))
	payload := map[string]any{
		"query":          query.Text,
		"recent_context": query.RecentContext,
		"current_time":   query.Timestamp.Format(time.RFC3339),
		"memory_catalog": catalog,
	}
	encoded, _ := json.Marshal(payload)
	system := "Expand a memory retrieval query without selecting retrieval channels or filtering memory. " +
		"Use the ExpandMemoryQuery tool exactly once. Return one facet for each independent piece of evidence needed; " +
		"a simple lookup may use one facet, while any derived answer must separate every factual operand or reference point. " +
		"Facets describe retrievable facts, not the final calculation, and must not be paraphrases of one another. " +
		"Queries are only retrieval rewrites and must not substitute for facets. Preserve uncertainty and do not invent facts."
	tool := memoryExpansionToolSchema(cfg.MemoryQueryExpansionMaxQueries)
	if structured, ok := client.(api.StructuredCompletionClient); ok {
		response, structuredErr := structured.CompleteStructured(expansionCtx, system,
			[]map[string]any{{"role": "user", "content": string(encoded)}}, api.StructuredCompletionOptions{
				MaxTokens: 512, Tools: []map[string]any{tool}, RequiredTool: "ExpandMemoryQuery", DisableThinking: true,
			})
		if structuredErr == nil {
			input := expansionToolInput(response.ToolCalls)
			parseMode := "structured_tool"
			if input == nil {
				input = parseExpansionJSONContent(response.Text)
				parseMode = "structured_json"
			}
			if input != nil {
				expansion, parseErr := parseQueryExpansion(input, cfg.MemoryQueryExpansionMaxQueries)
				if parseErr == nil {
					expansion.ParseMode = parseMode
					return expansion, model, ""
				}
			}
		}
		if structuredErr != nil && expansionCtx.Err() != nil {
			return longmemory.QueryExpansion{}, model, structuredErr.Error()
		}
	}
	var input map[string]any
	var textOutput strings.Builder
	parseMode := "tool_call"
	var streamErr error
	for result := range client.StreamChat(expansionCtx, system,
		[]map[string]any{{"role": "user", "content": string(encoded)}}, []map[string]any{tool}, nil) {
		if result.Err != nil {
			streamErr = result.Err
			continue
		}
		if stringFromAny(result.Event["type"]) == "error" {
			streamErr = fmt.Errorf("%s", stringFromAny(result.Event["message"]))
			continue
		}
		if stringFromAny(result.Event["type"]) == "tool_use" &&
			strings.EqualFold(stringFromAny(result.Event["name"]), "ExpandMemoryQuery") {
			input, _ = result.Event["input"].(map[string]any)
		}
		if eventType := stringFromAny(result.Event["type"]); eventType == "text" || eventType == "text_delta" || eventType == "content_block_delta" {
			textOutput.WriteString(stringFromAny(result.Event["text"]))
		}
	}
	if streamErr != nil {
		return longmemory.QueryExpansion{}, model, streamErr.Error()
	}
	if input == nil {
		if parsed := parseExpansionJSONContent(textOutput.String()); parsed != nil {
			input = parsed
			parseMode = "json_content"
		} else {
			return longmemory.QueryExpansion{}, model, "query expansion model returned neither ExpandMemoryQuery nor valid JSON content"
		}
	}
	expansion, err := parseQueryExpansion(input, cfg.MemoryQueryExpansionMaxQueries)
	if err != nil {
		return longmemory.QueryExpansion{}, model, err.Error()
	}
	expansion.ParseMode = parseMode
	return expansion, model, ""
}

func expansionToolInput(calls []map[string]any) map[string]any {
	for _, call := range calls {
		if !strings.EqualFold(stringFromAny(call["name"]), "ExpandMemoryQuery") {
			continue
		}
		if input, ok := call["input"].(map[string]any); ok {
			return input
		}
	}
	return nil
}

func ExpandMemoryQuery(ctx context.Context, cfg config.Config, query longmemory.MemoryQuery, catalog longmemory.MemoryCatalog, factory MemoryExpansionClientFactory) (longmemory.QueryExpansion, string, string) {
	return expandMemoryQuery(ctx, cfg, query, catalog, factory)
}

func memoryExpansionToolSchema(maxQueries int) map[string]any {
	if maxQueries <= 0 {
		maxQueries = 5
	}
	return map[string]any{
		"name":        "ExpandMemoryQuery",
		"description": "Generate generic query rewrites and non-overlapping atomic evidence facets sufficient to derive an answer, plus normalized entities, time constraints and relation terms. It cannot select retrieval channels, scopes, memory types, limits or memory IDs.",
		"input_schema": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []string{"facets"},
			"properties": map[string]any{
				"facets": map[string]any{"type": "array", "minItems": 1, "maxItems": 8,
					"items": map[string]any{"type": "object", "additionalProperties": false,
						"required": []string{"text"}, "properties": map[string]any{
							"text":                 map[string]any{"type": "string"},
							"entities":             map[string]any{"type": "array", "maxItems": 8, "items": map[string]any{"type": "string"}},
							"relations":            map[string]any{"type": "array", "maxItems": 8, "items": map[string]any{"type": "string"}},
							"temporal_constraints": temporalConstraintSchema(2),
							"provenance_hints":     map[string]any{"type": "array", "maxItems": 4, "items": map[string]any{"type": "string"}},
						}}},
				"queries": map[string]any{"type": "array", "maxItems": maxQueries,
					"items": map[string]any{"type": "string"}},
				"entities": map[string]any{"type": "array", "maxItems": 16,
					"items": map[string]any{"type": "string"}},
				"temporal_constraints": temporalConstraintSchema(4),
				"relation_terms": map[string]any{"type": "array", "maxItems": 16,
					"items": map[string]any{"type": "string"}},
				"provenance_hints": map[string]any{"type": "array", "maxItems": 8,
					"items": map[string]any{"type": "string"}},
			},
		},
	}
}

func temporalConstraintSchema(maxItems int) map[string]any {
	return map[string]any{"type": "array", "maxItems": maxItems,
		"items": map[string]any{"type": "object", "additionalProperties": false,
			"properties": map[string]any{
				"from": map[string]any{"type": "string"}, "to": map[string]any{"type": "string"},
				"at": map[string]any{"type": "string"}, "order": map[string]any{"type": "string", "enum": []string{"asc", "desc", "none"}},
			}}}
}

func parseQueryExpansion(input map[string]any, maxQueries int) (longmemory.QueryExpansion, error) {
	if maxQueries <= 0 {
		maxQueries = 5
	}
	expansion := longmemory.QueryExpansion{
		Queries:         stringList(input["queries"], maxQueries),
		Entities:        stringList(input["entities"], 16),
		RelationTerms:   stringList(input["relation_terms"], 16),
		ProvenanceHints: stringList(input["provenance_hints"], 8),
	}
	allowed := map[string]bool{"queries": true, "facets": true, "entities": true,
		"temporal_constraints": true, "relation_terms": true, "provenance_hints": true}
	for key := range input {
		if !allowed[key] {
			expansion.Diagnostics = append(expansion.Diagnostics, "ignored expansion field: "+key)
		}
	}
	sort.Strings(expansion.Diagnostics)
	if rawConstraints, ok := input["temporal_constraints"].([]any); ok {
		if len(rawConstraints) > 4 {
			rawConstraints = rawConstraints[:4]
		}
		for _, raw := range rawConstraints {
			value, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			constraint := longmemory.TemporalConstraint{Order: strings.ToLower(strings.TrimSpace(stringFromAny(value["order"])))}
			if constraint.Order == "" {
				constraint.Order = "none"
			}
			if constraint.Order != "none" && constraint.Order != "asc" && constraint.Order != "desc" {
				constraint.Order = "none"
			}
			constraint.FromText = strings.TrimSpace(stringFromAny(value["from"]))
			constraint.ToText = strings.TrimSpace(stringFromAny(value["to"]))
			constraint.AtText = strings.TrimSpace(stringFromAny(value["at"]))
			constraint.From, _ = parseExpansionTime(value["from"])
			constraint.To, _ = parseExpansionTime(value["to"])
			constraint.At, _ = parseExpansionTime(value["at"])
			expansion.TemporalConstraints = append(expansion.TemporalConstraints, constraint)
		}
	}
	if rawFacets, ok := input["facets"].([]any); ok {
		if len(rawFacets) > 8 {
			rawFacets = rawFacets[:8]
		}
		for _, raw := range rawFacets {
			value, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			text := strings.TrimSpace(stringFromAny(value["text"]))
			if text == "" {
				continue
			}
			expansion.Facets = append(expansion.Facets, longmemory.FacetDraft{Text: text,
				Entities: stringList(value["entities"], 8), Relations: stringList(value["relations"], 8),
				TemporalConstraints: parseFacetTemporalConstraints(value["temporal_constraints"], 2),
				ProvenanceHints:     stringList(value["provenance_hints"], 4)})
		}
	}
	for _, facet := range expansion.Facets {
		expansion.Entities = append(expansion.Entities, facet.Entities...)
		expansion.RelationTerms = append(expansion.RelationTerms, facet.Relations...)
		expansion.TemporalConstraints = append(expansion.TemporalConstraints, facet.TemporalConstraints...)
		expansion.ProvenanceHints = append(expansion.ProvenanceHints, facet.ProvenanceHints...)
	}
	expansion.Entities = stringList(expansion.Entities, 16)
	expansion.RelationTerms = stringList(expansion.RelationTerms, 16)
	expansion.ProvenanceHints = stringList(expansion.ProvenanceHints, 8)
	return expansion, nil
}

func parseFacetTemporalConstraints(raw any, limit int) []longmemory.TemporalConstraint {
	values, ok := raw.([]any)
	if !ok {
		return nil
	}
	if len(values) > limit {
		values = values[:limit]
	}
	result := make([]longmemory.TemporalConstraint, 0, len(values))
	for _, rawValue := range values {
		value, ok := rawValue.(map[string]any)
		if !ok {
			continue
		}
		constraint := longmemory.TemporalConstraint{Order: strings.ToLower(strings.TrimSpace(stringFromAny(value["order"])))}
		if constraint.Order != "asc" && constraint.Order != "desc" {
			constraint.Order = "none"
		}
		constraint.FromText = strings.TrimSpace(stringFromAny(value["from"]))
		constraint.ToText = strings.TrimSpace(stringFromAny(value["to"]))
		constraint.AtText = strings.TrimSpace(stringFromAny(value["at"]))
		constraint.From, _ = parseExpansionTime(value["from"])
		constraint.To, _ = parseExpansionTime(value["to"])
		constraint.At, _ = parseExpansionTime(value["at"])
		result = append(result, constraint)
	}
	return result
}

func parseExpansionJSONContent(value string) map[string]any {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "```json")
	value = strings.TrimPrefix(value, "```")
	value = strings.TrimSuffix(value, "```")
	value = strings.TrimSpace(value)
	start, end := strings.Index(value, "{"), strings.LastIndex(value, "}")
	if start < 0 || end < start {
		return nil
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(value[start:end+1]), &result); err != nil {
		return nil
	}
	return result
}

func parseExpansionTime(value any) (time.Time, error) {
	text := strings.TrimSpace(stringFromAny(value))
	if text == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, text)
	if err != nil {
		parsed, err = dateparse.ParseAny(text)
		if err != nil {
			return time.Time{}, err
		}
	}
	return parsed.UTC(), nil
}

func stringList(value any, limit int) []string {
	var values []string
	switch typed := value.(type) {
	case []any:
		for _, item := range typed {
			if text := strings.TrimSpace(stringFromAny(item)); text != "" {
				values = append(values, text)
			}
		}
	case []string:
		values = append(values, typed...)
	}
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, value)
		if len(result) >= limit {
			break
		}
	}
	sort.Strings(result)
	return result
}
