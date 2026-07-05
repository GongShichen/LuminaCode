package test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"LuminaCode/config"
	"LuminaCode/skills"
	coretools "LuminaCode/tools"
)

func TestSkillParsePromptProcessorAndInlineMessage(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "my-skill")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillFile := filepath.Join(skillDir, "SKILL.md")
	raw := `---
name: My Skill
description: Helps with $ARGUMENTS
argument-hint: target
arguments: [thing]
allowed-tools: read_file
model: inherit
effort: quick
paths: ["src/**"]
---
Use $thing from $ARGUMENTS[0].
Session ${LUMINA_SESSION_ID}
Dir ${LUMINA_SKILL_DIR}
`
	if err := os.WriteFile(skillFile, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	fm, content, err := skills.ParseSkillMD(skillFile)
	if err != nil {
		t.Fatal(err)
	}
	if fm.Name != "My Skill" || fm.Context != "inline" || len(fm.AllowedTools) != 1 || fm.Model != nil {
		t.Fatalf("unexpected frontmatter: %#v", fm)
	}
	spec := skills.SkillSpec{Frontmatter: fm, Source: skills.SkillSourceUser, Directory: skillDir, SkillFile: skillFile, CanonicalName: "my-skill", Content: &content}
	cfg := config.NewConfig()
	cfg.CWD = dir
	processor := skills.NewPromptProcessor(cfg)
	rendered, err := processor.Process(spec, "alpha beta", "session-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, "Use alpha from alpha.") ||
		!strings.Contains(rendered, "Session session-1") ||
		!strings.Contains(rendered, "Dir "+filepath.ToSlash(skillDir)) {
		t.Fatalf("unexpected rendered prompt: %q", rendered)
	}
	executor := skills.NewSkillExecutor(skills.NewSkillLoader(cfg), processor)
	msg := executor.BuildInlineSkillMessage(spec, rendered, false)
	metadata, _ := msg["metadata"].(map[string]any)
	if metadata["source"] != skills.SkillInlineSource || metadata[skills.SkillInlineAllowedToolsKey] == nil {
		t.Fatalf("unexpected inline metadata: %#v", metadata)
	}
}

func TestSkillTransientSourcesDefaultStripMatchesPython(t *testing.T) {
	for _, source := range []string{skills.SkillInlineSource, skills.SkillListingSource, skills.SkillRecoverySource} {
		if _, ok := skills.SkillTransientSources[source]; !ok {
			t.Fatalf("missing transient skill source %s", source)
		}
	}
	messages := []map[string]any{
		{"role": "user", "metadata": map[string]any{"source": skills.SkillInlineSource}},
		{"role": "user", "metadata": map[string]any{"source": skills.SkillListingSource}},
		{"role": "user", "metadata": map[string]any{"source": skills.SkillRecoverySource}},
		{"role": "user", "content": "keep"},
	}
	kept := skills.StripSkillContextMessages(messages, nil)
	if len(kept) != 1 || kept[0]["content"] != "keep" {
		t.Fatalf("default strip should remove all transient skill sources, got %#v", kept)
	}
}

func TestSkillParseReplacesInvalidUTF8LikePython(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "bad-bytes")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw := []byte("---\nname: Bad Bytes\ndescription: Desc ")
	raw = append(raw, 0xff)
	raw = append(raw, []byte("\n---\nBody ")...)
	raw = append(raw, 0xfe)
	raw = append(raw, '\n')
	skillFile := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillFile, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	fm, content, err := skills.ParseSkillMD(skillFile)
	if err != nil {
		t.Fatal(err)
	}
	if fm.Description != "Desc \uFFFD" || content != "Body \uFFFD\n" {
		t.Fatalf("expected invalid UTF-8 replacement like Python, description=%q content=%q", fm.Description, content)
	}
}

