// Package serverprops reads and writes a Minecraft server.properties file
// the way the legacy manager does: flat "key=value" lines matched literally
// rather than through Java's Properties escaping rules, so a hand-edited
// value wrapped in one matching pair of quotes is accepted with the quotes
// stripped. Comments, blank lines, and every key this package's caller
// never touches survive a write byte-for-byte.
//
// Per-server MSM settings are stored inside this same file as
// msm-<lowercase-dash-name> keys, alongside the ordinary Minecraft
// properties they override defaults for.
package serverprops

import (
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/thehatchcloud/msm/internal/atomicfile"
)

// overridePrefix marks a key as an MSM per-server setting rather than an
// ordinary Minecraft property.
const overridePrefix = "msm-"

// Document is the parsed contents of a server.properties file, preserving
// every original line so an update touches only the lines it changes.
type Document struct {
	lines []string
}

// Parse loads document contents from data. It never fails: any line that is
// not a recognizable "key=value" assignment (a comment, a blank line, or
// anything else) is kept as opaque text and returned unchanged by Bytes.
func Parse(data []byte) *Document {
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return &Document{}
	}
	return &Document{lines: strings.Split(text, "\n")}
}

// Load reads and parses path.
func Load(path string) (*Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("serverprops: read %s: %w", path, err)
	}
	return Parse(data), nil
}

// Bytes serializes the document, one LF-terminated line per original or
// appended entry.
func (d *Document) Bytes() []byte {
	if len(d.lines) == 0 {
		return nil
	}
	return []byte(strings.Join(d.lines, "\n") + "\n")
}

// Save writes the document to path through atomicfile.Write, so a reader
// never observes a half-written file. path's existing permission bits are
// preserved; perm applies only if path does not yet exist.
func (d *Document) Save(path string, perm fs.FileMode) error {
	if err := atomicfile.Write(path, d.Bytes(), perm); err != nil {
		return fmt.Errorf("serverprops: write %s: %w", path, err)
	}
	return nil
}

// parseLine splits a raw line into a key/value assignment. A comment
// ('#' or '!', the two prefixes Java's Properties format recognizes), a
// blank line, or a line without '=' is not an assignment.
func parseLine(line string) (key, value string, ok bool) {
	t := strings.TrimSpace(line)
	if t == "" || t[0] == '#' || t[0] == '!' {
		return "", "", false
	}
	i := strings.IndexByte(line, '=')
	if i < 0 {
		return "", "", false
	}
	return line[:i], line[i+1:], true
}

// unquote strips exactly one layer of matching single or double quotes
// around a value, the same literal shape the legacy manager's sed-based
// reader strips; it never interprets Java Properties backslash escapes.
func unquote(v string) string {
	if len(v) >= 2 {
		first, last := v[0], v[len(v)-1]
		if (first == '"' && last == '"') || (first == '\'' && last == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

// Get returns the value of key, matched case-insensitively against every
// assignment line, with the last matching line winning and its value
// quote-stripped. This mirrors the legacy manager's own per-server property
// reader.
func (d *Document) Get(key string) (string, bool) {
	value, _, found := d.lastMatch(key)
	return value, found
}

func (d *Document) lastMatch(key string) (value string, index int, found bool) {
	index = -1
	for i, line := range d.lines {
		k, v, ok := parseLine(line)
		if !ok || !strings.EqualFold(k, key) {
			continue
		}
		value, index, found = unquote(v), i, true
	}
	return value, index, found
}

// Set assigns key to value, in place if key already has an exact,
// case-sensitive matching assignment line, or otherwise by appending a new
// unquoted "key=value" line. The written value is never quoted, matching
// the legacy manager's own property writer; only Get needs to tolerate
// quotes left over from a hand-edited file.
func (d *Document) Set(key, value string) {
	line := key + "=" + value
	for i, existing := range d.lines {
		if k, _, ok := parseLine(existing); ok && k == key {
			d.lines[i] = line
			return
		}
	}
	d.lines = append(d.lines, line)
}

// Overrides returns every msm-<lowercase-dash-name> per-server setting in
// the document, keyed by name with the "msm-" prefix and any surrounding
// quotes removed. A name repeated with different casing keeps only the
// value from its last occurrence, matching Get.
func (d *Document) Overrides() map[string]string {
	overrides := map[string]string{}
	for _, line := range d.lines {
		k, v, ok := parseLine(line)
		if !ok || len(k) <= len(overridePrefix) || !strings.EqualFold(k[:len(overridePrefix)], overridePrefix) {
			continue
		}
		name := strings.ToLower(k[len(overridePrefix):])
		overrides[name] = unquote(v)
	}
	return overrides
}

// SetOverride assigns the msm-<name> per-server setting to value. name is
// the lowercase-dash setting name without the "msm-" prefix, for example
// "stop-delay" or "message-stop".
func (d *Document) SetOverride(name, value string) {
	d.Set(overridePrefix+strings.ToLower(name), value)
}
