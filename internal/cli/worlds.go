package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/console"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/profiles"
	"github.com/thehatchcloud/msm/internal/servers"
	"github.com/thehatchcloud/msm/internal/worlds"
)

// hostWorlds builds the world manager over the host's servers.
func hostWorlds(d deps) func(io.Writer) (*worlds.Manager, error) {
	return func(w io.Writer) (*worlds.Manager, error) {
		m, err := d.servers(w)
		if err != nil {
			return nil, err
		}
		return worlds.New(worlds.Config{Servers: m})
	}
}

// worldsOp is one worlds command, ready to run on a server.
type worldsOp func(ctx context.Context, m *worlds.Manager, server string, out worlds.Output) error

// runWorldsCommand handles msm <server> worlds list|load|on <world>|off
// <world>|ram <world>|todisk. (worlds backup is runBackupCommand.)
//
// With "all" as the server, list, load and todisk run on every server, and
// on and off take "all" as the world and apply to every world of every
// server; any other world name is ambiguous across servers and is refused,
// and so is ram, which changes one world.
func runWorldsCommand(cmd *cobra.Command, d deps, o lifecycleOptions, target string, rest []string) error {
	var op worldsOp
	var allOp worldsOp
	var world string
	var toDisk bool
	switch {
	case len(rest) == 1 && rest[0] == "list":
		op = func(ctx context.Context, m *worlds.Manager, s string, out worlds.Output) error {
			return m.List(ctx, s, out)
		}
	case len(rest) == 1 && rest[0] == "load":
		op = func(ctx context.Context, m *worlds.Manager, s string, out worlds.Output) error {
			return m.Load(ctx, s, out)
		}
	case len(rest) == 2 && (rest[0] == "on" || rest[0] == "off"):
		world = rest[1]
		activate := rest[0] == "on"
		op = func(ctx context.Context, m *worlds.Manager, s string, out worlds.Output) error {
			if activate {
				return m.Activate(ctx, s, world, out)
			}
			return m.Deactivate(ctx, s, world, out)
		}
		allOp = func(ctx context.Context, m *worlds.Manager, s string, out worlds.Output) error {
			if activate {
				return m.ActivateAll(ctx, s, out)
			}
			return m.DeactivateAll(ctx, s, out)
		}
	case len(rest) == 2 && rest[0] == "ram":
		if target == "all" {
			return errors.New(`a world's RAM status is changed on one server at a time; name the server instead of "all"`)
		}
		world = rest[1]
		op = func(ctx context.Context, m *worlds.Manager, s string, out worlds.Output) error {
			return m.ToggleRAM(ctx, s, world, out)
		}
	case len(rest) == 1 && rest[0] == "todisk":
		toDisk = true
		op = func(ctx context.Context, m *worlds.Manager, s string, out worlds.Output) error {
			return m.ToDisk(ctx, s, out)
		}
	default:
		return errNoSuchCommand
	}
	m, err := d.worlds(cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	if toDisk {
		if err := o.validate(); err != nil {
			return err
		}
		cm, err := d.console(cmd.ErrOrStderr(), o)
		if err != nil {
			return err
		}
		m = m.WithSaver(consoleSaver{cm})
	}
	ctx := cmd.Context()
	out := worlds.Output{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()}
	if target == "all" {
		if allOp != nil {
			if world != "all" {
				return errors.New(`when specifying "all" servers, "all" worlds must be specified also`)
			}
			op = allOp
		}
		return m.ForAll(ctx, out, func(name string, o worlds.Output) error { return op(ctx, m, name, o) })
	}
	if world == "all" {
		return fmt.Errorf("there is no world with the name %q", world)
	}
	err = op(ctx, m, target, out)
	switch {
	case errors.Is(err, servers.ErrNotFound) || errors.Is(err, servers.ErrInvalidName):
		return fmt.Errorf("there is no server with the name %q: %w", target, err)
	case errors.Is(err, worlds.ErrNoWorld):
		return fmt.Errorf("there is no world with the name %q", world)
	}
	return err
}

// consoleSaver saves running servers through the console manager.
type consoleSaver struct{ m *console.Manager }

func (c consoleSaver) Running(ctx context.Context, set *legacyconf.ServerSettings, out worlds.Output) (bool, error) {
	return c.m.Running(ctx, set, console.Output{Out: out.Out, Err: out.Err})
}

func (c consoleSaver) do(ctx context.Context, set *legacyconf.ServerSettings, out worlds.Output, step console.Step) error {
	err := c.m.DoLocked(ctx, set, []console.Step{step}, console.Output{Out: out.Out, Err: out.Err})
	if errors.Is(err, console.ErrRefused) {
		return fmt.Errorf("%w: %v", worlds.ErrSaveRefused, err)
	}
	return err
}

func (c consoleSaver) SaveOff(ctx context.Context, set *legacyconf.ServerSettings, out worlds.Output) error {
	return c.do(ctx, set, out, console.Step{Command: profiles.SaveOff, Doing: "Disabling saving...", Done: "Done."})
}

func (c consoleSaver) SaveAll(ctx context.Context, set *legacyconf.ServerSettings, out worlds.Output) error {
	return c.do(ctx, set, out, console.Step{Command: profiles.SaveAll, Doing: "Saving the worlds...", Done: "Done.", Sync: true})
}

func (c consoleSaver) SaveOn(ctx context.Context, set *legacyconf.ServerSettings, out worlds.Output) error {
	return c.do(ctx, set, out, console.Step{Command: profiles.SaveOn, Doing: "Enabling saving...", Done: "Done."})
}
