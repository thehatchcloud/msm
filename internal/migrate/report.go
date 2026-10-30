package migrate

import (
	"fmt"
	"io"
	"strings"
)

// WriteReport prints the inventory, the findings and the plan.
func (p *Plan) WriteReport(w io.Writer, acks map[string]bool) {
	inv := p.Inventory
	fmt.Fprintf(w, "Legacy installation: %s (manager user %s, init script version %s)\n", inv.Conf, orUnknown(inv.ManagerUser), orUnknown(inv.InitVersion))
	if len(inv.Servers) > 0 {
		fmt.Fprintf(w, "\nServers in %s:\n", inv.ServerRoot)
		for _, s := range inv.Servers {
			flag := "inactive"
			if s.Active {
				flag = "ACTIVE"
			}
			ram := 0
			for _, wd := range s.Worlds {
				if wd.InRAM {
					ram++
				}
			}
			fmt.Fprintf(w, "  %-16s %-8s %-8s owner %s, screen %s, %d worlds (%d in RAM), version %s\n",
				s.Name, flag, s.State, s.Owner, s.ScreenName, len(s.Worlds), ram, s.Version)
		}
	}
	if len(inv.JarGroups) > 0 {
		fmt.Fprintf(w, "\nJAR groups in %s:\n", inv.JarRoot)
		for _, g := range inv.JarGroups {
			fmt.Fprintf(w, "  %-16s %d JARs, target %s\n", g.Name, g.JARs, orUnknown(g.Target))
		}
	}
	fmt.Fprintf(w, "\nArchives:\n")
	for _, a := range inv.Archives {
		state := "missing"
		if a.Exists {
			state = fmt.Sprintf("%d entries", a.Entries)
		}
		fmt.Fprintf(w, "  %-20s %s (%s)\n", a.Setting, a.Path, state)
	}
	if len(inv.Cron) > 0 {
		fmt.Fprintf(w, "\nLegacy schedule, and what it becomes:\n")
		for _, e := range inv.Cron {
			switch e.Kind {
			case KindJob:
				fmt.Fprintf(w, "  %-14s msm maintenance run %s\n", e.Schedule, e.Job)
			case KindRetention:
				fmt.Fprintf(w, "  %-14s prune %s (%d days)\n", e.Schedule, e.Category, e.Days)
			case KindCommand:
				fmt.Fprintf(w, "  %-14s msm %s (unchanged)\n", e.Schedule, strings.Join(e.Args, " "))
			case KindUnsupported:
				fmt.Fprintf(w, "  line %-9d NOT MIGRATED: %s\n", e.Line, e.Why)
			}
		}
		for _, n := range p.Schedule.Notes {
			fmt.Fprintf(w, "  note: %s\n", n)
		}
	}
	counts := map[Severity]int{}
	for _, f := range inv.Findings {
		counts[f.Severity]++
	}
	fmt.Fprintf(w, "\nFindings: %d blockers, %d warnings, %d notes\n", counts[Blocker], counts[Warning], counts[Info])
	for _, sev := range []Severity{Blocker, Warning, Info} {
		for _, f := range inv.Findings {
			if f.Severity != sev {
				continue
			}
			mark := strings.ToUpper(string(sev))
			if sev == Blocker && f.Code != "" && acks[f.Code] {
				mark = "ACCEPTED"
			}
			subject := ""
			if f.Subject != "" {
				subject = " " + f.Subject
			}
			fmt.Fprintf(w, "  [%s] %s%s: %s\n", mark, f.Area, subject, f.Message)
		}
	}
	fmt.Fprintf(w, "\nApply would, in this order:\n")
	for _, s := range p.Steps {
		fmt.Fprintf(w, "  %s\n", s)
	}
}
