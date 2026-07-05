package bash

import (
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

var pathCommandStrategies = map[string]string{
	"cd":      "cd",
	"rm":      "all_args",
	"rmdir":   "all_args",
	"mv":      "all_args",
	"cp":      "all_args",
	"cat":     "all_args",
	"head":    "all_args",
	"tail":    "all_args",
	"less":    "all_args",
	"more":    "all_args",
	"grep":    "grep",
	"rg":      "grep",
	"find":    "find",
	"touch":   "all_args",
	"mkdir":   "all_args",
	"chmod":   "all_args",
	"chown":   "chown",
	"ln":      "all_args",
	"stat":    "all_args",
	"file":    "all_args",
	"du":      "all_args",
	"df":      "all_args",
	"source":  "first_arg",
	".":       "first_arg",
	"code":    "all_args",
	"vim":     "all_args",
	"nvim":    "all_args",
	"nano":    "all_args",
	"emacs":   "all_args",
	"python":  "first_arg",
	"python3": "first_arg",
	"node":    "first_arg",
}

type DestructiveWarning struct {
	Category string `json:"category"`
	Message  string `json:"message"`
	Pattern  string `json:"pattern"`
}

type destructivePattern struct {
	Pattern  *regexp.Regexp
	Category string
	Message  string
}

var destructivePatterns = []destructivePattern{
	{regexp.MustCompile(`(?i)git\s+reset\s+--hard`), "Git data loss", "may discard uncommitted changes"},
	{regexp.MustCompile(`(?i)git\s+push\s+.*(?:--force|-f\b)`), "Git history overwrite", "may overwrite remote history"},
	{regexp.MustCompile(`(?i)--no-verify`), "Git safety bypass", "may skip safety hooks"},
	{regexp.MustCompile(`(?i)git\s+commit\s+--amend`), "Git commit overwrite", "may rewrite the last commit"},
	{regexp.MustCompile(`(?i)rm\s+.*(?:-r|-rf|--recursive)`), "Recursive force delete", "may recursively force-remove files"},
	{regexp.MustCompile(`(?i)\bDROP\s+TABLE`), "Database drop", "may drop database objects"},
	{regexp.MustCompile(`(?i)\bTRUNCATE\s+`), "Database truncate", "may truncate database objects"},
	{regexp.MustCompile(`(?i)\bDELETE\s+FROM\s+\w+\s*;`), "Database delete (no WHERE)", "may delete all rows"},
	{regexp.MustCompile(`(?i)\bkubectl\s+delete\b`), "Kubernetes delete", "may delete Kubernetes resources"},
	{regexp.MustCompile(`(?i)\bterraform\s+destroy\b`), "Terraform destroy", "may destroy Terraform infrastructure"},
	{regexp.MustCompile(`(?i)\bDROP\s+DATABASE`), "Database drop", "may drop entire database"},
	{regexp.MustCompile(`(?i)\bdocker\s+rm\b`), "Docker remove", "may remove Docker containers"},
	{regexp.MustCompile(`(?i)\bdocker\s+rmi\b`), "Docker remove image", "may remove Docker images"},
	{regexp.MustCompile(`(?i)\bdocker\s+system\s+prune\b`), "Docker prune", "may remove unused Docker data"},
	{regexp.MustCompile(`(?i):\s*\(\)\s*\{`), "Fork bomb", "fork bomb pattern detected"},
}

func CheckDestructiveWarnings(command string) []DestructiveWarning {
	warnings := []DestructiveWarning{}

	for _, item := range destructivePatterns {
		if item.Pattern.MatchString(command) {
			warnings = append(warnings, DestructiveWarning{
				Category: item.Category,
				Message:  item.Message,
				Pattern:  item.Pattern.String(),
			})
		}
	}

	return warnings
}

var absoluteDestroyPattern = regexp.MustCompile(`(?i)\brm\s+.*(?:-r|-rf|--recursive)\s+(?:/\s|/[\*]|~)`)

func IsAbsoluteDestroy(command string) bool {
	return absoluteDestroyPattern.MatchString(command)
}

func ExtractPaths(command string) []string {
	base, tokens := parseCommandStructure(command)
	if base == "" {
		return nil
	}

	strategy := pathCommandStrategies[base]
	switch strategy {
	case "cd":
		return extractCDPaths(tokens)
	case "find":
		return extractFindPaths(tokens)
	case "grep":
		return extractGrepPaths(tokens)
	case "chown":
		return extractChownPaths(tokens)
	case "all_args":
		return extractAllArgPaths(tokens)
	case "first_arg":
		return extractFirstArgPath(tokens)
	default:
		return nil
	}
}

var envAssignmentPattern = regexp.MustCompile(`^[A-Za-z_]\w*=`)

func parseCommandStructure(command string) (string, []string) {
	tokens := Tokenize(command, false)
	if len(tokens) == 0 {
		return "", nil
	}

	idx := 0
	for idx < len(tokens) && envAssignmentPattern.MatchString(tokens[idx]) {
		idx++
	}

	if idx >= len(tokens) {
		return "", nil
	}

	base := tokens[idx]
	if slash := strings.LastIndex(base, "/"); slash >= 0 {
		base = base[slash+1:]
	}

	return base, tokens[idx+1:]
}

func extractCDPaths(tokens []string) []string {
	parts := []string{}

	for _, t := range tokens {
		if t == "--" {
			continue
		}
		if strings.HasPrefix(t, "-") {
			continue
		}
		parts = append(parts, t)
	}

	if len(parts) == 0 {
		return nil
	}

	return []string{strings.Join(parts, " ")}
}

func extractFindPaths(tokens []string) []string {
	paths := []string{}

	for _, t := range tokens {
		if t == "--" {
			continue
		}

		if strings.HasPrefix(t, "-") && t != "-print" && t != "-print0" {
			if _, err := strconv.ParseFloat(t, 64); err == nil {
				paths = append(paths, t)
				continue
			}
			break
		}

		if !strings.HasPrefix(t, "-") {
			paths = append(paths, t)
		}
	}

	return paths
}

func extractGrepPaths(tokens []string) []string {
	paths := []string{}
	patternFound := false
	skipNext := false

	for _, t := range tokens {
		if skipNext {
			skipNext = false
			continue
		}

		if t == "--" {
			continue
		}

		if t == "-e" || t == "-f" || t == "--regexp" || t == "--file" {
			if !patternFound {
				patternFound = true
			}
			skipNext = true
			continue
		}

		if strings.HasPrefix(t, "-") {
			continue
		}

		if !patternFound {
			patternFound = true
			continue
		}

		paths = append(paths, t)
	}

	return paths
}

func extractChownPaths(tokens []string) []string {
	ownerFound := false
	paths := []string{}

	for _, t := range tokens {
		if t == "--" {
			continue
		}
		if strings.HasPrefix(t, "-") {
			continue
		}
		if !ownerFound {
			ownerFound = true
			continue
		}
		paths = append(paths, t)
	}

	return paths
}

func extractAllArgPaths(tokens []string) []string {
	paths := []string{}
	pastDashDash := false

	for _, t := range tokens {
		if t == "--" {
			pastDashDash = true
			continue
		}

		if !pastDashDash && strings.HasPrefix(t, "-") {
			continue
		}

		if isRedirectOperator(t) {
			continue
		}

		paths = append(paths, t)
	}

	return paths
}

func extractFirstArgPath(tokens []string) []string {
	for _, t := range tokens {
		if t == "--" {
			continue
		}
		if strings.HasPrefix(t, "-") {
			continue
		}
		return []string{t}
	}

	return nil
}

func isRedirectOperator(t string) bool {
	switch t {
	case ">", ">>", "<", "2>", "1>", "&>", "|":
		return true
	default:
		return false
	}
}

func IsPathWithinWorkspace(path string, workspaceRoot string) bool {
	resolved, err := resolvePath(path)
	if err != nil {
		return false
	}

	root, err := resolvePath(workspaceRoot)
	if err != nil {
		return false
	}

	rel, err := filepath.Rel(root, resolved)
	if err != nil {
		return false
	}

	return rel == "." || (!strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != "..")
}

func ValidatePaths(command string, workspaceRoot string) (bool, []string) {
	paths := ExtractPaths(command)
	invalid := []string{}

	root, err := resolvePath(workspaceRoot)
	if err != nil {
		return false, paths
	}

	for _, p := range paths {
		if strings.HasPrefix(p, "/dev/") || strings.HasPrefix(p, "/proc/") || strings.HasPrefix(p, "/sys/") {
			continue
		}

		if isSpecialSafePath(p) {
			continue
		}

		if IsPathWithinWorkspace(p, root) {
			continue
		}

		full := p
		if !filepath.IsAbs(p) {
			full = filepath.Join(root, p)
		}
		resolvedFull, err := resolvePath(full)
		if err != nil {
			invalid = append(invalid, p)
			continue
		}

		rel, err := filepath.Rel(root, resolvedFull)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			invalid = append(invalid, p)
		}
	}

	return len(invalid) == 0, invalid
}

func isSpecialSafePath(p string) bool {
	switch p {
	case "-", "/dev/null", "/dev/zero", "/dev/random", "/dev/urandom":
		return true
	default:
		return false
	}
}

func resolvePath(path string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return filepath.Abs(resolved)
	}
	return filepath.Abs(path)
}

func IsWindows() bool {
	return runtime.GOOS == "windows"
}
