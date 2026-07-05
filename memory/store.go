package memory

import (
	"os"
	"path/filepath"
	"sort"
)

type MemoryStore struct {
	dir string
}

func NewMemoryStore(memoryDir string) *MemoryStore {
	return &MemoryStore{dir: memoryDir}
}

func (s *MemoryStore) Directory() string {
	return s.dir
}

func (s *MemoryStore) ListEntries() []MemoryEntry {
	matches, err := filepath.Glob(filepath.Join(s.dir, "*.md"))
	if err != nil {
		return nil
	}
	sort.Strings(matches)
	entries := make([]MemoryEntry, 0, len(matches))
	for _, path := range matches {
		if filepath.Base(path) == IndexFilename {
			continue
		}
		entry, err := ParseMemoryFile(path)
		if err == nil && entry != nil {
			entries = append(entries, *entry)
		}
	}
	return entries
}

func (s *MemoryStore) GetEntry(name string) *MemoryEntry {
	path := filepath.Join(s.dir, SlugifyName(name)+".md")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	entry, err := ParseMemoryFile(path)
	if err != nil {
		return nil
	}
	return entry
}

func (s *MemoryStore) EntryCount() int {
	count := 0
	matches, _ := filepath.Glob(filepath.Join(s.dir, "*.md"))
	for _, path := range matches {
		if filepath.Base(path) != IndexFilename {
			count++
		}
	}
	return count
}

func (s *MemoryStore) SaveEntry(entry *MemoryEntry) (string, error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return "", err
	}
	entry.Metadata = normalizedMemoryMetadata(entry.Metadata)
	filePath := s.resolveEntryPath(entry)
	content, err := SerializeMemoryFile(*entry)
	if err != nil {
		return "", err
	}
	if err := atomicWriteText(filePath, content); err != nil {
		return "", err
	}
	previous := ""
	if entry.FilePath != "" {
		previous = filepath.Base(entry.FilePath)
	}
	entry.FilePath = filePath
	if previous != "" && previous != filepath.Base(filePath) {
		oldPath := filepath.Join(s.dir, previous)
		_ = os.Remove(oldPath)
		_ = os.Remove(filepath.Join(s.dir, accessDirname, previous+".atime"))
		s.removeIndexEntry(previous)
	}
	if err := s.updateIndexForEntry(*entry); err != nil {
		return "", err
	}
	return filePath, nil
}

func (s *MemoryStore) DeleteEntry(name string) (bool, error) {
	filename := SlugifyName(name) + ".md"
	path := filepath.Join(s.dir, filename)
	if _, err := os.Stat(path); err != nil {
		return false, nil
	}
	if err := s.removeIndexEntry(filename); err != nil {
		return false, err
	}
	if err := os.Remove(path); err != nil {
		_, _ = s.RefreshIndex()
		return false, err
	}
	return true, nil
}

func (s *MemoryStore) RefreshIndex() (string, error) {
	return WriteMemoryIndex(s.dir)
}

func (s *MemoryStore) updateIndexForEntry(entry MemoryEntry) error {
	content := UpdateIndexEntry(s.dir, entry)
	return atomicWriteText(filepath.Join(s.dir, IndexFilename), content)
}

func (s *MemoryStore) removeIndexEntry(filename string) error {
	content := RemoveIndexEntry(s.dir, filename)
	return atomicWriteText(filepath.Join(s.dir, IndexFilename), content)
}

func (s *MemoryStore) resolveEntryPath(entry *MemoryEntry) string {
	if entry.FilePath != "" {
		current := resolveMemoryPathBestEffort(entry.FilePath)
		if current != "" {
			dir := resolveMemoryPathBestEffort(s.dir)
			if dir != "" && filepath.Dir(current) == dir {
				return filepath.Join(s.dir, entry.SlugFilename())
			}
		}
	}
	return filepath.Join(s.dir, entry.SlugFilename())
}

func resolveMemoryPathBestEffort(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	cleaned := filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(cleaned); err == nil {
		return filepath.Clean(resolved)
	}
	current := cleaned
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(current); err == nil {
			parts := append([]string{resolved}, suffix...)
			return filepath.Clean(filepath.Join(parts...))
		}
		parent := filepath.Dir(current)
		if parent == current {
			return cleaned
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		current = parent
	}
}
