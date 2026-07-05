package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type SkillSource string

const (
	SkillSourceUser    SkillSource = "user"
	SkillSourceProject SkillSource = "project"
	SkillSourceBundled SkillSource = "bundled"
)

type SkillFrontmatter struct {
	Name                   string
	Description            string
	WhenToUse              *string
	ArgumentHint           *string
	Arguments              []string
	Context                string
	AllowedTools           []string
	Model                  *string
	Effort                 any
	Agent                  *string
	Shell                  *string
	Paths                  []string
	UserInvocable          bool
	DisableModelInvocation bool
}

type SkillSpec struct {
	Frontmatter   SkillFrontmatter
	Source        SkillSource
	Directory     string
	SkillFile     string
	CanonicalName string
	Content       *string
	LoadedAt      *float64
}

type SkillParseError struct {
	Message string
}

func (e SkillParseError) Error() string { return e.Message }

func ParseSkillMD(filePath string) (SkillFrontmatter, string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return SkillFrontmatter{}, "", SkillParseError{Message: "Unable to read skill file: " + filePath}
	}
	raw := strings.ToValidUTF8(string(data), "\uFFFD")
	if !strings.HasPrefix(raw, "---") {
		return SkillFrontmatter{}, "", SkillParseError{Message: "Missing YAML frontmatter in " + filePath}
	}
	parts := strings.SplitN(raw, "---", 3)
	if len(parts) < 3 {
		return SkillFrontmatter{}, "", SkillParseError{Message: "Malformed YAML frontmatter in " + filePath}
	}
	loaded := map[string]any{}
	if err := yaml.Unmarshal([]byte(parts[1]), &loaded); err != nil {
		return SkillFrontmatter{}, "", SkillParseError{Message: "Invalid YAML frontmatter in " + filePath}
	}
	fm := ValidateFrontmatter(loaded, filePath)
	return fm, strings.TrimLeft(parts[2], "\r\n"), nil
}

func ValidateFrontmatter(raw map[string]any, filePath string) SkillFrontmatter {
	canonical := map[string]any{}
	for key, value := range raw {
		canonical[strings.ReplaceAll(key, "-", "_")] = value
	}
	name, _ := canonical["name"].(string)
	name = strings.TrimSpace(name)
	if name == "" {
		name = filepath.Base(filepath.Dir(filePath))
	}
	description, _ := canonical["description"].(string)
	description = strings.TrimSpace(description)
	if description == "" {
		description = name
	}
	contextValue, _ := canonical["context"].(string)
	if contextValue != "fork" {
		contextValue = "inline"
	}
	return SkillFrontmatter{
		Name:                   name,
		Description:            description,
		WhenToUse:              optionalString(canonical["when_to_use"]),
		ArgumentHint:           optionalString(canonical["argument_hint"]),
		Arguments:              coerceStringList(canonical["arguments"], true),
		Context:                contextValue,
		AllowedTools:           coerceStringList(canonical["allowed_tools"], true),
		Model:                  coerceModel(canonical["model"]),
		Effort:                 coerceEffort(canonical["effort"]),
		Agent:                  optionalString(canonical["agent"]),
		Shell:                  optionalString(canonical["shell"]),
		Paths:                  coerceStringList(canonical["paths"], true),
		UserInvocable:          coerceBool(canonical["user_invocable"], true),
		DisableModelInvocation: coerceBool(canonical["disable_model_invocation"], false),
	}
}

func optionalString(value any) *string {
	if value == nil {
		return nil
	}
	text := strings.TrimSpace(fmt.Sprint(value))
	if text == "" {
		return nil
	}
	return &text
}

func coerceModel(value any) *string {
	result := optionalString(value)
	if result != nil && strings.EqualFold(*result, "inherit") {
		return nil
	}
	return result
}

func coerceStringList(value any, allowScalar bool) []string {
	if value == nil {
		return nil
	}
	if s, ok := value.(string); ok {
		stripped := strings.TrimSpace(s)
		if allowScalar && stripped != "" {
			return []string{stripped}
		}
		return nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, item := range values {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil
		}
		out = append(out, strings.TrimSpace(text))
	}
	return out
}

func coerceBool(value any, fallback bool) bool {
	if value == nil {
		return fallback
	}
	if b, ok := value.(bool); ok {
		return b
	}
	return fallback
}

func coerceEffort(value any) any {
	switch v := value.(type) {
	case nil:
		return nil
	case int:
		return v
	case int64:
		return int(v)
	case bool:
		return v
	case string:
		stripped := strings.TrimSpace(v)
		if stripped == "quick" || stripped == "standard" {
			return stripped
		}
		if regexp.MustCompile(`^\d+$`).MatchString(stripped) {
			if parsed, err := strconv.Atoi(stripped); err == nil {
				return parsed
			}
		}
	}
	return nil
}

func loadedNow() *float64 {
	now := float64(time.Now().UnixNano()) / 1e9
	return &now
}
