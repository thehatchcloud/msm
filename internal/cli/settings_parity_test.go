package cli

import (
	"bufio"
	"os"
	"strings"
	"testing"
)

type settingRow struct{ scope, name, def, ct string }

func readSettings(t *testing.T) []settingRow {
	t.Helper()
	f, err := os.Open("../../compatibility/settings.tsv")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var rows []settingRow
	sc := bufio.NewScanner(f)
	sc.Scan()
	for sc.Scan() {
		c := strings.Split(sc.Text(), "\t")
		rows = append(rows, settingRow{c[0], c[1], c[2], c[4]})
	}
	return rows
}

// Every setting in the compatibility contract is shown by `msm config` or
// `msm <server> config`, with the default the contract records, unless the
// fixture's msm.conf sets it or a deviation changes the default.
func TestEverySettingIsListedWithItsDefault(t *testing.T) {
	f := newServerFixture(t)
	f.mustRun(t, "server", "create", "alpha")
	var global globalConfigDocument
	decodeStrict(t, f.mustRun(t, "config", "--format", "json"), &global)
	globals := map[string]globalSetting{}
	for _, s := range global.Settings {
		globals[s.Name] = s
	}
	var server serverConfigDocument
	decodeStrict(t, f.mustRun(t, "alpha", "config", "--format", "json"), &server)
	perServer := map[string]serverSetting{}
	for _, s := range server.Settings {
		perServer[s.Key] = s
	}

	// The fixture msm.conf sets these; rootless detection sets the paths.
	configured := map[string]bool{"USERNAME": true, "SERVER_STORAGE_PATH": true, "JAR_STORAGE_PATH": true,
		"WORLD_ARCHIVE_PATH": true, "BACKUP_ARCHIVE_PATH": true, "LOG_ARCHIVE_PATH": true, "DEFAULT_USERNAME": true}
	// DEV-008 ships RAM worlds off.
	changed := map[string]string{"RAMDISK_STORAGE_ENABLED": "false"}
	checked := 0
	for _, r := range readSettings(t) {
		var key string
		switch r.scope {
		case "global":
			key = r.name
		case "server":
			key = "DEFAULT_" + r.name
		default:
			continue // legacy-config: an input spelling, not a registered setting
		}
		g, ok := globals[key]
		if !ok {
			t.Errorf("%s: msm config does not list %s", r.ct, key)
			continue
		}
		checked++
		want := r.def
		if v, ok := changed[key]; ok {
			want = v
		}
		if !configured[key] && g.Value != want {
			t.Errorf("%s: %s = %q, the contract's default is %q", r.ct, key, g.Value, want)
		}
		if r.scope != "server" {
			continue
		}
		s, ok := perServer["msm-"+strings.ToLower(strings.ReplaceAll(r.name, "_", "-"))]
		if !ok {
			t.Errorf("%s: alpha config does not list the per-server setting %s", r.ct, r.name)
			continue
		}
		// Values the manager expands (paths, placeholders) or takes from the
		// version profile are compared by the owning packages' tests.
		if strings.ContainsAny(r.def, "{}") || strings.HasSuffix(r.name, "_PATH") || r.name == "USERNAME" {
			continue
		}
		if s.Value != r.def {
			t.Errorf("%s: alpha %s = %q, the contract's default is %q", r.ct, s.Key, s.Value, r.def)
		}
	}
	if checked < 59 {
		t.Fatalf("only %d settings checked", checked)
	}
}
