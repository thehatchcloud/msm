package compatibility

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The no-shell-manager audit (P17, DEV-039). It fails when the release source
// tree gains a Bash manager, a sourced configuration, a shell launcher, a
// runtime .sh profile, or a subprocess that docs/audit.md does not list.

// harnessScripts are the only scripts in the tree. They are test harnesses
// that CI runs on disposable machines; the msm executable never runs them and
// they are not part of a release archive.
var harnessScripts = map[string]bool{
	"compatibility/migration/rehearse.sh": true,
	"compatibility/worldedit/build.sh":    true,
}

// removedPaths belonged to the inherited Bash implementation. They live in git
// tag legacy-bash-final, not in the release source tree.
var removedPaths = []string{
	"init", "cron", "installers", "versioning", "bash_completion",
	"test.sh", "msm.conf", ".github/workflows/test.yml",
}

// execFiles are the only non-test Go files that start a process, and why.
// Every one passes an explicit argument vector; none goes through a shell.
var execFiles = map[string]string{
	"internal/process/process.go": "the single runner behind screen, java, rdiff-backup and nice",
	"internal/update/update.go":   "runs the staged, signature-verified new executable with `version`",
	"internal/install/install.go": "systemctl / launchctl after `install service --write --enable`",
	"internal/worlds/worlds.go":   "hdiutil info, to prove a macOS RAM volume",
	"internal/cli/migrate.go":     "systemctl, when migrating a host that uses systemd",
	"internal/cli/server.go":      "looks screen up on PATH (exec.LookPath only)",
	"internal/cli/install.go":     "exec.LookPath for `install check` (never runs the program)",
	"internal/cli/help.go":        "exec.LookPath, to name the executable as typed",
	"internal/backup/backup.go":   "exec.LookPath for the optional rdiff-backup adapter",
}

// programs are the external programs a literal call may name.
var programs = map[string]bool{
	"screen": true, "java": true, "rdiff-backup": true, "nice": true,
	"systemctl": true, "launchctl": true, "hdiutil": true,
}

// shellWords must never appear as a program or an argument to one. The one
// exception is the legacy VERSIONING_FILE_EXTENSION default, which is data.
var shellWords = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"/bin/sh": true, "/bin/bash": true, "/usr/bin/env": true, "env": true,
	"eval": true, "source": true, "-c": true, "su": true, "sudo": true,
}

