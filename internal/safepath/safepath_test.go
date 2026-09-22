package safepath

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	valid := []string{"survival", "survival-2", "survival_2", "A1", "a"}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"", "has space", "has/slash", "has\\backslash", "../escape",
		"--noinput", "start", "STOP", "All", "config", "help",
	}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", name)
		}
	}
}

func TestContainAllowsNestedExistingAndMissingPaths(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "worldstorage"), 0700); err != nil {
		t.Fatal(err)
	}

	got, err := Contain(root, "worldstorage")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(filepath.Join(root, "worldstorage"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("Contain = %q, want %q", got, want)
	}

	// A not-yet-created leaf under an existing root is allowed.
	got, err = Contain(root, "worldstorage/new-world")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(got, want) {
		t.Fatalf("Contain(new leaf) = %q, want prefix %q", got, want)
	}
}

func TestContainRejectsAbsoluteAndTraversal(t *testing.T) {
	root := t.TempDir()
	cases := []string{"/etc/passwd", "../escape", "worldstorage/../../escape", ".."}
	for _, rel := range cases {
		if _, err := Contain(root, rel); err == nil {
			t.Errorf("Contain(root, %q) = nil error, want a rejection", rel)
		}
	}
}

func TestContainRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}

	if _, err := Contain(root, "escape/secret"); err == nil {
		t.Fatal("expected Contain to reject a symlink pointing outside root")
	}
}

func TestContainAllowsSymlinkStayingInsideRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real-world"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(root, "real-world"), filepath.Join(root, "world")); err != nil {
		t.Fatal(err)
	}

	got, err := Contain(root, "world")
	if err != nil {
		t.Fatal(err)
	}
	wantTarget, err := filepath.EvalSymlinks(filepath.Join(root, "real-world"))
	if err != nil {
		t.Fatal(err)
	}
	if got != wantTarget {
		t.Fatalf("Contain(world) = %q, want %q", got, wantTarget)
	}
}

// TestContainAllowsSymlinkStayingInsideRootViaAnUnresolvedRoot reproduces a
// real bug found on macOS CI: t.TempDir() there lives under /var, itself a
// symlink to /private/var, so the caller's "root" argument is already
// unresolved before Contain ever sees it. A symlink written using that same
// unresolved root (exactly how a caller would naturally construct one, as
// the test above does with filepath.Join(root, "real-world")) must not be
// flagged as escaping once root's own ancestry is canonicalized.
func TestContainAllowsSymlinkStayingInsideRootViaAnUnresolvedRoot(t *testing.T) {
	outer := t.TempDir()
	real := filepath.Join(outer, "real-root")
	if err := os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	unresolvedRoot := filepath.Join(outer, "alias-root")
	if err := os.Symlink(real, unresolvedRoot); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(unresolvedRoot, "real-world"), 0700); err != nil {
		t.Fatal(err)
	}
	// Built from unresolvedRoot, exactly as a caller who was handed that
	// root (not knowing or caring that it is itself a symlink) would.
	if err := os.Symlink(filepath.Join(unresolvedRoot, "real-world"), filepath.Join(unresolvedRoot, "world")); err != nil {
		t.Fatal(err)
	}

	got, err := Contain(unresolvedRoot, "world")
	if err != nil {
		t.Fatal(err)
	}
	wantTarget, err := filepath.EvalSymlinks(filepath.Join(real, "real-world"))
	if err != nil {
		t.Fatal(err)
	}
	if got != wantTarget {
		t.Fatalf("Contain(world) = %q, want %q", got, wantTarget)
	}
}

func TestContainDetectsSymlinkCycle(t *testing.T) {
	root := t.TempDir()
	a := filepath.Join(root, "a")
	b := filepath.Join(root, "b")
	if err := os.Symlink(b, a); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Fatal(err)
	}

	if _, err := Contain(root, "a"); err == nil {
		t.Fatal("expected an error for a symlink cycle")
	}
}

func TestValidateConfiguredRoot(t *testing.T) {
	valid := []string{"/opt/msm/servers", "/opt/msm/jars"}
	for _, path := range valid {
		if err := ValidateConfiguredRoot(path); err != nil {
			t.Errorf("ValidateConfiguredRoot(%q) = %v, want nil", path, err)
		}
	}
	invalid := []string{"", "relative/path", "/opt/msm/../etc", "/"}
	for _, path := range invalid {
		if err := ValidateConfiguredRoot(path); err == nil {
			t.Errorf("ValidateConfiguredRoot(%q) = nil, want an error", path)
		}
	}
}

func TestCaseInsensitiveCollision(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "Survival"), 0700); err != nil {
		t.Fatal(err)
	}

	existing, found, err := CaseInsensitiveCollision(dir, "survival")
	if err != nil {
		t.Fatal(err)
	}
	if !found || existing != "Survival" {
		t.Fatalf("got (%q, %v), want (\"Survival\", true)", existing, found)
	}

	_, found, err = CaseInsensitiveCollision(dir, "Survival")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("an exact name match must not be reported as a collision")
	}

	_, found, err = CaseInsensitiveCollision(dir, "creative")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("unrelated name must not collide")
	}

	_, found, err = CaseInsensitiveCollision(filepath.Join(dir, "missing"), "anything")
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("a missing directory must not report a collision")
	}
}