func TestSkillNamedArgumentReplacementTreatsDollarLiterallyLikePython(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	cfg.CWD = dir
	processor := skills.NewPromptProcessor(cfg)
	content := "Path: $file end"
	spec := skills.SkillSpec{
		Frontmatter:   skills.SkillFrontmatter{Name: "Dollar Args", Description: "d", Arguments: []string{"file"}},
		Source:        skills.SkillSourceProject,
		Directory:     dir,
		CanonicalName: "dollar-args",
		Content:       &content,
	}
	rendered, err := processor.Process(spec, "$HOME", "session-1", func(skills.SkillShellPermissionRequest) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, "Path: $HOME end") {
		t.Fatalf("named argument value should be literal like Python, got %q", rendered)
	}
}

func TestSkillRegistryVisibilityAndSkillToolInjection(t *testing.T) {
	dir := t.TempDir()
	visibleDir := filepath.Join(dir, "src", "pkg")
	if err := os.MkdirAll(visibleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillDir := filepath.Join(dir, ".Lumina", "PROJECT_SKILLS", "reader")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillFile := filepath.Join(skillDir, "SKILL.md")
	raw := `---
name: Reader
description: Read things
paths: ["src/**"]
---
Read carefully: $ARGUMENTS
`
	if err := os.WriteFile(skillFile, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.SkillsDir = ".Lumina/PROJECT_SKILLS"
	loader := skills.NewSkillLoader(cfg)
	registry := skills.NewSkillRegistry(dir)
	for _, skill := range loader.LoadFrontmatterOnly() {
		registry.Register(skill)
	}
	if registry.FindVisible("reader", visibleDir) == nil {
		t.Fatal("expected reader skill visible under src")
	}
	if registry.FindVisible("reader", filepath.Join(dir, "docs")) != nil {
		t.Fatal("expected reader skill hidden outside src")
	}
	executor := skills.NewSkillExecutor(loader, skills.NewPromptProcessor(cfg))
	tool := skills.NewSkillTool(registry, executor)
	baseRegistry := coretools.NewToolRegistry(tool)
	persistence := skills.NewSkillPersistence()
	execCtx := coretools.ExecutionContext{
		"cwd":                     visibleDir,
		"_registry":               baseRegistry,
		"_pending_skill_messages": []map[string]any{},
		"_skill_persistence":      persistence,
		"_skill_agent_scope":      "main",
		"_turn_count":             7,
	}
	result := baseRegistry.Execute(context.Background(), coretools.ToolCall{
		ID: "skill-1", Name: "Skill", Input: map[string]any{"skill": "reader", "args": "target"},
	}, execCtx)
	if result.IsError {
		t.Fatalf("skill tool failed: %s", result.Content)
	}
	pending, _ := execCtx["_pending_skill_messages"].([]map[string]any)
	if len(pending) != 1 {
		t.Fatalf("expected one pending skill message, got %#v", pending)
	}
	recovery := persistence.BuildRecoveryMessage("main")
	if recovery == nil {
		t.Fatal("expected skill invocation to be recoverable")
	}
	content, _ := recovery["content"].([]map[string]any)
	if len(content) != 1 || !strings.Contains(content[0]["text"].(string), "Read carefully: target") {
		t.Fatalf("unexpected recovery content: %#v", recovery)
	}
}

func TestSkillPersistenceSnapshotAndBudgeting(t *testing.T) {
	persistence := skills.NewSkillPersistence()
	persistence.RecordInvocation("main", "older", "/tmp/older/SKILL.md", strings.Repeat("old ", 10), 1)
	persistence.RecordInvocation("main", "newer", "/tmp/newer/SKILL.md", "fresh content", 5)
	persistence.RecordInvocation("main", "newer", "/tmp/newer/SKILL.md", "fresh content again", 6)

	attachment := persistence.BuildRecoveryAttachment("main")
	if attachment == nil ||
		!strings.Contains(*attachment, "The following skills were previously invoked") ||
		!strings.Contains(*attachment, "## Skill: newer") {
		t.Fatalf("unexpected recovery attachment: %v", attachment)
	}

	snapshot := persistence.ExportSnapshot()
	restored := skills.NewSkillPersistence()
	restored.ImportSnapshot(snapshot)
	message := restored.BuildRecoveryMessage("main")
	if message == nil {
		t.Fatal("expected restored recovery message")
	}
	metadata, _ := message["metadata"].(map[string]any)
	if metadata["source"] != skills.SkillRecoverySource || metadata[skills.SkillRecoveryMetaKey] != true {
		t.Fatalf("unexpected recovery metadata: %#v", metadata)
	}
}

func TestSkillPersistenceImportSnapshotParsesStringsAndSkipsInvalidLikePython(t *testing.T) {
	restored := skills.NewSkillPersistence()
	restored.ImportSnapshot(map[string]any{
		"version": 1,
		"agent_scopes": map[string]any{
			"main": map[string]any{
				"good": map[string]any{
					"name":             "good",
					"path":             "/tmp/good/SKILL.md",
					"content":          "good content",
					"invoked_at":       "12.5",
					"agent_scope":      "main",
					"last_turn_index":  "7",
					"invocation_count": "2",
				},
				"bad": map[string]any{
					"name":             "bad",
					"path":             "/tmp/bad/SKILL.md",
					"content":          "bad content",
					"invoked_at":       "not-a-number",
					"last_turn_index":  1,
					"invocation_count": 1,
				},
			},
		},
	})
	attachment := restored.BuildRecoveryAttachment("main")
	if attachment == nil {
		t.Fatal("expected valid string-number snapshot record to restore")
	}
	if !strings.Contains(*attachment, "## Skill: good") || strings.Contains(*attachment, "## Skill: bad") {
		t.Fatalf("snapshot import should restore good record and skip invalid numeric record, got:\n%s", *attachment)
	}

	restored = skills.NewSkillPersistence()
	restored.ImportSnapshot(map[string]any{
		"version": "1",
		"agent_scopes": map[string]any{
			"main": map[string]any{
				"wrong-version": map[string]any{
					"content":          "must not import",
					"invoked_at":       1,
					"last_turn_index":  1,
					"invocation_count": 1,
				},
			},
		},
	})
	if attachment := restored.BuildRecoveryAttachment("main"); attachment != nil {
		t.Fatalf("string snapshot version should be rejected like Python, got:\n%s", *attachment)
	}
}

func TestSkillRecoveryBudgetUsesPythonCharacters(t *testing.T) {
	persistence := skills.NewSkillPersistence()
	persistence.RecordInvocation("main", "unicode", "/tmp/技能/SKILL.md", strings.Repeat("技能内容", 6000), 1)
	attachment := persistence.BuildRecoveryAttachment("main")
	if attachment == nil {
		t.Fatal("expected unicode recovery attachment")
	}
	if got := len([]rune(*attachment)); got != 20110 {
		t.Fatalf("recovery attachment should match Python character budget, got chars=%d bytes=%d", got, len([]byte(*attachment)))
	}
	if !strings.Contains(*attachment, "## Skill: unicode\nPath: /tmp/技能/SKILL.md\n\n技能内容") || !strings.Contains(*attachment, "...\n</system-reminder>") {
		t.Fatalf("unexpected unicode recovery attachment ending: %q", (*attachment)[len(*attachment)-min(len(*attachment), 200):])
	}
}

func TestCollectInlineSkillRuntimeCoercesAllowedToolsLikePython(t *testing.T) {
	messages := []map[string]any{{
		"role": "user",
		"metadata": map[string]any{
			"source": skills.SkillInlineSource,
			skills.SkillInlineAllowedToolsKey: []any{
				" read_file ",
				42,
				" ",
			},
		},
	}}
	runtime := skills.CollectInlineSkillRuntime(messages)
	if !runtime.HasAllowedTools {
		t.Fatalf("expected allowed tools to be active")
	}
	seen := map[string]bool{}
	for _, name := range runtime.AllowedToolNames {
		seen[name] = true
	}
	if !seen["read_file"] || !seen["42"] || seen[" read_file "] || seen[" "] {
		t.Fatalf("expected Python str(...).strip() allowed tool coercion, got %#v", runtime.AllowedToolNames)
	}
}

func TestBundledSkillsLoadFromLuminaDirectory(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", filepath.Join(dir, "home"))
	bundledDir := filepath.Join(dir, ".Lumina", "SKILLS", "review")
	if err := os.MkdirAll(bundledDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bundledDir, "SKILL.md"), []byte(`---
name: Review Changes
description: Review code
---
Review it.
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.SkillsDir = ".Lumina/PROJECT_SKILLS"
	cfg.BundledSkillsDir = filepath.Join(dir, ".Lumina", "SKILLS")
	loader := skills.NewSkillLoader(cfg)
	loaded := loader.LoadFrontmatterOnly()
	if len(loaded) != 1 {
		t.Fatalf("expected one bundled skill, got %#v", loaded)
	}
	if loaded[0].Source != skills.SkillSourceBundled || loaded[0].CanonicalName != "review" {
		t.Fatalf("expected .Lumina bundled review skill, got %#v", loaded[0])
	}
}

func TestSkillLoaderOrderingAndHomeExpansionMatchPython(t *testing.T) {
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	t.Setenv("HOME", home)
	userRoot := filepath.Join(home, ".Lumina", "skills")
	for _, name := range []string{"b-skill", "A-skill"} {
		skillDir := filepath.Join(userRoot, name)
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			t.Fatal(err)
		}
		raw := "---\nname: " + name + "\ndescription: test\n---\nBody\n"
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.NewConfig()
	cfg.CWD = filepath.Join(dir, "project")
	cfg.UserSkillsDir = "~/.Lumina/skills"
	cfg.SkillsDir = ".Lumina/PROJECT_SKILLS"
	cfg.BundledSkillsDir = filepath.Join(dir, "missing-bundled")
	loader := skills.NewSkillLoader(cfg)
	if got := loader.UserSkillsDir(); got != userRoot {
		t.Fatalf("expanded user skill dir=%q want %q", got, userRoot)
	}
	loaded := loader.LoadFrontmatterOnly()
	if len(loaded) != 2 {
		t.Fatalf("expected two user skills, got %#v", loaded)
	}
	if loaded[0].CanonicalName != "A-skill" || loaded[1].CanonicalName != "b-skill" {
		t.Fatalf("expected case-insensitive Python ordering, got %q then %q", loaded[0].CanonicalName, loaded[1].CanonicalName)
	}
}

func TestSkillDiscoveryListingUsesPythonCharacterBudgets(t *testing.T) {
	discovery := &skills.SkillDiscovery{}
	unicodeDesc := strings.Repeat("技", 251)
	listing := discovery.FormatListing([]skills.SkillSpec{{
		Frontmatter:   skills.SkillFrontmatter{Name: "Unicode", Description: unicodeDesc},
		Source:        skills.SkillSourceUser,
		CanonicalName: "unicode",
	}}, 10_000, false)
	wantClipped := "- unicode: " + strings.Repeat("技", 247) + "..."
	if !strings.Contains(listing, wantClipped) {
		t.Fatalf("description should clip at Python character count, got:\n%s", listing)
	}

	shortDesc := strings.Repeat("界", 10)
	entry := "- unicode: " + shortDesc
	header := "<system-reminder>\nThe following skills are available for use with the Skill tool:\n\n"
	footer := "\n</system-reminder>"
	exactCharBudget := len([]rune(header + entry + footer))
	exactListing := discovery.FormatListing([]skills.SkillSpec{{
		Frontmatter:   skills.SkillFrontmatter{Name: "Unicode", Description: shortDesc},
		Source:        skills.SkillSourceUser,
		CanonicalName: "unicode",
	}}, exactCharBudget, false)
	if !strings.Contains(exactListing, entry) || strings.Contains(exactListing, "�") {
		t.Fatalf("listing should fit exact Python character budget without byte truncation, got:\n%s", exactListing)
	}
}

func TestSkillRegistryRootContainmentMatchesPythonRelativeTo(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "proj")
	sibling := filepath.Join(dir, "proj-other", "src")
	inside := filepath.Join(root, "src")
	for _, path := range []string{sibling, inside} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	registry := skills.NewSkillRegistry(root)
	spec := skills.SkillSpec{
		Frontmatter:   skills.SkillFrontmatter{Name: "Src Skill", Description: "src", Paths: []string{"/src"}},
		Source:        skills.SkillSourceProject,
		CanonicalName: "src-skill",
	}
	registry.Register(spec)
	if registry.FindVisible("src-skill", inside) == nil {
		t.Fatalf("expected anchored /src skill visible inside root/src")
	}
	if registry.FindVisible("src-skill", sibling) != nil {
		t.Fatalf("anchored /src skill must not be visible for sibling directory outside root")
	}
}

func TestSkillRegistryListOrderAndFirstRegistrationMatchPythonDict(t *testing.T) {
	registry := skills.NewSkillRegistry("")
	first := skills.SkillSpec{
		Frontmatter:   skills.SkillFrontmatter{Name: "Zed", Description: "first"},
		Source:        skills.SkillSourceUser,
		CanonicalName: "Zed",
	}
	duplicate := skills.SkillSpec{
		Frontmatter:   skills.SkillFrontmatter{Name: "zed duplicate", Description: "duplicate"},
		Source:        skills.SkillSourceProject,
		CanonicalName: "zed",
	}
	second := skills.SkillSpec{
		Frontmatter:   skills.SkillFrontmatter{Name: "Alpha", Description: "second"},
		Source:        skills.SkillSourceBundled,
		CanonicalName: "alpha",
	}
	registry.Register(first)
	registry.Register(duplicate)
	registry.Register(second)

	all := registry.ListAll()
	if len(all) != 2 || all[0].CanonicalName != "Zed" || all[1].CanonicalName != "alpha" {
		t.Fatalf("list_all should preserve first-registration Python dict order, got %#v", all)
	}
	if found := registry.Find("zed"); found == nil || found.Frontmatter.Description != "first" {
		t.Fatalf("duplicate canonical skill should not overwrite first registration, got %#v", found)
	}
}

func TestSkillRegistryVisibilityResolvesSymlinkRootAndCWDLikePython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	dir := t.TempDir()
	realRoot := filepath.Join(dir, "real-project")
	realSrc := filepath.Join(realRoot, "src")
	if err := os.MkdirAll(realSrc, 0o755); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(dir, "project-link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	linkSrc := filepath.Join(linkRoot, "src")

	registry := skills.NewSkillRegistry(linkRoot)
	spec := skills.SkillSpec{
		Frontmatter:   skills.SkillFrontmatter{Name: "Src Skill", Description: "src", Paths: []string{"/src"}},
		Source:        skills.SkillSourceProject,
		CanonicalName: "src-skill",
	}
	registry.Register(spec)
	if registry.FindVisible("src-skill", realSrc) == nil {
		t.Fatalf("resolved symlink root should make real cwd visible like Python Path.resolve")
	}
	if registry.FindVisible("src-skill", linkSrc) == nil {
		t.Fatalf("resolved symlink cwd should remain visible like Python Path.resolve")
	}
}

func TestResolveSkillContextCWDResolvesSymlinksLikePython(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink permissions vary on Windows")
	}
	dir := t.TempDir()
	realRoot := filepath.Join(dir, "project-real")
	if err := os.MkdirAll(realRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	linkRoot := filepath.Join(dir, "project-link")
	if err := os.Symlink(realRoot, linkRoot); err != nil {
		t.Skipf("cannot create symlink: %v", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(realRoot)
	if err != nil {
		t.Fatal(err)
	}
	if got := skills.ResolveSkillContextCWD(linkRoot, nil); got != resolvedRoot {
		t.Fatalf("default cwd should resolve symlink like Python Path.resolve, got %q want %q", got, resolvedRoot)
	}
	nestedLink := filepath.Join(linkRoot, "missing", "child")
	want := filepath.Join(resolvedRoot, "missing", "child")
	if got := skills.ResolveSkillContextCWD(dir, map[string]any{"cwd": nestedLink}); got != want {
		t.Fatalf("context cwd should resolve existing symlink prefix like Python Path.resolve, got %q want %q", got, want)
	}
}

func TestSkillFrontmatterEmptyListsPreservedLikePython(t *testing.T) {
	dir := t.TempDir()
	skillDir := filepath.Join(dir, "empty-lists")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skillFile := filepath.Join(skillDir, "SKILL.md")
	raw := `---
name: Empty Lists
description: Empty list metadata
arguments: []
allowed-tools: []
paths: []
---
Body.
`
	if err := os.WriteFile(skillFile, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	fm, _, err := skills.ParseSkillMD(skillFile)
	if err != nil {
		t.Fatal(err)
	}
	if fm.Arguments == nil || len(fm.Arguments) != 0 {
		t.Fatalf("arguments empty list should be preserved, got %#v", fm.Arguments)
	}
	if fm.AllowedTools == nil || len(fm.AllowedTools) != 0 {
		t.Fatalf("allowed-tools empty list should be preserved, got %#v", fm.AllowedTools)
	}
	if fm.Paths == nil || len(fm.Paths) != 0 {
		t.Fatalf("paths empty list should be preserved, got %#v", fm.Paths)
	}
}

func TestSkillFrontmatterEffortCoercionMatchesPython(t *testing.T) {
	dir := t.TempDir()
	floatDir := filepath.Join(dir, "float-effort")
	stringDir := filepath.Join(dir, "string-effort")
	boolDir := filepath.Join(dir, "bool-effort")
	for _, path := range []string{floatDir, stringDir, boolDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	floatFile := filepath.Join(floatDir, "SKILL.md")
	if err := os.WriteFile(floatFile, []byte("---\nname: Float Effort\ndescription: d\neffort: 1.0\n---\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fm, _, err := skills.ParseSkillMD(floatFile)
	if err != nil {
		t.Fatal(err)
	}
	if fm.Effort != nil {
		t.Fatalf("float effort should be ignored like Python, got %#v", fm.Effort)
	}

	stringFile := filepath.Join(stringDir, "SKILL.md")
	if err := os.WriteFile(stringFile, []byte("---\nname: String Effort\ndescription: d\neffort: \"42\"\n---\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fm, _, err = skills.ParseSkillMD(stringFile)
	if err != nil {
		t.Fatal(err)
	}
	if fm.Effort != 42 {
		t.Fatalf("numeric string effort should become int like Python, got %#v", fm.Effort)
	}

	boolFile := filepath.Join(boolDir, "SKILL.md")
	if err := os.WriteFile(boolFile, []byte("---\nname: Bool Effort\ndescription: d\neffort: true\ncontext: fork\n---\nBody.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fm, boolContent, err := skills.ParseSkillMD(boolFile)
	if err != nil {
		t.Fatal(err)
	}
	if fm.Effort != true {
		t.Fatalf("YAML bool effort should pass through like Python bool-is-int behavior, got %#v", fm.Effort)
	}
	cfg := config.NewConfig()
	cfg.CWD = dir
	executor := skills.NewSkillExecutor(skills.NewSkillLoader(cfg), skills.NewPromptProcessor(cfg))
	var capturedPrompt string
	var capturedBudget *int
	executor.ForkRunner = func(_ context.Context, _ skills.SkillSpec, prompt, _ string, thinkingBudgetTokens *int, _ *coretools.ToolRegistry, _ any, _ coretools.ExecutionContext) (string, int, int, error) {
		capturedPrompt = prompt
		capturedBudget = thinkingBudgetTokens
		return "done", 0, 0, nil
	}
	spec := skills.SkillSpec{
		Frontmatter:   fm,
		Source:        skills.SkillSourceBundled,
		Directory:     boolDir,
		SkillFile:     boolFile,
		CanonicalName: "bool-effort",
		Content:       &boolContent,
	}
	if _, err := executor.Execute(context.Background(), spec, "", "session-1", nil, coretools.NewToolRegistry(), nil, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(capturedPrompt, "Reasoning effort: True.") {
		t.Fatalf("bool effort prompt should use Python bool spelling, got:\n%s", capturedPrompt)
	}
	if capturedBudget == nil || *capturedBudget != 1 {
		t.Fatalf("bool true effort should resolve to Python int budget 1, got %#v", capturedBudget)
	}
}

func TestSkillInlineShellSecurityAndApproval(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	cfg.CWD = dir
	processor := skills.NewPromptProcessor(cfg)
	content := "Before !`printf hello` after"
	shellContent := content
	spec := skills.SkillSpec{
		Frontmatter:   skills.SkillFrontmatter{Name: "Shell Skill", Description: "Runs shell"},
		Source:        skills.SkillSourceUser,
		Directory:     dir,
		CanonicalName: "shell-skill",
		Content:       &shellContent,
	}
	_, err := processor.Process(spec, "", "session-1", nil)
	if err == nil || !strings.Contains(err.Error(), "shell command was denied") {
		t.Fatalf("expected user inline shell to require approval, got %v", err)
	}
	approved := false
	rendered, err := processor.Process(spec, "", "session-1", func(req skills.SkillShellPermissionRequest) bool {
		approved = req.SkillName == "shell-skill" && req.Command == "printf hello"
		return approved
	})
	if err != nil {
		t.Fatal(err)
	}
	if !approved || !strings.Contains(rendered, "Before hello after") {
		t.Fatalf("unexpected approved shell rendering: approved=%v rendered=%q", approved, rendered)
	}

	badShell := "python"
	spec.Frontmatter.Shell = &badShell
	_, err = processor.Process(spec, "", "session-1", func(skills.SkillShellPermissionRequest) bool { return true })
	if err == nil || !strings.Contains(err.Error(), "is not allowed for inline shell commands") {
		t.Fatalf("expected custom executable to be blocked, got %v", err)
	}
}

func TestSkillArgumentSubstitutionLongestNamesFirst(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	cfg.CWD = dir
	processor := skills.NewPromptProcessor(cfg)
	content := "$foobar|$foo|${foobar}|${foo}|$ARGUMENTS_EXTRA"
	spec := skills.SkillSpec{
		Frontmatter:   skills.SkillFrontmatter{Name: "Args", Description: "Args", Arguments: []string{"foo", "foobar"}},
		Source:        skills.SkillSourceBundled,
		Directory:     dir,
		CanonicalName: "args",
		Content:       &content,
	}
	rendered, err := processor.Process(spec, "alpha beta", "session-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, "beta|alpha|beta|alpha|alpha beta_EXTRA") {
		t.Fatalf("unexpected named argument substitution: %q", rendered)
	}
}

func TestSkillArgumentSubstitutionOnlyCountsActualPythonReplacements(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	cfg.CWD = dir
	processor := skills.NewPromptProcessor(cfg)
	content := "$fooBar|$ARGUMENTS[bad]"
	spec := skills.SkillSpec{
		Frontmatter:   skills.SkillFrontmatter{Name: "Args", Description: "Args", Arguments: []string{"foo"}},
		Source:        skills.SkillSourceBundled,
		Directory:     dir,
		CanonicalName: "args-boundary",
		Content:       &content,
	}
	rendered, err := processor.Process(spec, "alpha", "session-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, "$fooBar|$ARGUMENTS[bad]\n\nARGUMENTS: alpha\n") {
		t.Fatalf("invalid argument markers should not suppress Python fallback ARGUMENTS append, got %q", rendered)
	}
}

func TestSkillInlineShellApprovalPreflightAndFailureFormatMatchPython(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	cfg.CWD = dir
	processor := skills.NewPromptProcessor(cfg)
	marker := filepath.Join(dir, "marker.txt")
	content := "!`touch marker.txt` then !`printf denied`"
	spec := skills.SkillSpec{
		Frontmatter:   skills.SkillFrontmatter{Name: "Two Shells", Description: "Two"},
		Source:        skills.SkillSourceUser,
		Directory:     dir,
		CanonicalName: "two-shells",
		Content:       &content,
	}
	approvals := 0
	_, err := processor.Process(spec, "", "session-1", func(req skills.SkillShellPermissionRequest) bool {
		approvals++
		return approvals == 1
	})
	if err == nil || !strings.Contains(err.Error(), "shell command was denied") {
		t.Fatalf("expected second inline shell approval denial, got %v", err)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("first shell command should not run before all approvals pass")
	}

	failingContent := "!`printf stdout; printf stderr >&2; exit 7`"
	spec.Source = skills.SkillSourceBundled
	spec.Content = &failingContent
	_, err = processor.Process(spec, "", "session-1", nil)
	if err == nil || !strings.Contains(err.Error(), "Inline shell command failed (7):") || !strings.Contains(err.Error(), "stderr") {
		t.Fatalf("expected Python-style shell failure, got %v", err)
	}
	if strings.HasSuffix(err.Error(), "\n") {
		t.Fatalf("stderr should be stripped like Python, got %q", err.Error())
	}
	timeoutCfg := cfg
	timeoutCfg.ShellTimeoutSeconds = 1.0
	timeoutProcessor := skills.NewPromptProcessor(timeoutCfg)
	timeoutContent := "!`sleep 2`"
	spec.Content = &timeoutContent
	_, err = timeoutProcessor.Process(spec, "", "session-1", nil)
	if err == nil || !strings.Contains(err.Error(), "Inline shell command timed out after 1.0s: sleep 2") {
		t.Fatalf("expected Python-style timeout seconds, got %v", err)
	}
	timeoutCfg.ShellTimeoutSeconds = 0.0
	zeroTimeoutProcessor := skills.NewPromptProcessor(timeoutCfg)
	_, err = zeroTimeoutProcessor.Process(spec, "", "session-1", nil)
	if err == nil || !strings.Contains(err.Error(), "Inline shell command timed out after 0.0s: sleep 2") {
		t.Fatalf("expected Python-style zero timeout behavior, got %v", err)
	}
}

func TestSkillInlineShellOutputLimitUsesPythonDecodedStrippedBytes(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	cfg.CWD = dir
	cfg.ShellMaxOutputBytes = 2
	processor := skills.NewPromptProcessor(cfg)
	content := "!`printf ' ok '`"
	spec := skills.SkillSpec{
		Frontmatter:   skills.SkillFrontmatter{Name: "Shell Bytes", Description: "Shell bytes"},
		Source:        skills.SkillSourceBundled,
		Directory:     dir,
		CanonicalName: "shell-bytes",
		Content:       &content,
	}
	rendered, err := processor.Process(spec, "", "session-1", nil)
	if err != nil {
		t.Fatalf("trimmed output should fit Python byte budget, got %v", err)
	}
	if !strings.Contains(rendered, "ok") {
		t.Fatalf("unexpected rendered output: %q", rendered)
	}

	invalidContent := "!`printf '\\377'`"
	spec.Content = &invalidContent
	_, err = processor.Process(spec, "", "session-1", nil)
	if err == nil || !strings.Contains(err.Error(), "Inline shell command output exceeded 2 bytes") {
		t.Fatalf("invalid UTF-8 replacement should be counted after Python decode, got %v", err)
	}
}

func TestSkillInlineShellSafetyBlocksDangerousCommands(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	cfg.CWD = dir
	processor := skills.NewPromptProcessor(cfg)
	dangerousContent := "!`rm -rf /tmp/not-actually-run`"
	spec := skills.SkillSpec{
		Frontmatter:   skills.SkillFrontmatter{Name: "Danger Skill", Description: "Danger"},
		Source:        skills.SkillSourceBundled,
		Directory:     dir,
		CanonicalName: "danger-skill",
		Content:       &dangerousContent,
	}
	_, err := processor.Process(spec, "", "session-1", nil)
	if err == nil || !strings.Contains(err.Error(), "dangerous pattern") {
		t.Fatalf("expected dangerous command to be blocked, got %v", err)
	}
	substitutionContent := "!`echo $(whoami)`"
	spec.Content = &substitutionContent
	_, err = processor.Process(spec, "", "session-1", nil)
	if err == nil || !strings.Contains(err.Error(), "security checks") {
		t.Fatalf("expected command substitution to be blocked, got %v", err)
	}
}
