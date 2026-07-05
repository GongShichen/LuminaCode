package agentContext

func contentBlocks(content any) ([]map[string]any, bool) {
	switch blocks := content.(type) {
	case []map[string]any:
		return blocks, true
	case []any:
		out := make([]map[string]any, 0, len(blocks))
		for _, raw := range blocks {
			block, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			out = append(out, block)
		}
		return out, true
	default:
		return nil, false
	}
}

func contentLike(original any, blocks []map[string]any) any {
	if _, ok := original.([]any); ok {
		out := make([]any, 0, len(blocks))
		for _, block := range blocks {
			out = append(out, block)
		}
		return out
	}
	return blocks
}
