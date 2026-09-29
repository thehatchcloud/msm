package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/lifecycle"
	"github.com/thehatchcloud/msm/internal/screen"
	"github.com/thehatchcloud/msm/internal/servers"
)

// lifecycleOptions are the flags every lifecycle command takes.
type lifecycleOptions struct {
	timeout time.Duration
	jobs    int
}

func (o *lifecycleOptions) register(cmd *cobra.Command) {
	cmd.Flags().DurationVar(&o.timeout, "timeout", 0,
		"how long to wait for a server to start or stop (default 5m, or longer if its version profile says so)")
	cmd.Flags().IntVar(&o.jobs, "jobs", lifecycle.DefaultJobs, "how many servers a command for several servers handles at once")
}

func (o *lifecycleOptions) validate() error {
	if o.timeout < 0 {
		return errors.New("--timeout must not be negative")
	}
	if o.jobs < 1 {
		return errors.New("--jobs must be at least 1")
	}
	return nil
}

// hostLifecycle runs servers through the host's GNU screen. The screen
// version is checked once per server owner before it is used.
func hostLifecycle(d deps) func(io.Writer, lifecycleOptions) (*lifecycle.Manager, error) {
	return func(w io.Writer, o lifecycleOptions) (*lifecycle.Manager, error) {
		m, err := d.servers(w)
		if err != nil {
			return nil, err
		}
		p := hostProber()
		var mu sync.Mutex
		checked := map[int]error{}
		sessions := func(owner identity.Identity) (lifecycle.Sessions, error) {
			if p.Screen == "" {
				return nil, lifecycle.ErrNoScreen
			}
			b, err := screen.New(screen.Config{Screen: p.Screen, Owner: owner, Env: p.Env,
				Runner: p.Runner, Terminal: p.Terminal, Processes: p.Processes, Clock: p.Clock})
			if err != nil {
				return nil, err
			}
			mu.Lock()
			defer mu.Unlock()
			versionErr, ok := checked[owner.UID]
			if !ok {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_, versionErr = b.Version(ctx)
				cancel()
				checked[owner.UID] = versionErr
			}
			return b, versionErr
		}
		return lifecycle.New(lifecycle.Config{Servers: m, Sessions: sessions, Path: os.Getenv("PATH"),
			Clock: clock.Real{}, Timeout: o.timeout, Jobs: o.jobs})
	}
}

// newGlobalCommand is msm start, msm stop [now] or msm restart [now]: all
// servers, leaving their intent unchanged.
func newGlobalCommand(d deps, verb lifecycle.Verb) *cobra.Command {
	var o lifecycleOptions
	short := map[lifecycle.Verb]string{
		lifecycle.VerbStart:   "Start every active server that is stopped",
		lifecycle.VerbStop:    "Stop every running server, after a warning unless \"now\"",
		lifecycle.VerbRestart: "Stop every running server, then start the active ones",
	}[verb]
	use := verb.String()
	if verb != lifecycle.VerbStart {
		use += " [now]"
	}
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Long: short + `.

The global commands never change whether a server is active; use
"msm <server> start" or "msm <server> stop" for that. "now" skips the warning
and countdown, but every server is still saved and stopped cleanly.`,
		Args: nowArgs(verb),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := o.validate(); err != nil {
				return err
			}
			m, err := d.lifecycle(cmd.ErrOrStderr(), o)
			if err != nil {
				return err
			}
			out := lifecycle.Output{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()}
			now := len(args) == 1
			switch verb {
			case lifecycle.VerbStart:
				return m.StartAll(cmd.Context(), out)
			case lifecycle.VerbStop:
				return m.StopAll(cmd.Context(), now, out)
			default:
				return m.RestartAll(cmd.Context(), now, out)
			}
		},
	}
	o.register(cmd)
	return cmd
}

func nowArgs(verb lifecycle.Verb) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		switch {
		case len(args) == 0:
			return nil
		case len(args) == 1 && args[0] == "now" && (verb == lifecycle.VerbStop || verb == lifecycle.VerbRestart):
			return nil
		default:
			return fmt.Errorf("unexpected arguments %q; see msm help", args)
		}
	}
}

var serverVerbs = map[string]lifecycle.Verb{
	"start": lifecycle.VerbStart, "stop": lifecycle.VerbStop,
	"restart": lifecycle.VerbRestart, "status": lifecycle.VerbStatus,
}

// runServerCommand is the server-first grammar the root command accepts:
// msm <server> start|stop [now]|restart [now]|status, with "all" as the
// server applying the per-server command to every server. P14 extends
// this dispatcher to the remaining server commands.
func runServerCommand(cmd *cobra.Command, d deps, o lifecycleOptions, args []string) error {
	if len(args) == 0 {
		return cmd.Help()
	}
	if len(args) == 1 {
		return fmt.Errorf("no such command %q; see msm help", args[0])
	}
	target, word := args[0], args[1]
	verb, ok := serverVerbs[word]
	if !ok {
		return fmt.Errorf("no such command \"%s %s\"; server commands implemented so far: start, stop [now], restart [now], status", target, word)
	}
	now := false
	switch rest := args[2:]; {
	case len(rest) == 0:
	case len(rest) == 1 && rest[0] == "now" && (verb == lifecycle.VerbStop || verb == lifecycle.VerbRestart):
		now = true
	default:
		return fmt.Errorf("unexpected arguments %q after \"%s %s\"", rest, target, word)
	}
	if err := o.validate(); err != nil {
		return err
	}
	m, err := d.lifecycle(cmd.ErrOrStderr(), o)
	if err != nil {
		return err
	}
	ctx, out := cmd.Context(), cmd.OutOrStdout()
	if target == "all" {
		return m.Each(ctx, verb, now, lifecycle.Output{Out: out, Err: cmd.ErrOrStderr()})
	}
	switch verb {
	case lifecycle.VerbStart:
		err = m.Start(ctx, target, out)
	case lifecycle.VerbStop:
		err = m.Stop(ctx, target, now, out)
	case lifecycle.VerbRestart:
		err = m.Restart(ctx, target, now, out)
	default:
		err = m.Status(ctx, target, out)
	}
	if errors.Is(err, servers.ErrNotFound) || errors.Is(err, servers.ErrInvalidName) {
		return fmt.Errorf("there is no server with the name %q: %w", target, err)
	}
	return err
}
