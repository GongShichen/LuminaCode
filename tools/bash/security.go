package bash

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	CheckIncompleteCommands                   = 1
	CheckJQSystemFunction                     = 2
	CheckJQFileArguments                      = 3
	CheckObfuscatedFlags                      = 4
	CheckShellMetacharacters                  = 5
	CheckDangerousVariables                   = 6
	CheckNewlines                             = 7
	CheckDangerousPatternsCommandSubstitution = 8
	CheckDangerousPatternsInputRedirection    = 9
	CheckDangerousPatternsOutputRedirection   = 10
	CheckIFSInjection                         = 11
	CheckGitCommitSubstitution                = 12
	CheckProcEnvironAccess                    = 13
	CheckMalformedTokenInjection              = 14
	CheckBackslashEscapedWhitespace           = 15
	CheckBraceExpansion                       = 16
	CheckControlCharacters                    = 17
	CheckUnicodeWhitespace                    = 18
	CheckMidWordHash                          = 19
	CheckZshDangerousCommands                 = 20
	CheckBackslashEscapedOperators            = 21
	CheckCommentQuoteDesync                   = 22
	CheckQuotedNewline                        = 23
)

type QuotedExtraction struct {
	WithDoubleQuotes       string
	FullyUnquoted          string
	UnquotedKeepQuoteChars string
}

type SecurityFinding struct {
	ID          int
	Description string
}

type SecurityCheckResult struct {
	Passed   bool
	Findings []SecurityFinding
	CheckIDs map[int]bool
}

func ExtractQuotedContent(command string) QuotedExtraction {
	var withDQ, fully, keep strings.Builder
	for i := 0; i < len(command); {
		ch := command[i]
		switch {
		case ch == '\'':
			i++
			for i < len(command) && command[i] != '\'' {
				i++
			}
			if i < len(command) {
				i++
			}
		case ch == '"':
			keep.WriteByte(ch)
			withDQ.WriteByte(ch)
			i++
			for i < len(command) && command[i] != '"' {
				if command[i] == '\\' && i+1 < len(command) {
					keep.WriteByte(command[i])
					withDQ.WriteByte(command[i])
					i++
					keep.WriteByte(command[i])
					withDQ.WriteByte(command[i])
				} else {
					keep.WriteByte(command[i])
					withDQ.WriteByte(command[i])
				}
				i++
			}
			if i < len(command) {
				keep.WriteByte(command[i])
				withDQ.WriteByte(command[i])
				i++
			}
		case ch == '\\' && i+1 < len(command):
			keep.WriteByte(ch)
			keep.WriteByte(command[i+1])
			withDQ.WriteByte(ch)
			withDQ.WriteByte(command[i+1])
			fully.WriteByte(command[i+1])
			i += 2
		default:
			keep.WriteByte(ch)
			withDQ.WriteByte(ch)
			fully.WriteByte(ch)
			i++
		}
	}
	return QuotedExtraction{
		WithDoubleQuotes:       withDQ.String(),
		FullyUnquoted:          fully.String(),
		UnquotedKeepQuoteChars: keep.String(),
	}
}

type regexFinding struct {
	re          *regexp.Regexp
	description string
}

var commandSubstitutionPatterns = []regexFinding{
	{regexp.MustCompile(`<\(`), "process substitution <()"},
	{regexp.MustCompile(`>\(`), "process substitution >()"},
	{regexp.MustCompile(`=\(`), "Zsh process substitution =()"},
	{regexp.MustCompile(`(?:^|[\s;&|])=[a-zA-Z_]`), "Zsh equals expansion (=cmd)"},
	{regexp.MustCompile(`\$\(`), "$() command substitution"},
	{regexp.MustCompile(`\$\{`), "${} parameter substitution"},
	{regexp.MustCompile(`\$\[`), "$[] legacy arithmetic expansion"},
	{regexp.MustCompile(`~\[`), "Zsh-style parameter expansion"},
	{regexp.MustCompile(`\(e:`), "Zsh-style glob qualifiers"},
	{regexp.MustCompile(`\(\+`), "Zsh glob qualifier with command execution"},
	{regexp.MustCompile(`\}\s*always\s*\{`), "Zsh always block (try/always construct)"},
	{regexp.MustCompile(`<#`), "PowerShell comment syntax (defence in depth)"},
}

