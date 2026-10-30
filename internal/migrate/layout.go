// Package migrate moves a legacy (Bash) MSM installation to the Go port and
// back. It has three parts: a read-only preflight that inventories the
// installation, an apply that backs everything up, disables the legacy
// scheduler and service before enabling the Go ones, and a rollback that
// restores the legacy control files from that backup. It never writes inside
// a server directory, so world data is neither converted nor replaced by
// either direction.
package migrate

import (
	"path/filepath"
	"strings"
)

// Layout names the control-plane files of an installation. The fields are
// the logical absolute paths the files have on the host (and that generated
// files refer to); Root, when set, is a directory they are all read from and
// written under instead, which is how a rehearsal runs against a copy.
type Layout struct {
	Root string

	// Legacy files, as the Bash installers (installers/common.sh) place them.
	Conf       string // /etc/msm.conf, or whatever MSM_CONF names; read by both managers
	Cron       string // /etc/cron.d/msm
	Init       string // /etc/init.d/msm, the legacy executable
	Profile    string // /etc/profile.d/msm.sh
	Completion string // /etc/bash_completion.d/msm
	Command    string // /usr/local/bin/msm, a link to Init; the Go executable takes its place
	LegacyUnit string // /etc/systemd/system/msm.service, written by the Arch installer

	// Go files.
	GoCron   string // the scheduler the Go port uses on Linux
	GoBinary string // where the Go executable is installed
	GoUnit   string // the systemd unit

	// StateDir holds the backup, the journal and the lock of a migration.
	StateDir string
}

// DefaultLayout is the layout of a default legacy installation. conf is the
// configuration file in use (MSM_CONF), or empty for /etc/msm.conf.
func DefaultLayout(conf string) Layout {
	if conf == "" {
		conf = "/etc/msm.conf"
	}
	return Layout{
		Conf: conf, Cron: "/etc/cron.d/msm", Init: "/etc/init.d/msm",
		Profile: "/etc/profile.d/msm.sh", Completion: "/etc/bash_completion.d/msm",
		Command: "/usr/local/bin/msm", LegacyUnit: "/etc/systemd/system/msm.service",
		GoCron: "/etc/cron.d/msm-maintenance", GoBinary: "/usr/local/bin/msm",
		GoUnit:   "/etc/systemd/system/msm.service",
		StateDir: "/var/lib/msm-migration",
	}
}

// P is the physical path of the logical path p.
func (l Layout) P(p string) string {
	if l.Root == "" {
		return p
	}
	return filepath.Join(l.Root, p)
}

// within reports whether physical path p is inside dir (both physical).
func within(dir, p string) bool {
	rel, err := filepath.Rel(dir, p)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}
