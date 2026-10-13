// Package profiles holds the legacy version profiles
// (versioning/<type>/<version>.sh) as inert data (DEV-005): where the
// server writes its console log, how a log line starts, which line means
// the server has started, and, for every console command MSM sends, the
// line to type and the log lines that confirm or refuse it. Nothing is
// sourced or evaluated. A modern profile for Minecraft 1.13 and later is
// defined here too; it has no .sh file.
package profiles

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Event is a console_event: log lines that mark something happening.
type Event struct {
	// Pattern is the alternation of the accepted lines.
	Pattern string
	Timeout time.Duration
}

// Console command names, as versioning/*.sh calls them.
const (
	WhitelistOn          = "WHITELIST_ON"
	WhitelistOff         = "WHITELIST_OFF"
	WhitelistAdd         = "WHITELIST_ADD"
	WhitelistRemove      = "WHITELIST_REMOVE"
	WhitelistList        = "WHITELIST_LIST"
	WhitelistReload      = "WHITELIST_RELOAD"
	BlacklistPlayerAdd   = "BLACKLIST_PLAYER_ADD"
	BlacklistPlayerRem   = "BLACKLIST_PLAYER_REMOVE"
	BlacklistPlayerList  = "BLACKLIST_PLAYER_LIST"
	BlacklistIPAdd       = "BLACKLIST_IP_ADD"
	BlacklistIPRemove    = "BLACKLIST_IP_REMOVE"
	BlacklistIPList      = "BLACKLIST_IP_LIST"
	OpAdd                = "OP_ADD"
	OpRemove             = "OP_REMOVE"
	Gamemode             = "GAMEMODE"
	Kick                 = "KICK"
	Say                  = "SAY"
	ToggleDownfall       = "TOGGLEDOWNFALL"
	Give                 = "GIVE"
	XP                   = "XP"
	Connected            = "CONNECTED"
	TimeSet              = "TIME_SET"
	TimeAdd              = "TIME_ADD"
	SaveOn               = "SAVE_ON"
	SaveOff              = "SAVE_OFF"
	SaveAll              = "SAVE_ALL"
	defaultLegacyTimeout = time.Second
)

// Command is a console_command: the line to type, with <placeholders>, and
// the log lines (regular expressions) that answer it.
type Command struct {
	Line string
	// Outputs are the answers in the order the profile lists them. The
	// first Successes of them confirm the command; the rest refuse it.
	// The legacy manager treated every output alike, so a refusal
	// printed the same "done" message as a success.
	Outputs   []string
	Successes int
	// Timeout is the profile's wait for an answer.
	Timeout time.Duration
	// Unsupported, when set, says why this server version cannot do the
	// command; nothing is sent.
	Unsupported string
	// Optional lists placeholders that may be left out; they are dropped
	// from the line and match anything in the answers.
	Optional []string
}

// Confirm is the alternation of every output, as console_command builds it.
func (c Command) Confirm() string { return strings.Join(c.Outputs, "|") }

// ConnectedFormat describes the answer to the player-list command.
type ConnectedFormat struct {
	// Header matches the line that announces the list.
	Header string
	// NextLine says the names are on the line after the header instead of
	// after it on the same line.
	NextLine bool
}

// Profile is one version profile, with its inherited values filled.
type Profile struct {
	// Name is "<type>/<version>", for example "minecraft/1.7.0".
	Name string
	// LogPath is the profile's LOG_PATH, relative to the server directory.
	LogPath string
	// LinePrefix is console_event REGEX: what every log line starts with.
	LinePrefix string
	Start      Event
	Commands   map[string]Command
	Connected  ConnectedFormat
	// Modes maps the words and numbers for a game mode to what this
	// version's gamemode command takes. A value not listed is sent as is.
	Modes map[string]string
}

// SaveAllCommand is the profile's save-all command.
func (p Profile) SaveAllCommand() Command { return p.Commands[SaveAll] }

// Settings are the per-server setting values the profile supplies
// (legacyconf.ServerInput.Profile).
func (p Profile) Settings() map[string]string {
	return map[string]string{"LOG_PATH": p.LogPath}
}

// bracketGroup is how the legacy prefixes spell a tag such as [INFO] or
// [Server thread/INFO]. Taken literally, ".*" would let a player's chat
// line "x] Done" pass for the server's own "Done", so a tag cannot contain
// a closing bracket.
const bracketGroup = `\[.*\]`

