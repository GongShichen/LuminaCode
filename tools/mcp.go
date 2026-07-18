package tools

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"LuminaCode/config"
	"LuminaCode/mcp"

	jsonschemavalidator "github.com/santhosh-tekuri/jsonschema/v6"
)

type MCPDynamicTool struct {
	BaseTool
	ServerName string
	ToolName   string
	RawSchema  map[string]any
}

func NewMCPDynamicTool(publicName, serverName, toolName, description string, rawSchema map[string]any, searchHint string) *MCPDynamicTool {
	if rawSchema == nil {
		rawSchema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return &MCPDynamicTool{
		BaseTool: BaseTool{Spec: ToolSpec{
			Name:            publicName,
			Description:     description,
			InputPrototype:  nil,
			ShouldDefer:     true,
			SearchHint:      searchHint,
			ReadOnly:        BoolPtr(false),
			ConcurrencySafe: BoolPtr(true),
			Destructive:     BoolPtr(true),
			TimeoutSeconds:  mcp.DefaultRequestTimeout.Seconds() + 15,
			MaxOutputChars:  100_000,
		}},
		ServerName: serverName,
		ToolName:   toolName,
		RawSchema:  rawSchema,
	}
}

func (t *MCPDynamicTool) DecodeInput(raw map[string]any) (any, error) {
	schema := BuildMCPInputSchema(t.ToolName, t.RawSchema)
	normalized := coerceMCPInputForSchema(raw, schema)
	compiler := jsonschemavalidator.NewCompiler()
	const schemaURL = "lumina://mcp-input.schema.json"
	if err := compiler.AddResource(schemaURL, schema); err != nil {
		return nil, err
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		return nil, err
	}
	if err := compiled.Validate(normalized); err != nil {
		return nil, err
	}
	return filterMCPInputForModelDump(normalized, t.RawSchema), nil
}

func (t *MCPDynamicTool) Execute(ctx context.Context, execCtx ExecutionContext, input any) (string, error) {
	client := mcpClientFromContext(execCtx, t.ServerName)
	if client == nil {
		return fmt.Sprintf(
			"<tool_use_error>\nMCP server '%s' is not connected. It may have failed to start or been disconnected.\n</tool_use_error>",
			t.ServerName,
		), nil
	}
	args, _ := input.(map[string]any)
	output, err := client.CallTool(ctx, t.ToolName, args)
	if err != nil {
		if mcpErr, ok := err.(mcp.McpError); ok {
			return fmt.Sprintf(
				"<tool_use_error>\nMCP tool '%s' on server '%s' failed: [%d] %s\n</tool_use_error>",
				t.ToolName,
				t.ServerName,
				mcpErr.Code,
				mcpErr.Message,
			), nil
		}
		return fmt.Sprintf(
			"<tool_use_error>\nMCP tool '%s' on server '%s' failed: %s\n</tool_use_error>",
			t.ToolName,
			t.ServerName,
			err,
		), nil
	}
	return output, nil
}

func (t *MCPDynamicTool) ToAPISchema() map[string]any {
	return map[string]any{
		"name":         t.Name(),
		"description":  t.Description(),
		"input_schema": BuildMCPInputSchema(t.ToolName, t.RawSchema),
	}
}

type ListMCPResourcesTool struct{ BaseTool }

func NewListMCPResourcesTool() *ListMCPResourcesTool {
	return &ListMCPResourcesTool{BaseTool{Spec: ToolSpec{
		Name:            "mcp_list_resources",
		Description:     "List all available resources from connected MCP servers. Resources can include files, data sets, API endpoints, etc. Use this to discover what data sources are available, then use mcp_read_resource to fetch specific resources.",
		Aliases:         []string{"list_mcp_resources"},
		ReadOnly:        BoolPtr(true),
		ConcurrencySafe: BoolPtr(true),
		Destructive:     BoolPtr(false),
		ShouldDefer:     true,
		SearchHint:      "mcp list resources",
		TimeoutSeconds:  mcp.DefaultRequestTimeout.Seconds() + 15,
	}}}
}

func (t *ListMCPResourcesTool) Execute(ctx context.Context, execCtx ExecutionContext, _ any) (string, error) {
	clients := mcpClientsFromContext(execCtx)
	if len(clients) == 0 {
		return "No MCP servers are connected.", nil
	}
	names := orderedMCPClientNames(execCtx, clients)
	var lines []string
	for _, name := range names {
		client := clients[name]
		if client.State() != mcp.ConnectionConnected {
			lines = append(lines, fmt.Sprintf("[%s] (disconnected)", name))
			continue
		}
		resources, err := client.DiscoverResources(ctx)
		if err != nil {
			if mcpErr, ok := err.(mcp.McpError); ok {
				lines = append(lines, fmt.Sprintf("[%s] Error listing resources: %s", name, mcpErr.Message))
			} else {
				lines = append(lines, fmt.Sprintf("[%s] Error listing resources: %s", name, err))
			}
			continue
		}
		if len(resources) == 0 {
			lines = append(lines, fmt.Sprintf("[%s] No resources available", name))
			continue
		}
		for _, resource := range resources {
			uri := stringValueFromMap(resource, "uri", "?")
			desc := stringValueFromMap(resource, "name", stringValueFromMap(resource, "description", uri))
			mime := ""
			if mimeType := stringValueFromMap(resource, "mimeType", ""); mimeType != "" {
				mime = " (" + mimeType + ")"
			}
			lines = append(lines, fmt.Sprintf("[%s] %s%s - %s", name, desc, mime, uri))
		}
	}
	if len(lines) == 0 {
		return "No MCP resources available.", nil
	}
	return strings.Join(lines, "\n"), nil
}

type ReadMCPResourceInput struct {
	ServerName string `json:"server_name" jsonschema:"description=MCP server name"`
	URI        string `json:"uri" jsonschema:"description=Resource URI to read"`
}

type ReadMCPResourceTool struct{ BaseTool }

func NewReadMCPResourceTool() *ReadMCPResourceTool {
	return &ReadMCPResourceTool{BaseTool{Spec: ToolSpec{
		Name:            "mcp_read_resource",
		Description:     "Read the content of a specific MCP resource by its URI. Use mcp_list_resources first to discover available resources, then use this tool to fetch the content of one you need.",
		InputPrototype:  ReadMCPResourceInput{},
		Aliases:         []string{"read_mcp_resource"},
		ReadOnly:        BoolPtr(true),
		ConcurrencySafe: BoolPtr(true),
		Destructive:     BoolPtr(false),
		ShouldDefer:     true,
		SearchHint:      "mcp resource read fetch uri",
		TimeoutSeconds:  mcp.DefaultRequestTimeout.Seconds() + 15,
	}}}
}

func (t *ReadMCPResourceTool) Execute(ctx context.Context, execCtx ExecutionContext, input any) (string, error) {
	in := deref[ReadMCPResourceInput](input)
	client := mcpClientFromContext(execCtx, in.ServerName)
	if client == nil {
		return fmt.Sprintf("<tool_use_error>\nMCP server '%s' is not connected.\n</tool_use_error>", in.ServerName), nil
	}
	output, err := client.ReadResource(ctx, in.URI)
	if err != nil {
		if mcpErr, ok := err.(mcp.McpError); ok {
			return fmt.Sprintf(
				"<tool_use_error>\nError reading resource '%s' on server '%s': [%d] %s\n</tool_use_error>",
				in.URI,
				in.ServerName,
				mcpErr.Code,
				mcpErr.Message,
			), nil
		}
		return fmt.Sprintf(
			"<tool_use_error>\nError reading resource '%s' on server '%s': %s\n</tool_use_error>",
			in.URI,
			in.ServerName,
			err,
		), nil
	}
	return output, nil
}

func RegisterMCPTools(registry *ToolRegistry, cwd string, execCtx ExecutionContext) error {
	if registry == nil {
		return nil
	}
	configs := trustedMCPConfigs(cwd, execCtx)
	if len(configs) == 0 {
		return nil
	}
	clients := map[string]*mcp.McpClient{}
	execCtx["mcp_clients"] = clients
	var clientOrder []string
	registeredCount := 0
	for _, cfg := range configs {
		client := mcp.NewMcpClient(cfg)
		if !client.Connect(context.Background()) {
			continue
		}
		discovered, err := client.DiscoverTools(context.Background())
		if err != nil {
			client.Disconnect(context.Background())
			continue
		}
		clients[cfg.Name] = client
		clientOrder = append(clientOrder, cfg.Name)
		for _, toolDef := range discovered {
			toolName := stringValueFromMap(toolDef, "name", "")
			if toolName == "" {
				continue
			}
			safeName := SanitizeMCPToolName(cfg.Name, toolName)
			schema, _ := toolDef["inputSchema"].(map[string]any)
			description := stringValueFromMap(toolDef, "description", "MCP tool: "+toolName)
			registry.Register(NewMCPDynamicTool(
				safeName,
				cfg.Name,
				toolName,
				fmt.Sprintf("[MCP Server: %s] %s", cfg.Name, description),
				schema,
				fmt.Sprintf("mcp %s %s %s", cfg.Name, toolName, description),
			))
			registeredCount++
		}
	}
	if len(clients) > 0 {
		execCtx["mcp_client_order"] = clientOrder
		registry.Register(NewListMCPResourcesTool())
		registry.Register(NewReadMCPResourceTool())
	}
	_ = registeredCount
	return nil
}

func SanitizeMCPToolName(serverName, toolName string) string {
	return "mcp__" + sanitizeMCPPart(serverName) + "__" + sanitizeMCPPart(toolName)
}

var mcpNamePattern = regexp.MustCompile(`[^a-z0-9_]`)

func sanitizeMCPPart(value string) string {
	return mcpNamePattern.ReplaceAllString(strings.ToLower(value), "_")
}

func trustedMCPConfigs(projectRoot string, execCtx ExecutionContext) []mcp.McpServerConfig {
	if cfg, ok := execCtx["config"].(config.Config); ok && strings.TrimSpace(cfg.ProjectPaths.CanonicalRoot) != "" {
		projectRoot = cfg.ProjectPaths.CanonicalRoot
	}
	configs := append([]mcp.McpServerConfig{}, mcp.LoadUserMCPConfig()...)
	projectConfigs := mcp.LoadProjectMCPConfig(projectRoot)
	if len(projectConfigs) == 0 {
		return configs
	}
	trusted := mcp.LoadTrustedMCP(projectRoot)
	var pending []map[string]any
	var trustedProject []mcp.McpServerConfig
	for _, cfg := range projectConfigs {
		fingerprint := cfg.Fingerprint()
		if trusted[cfg.Name] == fingerprint {
			trustedProject = append(trustedProject, cfg)
			continue
		}
		pending = append(pending, map[string]any{
			"name":        cfg.Name,
			"fingerprint": fingerprint,
			"command":     stringPtrValue(cfg.Command),
			"args":        append([]string(nil), cfg.Args...),
			"cwd":         stringPtrValue(cfg.CWD),
			"url":         stringPtrValue(cfg.URL),
		})
	}
	if len(pending) > 0 && execCtx != nil {
		existing, _ := execCtx["pending_mcp_trust"].([]map[string]any)
		execCtx["pending_mcp_trust"] = append(existing, pending...)
	}
	approvals, _ := execCtx["trusted_mcp_servers"].(map[string]string)
	trustedChanged := false
	if approvals != nil {
		for _, cfg := range projectConfigs {
			fingerprint := cfg.Fingerprint()
			if approvals[cfg.Name] == fingerprint {
				trusted[cfg.Name] = fingerprint
				trustedProject = append(trustedProject, cfg)
				trustedChanged = true
			}
		}
	}
	if trustedChanged {
		_ = mcp.SaveTrustedMCP(projectRoot, trusted)
	}
	return append(configs, trustedProject...)
}

func mcpClientsFromContext(execCtx ExecutionContext) map[string]*mcp.McpClient {
	if execCtx == nil {
		return nil
	}
	if clients, ok := execCtx["mcp_clients"].(map[string]*mcp.McpClient); ok {
		return clients
	}
	raw, ok := execCtx["mcp_clients"].(map[string]any)
	if !ok {
		return nil
	}
	out := map[string]*mcp.McpClient{}
	for name, value := range raw {
		if client, ok := value.(*mcp.McpClient); ok {
			out[name] = client
		}
	}
	return out
}

func mcpClientFromContext(execCtx ExecutionContext, name string) *mcp.McpClient {
	return mcpClientsFromContext(execCtx)[name]
}

func orderedMCPClientNames(execCtx ExecutionContext, clients map[string]*mcp.McpClient) []string {
	names := make([]string, 0, len(clients))
	seen := map[string]struct{}{}
	for _, name := range stringListFromAny(execCtx["mcp_client_order"]) {
		if _, ok := clients[name]; ok {
			names = append(names, name)
			seen[name] = struct{}{}
		}
	}
	var rest []string
	for name := range clients {
		if _, ok := seen[name]; !ok {
			rest = append(rest, name)
		}
	}
	sort.Strings(rest)
	return append(names, rest...)
}

func stringListFromAny(raw any) []string {
	switch values := raw.(type) {
	case []string:
		return values
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
}

func coerceMCPInputForSchema(raw map[string]any, schema map[string]any) map[string]any {
	coerced, ok := coerceMCPValueForSchema(raw, schema, schema).(map[string]any)
	if !ok || coerced == nil {
		return raw
	}
	return coerced
}

func coerceMCPValueForSchema(value any, schema map[string]any, root map[string]any) any {
	if value == nil || len(schema) == 0 {
		return value
	}
	if ref, _ := schema["$ref"].(string); ref != "" {
		if resolved := resolveMCPRef(root, ref); len(resolved) > 0 {
			return coerceMCPValueForSchema(value, resolved, root)
		}
		return value
	}
	if variants, ok := schema["anyOf"].([]any); ok {
		for _, variant := range variants {
			variantSchema := mapFromAny(variant)
			if len(variantSchema) == 0 {
				continue
			}
			if typ, _ := variantSchema["type"].(string); typ == "null" {
				continue
			}
			return coerceMCPValueForSchema(value, variantSchema, root)
		}
	}
	jsonType, _ := mcpJSONType(schema["type"], inferMCPType(schema))
	switch jsonType {
	case "object":
		valueMap, ok := value.(map[string]any)
		if !ok {
			return value
		}
		props := mapFromAny(schema["properties"])
		if len(props) == 0 {
			return value
		}
		out := make(map[string]any, len(valueMap))
		for key, rawValue := range valueMap {
			if propSchema := mapFromAny(props[key]); len(propSchema) > 0 {
				out[key] = coerceMCPValueForSchema(rawValue, propSchema, root)
			} else {
				out[key] = rawValue
			}
		}
		return out
	case "array":
		items := mapFromAny(schema["items"])
		if len(items) == 0 {
			return value
		}
		switch typed := value.(type) {
		case []any:
			out := make([]any, 0, len(typed))
			for _, item := range typed {
				out = append(out, coerceMCPValueForSchema(item, items, root))
			}
			return out
		case []string:
			out := make([]any, 0, len(typed))
			for _, item := range typed {
				out = append(out, coerceMCPValueForSchema(item, items, root))
			}
			return out
		default:
			return value
		}
	case "integer":
		return coerceMCPInteger(value)
	case "number":
		return coerceMCPNumber(value)
	case "boolean":
		return coerceMCPBoolean(value)
	default:
		return value
	}
}

func resolveMCPRef(root map[string]any, ref string) map[string]any {
	const prefix = "#/$defs/"
	if !strings.HasPrefix(ref, prefix) {
		return nil
	}
	defs := mapFromAny(root["$defs"])
	return mapFromAny(defs[strings.TrimPrefix(ref, prefix)])
}

func coerceMCPInteger(value any) any {
	switch typed := value.(type) {
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return value
		}
		parsed, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return value
		}
		return parsed
	case float64:
		if typed == float64(int64(typed)) {
			return int64(typed)
		}
		return value
	case float32:
		if typed == float32(int64(typed)) {
			return int64(typed)
		}
		return value
	default:
		return value
	}
}

