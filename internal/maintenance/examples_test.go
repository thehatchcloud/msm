package maintenance

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The scheduling examples and the documented job inventory are part of the
// deliverable; these tests keep them in step with the code.

const examples = "../../examples/scheduling"

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func jobNames() []string {
	var names []string
	for _, j := range Jobs {
		names = append(names, string(j))
	}
	return names
}

func TestSystemdExamplesCoverEveryJob(t *testing.T) {
	unit := read(t, filepath.Join(examples, "systemd", "msm-maintenance@.service"))
	for _, want := range []string{"ExecStart=/usr/local/bin/msm maintenance run %i", "Type=oneshot", "User=minecraft",
		"Environment=MSM_CONF=", "Environment=PATH=", "TimeoutStartSec="} {
		if !strings.Contains(unit, want) {
			t.Errorf("service lacks %q", want)
		}
	}
	for _, job := range jobNames() {
		timer := read(t, filepath.Join(examples, "systemd", "msm-"+job+".timer"))
		if !strings.Contains(timer, "Unit=msm-maintenance@"+job+".service") {
			t.Errorf("msm-%s.timer does not start its job:\n%s", job, timer)
		}
		if !strings.Contains(timer, "OnCalendar=") || !strings.Contains(timer, "Persistent=false") {
			t.Errorf("msm-%s.timer lacks a schedule or the Persistent setting", job)
		}
	}
	if m, _ := filepath.Glob(filepath.Join(examples, "systemd", "msm-*.timer")); len(m) != len(Jobs) {
		t.Errorf("%d timers for %d jobs", len(m), len(Jobs))
	}
}

func TestCronExampleRunsEveryJobDirectly(t *testing.T) {
	text := read(t, filepath.Join(examples, "cron", "msm-maintenance"))
	seen := map[string]bool{}
	for _, line := range strings.Split(text, "\n") {
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "/usr/local/bin/msm") {
			continue
		}
		if strings.ContainsAny(line, "|;&`$<>") {
			t.Errorf("a job line uses a shell construct: %s", line)
		}
		m := regexp.MustCompile(`^(\S+\s+){5}minecraft\s+/usr/local/bin/msm maintenance run (\w+)$`).FindStringSubmatch(line)
		if m == nil {
			t.Errorf("not a direct msm invocation as the manager user: %s", line)
			continue
		}
		seen[m[2]] = true
	}
	for _, job := range jobNames() {
		if !seen[job] {
			t.Errorf("no cron line for %s", job)
		}
	}
	for _, want := range []string{"MSM_CONF=", "PATH="} {
		if !strings.Contains(text, want) {
			t.Errorf("cron file does not set %s", want)
		}
	}
}

func TestLaunchdExamplesCoverEveryJob(t *testing.T) {
	for _, job := range jobNames() {
		path := filepath.Join(examples, "launchd", "com.example.msm."+job+".plist")
		text := read(t, path)
		// Well-formed XML, with the program as an argument vector.
		d := xml.NewDecoder(strings.NewReader(text))
		d.Strict = true
		d.Entity = xml.HTMLEntity
		var strs []string
		for {
			tok, err := d.Token()
			if err != nil {
				if err.Error() != "EOF" {
					t.Errorf("%s: %v", path, err)
				}
				break
			}
			if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "string" {
				var s string
				if err := d.DecodeElement(&s, &se); err != nil {
					t.Fatal(err)
				}
				strs = append(strs, s)
			}
		}
		joined := strings.Join(strs, " ")
		if !strings.Contains(joined, "com.example.msm."+job+" minecraft /usr/local/bin/msm maintenance run "+job) {
			t.Errorf("%s: label, user and argument vector not as expected: %s", path, joined)
		}
		if !strings.Contains(text, "<key>EnvironmentVariables</key>") || !strings.Contains(text, "MSM_CONF") {
			t.Errorf("%s does not set the environment explicitly", path)
		}
		if !strings.Contains(text, "StartCalendarInterval") && !strings.Contains(text, "StartInterval") {
			t.Errorf("%s has no schedule", path)
		}
	}
}

// Every job line of the upstream cron/msm is in the documented inventory.
func TestDocumentedInventoryCoversUpstreamCron(t *testing.T) {
	cron := read(t, "../../cron/msm")
	doc := read(t, "../../docs/maintenance.md")
	n := 0
	for _, line := range strings.Split(cron, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		var cmd, cadence string
		if strings.HasPrefix(fields[0], "@") {
			cadence, cmd = fields[0], strings.Join(fields[2:], " ")
		} else {
			cadence, cmd = strings.Join(fields[:5], " "), strings.Join(fields[6:], " ")
		}
		n++
		// The doc escapes the pipe inside its table.
		if want := strings.ReplaceAll(cmd, "|", `\|`); !strings.Contains(doc, want) {
			t.Errorf("the upstream job %q is not in docs/maintenance.md", cmd)
		}
		if !strings.Contains(doc, "`"+cadence+"`") {
			t.Errorf("the cadence %q of %q is not in docs/maintenance.md", cadence, cmd)
		}
	}
	if n != 8 {
		t.Errorf("cron/msm has %d jobs, the inventory was written for 8", n)
	}
	for _, job := range jobNames() {
		if !strings.Contains(doc, "maintenance run "+job) {
			t.Errorf("docs/maintenance.md does not mention job %s", job)
		}
	}
}
