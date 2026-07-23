package localmodel

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"LuminaCode/apppaths"
)

func DefaultModelPath(modelName string) string {
	paths, err := apppaths.ResolveCurrent()
	if err != nil {
		return filepath.Join("cache", "models", "memory", modelName)
	}
	if modelName == BGEModelName || strings.TrimSpace(modelName) == "" {
		return paths.MemoryModelDir
	}
	return filepath.Join(paths.ModelsDir, "memory", modelName)
}

func ExpandPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func itoa(value int) string {
	return strconv.Itoa(value)
}
