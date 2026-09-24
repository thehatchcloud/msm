package serverprops

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGetQuotedAndUnquotedValues(t *testing.T) {
	doc := Parse([]byte("level-name=world\nmotd=\"Welcome!\"\nmsm-version=minecraft/1.7.0\n"))

	cases := map[string]string{
		"level-name":  "world",
		"motd":        "Welcome!",
		"msm-version": "minecraft/1.7.0",
	}
	for key, want := range cases {
		got, ok := doc.Get(key)
		if !ok || got != want {
			t.Errorf("Get(%q) = (%q, %v), want (%q, true)", key, got, ok, want)
		}
	}
	if _, ok := doc.Get("absent"); ok {
		t.Fatal("Get of an absent key must report ok=false")
	}
}

func TestGetIsCaseInsensitiveAndLastAssignmentWins(t *testing.T) {
	doc := Parse([]byte("MSM-Version=minecraft/1.2.0\nmsm-version=minecraft/1.7.0\n"))
	got, ok := doc.Get("msm-version")
	if !ok || got != "minecraft/1.7.0" {
		t.Fatalf("Get = (%q, %v), want the last assignment regardless of case", got, ok)
	}
}

func TestSetUpdatesExistingLineInPlace(t *testing.T) {
	doc := Parse([]byte("# a comment\nlevel-name=world\nmotd=hi\n"))
	if err := doc.Set("level-name", "creative"); err != nil {
		t.Fatal(err)
	}

	got := string(doc.Bytes())
	want := "# a comment\nlevel-name=creative\nmotd=hi\n"
	if got != want {
		t.Fatalf("Bytes() =\n%q\nwant\n%q", got, want)
	}
}

func TestSetAppendsNewKey(t *testing.T) {
	doc := Parse([]byte("level-name=world\n"))
	if err := doc.Set("msm-ram", "2048"); err != nil {
		t.Fatal(err)
	}

	got := string(doc.Bytes())
	want := "level-name=world\nmsm-ram=2048\n"
	if got != want {
		t.Fatalf("Bytes() =\n%q\nwant\n%q", got, want)
	}
}

func TestSetNeverQuotesTheWrittenValue(t *testing.T) {
	doc := Parse(nil)
	if err := doc.Set("msm-message-stop", "Fixture stopping in {DELAY} seconds"); err != nil {
		t.Fatal(err)
	}
	got := string(doc.Bytes())
	want := "msm-message-stop=Fixture stopping in {DELAY} seconds\n"
	if got != want {
		t.Fatalf("Bytes() = %q, want %q", got, want)
	}
	// A later Get must still read back the same literal, unquoted value.
	value, ok := doc.Get("msm-message-stop")
	if !ok || value != "Fixture stopping in {DELAY} seconds" {
		t.Fatalf("Get after Set = (%q, %v)", value, ok)
	}
}

func TestUnknownPropertiesAndCommentsSurviveARoundTrip(t *testing.T) {
	original := "# header comment\n! bang comment\n\nlevel-name=world\ngenerator-settings={\"layers\":[]}\nmsm-ram=1024\n"
	doc := Parse([]byte(original))
	if err := doc.Set("msm-ram", "2048"); err != nil {
		t.Fatal(err)
	}

	got := string(doc.Bytes())
	want := "# header comment\n! bang comment\n\nlevel-name=world\ngenerator-settings={\"layers\":[]}\nmsm-ram=2048\n"
	if got != want {
		t.Fatalf("Bytes() =\n%q\nwant\n%q", got, want)
	}
}

func TestOverridesExtractsMsmPrefixedKeys(t *testing.T) {
	doc := Parse([]byte(`level-name=world
motd=Compatibility fixture
msm-version=minecraft/1.7.0
msm-ram=512
msm-stop-delay=3
msm-message-stop="Fixture stopping in {DELAY} seconds"
`))
	got := doc.Overrides()
	want := map[string]string{
		"version":      "minecraft/1.7.0",
		"ram":          "512",
		"stop-delay":   "3",
		"message-stop": "Fixture stopping in {DELAY} seconds",
	}
	if len(got) != len(want) {
		t.Fatalf("Overrides() = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Overrides()[%q] = %q, want %q", k, got[k], v)
		}
	}
}

