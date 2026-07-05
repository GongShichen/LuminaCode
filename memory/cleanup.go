package memory

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

const (
	accessDirname      = ".access-times"
	DefaultMaxMemories = 200
)

var DefaultTTLDays = map[MemoryType]int{
	MemoryTypeUser:      180,
	MemoryTypeFeedback:  180,
	MemoryTypeProject:   60,
	MemoryTypeReference: 120,
}

type CleanupStats struct {
	ExpiredCount   int
	EvictedCount   int
	RemainingCount int
	ExpiredNames   []string
	EvictedNames   []string
}

func TouchMemoryAccess(path string) {
	now := time.Now()
	info, err := os.Stat(path)
	if err != nil {
		return
	}
	_ = os.Chtimes(path, now, info.ModTime())
	sidecar := accessSidecarPath(path)
	if err := os.MkdirAll(filepath.Dir(sidecar), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(sidecar, []byte(strconv.FormatFloat(float64(now.UnixNano())/1e9, 'f', -1, 64)), 0o644)
}

func CleanupExpiredMemories(memoryDir string, ttlDays map[MemoryType]int, now time.Time) []string {
	if ttlDays == nil {
		ttlDays = DefaultTTLDays
	}
	if now.IsZero() {
		now = time.Now()
	}
	files := memoryFiles(memoryDir)
	var deleted []string
	for _, path := range files {
		lastAccess := getLastAccessTime(path)
		entry, err := ParseMemoryFile(path)
		if err != nil || entry == nil {
			continue
		}
		days, ok := ttlDays[entry.MemoryType()]
		if !ok {
			days = 180
		}
		maxAge := time.Duration(days) * 24 * time.Hour
		if now.Sub(lastAccess) > maxAge {
			if err := os.Remove(path); err == nil {
				_ = os.Remove(accessSidecarPath(path))
				deleted = append(deleted, filepath.Base(path))
			}
		}
	}
	if len(deleted) > 0 {
		_, _ = WriteMemoryIndex(memoryDir)
	}
	return deleted
}

func EvictLeastAccessed(memoryDir string, maxMemories int) []string {
	files := memoryFiles(memoryDir)
	if len(files) <= maxMemories {
		return nil
	}
	type candidate struct {
		path   string
		access time.Time
	}
	candidates := make([]candidate, 0, len(files))
	for _, path := range files {
		candidates = append(candidates, candidate{path: path, access: getLastAccessTime(path)})
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].access.Before(candidates[j].access) })
	evictCount := len(candidates) - maxMemories
	if evictCount > len(candidates) {
		evictCount = len(candidates)
	}
	toEvict := candidates[:evictCount]
	var evicted []string
	for _, item := range toEvict {
		if err := os.Remove(item.path); err == nil {
			_ = os.Remove(accessSidecarPath(item.path))
			evicted = append(evicted, filepath.Base(item.path))
		}
	}
	if len(evicted) > 0 {
		_, _ = WriteMemoryIndex(memoryDir)
	}
	return evicted
}

func RunCleanup(memoryDir string, ttlDays map[MemoryType]int, maxMemories int, now time.Time) CleanupStats {
	expired := CleanupExpiredMemories(memoryDir, ttlDays, now)
	evicted := EvictLeastAccessed(memoryDir, maxMemories)
	if len(expired) > 0 || len(evicted) > 0 {
		_, _ = WriteMemoryIndex(memoryDir)
	}
	return CleanupStats{
		ExpiredCount: len(expired), EvictedCount: len(evicted),
		RemainingCount: len(memoryFiles(memoryDir)), ExpiredNames: expired, EvictedNames: evicted,
	}
}

func memoryFiles(memoryDir string) []string {
	matches, _ := filepath.Glob(filepath.Join(memoryDir, "*.md"))
	var files []string
	for _, path := range matches {
		if filepath.Base(path) != IndexFilename {
			files = append(files, path)
		}
	}
	return files
}

func getLastAccessTime(path string) time.Time {
	if data, err := os.ReadFile(accessSidecarPath(path)); err == nil {
		if seconds, err := strconv.ParseFloat(string(data), 64); err == nil {
			return time.Unix(0, int64(seconds*1e9))
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Unix(0, 0)
	}
	atime := fileAccessTime(info)
	if atime.After(info.ModTime()) {
		return atime
	}
	return info.ModTime()
}

func accessSidecarPath(path string) string {
	return filepath.Join(filepath.Dir(path), accessDirname, filepath.Base(path)+".atime")
}
