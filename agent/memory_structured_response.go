package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"LuminaCode/api"
)

func structuredMemoryInput(response api.Response, requiredTool string) (map[string]any, error) {
	for _, call := range response.ToolCalls {
		if !strings.EqualFold(stringFromAny(call["name"]), requiredTool) {
			continue
		}
		input, ok := call["input"].(map[string]any)
		if !ok || input == nil {
			return nil, fmt.Errorf("%s returned invalid tool input", requiredTool)
		}
		return input, nil
	}
	text := strings.TrimSpace(response.Text)
	if strings.HasPrefix(text, "```") {
		if lineEnd := strings.IndexByte(text, '\n'); lineEnd >= 0 {
			text = text[lineEnd+1:]
		}
		text = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
	}
	var input map[string]any
	if text == "" || json.Unmarshal([]byte(text), &input) != nil || input == nil {
		return nil, fmt.Errorf("memory model returned neither %s nor a strict JSON object", requiredTool)
	}
	return input, nil
}

func normalizeStructuredJSONArrayFields(input map[string]any, fields ...string) (map[string]any, error) {
	if input == nil {
		return nil, errors.New("structured memory input is nil")
	}
	normalized := make(map[string]any, len(input))
	for key, value := range input {
		normalized[key] = value
	}
	for _, field := range fields {
		value, exists := normalized[field]
		if !exists {
			continue
		}
		decoded, err := decodeStructuredJSONArray(value)
		if err != nil {
			if text, ok := value.(string); ok {
				if extracted := extractStructuredFieldObjects(text, field); len(extracted) > 0 {
					decoded, err = extracted, nil
				}
			}
		}
		if err != nil {
			return nil, fmt.Errorf("decode structured memory field %q: %w", field, err)
		}
		normalized[field] = decoded
	}
	return normalized, nil
}

func extractStructuredFieldObjects(text, field string) []any {
	type locatedObject struct {
		start int
		value map[string]any
	}
	var starts []int
	var objects []locatedObject
	inString, escaped := false, false
	for index := 0; index < len(text); index++ {
		switch current := text[index]; {
		case inString && escaped:
			escaped = false
		case inString && current == '\\':
			escaped = true
		case current == '"':
			inString = !inString
		case !inString && current == '{':
			starts = append(starts, index)
		case !inString && current == '}' && len(starts) > 0:
			start := starts[len(starts)-1]
			starts = starts[:len(starts)-1]
			var candidate map[string]any
			if json.Unmarshal([]byte(text[start:index+1]), &candidate) == nil && structuredFieldObject(field, candidate) {
				objects = append(objects, locatedObject{start: start, value: candidate})
			}
		}
	}
	sort.SliceStable(objects, func(i, j int) bool { return objects[i].start < objects[j].start })
	result := make([]any, 0, len(objects))
	for _, object := range objects {
		result = append(result, object.value)
	}
	return result
}

func structuredFieldObject(field string, value map[string]any) bool {
	switch field {
	case "nodes":
		_, hasKind := value["kind"]
		_, hasStatement := value["statement"]
		_, hasSources := value["sources"]
		return hasKind && hasStatement && hasSources
	case "aliases":
		_, hasCanonical := value["canonical"]
		_, hasAliases := value["aliases"]
		return hasCanonical && hasAliases
	default:
		return false
	}
}

func decodeStructuredJSONArray(value any) (any, error) {
	for depth := 0; depth < 3; depth++ {
		text, ok := value.(string)
		if !ok {
			break
		}
		text = strings.TrimSpace(text)
		if text == "" {
			return nil, errors.New("empty JSON array string")
		}
		decoded, err := decodeStructuredJSONArrayText(text)
		if err != nil {
			return nil, err
		}
		value = decoded
	}
	if _, ok := value.([]any); !ok {
		return nil, fmt.Errorf("expected JSON array, got %T", value)
	}
	return value, nil
}

