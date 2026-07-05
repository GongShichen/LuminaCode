package tools

import (
	"regexp"
	"strings"
)

func BuildMCPInputSchema(toolName string, rawSchema map[string]any) map[string]any {
	props := mapFromAny(rawSchema["properties"])
	if len(props) == 0 {
		return map[string]any{
			"description": "Empty input for tools that need no parameters.",
			"properties":  map[string]any{},
			"title":       "_EmptyInput",
			"type":        "object",
		}
	}
	required := stringSetFromAny(rawSchema["required"])
	defs := map[string]any{}
	modelName := sanitizeMCPModelName(toolName)
	out := buildMCPObjectSchema(modelName, props, required, defs)
	if rawSchema["additionalProperties"] == false {
		out["additionalProperties"] = false
	}
	if len(defs) > 0 {
		out["$defs"] = defs
	}
	return out
}

func buildMCPObjectSchema(modelName string, props map[string]any, required map[string]struct{}, defs map[string]any) map[string]any {
	properties := map[string]any{}
	requiredList := []any{}
	for propName, rawProp := range props {
		propDef := mapFromAny(rawProp)
		if len(propDef) == 0 {
			continue
		}
		schema := convertMCPProperty(propName, propDef, modelName, defs)
		if _, ok := required[propName]; ok {
			properties[propName] = schema
			requiredList = append(requiredList, propName)
		} else {
			properties[propName] = optionalMCPSchema(schema)
		}
	}
	out := map[string]any{
		"properties": properties,
		"title":      modelName,
		"type":       "object",
	}
	if len(requiredList) > 0 {
		out["required"] = requiredList
	}
	return out
}

func convertMCPProperty(name string, schema map[string]any, prefix string, defs map[string]any) map[string]any {
	field := map[string]any{}
	if description, ok := schema["description"].(string); ok {
		field["description"] = description
	}
	rawType := schema["type"]
	jsonType, nullable := mcpJSONType(rawType, inferMCPType(schema))
	if hasAnyComposition(schema) {
		field["title"] = mcpFieldTitle(name)
		return field
	}
	if enumValues, ok := schema["enum"]; ok {
		field["enum"] = enumValues
		field["title"] = mcpFieldTitle(name)
		if enumType := inferMCPEnumType(enumValues); enumType != "" {
			field["type"] = enumType
		}
		if nullable {
			return nullableMCPSchema(field)
		}
		return field
	}
	switch jsonType {
	case "object":
		if nestedProps := mapFromAny(schema["properties"]); len(nestedProps) > 0 {
			nestedName := prefix + "_" + name
			nestedRequired := stringSetFromAny(schema["required"])
			nested := buildMCPObjectSchema(sanitizeMCPModelName(nestedName), nestedProps, nestedRequired, defs)
			if schema["additionalProperties"] == false {
				nested["additionalProperties"] = false
			}
			defs[nested["title"].(string)] = nested
			ref := map[string]any{"$ref": "#/$defs/" + nested["title"].(string)}
			if nullable {
				return nullableMCPSchema(ref)
			}
			return ref
		}
		field["title"] = mcpFieldTitle(name)
		field["type"] = "object"
	case "array":
		field["title"] = mcpFieldTitle(name)
		field["type"] = "array"
		if itemSchema := mapFromAny(schema["items"]); len(itemSchema) > 0 {
			field["items"] = convertMCPArrayItem(name+"_item", itemSchema, prefix, defs)
		}
	case "integer", "number", "string", "boolean":
		field["title"] = mcpFieldTitle(name)
		field["type"] = jsonType
	default:
		field["title"] = mcpFieldTitle(name)
	}
	copyMCPConstraints(field, schema)
	if nullable {
		return nullableMCPSchema(field)
	}
	return field
}

func convertMCPArrayItem(name string, schema map[string]any, prefix string, defs map[string]any) map[string]any {
	item := convertMCPProperty(name, schema, prefix, defs)
	delete(item, "title")
	return item
}

func optionalMCPSchema(schema map[string]any) map[string]any {
	if _, ok := schema["anyOf"]; ok {
		out := copySchemaMap(schema)
		out["default"] = nil
		return out
	}
	out := nullableMCPSchema(schema)
	out["default"] = nil
	return out
}

func nullableMCPSchema(schema map[string]any) map[string]any {
	inner := copySchemaMap(schema)
	out := map[string]any{
		"anyOf": []any{schema, map[string]any{"type": "null"}},
	}
	if title, ok := inner["title"]; ok {
		out["title"] = title
		delete(inner, "title")
	}
	if description, ok := inner["description"]; ok {
		out["description"] = description
		delete(inner, "description")
	}
	out["anyOf"] = []any{inner, map[string]any{"type": "null"}}
	return out
}

func mcpJSONType(raw any, fallback string) (string, bool) {
	switch v := raw.(type) {
	case string:
		return v, false
	case []any:
		nullable := false
		for _, item := range v {
			text, _ := item.(string)
			if text == "null" {
				nullable = true
				continue
			}
			if text != "" {
				return text, nullable
			}
		}
		return fallback, nullable
	case []string:
		nullable := false
		for _, text := range v {
			if text == "null" {
				nullable = true
				continue
			}
			if text != "" {
				return text, nullable
			}
		}
		return fallback, nullable
	default:
		return fallback, false
	}
}

func inferMCPType(schema map[string]any) string {
	if _, ok := schema["properties"]; ok {
		return "object"
	}
	if _, ok := schema["items"]; ok {
		return "array"
	}
	if _, ok := schema["enum"]; ok {
		return "string"
	}
	return "string"
}

func inferMCPEnumType(raw any) string {
	values, ok := raw.([]any)
	if !ok || len(values) == 0 {
		return ""
	}
	switch values[0].(type) {
	case string:
		return "string"
	case float64, int, int64:
		return "number"
	case bool:
		return "boolean"
	default:
		return ""
	}
}

func copyMCPConstraints(dst, src map[string]any) {
	for _, key := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "minLength", "maxLength", "pattern"} {
		if value, ok := src[key]; ok {
			dst[key] = value
		}
	}
}

func hasAnyComposition(schema map[string]any) bool {
	for _, key := range []string{"oneOf", "anyOf", "allOf"} {
		if _, ok := schema[key]; ok {
			return true
		}
	}
	return false
}

func sanitizeMCPModelName(name string) string {
	safe := regexp.MustCompile(`[^a-zA-Z0-9]`).ReplaceAllString(name, "_")
	if safe == "" {
		return "McpDynamicInput"
	}
	if safe[0] >= '0' && safe[0] <= '9' {
		return "_" + safe
	}
	return safe
}

func mcpFieldTitle(name string) string {
	if name == "" {
		return ""
	}
	parts := strings.Split(name, "_")
	for i, part := range parts {
		if part == "" {
			continue
		}
		runes := []rune(part)
		runes[0] = []rune(strings.ToUpper(string(runes[0])))[0]
		parts[i] = string(runes)
	}
	return strings.Join(parts, " ")
}

func mapFromAny(value any) map[string]any {
	if typed, ok := value.(map[string]any); ok {
		return typed
	}
	return nil
}

func stringSetFromAny(value any) map[string]struct{} {
	out := map[string]struct{}{}
	switch typed := value.(type) {
	case []string:
		for _, item := range typed {
			out[item] = struct{}{}
		}
	case []any:
		for _, item := range typed {
			if text, ok := item.(string); ok {
				out[text] = struct{}{}
			}
		}
	}
	return out
}

func copySchemaMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for key, value := range in {
		out[key] = value
	}
	return out
}
