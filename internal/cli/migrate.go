package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/backup"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/migrate"
	"github.com/thehatchcloud/msm/internal/servers"
)

// migrateEnv is what the migration commands need from the host, replaceable
// in tests.
type migrateEnv struct {
	euid       int
	executable func() (string, error)
	lookup     func(string) (identity.Identity, error)
	prober     func() servers.Prober
	commands   migrate.Commands
}

func defaultMigrateEnv() migrateEnv {
	return migrateEnv{
		euid: os.Geteuid(), executable: os.Executable, lookup: identity.Lookup,
		prober:   func() servers.Prober { return hostProber() },
		commands: execCommands{},
	}
}

// execCommands runs the external programs a migration names, directly and
// without a shell.
type execCommands struct{}

func (execCommands) Have(name string) bool { _, err := exec.LookPath(name); return err == nil }
func (execCommands) Run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// rehearsalCommands print the commands a migration would run instead of
// running them, so a rehearsal against a copy (--root) never touches the
// real services.
type rehearsalCommands struct{ out io.Writer }

func (rehearsalCommands) Have(string) bool { return true }
func (c rehearsalCommands) Run(name string, args ...string) (string, error) {
	fmt.Fprintf(c.out, "  (rehearsal) would run: %s %s\n", name, strings.Join(args, " "))
	if len(args) > 0 && args[0] == "is-enabled" {
		return "enabled\n", nil
	}
	return "", nil
}

type migrateFlags struct {
	conf, root, stateDir string
}

func (f *migrateFlags) register(cmd *cobra.Command) {
	p := cmd.PersistentFlags()
	p.StringVar(&f.conf, "legacy-conf", "", "the legacy configuration file (default MSM_CONF, else /etc/msm.conf; with --root, the copy's /etc/msm.conf)")
	p.StringVar(&f.root, "root", "", "read and write every path below this directory instead of /, to rehearse on a copy; services are not touched")
	p.StringVar(&f.stateDir, "state-dir", "", "where the backup, journal and lock are kept (default /var/lib/msm-migration)")
}

func (f *migrateFlags) layout() migrate.Layout {
	conf := f.conf
	if conf == "" && f.root == "" {
		// A rehearsal reads the copy's /etc/msm.conf; the host's MSM_CONF
		// names a file outside it.
		conf = os.Getenv("MSM_CONF")
	}
	l := migrate.DefaultLayout(conf)
	if f.stateDir != "" {
		l.StateDir = f.stateDir
	}
	l.Root = f.root
	return l
}

// newMigrateCommand is msm migrate: move a legacy Bash installation to this
// executable, and back (DEV-038).
func newMigrateCommand(d deps) *cobra.Command {
	if d.migrate.executable == nil {
		d.migrate = defaultMigrateEnv()
	}
	var f migrateFlags
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Move a legacy Bash installation to this executable, and back",
		Long: `Migrate an installation made by the legacy Bash manager to this executable.

  migrate preflight   read-only inventory and plan; reports what cannot move
  migrate apply       back up, disable the legacy scheduler and service, then
                      install and enable the Go executable, schedule and service
  migrate rollback    restore the legacy cron file, service and command

Migration needs a stopped maintenance window: no server may be running.
Nothing is started; when the window ends start the servers with "msm start".
No world is converted, moved or written by either direction. See
docs/migration.md.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	f.register(cmd)
	cmd.AddCommand(newMigratePreflightCommand(d, &f), newMigrateApplyCommand(d, &f), newMigrateRollbackCommand(d, &f))
	return cmd
}

func (f *migrateFlags) env(d deps) migrate.Env {
	return migrate.Env{Layout: f.layout(), Prober: d.migrate.prober(), Lookup: d.migrate.lookup}
}

func (f *migrateFlags) commands(d deps, out io.Writer) migrate.Commands {
	if f.root != "" {
		return rehearsalCommands{out: out}
	}
	return d.migrate.commands
}

func newMigratePreflightCommand(d deps, f *migrateFlags) *cobra.Command {
	var drop bool
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Inventory the legacy installation and report what blocks a migration",
		Long: `Read the legacy configuration, users, JAR groups, active flags, screen
sessions, worlds, archives and cron schedule, and report unsupported shell
customizations. It changes nothing: no file is written, no lock is taken and
no server is contacted. Exit status 5 when something blocks the migration.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			o := migrate.Options{Env: f.env(d), AcceptUnsupported: drop}
			plan, err := migrate.Preflight(cmd.Context(), o)
			if err != nil {
				return err
			}
			plan.WriteReport(cmd.OutOrStdout(), map[string]bool{migrate.CodeUnsupportedSchedule: drop})
			if n := len(plan.Inventory.Blocked(map[string]bool{migrate.CodeUnsupportedSchedule: drop})); n > 0 {
				return fmt.Errorf("%w: %d; nothing was changed", migrate.ErrBlocked, n)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "\nNo blockers. Nothing was changed.")
			return nil
		},
	}
	cmd.Flags().BoolVar(&drop, "drop-unsupported", false, "report legacy cron lines that cannot be migrated as accepted")
	return cmd
}

