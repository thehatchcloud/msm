package migrate

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/thehatchcloud/msm/internal/maintenance"
)

// EntryKind says what a line of the legacy cron file becomes.
type EntryKind string

const (
	// KindJob is one of the lines cron/msm ships that "msm maintenance run
	// <job>" replaces.
	KindJob EntryKind = "job"
	// KindRetention is one of the four find | xargs rm lines, which the
	// prune job replaces together.
	KindRetention EntryKind = "retention"
	// KindCommand is another line that runs the manager with arguments the
	// Go executable accepts unchanged.
	KindCommand EntryKind = "command"
	// KindEnv is a NAME=value line, which cron applies to the lines below.
	KindEnv EntryKind = "env"
	// KindUnsupported is a line that cannot be translated safely.
	KindUnsupported EntryKind = "unsupported"
)

// CronEntry is one meaningful line of the legacy cron file.
type CronEntry struct {
	Line     int
	Raw      string
	Kind     EntryKind
	Schedule string // the five fields or @macro, as written
	User     string
	Command  string
	Job      maintenance.Job // KindJob
	Args     []string        // KindCommand: the arguments after the program
	Days     int             // KindRetention: the -mtime value
	Category string          // KindRetention: the prune category
	Why      string          // KindUnsupported: the reason
}

var (
	envLine       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*\s*=`)
	retentionLine = regexp.MustCompile(`^find (\S+) -maxdepth [0-9]+ -mtime \+([0-9]+) -type f -name "([^"]+)" \| xargs rm --force$`)
	plainWord     = regexp.MustCompile(`^[A-Za-z0-9_.:-]+$`)
	cronMacros    = map[string]bool{"@reboot": true, "@yearly": true, "@annually": true, "@monthly": true,
		"@weekly": true, "@daily": true, "@midnight": true, "@hourly": true}
)

// ParseLegacyCron reads the text of a cron.d file as data. Comments and
// blank lines are dropped. Nothing is executed.
func ParseLegacyCron(text string) []CronEntry {
	var entries []CronEntry
	for i, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		e := CronEntry{Line: i + 1, Raw: line}
		if envLine.MatchString(line) {
			e.Kind = KindEnv
			entries = append(entries, e)
			continue
		}
		fields := strings.Fields(line)
		n := 5
		if cronMacros[strings.ToLower(fields[0])] {
			n = 1
		}
		if len(fields) < n+2 {
			e.Kind, e.Why = KindUnsupported, "not a cron schedule, user and command"
			entries = append(entries, e)
			continue
		}
		e.Schedule = strings.Join(fields[:n], " ")
		e.User = fields[n]
		e.Command = strings.Join(fields[n+1:], " ")
		classify(&e)
		entries = append(entries, e)
	}
	return entries
}

func classify(e *CronEntry) {
	if m := retentionLine.FindStringSubmatch(e.Command); m != nil {
		days, _ := strconv.Atoi(m[2])
		if cat := retentionCategory(m[1], m[3]); cat != "" {
			e.Kind, e.Days, e.Category = KindRetention, days, cat
			return
		}
		e.Kind, e.Why = KindUnsupported, "a retention line for a directory the prune job does not cover"
		return
	}
	if strings.ContainsAny(e.Command, "|&;<>$`\\\"'*?(){}[]~!#") {
		e.Kind, e.Why = KindUnsupported, "uses shell syntax, which the Go port does not evaluate"
		return
	}
	words := strings.Fields(e.Command)
	if !isLegacyProgram(words[0]) {
		e.Kind, e.Why = KindUnsupported, "does not run the legacy msm command"
		return
	}
	args := words[1:]
	for _, a := range args {
		if !plainWord.MatchString(a) {
			e.Kind, e.Why = KindUnsupported, fmt.Sprintf("argument %q is not a plain word", a)
			return
		}
	}
	switch strings.Join(args, " ") {
	case "all worlds backup":
		e.Kind, e.Job = KindJob, maintenance.JobBackup
	case "all logroll":
		e.Kind, e.Job = KindJob, maintenance.JobLogroll
	case "all worlds todisk":
		e.Kind, e.Job = KindJob, maintenance.JobToDisk
	case "start":
		e.Kind, e.Job = KindJob, maintenance.JobRecover
	default:
		if len(args) == 0 {
			e.Kind, e.Why = KindUnsupported, "runs msm with no command"
			return
		}
		e.Kind, e.Args = KindCommand, args
	}
}

