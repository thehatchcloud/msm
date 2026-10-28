package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/lifecycle"
	"github.com/thehatchcloud/msm/internal/maintenance"
	"github.com/thehatchcloud/msm/internal/servers"
)

// hostRoller builds the log roller over the host's servers and console.
func hostRoller(d deps) func(io.Writer, lifecycleOptions) (*maintenance.Roller, error) {
	return func(w io.Writer, o lifecycleOptions) (*maintenance.Roller, error) {
		sm, err := d.servers(w)
		if err != nil {
			return nil, err
		}
		wm, err := d.worlds(w)
		if err != nil {
			return nil, err
		}
		cm, err := d.console(w, o)
		if err != nil {
			return nil, err
		}
		return maintenance.NewRoller(maintenance.RollConfig{Servers: sm, Worlds: wm, Console: cm, Clock: clock.Real{}})
	}
}

// hostPruner builds the retention pruner over the host's servers.
func hostPruner(d deps) func(io.Writer) (*maintenance.Pruner, error) {
	return func(w io.Writer) (*maintenance.Pruner, error) {
		sm, err := d.servers(w)
		if err != nil {
			return nil, err
		}
		return maintenance.NewPruner(maintenance.PruneConfig{Servers: sm, Clock: clock.Real{}})
	}
}

// runLogrollCommand handles msm <server> logroll; "all" rolls every server.
func runLogrollCommand(cmd *cobra.Command, d deps, o lifecycleOptions, target string) error {
	if err := o.validate(); err != nil {
		return err
	}
	r, err := d.roller(cmd.ErrOrStderr(), o)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	out := maintenance.Output{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()}
	if target == "all" {
		return r.ForAll(ctx, out, func(name string, o maintenance.Output) error { return r.Roll(ctx, name, o) })
	}
	err = r.Roll(ctx, target, out)
	if errors.Is(err, servers.ErrNotFound) || errors.Is(err, servers.ErrInvalidName) {
		return fmt.Errorf("there is no server with the name %q: %w", target, err)
	}
	return err
}

// pruneFlags are the retention flags of maintenance prune and run.
type pruneFlags struct {
	days, keep int
	dryRun     bool
	only       string
}

func (f *pruneFlags) register(cmd *cobra.Command, withDryRun bool) {
	cmd.Flags().IntVar(&f.days, "days", maintenance.DefaultDays,
		"remove files more than this many whole days old (find -mtime +N)")
	cmd.Flags().IntVar(&f.keep, "keep", 1,
		"always keep this many of the newest recoverable archives of each backup directory, however old")
	cmd.Flags().StringVar(&f.only, "only", "",
		"comma-separated categories to prune (server-logs, backups, log-archives, world-backups; default all)")
	if withDryRun {
		cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "report what would be removed and remove nothing")
	}
}

func (f *pruneFlags) options() (maintenance.PruneOptions, error) {
	o := maintenance.PruneOptions{Days: f.days, Keep: f.keep, DryRun: f.dryRun}
	if f.only != "" {
		var err error
		if o.Only, err = maintenance.ParseCategories(f.only); err != nil {
			return o, err
		}
	}
	return o, nil
}

func runPrune(ctx context.Context, cmd *cobra.Command, d deps, f *pruneFlags) error {
	opts, err := f.options()
	if err != nil {
		return err
	}
	p, err := d.pruner(cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	return p.Prune(ctx, opts, maintenance.Output{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()})
}

// newMaintenanceCommand is msm maintenance: what the host's scheduler runs.
func newMaintenanceCommand(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "maintenance",
		Short: "Scheduled maintenance: retention pruning and one-at-a-time scheduled jobs",
		Long: `Scheduled maintenance. Scheduling itself is the host's (systemd, cron,
launchd); see docs/maintenance.md for the units and the cadence of each job.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newPruneCommand(d), newRunCommand(d))
	return cmd
}

func newPruneCommand(d deps) *cobra.Command {
	var f pruneFlags
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove old server logs, rolled logs, backups and world backups",
		Long: `Remove files older than --days whole days (by modification time), in four
places, replacing the legacy find | xargs rm lines:

  server-logs    <server>/logs/*.log.gz
  backups        <BACKUP_ARCHIVE_PATH>/<server>/*.zip
  log-archives   <LOG_ARCHIVE_PATH>/<server>/*.log.gz
  world-backups  <WORLD_ARCHIVE_PATH>/<server>/<world>/*.zip

Only regular files with those names are removed, directly inside those
directories, and only through descriptors that cannot be led outside them. A
backup directory always keeps its --keep newest archives that verify as
finished ZIPs, however old; if none verifies, nothing in it is removed. Roots
that are relative, symbolic links, too close to / or overlap one another are
refused. --dry-run lists what would go and removes nothing.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return runPrune(cmd.Context(), cmd, d, &f) },
	}
	f.register(cmd, true)
	return cmd
}

func newRunCommand(d deps) *cobra.Command {
	var o lifecycleOptions
	var f pruneFlags
	var jobTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "run <backup|logroll|todisk|recover|prune>",
		Short: "Run one scheduled job, skipping it if the previous run is still going",
		Long: `Run one of the jobs the legacy cron/msm scheduled:

  backup    msm all worlds backup
  logroll   msm all logroll
  todisk    msm all worlds todisk
  recover   msm start (start active servers that stopped)
  prune     msm maintenance prune

The job takes the same per-server locks as the command typed by hand, and
runs as whoever the scheduler runs msm as, with the configuration file the
scheduler's environment names (MSM_CONF). A second run of the same job while
the first still holds the job's lock is skipped, with a note and a zero
exit status; it is bounded by --job-timeout. Other jobs and interactive
commands wait for the server locks they need.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			job, err := maintenance.ParseJob(args[0])
			if err != nil {
				return err
			}
			if err := o.validate(); err != nil {
				return err
			}
			sm, err := d.servers(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			cfg := maintenance.RunConfig{Servers: sm, Timeout: jobTimeout, Log: cmd.ErrOrStderr(), Skipped: cmd.OutOrStdout()}
			return maintenance.Run(cmd.Context(), cfg, job, func(ctx context.Context) error {
				cmd.SetContext(ctx)
				return runJob(cmd, d, o, &f, job)
			})
		},
	}
	o.register(cmd)
	cmd.Flags().DurationVar(&o.backupTimeout, "backup-timeout", 0, "how long compressing a backup may take (default 2h)")
	cmd.Flags().BoolVar(&o.allowExternalLinks, "allow-external-links", false,
		"let a complete backup with COMPLETE_BACKUP_FOLLOW_SYMLINKS=true include link targets outside the server")
	cmd.Flags().DurationVar(&jobTimeout, "job-timeout", 0, "how long the whole job may run (default depends on the job: backup 4h, todisk 20m, recover 30m, prune 1h, logroll 15m)")
	f.register(cmd, false)
	return cmd
}

func runJob(cmd *cobra.Command, d deps, o lifecycleOptions, f *pruneFlags, job maintenance.Job) error {
	switch job {
	case maintenance.JobBackup:
		return runBackupCommand(cmd, d, o, "all", false)
	case maintenance.JobLogroll:
		return runLogrollCommand(cmd, d, o, "all")
	case maintenance.JobToDisk:
		return runWorldsCommand(cmd, d, o, "all", []string{"todisk"})
	case maintenance.JobRecover:
		m, err := d.lifecycle(cmd.ErrOrStderr(), o)
		if err != nil {
			return err
		}
		return m.StartAll(cmd.Context(), lifecycle.Output{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()})
	default:
		return runPrune(cmd.Context(), cmd, d, f)
	}
}