var zshDangerousCommands = map[string]bool{
	"zmodload": true, "emulate": true, "sysopen": true, "sysread": true,
	"syswrite": true, "sysseek": true, "zf_rm": true, "zf_mv": true,
	"zf_ln": true, "zf_chmod": true, "zf_chown": true, "zf_mkdir": true,
	"zf_rmdir": true, "zf_symlink": true,
}

var (
	obfuscatedFlagRE      = regexp.MustCompile(`(?:^|[\s;&|])-[a-zA-Z]*[` + "`" + `$]`)
	dangerousVariablesRE  = regexp.MustCompile(`(?i)(?:^|\s)(?:TF|VAR|CMD|EXEC|EVAL|PAYLOAD|SHELL|BASH|EXPLOIT)\s*=`)
	ifsInjectionRE        = regexp.MustCompile(`IFS\s*=`)
	gitCommitSubRE        = regexp.MustCompile(`(?i)git\s+commit.*\$\(`)
	procEnvironRE         = regexp.MustCompile(`/proc/(?:self|\d+)/environ`)
	malformedTokenRE      = regexp.MustCompile(`[A-Za-z0-9+/=]{40,}`)
	backslashWhitespaceRE = regexp.MustCompile(`\\[ \t]`)
	braceExpansionRE      = regexp.MustCompile(`\{[a-zA-Z0-9_.,]+\.\.[a-zA-Z0-9_.,]+\}`)
	controlCharsRE        = regexp.MustCompile(`[\x00-\x08\x0b\x0c\x0e-\x1f\x7f]`)
	unicodeWhitespaceRE   = regexp.MustCompile("[\u00a0\u1680\u180e\u2000\u2001\u2002\u2003\u2004\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f\u205f\u3000\ufeff]")
	midWordHashRE         = regexp.MustCompile(`[a-zA-Z_]#[a-zA-Z_]`)
	backslashOperatorRE   = regexp.MustCompile(`\\(?:&&|\|\||;|\||&|>|<)`)
	jqSystemRE            = regexp.MustCompile(`\bjq\b.*system\s*\(`)
	jqFileArgRE           = regexp.MustCompile(`\bjq\b.*(?:--arg|--argjson|--rawfile)`)
	dangerousInputRE      = regexp.MustCompile(`<\s*/dev/(?:tcp|udp)`)
	dangerousOutputRE     = regexp.MustCompile(`>\s*(?:/dev/[hs]d|/proc/)`)
)

var incompletePatterns = []regexFinding{
	{regexp.MustCompile(`\|\s*$`), "Trailing pipe"},
	{regexp.MustCompile(`&&\s*$`), "Trailing &&"},
	{regexp.MustCompile(`\|\|\s*$`), "Trailing ||"},
	{regexp.MustCompile(`;\s*$`), "Trailing semicolon"},
	{regexp.MustCompile(`\\\s*$`), "Trailing backslash (line continuation)"},
}

var blockingCheckIDs = map[int]bool{
	CheckDangerousPatternsCommandSubstitution: true,
	CheckZshDangerousCommands:                 true,
	CheckProcEnvironAccess:                    true,
	CheckControlCharacters:                    true,
	CheckNewlines:                             true,
	CheckIFSInjection:                         true,
}

var warningCheckIDs = map[int]bool{
	CheckObfuscatedFlags:                    true,
	CheckShellMetacharacters:                true,
	CheckDangerousVariables:                 true,
	CheckMalformedTokenInjection:            true,
	CheckBackslashEscapedWhitespace:         true,
	CheckBraceExpansion:                     true,
	CheckUnicodeWhitespace:                  true,
	CheckMidWordHash:                        true,
	CheckBackslashEscapedOperators:          true,
	CheckCommentQuoteDesync:                 true,
	CheckQuotedNewline:                      true,
	CheckDangerousPatternsInputRedirection:  true,
	CheckDangerousPatternsOutputRedirection: true,
	CheckGitCommitSubstitution:              true,
	CheckIncompleteCommands:                 true,
	CheckJQSystemFunction:                   true,
	CheckJQFileArguments:                    true,
}