func coerceMCPNumber(value any) any {
	switch typed := value.(type) {
	case string:
		text := strings.TrimSpace(typed)
		if text == "" {
			return value
		}
		parsed, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return value
		}
		return parsed
	default:
		return value
	}
}

func coerceMCPBoolean(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	switch strings.ToLower(strings.TrimSpace(text)) {
	case "true", "t", "yes", "y", "1":
		return true
	case "false", "f", "no", "n", "0":
		return false
	default:
		return value
	}
}

func copyAnySchema(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func filterMCPInputForModelDump(raw map[string]any, schema map[string]any) map[string]any {
	props := mapFromAny(schema["properties"])
	if len(props) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(props))
	for key, propSchemaAny := range props {
		value, ok := raw[key]
		if !ok || value == nil {
			continue
		}
		propSchema := mapFromAny(propSchemaAny)
		out[key] = filterMCPValueForModelDump(value, propSchema)
	}
	return out
}

func filterMCPValueForModelDump(value any, schema map[string]any) any {
	if value == nil {
		return nil
	}
	jsonType, _ := mcpJSONType(schema["type"], inferMCPType(schema))
	switch jsonType {
	case "object":
		if len(mapFromAny(schema["properties"])) > 0 {
			if valueMap, ok := value.(map[string]any); ok {
				return filterMCPInputForModelDump(valueMap, schema)
			}
		}
	case "array":
		itemSchema := mapFromAny(schema["items"])
		if len(itemSchema) == 0 {
			return value
		}
		if items, ok := value.([]any); ok {
			filtered := make([]any, 0, len(items))
			for _, item := range items {
				filtered = append(filtered, filterMCPValueForModelDump(item, itemSchema))
			}
			return filtered
		}
	}
	return value
}

func stringValueFromMap(m map[string]any, key, fallback string) string {
	if value, ok := m[key].(string); ok {
		return value
	}
	return fallback
}

func stringPtrValue(value *string) any {
	if value == nil {
		return nil
	}
	return *value
}
