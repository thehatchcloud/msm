package profiles

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// legacyProfile reads the values this package encodes from a shipped
// versioning/*.sh file, following extends, without executing it.
func legacyProfile(t *testing.T, name string) Profile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("../../versioning", name+".sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(data), "\\\n", " ")
	p := Profile{Name: name}
	quoted := regexp.MustCompile(`"([^"]*)"`)
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		args := quoted.FindAllStringSubmatch(line, -1)
		switch {
		case fields[0] == "extends":
			base := legacyProfile(t, args[0][1])
			base.Name = name
			p = base
		case fields[0] == "set_property" && fields[1] == "LOG_PATH":
			p.LogPath = args[0][1]
		case fields[0] == "console_event" && args != nil:
			event, timeout, _ := strings.Cut(fields[1], ":")
			switch event {
			case "REGEX":
				p.LinePrefix = args[0][1]
			case "START":
				p.Start = Event{Pattern: args[0][1], Timeout: seconds(t, timeout)}
			}
		case fields[0] == "console_command" && strings.HasPrefix(fields[1], "SAVE_ALL"):
			_, timeout, _ := strings.Cut(fields[1], ":")
			p.SaveAll = Command{Line: args[0][1], Confirm: args[1][1], Timeout: seconds(t, timeout)}
		}
	}
	return p
}

func seconds(t *testing.T, s string) time.Duration {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}
	return time.Duration(n) * time.Second
}

// CT-SURF-021 to CT-SURF-025 (the parts P06 uses): the Go data equals the
// shipped profiles, inheritance included.
func TestProfilesMatchVersioningFiles(t *testing.T) {
	files, err := filepath.Glob("../../versioning/*/*.sh")
	if err != nil || len(files) != len(All) {
		t.Fatalf("versioning files %v (%v); want %d", files, err, len(All))
	}
	for _, p := range All {
		if want := legacyProfile(t, p.Name); p != want {
			t.Errorf("%s:\n got  %+v\n want %+v", p.Name, p, want)
		}
	}
}

func TestSelect(t *testing.T) {
	for _, tc := range []struct{ version, want string }{
		{"minecraft/1.2.5", "minecraft/1.2.0"},
		{"minecraft/1.3", "minecraft/1.3.0"},
		{"minecraft/1.6.4", "minecraft/1.3.0"},
		{"minecraft/1.7.0", "minecraft/1.7.0"},
		{"minecraft/1.21.4", "minecraft/1.7.0"},
		{"minecraft/2.0.0", "minecraft/1.7.0"},
		{"craftbukkit/1.2.5", "craftbukkit/1.2.0"},
		{"craftbukkit/1.20.1", "craftbukkit/1.3.0"},
	} {
		if p, note := Select(tc.version); p.Name != tc.want || note != "" {
			t.Errorf("Select(%q) = %s, %q; want %s", tc.version, p.Name, note, tc.want)
		}
	}
	for _, v := range []string{"unknown", "", "minecraft/1.1.0", "paper/1.20.1", "minecraft/x", "minecraft/1.2.3.4"} {
		if p, note := Select(v); p.Name != Newest.Name || note == "" {
			t.Errorf("Select(%q) = %s, %q; want fallback with a note", v, p.Name, note)
		}
	}
}

// Readiness matches only a real start line, with the legacy prefix.
func TestMatch(t *testing.T) {
	re, err := Newest.Match(Newest.Start.Pattern)
	if err != nil {
		t.Fatal(err)
	}
	for line, want := range map[string]bool{
		`[12:00:01] [Server thread/INFO]: Done (3.2s)! For help, type "help"`: true,
		`[12:00:01] [Server thread/INFO]: Preparing spawn area: 40%`:          false,
		`<player> Done`: false,
		`Done`:          false,
	} {
		if re.MatchString(line) != want {
			t.Errorf("%q: match = %v", line, !want)
		}
	}
	re, _ = All[0].Match(All[0].Start.Pattern)
	if !re.MatchString("2012-03-01 12:00:00 [INFO] Done (1.2s)! For help") {
		t.Error("minecraft/1.2.0 start line not matched")
	}
}
