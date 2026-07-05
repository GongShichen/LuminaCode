package mcp

import "fmt"

func ExtractContent(parts []map[string]any) string {
	if len(parts) == 0 {
		return ""
	}
	lines := make([]string, 0, len(parts))
	for _, item := range parts {
		itemType, _ := item["type"].(string)
		if itemType == "" {
			itemType = "text"
		}
		switch itemType {
		case "text":
			if text, ok := item["text"].(string); ok {
				lines = append(lines, text)
			} else {
				lines = append(lines, "")
			}
		case "image":
			data, _ := item["data"].(string)
			mime, _ := item["mimeType"].(string)
			if mime == "" {
				mime = "image/png"
			}
			lines = append(lines, fmt.Sprintf("[Image: %s, %d bytes base64]", mime, len(data)))
		case "resource":
			uri := "?"
			if resource, ok := item["resource"].(map[string]any); ok {
				if raw, ok := resource["uri"].(string); ok && raw != "" {
					uri = raw
				}
			} else if resource != nil {
				uri = fmt.Sprint(resource)
			}
			lines = append(lines, "[Resource: "+uri+"]")
		default:
			lines = append(lines, "[Unknown content type: "+itemType+"]")
		}
	}
	return joinLines(lines)
}

func joinLines(lines []string) string {
	if len(lines) == 0 {
		return ""
	}
	out := lines[0]
	for _, line := range lines[1:] {
		out += "\n" + line
	}
	return out
}
