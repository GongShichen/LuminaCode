package bash

import (
	"strings"

	appsecurity "LuminaCode/security"
)

type CommandClass string

const (
	CommandClassSafe            CommandClass = "safe"
	CommandClassNeedsPermission CommandClass = "needs_permission"
	CommandClassDangerous       CommandClass = "dangerous"
)

type ClassifierResult struct {
	CommandClass CommandClass
	SafeCommand  string
	Reason       string
}

var privilegePrefixes = map[string]struct{}{
	"sudo": {}, "doas": {}, "pkexec": {},
}

var commandStopTokens = map[string]struct{}{
	">": {}, ">>": {}, "<": {}, "2>": {}, "1>": {}, "&>": {}, "2>&1": {}, "1>&2": {},
	"|": {}, ";": {}, "&": {}, "&&": {}, "||": {},
}

var dangerousEnvPrefixes = map[string]struct{}{
	"IFS": {},
}

var sensitiveReadTargets = map[string]struct{}{
	"/etc/shadow":  {},
	"/etc/sudoers": {},
}

var secretReadTargets = map[string]struct{}{
	"/proc/self/environ": {},
	"/proc/1/environ":    {},
}

var classifierSafeCommands = map[string]struct{}{
	"ls": {}, "dir": {}, "cat": {}, "head": {}, "tail": {}, "less": {}, "more": {},
	"file": {}, "stat": {}, "wc": {}, "du": {}, "df": {}, "tree": {},
	"grep": {}, "rg": {}, "find": {}, "locate": {}, "which": {}, "whereis": {}, "where": {},
	"awk": {}, "sed": {}, "sort": {}, "uniq": {}, "cut": {}, "tr": {}, "column": {},
	"echo": {}, "printf": {}, "date": {}, "uptime": {}, "uname": {}, "hostname": {},
	"whoami": {}, "id": {}, "groups": {}, "env": {}, "printenv": {}, "pwd": {},
	"type": {}, "command": {}, "help": {}, "man": {},
	"nvm": {}, "pyenv": {}, "rbenv": {}, "sdk": {},
}

var classifierSafeSubcommands = map[string]map[string]struct{}{
	"git": {
		"status": {}, "log": {}, "diff": {}, "show": {}, "branch": {}, "tag": {},
		"blame": {}, "stash": {}, "remote": {}, "config": {}, "rev-parse": {},
		"ls-files": {}, "ls-tree": {}, "describe": {}, "shortlog": {},
		"cherry": {}, "grep": {}, "reflog": {}, "whatchanged": {},
	},
	"npm":            {"ls": {}, "list": {}, "view": {}, "info": {}, "outdated": {}, "audit": {}, "root": {}, "bin": {}},
	"yarn":           {"list": {}, "info": {}, "why": {}, "audit": {}, "outdated": {}},
	"pnpm":           {"list": {}, "outdated": {}, "audit": {}, "why": {}},
	"pip":            {"list": {}, "show": {}, "freeze": {}, "check": {}},
	"pip3":           {"list": {}, "show": {}, "freeze": {}, "check": {}},
	"docker":         {"ps": {}, "images": {}, "inspect": {}, "logs": {}, "stats": {}, "version": {}, "info": {}},
	"docker-compose": {"ps": {}, "images": {}, "logs": {}, "config": {}, "port": {}},
	"kubectl":        {"get": {}, "describe": {}, "logs": {}, "explain": {}, "api-versions": {}, "cluster-info": {}},
	"systemctl":      {"status": {}, "list-units": {}, "is-enabled": {}, "is-active": {}},
	"journalctl":     {},
	"gh":             {"pr": {}, "issue": {}, "repo": {}, "status": {}, "auth": {}},
	"poetry":         {"show": {}, "check": {}, "env": {}, "list": {}},
	"cargo":          {"check": {}, "build": {}, "test": {}, "doc": {}, "clippy": {}},
	"go":             {"build": {}, "test": {}, "vet": {}, "fmt": {}, "list": {}, "mod": {}},
}

