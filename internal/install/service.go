package install

import (
	"encoding/xml"
	"fmt"
	"os"
	"strings"

	"github.com/thehatchcloud/msm/internal/atomicfile"
)

// Service locations and names.
const (
	SystemdUnitName  = "msm.service"
	SystemdUnitPath  = "/etc/systemd/system/" + SystemdUnitName
	LaunchdLabel     = "com.thehatchcloud.msm"
	LaunchdPlistPath = "/Library/LaunchDaemons/" + LaunchdLabel + ".plist"
	launchdLog       = "/var/log/msm-service.log"
)

// ServiceOptions describes a generated service definition.
type ServiceOptions struct {
	// Binary is the absolute path of the msm executable.
	Binary string
	// Conf is the msm.conf the service uses (MSM_CONF); empty uses the
	// manager's own discovery (/etc/msm.conf).
	Conf string
	// RunAs, when set, runs the service as that user instead of root.
	// Otherwise the manager starts as root and drops to each server's owner,
	// as the legacy init script did.
	RunAs string
}

func (o ServiceOptions) validate() error {
	if err := checkPath("msm executable", o.Binary); err != nil {
		return err
	}
	if o.Conf != "" {
		if err := checkPath("configuration file", o.Conf); err != nil {
			return err
		}
	}
	if o.RunAs != "" {
		return checkUser(o.RunAs)
	}
	return nil
}

// SystemdUnit renders the system unit. It is a oneshot service that remains
// "active" after start: starting it runs "msm start", which starts only the
// servers marked active, and stopping it (shutdown, or systemctl stop) runs
// "msm stop", which stops every running server cleanly but does not change
// which are active, so the same servers return at the next boot. A
// generous stop timeout lets the player warning and saves finish.
func SystemdUnit(o ServiceOptions) (string, error) {
	if err := o.validate(); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("[Unit]\nDescription=Minecraft Server Manager\nAfter=network.target local-fs.target\n\n[Service]\nType=oneshot\nRemainAfterExit=yes\n")
	if o.RunAs != "" {
		fmt.Fprintf(&b, "User=%s\n", o.RunAs)
	}
	if o.Conf != "" {
		fmt.Fprintf(&b, "Environment=MSM_CONF=%s\n", o.Conf)
	}
	fmt.Fprintf(&b, "ExecStart=%s start\nExecStop=%s stop\nExecReload=%s restart\n", o.Binary, o.Binary, o.Binary)
	b.WriteString("TimeoutStartSec=600\nTimeoutStopSec=900\n\n[Install]\nWantedBy=multi-user.target\n")
	return b.String(), nil
}

// LaunchdPlist renders the system launch daemon. launchd has no stop
// command, only SIGTERM, so the daemon runs "msm install supervise": it
// starts the active servers, stays in the foreground, and on SIGTERM stops
// every running server cleanly without changing which are active.
// ExitTimeOut is how long launchd waits for that before SIGKILL.
func LaunchdPlist(o ServiceOptions) (string, error) {
	if err := o.validate(); err != nil {
		return "", err
	}
	esc := func(s string) string {
		var b strings.Builder
		_ = xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	fmt.Fprintf(&b, "\t<key>Label</key>\n\t<string>%s</string>\n", LaunchdLabel)
	fmt.Fprintf(&b, "\t<key>ProgramArguments</key>\n\t<array>\n\t\t<string>%s</string>\n\t\t<string>install</string>\n\t\t<string>supervise</string>\n\t</array>\n", esc(o.Binary))
	if o.Conf != "" {
		fmt.Fprintf(&b, "\t<key>EnvironmentVariables</key>\n\t<dict>\n\t\t<key>MSM_CONF</key>\n\t\t<string>%s</string>\n\t</dict>\n", esc(o.Conf))
	}
	if o.RunAs != "" {
		fmt.Fprintf(&b, "\t<key>UserName</key>\n\t<string>%s</string>\n", esc(o.RunAs))
	} else {
		fmt.Fprintf(&b, "\t<key>StandardOutPath</key>\n\t<string>%s</string>\n\t<key>StandardErrorPath</key>\n\t<string>%s</string>\n", launchdLog, launchdLog)
	}
	b.WriteString("\t<key>RunAtLoad</key>\n\t<true/>\n\t<key>KeepAlive</key>\n\t<false/>\n\t<key>ExitTimeOut</key>\n\t<integer>900</integer>\n</dict>\n</plist>\n")
	return b.String(), nil
}

// Definition returns the service file text, its default path and the
// commands that enable it (without starting it now) for goos.
func Definition(goos string, o ServiceOptions) (text, path string, enable [][]string, err error) {
	switch goos {
	case "linux":
		text, err = SystemdUnit(o)
		return text, SystemdUnitPath, [][]string{{"systemctl", "daemon-reload"}, {"systemctl", "enable", SystemdUnitName}}, err
	case "darwin":
		text, err = LaunchdPlist(o)
		return text, LaunchdPlistPath, [][]string{{"launchctl", "enable", "system/" + LaunchdLabel}}, err
	}
	return "", "", nil, fmt.Errorf("install: no service definition for %s", goos)
}

// WriteService writes the definition to path. An existing different file is
// not replaced unless force is set. When enable is true, run executes the
// enable commands; otherwise nothing is enabled and nothing is started.
func WriteService(text, path string, force bool, enable [][]string, run Runner) (wrote bool, err error) {
	if err := checkPath("service file", path); err != nil {
		return false, err
	}
	unchanged := false
	if old, err := os.ReadFile(path); err == nil {
		if unchanged = string(old) == text; !unchanged && !force {
			return false, fmt.Errorf("%w: %s differs from the generated definition; review it, or pass --force to replace it", ErrExists, path)
		}
	}
	if !unchanged {
		if err := atomicfile.Write(path, []byte(text), 0o644); err != nil {
			return false, err
		}
		wrote = true
	}
	if run == nil {
		return wrote, nil
	}
	for _, c := range enable {
		if err := run(c[0], c[1:]...); err != nil {
			return wrote, err
		}
	}
	return wrote, nil
}
