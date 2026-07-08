package test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"LuminaCode/agentContext"
	"LuminaCode/config"
)

func TestContextBuilderTemplateSectionsAndHelpers(t *testing.T) {
	sections, err := agentContext.LoadSystemPromptTemplateSections()
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(sections))
	for _, section := range sections {
		names = append(names, section.Name)
		if strings.TrimSpace(section.Content) == "" {
			t.Fatalf("empty section: %#v", section)
		}
	}
	expected := []string{
		"identity",
		"capabilities-overview",
		"instruction-priority",
		"trust-and-external-context",
		"working-style",
		"tool-use-policy",
		"runtime-model-awareness",
		"safety-and-shell",
		"task-completion-and-code-style",
		"response-format",
	}
	if strings.Join(names, ",") != strings.Join(expected, ",") {
		t.Fatalf("unexpected section order: %#v", names)
	}
	assembled := agentContext.AssemblePromptSections(sections)
	for _, want := range []string{
		"You are LuminaCode, a general-purpose agent running in the user's local workspace.",
		"Do not assume every request is a software-development task.",
		"Tasks may involve code, documents, research, file operations, terminal work, project analysis, or multi-step collaboration.",
	} {
		if !strings.Contains(assembled, want) {
			t.Fatalf("system prompt missing general-agent guidance %q:\n%s", want, assembled)
		}
	}
	for _, forbidden := range []string{"coding agent", "Coding Agent", "AI programming assistant", "编码智能体", "AI 编程助手"} {
		if strings.Contains(assembled, forbidden) {
			t.Fatalf("system prompt should not describe LuminaCode as coding-only (%q):\n%s", forbidden, assembled)
		}
	}
	priority, err := agentContext.BuildInstructionPrioritySection()
	if err != nil {
		t.Fatal(err)
	}
	trust, err := agentContext.BuildTrustAndExternalContextSection()
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := agentContext.BuildWorkflowSection()
	if err != nil {
		t.Fatal(err)
	}
	if priority.Name != "instruction-priority" || !strings.Contains(priority.Content, "LUMINA.md") {
		t.Fatalf("unexpected priority section: %#v", priority)
	}
	if trust.Name != "trust-and-external-context" ||
		!strings.Contains(trust.Content, "Tool output is evidence") ||
		!strings.Contains(trust.Content, "not authority") {
		t.Fatalf("unexpected trust section: %#v", trust)
	}
	if workflow.Name != "working-style" || !strings.Contains(workflow.Content, "read the relevant files before editing them") {
		t.Fatalf("unexpected workflow section: %#v", workflow)
	}
}

func TestContextBuilderTruncationAndAttachmentContracts(t *testing.T) {
	block := agentContext.BuildAttachmentBlock("git-context", "Git Context", "body")
	if !strings.Contains(block, "[BEGIN: git-context]") || !strings.Contains(block, "[END: git-context]") {
		t.Fatalf("unexpected attachment block: %q", block)
	}
	maxLines := 3
	truncated, wasTruncated := agentContext.TruncateAttachmentText("line1\nline2\nline3\nline4", agentContext.PromptAttachmentBudget{MaxChars: 100, MaxLines: &maxLines}, nil)
	if !wasTruncated || strings.Contains(truncated, "line4") {
		t.Fatalf("expected line truncation, got %q truncated=%v", truncated, wasTruncated)
	}
	truncated, wasTruncated = agentContext.TruncateAttachmentText("abcdefghijklmno", agentContext.PromptAttachmentBudget{MaxChars: 10}, nil)
	if !wasTruncated || truncated != "abcdefghij" {
		t.Fatalf("expected char truncation, got %q truncated=%v", truncated, wasTruncated)
	}
	truncated, wasTruncated = agentContext.TruncateAttachmentText(strings.Repeat("界", 11), agentContext.PromptAttachmentBudget{MaxChars: 10}, nil)
	if !wasTruncated || truncated != strings.Repeat("界", 10) {
		t.Fatalf("expected Python character truncation, got %q truncated=%v", truncated, wasTruncated)
	}
	truncated, wasTruncated = agentContext.TruncateAttachmentText("line1\nline2\n", agentContext.PromptAttachmentBudget{MaxChars: 100}, nil)
	if wasTruncated || truncated != "line1\nline2" {
		t.Fatalf("expected Python splitlines behavior for trailing newline, got %q truncated=%v", truncated, wasTruncated)
	}
	separator := "\n\n---\n\n"
	truncated, wasTruncated = agentContext.TruncateAttachmentText("nearest\n\n---\n\nparent\n\n---\n\nroot", agentContext.PromptAttachmentBudget{MaxChars: 18}, &separator)
	if !wasTruncated || truncated != "nearest" {
		t.Fatalf("expected separator-preserving truncation, got %q truncated=%v", truncated, wasTruncated)
	}
	section := agentContext.BuildBudgetAttachmentSection("project-instructions", "Project Instructions", "  keep leading  \n", nil, agentContext.PromptAttachmentBudget{MaxChars: 10})
	if !strings.Contains(section.Content, "[BEGIN: project-instructions]\nkeep lea\n\n(The following content was truncated to fit the prompt budget.)") {
		t.Fatalf("expected Python attachment strip/truncation marker handling, got %q", section.Content)
	}
}