func newMigrateApplyCommand(d deps, f *migrateFlags) *cobra.Command {
	var lo lifecycleOptions
	var binary string
	var o migrate.Options
	var backupServers, yes bool
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "Back up, disable the legacy manager, and install and enable this one",
		Long: `Migrate, in this order, stopping at the first problem and undoing what was done:

  1. copy the legacy control files into the state directory and check the copies;
  2. fingerprint every world directory;
  3. a complete backup of every stopped server (--backup-servers), or your
     statement that you made and checked your own (--external-backup);
  4. remove the legacy cron file, disable the legacy service, make the legacy
     init script non-executable and remove the msm command link;
  5. check that the legacy manager can no longer run;
  6. only then install this executable, the Go cron file and the Go service,
     and enable the service (it is not started);
  7. check the configuration, servers, active flags and world fingerprints.

It needs root, a stopped maintenance window, and one of --backup-servers and
--external-backup. Run "msm migrate preflight" first, and rehearse with
--root on a copy. Rollback: "msm migrate rollback".`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if backupServers && o.ExternalBackup {
				return usageError{errors.New("choose one of --backup-servers and --external-backup")}
			}
			if backupServers && f.root != "" {
				return usageError{errors.New("--backup-servers backs up the installation msm is configured for, not the copy --root names; use --external-backup when rehearsing")}
			}
			if err := lo.validate(); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			o.Env, o.Out, o.EUID = f.env(d), out, d.migrate.euid
			o.Commands = f.commands(d, out)
			if o.Binary = binary; o.Binary == "" {
				exe, err := d.migrate.executable()
				if err != nil {
					return err
				}
				o.Binary = exe
			}
			if o.Binary != "" && !filepath.IsAbs(o.Binary) {
				abs, err := filepath.Abs(o.Binary)
				if err != nil {
					return err
				}
				o.Binary = abs
			}
			if backupServers {
				bm, err := d.backup(cmd.ErrOrStderr(), lo)
				if err != nil {
					return err
				}
				o.DataBackup = func(ctx context.Context, name string, w io.Writer) error {
					return bm.Server(ctx, name, backupOutput(w, cmd.ErrOrStderr()))
				}
			}
			if !o.DryRun {
				// Refuse before asking: a blocker is no reason to make
				// anyone answer a question.
				check := o
				check.DryRun = true
				if err := migrate.Apply(cmd.Context(), check); err != nil {
					return err
				}
				if !yes {
					ok, err := confirmMigrate(cmd, d, "Migrate this installation to the Go manager? The legacy cron file, service and command will be disabled. [y/N]: ")
					if err != nil || !ok {
						return err
					}
				}
			}
			if err := migrate.Apply(cmd.Context(), o); err != nil {
				return err
			}
			if o.DryRun {
				fmt.Fprintln(out, "Dry run: the preflight has no blockers and nothing was changed.")
			}
			return nil
		},
	}
	lo.register(cmd)
	cmd.Flags().DurationVar(&lo.backupTimeout, "backup-timeout", 0, "how long compressing a backup may take (default 2h)")
	fl := cmd.Flags()
	fl.StringVar(&binary, "binary", "", "the Go executable to install (default: this one)")
	fl.BoolVar(&backupServers, "backup-servers", false, "make a complete backup of every server first, as \"msm <server> backup\" does")
	fl.BoolVar(&o.ExternalBackup, "external-backup", false, "you made and checked your own backup of the server data (snapshot, copy)")
	fl.BoolVar(&o.AcceptUnsupported, "drop-unsupported", false, "accept losing legacy cron lines that cannot be migrated (kept as comments and in the backup)")
	fl.BoolVar(&o.DryRun, "dry-run", false, "run the preflight and report; change nothing")
	fl.BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func newMigrateRollbackCommand(d deps, f *migrateFlags) *cobra.Command {
	var o migrate.Options
	var yes bool
	cmd := &cobra.Command{
		Use:   "rollback",
		Short: "Restore the legacy cron file, service and command from the backup",
		Long: `Undo a migration (or finish undoing one that was interrupted): disable the
Go service and schedule, remove the Go executable, and put back the legacy
cron file, service, init script mode and msm command link exactly as the
backup recorded them. What it removes is first copied into the state
directory. It needs the same stopped window and the same lock as apply, and
never writes inside a server directory, so worlds are neither replaced nor
downgraded.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			o.Env, o.Out, o.EUID = f.env(d), out, d.migrate.euid
			o.Commands = f.commands(d, out)
			if !o.DryRun && !yes {
				ok, err := confirmMigrate(cmd, d, "Roll back to the legacy manager? [y/N]: ")
				if err != nil || !ok {
					return err
				}
			}
			if err := migrate.Rollback(cmd.Context(), o); err != nil {
				return err
			}
			if o.DryRun {
				fmt.Fprintln(out, "Dry run: the window is stopped and nothing was changed.")
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&o.DryRun, "dry-run", false, "check the window and report; change nothing")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func confirmMigrate(cmd *cobra.Command, d deps, question string) (bool, error) {
	in := cmd.InOrStdin()
	if !d.isTerminal(in) {
		return false, fmt.Errorf("refusing to change the installation without confirmation: %w", errNeedsConfirmation)
	}
	fmt.Fprint(cmd.OutOrStdout(), question)
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	if !confirmAnswer.MatchString(strings.TrimRight(answer, "\r\n")) {
		fmt.Fprintln(cmd.OutOrStdout(), "Nothing was changed.")
		return false, nil
	}
	return true, nil
}

func backupOutput(out, errw io.Writer) backup.Output { return backup.Output{Out: out, Err: errw} }
