package screen

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxInputBytes bounds one console line sent through Send. The server
// reads its console in canonical tty mode, and macOS truncates a canonical
// line at MAX_CANON (1024 bytes including the newline), so a longer line
// could not arrive intact on every supported platform.
const MaxInputBytes = 1000

var ErrUnsafeInput = errors.New("screen: unsafe console input")

// ValidateInput rejects a console line that is empty, too long, not UTF-8,
// or contains any control character, including CR/LF, so one call can
// never enter more than one line or send a terminal control sequence.
func ValidateInput(line string) error {
	switch {
	case line == "":
		return fmt.Errorf("%w: empty line", ErrUnsafeInput)
	case len(line) > MaxInputBytes:
		return fmt.Errorf("%w: longer than %d bytes", ErrUnsafeInput, MaxInputBytes)
	case !utf8.ValidString(line):
		return fmt.Errorf("%w: not valid UTF-8", ErrUnsafeInput)
	}
	for i, r := range line {
		if unicode.IsControl(r) {
			return fmt.Errorf("%w: control character %U at byte %d", ErrUnsafeInput, r, i)
		}
	}
	return nil
}

// stuffChunkBytes bounds one `-X stuff` argument. Screen copies the
// argument into a fixed message buffer and, when it does not fit, drops it
// while still exiting 0: 4.09 silently discards anything over 756 bytes.
// Chunks well under that keep a line intact on older releases too.
const stuffChunkBytes = 128

// stuffChunks escapes a validated line for screen's `-X stuff` parser and
// splits it into arguments of at most stuffChunkBytes, ending with a
// carriage return. Measured on screen 4.09 (see docs/development.md): `\`
// starts an octal or literal escape, `^X` becomes a control character, and
// `$NAME`/`${NAME}` expands the screen server's environment, so each of
// those is backslash-escaped. Quotes pass through unchanged because every
// chunk is already one argv element. A chunk never splits an escape or a
// UTF-8 sequence. The native tests round-trip every printable ASCII
// character and a maximum-length line through a real screen.
func stuffChunks(line string) []string {
	var chunks []string
	var b strings.Builder
	for _, r := range line + "\r" {
		piece := string(r)
		if r == '\\' || r == '^' || r == '$' {
			piece = "\\" + piece
		}
		if b.Len()+len(piece) > stuffChunkBytes {
			chunks = append(chunks, b.String())
			b.Reset()
		}
		b.WriteString(piece)
	}
	return append(chunks, b.String())
}
