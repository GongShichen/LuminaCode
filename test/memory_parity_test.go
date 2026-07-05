package test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"LuminaCode/agent"
	"LuminaCode/config"
	"LuminaCode/memory"
	coretools "LuminaCode/tools"
)

type fakeMemoryClient struct {
	response string
}

func (c fakeMemoryClient) Complete(_ context.Context, _ string, _ []map[string]any, _ int) (string, error) {
	return c.response, nil
}

type capturingMemoryClient struct {
	response string
	messages *[]map[string]any
}

func (c capturingMemoryClient) Complete(_ context.Context, _ string, messages []map[string]any, _ int) (string, error) {
	*c.messages = messages
	return c.response, nil
}

func TestMemoryFileParseIndexAndRecall(t *testing.T) {
	dir := t.TempDir()
	store := memory.NewMemoryStore(dir)
	entry := &memory.MemoryEntry{
		Name:        "Git Safety",
		Description: "Remember force-push policy",
		Content:     "Never force push without explicit approval.",
		Metadata:    map[string]any{"type": "project"},
	}
	path, err := store.SaveEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "git-safety.md" {
		t.Fatalf("unexpected saved path: %s", path)
	}
	index := memory.LoadMemoryIndex(dir)
	if !strings.Contains(index, "- [Git safety](git-safety.md) - Remember force-push policy") {
		t.Fatalf("unexpected index: %q", index)
	}
	parsed := memory.ParseMemoryIndex(index)
	if len(parsed) != 1 || parsed[0].Filename != "git-safety.md" {
		t.Fatalf("unexpected parsed index: %#v", parsed)
	}

	factory := func(context.Context) (memory.CompletionClient, error) {
		return fakeMemoryClient{response: "```json\n[\"git-safety.md\"]\n```"}, nil
	}
	recalls := memory.RecallMemoriesForQuery(context.Background(), "can I force push?", dir, factory, nil, nil)
	if len(recalls) != 1 || recalls[0].Filename != "git-safety.md" {
		t.Fatalf("unexpected recalls: %#v", recalls)
	}
	if !strings.Contains(recalls[0].Content, "Never force push") {
		t.Fatalf("unexpected recalled content: %q", recalls[0].Content)
	}
	sidecar := filepath.Join(dir, ".access-times", "git-safety.md.atime")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("expected recall to create access sidecar: %v", err)
	}
	deleted, err := store.DeleteEntry("Git Safety")
	if err != nil || !deleted {
		t.Fatalf("delete failed: deleted=%v err=%v", deleted, err)
	}
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("delete_entry should leave access sidecar like Python, stat err=%v", err)
	}
}

func TestMemoryRecallAlreadySurfacedPromptIsSortedLikePython(t *testing.T) {
	manifest := strings.Join([]string{
		"- [indexed] a.md: first",
		"- [indexed] z.md: last",
	}, "\n")
	var captured []map[string]any
	factory := func(context.Context) (memory.CompletionClient, error) {
		return capturingMemoryClient{response: "[]", messages: &captured}, nil
	}
	memory.SelectRelevantMemories(context.Background(), "query", manifest, factory, nil, map[string]struct{}{
		"z.md":     {},
		"a.md":     {},
		"note.txt": {},
	})
	if len(captured) != 1 {
		t.Fatalf("expected selector prompt to be sent, got %#v", captured)
	}
	content, _ := captured[0]["content"].(string)
	if !strings.Contains(content, "Already shown (do NOT select these): a.md, z.md") {
		t.Fatalf("expected Python sorted surfaced filenames in prompt, got %q", content)
	}
}

func TestMemoryRecallClipUsesPythonCharacters(t *testing.T) {
	if folded := agent.ClipRecallText("  alpha   beta  ", 400); folded != "alpha beta" {
		t.Fatalf("recall clip should fold whitespace like Python split/join, got %q", folded)
	}
	clipped := agent.ClipRecallText(strings.Repeat("记", 12), 10)
	if clipped != strings.Repeat("记", 7)+"..." {
		t.Fatalf("recall clip should use Python character counts after whitespace folding, got %q", clipped)
	}
	if strings.Contains(clipped, "�") {
		t.Fatalf("recall clip should not split UTF-8 runes, got %q", clipped)
	}
}

