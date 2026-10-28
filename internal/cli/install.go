package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/config"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/install"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/lifecycle"
)

// installEnv is what the installation commands need from the host,
// replaceable in tests.
type installEnv struct {
	goos       string
	euid       int
	lookPath   func(string) (string, error)
	lookup     func(string) (identity.Identity, error)
	run        install.Runner
	executable func() (string, error)
}

func defaultInstallEnv() installEnv {
	return installEnv{
		goos: runtime.GOOS, euid: os.Geteuid(), lookPath: exec.LookPath,
		lookup: identity.Lookup, run: install.ExecRunner, executable: os.Executable,
	}
}

// newInstallCommand is msm install: the manual-installation helpers
// (DEV-040). Nothing here installs a package, enables a service or starts a
// server unless a flag says so.
func newInstallCommand(d deps) *cobra.Command {
	if d.install.executable == nil {
		d.install = defaultInstallEnv()
	}
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Check prerequisites and generate the configuration and service files",
		Long: `Helpers for a manual installation of the msm executable. None of them
installs a package, and none enables or starts anything unless you pass the
flag that says so.

  install check      report whether GNU screen and Java are present
  install config     write msm.conf and create the data directories
  install service    print, or with --write install, the systemd unit
                     (Linux) or launchd daemon (macOS)

See docs/install.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newInstallCheckCommand(d), newInstallConfigCommand(d), newInstallServiceCommand(d), newSuperviseCommand(d))
	return cmd
}

func newInstallCheckCommand(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Report missing prerequisites and how to install them",
		Long: `Report whether GNU screen and Java are on PATH. A missing one is described
with the command that installs it on common systems; msm never runs that
command itself. Exit status 6 when something is missing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			var missing []string
			for _, c := range install.Prerequisites(d.install.goos, d.install.lookPath) {
				if c.Path != "" {
					fmt.Fprintf(out, "ok: %s found at %s\n", c.Name, c.Path)
					continue
				}
				missing = append(missing, c.Name)
				fmt.Fprintf(out, "missing: %s, needed for %s\n  install it yourself: %s\n", c.Name, c.Required, c.Hint)
			}
			if len(missing) > 0 {
				return fmt.Errorf("missing %s; nothing was installed: %w", strings.Join(missing, " and "), errUnavailable)
			}
			return nil
		},
	}
}

func newInstallConfigCommand(d deps) *cobra.Command {
	var o install.ConfigOptions
	var servers, jars, archives string
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Write msm.conf and create the data directories",
		Long: `Write a minimal msm.conf (every other setting keeps its default) and create
the server, JAR and archive directories it names, owned by the manager user.
It never overwrites a different existing file without --force and never
touches what is inside an existing directory.

As root the file defaults to MSM_CONF or /etc/msm.conf, the user to
"minecraft" and the directories to /opt/msm, and the user must already exist.
Without root, pass --conf; point MSM_CONF at the file afterwards so msm reads
it. (A rootless msm needs no msm.conf at all: it uses per-user directories.)`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env := d.install
			if o.Path == "" {
				o.Path = os.Getenv("MSM_CONF")
			}
			if o.Path == "" {
				if env.euid != 0 {
					return usageError{errors.New("pass --conf PATH (and set MSM_CONF to it); only root writes /etc/msm.conf by default")}
				}
				o.Path = systemMSMConf
			}
			if o.Username == "" {
				if env.euid == 0 {
					o.Username = legacyconf.DefaultUsername
				} else if me, err := identity.Current(); err == nil {
					o.Username = me.Username
				} else {
					return err
				}
			}
			roots, err := config.ResolveDataRoots(nil, env.euid)
			if err != nil {
				return err
			}
			o.Servers, o.Jars = firstNonEmpty(servers, roots.ServerStoragePath), firstNonEmpty(jars, roots.JarStoragePath)
			o.Archives = firstNonEmpty(archives, filepath.Join(filepath.Dir(o.Servers), "archives"))
			o.Lookup = env.lookup
			created, err := install.WriteConfig(o)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(created) == 0 {
				_, err = fmt.Fprintf(out, "%s is already as it would be generated; nothing was changed.\n", o.Path)
				return err
			}
			for _, c := range created {
				fmt.Fprintf(out, "created %s\n", c)
			}
			_, err = fmt.Fprintf(out, "Next: msm install check; msm server create <name>; msm install service\n")
			return err
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.Path, "conf", "", "the configuration file to write (default MSM_CONF, else /etc/msm.conf as root)")
	f.StringVar(&o.Username, "user", "", "the manager user (default \"minecraft\" as root, else you)")
	f.StringVar(&servers, "servers", "", "SERVER_STORAGE_PATH (default /opt/msm/servers as root)")
	f.StringVar(&jars, "jars", "", "JAR_STORAGE_PATH (default /opt/msm/jars as root)")
	f.StringVar(&archives, "archives", "", "directory for world, backup and log archives (default next to the servers)")
	f.BoolVar(&o.Force, "force", false, "replace an existing msm.conf that differs")
	return cmd
}

