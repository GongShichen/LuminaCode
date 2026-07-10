package memory

import (
	"strings"
	"unicode"
)

type MemoryType string

const (
	MemoryTypeUser      MemoryType = "user"
	MemoryTypeFeedback  MemoryType = "feedback"
	MemoryTypeProject   MemoryType = "project"
	MemoryTypeReference MemoryType = "reference"
)

type MemoryRecall struct {
	Filename   string
	FilePath   string
	Content    string
	MemoryType MemoryType
	RecallID   string
	Score      float64
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
