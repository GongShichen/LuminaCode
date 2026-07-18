package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"time"

	"LuminaCode/config"

	"github.com/invopop/jsonschema"
)

const (
	DefaultToolTimeoutSeconds = 30.0
	DefaultMaxOutputChars     = 50_000
)

type ExecutionContext map[string]any

type ToolCall struct {
	ID    string         `json:"id"`
	Name  string         `json:"name"`
	Input map[string]any `json:"input"`
}

type ToolResult struct {
	ToolUseID string `json:"tool_use_id"`
	Content   string `json:"content"`
	IsError   bool   `json:"is_error"`
}

type Tool interface {
	Name() string
	Description() string
	InputPrototype() any
	Aliases() []string
	DeprecatedAliases() map[string]string

	DecodeInput(raw map[string]any) (any, error)
	ValidateInput(ctx ExecutionContext, input any) (bool, string)
	Execute(ctx context.Context, execCtx ExecutionContext, input any) (string, error)

	IsReadOnly(input any) bool
	IsConcurrencySafe(input any) bool
	NeedsPermission(input any) bool
	IsDestructive(input any) bool
	HasCommandClassifier() bool
	ConfirmsFilePaths() bool
	SupportsSiblingAbort() bool
	IsEnabled() bool

	Timeout() time.Duration
	MaxOutputChars() int
	ShouldDefer() bool
	SearchHint() string
	FormatLargeResult(ctx context.Context, content string, maxChars int, toolUseID, sessionDir string) (string, error)
	ToAPISchema() map[string]any
}

type ObservableInputBackfiller interface {
	BackfillObservableInput(input any, ctx ExecutionContext) any
}

type InputTimeoutTool interface {
	TimeoutForInput(input any) time.Duration
}

type ToolSpec struct {
	Name              string
	Description       string
	InputPrototype    any
	Aliases           []string
	DeprecatedAliases map[string]string

	TimeoutSeconds float64
	MaxOutputChars int
	ShouldDefer    bool
	SearchHint     string

	ReadOnly        *bool
	ConcurrencySafe *bool
	Destructive     *bool
	Enabled         *bool

	CommandClassifier bool
	ConfirmFilePaths  bool
	SiblingAbort      bool
}

type BaseTool struct {
	Spec ToolSpec
}

func (t *BaseTool) Name() string { return t.Spec.Name }

func (t *BaseTool) Description() string { return t.Spec.Description }

func (t *BaseTool) InputPrototype() any { return t.Spec.InputPrototype }

func (t *BaseTool) Aliases() []string { return append([]string(nil), t.Spec.Aliases...) }

func (t *BaseTool) DeprecatedAliases() map[string]string {
	return copyStringMap(t.Spec.DeprecatedAliases)
}

func (t *BaseTool) DecodeInput(raw map[string]any) (any, error) {
	proto := t.InputPrototype()
	if proto == nil {
		return raw, nil
	}

	targetType := reflect.TypeOf(proto)
	if targetType.Kind() == reflect.Pointer {
		targetType = targetType.Elem()
	}
	target := reflect.New(targetType).Interface()
	b, err := json.Marshal(raw)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, target); err != nil {
		normalized := normalizeNumericStringInputs(raw, targetType)
		if normalized == nil {
			return nil, err
		}
		b, marshalErr := json.Marshal(normalized)
		if marshalErr != nil {
			return nil, err
		}
		if retryErr := json.Unmarshal(b, target); retryErr != nil {
			return nil, err
		}
	}
	return target, nil
}

