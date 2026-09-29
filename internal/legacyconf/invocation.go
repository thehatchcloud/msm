package legacyconf

import (
	"errors"
	"fmt"
	"strings"
)

// ErrUnsafeInvocation reports an INVOCATION that is not a plain command
// line: the legacy manager handed it to a shell, which would have expanded,
// redirected or chained whatever these characters mean.
var ErrUnsafeInvocation = errors.New("legacyconf: INVOCATION is not a plain command line")

// invocationPlaceholders are the substitutions server_set_property applies
// to INVOCATION.
var invocationPlaceholders = []string{"{RAM}", "{JAR}"}

// Invocation splits the server's INVOCATION into the argument vector that
// is executed directly, without a shell (DEV-001).
//
// Words are separated by spaces and tabs. Single quotes keep their contents
// literally; double quotes do too, but may not contain $, ` or \, which a
// shell would have expanded. Outside quotes, every character a shell treats
// specially (; & | < > ( ) $ ` \ " ' * ? [ ] { } ~ # !) is refused rather than
// guessed at; quote an argument that needs one. {RAM} and {JAR} are then
// filled in each word with RAM (a positive whole number of megabytes) and
// the resolved JAR_PATH, so a JAR path containing spaces stays one argument.
func (s *ServerSettings) Invocation() ([]string, error) {
	raw := s.raw["INVOCATION"]
	fail := func(format string, args ...any) ([]string, error) {
		return nil, fmt.Errorf("%w: server %q INVOCATION %q (from %s): %s",
			ErrUnsafeInvocation, s.Name, raw, s.Source("INVOCATION"), fmt.Sprintf(format, args...))
	}
	words, err := splitInvocation(raw)
	if err != nil {
		return fail("%v", err)
	}
	if len(words) == 0 {
		return fail("it is empty")
	}
	ram := s.Get("RAM")
	for i, w := range words {
		if strings.Contains(w, "{RAM}") && !positiveInteger(ram) {
			return fail("RAM %q (from %s) must be a positive whole number of megabytes", ram, s.Source("RAM"))
		}
		w = strings.ReplaceAll(w, "{RAM}", ram)
		words[i] = strings.ReplaceAll(w, "{JAR}", s.Get("JAR_PATH"))
	}
	return words, nil
}

func positiveInteger(v string) bool {
	if v == "" || len(v) > 9 || strings.TrimLeft(v, "0") == "" {
		return false
	}
	for _, r := range v {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// splitInvocation tokenizes a command line without evaluating anything.
func splitInvocation(line string) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord := false
	for i := 0; i < len(line); {
		c := line[i]
		switch {
		case c < 0x20 && c != '\t' || c == 0x7f:
			return nil, fmt.Errorf("control character %q at byte %d", c, i)
		case c == ' ' || c == '\t':
			if inWord {
				words, inWord = append(words, word.String()), false
				word.Reset()
			}
			i++
			continue
		case c == '\'' || c == '"':
			end := strings.IndexByte(line[i+1:], c)
			if end < 0 {
				return nil, fmt.Errorf("unterminated %c quote at byte %d", c, i)
			}
			quoted := line[i+1 : i+1+end]
			if c == '"' {
				if j := strings.IndexAny(quoted, "$`\\"); j >= 0 {
					return nil, fmt.Errorf("%q inside double quotes at byte %d would be expanded by a shell; use single quotes", quoted[j], i+1+j)
				}
			}
			word.WriteString(quoted)
			i += end + 2
		case c == '{':
			p := placeholderAt(line[i:])
			if p == "" {
				return nil, fmt.Errorf("'{' at byte %d is not {RAM} or {JAR}; quote it if it is meant literally", i)
			}
			word.WriteString(p)
			i += len(p)
		case strings.IndexByte(";&|<>()$`\\*?[]}~#!", c) >= 0:
			return nil, fmt.Errorf("shell character %q at byte %d; quote the argument if it is meant literally", c, i)
		default:
			word.WriteByte(c)
			i++
		}
		inWord = true
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, nil
}

func placeholderAt(s string) string {
	for _, p := range invocationPlaceholders {
		if strings.HasPrefix(s, p) {
			return p
		}
	}
	return ""
}
