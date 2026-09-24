package legacyconf

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "msm.conf")
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadParsesLiteralAssignments(t *testing.T) {
	path := write(t, `
# a comment
USERNAME="minecraft"
SERVER_STORAGE_PATH='/opt/msm/servers'
UNQUOTED=plain-value
DEFAULT_INVOCATION="java -Xms{RAM}M -Xmx{RAM}M -jar {JAR} nogui"

# duplicate assignment: last one wins, like eval processing each line
USERNAME="admin"
`)
	f, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"USERNAME":            "admin",
		"SERVER_STORAGE_PATH": "/opt/msm/servers",
		"UNQUOTED":            "plain-value",
		"DEFAULT_INVOCATION":  "java -Xms{RAM}M -Xmx{RAM}M -jar {JAR} nogui",
	}
	for name, want := range cases {
		got, ok := f.Get(name)
		if !ok || got != want {
			t.Errorf("Get(%s) = (%q, %v), want (%q, true)", name, got, ok, want)
		}
	}
	if _, ok := f.Get("MISSING"); ok {
		t.Fatal("Get of an absent key must report ok=false")
	}
}

func TestLoadRejectsShellSyntax(t *testing.T) {
	cases := map[string]string{
		"backtick substitution": "USERNAME=`whoami`",
		"$() substitution":      "USERNAME=$(whoami)",
		"variable expansion":    "USERNAME=$HOME",
		"command chaining ;":    "USERNAME=minecraft; rm -rf /",
		"command chaining &&":   "USERNAME=minecraft && rm -rf /",
		"pipe":                  "USERNAME=minecraft | tee /etc/passwd",
		"redirection":           "USERNAME=minecraft > /etc/passwd",
		"trailing continuation": "USERNAME=minecraft\\",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			path := write(t, line+"\n")
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load(%q) = nil error, want a rejection", line)
			}
			var syntaxErr *SyntaxError
			if !errors.As(err, &syntaxErr) {
				t.Fatalf("error %v does not wrap *SyntaxError", err)
			}
			if syntaxErr.Line != 1 || syntaxErr.Path != path {
				t.Fatalf("SyntaxError at %s:%d, want %s:1", syntaxErr.Path, syntaxErr.Line, path)
			}
			// The message an administrator sees names the file and line and
			// says how to migrate.
			if msg := err.Error(); !strings.Contains(msg, path+":1:") || !strings.Contains(msg, migrationAdvice) {
				t.Fatalf("error %q lacks the file:line location or migration advice", msg)
			}
		})
	}
}

func TestLoadRejectsNonAssignmentLine(t *testing.T) {
	path := write(t, "USERNAME=minecraft\nnot an assignment\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected an error for a non-assignment line")
	}
	var syntaxErr *SyntaxError
	if !errors.As(err, &syntaxErr) || syntaxErr.Line != 2 {
		t.Fatalf("error = %v, want a *SyntaxError on line 2", err)
	}
}

