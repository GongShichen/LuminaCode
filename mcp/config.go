package mcp

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf16"

	"LuminaCode/config"

	orderedmap "github.com/pb33f/ordered-map/v2"
)

type McpServerConfig struct {
	Name    string            `json:"name"`
	Command *string           `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	CWD     *string           `json:"cwd,omitempty"`
	URL     *string           `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

func (c McpServerConfig) IsStdio() bool { return c.Command != nil }
func (c McpServerConfig) IsHTTP() bool  { return c.URL != nil }

func (c McpServerConfig) Validate() []string {
	var warnings []string
	if c.Command == nil && c.URL == nil {
		warnings = append(warnings, "MCP server '"+c.Name+"': missing 'command' or 'url' — server will be skipped.")
	}
	if c.Command != nil && c.URL != nil {
		warnings = append(warnings, "MCP server '"+c.Name+"': both 'command' and 'url' set — using 'command' (stdio).")
	}
	return warnings
}

func (c McpServerConfig) Fingerprint() string {
	payload := map[string]any{
		"command": c.Command, "args": c.Args, "env": c.Env, "cwd": c.CWD,
		"url": c.URL, "headers": c.Headers,
	}
	raw := pythonCompactJSON(payload)
	sum := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("%x", sum[:])
}

func LoadMCPConfig(projectRoot string) []McpServerConfig {
	merged := map[string]map[string]any{}
	var order []string
	loadMCPFile(filepath.Join(projectRoot, ".mcp.json"), merged, &order)
	loadMCPFile(filepath.Join(projectRoot, ".Lumina", "CONFIG", "mcp.json"), merged, &order)
	return buildConfigs(merged, order)
}

func LoadUserMCPConfig() []McpServerConfig {
	home, _ := os.UserHomeDir()
	merged := map[string]map[string]any{}
	var order []string
	loadMCPFile(filepath.Join(home, ".lumina", "CONFIG", "mcp.json"), merged, &order)
	loadMCPFile(filepath.Join(home, ".Lumina", "CONFIG", "mcp.json"), merged, &order)
	return buildConfigs(merged, order)
}

func LoadProjectMCPConfig(projectRoot string) []McpServerConfig {
	return LoadMCPConfig(projectRoot)
}

func TrustedMCPPath(projectRoot string) string {
	return filepath.Join(config.ProjectRuntimeDir(projectRoot), "CONFIG", "trusted_mcp.json")
}

func LoadTrustedMCP(projectRoot string) map[string]string {
	data, err := os.ReadFile(TrustedMCPPath(projectRoot))
	if err != nil {
		return map[string]string{}
	}
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return map[string]string{}
	}
	rawServers, ok := payload["servers"].(map[string]any)
	if !ok {
		return map[string]string{}
	}
	out := map[string]string{}
	for name, fingerprint := range rawServers {
		if s, ok := fingerprint.(string); ok {
			out[name] = s
		}
	}
	return out
}

func SaveTrustedMCP(projectRoot string, trusted map[string]string) error {
	path := TrustedMCPPath(projectRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	payload := map[string]any{"version": 1, "servers": trusted}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func loadMCPFile(path string, merged map[string]map[string]any, order *[]string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	payload := orderedmap.New[string, json.RawMessage]()
	if err := json.Unmarshal(data, &payload); err != nil {
		return
	}
	rawServers, ok := payload.Get("mcpServers")
	if !ok {
		return
	}
	servers := orderedmap.New[string, any]()
	if err := json.Unmarshal(rawServers, &servers); err != nil {
		return
	}
	if servers.Len() == 0 {
		return
	}
	for name, raw := range servers.FromOldest() {
		cfg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := merged[name]; !exists {
			*order = append(*order, name)
		}
		merged[name] = cfg
	}
}

func buildConfigs(merged map[string]map[string]any, order []string) []McpServerConfig {
	var configs []McpServerConfig
	for _, name := range order {
		raw, ok := merged[name]
		if !ok {
			continue
		}
		cfg := McpServerConfig{
			Name:    name,
			Args:    stringList(raw["args"]),
			Env:     stringMap(raw["env"]),
			Headers: stringMap(raw["headers"]),
		}
		if command, ok := raw["command"].(string); ok {
			cfg.Command = &command
		}
		if cwd, ok := raw["cwd"].(string); ok {
			cfg.CWD = &cwd
		}
		if url, ok := raw["url"].(string); ok {
			cfg.URL = &url
		}
		if cfg.Command != nil || cfg.URL != nil {
			configs = append(configs, cfg)
		}
	}
	return configs
}

func stringList(raw any) []string {
	if raw == nil {
		return []string{}
	}
	values, ok := raw.([]any)
	if !ok {
		return []string{}
	}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if s, ok := value.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func stringMap(raw any) map[string]string {
	values, ok := raw.(map[string]any)
	if !ok {
		return map[string]string{}
	}
	out := map[string]string{}
	for key, value := range values {
		if s, ok := value.(string); ok {
			out[key] = s
		}
	}
	return out
}

func pythonCompactJSON(value any) string {
	switch v := value.(type) {
	case nil:
		return "null"
	case *string:
		if v == nil {
			return "null"
		}
		return pythonJSONString(*v)
	case string:
		return pythonJSONString(v)
	case bool:
		if v {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case []string:
		parts := make([]string, 0, len(v))
		for _, item := range v {
			parts = append(parts, pythonCompactJSON(item))
		}
		return "[" + strings.Join(parts, ",") + "]"
	case map[string]string:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, pythonJSONString(key)+":"+pythonCompactJSON(v[key]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, pythonJSONString(key)+":"+pythonCompactJSON(v[key]))
		}
		return "{" + strings.Join(parts, ",") + "}"
	default:
		return pythonJSONString(fmt.Sprint(v))
	}
}

func pythonJSONString(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else if r < 0x80 {
				b.WriteRune(r)
			} else if r <= 0xffff {
				b.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else {
				encoded := utf16.Encode([]rune{r})
				for _, unit := range encoded {
					b.WriteString(fmt.Sprintf(`\u%04x`, unit))
				}
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}