func RunAllSecurityChecks(command string) SecurityCheckResult {
	result := SecurityCheckResult{Passed: true, CheckIDs: map[int]bool{}}
	appendFindings := func(findings []SecurityFinding) {
		for _, finding := range findings {
			result.Findings = append(result.Findings, finding)
			result.CheckIDs[finding.ID] = true
		}
	}
	appendFindings(checkIncompleteCommands(command))
	appendFindings(singleREFinding(command, jqSystemRE, CheckJQSystemFunction, "jq system() function call detected", true))
	appendFindings(singleREFinding(command, jqFileArgRE, CheckJQFileArguments, "jq file argument flags detected", true))
	appendFindings(singleREFinding(command, obfuscatedFlagRE, CheckObfuscatedFlags, "Obfuscated flag detected: command substitution in flag", true))
	appendFindings(checkShellMetacharacters(command))
	appendFindings(singleREFinding(command, dangerousVariablesRE, CheckDangerousVariables, "Suspicious variable assignment detected", true))
	appendFindings(checkNewlines(command))
	appendFindings(checkCommandSubstitution(command))
	appendFindings(singleREFinding(command, dangerousInputRE, CheckDangerousPatternsInputRedirection, "Dangerous input redirection to /dev/tcp or /dev/udp", true))
	appendFindings(singleREFinding(command, dangerousOutputRE, CheckDangerousPatternsOutputRedirection, "Dangerous output redirection to device/proc files", true))
	appendFindings(singleREFinding(command, ifsInjectionRE, CheckIFSInjection, "IFS variable manipulation detected", true))
	appendFindings(singleREFinding(command, gitCommitSubRE, CheckGitCommitSubstitution, "Command substitution in git commit", true))
	appendFindings(singleRawFinding(command, procEnvironRE, CheckProcEnvironAccess, "/proc/self/environ access detected"))
	appendFindings(singleREFinding(command, malformedTokenRE, CheckMalformedTokenInjection, "Base64-encoded blob detected", true))
	appendFindings(singleREFinding(command, backslashWhitespaceRE, CheckBackslashEscapedWhitespace, "Backslash-escaped whitespace detected", false))
	appendFindings(singleREFinding(command, braceExpansionRE, CheckBraceExpansion, "Brace expansion detected", true))
	appendFindings(singleRawFinding(command, controlCharsRE, CheckControlCharacters, "Control characters detected"))
	appendFindings(singleRawFinding(command, unicodeWhitespaceRE, CheckUnicodeWhitespace, "Unicode whitespace homoglyph detected"))
	appendFindings(singleREFinding(command, midWordHashRE, CheckMidWordHash, "Mid-word # detected (possible comment injection)", true))
	appendFindings(checkZshDangerous(command))
	appendFindings(singleREFinding(command, backslashOperatorRE, CheckBackslashEscapedOperators, "Backslash-escaped shell operator detected", false))
	appendFindings(checkCommentQuoteDesync(command))
	appendFindings(checkQuotedNewline(command))
	result.Passed = len(result.Findings) == 0
	return result
}

func IsBlocking(result SecurityCheckResult) bool {
	for id := range result.CheckIDs {
		if blockingCheckIDs[id] {
			return true
		}
	}
	return false
}

func IsWarningOnly(result SecurityCheckResult) bool {
	if len(result.Findings) == 0 {
		return false
	}
	for id := range result.CheckIDs {
		if blockingCheckIDs[id] {
			return false
		}
	}
	return true
}

func CheckSemantics(command string) string {
	extraction := ExtractQuotedContent(command)
	target := extraction.FullyUnquoted
	for _, pattern := range commandSubstitutionPatterns[:3] {
		if pattern.re.MatchString(target) {
			return "dangerous"
		}
	}
	indicators := []*regexp.Regexp{
		regexp.MustCompile("`[^`]+`"),
		regexp.MustCompile(`\$\([^)]+\)`),
		regexp.MustCompile(`\|.*\|`),
		regexp.MustCompile(`&&.*&&`),
		regexp.MustCompile(`\|\|.*\|\|`),
	}
	score := 0
	for _, indicator := range indicators {
		if indicator.MatchString(target) {
			score++
		}
	}
	if score >= 3 {
		return "too-complex"
	}
	if score >= 1 {
		return "dangerous"
	}
	return "safe"
}

