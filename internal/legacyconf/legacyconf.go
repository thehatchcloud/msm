// Package legacyconf reads MSM's inherited Bash configuration files
// (msm.conf and whatever file MSM_CONF points at) as literal data. The
// Bash implementation loads these through eval; nothing in this package
// executes shell code. A value that still looks like it depends on shell
// evaluation after unquoting is rejected with the file and line it was
// found on and migration advice, rather than silently passed through.
package legacyconf

import (
	"bufio"
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Assignment is one literal NAME=value pairing, with the source line it was
// read from for diagnostics.
type Assignment struct {
	Value string
	Line  int
}

// File is the literal contents of a parsed legacy configuration file. When a
// name is assigned more than once, the last assignment wins, matching the
// Bash implementation, which evals each line in file order.
type File struct {
	Path   string
	Values map[string]Assignment
}

// Get returns the last assignment to name, if any, matched exactly.
func (f *File) Get(name string) (string, bool) {
	a, ok := f.Values[name]
	return a.Value, ok
}

// Lookup reads a setting the way the legacy manager's manager_property
// does: the name is matched case-insensitively, the last matching line in
// the file wins, and an empty value counts as unset, so the caller's
// default applies. Use it, not Get, to resolve a setting's effective value.
func (f *File) Lookup(name string) (string, bool) {
	last := Assignment{Line: -1}
	for k, a := range f.Values {
		if strings.EqualFold(k, name) && a.Line > last.Line {
			last = a
		}
	}
	return last.Value, last.Value != ""
}

const migrationAdvice = "rewrite the assignment as a plain literal value (quoted if it contains spaces); the Go port never evaluates shell syntax in configuration"

// SyntaxError reports a line that is not a literal assignment, or whose
// value still looks like it depends on shell evaluation once unquoted. It
// is safe to print directly: it names the offending file, line, and what to
// do about it.
type SyntaxError struct {
	Path   string
	Line   int
	Raw    string
	Reason string
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("%s:%d: %s: %q; %s", e.Path, e.Line, e.Reason, e.Raw, migrationAdvice)
}

var assignmentRE = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*)=(.*)$`)

// unsafeValueReason reports why a literal, already-unquoted value cannot be
// accepted, or "" if it is safe. The legacy placeholders {SERVER_NAME},
// {RAM}, {JAR} and {DELAY} are plain braces, not shell syntax, so they are
// never flagged here.
func unsafeValueReason(v string) string {
	switch {
	case strings.Contains(v, "`"):
		return "backtick command substitution is not evaluated"
	case strings.Contains(v, "$("):
		return "$() command substitution is not evaluated"
	case strings.Contains(v, "$"):
		return "shell variable expansion is not evaluated"
	case strings.ContainsAny(v, ";&|"):
		return "command chaining is not evaluated"
	case strings.ContainsAny(v, "<>"):
		return "shell redirection is not evaluated"
	case strings.HasSuffix(v, "\\"):
		return "a trailing line continuation is not evaluated"
	default:
		return ""
	}
}

// unquote strips exactly one layer of matching single or double quotes, the
// same shape the Bash implementation strips before eval. Unlike Bash,
// unquote never expands $ or backticks inside double quotes; Load rejects
// them explicitly instead so nothing is silently evaluated.
func unquote(v string) string {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// Load reads and parses path. Blank lines and lines whose first
// non-whitespace character is '#' are ignored. Every other line must match
// NAME=value, optionally with the value wrapped in one layer of matching
// quotes. Load returns a *SyntaxError, wrapped so errors.As finds it, for
// the first line that is not a literal assignment or whose value still
// looks like it depends on shell evaluation.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("legacyconf: read %s: %w", path, err)
	}

	f := &File{Path: path, Values: map[string]Assignment{}}
	scanner := bufio.NewScanner(strings.NewReader(string(data)))
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		match := assignmentRE.FindStringSubmatch(trimmed)
		if match == nil {
			return nil, fmt.Errorf("legacyconf: %w", &SyntaxError{
				Path: path, Line: lineNo, Raw: line, Reason: "not a literal NAME=value assignment",
			})
		}
		name, value := match[1], unquote(match[2])
		if reason := unsafeValueReason(value); reason != "" {
			return nil, fmt.Errorf("legacyconf: %w", &SyntaxError{
				Path: path, Line: lineNo, Raw: line, Reason: reason,
			})
		}
		f.Values[name] = Assignment{Value: value, Line: lineNo}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("legacyconf: scan %s: %w", path, err)
	}
	return f, nil
}