func TestLoadAcceptsRealConfigurationFixtures(t *testing.T) {
	for _, path := range []string{
		"../../msm.conf",
		"../../compatibility/fixtures/minimal/msm.conf",
	} {
		t.Run(path, func(t *testing.T) {
			f, err := Load(path)
			if err != nil {
				t.Fatalf("Load(%s) = %v, want the shipped configuration to parse as literal data", path, err)
			}
			if len(f.Values) == 0 {
				t.Fatalf("Load(%s) parsed no assignments", path)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "absent.conf")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

func TestDiscoverPrecedence(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "explicit.conf")
	env := filepath.Join(dir, "env.conf")
	system := filepath.Join(dir, "system.conf")
	for _, p := range []string{explicit, env, system} {
		if err := os.WriteFile(p, []byte("USERNAME=x\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	if got, ok := Discover(explicit, env, system); !ok || got != explicit {
		t.Fatalf("Discover with all present = (%q, %v), want explicit path", got, ok)
	}
	if got, ok := Discover("", env, system); !ok || got != env {
		t.Fatalf("Discover without explicit = (%q, %v), want env path", got, ok)
	}
	if got, ok := Discover("", "", system); !ok || got != system {
		t.Fatalf("Discover with only system = (%q, %v), want system path", got, ok)
	}
	if got, ok := Discover("", "", filepath.Join(dir, "absent.conf")); ok {
		t.Fatalf("Discover with no existing candidate = (%q, true), want ok=false", got)
	}
	if got, ok := Discover(filepath.Join(dir, "absent.conf"), env, system); !ok || got != env {
		t.Fatalf("Discover must fall through a non-existent explicit path, got (%q, %v)", got, ok)
	}
}

func TestGlobalDefaults(t *testing.T) {
	f, err := Load(write(t, "# empty\n"))
	if err != nil {
		t.Fatal(err)
	}
	settings := Global(f)
	if settings.Username != DefaultUsername ||
		settings.ServerStoragePath != DefaultServerStoragePath ||
		settings.JarStoragePath != DefaultJarStoragePath ||
		settings.ServerPropertiesFile != DefaultServerPropertiesFile {
		t.Fatalf("Global(empty file) = %+v, want documented defaults", settings)
	}
	if len(settings.Warnings) != 0 {
		t.Fatalf("Global(empty file) warnings = %v, want none", settings.Warnings)
	}
}

func TestGlobalPropertiesFilenameSpelling(t *testing.T) {
	load := func(contents string) GlobalSettings {
		t.Helper()
		f, err := Load(write(t, contents))
		if err != nil {
			t.Fatal(err)
		}
		return Global(f)
	}

	t.Run("only registered spelling", func(t *testing.T) {
		s := load(`SERVER_PROPERTIES="server.properties"` + "\n")
		if s.ServerPropertiesFile != "server.properties" || len(s.Warnings) != 0 {
			t.Fatalf("got %+v", s)
		}
	})

	t.Run("only legacy sample spelling", func(t *testing.T) {
		s := load(`DEFAULT_PROPERTIES_PATH="server.properties"` + "\n")
		if s.ServerPropertiesFile != "server.properties" || len(s.Warnings) != 1 {
			t.Fatalf("got %+v, want one warning importing the legacy spelling", s)
		}
	})

	t.Run("both set and agreeing", func(t *testing.T) {
		s := load("SERVER_PROPERTIES=\"custom.properties\"\nDEFAULT_PROPERTIES_PATH=\"custom.properties\"\n")
		if s.ServerPropertiesFile != "custom.properties" || len(s.Warnings) != 0 {
			t.Fatalf("got %+v, want no warning when both spellings agree", s)
		}
	})

	t.Run("both set and disagreeing", func(t *testing.T) {
		s := load("SERVER_PROPERTIES=\"registered.properties\"\nDEFAULT_PROPERTIES_PATH=\"other.properties\"\n")
		if s.ServerPropertiesFile != "registered.properties" || len(s.Warnings) != 1 {
			t.Fatalf("got %+v, want the registered spelling and one conflict warning", s)
		}
	})
}

// Global reads msm.conf the way init/msm's manager_property does: keys in
// any case, the last matching line wins, and an empty value keeps the
// default.
func TestGlobalFollowsLegacyLookupRules(t *testing.T) {
	f, err := Load(write(t, `USERNAME="admin"
USERNAME=""
server_storage_path=/srv/servers
JAR_STORAGE_PATH="/srv/jars"
Jar_Storage_Path='/srv/jars-later'
`))
	if err != nil {
		t.Fatal(err)
	}
	s := Global(f)
	if s.Username != DefaultUsername || s.ServerStoragePath != "/srv/servers" || s.JarStoragePath != "/srv/jars-later" {
		t.Fatalf("Global = %+v", s)
	}
	if v, ok := f.Lookup("username"); ok || v != "" {
		t.Fatalf("Lookup of an emptied setting = (%q, %v), want unset", v, ok)
	}
	if v, ok := f.Get("server_storage_path"); !ok || v != "/srv/servers" {
		t.Fatalf("Get must still match exactly: (%q, %v)", v, ok)
	}
}
