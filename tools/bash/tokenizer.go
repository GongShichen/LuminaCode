package bash

import (
	"regexp"
	"strings"
	"unicode"

	mapset "github.com/deckarep/golang-set/v2"
)

var safeEnvVars = mapset.NewSet[string](
	"GOEXPERIMENT",
	"GOOS",
	"GOARCH",
	"GOPATH",
	"GOROOT",
	"GOPROXY",
	"GOMODCACHE",
	"GONOSUMCHECK",
	"GONOSUMDB",
	"GOPRIVATE",
	"RUST_BACKTRACE",
	"RUST_LOG",
	"RUSTFLAGS",
	"NODE_ENV",
	"NODE_OPTIONS",
	"PYTHONPATH",
	"PYTHONUNBUFFERED",
	"PYTHONWARNINGS",
	"LANG",
	"LC_ALL",
	"LC_CTYPE",
	"LC_MESSAGES",
	"LC_TIME",
	"HOME",
	"USER",
	"PATH",
	"TERM",
	"SHELL",
	"CI",
	"GITHUB_ACTIONS",
	"GITLAB_CI",
	"DISPLAY",
	"WAYLAND_DISPLAY",
	"EDITOR",
	"VISUAL",
	"PAGER",
)

var envNameRe = regexp.MustCompile(`^[A-Za-z_]\w*$`)

func Tokenize(command string, stripQuotes bool) []string {
	var tokens []string
	var current []rune
	inSingle := false
	inDouble := false
	i := 0
	runes := []rune(command)
	n := len(runes)

	for i < n {
		ch := runes[i]
		if ch == '\\' && i+1 < n {
			nextCh := runes[i+1]
			if nextCh == ' ' || nextCh == '\t' || nextCh == '\\' || nextCh == '\'' || nextCh == '"' {
				current = append(current, nextCh)
				i += 2
				continue
			}
			i += 1
			current = append(current, ch)
			continue
		} else if ch == '\'' && !inDouble {
			inSingle = !inSingle
			if !stripQuotes {
				current = append(current, ch)
			}
		} else if ch == '"' && !inSingle {
			inDouble = !inDouble
			if !stripQuotes {
				current = append(current, ch)
			}
		} else if (ch == ' ' || ch == '\t') && !inSingle && !inDouble {
			if current != nil && len(current) > 0 {
				tokens = append(tokens, string(current))
				current = []rune{}
			}
		} else {
			current = append(current, ch)
		}
		i += 1
	}
	if current != nil && len(current) > 0 {
		tokens = append(tokens, string(current))
	}
	return tokens
}

func SplitPipeline(command string) []string {
	var segments []string
	var current []rune
	inSingle := false
	inDouble := false
	i := 0
	runes := []rune(command)
	n := len(runes)
	for i < n {
		ch := runes[i]
		if ch == '\\' && i+1 < n {
			current = append(current, ch)
			current = append(current, runes[i+1])
			i += 2
			continue
		} else if ch == '\'' && !inDouble {
			inSingle = !inSingle
		} else if ch == '"' && !inSingle {
			inDouble = !inDouble
		} else if !inSingle && !inDouble {
			towChar := ""
			if i+2 <= n {
				towChar = string(runes[i : i+2])
			}
			if towChar == "||" || towChar == "&&" {
				seg := strings.TrimSpace(string(current))
				if seg != "" {
					segments = append(segments, seg)
				}
				current = []rune{}
				i += 2
				continue
			}
			if ch == ';' || ch == '|' {
				seg := strings.TrimSpace(string(current))
				if seg != "" {
					segments = append(segments, seg)
				}
				current = []rune{}
				i += 1
				continue
			}
			if ch == '&' {
				prev := []rune(strings.TrimRightFunc(string(current), unicode.IsSpace))
				prevIsRedirect := len(prev) > 0 && (prev[len(prev)-1] == '>' || prev[len(prev)-1] == '<')
				if prevIsRedirect || (i+1 < n && runes[i+1] == '>') {
					current = append(current, ch)
					i += 1
					continue
				}
				seg := strings.TrimSpace(string(current))
				if seg != "" {
					segments = append(segments, seg)
				}
				current = []rune{}
				i += 1
				continue
			}
		}
		current = append(current, ch)
		i += 1
	}
	remaining := strings.TrimSpace(string(current))
	if remaining != "" {
		segments = append(segments, remaining)
	}
	if segments != nil && len(segments) > 0 {
		return segments
	}
	return []string{command}
}

func splitFirstShellToken(command string) (string, string) {
	cleaned := strings.TrimLeftFunc(command, unicode.IsSpace)
	if cleaned == "" {
		return "", ""
	}
	runes := []rune(cleaned)
	inSingle := false
	inDouble := false
	i := 0
	n := len(runes)
	for i < n {
		ch := runes[i]
		if ch == '\\' && i+1 < n {
			i += 2
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
		} else if ch == '"' && !inSingle {
			inDouble = !inDouble
		} else if !inSingle && !inDouble && (ch == ' ' || ch == '\t') {
			break
		}
		i += 1
	}
	return string(runes[:i]), string(runes[i:])
}

func leadingEnvName(token string) string {
	if !strings.Contains(token, "=") {
		return ""
	}
	vars := strings.SplitN(token, "=", 2)
	name := vars[0]
	if !envNameRe.MatchString(name) {
		return ""
	}
	return name
}

func StripSafeEnvVars(command string) string {
	cleaned := strings.TrimLeftFunc(command, unicode.IsSpace)
	if cleaned == "" {
		return ""
	}

	token, rest := splitFirstShellToken(cleaned)
	if token == "" && rest == "" {
		return ""
	}
	envName := leadingEnvName(token)
	if envName == "" || !safeEnvVars.Contains(envName) {
		return cleaned
	}
	if strings.TrimSpace(rest) == "" {
		return cleaned
	}
	return strings.TrimLeftFunc(rest, unicode.IsSpace)
}

func StripAllSafeEnvPrefixes(command string) string {
	var prev string
	current := command
	for prev != current {
		prev = current
		current = StripSafeEnvVars(current)
	}
	return current
}

func NormalizeBaseToken(token string) string {
	if strings.Contains(token, "=") {
		return token
	}

	if idx := strings.LastIndex(token, "/"); idx >= 0 {
		token = token[idx+1:]
	}

	if idx := strings.LastIndex(token, `\`); idx >= 0 {
		token = token[idx+1:]
	}

	if idx := strings.LastIndex(token, "."); idx >= 0 {
		namePart := token[:idx]
		if namePart != "" {
			token = namePart
		}
	}

	return token
}

func ExtractBaseCommand(command string) string {
	cleaned := StripAllSafeEnvPrefixes(strings.TrimSpace(command))
	if cleaned == "" {
		return ""
	}
	tokens := Tokenize(cleaned, false)
	if len(tokens) == 0 {
		return ""
	}
	base := tokens[0]
	if base == "sudo" || base == "doas" || base == "pkexec" {
		if len(tokens) == 1 {
			return ""
		}
		base = tokens[1]
	}
	return NormalizeBaseToken(base)
}
