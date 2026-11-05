package compatibility

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/testutil"
)

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func table(t *testing.T, path string, header string) [][]string {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(read(t, path)), "\n")
	if lines[0] != header {
		t.Fatalf("%s: unexpected header %q", path, lines[0])
	}
	width := len(strings.Split(header, "\t"))
	var rows [][]string
	ids := map[string]bool{}
	for i, line := range lines[1:] {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != width {
			t.Fatalf("%s:%d: expected %d fields, got %d", path, i+2, width, len(fields))
		}
		id := fields[len(fields)-1]
		if ids[id] || id == "" {
			t.Fatalf("%s:%d: duplicate or missing contract test %q", path, i+2, id)
		}
		ids[id] = true
		owner := fields[len(fields)-2]
		if !regexp.MustCompile(`^(P(0[2-9]|1[0-6])|DEVIATION)$`).MatchString(owner) {
			t.Fatalf("%s:%d: invalid owner %q", path, i+2, owner)
		}
		rows = append(rows, fields)
	}
	return rows
}

func baseline(t *testing.T) map[string]string {
	t.Helper()
	values := map[string]string{}
	scanner := bufio.NewScanner(strings.NewReader(read(t, "baseline.env")))
	re := regexp.MustCompile(`^([A-Z_]+)=([A-Za-z0-9._/-]+)$`)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		match := re.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("not a literal baseline assignment: %q", line)
		}
		if _, found := values[match[1]]; found {
			t.Fatalf("duplicate baseline key %s", match[1])
		}
		values[match[1]] = match[2]
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if values["UPSTREAM_COMMIT"] != "7a9d120d6b93f2f62178a60416955eea2065c5ce" {
		t.Fatal("upstream pin changed; review the compatibility baseline explicitly")
	}
	return values
}

func count(t *testing.T, key string, got int) {
	t.Helper()
	want, err := strconv.Atoi(baseline(t)[key])
	if err != nil || got != want {
		t.Fatalf("%s: got %d, want %d (parse error %v)", key, got, want, err)
	}
}

// The Bash manager that these inventories were taken from was removed in P17
// (DEV-039; git tag legacy-bash-final). The inventories are now the frozen
// record, and baseline.env says how large they must be.
func TestCommandInventory(t *testing.T) {
	rows := table(t, "commands.tsv", "id\tsignature\thandler\towner\tcontract_test")
	count(t, "COMMAND_COUNT", len(rows))
	ids := map[string]bool{}
	for _, row := range rows {
		if ids[row[0]] {
			t.Fatalf("duplicate command ID %s", row[0])
		}
		ids[row[0]] = true
	}
	if baseline(t)["UPSTREAM_MSM_VERSION"] != "0.11.0" {
		t.Fatal("legacy version changed")
	}
}

func TestSettingsInventory(t *testing.T) {
	rows := table(t, "settings.tsv", "scope\tname\tdefault\towner\tcontract_test")
	var expected []string
	knownKeys := map[string]bool{}
	totals := map[string]int{}
	for _, row := range rows {
		if row[0] == "legacy-config" {
			knownKeys[row[1]] = true
			continue
		}
		if row[0] != "server" && row[0] != "global" {
			t.Fatalf("unknown setting scope %q", row[0])
		}
		expected = append(expected, strings.Join(row[:3], "\t"))
		name := row[1]
		if row[0] == "server" {
			name = "DEFAULT_" + name
		}
		if knownKeys[name] {
			t.Fatalf("duplicate setting %s", name)
		}
		knownKeys[name] = true
	}
	for _, row := range rows {
		if row[0] == "global" || row[0] == "server" {
			totals[row[0]]++
		}
	}
	count(t, "GLOBAL_SETTING_COUNT", totals["global"])
	count(t, "SERVER_SETTING_COUNT", totals["server"])
	keys := regexp.MustCompile(`(?m)^([A-Z][A-Z0-9_]*)=`).FindAllStringSubmatch(read(t, "legacy/msm.conf"), -1)
	count(t, "CONFIG_FILE_KEY_COUNT", len(keys))
	for _, key := range keys {
		if !knownKeys[key[1]] {
			t.Fatalf("msm.conf setting is absent from the contract: %s", key[1])
		}
	}
}

// removedLegacy lists what a surface's source may name once the file is gone.
var removedLegacy = regexp.MustCompile(`^legacy:(init/|cron/|bash_completion/|installers/|versioning/|test\.sh$|msm\.conf$)`)

func TestSurfaceInventory(t *testing.T) {
	rows := table(t, "surfaces.tsv", "id\ttype\tsource\tbehavior\towner\tcontract_test")
	var profiles []string
	for _, row := range rows {
		if removedLegacy.MatchString(row[2]) {
			// Historical: the file is in git tag legacy-bash-final, not the tree.
			if _, err := os.Stat(filepath.Join("..", strings.TrimPrefix(row[2], "legacy:"))); err == nil {
				t.Fatalf("%s: %s was removed from the tree (DEV-039) but exists", row[0], row[2])
			}
		} else if _, err := os.Stat(filepath.Join("..", row[2])); err != nil {
			t.Fatalf("%s: missing source: %v", row[0], err)
		}
		if row[3] == "" {
			t.Fatalf("%s: missing behavior", row[0])
		}
		if row[1] == "version-profile" {
			name := strings.TrimSuffix(strings.TrimPrefix(row[2], "legacy:versioning/"), ".sh")
			profiles = append(profiles, filepath.Join("legacy/profiles", name+".profile"))
		}
	}
	actual, err := filepath.Glob("legacy/profiles/*/*.profile")
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(profiles)
	slices.Sort(actual)
	if !slices.Equal(profiles, actual) {
		t.Fatal("version profiles differ from the surface inventory")
	}
	count(t, "VERSION_PROFILE_COUNT", len(profiles))
}

func TestTemporaryReferenceFixture(t *testing.T) {
	root := testutil.CopyFixture(t, "fixtures/minimal")
	config := read(t, filepath.Join(root, "msm.conf"))
	if strings.Contains(config, "__FIXTURE_ROOT__") || !strings.Contains(config, `SERVER_STORAGE_PATH="`+root+`/servers"`) {
		t.Fatal("temporary paths not materialized")
	}
	for _, path := range []string{
		"servers/active-example/active",
		"servers/active-example/worldstorage/world/inram",
		"servers/inactive-example/worldstorage_inactive/archived/level.dat.fixture",
	} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "servers/inactive-example/active")); !os.IsNotExist(err) {
		t.Fatal("inactive server must not have an active marker")
	}
}