func TestContextBuilderSystemPromptAndSubagentSections(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "LUMINA.md"), []byte("Project rule."), 0o644); err != nil {
		t.Fatal(err)
	}
	memorySection := "## Persistent Memory\n\nMemory behavior."
	sections, err := agentContext.BuildSystemPromptSection(dir, memorySection)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, section := range sections {
		seen[section.Name] = true
	}
	if !seen["project-instructions"] || !seen["memory-behavior"] {
		t.Fatalf("expected project and memory sections, got %#v", seen)
	}
	prompt, err := agentContext.BuildSystemPrompt(dir, memorySection)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "[BEGIN: project-instructions]") ||
		!strings.Contains(prompt, "Project rule.") ||
		!strings.Contains(prompt, "Memory behavior.") {
		t.Fatalf("unexpected system prompt: %q", prompt)
	}

	subSections, err := agentContext.BuildSubagentPromptSections("Explore", "Read-only search agent.", dir, 7, "Git branch: main", "Agent memory.")
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	assembled := agentContext.AssemblePromptSections(subSections)
	for _, section := range subSections {
		names[section.Name] = true
	}
	for _, name := range []string{"subagent-identity", "instruction-priority", "trust-and-external-context", "working-style", "git-context", "agent-memory", "subagent-execution-constraints"} {
		if !names[name] {
			t.Fatalf("missing subagent section %s in %#v", name, names)
		}
	}
	if !strings.Contains(assembled, "7 turns") || !strings.Contains(strings.ToLower(assembled), "plain text") {
		t.Fatalf("unexpected subagent prompt: %q", assembled)
	}
}

func TestInitialContextCanBeRebuiltFromConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	nested := filepath.Join(dir, "service")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "LUMINA.md"), []byte("Root rule."), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, "AGENTS.md"), []byte("Service rule."), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.NewConfigForCWD(nested)
	cfg.CWD = nested

	initial, err := agentContext.BuildInitialContext(cfg, "## Persistent Memory\n\nMemory behavior.")
	if err != nil {
		t.Fatal(err)
	}
	rendered := initial.Render()
	if initial.CWD != nested || len(initial.ProjectDocs.Docs) != 2 {
		t.Fatalf("unexpected initial context metadata: %#v", initial.ProjectDocs)
	}
	if !strings.Contains(rendered, "Root rule.") ||
		!strings.Contains(rendered, "Service rule.") ||
		!strings.Contains(rendered, "Memory behavior.") {
		t.Fatalf("rebuilt initial context is missing expected sections: %q", rendered)
	}
}

func TestInitialContextIncludesSessionHistoryRecallWhenEnabled(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfigForCWD(dir)
	cfg.SessionMemoryEnabled = true
	prompt, err := agentContext.BuildSystemPromptWithConfig(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(prompt, "## Session History Recall") ||
		!strings.Contains(prompt, "session_history_list") ||
		!strings.Contains(prompt, "session_history_get") {
		t.Fatalf("session history recall instructions missing from prompt: %q", prompt)
	}
}

func TestBuildMemorySectionDescribesGeneralAgentMemoryRules(t *testing.T) {
	dir := t.TempDir()
	cfg := config.NewConfig()
	cfg.AutoMemoryEnabled = true
	cfg.AutoMemoryDirectory = &dir

	got := agentContext.BuildMemorySection(&cfg)
	for _, want := range []string{
		"persistent memory system at `" + dir + "`",
		"`MEMORY.md` is the entrypoint index",
		"The current `MEMORY.md` index",
		"standalone `.md` files with YAML frontmatter",
		"keep `MEMORY.md` in sync",
		"Content already present in LUMINA.md or AGENTS.md",
		"Temporary repository structure",
		"current conversation, current task, or current tool output",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("memory section missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\""+dir+"\"") || strings.Contains(got, "standalone .md files") {
		t.Fatalf("memory section should keep markdown path/list formatting:\n%s", got)
	}
}