func decodeStructuredJSONArrayText(text string) (any, error) {
	text = trimStructuredJSONFence(strings.TrimSpace(text))
	var decoded any
	if err := json.Unmarshal([]byte(text), &decoded); err == nil {
		switch typed := decoded.(type) {
		case []any:
			return typed, nil
		case map[string]any:
			if nested, exists := typed["nodes"]; exists {
				return decodeStructuredJSONArray(nested)
			}
			return []any{typed}, nil
		case string:
			return typed, nil
		default:
			return nil, fmt.Errorf("expected JSON array or object, got %T", decoded)
		}
	}

	candidates := structuredJSONArrayRepairCandidates(text)
	var lastErr error
	for _, candidate := range candidates {
		var repaired []any
		if err := json.Unmarshal([]byte(candidate), &repaired); err == nil {
			return repaired, nil
		} else {
			lastErr = err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no conservative JSON array repair applies")
	}
	return nil, lastErr
}

func structuredJSONArrayRepairCandidates(text string) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	seen := map[string]struct{}{}
	result := []string{}
	add := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" {
			return
		}
		if _, exists := seen[candidate]; exists {
			return
		}
		seen[candidate] = struct{}{}
		result = append(result, candidate)
	}
	add(repairStructuredJSONDelimiters(text))

	if first := strings.IndexByte(text, '['); first >= 0 {
		if last := strings.LastIndexByte(text, ']'); last > first {
			array := text[first : last+1]
			add(array)
			add(repairStructuredJSONDelimiters(array))
		}
	}
	if strings.HasPrefix(text, "{{") && strings.HasSuffix(text, "}}") {
		add("[" + text[1:len(text)-1] + "]")
	}
	if strings.HasPrefix(text, "{") && strings.HasSuffix(text, "}") {
		add("[" + text + "]")
		add("[" + text[1:len(text)-1] + "]")
	}
	if strings.HasPrefix(text, "{") {
		sequence := strings.ReplaceAll(text, "}\n{", "},{")
		sequence = strings.ReplaceAll(sequence, "}\r\n{", "},{")
		add("[" + sequence + "]")
	}
	return result
}

// Some structured-model providers occasionally emit a colon where JSON
// requires a comma between completed values or object fields. Repair only
// those impossible JSON boundaries and leave all quoted content untouched.
func repairStructuredJSONDelimiters(text string) string {
	var repaired strings.Builder
	repaired.Grow(len(text))
	inString, escaped := false, false
	for index := 0; index < len(text); index++ {
		current := text[index]
		if inString {
			repaired.WriteByte(current)
			switch {
			case escaped:
				escaped = false
			case current == '\\':
				escaped = true
			case current == '"':
				inString = false
			}
			continue
		}
		if current == '"' {
			inString = true
			repaired.WriteByte(current)
			continue
		}
		if current == ':' {
			previous := previousNonSpaceByte(text, index)
			next := nextNonSpaceByte(text, index+1)
			if (previous == '}' || previous == ']') && (next == '{' || next == '[' || next == '"') {
				repaired.WriteByte(',')
				continue
			}
		}
		repaired.WriteByte(current)
	}
	return repaired.String()
}

func previousNonSpaceByte(text string, before int) byte {
	for index := before - 1; index >= 0; index-- {
		if !isStructuredJSONSpace(text[index]) {
			return text[index]
		}
	}
	return 0
}

func nextNonSpaceByte(text string, from int) byte {
	for index := from; index < len(text); index++ {
		if !isStructuredJSONSpace(text[index]) {
			return text[index]
		}
	}
	return 0
}

func isStructuredJSONSpace(value byte) bool {
	switch value {
	case ' ', '\t', '\r', '\n':
		return true
	default:
		return false
	}
}

func trimStructuredJSONFence(text string) string {
	if !strings.HasPrefix(text, "```") {
		return text
	}
	if lineEnd := strings.IndexByte(text, '\n'); lineEnd >= 0 {
		text = text[lineEnd+1:]
	}
	return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(text), "```"))
}