// Match compiles the legacy test for a log line: the profile's line
// prefix, one space, then one of the alternatives in pattern. The
// prefix's tags may not contain ']' (see bracketGroup).
func (p Profile) Match(pattern string) (*regexp.Regexp, error) {
	prefix := strings.ReplaceAll(p.LinePrefix, bracketGroup, `\[[^\]]*\]`)
	re, err := regexp.Compile(prefix + " (" + pattern + ")")
	if err != nil {
		return nil, fmt.Errorf("profiles: %s: %w", p.Name, err)
	}
	return re, nil
}

// Body compiles the test that splits a log line into its prefix and the
// message after it (the last group, possibly empty). It accepts only lines that
// start with the profile's prefix.
func (p Profile) Body() (*regexp.Regexp, error) {
	prefix := strings.ReplaceAll(p.LinePrefix, bracketGroup, `\[[^\]]*\]`)
	re, err := regexp.Compile(prefix + ` ?(.*)$`)
	if err != nil {
		return nil, fmt.Errorf("profiles: %s: %w", p.Name, err)
	}
	return re, nil
}

// Errors from Prepare.
var (
	ErrUnknownCommand = errors.New("profiles: unknown console command")
	ErrUnsupported    = errors.New("profiles: not supported by this server version")
)

// Prepared is a command with its arguments filled in.
type Prepared struct {
	// Line is what to type into the console.
	Line string
	// Success and Failure match the log lines that answer it. Both are
	// nil for a command the profile does not confirm.
	Success, Failure *regexp.Regexp
	Timeout          time.Duration
}

var placeholder = regexp.MustCompile(`<([a-z]+)>`)

// Prepare fills a command's placeholders. Values are never interpreted: in
// the line they replace a whole space-separated word, and in the answers
// they are matched literally. An argument the command has no placeholder
// for fails with ErrUnsupported rather than being dropped.
func (p Profile) Prepare(name string, args map[string]string) (Prepared, error) {
	c, ok := p.Commands[name]
	if !ok {
		return Prepared{}, fmt.Errorf("%w: %s in %s", ErrUnknownCommand, name, p.Name)
	}
	if c.Unsupported != "" {
		return Prepared{}, fmt.Errorf("%w: %s", ErrUnsupported, c.Unsupported)
	}
	args = maps.Clone(args)
	if mode, ok := args["mode"]; ok {
		if m, ok := p.Modes[strings.ToLower(mode)]; ok {
			args["mode"] = m
		}
	}
	for k, v := range args {
		if k != "message" {
			if err := validToken(k, v); err != nil {
				return Prepared{}, err
			}
		}
	}
	used := map[string]bool{}
	var words []string
	for _, w := range strings.Fields(c.Line) {
		m := placeholder.FindStringSubmatch(w)
		if m == nil || m[0] != w {
			words = append(words, w)
			continue
		}
		used[m[1]] = true
		if v := args[m[1]]; v != "" {
			words = append(words, v)
		} else if !contains(c.Optional, m[1]) {
			return Prepared{}, fmt.Errorf("profiles: %s needs a %s", name, m[1])
		}
	}
	for k, v := range args {
		if v != "" && !used[k] {
			return Prepared{}, fmt.Errorf("%w: %s does not take a %s in %s", ErrUnsupported, name, k, p.Name)
		}
	}
	out := Prepared{Line: strings.Join(words, " "), Timeout: c.Timeout}
	fill := func(s string) string {
		return placeholder.ReplaceAllStringFunc(s, func(ph string) string {
			if v := args[ph[1:len(ph)-1]]; v != "" {
				return regexp.QuoteMeta(v)
			}
			return ".*"
		})
	}
	var ok1, bad []string
	for i, o := range c.Outputs {
		if i < c.Successes {
			ok1 = append(ok1, fill(o))
		} else {
			bad = append(bad, fill(o))
		}
	}
	var err error
	if len(ok1) > 0 {
		if out.Success, err = p.Match(strings.Join(ok1, "|")); err != nil {
			return Prepared{}, err
		}
	}
	if len(bad) > 0 {
		if out.Failure, err = p.Match(strings.Join(bad, "|")); err != nil {
			return Prepared{}, err
		}
	}
	return out, nil
}

// ErrBadArgument is a command argument that is not one word. The legacy
// manager spliced arguments into the console line, so "Steve op" typed as
// a player name entered two commands' worth of words.
var ErrBadArgument = errors.New("profiles: invalid command argument")

const maxToken = 256

