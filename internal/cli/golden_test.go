package cli

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/identity"
)

var update = flag.Bool("update", false, "rewrite testdata/commands.golden")

// goldenFile records, for every command the compatibility contract lists
// (compatibility/commands.tsv): its registration, its aliases, what a
// missing or extra argument does, and the exit status, standard output and
// standard error of each, on a small installation with no GNU screen.
const goldenFile = "testdata/commands.golden"

type commandRow struct{ id, signature, contractTest string }

func readCommands(t *testing.T) []commandRow {
	t.Helper()
	f, err := os.Open("../../compatibility/commands.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rows []commandRow
	sc := bufio.NewScanner(f)
	sc.Scan() // header
	for sc.Scan() {
		c := strings.Split(sc.Text(), "\t")
		rows = append(rows, commandRow{c[0], c[1], c[4]})
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return rows
}

// sampleArgs are the values that stand for the signature placeholders.
// Most are arbitrary; the few that must name something real are overridden
// per command below.
var sampleArgs = map[string][]string{
	"<name:server>": {"alpha"}, "<name:world>": {"main"}, "<name>": {"alpha"},
	"<string>": {"x"}, "<strings>": {"hello", "world"}, "<flags>": {"--noinput"},
}

var sampleOverrides = map[string][]string{
	"CMD-010": {"server", "create", "gamma"},
	"CMD-012": {"server", "rename", "alpha", "renamed"},
	"CMD-014": {"jargroup", "create", "newgroup", "https://example.invalid/x.jar"},
	"CMD-015": {"jargroup", "delete", "paper"},
	"CMD-016": {"jargroup", "rename", "paper", "renamed"},
	"CMD-017": {"jargroup", "changeurl", "paper", "https://example.invalid/y.jar"},
	"CMD-018": {"jargroup", "getlatest", "ghost"},
	"CMD-036": {"alpha", "jar", "paper"},
	"CMD-037": {"alpha", "jar", "paper", "x.jar"},
	"CMD-040": {"alpha", "config", "motd"},
	"CMD-041": {"alpha", "config", "motd", "hello"},
	"CMD-055": {"alpha", "gamemode", "survival", "Steve"},
	"CMD-058": {"alpha", "time", "set", "6000"},
	"CMD-059": {"alpha", "time", "add", "100"},
	"CMD-061": {"alpha", "give", "Steve", "stone"},
	"CMD-064": {"alpha", "xp", "Steve", "5"},
}

type goldenCase struct {
	id, kind string
	args     []string
}

// expand turns a signature into its command lines: every alias spelling,
// with `all` for the server, plus a missing-argument and an extra-argument
// variant of the first spelling.
func expand(row commandRow) []goldenCase {
	tokens := strings.Fields(row.signature)
	variants := [][]string{nil}
	for _, tok := range tokens {
		alts := []string{tok}
		if !strings.HasPrefix(tok, "<") {
			alts = strings.Split(tok, "|")
		}
		var next [][]string
		for _, v := range variants {
			for _, a := range alts {
				next = append(next, append(slices.Clone(v), a))
			}
		}
		variants = next
	}
	var cases []goldenCase
	for n, v := range variants {
		kind := "signature"
		if n > 0 {
			kind = "alias"
		}
		args := fill(row.id, v)
		cases = append(cases, goldenCase{row.id, kind, args})
		if v[0] == "<name:server>" {
			all := slices.Clone(args)
			all[0] = "all"
			cases = append(cases, goldenCase{row.id, "all", all})
		}
	}
	base := fill(row.id, variants[0])
	if last := tokens[len(tokens)-1]; strings.HasPrefix(last, "<") && last != "<flags>" {
		n := len(sampleArgs[last])
		if _, ok := sampleOverrides[row.id]; ok {
			n = 1
		}
		cases = append(cases, goldenCase{row.id, "missing-argument", slices.Clone(base[:len(base)-n])})
	}
	if last := tokens[len(tokens)-1]; last != "<strings>" && last != "<flags>" {
		cases = append(cases, goldenCase{row.id, "extra-argument", append(slices.Clone(base), "extra")})
	}
	return cases
}

// fill replaces a signature's placeholders with sample values. An override
// lists one value per signature token, so an alias keeps its spelling.
func fill(id string, tokens []string) []string {
	if o, ok := sampleOverrides[id]; ok {
		out := slices.Clone(o)
		for i, tok := range tokens {
			if !strings.HasPrefix(tok, "<") {
				out[i] = tok
			}
		}
		return out
	}
	var args []string
	for _, tok := range tokens {
		if s, ok := sampleArgs[tok]; ok {
			args = append(args, s...)
		} else {
			args = append(args, tok)
		}
	}
	return args
}

// reservedWords can never name a server (docs/compatibility/README.md).
var reservedWords = []string{"start", "stop", "restart", "version", "server", "jargroup", "config", "update", "help", "maintenance"}

// registeredLine reports whether args match the signature of any row, the
// way the legacy dispatcher matches them: literal words (with their
// aliases) exactly, one argument per <string>, <name> and <name:world>, one
// or more for <strings>, any run of dash-prefixed arguments for <flags>,
// and a server name or all for <name:server>.
func registeredLine(rows []commandRow, args []string) bool {
	for _, row := range rows {
		if matchesSignature(row.signature, args) {
			return true
		}
	}
	return false
}

func matchesSignature(signature string, args []string) bool {
	i := 0
	for _, tok := range strings.Fields(signature) {
		switch {
		case tok == "<strings>":
			if i >= len(args) {
				return false
			}
			i = len(args)
		case tok == "<flags>":
			for i < len(args) && strings.HasPrefix(args[i], "-") {
				i++
			}
		case tok == "<name:server>":
			if i >= len(args) || slices.Contains(reservedWords, args[i]) {
				return false
			}
			i++
		case strings.HasPrefix(tok, "<"):
			if i >= len(args) {
				return false
			}
			i++
		default:
			if i >= len(args) || !slices.Contains(strings.Split(tok, "|"), args[i]) {
				return false
			}
			i++
		}
	}
	return i == len(args)
}

// failingReader makes any attempt to read standard input a test failure: no
// command may wait for input when it is not a terminal.
type failingReader struct {
	t   *testing.T
	cmd string
}

func (r failingReader) Read([]byte) (int, error) {
	r.t.Errorf("%s read standard input although it is not a terminal", r.cmd)
	return 0, fmt.Errorf("unexpected read")
}

var (
	timestampRE = regexp.MustCompile(`\d{4}-\d{2}-\d{2}-\d{2}-\d{2}-\d{2}`)
	humanSizeRE = regexp.MustCompile(`\d+ files, [0-9.]+ [KMGT]?i?B`)
)

// normalize hides what differs between runs: the temporary directory, the
// timestamps in archive names and sizes. Bulk commands handle servers
// concurrently, so their lines are sorted to be comparable.
func normalize(s, base string, sorted bool) string {
	s = strings.ReplaceAll(s, base, "$TMP")
	if me, err := identity.Current(); err == nil {
		s = strings.ReplaceAll(s, `"`+me.Username+`"`, `"$USER"`)
	}
	if sorted {
		lines := strings.SplitAfter(s, "\n")
		slices.Sort(lines)
		s = strings.Join(lines, "")
	}
	s = timestampRE.ReplaceAllString(s, "$$TIME")
	return humanSizeRE.ReplaceAllString(s, "$$SIZE")
}

// refuseNetwork fails every request the same way everywhere.
type refuseNetwork struct{}

func (refuseNetwork) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("the network is disabled in tests")
}

// goldenFixture is the installation every case starts from: two servers,
// alpha with an active and an inactive world, and a JAR group.
func goldenFixture(t *testing.T) (*serverFixture, string) {
	t.Helper()
	f := newServerFixture(t)
	// No case may reach the network, or its output would depend on it.
	f.http = &http.Client{Transport: refuseNetwork{}}
	f.mustRun(t, "server", "create", "alpha")
	f.mustRun(t, "server", "create", "beta")
	for _, w := range []string{"alpha/worldstorage/main", "alpha/worldstorage_inactive/spare"} {
		if err := os.MkdirAll(filepath.Join(f.root, w), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.mustRun(t, "jargroup", "create", "paper", "https://example.invalid/paper.jar")
	return f, filepath.Dir(f.root)
}

func TestGoldenCommands(t *testing.T) {
	rows := readCommands(t)
	var all []goldenCase
	for _, row := range rows {
		all = append(all, expand(row)...)
	}
	var golden strings.Builder
	for _, c := range all {
		cmd := "msm " + strings.Join(c.args, " ")
		// A shortened or extended line can be another registered signature
		// (jar <group> is jar <group> <file> without its file); that is not
		// an arity error.
		expectValid := registeredLine(rows, c.args)
		t.Run(fmt.Sprintf("%s/%s/%s", c.id, c.kind, strings.Join(c.args, "_")), func(t *testing.T) {
			f, base := goldenFixture(t)
			f.stdin = failingReader{t, cmd}
			code, out, errOut := f.run(t, "", c.args...)
			registered := !strings.HasPrefix(errOut, "No such command")
			if expectValid && !registered {
				t.Errorf("%s: a registered command line was not recognised: %q", cmd, errOut)
			}
			if !expectValid && code != ExitUsage {
				t.Errorf("%s (%s): exit %d, want %d (usage); stdout %q stderr %q", cmd, c.kind, code, ExitUsage, out, errOut)
			}
			if code == ExitInterrupted {
				t.Errorf("%s: interrupted", cmd)
			}
			fmt.Fprintf(&golden, "=== %s [%s] %s\nexit: %d (%s)\n--- stdout\n%s--- stderr\n%s\n",
				c.id, c.kind, cmd, code, ExitName(code), normalize(out, base, bulk(c.args)), normalize(errOut, base, bulk(c.args)))
		})
	}
	got := golden.String()
	if *update {
		if err := os.MkdirAll(filepath.Dir(goldenFile), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenFile, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Fatalf("%v; create it with: make golden", err)
	}
	if string(want) != got {
		gl, wl := strings.Split(got, "\n"), strings.Split(string(want), "\n")
		for i := 0; i < len(gl) && i < len(wl); i++ {
			if gl[i] != wl[i] {
				t.Fatalf("%s differs at line %d:\n got: %q\nwant: %q\nrun: make golden", goldenFile, i+1, gl[i], wl[i])
			}
		}
		t.Fatalf("%s differs in length (%d lines, want %d); run: make golden", goldenFile, len(gl), len(wl))
	}
}

// Every row of the contract appears in the golden file at least once as a
// registered signature, so no command can be dropped without notice.
func TestGoldenCoversEveryContractRow(t *testing.T) {
	data, err := os.ReadFile(goldenFile)
	if err != nil {
		t.Skip("golden file not generated yet")
	}
	for _, row := range readCommands(t) {
		if !strings.Contains(string(data), "=== "+row.id+" [signature] ") {
			t.Errorf("%s (%s) is missing from %s", row.id, row.signature, goldenFile)
		}
	}
}

// bulk is whether a command line addresses several servers at once.
func bulk(args []string) bool {
	switch args[0] {
	case "all", "start", "stop", "restart":
		return true
	}
	return false
}
