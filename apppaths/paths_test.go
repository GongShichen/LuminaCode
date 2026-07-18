package apppaths

import (
	"bufio"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestResolvePlatformRoots(t *testing.T) {
	tests := []struct {
		name  string
		goos  string
		home  string
		local string
		want  string
	}{
		{name: "darwin", goos: "darwin", home: "/Users/tester", want: "/Users/tester/.lumina"},
		{name: "linux", goos: "linux", home: "/home/tester", want: "/home/tester/.lumina"},
		{name: "windows local app data", goos: "windows", home: `C:\Users\tester`, local: `C:\Users\tester\AppData\Local`, want: `C:\Users\tester\AppData\Local\LuminaCode`},
		{name: "windows fallback", goos: "windows", home: `C:\Users\tester`, want: `C:\Users\tester\.lumina`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			paths, err := Resolve(ResolveOptions{GOOS: tt.goos, HomeDir: tt.home, LocalAppData: tt.local, Env: map[string]string{}})
			if err != nil {
				t.Fatal(err)
			}
			if paths.Root != tt.want {
				t.Fatalf("root=%q want %q", paths.Root, tt.want)
			}
		})
	}
}

func TestSharedAppPathContract(t *testing.T) {
	file, err := os.Open(filepath.Join("..", "testdata", "app-path-contract.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "|")
		if len(fields) != 6 {
			t.Fatalf("invalid path contract row: %q", line)
		}
		for index := 2; index <= 4; index++ {
			if fields[index] == "-" {
				fields[index] = ""
			}
		}
		t.Run(fields[0], func(t *testing.T) {
			paths, err := Resolve(ResolveOptions{
				GOOS: fields[1], HomeDir: fields[2], LocalAppData: fields[3],
				Env: map[string]string{"LUMINA_APP_ROOT": fields[4]},
			})
			if err != nil {
				t.Fatal(err)
			}
			if paths.Root != fields[5] {
				t.Fatalf("root=%q want %q", paths.Root, fields[5])
			}
		})
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestResolveOverrideAndLayout(t *testing.T) {
	root := filepath.Join(t.TempDir(), "custom root")
	paths, err := Resolve(ResolveOptions{GOOS: runtime.GOOS, HomeDir: t.TempDir(), Env: map[string]string{"LUMINA_APP_ROOT": root}})
	if err != nil {
		t.Fatal(err)
	}
	if paths.SettingsFile != filepath.Join(root, "config", "settings.json") || paths.EndpointFile != filepath.Join(root, "state", "run", "backend.json") {
		t.Fatalf("unexpected paths: %#v", paths)
	}
	if err := WriteLayout(paths, "test"); err != nil {
		t.Fatal(err)
	}
	if err := CheckLayout(paths); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(paths.LayoutFile)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("layout mode=%o", info.Mode().Perm())
	}
}

func TestResolveRejectsRelativeOverride(t *testing.T) {
	_, err := Resolve(ResolveOptions{GOOS: "linux", HomeDir: t.TempDir(), Env: map[string]string{"LUMINA_APP_ROOT": "relative/root"}})
	if err == nil {
		t.Fatal("relative LUMINA_APP_ROOT must be rejected")
	}
}

func TestResolveRejectsRelativePlatformDefaults(t *testing.T) {
	if _, err := Resolve(ResolveOptions{GOOS: "linux", HomeDir: "relative-home", Env: map[string]string{}}); err == nil {
		t.Fatal("relative POSIX home was accepted")
	}
	if _, err := Resolve(ResolveOptions{GOOS: "windows", HomeDir: `C:\Users\tester`, LocalAppData: "relative-local", Env: map[string]string{}}); err == nil {
		t.Fatal("relative Windows LOCALAPPDATA was accepted")
	}
}

func TestResolveWindowsLocalAppDataFromEnvironment(t *testing.T) {
	local := `C:\Users\tester\AppData\Local`
	paths, err := Resolve(ResolveOptions{GOOS: "windows", HomeDir: `C:\Users\tester`, Env: map[string]string{"LOCALAPPDATA": local}})
	if err != nil {
		t.Fatal(err)
	}
	if paths.Root != `C:\Users\tester\AppData\Local\LuminaCode` {
		t.Fatalf("root=%q", paths.Root)
	}
}

func TestResolveWindowsOverrideUsesWindowsSemanticsOnEveryHost(t *testing.T) {
	paths, err := Resolve(ResolveOptions{
		GOOS: "windows", HomeDir: `C:\Users\tester`,
		Env: map[string]string{"LUMINA_APP_ROOT": `D:\Lumina Data\..\LuminaCode`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if paths.Root != `D:\LuminaCode` || paths.EndpointFile != `D:\LuminaCode\state\run\backend.json` {
		t.Fatalf("unexpected synthetic Windows paths: %#v", paths)
	}
}

func TestProjectIDSeparatesSameBasename(t *testing.T) {
	base := t.TempDir()
	one := filepath.Join(base, "a", "repo")
	two := filepath.Join(base, "b", "repo")
	if err := os.MkdirAll(one, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(two, 0o755); err != nil {
		t.Fatal(err)
	}
	paths, err := Resolve(ResolveOptions{GOOS: runtime.GOOS, HomeDir: t.TempDir(), Env: map[string]string{}})
	if err != nil {
		t.Fatal(err)
	}
	p1, err := paths.ForProject(one)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := paths.ForProject(two)
	if err != nil {
		t.Fatal(err)
	}
	if p1.ID == p2.ID || !strings.HasPrefix(p1.ID, "repo-") || !strings.HasPrefix(p2.ID, "repo-") {
		t.Fatalf("ids must be readable and distinct: %q %q", p1.ID, p2.ID)
	}
	if err := EnsureProjectManifest(p1, time.Unix(10, 0)); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverProjectRootUsesNearestMarker(t *testing.T) {
	outer := t.TempDir()
	if err := os.Mkdir(filepath.Join(outer, ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	nestedProject := filepath.Join(outer, "packages", "demo")
	if err := os.MkdirAll(filepath.Join(nestedProject, ProjectLocalDirName), 0o700); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(nestedProject, "src", "internal")
	if err := os.MkdirAll(workDir, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := DiscoverProjectRoot(workDir, []string{".git"})
	if err != nil {
		t.Fatal(err)
	}
	if root != nestedProject {
		t.Fatalf("root=%q want nearest project %q", root, nestedProject)
	}
}

func TestEnsureProjectManifestRefusesMalformedExistingFile(t *testing.T) {
	root := t.TempDir()
	projectRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(projectRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := Resolve(ResolveOptions{GOOS: runtime.GOOS, HomeDir: t.TempDir(), Env: map[string]string{"LUMINA_APP_ROOT": root}})
	if err != nil {
		t.Fatal(err)
	}
	project, err := paths.ForProject(projectRoot)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(project.ManifestFile), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(project.ManifestFile, []byte("not-json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProjectManifest(project, time.Now()); err == nil {
		t.Fatal("malformed project manifest was overwritten")
	}
	data, err := os.ReadFile(project.ManifestFile)
	if err != nil || string(data) != "not-json" {
		t.Fatalf("malformed project manifest changed: %q %v", data, err)
	}
}

func TestWindowsProjectCanonicalization(t *testing.T) {
	tests := []struct {
		left  string
		right string
	}{
		{`C:\Users\Alice\Repo`, `c:/users/alice/repo/`},
		{`\\Server\Share\Team\Repo`, `//server/share/team/repo/`},
		{`D:\work\parent\child\..\repo`, `d:/work/parent/repo`},
		{`\\Server\Share\..\Repo`, `//server/share/repo`},
	}
	for _, tt := range tests {
		left, err := CanonicalProjectRoot(tt.left, "windows")
		if err != nil {
			t.Fatal(err)
		}
		right, err := CanonicalProjectRoot(tt.right, "windows")
		if err != nil {
			t.Fatal(err)
		}
		if left != right || ProjectIDFromCanonical(left) != ProjectIDFromCanonical(right) {
			t.Fatalf("windows paths did not canonicalize equally: %q != %q", left, right)
		}
	}
}

func TestProjectIDTruncatesUnicodeSlugByRune(t *testing.T) {
	id := ProjectIDFromCanonical("/workspace/" + strings.Repeat("项目", 40))
	slug, _, ok := strings.Cut(id, "-")
	if !ok || !utf8.ValidString(id) || utf8.RuneCountInString(slug) > 48 {
		t.Fatalf("invalid project id: %q", id)
	}
}

func TestLegacyLayoutRequiresMigration(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "CONFIG"), 0o755); err != nil {
		t.Fatal(err)
	}
	paths, err := Resolve(ResolveOptions{GOOS: runtime.GOOS, HomeDir: t.TempDir(), Env: map[string]string{"LUMINA_APP_ROOT": root}})
	if err != nil {
		t.Fatal(err)
	}
	if err := CheckLayout(paths); err != ErrMigrationRequired {
		t.Fatalf("got %v want ErrMigrationRequired", err)
	}
}

func TestLowercaseV2ConfigIsNotLegacyOnCaseInsensitiveFilesystems(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if HasLegacyLayout(root) {
		t.Fatal("lowercase v2 config directory was classified as legacy CONFIG")
	}
}

func TestRuntimeRefusesUninitializedLayout(t *testing.T) {
	paths, err := Resolve(ResolveOptions{
		GOOS: runtime.GOOS, HomeDir: t.TempDir(), Env: map[string]string{"LUMINA_APP_ROOT": t.TempDir()},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareRuntime(paths, "test"); err != ErrLayoutNotInitialized {
		t.Fatalf("got %v want ErrLayoutNotInitialized", err)
	}
	if _, err := os.Stat(paths.LayoutFile); !os.IsNotExist(err) {
		t.Fatalf("runtime wrote an uninitialized layout: %v", err)
	}
}