func ClassifyCommand(command string) ClassifierResult {
	cleaned := strings.TrimSpace(command)
	segments := SplitPipeline(cleaned)
	if len(segments) > 1 {
		for _, segment := range segments {
			result := ClassifyCommand(segment)
			if result.CommandClass != CommandClassSafe {
				return ClassifierResult{CommandClass: CommandClassNeedsPermission, Reason: "pipeline contains non-safe command"}
			}
		}
		return ClassifierResult{CommandClass: CommandClassSafe, Reason: "all pipeline segments are safe"}
	}

	securityResult := RunAllSecurityChecks(cleaned)
	if IsBlocking(securityResult) {
		return ClassifierResult{CommandClass: CommandClassDangerous, Reason: "blocked by shell security checks"}
	}

	base, subcommand, hasSudo := extractClassifierBaseCommand(cleaned)
	if base == "" {
		return ClassifierResult{CommandClass: CommandClassNeedsPermission, Reason: "could not parse command"}
	}
	if hasSudo {
		return ClassifierResult{CommandClass: CommandClassDangerous, Reason: "sudo/doas elevates privileges"}
	}
	if _, ok := dangerousEnvPrefixes[base]; ok {
		return ClassifierResult{CommandClass: CommandClassDangerous, Reason: base + " assignment changes shell parsing"}
	}
	if strings.Contains(base, "=") {
		return ClassifierResult{CommandClass: CommandClassNeedsPermission, Reason: "command has unsafe env prefix"}
	}
	if hasOutputRedirection(cleaned) {
		return ClassifierResult{CommandClass: CommandClassNeedsPermission, Reason: "command writes output"}
	}
	if sensitive := classifySensitiveRead(base, cleaned); sensitive != nil {
		return *sensitive
	}
	if _, ok := classifierSafeCommands[base]; ok {
		return ClassifierResult{CommandClass: CommandClassSafe, SafeCommand: base, Reason: "'" + base + "' is read-only"}
	}
	if safeSubs, ok := classifierSafeSubcommands[base]; ok {
		if _, ok := safeSubs[subcommand]; subcommand != "" && ok {
			return ClassifierResult{CommandClass: CommandClassSafe, SafeCommand: base, Reason: "'" + base + " " + subcommand + "' is read-only"}
		}
	}
	if appsecurity.IsDangerous(cleaned) {
		return ClassifierResult{CommandClass: CommandClassDangerous, Reason: "matches dangerous pattern"}
	}
	return ClassifierResult{CommandClass: CommandClassNeedsPermission, Reason: "may have side effects"}
}

func extractClassifierBaseCommand(command string) (string, string, bool) {
	cleaned := StripAllSafeEnvPrefixes(strings.TrimSpace(command))
	tokens := Tokenize(cleaned, false)
	var filtered []string
	for _, token := range tokens {
		if _, stop := commandStopTokens[token]; stop {
			break
		}
		filtered = append(filtered, token)
	}
	if len(filtered) == 0 {
		return "", "", false
	}
	_, hasSudo := privilegePrefixes[filtered[0]]
	idx := 0
	if hasSudo {
		idx = 1
	}
	if idx >= len(filtered) {
		return "", "", hasSudo
	}
	baseToken := filtered[idx]
	base := baseToken
	if !strings.Contains(baseToken, "=") {
		base = ExtractBaseCommand(cleaned)
		if base == "" {
			base = NormalizeBaseToken(baseToken)
		}
	}
	subcommand := ""
	if idx+1 < len(filtered) {
		subcommand = filtered[idx+1]
	}
	return base, subcommand, hasSudo
}

func classifySensitiveRead(base, command string) *ClassifierResult {
	switch base {
	case "cat", "head", "tail", "less", "more", "grep", "rg", "awk", "sed":
	default:
		return nil
	}
	tokens := Tokenize(command, false)
	for _, token := range tokens[1:] {
		if token == "" || strings.HasPrefix(token, "-") {
			continue
		}
		if _, ok := secretReadTargets[token]; ok {
			return &ClassifierResult{CommandClass: CommandClassDangerous, SafeCommand: base, Reason: "reads process environment secrets"}
		}
		if _, ok := sensitiveReadTargets[token]; ok {
			return &ClassifierResult{CommandClass: CommandClassNeedsPermission, SafeCommand: base, Reason: "reads sensitive system file"}
		}
	}
	return nil
}

func hasOutputRedirection(command string) bool {
	for _, token := range Tokenize(command, false) {
		switch token {
		case ">", ">>", "1>", "2>", "&>":
			return true
		}
	}
	return false
}
