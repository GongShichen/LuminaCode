package bash

import (
	"regexp"
	"strings"
)

type SedValidationResult struct {
	Safe                bool
	Reason              string
	NeedsFilePermission bool
	FilePaths           []string
}

var (
	safeSubstituteFlagsRE = regexp.MustCompile(`^[gpiImM1-9]*$`)
	sedWriteCommandsRE    = regexp.MustCompile(`[wW]`)
	sedExecuteFlagsRE     = regexp.MustCompile(`[eE]`)
)

func ValidateSedCommand(command string) SedValidationResult {
	if strings.TrimSpace(command) == "" {
		return SedValidationResult{Safe: false, Reason: "Empty command"}
	}
	expressions := extractSedExpressions(command)
	if len(expressions) == 0 {
		return SedValidationResult{Safe: false, Reason: "Could not parse sed expression"}
	}
	hasIFlag := hasInplaceFlag(command)
	for _, expr := range expressions {
		result := validateSingleSedExpression(expr, hasIFlag)
		if !result.Safe {
			return result
		}
	}
	files := []string{}
	if hasIFlag {
		files = extractSedFileArgs(command)
	}
	return SedValidationResult{
		Safe: true, Reason: "Safe sed operation",
		NeedsFilePermission: hasIFlag, FilePaths: files,
	}
}

func extractSedExpressions(command string) []string {
	var expressions []string
	tokens := Tokenize(command, true)
	for i := 0; i < len(tokens); {
		token := tokens[i]
		switch {
		case token == "sed":
			i++
		case token == "-e" || token == "--expression":
			if i+1 < len(tokens) {
				expressions = append(expressions, tokens[i+1])
				i += 2
			} else {
				i++
			}
		case token == "-f" || token == "--file":
			return nil
		case strings.HasPrefix(token, "-"):
			i++
		default:
			if looksLikeSedScript(token) {
				expressions = append(expressions, token)
			}
			i++
		}
	}
	return expressions
}

func validateSingleSedExpression(expr string, hasInplace bool) SedValidationResult {
	if expr == "" {
		return SedValidationResult{Safe: false, Reason: "Empty expression"}
	}
	if !IsASCII(expr) {
		return SedValidationResult{Safe: false, Reason: "Expression contains non-ASCII characters"}
	}
	if strings.HasPrefix(expr, `s\`) {
		return SedValidationResult{Safe: false, Reason: "Backslash delimiter in substitute command"}
	}
	if strings.Contains(expr, "{") || strings.Contains(expr, "}") {
		return SedValidationResult{Safe: false, Reason: "Script blocks {} are not auto-approved"}
	}
	trimmed := strings.TrimSpace(expr)
	if strings.Contains(expr, "!") && !strings.HasPrefix(expr, "#!") {
		if regexp.MustCompile(`(?:/(?:.*)|\d)! *[a-zA-Z]`).MatchString(expr) ||
			regexp.MustCompile(`^!\s*[a-zA-Z]`).MatchString(trimmed) {
			return SedValidationResult{Safe: false, Reason: "Address negation (!) is not auto-approved"}
		}
	}
	if sedExecuteFlagsRE.MatchString(expr) {
		return SedValidationResult{Safe: false, Reason: "Execute flag (e/E) can run arbitrary shell commands"}
	}
	if sedWriteCommandsRE.MatchString(expr) {
		return SedValidationResult{Safe: false, Reason: "Write command (w/W) can write to arbitrary files"}
	}
	if hasInplace {
		if match := regexp.MustCompile(`(?:^|[^a-zA-Z])([daic])\s`).FindStringSubmatch(expr); len(match) > 1 {
			return SedValidationResult{Safe: false, Reason: "'" + match[1] + "' command with -i modifies files"}
		}
	}
	if regexp.MustCompile(`^[\d,\s;$]*p[\d;]*$`).MatchString(trimmed) {
		return SedValidationResult{Safe: true, Reason: "Safe: line printing only (p command)"}
	}
	if flags, ok := parseSedSubstituteFlags(trimmed); ok {
		if flags == "" || safeSubstituteFlagsRE.MatchString(flags) {
			return SedValidationResult{Safe: true, Reason: "Safe: substitution with allowed flags"}
		}
	}
	if strings.Contains(expr, ";") {
		parts := splitSedExpressions(expr)
		if len(parts) > 1 {
			for _, part := range parts {
				result := validateSingleSedExpression(strings.TrimSpace(part), hasInplace)
				if !result.Safe {
					return result
				}
			}
			return SedValidationResult{Safe: true, Reason: "Safe: all sub-expressions passed"}
		}
	}
	return SedValidationResult{Safe: false, Reason: "Expression does not match any safe pattern"}
}

func looksLikeSedScript(token string) bool {
	return regexp.MustCompile(`^[sdyapicq]`).MatchString(token) ||
		regexp.MustCompile(`^[\d,$]`).MatchString(token) ||
		strings.HasPrefix(token, "/")
}

func splitSedExpressions(expr string) []string {
	var parts []string
	var current strings.Builder
	inSingle := false
	for _, ch := range expr {
		if ch == '\'' {
			inSingle = !inSingle
			current.WriteRune(ch)
		} else if ch == ';' && !inSingle {
			parts = append(parts, current.String())
			current.Reset()
		} else {
			current.WriteRune(ch)
		}
	}
	if current.Len() > 0 {
		parts = append(parts, current.String())
	}
	return parts
}

func hasInplaceFlag(command string) bool {
	for _, token := range Tokenize(command, true) {
		if token == "-i" || strings.HasPrefix(token, "--in-place") || regexp.MustCompile(`^-i\.`).MatchString(token) {
			return true
		}
	}
	return false
}

func extractSedFileArgs(command string) []string {
	tokens := Tokenize(command, true)
	var files []string
	pastScript := false
	for _, token := range tokens {
		switch {
		case token == "sed":
			continue
		case token == "-i" || token == "--in-place" || regexp.MustCompile(`^-i\.`).MatchString(token):
			continue
		case token == "-n" || token == "--quiet" || token == "--silent" || token == "-e" || token == "--expression" || token == "-f" || token == "--file":
			continue
		case strings.HasPrefix(token, "-") && len(token) > 1 && token[1] != '-':
			continue
		case looksLikeSedScript(token):
			pastScript = true
			continue
		case pastScript:
			files = append(files, token)
		}
	}
	return files
}

func parseSedSubstituteFlags(expr string) (string, bool) {
	if len(expr) < 4 || expr[0] != 's' {
		return "", false
	}
	delimiter := expr[1]
	if (delimiter >= 'a' && delimiter <= 'z') || (delimiter >= 'A' && delimiter <= 'Z') ||
		(delimiter >= '0' && delimiter <= '9') || delimiter == ' ' || delimiter == '\t' || delimiter == '\n' || delimiter == '\r' {
		return "", false
	}
	parts := 0
	escaped := false
	for i := 2; i < len(expr); i++ {
		ch := expr[i]
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == delimiter {
			parts++
			if parts == 3 {
				return expr[i+1:], true
			}
		}
	}
	return "", false
}