func normalizeNumericStringInputs(raw map[string]any, targetType reflect.Type) map[string]any {
	if targetType.Kind() != reflect.Struct {
		return nil
	}
	out := make(map[string]any, len(raw))
	for key, value := range raw {
		out[key] = value
	}
	changed := false
	for i := 0; i < targetType.NumField(); i++ {
		field := targetType.Field(i)
		jsonName := jsonFieldName(field)
		if jsonName == "" {
			continue
		}
		value, exists := out[jsonName]
		if !exists {
			continue
		}
		normalized, ok := normalizeNumericStringValue(value, field.Type)
		if ok {
			out[jsonName] = normalized
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return out
}

func jsonFieldName(field reflect.StructField) string {
	tag := field.Tag.Get("json")
	if tag == "-" {
		return ""
	}
	name := strings.Split(tag, ",")[0]
	if name != "" {
		return name
	}
	return field.Name
}

func normalizeNumericStringValue(value any, targetType reflect.Type) (any, bool) {
	if targetType.Kind() == reflect.Pointer {
		if value == nil {
			return value, false
		}
		targetType = targetType.Elem()
	}
	text, ok := value.(string)
	if !ok {
		return value, false
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return value, false
	}
	switch targetType.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		parsed, err := strconv.ParseInt(text, 10, targetType.Bits())
		if err != nil {
			return value, false
		}
		return parsed, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		parsed, err := strconv.ParseUint(text, 10, targetType.Bits())
		if err != nil {
			return value, false
		}
		return parsed, true
	case reflect.Float32, reflect.Float64:
		parsed, err := strconv.ParseFloat(text, targetType.Bits())
		if err != nil {
			return value, false
		}
		return parsed, true
	default:
		return value, false
	}
}

func (t *BaseTool) ValidateInput(_ ExecutionContext, _ any) (bool, string) { return true, "" }

func (t *BaseTool) Execute(_ context.Context, _ ExecutionContext, _ any) (string, error) {
	return "", fmt.Errorf("tool %q did not implement Execute", t.Name())
}

func (t *BaseTool) IsReadOnly(_ any) bool { return boolOrDefault(t.Spec.ReadOnly, false) }

func (t *BaseTool) IsConcurrencySafe(_ any) bool {
	return boolOrDefault(t.Spec.ConcurrencySafe, false)
}

func (t *BaseTool) NeedsPermission(input any) bool { return !t.IsReadOnly(input) }

func (t *BaseTool) IsDestructive(_ any) bool { return boolOrDefault(t.Spec.Destructive, true) }

func (t *BaseTool) HasCommandClassifier() bool { return t.Spec.CommandClassifier }

func (t *BaseTool) ConfirmsFilePaths() bool { return t.Spec.ConfirmFilePaths }

func (t *BaseTool) SupportsSiblingAbort() bool { return t.Spec.SiblingAbort }

func (t *BaseTool) IsEnabled() bool { return boolOrDefault(t.Spec.Enabled, true) }

func (t *BaseTool) Timeout() time.Duration {
	seconds := t.Spec.TimeoutSeconds
	if seconds <= 0 {
		seconds = DefaultToolTimeoutSeconds
	}
	return time.Duration(seconds * float64(time.Second))
}

func (t *BaseTool) MaxOutputChars() int {
	if t.Spec.MaxOutputChars > 0 {
		return t.Spec.MaxOutputChars
	}
	return DefaultMaxOutputChars
}

func (t *BaseTool) ShouldDefer() bool { return t.Spec.ShouldDefer }

func (t *BaseTool) SearchHint() string { return t.Spec.SearchHint }

func (t *BaseTool) FormatLargeResult(_ context.Context, content string, maxChars int, toolUseID, sessionDir string) (string, error) {
	cfg := config.GetConfig()
	if cfg.MaxToolResultCharsAbsolute > 0 && charLen(content) > cfg.MaxToolResultCharsAbsolute {
		content = ClampToAbsoluteMax(content, cfg.MaxToolResultCharsAbsolute)
	}
	if filepath.Base(filepath.Dir(filepath.Clean(sessionDir))) == "tool-results" {
		return ApplyToolResultBudgetInDir(content, toolUseID, sessionDir, maxChars)
	}
	return ApplyToolResultBudget(content, toolUseID, sessionDir, maxChars)
}

func (t *BaseTool) ToAPISchema() map[string]any {
	schema := map[string]any{"type": "object", "properties": map[string]any{}}
	if proto := t.InputPrototype(); proto != nil {
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					schema = map[string]any{
						"type":        "object",
						"properties":  map[string]any{},
						"description": fmt.Sprintf("Schema generation failed for %s: %v", t.Name(), recovered),
					}
				}
			}()
			reflected := jsonschema.Reflect(proto)
			b, err := json.Marshal(reflected)
			if err == nil {
				_ = json.Unmarshal(b, &schema)
			}
			schema = inlineTopLevelSchemaRef(schema)
			delete(schema, "$schema")
			delete(schema, "$id")
			normalizeToolInputSchema(schema)
		}()
	}
	return map[string]any{
		"name":         t.Name(),
		"description":  t.Description(),
		"input_schema": schema,
	}
}

