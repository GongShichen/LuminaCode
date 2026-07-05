package tools

import (
	"fmt"
	"sort"
	"strings"
)

func RenderMCPDynamicToolUse(serverName, toolName string, args map[string]any) string {
	argsSummary := "no args"
	if len(args) > 0 {
		keys := make([]string, 0, len(args))
		for key := range args {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+"="+truncateString(fmt.Sprint(args[key]), 40))
		}
		argsSummary = strings.Join(parts, ", ")
	}
	return "MCP:" + serverName + "/" + toolName + "(" + argsSummary + ")"
}

func RenderMCPDynamicToolResult(content string, isError bool) string {
	if isError {
		return "MCP failed: " + truncateString(content, 150)
	}
	if strings.Contains(content, "Full output saved to:") {
		return fmt.Sprintf("MCP output saved (%d chars)", len([]rune(content)))
	}
	return fmt.Sprintf("MCP result (%d chars)", len([]rune(content)))
}

func RenderListMCPResourcesToolUse() string {
	return "MCP: list_resources"
}

func RenderListMCPResourcesToolResult(content string, isError bool) string {
	if isError {
		return "MCP list_resources failed: " + truncateString(content, 100)
	}
	count := 0
	if content != "" {
		count = strings.Count(content, "\n") + 1
	}
	return fmt.Sprintf("MCP resources (%d entries)", count)
}

func RenderReadMCPResourceToolUse(input ReadMCPResourceInput) string {
	return "MCP: read " + input.URI
}

func RenderReadMCPResourceToolResult(content string, isError bool) string {
	if isError {
		return "MCP read_resource failed: " + truncateString(content, 100)
	}
	return fmt.Sprintf("MCP resource (%d chars)", len([]rune(content)))
}
