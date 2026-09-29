// Package profiles holds the parts of the legacy version profiles
// (versioning/<type>/<version>.sh) that server lifecycle needs, as inert
// data: where the server writes its console log, how a log line starts,
// which line means the server has started, and how save-all is confirmed.
// Nothing is sourced or evaluated (DEV-005). P08 extends these profiles
// with the console commands and adds a modern profile.
package profiles

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Event is a console_event: log lines that mark something happening.
type Event struct {
	// Pattern is the alternation of the accepted lines.
	Pattern string
	Timeout time.Duration
}

// Command is a console_command whose output confirms it.
type Command struct {
	Line    string
	Confirm string
	Timeout time.Duration
}

// Profile is one legacy version profile, with its inherited values filled.
type Profile struct {
	// Name is "<type>/<version>", for example "minecraft/1.7.0".
	Name string
	// LogPath is the profile's LOG_PATH, relative to the server directory.
	LogPath string
	// LinePrefix is console_event REGEX: what every log line starts with.
	LinePrefix string
	Start      Event
	SaveAll    Command
}

// Settings are the per-server setting values the profile supplies
// (legacyconf.ServerInput.Profile).
func (p Profile) Settings() map[string]string {
	return map[string]string{"LOG_PATH": p.LogPath}
}

// Match compiles the legacy test for a log line: the profile's line
// prefix, one space, then one of the alternatives in pattern.
func (p Profile) Match(pattern string) (*regexp.Regexp, error) {
	re, err := regexp.Compile(p.LinePrefix + " (" + pattern + ")")
	if err != nil {
		return nil, fmt.Errorf("profiles: %s: %w", p.Name, err)
	}
	return re, nil
}

var (
	minecraft120 = Profile{
		Name:       "minecraft/1.2.0",
		LogPath:    "server.log",
		LinePrefix: `^[0-9]{4}-[0-9]{2}-[0-9]{2} [0-9]{2}:[0-9]{2}:[0-9]{2} \[.*\]`,
		Start:      Event{Pattern: "Done", Timeout: 30 * time.Second},
		SaveAll:    Command{Line: "save-all", Confirm: "CONSOLE: Save complete.", Timeout: 10 * time.Second},
	}
	minecraft130 = extend(minecraft120, "minecraft/1.3.0", func(p *Profile) { p.SaveAll.Confirm = "Saved the world" })
	minecraft170 = extend(minecraft130, "minecraft/1.7.0", func(p *Profile) {
		p.LogPath, p.LinePrefix = "logs/latest.log", `^\[[0-9]{2}:[0-9]{2}:[0-9]{2}\] \[.*\]:`
	})
	craftbukkit120 = extend(minecraft120, "craftbukkit/1.2.0", nil)
	craftbukkit130 = extend(minecraft130, "craftbukkit/1.3.0", func(p *Profile) { p.SaveAll.Confirm = "Save complete." })
)

func extend(base Profile, name string, change func(*Profile)) Profile {
	base.Name = name
	if change != nil {
		change(&base)
	}
	return base
}

// All is every built-in profile, as shipped in versioning/.
var All = []Profile{minecraft120, minecraft130, minecraft170, craftbukkit120, craftbukkit130}

// Newest is what the legacy manager assumes for a server whose VERSION is
// unset or matches no profile: the newest minecraft profile.
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
		for i := range All {
			pk, pn, _ := strings.Cut(All[i].Name, "/")
			v, _ := parseVersion(pn)
			if pk == kind && !less(want, v) && (best == nil || less(bestVersion, v)) {
				best, bestVersion = &All[i], v
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