// isLegacyProgram: the init script, or the command link to it.
func isLegacyProgram(p string) bool {
	return p == "/etc/init.d/msm" || p == "/usr/local/bin/msm" || p == "msm"
}

// retentionCategory maps a legacy find directory and glob to the prune
// category that replaces it.
func retentionCategory(dir, glob string) string {
	switch {
	case glob == "*.log.gz" && strings.Contains(dir, "*") && strings.HasSuffix(dir, "/logs"):
		return "server-logs"
	case glob == "*.log.gz" && strings.HasSuffix(dir, "/logs"):
		return "log-archives"
	case glob == "*.zip" && strings.HasSuffix(dir, "/backups"):
		return "backups"
	case glob == "*.zip" && strings.HasSuffix(dir, "/worlds"):
		return "world-backups"
	}
	return ""
}

// Schedule is the Go cron file's content, built from the legacy file.
type Schedule struct {
	Text string
	// Dropped are the lines left out, each with the reason, so the caller
	// can refuse or report them.
	Dropped []CronEntry
	// Notes are things the administrator should know about the translation.
	Notes []string
}

// BuildSchedule renders the Go port's cron file for entries. binary is the
// Go executable and conf the msm.conf its jobs read.
//
// A job line keeps the schedule and user of the legacy line and runs
// "msm maintenance run <job>". The four retention lines become one prune
// job on the first of their schedules; it keeps the largest -mtime of them,
// so a file the legacy lines would have kept is never removed earlier, and
// its --only list names just the categories the legacy file pruned. A line
// that runs the manager with plain-word arguments keeps them. Anything else
// is dropped and listed, never guessed at.
func BuildSchedule(entries []CronEntry, binary, conf string) Schedule {
	var s Schedule
	var b strings.Builder
	b.WriteString("# Minecraft Server Manager (Go) scheduled maintenance, written by \"msm migrate apply\"\n")
	b.WriteString("# from the legacy cron file. See docs/maintenance.md and docs/migration.md.\n\n")
	b.WriteString("SHELL=/bin/sh\nPATH=/usr/local/bin:/usr/bin:/bin\nLANG=C.UTF-8\n")
	if conf != "/etc/msm.conf" {
		fmt.Fprintf(&b, "MSM_CONF=%s\n", conf)
	}
	b.WriteString("\n")

	var retention []CronEntry
	for _, e := range entries {
		switch e.Kind {
		case KindJob:
			fmt.Fprintf(&b, "%s %s %s maintenance run %s\n", e.Schedule, e.User, binary, e.Job)
		case KindCommand:
			fmt.Fprintf(&b, "%s %s %s %s\n", e.Schedule, e.User, binary, strings.Join(e.Args, " "))
			s.Notes = append(s.Notes, fmt.Sprintf("line %d runs msm with the arguments %q unchanged", e.Line, strings.Join(e.Args, " ")))
		case KindRetention:
			retention = append(retention, e)
		case KindEnv:
			s.Notes = append(s.Notes, fmt.Sprintf("line %d (%s) sets cron environment, which is not carried over; the file sets only SHELL, PATH, LANG and MSM_CONF", e.Line, e.Raw))
		case KindUnsupported:
			s.Dropped = append(s.Dropped, e)
		}
	}
	if len(retention) > 0 {
		days, only := 0, []string{}
		differ := false
		for _, e := range retention {
			if days != 0 && e.Days != days {
				differ = true
			}
			days = max(days, e.Days)
			only = append(only, e.Category)
		}
		first := retention[0]
		line := fmt.Sprintf("%s %s %s maintenance run prune --days %d", first.Schedule, first.User, binary, days)
		if len(only) < 4 {
			line += " --only " + strings.Join(only, ",")
		}
		fmt.Fprintln(&b, line)
		if differ {
			s.Notes = append(s.Notes, fmt.Sprintf("the retention lines use different ages; the prune job uses the largest, %d days, for every category", days))
		}
		if len(retention) > 1 {
			s.Notes = append(s.Notes, fmt.Sprintf("the %d retention lines became one prune job at %q", len(retention), first.Schedule))
		}
	}
	for _, e := range s.Dropped {
		fmt.Fprintf(&b, "# NOT MIGRATED (line %d: %s): %s\n", e.Line, e.Why, e.Raw)
	}
	s.Text = b.String()
	return s
}