func TestMemoryRecallReplacesInvalidUTF8LikePython(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("- [Bad Bytes](bad-bytes.md) - Invalid UTF-8\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	mainContent := []byte("---\nname: Bad Bytes\ndescription: Invalid UTF-8\nmetadata:\n  type: user\n---\n\nbad ")
	mainContent = append(mainContent, 0xff, ' ', 't', 'e', 'x', 't', '\n')
	if err := os.WriteFile(filepath.Join(dir, "bad-bytes.md"), mainContent, 0o644); err != nil {
		t.Fatal(err)
	}
	factory := func(context.Context) (memory.CompletionClient, error) {
		return fakeMemoryClient{response: "[\"bad-bytes.md\"]"}, nil
	}
	recalls := memory.RecallMemoriesForQuery(context.Background(), "invalid bytes", dir, factory, nil, nil)
	if len(recalls) != 1 || !strings.Contains(recalls[0].Content, "bad \uFFFD text") {
		t.Fatalf("expected invalid UTF-8 to be replaced like Python, got %#v", recalls)
	}
}

func TestMemoryParseFileReplacesInvalidUTF8LikePython(t *testing.T) {
	dir := t.TempDir()
	raw := []byte("---\nname: Bad ")
	raw = append(raw, 0xff)
	raw = append(raw, []byte("\ndescription: Desc\nmetadata:\n  type: user\n---\n\nbody ")...)
	raw = append(raw, 0xfe)
	path := filepath.Join(dir, "bad.md")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := memory.ParseMemoryFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Name != "Bad \uFFFD" || entry.Content != "body \uFFFD" {
		t.Fatalf("memory parser should replace invalid UTF-8 like Python, got name=%q content=%q", entry.Name, entry.Content)
	}
}

func TestMemorySerializeFileMatchesPythonYAMLOrderAndIndent(t *testing.T) {
	serialized, err := memory.SerializeMemoryFile(memory.MemoryEntry{
		Name:        "Project Note",
		Description: "Useful detail",
		Content:     "Body\n",
		Metadata:    map[string]any{"type": "project"},
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "---\nname: Project Note\ndescription: Useful detail\nmetadata:\n  type: project\n---\n\nBody\n"
	if serialized != want {
		t.Fatalf("memory serialization should match Python yaml.dump(sort_keys=False), got:\n%s\nwant:\n%s", serialized, want)
	}
}

func TestMemorySerializeDefaultMetadataMatchesPythonDataclass(t *testing.T) {
	serialized, err := memory.SerializeMemoryFile(memory.MemoryEntry{
		Name:        "Default User",
		Description: "Default metadata",
		Content:     "Body",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "---\nname: Default User\ndescription: Default metadata\nmetadata:\n  type: user\n---\n\nBody\n"
	if serialized != want {
		t.Fatalf("memory default metadata should match Python dataclass default, got:\n%s\nwant:\n%s", serialized, want)
	}

	dir := t.TempDir()
	store := memory.NewMemoryStore(dir)
	entry := &memory.MemoryEntry{Name: "Saved Default", Description: "Saved metadata", Content: "Body"}
	path, err := store.SaveEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	if entry.Metadata["type"] != "user" {
		t.Fatalf("SaveEntry should populate Python default metadata, got %#v", entry.Metadata)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "metadata:\n  type: user") {
		t.Fatalf("saved default memory should include user metadata like Python, got:\n%s", content)
	}
}

func TestMemorySlugifyPreservesUnicodeWordCharactersLikePython(t *testing.T) {
	if got := memory.SlugifyName("项目 记忆!"); got != "项目-记忆" {
		t.Fatalf("expected Python unicode word slug, got %q", got)
	}
	dir := t.TempDir()
	store := memory.NewMemoryStore(dir)
	path, err := store.SaveEntry(&memory.MemoryEntry{
		Name:        "项目 记忆!",
		Description: "中文 memory",
		Content:     "body",
		Metadata:    map[string]any{"type": "project"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "项目-记忆.md" {
		t.Fatalf("unexpected unicode memory filename: %s", path)
	}
	index := memory.LoadMemoryIndex(dir)
	if !strings.Contains(index, "- [项目 记忆!](项目-记忆.md) - 中文 memory") {
		t.Fatalf("unexpected unicode index: %q", index)
	}
}

func TestMemoryStorePathLikeNamesTempCleanupAndRenameMatchPython(t *testing.T) {
	dir := t.TempDir()
	store := memory.NewMemoryStore(dir)

	path, err := store.SaveEntry(&memory.MemoryEntry{
		Name:        "../../outside/secret",
		Description: "Desc",
		Content:     "Body",
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, "outsidesecret.md") {
		t.Fatalf("path-like memory name should not escape directory, got %s", path)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "outside")); !os.IsNotExist(err) {
		t.Fatalf("save should not create outside directory, stat err=%v", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(leftovers) != 0 {
		t.Fatalf("successful atomic save should clean temp files like Python, got %#v", leftovers)
	}

	entry := &memory.MemoryEntry{Name: "old-name", Description: "Original", Content: "v1"}
	oldPath, err := store.SaveEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	entry.Name = "new-name"
	newPath, err := store.SaveEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(newPath) != "new-name.md" {
		t.Fatalf("renamed memory should use new slug filename, got %s", newPath)
	}
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("expected renamed memory file: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Fatalf("old memory file should be removed after rename, stat err=%v", err)
	}
	if loaded := store.GetEntry("new-name"); loaded == nil || loaded.FilePath != newPath {
		t.Fatalf("renamed memory should load by new name with updated file path, got %#v", loaded)
	}
}

func TestMemoryIndexLineBudgetUsesPythonCharacters(t *testing.T) {
	dir := t.TempDir()
	store := memory.NewMemoryStore(dir)
	description := strings.Repeat("这是一段很长的中文描述", 12)
	if _, err := store.SaveEntry(&memory.MemoryEntry{
		Name:        "项目记忆",
		Description: description,
		Content:     "body",
		Metadata:    map[string]any{"type": "user"},
	}); err != nil {
		t.Fatal(err)
	}
	index := strings.TrimSpace(memory.LoadMemoryIndex(dir))
	if got := len([]rune(index)); got != 150 {
		t.Fatalf("index line should use Python character budget, got chars=%d bytes=%d line=%q", got, len([]byte(index)), index)
	}
	if !strings.HasPrefix(index, "- [项目记忆](项目记忆.md) - 这是一段很长的中文描述") || !strings.HasSuffix(index, "...") {
		t.Fatalf("unexpected unicode-truncated index line: %q", index)
	}
}

func TestMemoryEntrypointTruncationRstripUsesPythonUnicodeWhitespace(t *testing.T) {
	raw := "- [One](one.md) - desc\n\u00a0\t \n"
	truncated := memory.TruncateEntrypointContent(raw)
	if truncated.Content != "- [One](one.md) - desc" {
		t.Fatalf("MEMORY.md entrypoint should use Python unicode rstrip, got %q", truncated.Content)
	}
	if truncated.LineCount != 1 {
		t.Fatalf("line count should be computed after Python rstrip, got %d", truncated.LineCount)
	}
}

func TestMemoryIndexIncrementalEditsPreserveBlankLinesLikePython(t *testing.T) {
	dir := t.TempDir()
	index := "- [One](one.md) - old\n\n- [Two](two.md) - keep\n"
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	updated := memory.UpdateIndexEntry(dir, memory.MemoryEntry{
		Name:        "One",
		Description: "new",
		Metadata:    map[string]any{"type": "project"},
		FilePath:    filepath.Join(dir, "one.md"),
	})
	if !strings.Contains(updated, "- [One](one.md) - new\n\n- [Two](two.md) - keep") {
		t.Fatalf("incremental update should preserve blank lines like Python, got %q", updated)
	}
	removed := memory.RemoveIndexEntry(dir, "one.md")
	if !strings.HasPrefix(removed, "\n- [Two](two.md) - keep") {
		t.Fatalf("incremental remove should preserve blank lines like Python, got %q", removed)
	}
}

func TestMemoryPathSanitizationExpandsHomeLikePython(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home-dir")
	t.Setenv("HOME", home)
	want := memory.SanitizePathForPath(filepath.Join(home, "project alpha"))
	got := memory.SanitizePathForPath("~/project alpha")
	if got != want {
		t.Fatalf("expected ~/ path to expand before slugging like Python, got %q want %q", got, want)
	}
}

func TestMemoryEvictZeroMaxRemovesAllLikePython(t *testing.T) {
	dir := t.TempDir()
	store := memory.NewMemoryStore(dir)
	for _, name := range []string{"One", "Two"} {
		if _, err := store.SaveEntry(&memory.MemoryEntry{
			Name:        name,
			Description: name + " desc",
			Content:     name + " content",
			Metadata:    map[string]any{"type": "project"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	evicted := memory.EvictLeastAccessed(dir, 0)
	if len(evicted) != 2 {
		t.Fatalf("max_memories=0 should evict all memories like Python, got %#v", evicted)
	}
	for _, filename := range []string{"one.md", "two.md"} {
		if _, err := os.Stat(filepath.Join(dir, filename)); !os.IsNotExist(err) {
			t.Fatalf("expected %s to be evicted, stat err=%v", filename, err)
		}
	}
}

func TestTouchMemoryAccessPreservesMtimeAndWritesSidecarLikePython(t *testing.T) {
	dir := t.TempDir()
	store := memory.NewMemoryStore(dir)
	path, err := store.SaveEntry(&memory.MemoryEntry{Name: "Touch Me", Description: "Touch", Content: "Body"})
	if err != nil {
		t.Fatal(err)
	}
	mtime := time.Unix(1_700_000_000, 0)
	if err := os.Chtimes(path, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	memory.TouchMemoryAccess(path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !info.ModTime().Equal(mtime) {
		t.Fatalf("TouchMemoryAccess should preserve mtime like Python, got %s want %s", info.ModTime(), mtime)
	}
	sidecar := filepath.Join(dir, ".access-times", "touch-me.md.atime")
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("expected sidecar access time file: %v", err)
	}
}

func TestMemoryCleanupUsesAtimeFallbackAndDefaultTTLLikePython(t *testing.T) {
	dir := t.TempDir()
	store := memory.NewMemoryStore(dir)
	path, err := store.SaveEntry(&memory.MemoryEntry{
		Name:        "Recent Access",
		Description: "recent access",
		Content:     "body",
		Metadata:    map[string]any{"type": "project"},
	})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(2_000_000, 0)
	old := now.Add(-10 * 24 * time.Hour)
	if err := os.Chtimes(path, now, old); err != nil {
		t.Fatal(err)
	}
	expired := memory.CleanupExpiredMemories(dir, map[memory.MemoryType]int{memory.MemoryTypeProject: 1}, now)
	if len(expired) != 0 {
		t.Fatalf("atime newer than mtime should prevent expiry like Python, got %#v", expired)
	}

	refPath, err := store.SaveEntry(&memory.MemoryEntry{
		Name:        "Reference TTL",
		Description: "reference ttl",
		Content:     "body",
		Metadata:    map[string]any{"type": "reference"},
	})
	if err != nil {
		t.Fatal(err)
	}
	oneDayOld := now.Add(-24 * time.Hour)
	if err := os.Chtimes(refPath, oneDayOld, oneDayOld); err != nil {
		t.Fatal(err)
	}
	expired = memory.CleanupExpiredMemories(dir, map[memory.MemoryType]int{memory.MemoryTypeProject: 1}, now)
	for _, name := range expired {
		if name == "reference-ttl.md" {
			t.Fatalf("missing TTL map entry should use Python 180-day fallback, got expired=%#v", expired)
		}
	}
}

func TestRunMemoryCleanupPipelineMatchesPython(t *testing.T) {
	dir := t.TempDir()
	store := memory.NewMemoryStore(dir)
	now := time.Unix(2_000_000, 0)

	expiredPath, err := store.SaveEntry(&memory.MemoryEntry{
		Name:        "Expired",
		Description: "Expired",
		Content:     "Old",
		Metadata:    map[string]any{"type": "project"},
	})
	if err != nil {
		t.Fatal(err)
	}
	oldAccess := now.Add(-61 * 24 * time.Hour)
	memory.TouchMemoryAccess(expiredPath)
	writeAccessSidecar(t, dir, "expired.md", oldAccess)

	for _, name := range []string{"Fresh 1", "Fresh 2"} {
		if _, err := store.SaveEntry(&memory.MemoryEntry{
			Name:        name,
			Description: name,
			Content:     "Body",
		}); err != nil {
			t.Fatal(err)
		}
	}

	stats := memory.RunCleanup(dir, nil, 200, now)
	if stats.ExpiredCount != 1 || stats.EvictedCount != 0 || stats.RemainingCount != 2 {
		t.Fatalf("unexpected cleanup stats: %#v", stats)
	}
	if len(stats.ExpiredNames) != 1 || stats.ExpiredNames[0] != "expired.md" {
		t.Fatalf("unexpected expired names: %#v", stats.ExpiredNames)
	}
	if stats.EvictedNames != nil {
		t.Fatalf("no evictions should report nil names like Python, got %#v", stats.EvictedNames)
	}
	if _, err := os.Stat(expiredPath); !os.IsNotExist(err) {
		t.Fatalf("expected expired memory to be deleted, stat err=%v", err)
	}
	index := memory.LoadMemoryIndex(dir)
	if strings.Contains(index, "expired.md") || !strings.Contains(index, "fresh-1.md") || !strings.Contains(index, "fresh-2.md") {
		t.Fatalf("cleanup should refresh MEMORY.md like Python, got %q", index)
	}
}

func TestMemoryEvictNegativeMaxRemovesAllLikePython(t *testing.T) {
	dir := t.TempDir()
	store := memory.NewMemoryStore(dir)
	for _, name := range []string{"One", "Two"} {
		if _, err := store.SaveEntry(&memory.MemoryEntry{
			Name:        name,
			Description: name + " desc",
			Content:     name + " content",
			Metadata:    map[string]any{"type": "project"},
		}); err != nil {
			t.Fatal(err)
		}
	}
	evicted := memory.EvictLeastAccessed(dir, -1)
	if len(evicted) != 2 {
		t.Fatalf("negative max_memories should evict all memories like Python slicing, got %#v", evicted)
	}
}

func writeAccessSidecar(t *testing.T, dir, filename string, access time.Time) {
	t.Helper()
	sidecar := filepath.Join(dir, ".access-times", filename+".atime")
	if err := os.MkdirAll(filepath.Dir(sidecar), 0o755); err != nil {
		t.Fatal(err)
	}
	seconds := float64(access.UnixNano()) / 1e9
	if err := os.WriteFile(sidecar, []byte(strconv.FormatFloat(seconds, 'f', -1, 64)), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestAgentAppendFreshRecalledMemoriesInjectsMetadata(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("- [Build Notes](build-notes.md) - Build troubleshooting\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "build-notes.md"), []byte("---\nname: Build Notes\ndescription: Build troubleshooting\nmetadata:\n  type: user\n---\n\nRun go test ./... after touching tools.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfig()
	cfg.AutoMemoryEnabled = true
	cfg.AutoMemoryDirectory = &dir
	state := agent.NewAgentState()
	state.LastQuery = "fix tests"
	observations := []map[string]any{{
		"tool_name": "grep_search",
		"input":     map[string]any{"pattern": "FAIL"},
		"content":   "some failure output",
		"is_error":  false,
	}}
	factory := func(context.Context) (memory.CompletionClient, error) {
		return fakeMemoryClient{response: "[\"build-notes.md\"]"}, nil
	}
	agent.AppendFreshRecalledMemoriesWithConfig(context.Background(), cfg, &state, observations, factory)
	if len(state.Messages) != 1 {
		t.Fatalf("expected injected memory message, got %d", len(state.Messages))
	}
	metadata, ok := state.Messages[0]["metadata"].(map[string]any)
	if !ok || metadata[memory.MemoryMetaKey] != true || metadata["source"] != memory.MemoryRecallSource {
		t.Fatalf("unexpected metadata: %#v", state.Messages[0]["metadata"])
	}
	if ids := memory.RecalledMemoryIDs(state.Messages); len(ids) != 1 {
		t.Fatalf("expected recalled memory id, got %#v", ids)
	}
}

func TestRecalledMemoryFilenamesMatchesPythonFiltering(t *testing.T) {
	messages := []map[string]any{
		memory.BuildMetaUserMessage("index", memory.MemoryIndexSource),
		{
			"role":   "user",
			"isMeta": true,
			"metadata": map[string]any{
				memory.MemoryMetaKey: true,
				"source":            memory.MemoryRecallSource,
				"filenames":         []any{"alpha.md", "notes.txt", 123, "beta.md"},
			},
		},
		{
			"role":   "user",
			"isMeta": true,
			"metadata": map[string]any{
				memory.MemoryMetaKey: true,
				"source":            "agent_memory_recall",
				"filenames":         []string{"agent.md"},
			},
		},
		{"role": "user", "metadata": map[string]any{"filenames": []string{"plain.md"}}},
	}
	filenames := memory.RecalledMemoryFilenames(messages)
	if len(filenames) != 2 {
		t.Fatalf("expected two recalled filenames, got %#v", filenames)
	}
	for _, name := range []string{"alpha.md", "beta.md"} {
		if _, ok := filenames[name]; !ok {
			t.Fatalf("missing recalled filename %s in %#v", name, filenames)
		}
	}
	if _, ok := filenames["agent.md"]; ok {
		t.Fatalf("agent memory recall source should be ignored like Python: %#v", filenames)
	}
}

func TestFollowupRecallQueryMatchesPythonShape(t *testing.T) {
	messages := []map[string]any{
		{"role": "assistant", "content": []map[string]any{
			{"type": "tool_use", "name": "read_file", "id": "r1"},
			{"type": "tool_use", "name": "read_file", "id": "r2"},
			{"type": "tool_use", "name": "grep_search", "id": "g1"},
		}},
	}
	names := agent.GetRecentToolNames(messages)
	if strings.Join(names, ",") != "read_file,grep_search" {
		t.Fatalf("expected recent tool names to be de-duplicated, got %#v", names)
	}
	windowedMessages := []map[string]any{{
		"role": "assistant",
		"content": []map[string]any{
			{"type": "tool_use", "name": "old_tool", "id": "old"},
		},
	}}
	for i := 0; i < 10; i++ {
		windowedMessages = append(windowedMessages, map[string]any{"role": "user", "content": "filler"})
	}
	if names := agent.GetRecentToolNames(windowedMessages); len(names) != 0 {
		t.Fatalf("expected Python last-10-message window to exclude old assistant tool, got %#v", names)
	}
	inputText := agent.FormatToolInputForRecall(map[string]any{
		"file_path": " src/main.go ",
		"path":      "ignored-after-two",
		"pattern":   "needle",
	})
	if inputText != "file_path=src/main.go, path=ignored-after-two" {
		t.Fatalf("unexpected recall input hints: %q", inputText)
	}
	if clipped := agent.ClipRecallText(" alpha \n beta\tgamma ", 20); clipped != "alpha beta gamma" {
		t.Fatalf("expected whitespace-normalized recall clip, got %q", clipped)
	}

	readObservation := map[string]any{
		"call":     coretools.ToolCall{Name: "read_file", Input: map[string]any{"file_path": "a.go"}},
		"content":  strings.Repeat("x", 410),
		"is_error": false,
	}
	errorObservation := map[string]any{
		"call":     coretools.ToolCall{Name: "run_shell", Input: map[string]any{"command": "go test ./..."}},
		"content":  " boom\nboom ",
		"is_error": true,
	}
	query := agent.BuildFollowupRecallQuery("fix the build", []map[string]any{readObservation, errorObservation})
	if !strings.Contains(query, "Recent tool errors:") || strings.Contains(query, "Recent observations:") {
		t.Fatalf("expected errors to take precedence over observations: %q", query)
	}
	if !strings.Contains(query, "- run_shell (command=go test ./...): boom boom") {
		t.Fatalf("expected command detail and clipped content in query: %q", query)
	}

	query = agent.BuildFollowupRecallQuery("inspect code", []map[string]any{readObservation})
	if !strings.Contains(query, "Recent observations:") || !strings.Contains(query, "read_file (file_path=a.go)") {
		t.Fatalf("expected read observation query, got %q", query)
	}

	if agent.IsReadLikeObservation(map[string]any{
		"call": coretools.ToolCall{Name: "read_file", Input: map[string]any{}},
		"tool": coretools.NewReadFileTool(),
	}) {
		t.Fatalf("read_file without a location hint should not trigger follow-up recall like Python")
	}
	if agent.IsReadLikeObservation(map[string]any{
		"tool_name": "tool_search",
		"input":     map[string]any{"query": "find notebook tools"},
	}) {
		t.Fatalf("main agent tool_search discovery input should not trigger follow-up recall like Python")
	}
}

func TestRunMemoryRecallIncludesMainAgentMemories(t *testing.T) {
	projectRoot := t.TempDir()
	mainMemoryDir := filepath.Join(projectRoot, "memory")
	mainStore := memory.NewMemoryStore(mainMemoryDir)
	if _, err := mainStore.SaveEntry(&memory.MemoryEntry{
		Name:        "Main Note",
		Description: "Main memory detail",
		Content:     "Main memory body.",
		Metadata:    map[string]any{"type": "project"},
	}); err != nil {
		t.Fatal(err)
	}
	agentMemoryDir := filepath.Join(projectRoot, ".Lumina", "agent-memory", "main")
	agentStore := memory.NewMemoryStore(agentMemoryDir)
	if _, err := agentStore.SaveEntry(&memory.MemoryEntry{
		Name:        "Agent Note",
		Description: "Agent memory detail",
		Content:     "Agent memory body.",
		Metadata:    map[string]any{"type": "reference"},
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfig()
	cfg.CWD = projectRoot
	cfg.AutoMemoryEnabled = true
	cfg.AutoMemoryDirectory = &mainMemoryDir
	state := agent.NewAgentState()
	factory := func(context.Context) (memory.CompletionClient, error) {
		return fakeMemoryClient{response: "[\"main-note.md\", \"project--agent-note.md\"]"}, nil
	}

	recalls := agent.RunMemoryRecallWithConfig(context.Background(), cfg, &state, "memory detail", factory, nil)
	var sawMain, sawAgent bool
	for _, recall := range recalls {
		if recall.RecallID == "main-note.md" {
			sawMain = true
		}
		if recall.RecallID == "project--agent-note.md" {
			sawAgent = true
		}
	}
	if !sawMain || !sawAgent {
		t.Fatalf("expected main and main-agent memories, got %#v", recalls)
	}
}

func TestAgentMemoryScopesAndScopedRecall(t *testing.T) {
	projectRoot := t.TempDir()
	scopePath := filepath.Join(projectRoot, ".Lumina", "agent-memory", "explore")
	store := memory.NewMemoryStore(scopePath)
	entry := &memory.MemoryEntry{
		Name:        "Repo Map",
		Description: "Where important files live",
		Content:     "Core files are under agent/ and tools/.",
		Metadata:    map[string]any{"type": "reference"},
	}
	if _, err := store.SaveEntry(entry); err != nil {
		t.Fatal(err)
	}
	scopes := memory.GetAgentMemoryDirectories("Explore", projectRoot)
	if len(scopes) != 3 || scopes[1].Name != "project" || scopes[1].Path != scopePath {
		t.Fatalf("unexpected scopes: %#v", scopes)
	}
	contextMessages := memory.BuildAgentMemoryContextMessages("Explore", projectRoot)
	if len(contextMessages) != 1 {
		t.Fatalf("expected one project context message, got %#v", contextMessages)
	}
	metadata, _ := contextMessages[0]["metadata"].(map[string]any)
	if metadata["source"] != "agent_memory_project" {
		t.Fatalf("unexpected context metadata: %#v", metadata)
	}

	factory := func(context.Context) (memory.CompletionClient, error) {
		return fakeMemoryClient{response: "[\"project--repo-map.md\"]"}, nil
	}
	recalls := memory.RecallAgentMemoriesForQuery(context.Background(), "Explore", projectRoot, "where is code?", factory, nil, nil)
	if len(recalls) != 1 {
		t.Fatalf("expected one agent memory recall, got %#v", recalls)
	}
	if recalls[0].Filename != "repo-map.md" || recalls[0].RecallID != "project--repo-map.md" {
		t.Fatalf("unexpected scoped recall: %#v", recalls[0])
	}
	msg := memory.BuildRecalledAgentMemoriesMessage(recalls, "agent_memory_recall")
	msgMeta, _ := msg["metadata"].(map[string]any)
	if msgMeta["source"] != "agent_memory_recall" {
		t.Fatalf("unexpected recalled metadata: %#v", msgMeta)
	}
}

func TestAgentMemoryDirectoriesPromptAndIndexReuseMatchPython(t *testing.T) {
	home := filepath.Join(t.TempDir(), "home")
	t.Setenv("HOME", home)
	repo := filepath.Join(t.TempDir(), "repo")
	nested := filepath.Join(repo, "nested", "deep")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := memory.SanitizeAgentTypeForPath(" Plan:Agent /// "); got != "plan-agent" {
		t.Fatalf("unexpected agent type slug: %q", got)
	}
	if got := memory.SanitizeAgentTypeForPath("///"); got != "general-purpose" {
		t.Fatalf("empty sanitized agent type should use Python fallback, got %q", got)
	}

	scopes := memory.GetAgentMemoryDirectories("Plan:Agent", nested)
	wantProject := filepath.Join(repo, ".Lumina", "agent-memory", "plan-agent")
	wantLocal := filepath.Join(repo, ".Lumina", "agent-memory-local", "plan-agent")
	if len(scopes) != 3 ||
		scopes[0].Name != "user" || scopes[0].Path != filepath.Join(home, ".Lumina", "agent-memory", "plan-agent") ||
		scopes[1].Name != "project" || scopes[1].Path != wantProject ||
		scopes[2].Name != "local" || scopes[2].Path != wantLocal {
		t.Fatalf("unexpected agent memory scopes: %#v", scopes)
	}

	store := memory.NewMemoryStore(wantProject)
	if _, err := store.SaveEntry(&memory.MemoryEntry{
		Name:        "Planning Style",
		Description: "Prefer risk-first plans.",
		Content:     "Use risk-first implementation plans.",
		Metadata:    map[string]any{"type": "feedback"},
	}); err != nil {
		t.Fatal(err)
	}
	indexPath := filepath.Join(wantProject, "MEMORY.md")
	before, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}

	prompt := memory.BuildAgentMemoryPrompt("Plan:Agent", nested)
	after, err := os.Stat(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("BuildAgentMemoryPrompt should not rewrite an existing index, before=%s after=%s", before.ModTime(), after.ModTime())
	}
	if !strings.Contains(prompt, "Agent-type memory") ||
		!strings.Contains(prompt, "project scope: "+wantProject) ||
		strings.Contains(prompt, "planning-style.md") ||
		strings.Contains(prompt, "risk-first") {
		t.Fatalf("agent memory prompt should include scope metadata only, got %q", prompt)
	}

	messages := memory.BuildAgentMemoryContextMessages("Plan:Agent", nested)
	if len(messages) != 1 {
		t.Fatalf("expected one hidden index context message, got %#v", messages)
	}
	text := messages[0]["content"].([]map[string]any)[0]["text"].(string)
	if !strings.Contains(text, "planning-style.md") || !strings.Contains(text, "risk-first") {
		t.Fatalf("context message should include MEMORY.md contents, got %q", text)
	}
}

func TestAgentMemoryRecallReplacesInvalidUTF8LikePython(t *testing.T) {
	projectRoot := t.TempDir()
	scopePath := filepath.Join(projectRoot, ".Lumina", "agent-memory", "explore")
	if err := os.MkdirAll(scopePath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopePath, "MEMORY.md"), []byte("- [Agent Bytes](agent-bytes.md) - Invalid scoped bytes\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	content := []byte("---\nname: Agent Bytes\ndescription: Invalid scoped bytes\nmetadata:\n  type: reference\n---\n\nscoped ")
	content = append(content, 0xfe, ' ', 'b', 'o', 'd', 'y', '\n')
	if err := os.WriteFile(filepath.Join(scopePath, "agent-bytes.md"), content, 0o644); err != nil {
		t.Fatal(err)
	}
	factory := func(context.Context) (memory.CompletionClient, error) {
		return fakeMemoryClient{response: "[\"project--agent-bytes.md\"]"}, nil
	}
	recalls := memory.RecallAgentMemoriesForQuery(context.Background(), "Explore", projectRoot, "invalid", factory, nil, nil)
	if len(recalls) != 1 || !strings.Contains(recalls[0].Content, "scoped \uFFFD body") {
		t.Fatalf("expected invalid scoped UTF-8 to be replaced like Python, got %#v", recalls)
	}
}
