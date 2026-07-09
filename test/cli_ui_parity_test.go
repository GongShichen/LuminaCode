package test

import (
	"os"
	"path/filepath"
	"testing"

	luminacli "LuminaCode/cli"
	"LuminaCode/skills"
)

func TestCLIThemeConstantsAndRiskLevelMatchPython(t *testing.T) {
	if luminacli.PromptSymbols["normal"] != "❯" || luminacli.PromptSymbols["yolo"] != "⚡" {
		t.Fatalf("prompt symbols mismatch: %#v", luminacli.PromptSymbols)
	}
	if luminacli.ToolIcons["run_shell"] != "💻" || luminacli.ToolDisplay["run_shell"] != "Bash" {
		t.Fatalf("tool display mismatch: %#v %#v", luminacli.ToolIcons, luminacli.ToolDisplay)
	}
	if luminacli.RiskStyles["high"] != "bold red" || luminacli.RiskBorders["medium"] != "orange1" {
		t.Fatalf("risk style maps mismatch: %#v %#v", luminacli.RiskStyles, luminacli.RiskBorders)
	}
	if luminacli.RiskLabels["medium"] != "Medium Risk - Review Carefully" {
		t.Fatalf("risk labels mismatch: %#v", luminacli.RiskLabels)
	}
	if luminacli.RichTheme["tool.error"] != "bold red" || luminacli.RichTheme["thinking"] != "dim italic" {
		t.Fatalf("rich theme mismatch: %#v", luminacli.RichTheme)
	}

	cases := []struct {
		name  string
		tool  string
		input map[string]any
		want  string
	}{
		{name: "read", tool: "read_file", want: "low"},
		{name: "safe shell", tool: "run_shell", input: map[string]any{"command": "go test ./..."}, want: "low"},
		{name: "medium shell", tool: "run_shell", input: map[string]any{"command": "sudo systemctl restart app"}, want: "medium"},
		{name: "high shell", tool: "run_shell", input: map[string]any{"command": "rm -rf /tmp/x"}, want: "high"},
		{name: "normal write", tool: "write_file", input: map[string]any{"file_path": "/tmp/file"}, want: "medium"},
		{name: "system write", tool: "edit_file", input: map[string]any{"file_path": "/etc/passwd"}, want: "high"},
		{name: "unknown", tool: "custom", input: map[string]any{"anything": "x"}, want: "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := luminacli.ToolRiskLevel(tc.tool, tc.input); got != tc.want {
				t.Fatalf("ToolRiskLevel(%q, %#v)=%q want %q", tc.tool, tc.input, got, tc.want)
			}
		})
	}
}

func TestCLICompleterMatchesPythonSlashCommandBehavior(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "src")
	registry := skills.NewSkillRegistry(dir)
	registry.Register(skills.SkillSpec{
		Frontmatter: skills.SkillFrontmatter{
			Description:   "Run smoke checks",
			Paths:         []string{"src/**"},
			UserInvocable: true,
		},
		Source:        skills.SkillSourceProject,
		CanonicalName: "smoke",
	})

	completions := luminacli.CompleteInput("  /s", registry, cwd)
	var names []string
	for _, completion := range completions {
		names = append(names, completion.Display)
		if completion.StartPosition != -2 {
			t.Fatalf("slash completion should replace stripped command text, got %#v", completion)
		}
	}
	want := []string{"/save", "/s", "/skill", "/storage", "/smoke"}
	if len(names) != len(want) {
		t.Fatalf("slash completion count mismatch: got %#v want %#v", names, want)
	}
	for i, name := range names {
		if name != want[i] {
			t.Fatalf("slash completion[%d]=%q want %q; all=%#v", i, name, want[i], names)
		}
	}
	if completions[0].Text != "/save" || completions[0].DisplayMeta != "Save session to disk" {
		t.Fatalf("unexpected slash completion metadata: %#v", completions[0])
	}

	allCommands := luminacli.CompleteInput("/", registry, cwd)
	if len(allCommands) <= len(completions) {
		t.Fatalf("bare slash should show the command menu, got %#v", allCommands)
	}
	if !luminacli.SlashCompletionActive("/") || !luminacli.SlashCompletionActive("  /he") {
		t.Fatal("slash completion should be active while typing the command token")
	}
	for _, input := range []string{"/ ", "/help ", "  /resume abc"} {
		if luminacli.SlashCompletionActive(input) {
			t.Fatalf("slash completion should disappear after command whitespace for %q", input)
		}
		if got := luminacli.CompleteInput(input, registry, cwd); len(got) != 0 {
			t.Fatalf("slash command with arguments should not show command completions for %q: %#v", input, got)
		}
	}
}

func TestCLICompleterMatchesPythonPathBehavior(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "alpha.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "alpine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "child.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	completions := luminacli.CompleteInput("al", nil, dir)
	if len(completions) != 2 {
		t.Fatalf("path completion count mismatch: %#v", completions)
	}
	if completions[0].Text != "pha.txt" || completions[0].Display != "alpha.txt" || completions[0].StartPosition != 0 {
		t.Fatalf("file path completion should insert Python-style suffix, got %#v", completions[0])
	}
	if completions[1].Text != "pine" || completions[1].Display != "alpine/" {
		t.Fatalf("directory path completion should show trailing slash, got %#v", completions[1])
	}
	for _, completion := range luminacli.CompleteInput("", nil, dir) {
		if completion.Display == ".git/" {
			t.Fatalf(".git should be hidden from path completions: %#v", completion)
		}
	}
	nested := luminacli.CompleteInput("nested/", nil, dir)
	if len(nested) != 1 || nested[0].Text != "child.txt" || nested[0].Display != "child.txt" {
		t.Fatalf("nested path completion mismatch: %#v", nested)
	}
	if got := luminacli.CompleteInput("cat al", nil, dir); len(got) != 0 {
		t.Fatalf("prompt_toolkit PathCompleter does not complete shell-style later tokens, got %#v", got)
	}
}
