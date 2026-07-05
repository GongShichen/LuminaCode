package skills

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"LuminaCode/config"

	shellquote "github.com/kballard/go-shellquote"
)

type SkillShellPermissionRequest struct {
	SkillName string
	Command   string
}

type PromptProcessor struct {
	Config config.Config
}

func NewPromptProcessor(cfg config.Config) *PromptProcessor {
	return &PromptProcessor{Config: cfg}
}

func (p *PromptProcessor) Process(skill SkillSpec, args, sessionID string, approveProjectShell func(SkillShellPermissionRequest) bool) (string, error) {
	if skill.Content == nil {
		return "", fmt.Errorf("Skill content must be loaded before processing")
	}
	content := *skill.Content
	if skill.Directory != "" {
		content = "Base directory for this skill: " + skill.Directory + "\n\n" + content
	}
	content = p.substituteArguments(content, args, skill.Frontmatter.Arguments, skill.Frontmatter.ArgumentHint)
	content = p.substituteEnvVars(content, skill.Directory, sessionID, skill.Source)
	return p.executeInlineShell(content, skill, approveProjectShell)
}

func (p *PromptProcessor) substituteArguments(content, args string, namedArgs []string, argumentHint *string) string {
	rawArgs := strings.TrimSpace(args)
	var argv []string
	var err error
	if rawArgs != "" {
		argv, err = shellquote.Split(rawArgs)
		if err != nil {
			argv = []string{rawArgs}
		}
	}
	replacementsApplied := false
	indexRe := regexp.MustCompile(`\$ARGUMENTS\[(\d+)\]`)
	content = indexRe.ReplaceAllStringFunc(content, func(match string) string {
		groups := indexRe.FindStringSubmatch(match)
		idx, _ := strconv.Atoi(groups[1])
		replacementsApplied = true
		if idx < len(argv) {
			return argv[idx]
		}
		return ""
	})
	allArgsRe := regexp.MustCompile(`\$ARGUMENTS([^\[]|$)`)
	allCount := 0
	content = allArgsRe.ReplaceAllStringFunc(content, func(match string) string {
		allCount++
		return rawArgs + strings.TrimPrefix(match, "$ARGUMENTS")
	})
	replacementsApplied = replacementsApplied || allCount > 0
	posRe := regexp.MustCompile(`\$(\d+)`)
	seen := map[int]struct{}{}
	for _, match := range posRe.FindAllStringSubmatch(content, -1) {
		idx, _ := strconv.Atoi(match[1])
		seen[idx] = struct{}{}
	}
	var indices []int
	for idx := range seen {
		indices = append(indices, idx)
	}
	for i := 0; i < len(indices); i++ {
		for j := i + 1; j < len(indices); j++ {
			if indices[j] > indices[i] {
				indices[i], indices[j] = indices[j], indices[i]
			}
		}
	}
	for _, idx := range indices {
		value := ""
		if idx < len(argv) {
			value = argv[idx]
		}
		content = strings.ReplaceAll(content, fmt.Sprintf("$%d", idx), value)
		replacementsApplied = true
	}
	if len(namedArgs) > 0 {
		valueMap := map[string]string{}
		for idx, name := range namedArgs {
			if idx < len(argv) {
				valueMap[name] = argv[idx]
			} else {
				valueMap[name] = ""
			}
		}
		names := append([]string(nil), namedArgs...)
		for i := 0; i < len(names); i++ {
			for j := i + 1; j < len(names); j++ {
				if len(names[j]) > len(names[i]) {
					names[i], names[j] = names[j], names[i]
				}
			}
		}
		for _, name := range names {
			value := valueMap[name]
			braced := "${" + name + "}"
			plain := "$" + name
			if strings.Contains(content, braced) {
				replacementsApplied = true
				content = strings.ReplaceAll(content, braced, value)
			}
			plainRe := regexp.MustCompile(regexp.QuoteMeta(plain) + `([^A-Za-z0-9_]|$)`)
			plainCount := 0
			content = plainRe.ReplaceAllStringFunc(content, func(match string) string {
				plainCount++
				suffix := ""
				if len(match) > len(plain) {
					suffix = match[len(plain):]
				}
				return value + suffix
			})
			replacementsApplied = replacementsApplied || plainCount > 0
		}
	}
	if !replacementsApplied && rawArgs != "" {
		content = strings.TrimRight(content, "\r\n\t ") + "\n\nARGUMENTS: " + rawArgs + "\n"
	} else if !replacementsApplied && rawArgs == "" && argumentHint != nil {
		content = strings.TrimRight(content, "\r\n\t ") + "\n\nHint: " + *argumentHint + "\n"
	}
	return content
}