func newInstallServiceCommand(d deps) *cobra.Command {
	var o install.ServiceOptions
	var write, enable, force bool
	var path string
	cmd := &cobra.Command{
		Use:   "service",
		Short: "Print or install the systemd unit (Linux) or launchd daemon (macOS)",
		Long: `Print the service definition for this system. With --write it is installed
at the system path (/etc/systemd/system/msm.service, or
/Library/LaunchDaemons/com.thehatchcloud.msm.plist); that needs root. With
--write --enable it is also enabled for the next boot. Nothing is started
now; start it with systemctl or "msm start".

The definition starts the servers marked active at boot and stops every
running server at shutdown without changing which are active, exactly as
"msm start" and "msm stop" do. On Linux that is a oneshot unit calling those
commands; launchd can only send SIGTERM, so the daemon runs the hidden
"msm install supervise" instead.

The service starts as root and msm drops to each server's owner. Use --user
to run it as the manager user from the start; that user must be the one
msm.conf names as USERNAME.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			env := d.install
			if enable && !write {
				return usageError{errors.New("--enable needs --write")}
			}
			if o.Binary == "" {
				exe, err := env.executable()
				if err != nil {
					return err
				}
				if o.Binary, err = filepath.Abs(exe); err != nil {
					return err
				}
			}
			if o.Conf == "" {
				o.Conf = os.Getenv("MSM_CONF")
			}
			if err := checkRunAs(o); err != nil {
				return err
			}
			text, dest, enableCmds, err := install.Definition(env.goos, o)
			if err != nil {
				return err
			}
			if path != "" {
				dest = path
			}
			out := cmd.OutOrStdout()
			if !write {
				fmt.Fprintf(cmd.ErrOrStderr(), "# This would be installed at %s; rerun with --write (as root) to install it.\n", dest)
				_, err = fmt.Fprint(out, text)
				return err
			}
			var run install.Runner
			if enable {
				run = env.run
			}
			wrote, err := install.WriteService(text, dest, force, enableCmds, run)
			if err != nil {
				return err
			}
			if wrote {
				fmt.Fprintf(out, "wrote %s\n", dest)
			} else {
				fmt.Fprintf(out, "%s is already as it would be generated.\n", dest)
			}
			if enable {
				fmt.Fprintln(out, "Enabled for the next boot. Nothing was started now.")
			} else {
				fmt.Fprintf(out, "Not enabled. To enable it for the next boot: %s\n", enableHint(enableCmds))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&write, "write", false, "install the definition at the system path instead of printing it")
	f.BoolVar(&enable, "enable", false, "with --write, enable the service for the next boot (never starts it now)")
	f.BoolVar(&force, "force", false, "replace an existing definition that differs")
	f.StringVar(&path, "path", "", "install here instead of the system path")
	f.StringVar(&o.Binary, "binary", "", "the msm executable the service runs (default: this one)")
	f.StringVar(&o.Conf, "conf", "", "the msm.conf the service uses (default MSM_CONF, else msm's own discovery)")
	f.StringVar(&o.RunAs, "user", "", "run the service as this user instead of root")
	return cmd
}

func enableHint(cmds [][]string) string {
	parts := make([]string, len(cmds))
	for i, c := range cmds {
		parts[i] = "sudo " + strings.Join(c, " ")
	}
	return strings.Join(parts, " && ")
}

// checkRunAs refuses a service user that msm.conf does not name as the
// manager user, which msm would then refuse to drop privileges to.
func checkRunAs(o install.ServiceOptions) error {
	if o.RunAs == "" {
		return nil
	}
	path, ok := legacyconf.Discover(o.Conf, "", systemMSMConf)
	if !ok {
		return nil
	}
	f, err := legacyconf.Load(path)
	if err != nil {
		return err
	}
	if g := legacyconf.Global(f); g.Username != o.RunAs {
		return fmt.Errorf("%w: --user %s but %s sets USERNAME=%q; they must match", install.ErrUnsafe, o.RunAs, path, g.Username)
	}
	return nil
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// newSuperviseCommand is the launchd entry point: start the active servers,
// stay in the foreground until SIGTERM, then stop every running server.
func newSuperviseCommand(d deps) *cobra.Command {
	var o lifecycleOptions
	cmd := &cobra.Command{
		Use:    "supervise",
		Short:  "Run in the foreground for launchd: start active servers, stop them on SIGTERM",
		Hidden: true,
		Args:   cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := d.lifecycle(cmd.ErrOrStderr(), o)
			if err != nil {
				return err
			}
			out := lifecycle.Output{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()}
			startErr := m.StartAll(cmd.Context(), out)
			<-cmd.Context().Done()
			// The signal that ended the wait must not cancel the shutdown:
			// launchd's ExitTimeOut, then SIGKILL, bounds it.
			stopErr := m.StopAll(context.WithoutCancel(cmd.Context()), false, out)
			return errors.Join(startErr, stopErr)
		},
	}
	o.register(cmd)
	return cmd
}
