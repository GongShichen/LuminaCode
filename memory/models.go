package memory

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

type MemoryType string

const (
	MemoryTypeUser      MemoryType = "user"
	MemoryTypeFeedback  MemoryType = "feedback"
	MemoryTypeProject   MemoryType = "project"
	MemoryTypeReference MemoryType = "reference"
)

type MemoryEntry struct {
	Name        string
	Description string
	Content     string
	Metadata    map[string]any
	FilePath    string
}

func (e MemoryEntry) MemoryType() MemoryType {
	value, _ := e.Metadata["type"].(string)
	switch MemoryType(value) {
	case MemoryTypeFeedback, MemoryTypeProject, MemoryTypeReference:
		return MemoryType(value)
	default:
		return MemoryTypeUser
	}
}

func (e MemoryEntry) Filename() string {
	if e.FilePath != "" {
		return filepath.Base(e.FilePath)
	}
	return SlugifyName(e.Name) + ".md"
}

func (e MemoryEntry) SlugFilename() string {
	return SlugifyName(e.Name) + ".md"
}

func ParseMemoryFile(path string) (*MemoryEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	meta, body := parseFrontmatter(strings.ToValidUTF8(string(data), "\uFFFD"))
	name, _ := meta["name"].(string)
	if name == "" {
		name = strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	}
	description, _ := meta["description"].(string)
	metadata := map[string]any{}
	switch raw := meta["metadata"].(type) {
	case map[string]any:
		metadata = raw
	case map[any]any:
		for k, v := range raw {
			if ks, ok := k.(string); ok {
				metadata[ks] = v
			}
		}
	case string:
		metadata["type"] = raw
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	return &MemoryEntry{
		Name:        name,
		Description: description,
		Content:     body,
		Metadata:    metadata,
		FilePath:    path,
	}, nil
}

func SerializeMemoryFile(entry MemoryEntry) (string, error) {
	meta := memoryFrontmatter{
		Name:        entry.Name,
		Description: entry.Description,
		Metadata:    normalizedMemoryMetadata(entry.Metadata),
	}
	var buf bytes.Buffer
	encoder := yaml.NewEncoder(&buf)
	encoder.SetIndent(2)
	if err := encoder.Encode(meta); err != nil {
		_ = encoder.Close()
		return "", err
	}
	if err := encoder.Close(); err != nil {
		return "", err
	}
	return "---\n" + strings.TrimSpace(buf.String()) + "\n---\n\n" + strings.TrimSpace(entry.Content) + "\n", nil
}

func normalizedMemoryMetadata(metadata map[string]any) map[string]any {
	if metadata == nil {
		return map[string]any{"type": string(MemoryTypeUser)}
	}
	return metadata
}

type memoryFrontmatter struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Metadata    map[string]any `yaml:"metadata"`
}

func SlugifyName(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	var cleaned strings.Builder
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || unicode.IsSpace(r) || r == '-' {
			cleaned.WriteRune(r)
		}
	}
	var slug strings.Builder
	lastHyphen := false
	for _, r := range cleaned.String() {
		isSeparator := r == '_' || unicode.IsSpace(r)
		if isSeparator || r == '-' {
			if !lastHyphen {
				slug.WriteRune('-')
				lastHyphen = true
			}
			continue
		}
		slug.WriteRune(r)
		lastHyphen = false
	}
	text = strings.Trim(slug.String(), "-")
	if text == "" {
		return "untitled"
	}
	return text
}

func parseFrontmatter(text string) (map[string]any, string) {
	if !strings.HasPrefix(text, "---") {
		return map[string]any{}, text
	}
	parts := strings.SplitN(text, "---", 3)
	if len(parts) < 3 {
		return map[string]any{}, text
	}
	meta := map[string]any{}
	if err := yaml.Unmarshal([]byte(parts[1]), &meta); err != nil {
		return map[string]any{}, text
	}
	body := strings.TrimSpace(parts[2])
	return meta, body
}
