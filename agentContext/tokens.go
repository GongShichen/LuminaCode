package agentContext

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

func RoughEstimate(source any) int {
	switch v := source.(type) {
	case string:
		return len([]rune(v)) / 4
	case []map[string]any:
		total := 0
		for _, msg := range v {
			content := msg["content"]
			switch c := content.(type) {
			case []map[string]any:
				for _, block := range c {
					total += len([]rune(pythonRepr(block))) / 4
				}
			case []any:
				for _, rawBlock := range c {
					block, ok := rawBlock.(map[string]any)
					if !ok {
						continue
					}
					total += len([]rune(pythonRepr(block))) / 4
				}

			case string:
				total += len([]rune(c)) / 4
			}
		}
		return total
	default:
		return 0
	}
}

func pythonRepr(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case string:
		return pythonStringRepr(v)
	case bool:
		if v {
			return "True"
		}
		return "False"
	case int, int32, int64, float32, float64, json.Number:
		return strings.TrimSpace(fmtAny(v))
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, pythonStringRepr(key)+": "+pythonRepr(v[key]))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, pythonRepr(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case []map[string]any:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, pythonRepr(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		return pythonStringRepr(fmtAny(v))
	}
}

func pythonStringRepr(value string) string {
	replacer := strings.NewReplacer(
		"\\", "\\\\",
		"'", "\\'",
		"\n", "\\n",
		"\r", "\\r",
		"\t", "\\t",
	)
	return "'" + replacer.Replace(value) + "'"
}

func fmtAny(value any) string {
	data, err := json.Marshal(value)
	if err == nil {
		var decoded any
		if json.Unmarshal(data, &decoded) == nil {
			switch n := decoded.(type) {
			case float64:
				if n == float64(int64(n)) {
					return strconv.FormatInt(int64(n), 10)
				}
			}
		}
		return string(data)
	}
	return ""
}

func TokenCountWithEstimation(messages []map[string]any) int {
	if len(messages) == 0 {
		return 0
	}

	n := len(messages)

	// Step 1: Find the last anchor: assistant message with usage.input_tokens.
	anchorIdx := -1

	for i := n - 1; i >= 0; i-- {
		msg := messages[i]

		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}

		usage, ok := msg["usage"].(map[string]any)
		if !ok {
			continue
		}

		if _, exists := usage["input_tokens"]; exists {
			anchorIdx = i
			break
		}
	}

	if anchorIdx == -1 {
		return RoughEstimate(messages)
	}

	// Step 2: Extend anchor backwards for split responses with same id.
	anchorMsg := messages[anchorIdx]
	anchorID, hasAnchorID := anchorMsg["id"]

	if hasAnchorID && anchorID != nil {
		for anchorIdx > 0 {
			prev := messages[anchorIdx-1]

			prevID, ok := prev["id"]
			if !ok || prevID != anchorID {
				break
			}

			anchorIdx--
		}
	}

	// Step 3: Exact anchor count.
	usage, ok := messages[anchorIdx]["usage"].(map[string]any)
	if !ok {
		usage = map[string]any{}
	}

	// Walk forward within the split group to find the message that actually has usage.
	for j := anchorIdx; j < n; j++ {
		u, ok := messages[j]["usage"].(map[string]any)
		if !ok {
			continue
		}

		if _, exists := u["input_tokens"]; exists {
			usage = u
			break
		}
	}

	exactBase := getInt(usage, "input_tokens") + getInt(usage, "output_tokens")

	// Step 3b: Find end of anchor group.
	anchorEndIdx := anchorIdx

	if hasAnchorID && anchorID != nil {
		for j := anchorIdx + 1; j < n; j++ {
			id, ok := messages[j]["id"]
			if !ok || id != anchorID {
				break
			}

			anchorEndIdx = j
		}
	}

	// Step 4: Estimate messages after anchor group.
	postAnchor := messages[anchorEndIdx+1:]
	delta := RoughEstimate(postAnchor)

	return exactBase + delta
}

func getInt(m map[string]any, key string) int {
	if m == nil {
		return 0
	}

	switch v := m[key].(type) {
	case int:
		return v
	case int32:
		return int(v)
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		i, _ := v.Int64()
		return int(i)
	default:
		return 0
	}
}