// validToken accepts a value that is a single word: no spaces and no
// control characters, so it cannot add words to the console line.
func validToken(name, v string) error {
	if len(v) > maxToken {
		return fmt.Errorf("%w: %s is longer than %d bytes", ErrBadArgument, name, maxToken)
	}
	for _, r := range v {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return fmt.Errorf("%w: %s %q must be one word without spaces or control characters", ErrBadArgument, name, v)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// cmd builds a command whose first successes outputs confirm it.
func cmd(line string, successes int, outputs ...string) Command {
	return Command{Line: line, Outputs: outputs, Successes: successes, Timeout: defaultLegacyTimeout}
}

var legacyModes = map[string]string{"survival": "0", "creative": "1", "adventure": "2", "0": "0", "1": "1", "2": "2"}

var (
	minecraft120 = Profile{
		Name:       "minecraft/1.2.0",
		LogPath:    "server.log",
		LinePrefix: `^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2} \[.*\]`,
		Start:      Event{Pattern: "Done", Timeout: 30 * time.Second},
		Connected:  ConnectedFormat{Header: `Connected players:`},
		Modes:      legacyModes,
		Commands: map[string]Command{
			WhitelistOn:         cmd("whitelist on", 0),
			WhitelistOff:        cmd("whitelist off", 0),
			WhitelistAdd:        cmd("whitelist add <player>", 0),
			WhitelistRemove:     cmd("whitelist remove <player>", 0),
			WhitelistList:       cmd("whitelist list", 0),
			WhitelistReload:     cmd("whitelist reload", 0),
			BlacklistPlayerAdd:  cmd("ban <player>", 0),
			BlacklistPlayerRem:  cmd("pardon <player>", 0),
			BlacklistPlayerList: cmd("banlist", 0),
			BlacklistIPAdd:      cmd("ban-ip <address>", 0),
			BlacklistIPRemove:   cmd("pardon-ip <address>", 0),
			BlacklistIPList:     cmd("banlist ips", 0),
			OpAdd:               cmd("op <player>", 0),
			OpRemove:            cmd("deop <player>", 0),
			Gamemode: cmd("gamemode <player> <mode>", 1,
				"CONSOLE: Setting <player> to game mode (1|0)", "Can't find user <player>", "<player> already has game mode (1|0)"),
			Kick: cmd("kick <player>", 1, "CONSOLE: Kicking <player>", "Can't find user <player>"),
			Say:  cmd("say <message>", 0),
			ToggleDownfall: cmd("toggledownfall", 1,
				"CONSOLE: Toggling downfall (on|off) for world", "No world exists with the name"),
			Give: optional(cmd("give <player> <item> <amount> <damage>", 1,
				"CONSOLE: Giving <player> some .+ (.+)", "Can't find user <player>", "There's no item called <item>"), "amount", "damage"),
			XP: cmd("xp <player> <amount>", 1,
				"CONSOLE: Giving <amount> exp to <player>", "Can't find user <player>", "Invalid exp count: <amount>"),
			Connected: cmd("list", 0, "Connected players:"),
			TimeSet:   cmd("time set <time>", 1, "CONSOLE: Set time to <time>", "Unable to convert time value"),
			TimeAdd:   cmd("time add <time>", 1, "CONSOLE: Added <time> to time", "Unable to convert time value"),
			SaveOn:    cmd("save-on", 1, "CONSOLE: Enabling level saving.."),
			SaveOff:   cmd("save-off", 1, "CONSOLE: Disabling level saving.."),
			SaveAll:   withTimeout(cmd("save-all", 1, "CONSOLE: Save complete."), 10*time.Second),
		},
	}
	minecraft130 = extend(minecraft120, "minecraft/1.3.0", func(p *Profile) {
		p.Connected = ConnectedFormat{Header: `There are [0-9]+/[0-9]+ players online:`, NextLine: true}
		p.Commands[Gamemode] = cmd("gamemode <mode> <player>", 0,
			"That player cannot be found",
			"The number you have entered \\(<mode>\\) is too big, it must be at most 2",
			"The number you have entered \\(<mode>\\) is too small, it must be at least 0",
			"'<mode>' is not a valid number")
		p.Commands[Kick] = cmd("kick <player>", 1, "Kicked <player> from the game", "That player cannot be found")
		p.Commands[TimeSet] = cmd("time set <time>", 1, "Set the time to <time>",
			"The number you have entered \\(<time>\\) is too small, it must be at least 0", "'<time>' is not a valid number")
		p.Commands[TimeAdd] = cmd("time add <time>", 1, "Added <time> to the time",
			"The number you have entered \\(<time>\\) is too small, it must be at least 0", "'<time>' is not a valid number")
		p.Commands[ToggleDownfall] = cmd("toggledownfall", 1, "Toggled downfall")
		// The shipped patterns spell some literal parentheses, "(ID <item>)"
		// and "(<mode>)", as regex groups, which cannot match the text the
		// server prints (DEV-022): here they are escaped.
		p.Commands[Give] = optional(cmd("give <player> <item> <amount> <damage>", 1,
			`Given .+ \(ID <item>\) \* <amount> to <player>`, "That player cannot be found", "'<item>' is not a valid number"), "amount", "damage")
		p.Commands[XP] = cmd("xp <amount> <player>", 1, "Given <amount> experience to <player>", "That player cannot be found",
			"'<amount>' is not a valid number", "The number you have entered \\(<amount>\\) is too small, it must be at least 0")
		p.Commands[Connected] = cmd("list", 0, `([^\ ]*)?(, [^\ ]+)*$`, "$")
		p.Commands[SaveOn] = cmd("save-on", 1, "Turned on world auto-saving")
		p.Commands[SaveOff] = cmd("save-off", 1, "Turned off world auto-saving")
		p.Commands[SaveAll] = withTimeout(cmd("save-all", 1, "Saved the world"), 10*time.Second)
	})
	minecraft170 = extend(minecraft130, "minecraft/1.7.0", func(p *Profile) {
		p.LogPath, p.LinePrefix = "logs/latest.log", `^\[[0-9]{2}:[0-9]{2}:[0-9]{2}\] \[.*\]:`
	})
	craftbukkit120 = extend(minecraft120, "craftbukkit/1.2.0", nil)
	craftbukkit130 = extend(minecraft130, "craftbukkit/1.3.0", func(p *Profile) {
		p.Commands[SaveOn] = cmd("save-on", 1, "Enabled level saving..")
		p.Commands[SaveOff] = cmd("save-off", 1, "Disabled level saving..")
		p.Commands[SaveAll] = withTimeout(cmd("save-all", 1, "Save complete."), 10*time.Second)
	})
)

// Modern is the profile for Minecraft 1.13 and later (Java edition), with
// the vanilla server's English log messages. It is not a legacy file:
// its lines are written from the vanilla language file and checked
// against recorded log fixtures, not against a live server. Paper and
// Spigot print the same messages, with a "[12:00:00 INFO]:" prefix that
// the line prefix also accepts. toggledownfall no longer exists, and
// give takes no damage value.
var modern = Profile{
	Name:       "minecraft/1.13.0",
	LogPath:    "logs/latest.log",
	LinePrefix: `^\[[0-9]{2}:[0-9]{2}:[0-9]{2}(\] \[.*\]| [A-Z]+\]):`,
	Start:      Event{Pattern: "Done", Timeout: 30 * time.Second},
	Connected:  ConnectedFormat{Header: `There are [0-9]+ of a max of [0-9]+ players online:`},
	Modes: map[string]string{"survival": "survival", "creative": "creative", "adventure": "adventure",
		"spectator": "spectator", "0": "survival", "1": "creative", "2": "adventure", "3": "spectator"},
	Commands: map[string]Command{
		WhitelistOn:  cmd("whitelist on", 1, "Whitelist is now turned on", "Whitelist is already turned on"),
		WhitelistOff: cmd("whitelist off", 1, "Whitelist is now turned off", "Whitelist is already turned off"),
		WhitelistAdd: cmd("whitelist add <player>", 1, "Added <player> to the whitelist",
			"Player is already whitelisted", "That player does not exist"),
		WhitelistRemove: cmd("whitelist remove <player>", 1, "Removed <player> from the whitelist",
			"Player is not whitelisted", "That player does not exist"),
		WhitelistList:   cmd("whitelist list", 0),
		WhitelistReload: cmd("whitelist reload", 1, "Reloaded the whitelist"),
		BlacklistPlayerAdd: cmd("ban <player>", 1, "Banned <player>",
			"Nothing changed. The player is already banned", "That player does not exist"),
		BlacklistPlayerRem:  cmd("pardon <player>", 1, "Unbanned <player>", "Nothing changed. The player isn't banned"),
		BlacklistPlayerList: cmd("banlist", 0),
		BlacklistIPAdd: cmd("ban-ip <address>", 1, "Banned IP <address>",
			"Nothing changed. That IP is already banned", "Invalid IP address or unknown player"),
		BlacklistIPRemove: cmd("pardon-ip <address>", 1, "Unbanned IP <address>",
			"Nothing changed. That IP isn't banned", "Invalid IP address"),
		BlacklistIPList: cmd("banlist ips", 0),
		OpAdd: cmd("op <player>", 1, "Made <player> a server operator",
			"Nothing changed. The player is already an operator", "That player does not exist"),
		OpRemove: cmd("deop <player>", 1, "Made <player> no longer a server operator",
			"Nothing changed. The player is not an operator", "That player does not exist"),
		Gamemode: cmd("gamemode <mode> <player>", 1, `Set <player>'s game mode to .+`,
			"No player was found", "Incorrect argument for command", "Unknown or incomplete command"),
		Kick: cmd("kick <player>", 1, "Kicked <player>", "No player was found"),
		Say:  cmd("say <message>", 0),
		ToggleDownfall: {Unsupported: "toggledownfall was removed in Minecraft 1.13; use " +
			`"cmd weather clear", "cmd weather rain" or "cmd weather thunder"`},
		Give: optional(cmd("give <player> <item> <amount>", 1, `Gave .+ to <player>`,
			"No player was found", "Unknown item", "Incorrect argument for command", "Unknown or incomplete command"), "amount"),
		XP: cmd("experience add <player> <amount> points", 1, "Gave <amount> experience points to <player>",
			"No player was found", "Incorrect argument for command", "Unknown or incomplete command"),
		Connected: cmd("list", 0),
		// time set day prints the number it set, so the answer cannot
		// name the argument.
		TimeSet: cmd("time set <time>", 1, `Set the time to [0-9]+`, "Incorrect argument for command", "Unknown or incomplete command"),
		TimeAdd: cmd("time add <time>", 1, `Set the time to [0-9]+`, "Incorrect argument for command", "Unknown or incomplete command"),
		SaveOn:  cmd("save-on", 1, "Turned on world auto-saving", "Nothing changed. Automatic saving is already on"),
		SaveOff: cmd("save-off", 1, "Turned off world auto-saving", "Nothing changed. Automatic saving is already off"),
		SaveAll: withTimeout(cmd("save-all", 1, "Saved the game"), 10*time.Second),
	},
}

func optional(c Command, names ...string) Command {
	c.Optional = names
	return c
}

func withTimeout(c Command, d time.Duration) Command {
	c.Timeout = d
	return c
}

func extend(base Profile, name string, change func(*Profile)) Profile {
	base.Name = name
	base.Commands = maps.Clone(base.Commands)
	if change != nil {
		change(&base)
	}
	return base
}

// All is every profile that ships as a versioning/ file.
var All = []Profile{minecraft120, minecraft130, minecraft170, craftbukkit120, craftbukkit130}

// Modern is the minecraft/1.13.0 profile for current servers.
var Modern = modern

// Builtin is All and Modern: every profile Select can return.
var Builtin = append(append([]Profile{}, All...), Modern)

// Newest is what the legacy manager assumes for a server whose VERSION is
// unset or matches no profile: the newest minecraft profile it shipped.
// The modern profile is chosen only by an explicit VERSION such as
// minecraft/1.21.0, so existing installations behave as before.
var Newest = minecraft170

// Select returns the profile for a server's VERSION setting the way
// get_closest_version does: the newest profile of the same type whose
// version is not newer than the requested one. With no such profile it
// falls back to Newest and returns a note saying so, as the legacy
// manager did. Versions compare numerically by major, minor and patch.
func Select(version string) (Profile, string) {
	kind, number, ok := strings.Cut(version, "/")
	want, valid := parseVersion(number)
	if ok && valid {
		var best *Profile
		var bestVersion [3]int
		for i := range Builtin {
			pk, pn, _ := strings.Cut(Builtin[i].Name, "/")
			v, _ := parseVersion(pn)
			if pk == kind && !less(want, v) && (best == nil || less(bestVersion, v)) {
				best, bestVersion = &Builtin[i], v
			}
		}
		if best != nil {
			return *best, ""
		}
	}
	return Newest, fmt.Sprintf("assuming %s for VERSION %q; set msm-version=minecraft/x.y.z in the server's properties to choose a profile", Newest.Name, version)
}

func parseVersion(s string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(s, ".")
	if s == "" || len(parts) > 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func less(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}
