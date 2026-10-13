package profiles

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// legacyProfile reads the values this package encodes from a shipped
// versioning/*.sh file, following extends, without executing it. Commands
// are returned with Successes unset: which outputs confirm a command is
// the Go data's own classification.
func legacyProfile(t *testing.T, name string) Profile {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("../../versioning", name+".sh"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(data), "\\\n", " ")
	p := Profile{Name: name}
	quoted := regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
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
		case fields[0] == "console_command" && args != nil:
			cname, timeout, _ := strings.Cut(fields[1], ":")
			c := Command{Line: args[0][1], Timeout: defaultLegacyTimeout}
			if timeout != "" {
				c.Timeout = seconds(t, timeout)
			}
			for _, a := range args[1:] {
				c.Outputs = append(c.Outputs, a[1])
			}
			if p.Commands == nil {
				p.Commands = map[string]Command{}
			}
			p.Commands[cname] = c
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

// escapeParens is DEV-022: the 1.3.0 patterns spell literal parentheses,
// as in "(ID <item>)" and "entered (<mode>)", where a regular expression
// reads a group that cannot match what the server prints. The Go data
// escapes them; profiles inheriting the 1.3.0 commands carry the same fix.
func escapeParens(p *Profile) {
	fix := strings.NewReplacer("(ID <item>)", `\(ID <item>\)`, "(<mode>)", `\(<mode>\)`,
		"(<time>)", `\(<time>\)`, "(<amount>)", `\(<amount>\)`)
	for name, c := range p.Commands {
		if len(c.Outputs) == 0 {
			continue
		}
		outputs := make([]string, len(c.Outputs))
		for i, o := range c.Outputs {
			outputs[i] = fix.Replace(o)
		}
		c.Outputs = outputs
		p.Commands[name] = c
	}
}

// CT-SURF-021 to CT-SURF-025: the Go data equals the shipped profiles,
// inheritance and every console command included.
func TestProfilesMatchVersioningFiles(t *testing.T) {
	files, err := filepath.Glob("../../versioning/*/*.sh")
	if err != nil || len(files) != len(All) {
		t.Fatalf("versioning files %v (%v); want %d", files, err, len(All))
	}
	for _, p := range All {
		want := legacyProfile(t, p.Name)
		// Go's own classification, labels and answers that replace a
		// legacy one are compared separately.
		got := p
		got.Modes, got.Connected = nil, ConnectedFormat{}
		got.Commands = map[string]Command{}
		for k, c := range p.Commands {
			c.Successes, c.Optional = 0, nil
			got.Commands[k] = c
		}
		if !strings.HasSuffix(p.Name, "/1.2.0") {
			escapeParens(&want)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got  %+v\n want %+v", p.Name, got, want)
		}
	}
}

// Each profile's classification is plausible: no command confirms more
// outputs than it has, and a refusal needs an output to be a refusal.
func TestCommandClassification(t *testing.T) {
	for _, p := range Builtin {
		for name, c := range p.Commands {
			if c.Successes < 0 || c.Successes > len(c.Outputs) {
				t.Errorf("%s %s: %d successes of %d outputs", p.Name, name, c.Successes, len(c.Outputs))
			}
			if c.Line == "" && c.Unsupported == "" {
				t.Errorf("%s %s: no line", p.Name, name)
			}
		}
		for _, name := range []string{WhitelistOn, WhitelistOff, WhitelistAdd, WhitelistRemove, BlacklistPlayerAdd,
			BlacklistIPAdd, OpAdd, OpRemove, Gamemode, Kick, Say, ToggleDownfall, Give, XP, Connected, TimeSet, TimeAdd,
			SaveOn, SaveOff, SaveAll} {
			if _, ok := p.Commands[name]; !ok {
				t.Errorf("%s lacks %s", p.Name, name)
			}
		}
	}
}

func TestSelect(t *testing.T) {
	for _, tc := range []struct{ version, want string }{
		{"minecraft/1.2.5", "minecraft/1.2.0"},
		{"minecraft/1.3", "minecraft/1.3.0"},
		{"minecraft/1.6.4", "minecraft/1.3.0"},
		{"minecraft/1.7.0", "minecraft/1.7.0"},
		{"minecraft/1.12.2", "minecraft/1.7.0"},
		{"minecraft/1.13", "minecraft/1.13.0"},
		{"minecraft/1.21.4", "minecraft/1.13.0"},
		{"minecraft/2.0.0", "minecraft/1.13.0"},
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

// outcome classifies a log line as an answer to a prepared command.
func outcome(p Prepared, line string) string {
	switch {
	case p.Failure != nil && p.Failure.MatchString(line):
		return "refused"
	case p.Success != nil && p.Success.MatchString(line):
		return "done"
	}
	return "none"
}

// Recorded log lines, by profile, answer the console commands as the
// profile says. The legacy lines come from the shipped patterns; the
// modern lines from the vanilla 1.21 language file.
func TestAnswers(t *testing.T) {
	const (
		v12 = "2012-03-01 12:00:00 [INFO] "
		v13 = "2012-09-01 12:00:00 [INFO] "
		v17 = "[12:00:00] [Server thread/INFO]: "
	)
	for _, tc := range []struct {
		profile string
		command string
		args    map[string]string
		line    string
		want    string
	}{
		{"minecraft/1.2.0", Kick, map[string]string{"player": "Steve"}, v12 + "CONSOLE: Kicking Steve", "done"},
		{"minecraft/1.2.0", Kick, map[string]string{"player": "Steve"}, v12 + "Can't find user Steve", "refused"},
		{"minecraft/1.2.0", Kick, map[string]string{"player": "Steve"}, v12 + "Can't find user Alex", "none"},
		{"minecraft/1.2.0", SaveAll, nil, v12 + "CONSOLE: Save complete.", "done"},
		{"minecraft/1.2.0", Gamemode, map[string]string{"player": "Steve", "mode": "creative"}, v12 + "CONSOLE: Setting Steve to game mode 1", "done"},
		{"minecraft/1.2.0", Gamemode, map[string]string{"player": "Steve", "mode": "1"}, v12 + "Steve already has game mode 1", "refused"},
		{"minecraft/1.2.0", XP, map[string]string{"player": "Steve", "amount": "5"}, v12 + "CONSOLE: Giving 5 exp to Steve", "done"},
		{"minecraft/1.2.0", TimeSet, map[string]string{"time": "0"}, v12 + "Unable to convert time value", "refused"},
		{"minecraft/1.3.0", Kick, map[string]string{"player": "Steve"}, v13 + "Kicked Steve from the game", "done"},
		{"minecraft/1.3.0", Gamemode, map[string]string{"player": "Steve", "mode": "2"}, v13 + "That player cannot be found", "refused"},
		{"minecraft/1.3.0", Gamemode, map[string]string{"player": "Steve", "mode": "9"}, v13 + "The number you have entered (9) is too big, it must be at most 2", "refused"},
		{"minecraft/1.3.0", TimeAdd, map[string]string{"time": "100"}, v13 + "Added 100 to the time", "done"},
		{"minecraft/1.3.0", Give, map[string]string{"player": "Steve", "item": "1", "amount": "64"}, v13 + "Given Stone (ID 1) * 64 to Steve", "done"},
		{"minecraft/1.3.0", Give, map[string]string{"player": "Steve", "item": "1"}, v13 + "Given Stone (ID 1) * 1 to Steve", "done"},
		{"minecraft/1.3.0", XP, map[string]string{"player": "Steve", "amount": "5"}, v13 + "Given 5 experience to Steve", "done"},
		{"minecraft/1.3.0", ToggleDownfall, nil, v13 + "Toggled downfall", "done"},
		{"minecraft/1.3.0", SaveOff, nil, v13 + "Turned off world auto-saving", "done"},
		{"minecraft/1.7.0", SaveAll, nil, v17 + "Saved the world", "done"},
		{"minecraft/1.7.0", SaveAll, nil, "[12:00:00] [Server thread/INFO] Saved the world", "none"},
		{"craftbukkit/1.3.0", SaveAll, nil, v13 + "Save complete.", "done"},
		{"craftbukkit/1.3.0", SaveOn, nil, v13 + "Enabled level saving..", "done"},
		{"craftbukkit/1.2.0", SaveOff, nil, v12 + "CONSOLE: Disabling level saving..", "done"},
		{"minecraft/1.13.0", WhitelistAdd, map[string]string{"player": "Steve"}, v17 + "Added Steve to the whitelist", "done"},
		{"minecraft/1.13.0", WhitelistAdd, map[string]string{"player": "Steve"}, v17 + "Player is already whitelisted", "refused"},
		{"minecraft/1.13.0", WhitelistRemove, map[string]string{"player": "Steve"}, v17 + "Removed Steve from the whitelist", "done"},
		{"minecraft/1.13.0", BlacklistPlayerAdd, map[string]string{"player": "Steve"}, v17 + "Banned Steve: Banned by an operator.", "done"},
		{"minecraft/1.13.0", BlacklistPlayerRem, map[string]string{"player": "Steve"}, v17 + "Unbanned Steve", "done"},
		{"minecraft/1.13.0", BlacklistIPAdd, map[string]string{"address": "10.0.0.1"}, v17 + "Banned IP 10.0.0.1: Banned by an operator.", "done"},
		{"minecraft/1.13.0", BlacklistIPAdd, map[string]string{"address": "10.0.0.1"}, v17 + "Banned IP 10x0y0z1: ", "none"},
		{"minecraft/1.13.0", OpAdd, map[string]string{"player": "Steve"}, v17 + "Made Steve a server operator", "done"},
		{"minecraft/1.13.0", OpRemove, map[string]string{"player": "Steve"}, v17 + "Made Steve no longer a server operator", "done"},
		{"minecraft/1.13.0", OpRemove, map[string]string{"player": "Steve"}, v17 + "Nothing changed. The player is not an operator", "refused"},
		{"minecraft/1.13.0", Gamemode, map[string]string{"player": "Steve", "mode": "creative"}, v17 + "Set Steve's game mode to Creative Mode", "done"},
		{"minecraft/1.13.0", Kick, map[string]string{"player": "Steve"}, v17 + "Kicked Steve: Kicked by an operator", "done"},
		{"minecraft/1.13.0", Kick, map[string]string{"player": "Steve"}, v17 + "No player was found", "refused"},
		{"minecraft/1.13.0", Give, map[string]string{"player": "Steve", "item": "stone", "amount": "2"}, v17 + "Gave 2 [Stone] to Steve", "done"},
		{"minecraft/1.13.0", XP, map[string]string{"player": "Steve", "amount": "-5"}, v17 + "Gave -5 experience points to Steve", "done"},
		{"minecraft/1.13.0", TimeSet, map[string]string{"time": "day"}, v17 + "Set the time to 1000", "done"},
		{"minecraft/1.13.0", TimeAdd, map[string]string{"time": "x"}, v17 + "Incorrect argument for command", "refused"},
		{"minecraft/1.13.0", SaveOn, nil, v17 + "Turned on world auto-saving", "done"},
		{"minecraft/1.13.0", SaveOff, nil, v17 + "Nothing changed. Automatic saving is already off", "refused"},
		{"minecraft/1.13.0", SaveAll, nil, v17 + "Saving the game (this may take a moment!)", "none"},
		{"minecraft/1.13.0", SaveAll, nil, v17 + "Saved the game", "done"},
		{"minecraft/1.13.0", SaveAll, nil, "[12:00:00 INFO]: Saved the game", "done"}, // Paper
	} {
		p, _ := Select(tc.profile)
		if p.Name != tc.profile {
			t.Fatalf("no profile %s", tc.profile)
		}
		prep, err := p.Prepare(tc.command, tc.args)
		if err != nil {
			t.Errorf("%s %s: %v", tc.profile, tc.command, err)
			continue
		}
		if got := outcome(prep, tc.line); got != tc.want {
			t.Errorf("%s %s %v: %q = %s, want %s", tc.profile, tc.command, tc.args, tc.line, got, tc.want)
		}
	}
}

func TestPrepareBuildsTheLine(t *testing.T) {
	legacy, _ := Select("minecraft/1.2.0")
	v13, _ := Select("minecraft/1.3.0")
	for _, tc := range []struct {
		p       Profile
		command string
		args    map[string]string
		want    string
	}{
		{legacy, Gamemode, map[string]string{"player": "Steve", "mode": "survival"}, "gamemode Steve 0"},
		{v13, Gamemode, map[string]string{"player": "Steve", "mode": "creative"}, "gamemode 1 Steve"},
		{v13, Gamemode, map[string]string{"player": "Steve", "mode": "2"}, "gamemode 2 Steve"},
		{v13, Gamemode, map[string]string{"player": "Steve", "mode": "s"}, "gamemode s Steve"}, // the server judges it
		{v13, Give, map[string]string{"player": "Steve", "item": "1"}, "give Steve 1"},
		{v13, Give, map[string]string{"player": "Steve", "item": "1", "amount": "3", "damage": "2"}, "give Steve 1 3 2"},
		{v13, XP, map[string]string{"player": "Steve", "amount": "5"}, "xp 5 Steve"},
		{legacy, XP, map[string]string{"player": "Steve", "amount": "5"}, "xp Steve 5"},
		{v13, Say, map[string]string{"message": "hello  big   world <player>"}, "say hello  big   world <player>"},
		{Modern, Gamemode, map[string]string{"player": "Steve", "mode": "1"}, "gamemode creative Steve"},
		{Modern, Gamemode, map[string]string{"player": "Steve", "mode": "Spectator"}, "gamemode spectator Steve"},
		{Modern, XP, map[string]string{"player": "Steve", "amount": "5"}, "experience add Steve 5 points"},
		{Modern, Give, map[string]string{"player": "Steve", "item": "stone"}, "give Steve stone"},
		{v13, SaveAll, nil, "save-all"},
	} {
		got, err := tc.p.Prepare(tc.command, tc.args)
		if err != nil || got.Line != tc.want {
			t.Errorf("%s %s %v = %q, %v; want %q", tc.p.Name, tc.command, tc.args, got.Line, err, tc.want)
		}
	}
}

func TestPrepareRefusesWhatItCannotSendSafely(t *testing.T) {
	v13, _ := Select("minecraft/1.3.0")
	for name, args := range map[string]map[string]string{
		"space":   {"player": "Steve deop"},
		"newline": {"player": "Steve\nstop"},
		"escape":  {"player": "St\x1bve"},
		"tab":     {"player": "a\tb"},
		"long":    {"player": strings.Repeat("a", 300)},
	} {
		if _, err := v13.Prepare(Kick, args); !errors.Is(err, ErrBadArgument) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := v13.Prepare(Kick, nil); err == nil {
		t.Error("a missing player was accepted")
	}
	if _, err := v13.Prepare(Kick, map[string]string{"player": "a", "reason": "b"}); !errors.Is(err, ErrUnsupported) {
		t.Errorf("an unused argument was dropped: %v", err)
	}
	if _, err := Modern.Prepare(ToggleDownfall, nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("toggledownfall on a modern server: %v", err)
	}
	if _, err := v13.Prepare("NOPE", nil); !errors.Is(err, ErrUnknownCommand) {
		t.Errorf("unknown: %v", err)
	}
}

// A player's name is matched literally in the answers, so a name made of
// regular expression syntax cannot make any line confirm its command.
func TestArgumentsAreMatchedLiterally(t *testing.T) {
	prep, err := Modern.Prepare(Kick, map[string]string{"player": ".*"})
	if err != nil {
		t.Fatal(err)
	}
	if outcome(prep, "[12:00:00] [Server thread/INFO]: Kicked Steve: Kicked by an operator") != "none" {
		t.Error("a regular expression player name matched another player's answer")
	}
	if outcome(prep, "[12:00:00] [Server thread/INFO]: Kicked .*: Kicked by an operator") != "done" {
		t.Error("the literal name did not match")
	}
}

// A chat line cannot pass for a server message, however it is built.
func TestMatchRejectsForgedLines(t *testing.T) {
	for _, p := range Builtin {
		re, err := p.Match(p.SaveAllCommand().Confirm())
		if err != nil {
			t.Fatal(err)
		}
		real := map[string]string{
			"minecraft/1.2.0": "2012-03-01 12:00:00 [INFO] CONSOLE: Save complete.", "craftbukkit/1.2.0": "2012-03-01 12:00:00 [INFO] CONSOLE: Save complete.",
			"minecraft/1.3.0": "2012-03-01 12:00:00 [INFO] Saved the world", "craftbukkit/1.3.0": "2012-03-01 12:00:00 [INFO] Save complete.",
			"minecraft/1.7.0": "[12:00:00] [Server thread/INFO]: Saved the world", "minecraft/1.13.0": "[12:00:00] [Server thread/INFO]: Saved the game",
		}[p.Name]
		if !re.MatchString(real) {
			t.Errorf("%s: real line %q not matched", p.Name, real)
		}
		prefix, msg, _ := strings.Cut(real, "] ")
		_ = prefix
		for _, forged := range []string{
			strings.Replace(real, "] ", "] <mallory> x] ", 1),
			strings.Replace(real, "]: ", "]: <mallory> x]: ", 1),
			"<mallory> " + real,
			"x " + real,
		} {
			if re.MatchString(forged) && forged != real {
				t.Errorf("%s: forged line %q accepted (msg %q)", p.Name, forged, msg)
			}
		}
	}
}

func TestBody(t *testing.T) {
	re, err := Modern.Body()
	if err != nil {
		t.Fatal(err)
	}
	for line, want := range map[string]string{
		"[12:00:00] [Server thread/INFO]: hello there": "hello there",
		"[12:00:00 INFO]: hello":                       "hello",
		"[12:00:00] [Server thread/INFO]:":             "",
	} {
		if m := re.FindStringSubmatch(line); m == nil || m[len(m)-1] != want {
			t.Errorf("%q: %q", line, m)
		}
	}
	if re.MatchString("no prefix here") {
		t.Error("a line without the prefix has a body")
	}
}
