package migrate

import (
	"fmt"
	"strings"
	"testing"

	"github.com/thehatchcloud/msm/internal/maintenance"
)

func TestShippedCronFileMapsToTheMaintenanceJobs(t *testing.T) {
	entries := ParseLegacyCron(string(readRepo(t, "cron/msm")))
	want := map[maintenance.Job]string{
		maintenance.JobBackup: "02 05 * * *", maintenance.JobLogroll: "55 04 * * *",
		maintenance.JobToDisk: "*/30 * * * *", maintenance.JobRecover: "@hourly",
	}
	cats := map[string]bool{}
	for _, e := range entries {
		switch e.Kind {
		case KindJob:
			if want[e.Job] != e.Schedule || e.User != "minecraft" {
				t.Errorf("%s: schedule %q user %q", e.Job, e.Schedule, e.User)
			}
			delete(want, e.Job)
		case KindRetention:
			cats[e.Category] = true
			if e.Days != 30 {
				t.Errorf("%s: days %d", e.Category, e.Days)
			}
		default:
			t.Errorf("unexpected %+v", e)
		}
	}
	if len(want) != 0 || len(cats) != 4 {
		t.Errorf("jobs left %v, retention categories %v", want, cats)
	}
	s := BuildSchedule(entries, "/usr/local/bin/msm", "/etc/msm.conf")
	if len(s.Dropped) != 0 {
		t.Errorf("dropped %+v", s.Dropped)
	}
	for _, line := range []string{
		"02 05 * * * minecraft /usr/local/bin/msm maintenance run backup",
		"@hourly minecraft /usr/local/bin/msm maintenance run recover",
		"10 05 * * * minecraft /usr/local/bin/msm maintenance run prune --days 30",
	} {
		if !strings.Contains(s.Text, line+"\n") {
			t.Errorf("missing %q in\n%s", line, s.Text)
		}
	}
	if strings.Contains(s.Text, "--only") || strings.Contains(s.Text, "MSM_CONF") {
		t.Errorf("a stock file needs neither --only nor MSM_CONF:\n%s", s.Text)
	}
}

func TestCustomCronLines(t *testing.T) {
	text := strings.Join([]string{
		"MAILTO=ops@example.com",
		"15 3 * * * minecraft /etc/init.d/msm alpha worlds backup",
		"0 4 * * * root find /opt/msm/archives/backups -maxdepth 2 -mtime +14 -type f -name \"*.zip\" | xargs rm --force",
		"5 4 * * * root find /opt/msm/archives/logs -maxdepth 2 -mtime +60 -type f -name \"*.log.gz\" | xargs rm --force",
		"0 5 * * * minecraft /etc/init.d/msm all worlds backup; touch /tmp/x",
		"0 6 * * * minecraft /usr/local/bin/other-tool all",
		"0 7 * * * minecraft /etc/init.d/msm alpha cmd say $HOME",
		"not a cron line",
	}, "\n")
	entries := ParseLegacyCron(text)
	s := BuildSchedule(entries, "/opt/bin/msm", "/srv/msm.conf")
	if len(s.Dropped) != 4 {
		t.Fatalf("dropped %d lines, want 4: %+v", len(s.Dropped), s.Dropped)
	}
	for _, line := range []string{
		"MSM_CONF=/srv/msm.conf",
		"15 3 * * * minecraft /opt/bin/msm alpha worlds backup",
		// The largest age wins, and only the categories that were pruned.
		"0 4 * * * root /opt/bin/msm maintenance run prune --days 60 --only backups,log-archives",
	} {
		if !strings.Contains(s.Text, line+"\n") {
			t.Errorf("missing %q in\n%s", line, s.Text)
		}
	}
	for _, line := range s.Dropped {
		if !strings.Contains(s.Text, "# NOT MIGRATED (line "+itoa(line.Line)) {
			t.Errorf("line %d is not kept as a comment", line.Line)
		}
	}
	notes := strings.Join(s.Notes, "\n")
	for _, want := range []string{"MAILTO", "different ages"} {
		if !strings.Contains(notes, want) {
			t.Errorf("notes lack %q: %s", want, notes)
		}
	}
}

func itoa(n int) string { return fmt.Sprint(n) }