func normalizeToolInputSchema(value any) {
	switch v := value.(type) {
	case map[string]any:
		if def, ok := v["default"].(string); ok && def == "null" {
			v["default"] = nil
		}
		if oneOf, ok := v["oneOf"]; ok && schemaListHasNull(oneOf) {
			v["anyOf"] = oneOf
			delete(v, "oneOf")
		}
		if anyOf, ok := v["anyOf"].([]any); ok && schemaListHasNull(anyOf) {
			if moveNullDefaultToParent(v, anyOf) {
				v["default"] = nil
			} else if _, exists := v["default"]; !exists {
				v["default"] = nil
			}
			moveFieldFromFirstNonNullSchema(v, anyOf, "description")
			moveFieldFromFirstNonNullSchema(v, anyOf, "title")
		}
		if additional, ok := v["additionalProperties"].(bool); ok && !additional {
			delete(v, "additionalProperties")
		}
		for _, child := range v {
			normalizeToolInputSchema(child)
		}
	case []any:
		for _, child := range v {
			normalizeToolInputSchema(child)
		}
	}
}

func moveFieldFromFirstNonNullSchema(parent map[string]any, items []any, key string) {
	if _, exists := parent[key]; exists {
		return
	}
	for _, item := range items {
		schema, ok := item.(map[string]any)
		if !ok || schema["type"] == "null" {
			continue
		}
		value, exists := schema[key]
		if !exists {
			return
		}
		parent[key] = value
		delete(schema, key)
		return
	}
}

func schemaListHasNull(raw any) bool {
	items, ok := raw.([]any)
	if !ok {
		return false
	}
	for _, item := range items {
		schema, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if schema["type"] == "null" {
			return true
		}
	}
	return false
}

func moveNullDefaultToParent(parent map[string]any, items []any) bool {
	if _, exists := parent["default"]; exists {
		return false
	}
	for _, item := range items {
		schema, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if def, exists := schema["default"]; exists {
			if def == nil {
				delete(schema, "default")
				return true
			}
			if s, ok := def.(string); ok && s == "null" {
				delete(schema, "default")
				return true
			}
		}
	}
	return false
}

func boolPtr(v bool) *bool { return &v }

func BoolPtr(v bool) *bool { return boolPtr(v) }

func boolOrDefault(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func inlineTopLevelSchemaRef(schema map[string]any) map[string]any {
	ref, _ := schema["$ref"].(string)
	if ref == "" {
		return schema
	}
	const prefix = "#/$defs/"
	if len(ref) <= len(prefix) || ref[:len(prefix)] != prefix {
		return schema
	}
	defs, ok := schema["$defs"].(map[string]any)
	if !ok {
		return schema
	}
	def, ok := defs[ref[len(prefix):]].(map[string]any)
	if !ok {
		return schema
	}
	inlined := map[string]any{}
	for k, v := range def {
		inlined[k] = v
	}
	if len(defs) > 1 {
		inlined["$defs"] = defs
	}
	return inlined
}
