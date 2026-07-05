package bash

import (
	"fmt"
	"strings"

	mapset "github.com/deckarep/golang-set/v2"
)

type ExitCodeInterpretation struct {
	IsError     bool
	Description string
}

var (
	exitCodeTables = map[string]map[int]*ExitCodeInterpretation{
		"grep": {
			0: &ExitCodeInterpretation{false, "Match(es) found"},
			1: &ExitCodeInterpretation{false, "No match found (not an error)"},
			2: &ExitCodeInterpretation{true, "Error: invalid pattern or file"},
		},
		"rg": {
			0: &ExitCodeInterpretation{false, "Match(es) found"},
			1: &ExitCodeInterpretation{false, "No match found (not an error)"},
			2: &ExitCodeInterpretation{true, "Error: invalid pattern or file"},
		},
		"diff": {
			0: &ExitCodeInterpretation{false, "Files are identical"},
			1: &ExitCodeInterpretation{false, "Files differ (not an error)"},
			2: &ExitCodeInterpretation{true, "Error: cannot compare files"},
		},
		"test": {
			0: &ExitCodeInterpretation{false, "Condition is true"},
			1: &ExitCodeInterpretation{false, "Condition is false (not an error)"},
			2: &ExitCodeInterpretation{true, "Syntax error in test expression"},
		},
		"[": {
			0: &ExitCodeInterpretation{false, "Condition is true"},
			1: &ExitCodeInterpretation{false, "Condition is false (not an error)"},
			2: &ExitCodeInterpretation{true, "Syntax error in test expression"},
		},
		"find": {
			0: &ExitCodeInterpretation{false, "Success"},
			1: &ExitCodeInterpretation{true, "Partial failure (some dirs inaccessible)"},
		},
		"cmp": {
			0: &ExitCodeInterpretation{false, "Files are identical"},
			1: &ExitCodeInterpretation{false, "Files differ (not an error)"},
			2: &ExitCodeInterpretation{true, "Error: cannot compare files"},
		},
		"expr": {
			0: &ExitCodeInterpretation{false, "Expression is non-zero/non-null"},
			1: &ExitCodeInterpretation{false, "Expression is zero or null"},
			2: &ExitCodeInterpretation{true, "Syntax error in expression"},
			3: &ExitCodeInterpretation{true, "Error: internal error"},
		},
		"pgrep": {
			0: &ExitCodeInterpretation{false, "Process(es) found"},
			1: &ExitCodeInterpretation{false, "No process found (not an error)"},
			2: &ExitCodeInterpretation{true, "Error: invalid pattern or option"},
			3: &ExitCodeInterpretation{true, "Error: internal error"},
		},
		"which": {
			0: &ExitCodeInterpretation{false, "Command found"},
			1: &ExitCodeInterpretation{false, "Command not found (not an error)"},
		},
		"type": {
			0: &ExitCodeInterpretation{false, "Command found"},
			1: &ExitCodeInterpretation{false, "Command not found (not an error)"},
		},
		"command": {
			0: &ExitCodeInterpretation{false, "Command exists"},
			1: &ExitCodeInterpretation{false, "Command not found (not an error)"},
		},
		"git": {
			0:   &ExitCodeInterpretation{false, "Success"},
			1:   &ExitCodeInterpretation{false, "No matches or minor warning"},
			128: &ExitCodeInterpretation{true, "Fatal error"},
		},
		"rsync": {
			0:  &ExitCodeInterpretation{false, "Success"},
			23: &ExitCodeInterpretation{true, "Partial transfer (some files failed)"},
			24: &ExitCodeInterpretation{true, "Partial transfer (source files vanished)"},
		},
	}

	searchCommands = mapset.NewSet[string]("find", "grep", "rg", "ag", "ack", "locate", "which", "whereis")
	readCommands   = mapset.NewSet[string]("cat", "head", "tail", "less", "more",
		"wc", "stat", "file", "jq", "awk", "cut", "sort", "uniq", "tr")
	listCommands            = mapset.NewSet[string]("ls", "tree", "du")
	semanticNeutralCommands = mapset.NewSet[string]("echo", "printf", "true", "false", ":")
)

func ResolveBaseCommand(commandLine string) string {
	return ExtractBaseCommand(commandLine)
}

func InterpretCommandResult(commandLine string, exitCode int) ExitCodeInterpretation {
	if exitCode == 0 {
		return ExitCodeInterpretation{false, "Success (exit 0)"}
	}

	base := ResolveBaseCommand(commandLine)
	if base == "" {
		return ExitCodeInterpretation{true, fmt.Sprintf("Exit code %d", exitCode)}
	}
	table, ok := exitCodeTables[base]
	if ok && table != nil {
		specific := table[exitCode]
		if specific != nil {
			return *specific
		}
	}

	if exitCode < 0 {
		return ExitCodeInterpretation{true, fmt.Sprintf("Killed by signal %d", -exitCode)}
	}
	return ExitCodeInterpretation{true, fmt.Sprintf("Error (exit code %d)", exitCode)}
}

func FormatExitCode(commandLine string, exitCode int) string {
	interpretation := InterpretCommandResult(commandLine, exitCode)
	if interpretation.IsError {
		return fmt.Sprintf("[Exit code: %d — ERROR: %s]", exitCode, interpretation.Description)
	}
	return fmt.Sprintf("[Exit code: %d — %s]", exitCode, interpretation.Description)
}

func ParseBase(command string) (string, string) {
	tokens := strings.Fields(strings.TrimSpace(command))
	if len(tokens) == 0 {
		return "", ""
	}

	base := tokens[0]
	if idx := strings.LastIndex(base, "/"); idx >= 0 {
		base = base[idx+1:]
	}

	sub := ""
	if len(tokens) > 1 && !strings.HasPrefix(tokens[1], "-") {
		sub = tokens[1]
	}

	return base, sub
}
func IsSearchOrReadBashCommand(commandLine string) bool {
	segments := SplitPipeline(commandLine)
	meaningfulSegments := 0
	for _, seg := range segments {
		base, _ := ParseBase(seg)
		if semanticNeutralCommands.Contains(base) {
			continue
		}
		meaningfulSegments++
		if !searchCommands.Contains(base) && !readCommands.Contains(base) && !listCommands.Contains(base) {
			return false
		}
	}

	return meaningfulSegments > 0
}

func ClassifyBashCommand(commandLine string) string {
	segments := SplitPipeline(commandLine)
	categories := mapset.NewSet[string]()
	for _, seg := range segments {
		base, _ := ParseBase(seg)
		if semanticNeutralCommands.Contains(base) {
			continue
		}
		if searchCommands.Contains(base) {
			categories.Add("search")
		} else if readCommands.Contains(base) {
			categories.Add("read")
		} else if listCommands.Contains(base) {
			categories.Add("list")
		} else {
			categories.Add("write")
		}
	}

	if categories.Contains("write") {
		return "write"
	}
	if categories.Contains("search") && categories.Difference(mapset.NewSet[string]("search", "read")).Cardinality() == 0 {
		return "search"
	}
	if categories.Contains("read") {
		return "read"
	}
	if categories.Contains("list") {
		return "list"
	}
	return "unknown"
}
