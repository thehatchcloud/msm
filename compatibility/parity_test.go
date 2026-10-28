package compatibility

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// parity.tsv is the parity matrix: one row for every command, setting and
// surface in the three inventories, saying how the Go port accounts for it.
//
//	implemented  the Go port preserves the behavior; implementation lists
//	             the files that do it and verification the tests that prove it
//	deviation    the behavior is deliberately different or absent, as a
//	             reviewed entry in docs/compatibility/deviations.md says
//	deferred     owned by a later task (P15 packaging, P16 migration); the
//	             note says what is missing, and it is listed in parity.md
//
// A row that is none of these, a missing link, a reference to a file or
// test that does not exist, or a deviation nobody reviewed fails the test.
const parityHeader = "id\tkind\tstatus\timplementation\tverification\tdeviations\tnote"

type parityRow struct {
	id, kind, status, note string
	impl, verify, devs     []string
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ";")
}

func readParity(t *testing.T) map[string]parityRow {
	t.Helper()
	lines := strings.Split(strings.TrimRight(read(t, "parity.tsv"), "\n"), "\n")
	if lines[0] != parityHeader {
		t.Fatalf("parity.tsv: unexpected header %q", lines[0])
	}
	rows := map[string]parityRow{}
	for i, line := range lines[1:] {
		f := strings.Split(line, "\t")
		if len(f) != 7 {
			t.Fatalf("parity.tsv:%d: %d fields, want 7", i+2, len(f))
		}
		r := parityRow{id: f[0], kind: f[1], status: f[2], impl: split(f[3]), verify: split(f[4]), devs: split(f[5]), note: f[6]}
		if _, dup := rows[r.id]; dup {
			t.Fatalf("parity.tsv:%d: duplicate row %s", i+2, r.id)
		}
		rows[r.id] = r
	}
	return rows
}

// inventory returns every id the parity matrix must cover, with the owner
// the inventory gives it.
func inventory(t *testing.T) (ids map[string]string, kinds map[string]string) {
	t.Helper()
	ids, kinds = map[string]string{}, map[string]string{}
	for _, row := range table(t, "commands.tsv", "id\tsignature\thandler\towner\tcontract_test") {
		ids[row[0]], kinds[row[0]] = row[3], "command"
	}
	for _, row := range table(t, "settings.tsv", "scope\tname\tdefault\towner\tcontract_test") {
		ids[row[4]], kinds[row[4]] = row[3], "setting"
	}
	for _, row := range table(t, "surfaces.tsv", "id\ttype\tsource\tbehavior\towner\tcontract_test") {
		ids[row[0]], kinds[row[0]] = row[4], "surface"
	}
	return ids, kinds
}

func repoFile(t *testing.T, path string) string {
	t.Helper()
	return read(t, filepath.Join("..", path))
}

func TestParityMatrixAccountsForEveryRow(t *testing.T) {
	rows := readParity(t)
	owners, kinds := inventory(t)
	if len(rows) != len(owners) {
		t.Errorf("parity.tsv has %d rows, the inventories have %d", len(rows), len(owners))
	}
	for id := range owners {
		if _, ok := rows[id]; !ok {
			t.Errorf("%s has no parity row", id)
		}
	}
	devs := regexp.MustCompile(`(?m)^\| (DEV-\d{3}) \|`).FindAllStringSubmatch(read(t, "../docs/compatibility/deviations.md"), -1)
	reviewed := map[string]bool{}
	for _, d := range devs {
		reviewed[d[1]] = true
	}
	golden := repoFile(t, "internal/cli/testdata/commands.golden")
	parityDoc := read(t, "../docs/compatibility/parity.md")
	testFunc := map[string]string{}

	for id, r := range rows {
		if kinds[id] != r.kind {
			t.Errorf("%s: kind %q, the inventory says %q", id, r.kind, kinds[id])
		}
		switch r.status {
		case "implemented", "deviation":
			if len(r.impl) == 0 || len(r.verify) == 0 {
				t.Errorf("%s: %s needs an implementation and a test", id, r.status)
			}
			if r.status == "deviation" && len(r.devs) == 0 {
				t.Errorf("%s: a deviation names no reviewed deviation", id)
			}
		case "deferred":
			if owners[id] != "P15" && owners[id] != "P16" {
				t.Errorf("%s: owned by %s, which is not a later task; it cannot be deferred", id, owners[id])
			}
			if r.note == "" || !strings.Contains(parityDoc, id) {
				t.Errorf("%s: a deferred row needs a note and a line in docs/compatibility/parity.md", id)
			}
		default:
			t.Errorf("%s: unknown status %q", id, r.status)
		}
		for _, d := range r.devs {
			if !reviewed[d] {
				t.Errorf("%s: %s is not in docs/compatibility/deviations.md", id, d)
			}
		}
		for _, p := range r.impl {
			if _, err := os.Stat(filepath.Join("..", p)); err != nil {
				t.Errorf("%s: implementation %s: %v", id, p, err)
			}
		}
		for _, v := range r.verify {
			path, name, ok := strings.Cut(v, "::")
			if !ok {
				t.Errorf("%s: verification %q is not path::TestName", id, v)
				continue
			}
			name, section, _ := strings.Cut(name, "#")
			src, cached := testFunc[path]
			if !cached {
				data, err := os.ReadFile(filepath.Join("..", path))
				if err != nil {
					t.Errorf("%s: verification %s: %v", id, path, err)
					continue
				}
				src = string(data)
				testFunc[path] = src
			}
			if !strings.HasSuffix(path, "_test.go") || !strings.Contains(src, "\nfunc "+name+"(") {
				t.Errorf("%s: %s has no test function %s", id, path, name)
			}
			if section != "" && !strings.Contains(golden, "=== "+id+" [signature] ") {
				t.Errorf("%s: the golden file has no section for it", id)
			}
		}
	}
	// parity.md must describe the matrix it sits beside.
	for _, status := range []string{"implemented", "deviation", "deferred"} {
		n := 0
		for _, r := range rows {
			if r.status == status {
				n++
			}
		}
		if want := "| " + status + " | " + itoa(n) + " |"; !strings.Contains(parityDoc, want) {
			t.Errorf("docs/compatibility/parity.md does not say %q", want)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
