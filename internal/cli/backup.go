package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/backup"
	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/servers"
)

// hostBackup builds the backup manager over the host's servers, worlds and
// console.
func hostBackup(d deps) func(io.Writer, lifecycleOptions) (*backup.Manager, error) {
	return func(w io.Writer, o lifecycleOptions) (*backup.Manager, error) {
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
		_, js, err := hostConfig(io.Discard)
		if err != nil {
			return nil, err
		}
		return backup.New(backup.Config{Servers: sm, Worlds: wm, Console: cm, Clock: clock.Real{},
			Timeout: o.backupTimeout, JarRoot: js.root, AllowExternal: o.allowExternalLinks})
	}
}

// runBackupCommand handles msm <server> worlds backup and msm <server>
// backup; "all" backs up every server in turn.
func runBackupCommand(cmd *cobra.Command, d deps, o lifecycleOptions, target string, complete bool) error {
	if err := o.validate(); err != nil {
		return err
	}
	m, err := d.backup(cmd.ErrOrStderr(), o)
	if err != nil {
		return err
	}
	ctx := cmd.Context()
	out := backup.Output{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()}
	op := m.Worlds
	if complete {
		op = m.Server
	}
	if target == "all" {
		return m.ForAll(ctx, out, func(name string, o backup.Output) error { return op(ctx, name, o) })
	}
	err = op(ctx, target, out)
	if errors.Is(err, servers.ErrNotFound) || errors.Is(err, servers.ErrInvalidName) {
		return fmt.Errorf("there is no server with the name %q: %w", target, err)
	}
	return err
}
