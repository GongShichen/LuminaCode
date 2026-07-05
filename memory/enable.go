package memory

import (
	"os"
	"strings"
)

func ParseBoolEnv(name string) *bool {
	value, ok := os.LookupEnv(name)
	if !ok {
		return nil
	}
	low := strings.ToLower(value)
	switch low {
	case "1", "true", "yes":
		parsed := true
		return &parsed
	case "0", "false", "no":
		parsed := false
		return &parsed
	default:
		return nil
	}
}

func IsAutoMemoryEnabled(configAutoMemoryEnabled, bareMode, remoteMode bool) bool {
	envDisable := ParseBoolEnv("CLAUDE_CODE_DISABLE_AUTO_MEMORY")
	if envDisable != nil && *envDisable {
		return false
	}
	if bareMode {
		return false
	}
	if remoteMode {
		return false
	}
	if !configAutoMemoryEnabled {
		return false
	}
	return true
}