// Discover resolves which legacy configuration file, if any, should be
// imported: an explicit override, then the MSM_CONF environment value, then
// the fixed system default (conventionally /etc/msm.conf). Discover never
// creates a file and does not require the winning candidate to be readable
// by the current user; that surfaces as an ordinary error from Load.
func Discover(explicit, envMSMConf, systemDefault string) (path string, ok bool) {
	for _, candidate := range []string{explicit, envMSMConf, systemDefault} {
		if candidate == "" {
			continue
		}
		if _, err := os.Stat(candidate); err == nil {
			return candidate, true
		}
	}
	return "", false
}

// GlobalSettings is the subset of legacy global msm.conf settings this task
// owns: manager identity, and where servers, shared JARs and each server's
// Minecraft properties file live. Every other key remains available through
// the *File this was built from, for its owning task to read later.
type GlobalSettings struct {
	Username             string
	ServerStoragePath    string
	JarStoragePath       string
	ServerPropertiesFile string

	// Warnings are non-fatal notes about the import, such as the
	// SERVER_PROPERTIES/DEFAULT_PROPERTIES_PATH spelling conflict recorded
	// in docs/compatibility/deviations.md.
	Warnings []string
}

// Exported so other packages resolving these same settings (for example
// internal/config's rootless-install default) share one literal source of
// truth with the defaults msm.conf documents, instead of repeating them.
const (
	DefaultUsername             = "minecraft"
	DefaultServerStoragePath    = "/opt/msm/servers"
	DefaultJarStoragePath       = "/opt/msm/jars"
	DefaultServerPropertiesFile = "server.properties"
)

// Global extracts the settings this task owns from a parsed file, applying
// the same defaults msm.conf documents. Values are read with Lookup, so an
// empty assignment such as USERNAME="" keeps the default, as it does in the
// legacy manager. It never errors on a key it does
// not recognize: a single legacy file is shared across every task that
// eventually reads its own settings from it.
func Global(f *File) GlobalSettings {
	settings := GlobalSettings{
		Username:             DefaultUsername,
		ServerStoragePath:    DefaultServerStoragePath,
		JarStoragePath:       DefaultJarStoragePath,
		ServerPropertiesFile: DefaultServerPropertiesFile,
	}
	if v, ok := f.Lookup("USERNAME"); ok {
		settings.Username = v
	}
	if v, ok := f.Lookup("SERVER_STORAGE_PATH"); ok {
		settings.ServerStoragePath = v
	}
	if v, ok := f.Lookup("JAR_STORAGE_PATH"); ok {
		settings.JarStoragePath = v
	}

	// SERVER_PROPERTIES is the setting init/msm actually registers and
	// reads. DEFAULT_PROPERTIES_PATH is the spelling msm.conf's sample file
	// ships, which upstream never reads back; import either spelling into
	// one canonical field rather than silently dropping an administrator's
	// configured filename.
	registered, hasRegistered := f.Lookup("SERVER_PROPERTIES")
	legacy, hasLegacy := f.Lookup("DEFAULT_PROPERTIES_PATH")
	switch {
	case hasRegistered:
		settings.ServerPropertiesFile = registered
		if hasLegacy && legacy != registered {
			settings.Warnings = append(settings.Warnings, fmt.Sprintf(
				"%s: both SERVER_PROPERTIES=%q and DEFAULT_PROPERTIES_PATH=%q are set; using SERVER_PROPERTIES because the original manager never read DEFAULT_PROPERTIES_PATH",
				f.Path, registered, legacy))
		}
	case hasLegacy:
		settings.ServerPropertiesFile = legacy
		settings.Warnings = append(settings.Warnings, fmt.Sprintf(
			"%s: DEFAULT_PROPERTIES_PATH=%q is set, a key the original manager never read; importing it as the Minecraft properties filename",
			f.Path, legacy))
	}
	return settings
}