func checkCommandSubstitution(command string) []SecurityFinding {
	target := ExtractQuotedContent(command).WithDoubleQuotes
	var findings []SecurityFinding
	for _, item := range commandSubstitutionPatterns {
		if match := item.re.FindString(target); match != "" {
			findings = append(findings, SecurityFinding{
				ID:          CheckDangerousPatternsCommandSubstitution,
				Description: item.description + ": matched '" + match + "'",
			})
		}
	}
	return findings
}

func checkZshDangerous(command string) []SecurityFinding {
	target := ExtractQuotedContent(command).FullyUnquoted
	var findings []SecurityFinding
	for _, token := range strings.Fields(target) {
		base := token
		if idx := strings.LastIndex(base, "/"); idx >= 0 {
			base = base[idx+1:]
		}
		if zshDangerousCommands[base] {
			findings = append(findings, SecurityFinding{
				ID:          CheckZshDangerousCommands,
				Description: "Zsh dangerous command: " + base,
			})
		}
	}
	return findings
}

func checkShellMetacharacters(command string) []SecurityFinding {
	target := ExtractQuotedContent(command).FullyUnquoted
	if strings.Count(target, "`") >= 2 {
		return []SecurityFinding{{ID: CheckShellMetacharacters, Description: "Backtick command substitution detected"}}
	}
	return nil
}

func checkNewlines(command string) []SecurityFinding {
	target := ExtractQuotedContent(command).FullyUnquoted
	if strings.ContainsAny(target, "\n\r") {
		return []SecurityFinding{{ID: CheckNewlines, Description: "Unquoted newline/CR in command"}}
	}
	return nil
}

func checkCommentQuoteDesync(command string) []SecurityFinding {
	if !strings.ContainsAny(command, "'\"") {
		return nil
	}
	inSingle, inDouble := false, false
	for i := 0; i < len(command); i++ {
		switch command[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
				for j := i + 1; j < len(command) && command[j] != '\''; j++ {
					if command[j] == '#' {
						return []SecurityFinding{{ID: CheckCommentQuoteDesync, Description: "Comment-quote desync: # inside single quotes"}}
					}
				}
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		}
	}
	return nil
}

func checkQuotedNewline(command string) []SecurityFinding {
	inSingle, inDouble := false, false
	for _, ch := range command {
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
		} else if ch == '"' && !inSingle {
			inDouble = !inDouble
		} else if (ch == '\n' || ch == '\r') && (inSingle || inDouble) {
			return []SecurityFinding{{ID: CheckQuotedNewline, Description: "Newline inside quoted string"}}
		}
	}
	return nil
}

func checkIncompleteCommands(command string) []SecurityFinding {
	trimmed := strings.TrimSpace(command)
	var findings []SecurityFinding
	for _, item := range incompletePatterns {
		if item.re.MatchString(trimmed) {
			findings = append(findings, SecurityFinding{ID: CheckIncompleteCommands, Description: item.description})
		}
	}
	return findings
}

func singleREFinding(command string, re *regexp.Regexp, id int, desc string, fullyUnquoted bool) []SecurityFinding {
	target := command
	if fullyUnquoted {
		target = ExtractQuotedContent(command).FullyUnquoted
	} else {
		target = ExtractQuotedContent(command).WithDoubleQuotes
	}
	if re.MatchString(target) {
		return []SecurityFinding{{ID: id, Description: desc}}
	}
	return nil
}

func singleRawFinding(command string, re *regexp.Regexp, id int, desc string) []SecurityFinding {
	if re.MatchString(command) {
		return []SecurityFinding{{ID: id, Description: desc}}
	}
	return nil
}

func joinFindingDescriptions(findings []SecurityFinding, limit int) string {
	if limit > len(findings) {
		limit = len(findings)
	}
	parts := make([]string, 0, limit)
	for i := 0; i < limit; i++ {
		parts = append(parts, findings[i].Description)
	}
	return strings.Join(parts, "; ")
}

func IsASCII(s string) bool {
	for len(s) > 0 {
		r, size := utf8.DecodeRuneInString(s)
		if r > 127 {
			return false
		}
		s = s[size:]
	}
	return true
}