func (p *PromptProcessor) substituteEnvVars(content, skillDir, sessionID string, source SkillSource) string {
	if source != "mcp" && skillDir != "" {
		skillDir = filepath.ToSlash(skillDir)
		content = strings.ReplaceAll(content, "${LUMINA_SKILL_DIR}", skillDir)
		content = strings.ReplaceAll(content, "${CLAUDE_SKILL_DIR}", skillDir)
	}
	content = strings.ReplaceAll(content, "${LUMINA_SESSION_ID}", sessionID)
	content = strings.ReplaceAll(content, "${CLAUDE_SESSION_ID}", sessionID)
	return content
}

func (p *PromptProcessor) executeInlineShell(content string, skill SkillSpec, approveProjectShell func(SkillShellPermissionRequest) bool) (string, error) {
	re := regexp.MustCompile("(?s)!`(?P<command>.+?)`")
	matches := re.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		return content, nil
	}
	var commands []string
	for _, match := range matches {
		command := strings.TrimSpace(content[match[2]:match[3]])
		decision := DecideInlineShellExecution(skill, command)
		if !decision.Allowed {
			return "", fmt.Errorf("%s", decision.Reason)
		}
		if decision.RequiresApproval {
			if approveProjectShell == nil || !approveProjectShell(SkillShellPermissionRequest{SkillName: skill.CanonicalName, Command: command}) {
				return "", fmt.Errorf("Skill '%s' shell command was denied: %s", skill.CanonicalName, command)
			}
		}
		commands = append(commands, command)
	}
	var replacements []string
	for _, command := range commands {
		runCWD := skill.Directory
		if runCWD == "" {
			runCWD = p.Config.CWD
		}
		if err := RunShellSafetyChecks(command, runCWD); err != nil {
			return "", err
		}
		output, err := p.runInlineShell(command, skill.Directory, optionalValue(skill.Frontmatter.Shell))
		if err != nil {
			return "", err
		}
		replacements = append(replacements, output)
	}
	var rendered strings.Builder
	cursor := 0
	for i, match := range matches {
		rendered.WriteString(content[cursor:match[0]])
		rendered.WriteString(replacements[i])
		cursor = match[1]
	}
	rendered.WriteString(content[cursor:])
	return rendered.String(), nil
}

func (p *PromptProcessor) runInlineShell(command, cwd, executable string) (string, error) {
	runCWD := cwd
	if runCWD == "" {
		runCWD = p.Config.CWD
	}
	timeout := time.Duration(p.Config.ShellTimeoutSeconds * float64(time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	argv := shellArgv(command, executable)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = runCWD
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("Inline shell command timed out after %ss: %s", pythonFloatString(p.Config.ShellTimeoutSeconds), command)
	}
	if err != nil {
		if cmd.ProcessState == nil {
			return "", err
		}
		return "", fmt.Errorf("Inline shell command failed (%d): %s\n%s", cmd.ProcessState.ExitCode(), command, strings.TrimSpace(strings.ToValidUTF8(stderr.String(), "\uFFFD")))
	}
	out := strings.TrimSpace(strings.ToValidUTF8(stdout.String(), "\uFFFD"))
	if len([]byte(out)) > p.Config.ShellMaxOutputBytes {
		return "", fmt.Errorf("Inline shell command output exceeded %d bytes: %s", p.Config.ShellMaxOutputBytes, command)
	}
	return out, nil
}

func shellArgv(command, executable string) []string {
	if executable != "" {
		name := strings.ToLower(filepath.Base(executable))
		if name == "cmd" || name == "cmd.exe" {
			return []string{executable, "/C", command}
		}
		if strings.HasPrefix(name, "powershell") || name == "pwsh" {
			return []string{executable, "-NoProfile", "-Command", command}
		}
		return []string{executable, "-c", command}
	}
	if runtime.GOOS == "windows" {
		comspec := os.Getenv("COMSPEC")
		if comspec == "" {
			comspec = "cmd.exe"
		}
		return []string{comspec, "/C", command}
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/sh"
	}
	return []string{shell, "-c", command}
}

func pythonFloatString(value float64) string {
	text := strconv.FormatFloat(value, 'f', -1, 64)
	if !strings.Contains(text, ".") && !strings.ContainsAny(text, "eE") {
		text += ".0"
	}
	return text
}

func optionalValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