func TestSetOverrideRoundTrips(t *testing.T) {
	doc := Parse([]byte("level-name=world\n"))
	if err := doc.SetOverride("Stop-Delay", "5"); err != nil {
		t.Fatal(err)
	}
	got, ok := doc.Get("msm-stop-delay")
	if !ok || got != "5" {
		t.Fatalf("Get(msm-stop-delay) = (%q, %v), want (\"5\", true)", got, ok)
	}
	if _, found := doc.Overrides()["stop-delay"]; !found {
		t.Fatal("SetOverride must be visible through Overrides")
	}
}

func TestLoadRealFixtures(t *testing.T) {
	for _, tc := range []struct {
		path      string
		overrides map[string]string
	}{
		{
			path: "../../compatibility/fixtures/minimal/servers/active-example/server.properties",
			overrides: map[string]string{
				"version":      "minecraft/1.7.0",
				"ram":          "512",
				"stop-delay":   "3",
				"message-stop": "Fixture stopping in {DELAY} seconds",
			},
		},
	} {
		t.Run(tc.path, func(t *testing.T) {
			doc, err := Load(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			levelName, ok := doc.Get("level-name")
			if !ok || levelName != "world" {
				t.Fatalf("Get(level-name) = (%q, %v), want (world, true)", levelName, ok)
			}
			overrides := doc.Overrides()
			for k, want := range tc.overrides {
				if got := overrides[k]; got != want {
					t.Errorf("Overrides()[%q] = %q, want %q", k, got, want)
				}
			}
		})
	}
}

func TestSaveIsAtomicAndPreservesPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "server.properties")
	if err := os.WriteFile(path, []byte("level-name=world\n"), 0640); err != nil {
		t.Fatal(err)
	}

	doc, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Set("level-name", "creative"); err != nil {
		t.Fatal(err)
	}
	if err := doc.Save(path, 0600); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0640 {
		t.Fatalf("mode = %v, want preserved 0640", info.Mode().Perm())
	}

	reloaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := reloaded.Get("level-name")
	if got != "creative" {
		t.Fatalf("Get(level-name) after Save = %q, want creative", got)
	}
}

func TestSetRefusesLineInjection(t *testing.T) {
	doc := Parse([]byte("level-name=world\n"))
	for _, tc := range []struct{ key, value string }{
		{"motd", "hi\nop-permission-level=4"},
		{"motd", "carriage\rreturn"},
		{"motd", "nul\x00byte"},
		{"motd\nextra", "x"},
		{"", "x"},
		{" motd", "x"},
		{"#motd", "x"},
		{"!motd", "x"},
		{"a=b", "x"},
	} {
		if err := doc.Set(tc.key, tc.value); !errors.Is(err, ErrUnsafeProperty) {
			t.Errorf("Set(%q, %q) = %v, want ErrUnsafeProperty", tc.key, tc.value, err)
		}
	}
	if err := doc.SetOverride("message-stop", "two\nlines"); !errors.Is(err, ErrUnsafeProperty) {
		t.Errorf("SetOverride with a newline = %v", err)
	}
	if got := string(doc.Bytes()); got != "level-name=world\n" {
		t.Fatalf("refused writes changed the document: %q", got)
	}
}

// server.properties is read and written literally, as the legacy
// manager's sed reader and writer do, not with Java Properties escaping
// (see docs/compatibility/README.md). Backslashes, unicode escapes and
// trailing continuations are data, and only an exact "key=" at the start
// of a line is an assignment.
func TestPropertiesAreLiteralNotJavaEscaped(t *testing.T) {
	doc := Parse([]byte(`motd=A \u00A7 sign\\and\:colon
level-name=C:\worlds\main
continued=first\
second-line=kept
  indented=ignored
spaced = ignored
colon:ignored
`))
	for key, want := range map[string]string{
		"motd":        `A \u00A7 sign\\and\:colon`,
		"level-name":  `C:\worlds\main`,
		"continued":   `first\`,
		"second-line": "kept",
	} {
		if got, ok := doc.Get(key); !ok || got != want {
			t.Errorf("Get(%q) = (%q, %v), want %q", key, got, ok, want)
		}
	}
	for _, key := range []string{"indented", "spaced", "colon"} {
		if got, ok := doc.Get(key); ok {
			t.Errorf("Get(%q) = %q; a Java-only assignment form must not match", key, got)
		}
	}
	if err := doc.Set("motd", `C:\new\u0041`); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(doc.Bytes()), "motd=C:\\new\\u0041\n") {
		t.Fatalf("value was not written literally: %q", doc.Bytes())
	}
}
