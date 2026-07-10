package test

import (
	"path/filepath"
	"testing"

	luminacli "LuminaCode/cli"
	"LuminaCode/skills"
)

func TestCLICommandMetadataMatchesPythonBuiltins(t *testing.T) {
	wantCommands := []string{
		"/help",
		"/clear",
		"/save",
		"/s",
		"/tokens",
		"/compact",
		"/compress",
		"/yolo",
		"/skill",
		"/Team",
		"/TeamOut",
		"/TeamSummary",
		"/NewTeam",
		"/Memory",
		"/MemorySearch",
		"/MemoryForget",
		"/MemoryApprove",
		"/MemoryRestore",
		"/MemoryPrioritize",
		"/MemoryDeprioritize",
		"/MemorySupersede",
		"/MemoryExport",
		"/MemoryImport",
		"/mcp",
		"/resume",
		"/storage",
		"/cleanup",
		"/pin",
		"/unpin",
		"/quit",
		"/q",
		"/exit",
	}
	if len(luminacli.BuiltinCommands) != len(wantCommands) {
		t.Fatalf("command count mismatch: got %#v want %#v", luminacli.BuiltinCommands, wantCommands)
	}
	for i, want := range wantCommands {
		if luminacli.BuiltinCommands[i] != want {
			t.Fatalf("command[%d]=%q want %q", i, luminacli.BuiltinCommands[i], want)
		}
	}
	if luminacli.CommandMeta["/s"] != "Alias for /save" {
		t.Fatalf("alias metadata mismatch: %#v", luminacli.CommandMeta)
	}
	for _, command := range []string{"/q", "/quit", "/exit"} {
		if !luminacli.IsExitCommand(command) {
			t.Fatalf("expected %s to be an exit command", command)
		}
	}
}

func TestCLIHelpAndCompletionRowsIncludeVisibleSkills(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "src")
	registry := skills.NewSkillRegistry(dir)
	argHint := "target"
	registry.Register(skills.SkillSpec{
		Frontmatter: skills.SkillFrontmatter{
			Description:   "Review code",
			ArgumentHint:  &argHint,
			Paths:         []string{"src/**"},
			UserInvocable: true,
		},
		Source:        skills.SkillSourceProject,
		CanonicalName: "review",
	})
	registry.Register(skills.SkillSpec{
		Frontmatter: skills.SkillFrontmatter{
			Description:   "Hidden",
			Paths:         []string{"docs/**"},
			UserInvocable: true,
		},
		Source:        skills.SkillSourceProject,
		CanonicalName: "hidden",
	})

	helpRows := luminacli.IterCommandHelpRows(registry, cwd)
	lastHelp := helpRows[len(helpRows)-1]
	if lastHelp.Command != "/review" || lastHelp.Description != "target" {
		t.Fatalf("expected visible skill help row last, got %#v", helpRows)
	}
	for _, row := range helpRows {
		if row.Command == "/hidden" {
			t.Fatalf("hidden skill should not be in help rows: %#v", helpRows)
		}
	}

	completionItems := luminacli.IterCommandCompletionItems(registry, cwd)
	lastCompletion := completionItems[len(completionItems)-1]
	if lastCompletion.Name != "/review" || lastCompletion.Description != "target" {
		t.Fatalf("expected visible skill completion last, got %#v", completionItems)
	}
}

func TestCLIHelpAndCompletionSkillOrderMatchesPythonRegistryOrder(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "src")
	registry := skills.NewSkillRegistry(dir)
	for _, name := range []string{"zeta", "alpha"} {
		registry.Register(skills.SkillSpec{
			Frontmatter: skills.SkillFrontmatter{
				Description:   name + " desc",
				Paths:         []string{"src/**"},
				UserInvocable: true,
			},
			Source:        skills.SkillSourceProject,
			CanonicalName: name,
		})
	}

	helpRows := luminacli.IterCommandHelpRows(registry, cwd)
	if got := []string{helpRows[len(helpRows)-2].Command, helpRows[len(helpRows)-1].Command}; got[0] != "/zeta" || got[1] != "/alpha" {
		t.Fatalf("skill help rows should preserve Python registry order, got %#v", got)
	}
	completionItems := luminacli.IterCommandCompletionItems(registry, cwd)
	if got := []string{completionItems[len(completionItems)-2].Name, completionItems[len(completionItems)-1].Name}; got[0] != "/zeta" || got[1] != "/alpha" {
		t.Fatalf("skill completion items should preserve Python registry order, got %#v", got)
	}
}

func TestCLIReplSlashDispatchMatchesPython(t *testing.T) {
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
	registry.Register(skills.SkillSpec{
		Frontmatter: skills.SkillFrontmatter{
			Description:   "Hidden elsewhere",
			Paths:         []string{"docs/**"},
			UserInvocable: true,
		},
		Source:        skills.SkillSourceProject,
		CanonicalName: "hidden",
	})

	cases := []struct {
		input string
		want  luminacli.SlashDispatchKind
		name  string
	}{
		{input: "hello", want: luminacli.SlashDispatchNone},
		{input: "/quit", want: luminacli.SlashDispatchExit, name: "quit"},
		{input: "/q now", want: luminacli.SlashDispatchUnknown, name: "q"},
		{input: "/help", want: luminacli.SlashDispatchBuiltin, name: "help"},
		{input: "/help extra", want: luminacli.SlashDispatchUnknown, name: "help"},
		{input: "/resume abc123", want: luminacli.SlashDispatchBuiltin, name: "resume"},
		{input: "/yolo please", want: luminacli.SlashDispatchBuiltin, name: "yolo"},
		{input: "/smoke target", want: luminacli.SlashDispatchSkill, name: "smoke"},
		{input: "/hidden target", want: luminacli.SlashDispatchUnknown, name: "hidden"},
		{input: "/does-not-exist", want: luminacli.SlashDispatchUnknown, name: "does-not-exist"},
	}

	for _, tc := range cases {
		got := luminacli.ClassifyREPLSlashCommand(tc.input, registry, cwd)
		if got.Kind != tc.want || got.Name != tc.name {
			t.Fatalf("ClassifyREPLSlashCommand(%q)=%#v want kind=%q name=%q", tc.input, got, tc.want, tc.name)
		}
	}
}