var shellWordAllowed = map[string]bool{
	"internal/legacyconf/globals.go": true, // VERSIONING_FILE_EXTENSION default, listed data
	"internal/migrate/cron.go":       true, // the cron entry kind "env" (an environment line)
	"internal/migrate/inventory.go":  true, // the extension of legacy version profiles, read as data
}

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func walk(t *testing.T, root string, visit func(rel string, d fs.DirEntry)) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() && (rel == ".git" || rel == "bin" || rel == "dist") {
			return filepath.SkipDir
		}
		visit(filepath.ToSlash(rel), d)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestNoShellManagerRemains(t *testing.T) {
	root := repoRoot(t)
	for _, p := range removedPaths {
		if _, err := os.Lstat(filepath.Join(root, p)); err == nil {
			t.Errorf("%s belongs to the removed Bash manager (DEV-039); it lives in git tag legacy-bash-final", p)
		}
	}
	walk(t, root, func(rel string, d fs.DirEntry) {
		if d.IsDir() {
			return
		}
		if strings.HasSuffix(rel, ".sh") && !harnessScripts[rel] {
			t.Errorf("%s: a shell script that is not a listed test harness", rel)
		}
		if harnessScripts[rel] || !d.Type().IsRegular() {
			return
		}
		f, err := os.Open(filepath.Join(root, rel))
		if err != nil {
			t.Error(err)
			return
		}
		defer f.Close()
		head := make([]byte, 2)
		if n, _ := f.Read(head); n == 2 && string(head) == "#!" {
			t.Errorf("%s starts with a shebang; only the listed test harnesses are scripts", rel)
		}
	})
	for _, h := range sortedKeys(harnessScripts) {
		if _, err := os.Stat(filepath.Join(root, h)); err != nil {
			t.Errorf("listed harness %s: %v", h, err)
		}
	}
	// Workflows may not bring the Bash manager's suite back.
	workflows, _ := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.yml"))
	for _, w := range workflows {
		data, err := os.ReadFile(w)
		if err != nil {
			t.Fatal(err)
		}
		for _, bad := range []string{"shunit2", "test.sh", "installers/", "init/msm", "git.io/"} {
			if strings.Contains(string(data), bad) {
				t.Errorf("%s mentions %q, which belongs to the removed Bash manager", filepath.Base(w), bad)
			}
		}
	}
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestSubprocessUsageIsAudited(t *testing.T) {
	root := repoRoot(t)
	fset := token.NewFileSet()
	walk(t, root, func(rel string, d fs.DirEntry) {
		if d.IsDir() || !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
			return
		}
		file, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Errorf("%s: %v", rel, err)
			return
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				x, ok := n.X.(*ast.Ident)
				if !ok {
					return true
				}
				switch {
				case x.Name == "exec" && (n.Sel.Name == "Command" || n.Sel.Name == "CommandContext"):
					if _, ok := execFiles[rel]; !ok {
						t.Errorf("%s: starts a process (exec.%s) but is not listed in docs/audit.md", rel, n.Sel.Name)
					}
				case x.Name == "syscall" && (n.Sel.Name == "Exec" || n.Sel.Name == "ForkExec"),
					x.Name == "os" && n.Sel.Name == "StartProcess":
					t.Errorf("%s: %s.%s replaces or starts a process outside the audited runner", rel, x.Name, n.Sel.Name)
				}
			case *ast.CallExpr:
				name := callName(n)
				switch name {
				case "Run", "Have", "LookPath", "run", "execProgram":
					if len(n.Args) > 0 {
						if lit, ok := n.Args[0].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							prog, _ := strconv.Unquote(lit.Value)
							if !programs[prog] {
								t.Errorf("%s: %s(%q) names a program that docs/audit.md does not list", rel, name, prog)
							}
						}
					}
				}
			case *ast.BasicLit:
				if n.Kind != token.STRING {
					return true
				}
				if v, err := strconv.Unquote(n.Value); err == nil && shellWords[v] && !shellWordAllowed[rel] {
					t.Errorf("%s: the string %q looks like a shell or a shell option", rel, v)
				}
			}
			return true
		})
	})
	for _, f := range sortedKeys(execFiles) {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("audited file %s: %v", f, err)
		}
	}
	// The audit document names every audited file and program.
	doc, err := os.ReadFile(filepath.Join(root, "docs", "audit.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range sortedKeys(execFiles) {
		if !strings.Contains(string(doc), "`"+f+"`") {
			t.Errorf("docs/audit.md does not list %s", f)
		}
	}
	for _, p := range sortedKeys(programs) {
		if !strings.Contains(string(doc), "`"+p+"`") {
			t.Errorf("docs/audit.md does not list the program %s", p)
		}
	}
	for _, h := range sortedKeys(harnessScripts) {
		if !strings.Contains(string(doc), "`"+h+"`") {
			t.Errorf("docs/audit.md does not list the harness %s", h)
		}
	}
}

func callName(c *ast.CallExpr) string {
	switch f := c.Fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}

// TestDependenciesAreReviewed ties go.mod to docs/dependencies.md: a module or
// version that changes needs its license reviewed on that page.
func TestDependenciesAreReviewed(t *testing.T) {
	root := repoRoot(t)
	mod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	doc, err := os.ReadFile(filepath.Join(root, "docs", "dependencies.md"))
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(string(mod), "\n") {
		fields := strings.Fields(strings.TrimSuffix(strings.TrimSpace(line), "// indirect"))
		if len(fields) != 2 || !strings.Contains(fields[0], ".") || strings.HasPrefix(fields[0], "//") {
			continue
		}
		n++
		if want := "| `" + fields[0] + "` | " + fields[1] + " |"; !strings.Contains(string(doc), want) {
			t.Errorf("docs/dependencies.md has no reviewed row %q", want)
		}
	}
	if n < 10 {
		t.Errorf("found only %d modules in go.mod", n)
	}
}
