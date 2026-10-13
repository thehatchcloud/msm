package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/console"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/process"
	"github.com/thehatchcloud/msm/internal/profiles"
	"github.com/thehatchcloud/msm/internal/servers"
)

// hostConsole sends console commands through the host's GNU screen.
func hostConsole(d deps) func(io.Writer, lifecycleOptions) (*console.Manager, error) {
	return func(w io.Writer, o lifecycleOptions) (*console.Manager, error) {
		m, err := d.servers(w)
		if err != nil {
			return nil, err
		}
		screens := hostScreens()
		sessions := func(owner identity.Identity) (console.Sessions, error) {
			b, err := screens(owner)
			if errors.Is(err, errNoScreen) {
				return nil, console.ErrNoScreen
			}
			return b, err
		}
		return console.New(console.Config{Servers: m, Sessions: sessions, Clock: clock.Real{}, Timeout: o.timeout})
	}
}

// hostStdio is the terminal the console attaches.
func hostStdio() process.Stdio {
	return process.Stdio{In: os.Stdin, Out: os.Stdout, Err: os.Stderr}
}

// consoleOp is one server-first console command, ready to run on a server.
type consoleOp func(ctx context.Context, m *console.Manager, name string, out console.Output) error

// consoleWords are the command words runServerCommand hands here, with
// their aliases.
var consoleWords = map[string]string{
	"connected": "connected", "console": "console",
	"whitelist": "whitelist", "wl": "whitelist",
	"blacklist": "blacklist", "bl": "blacklist",
	"operator": "operator", "op": "operator",
	"gamemode": "gamemode", "gm": "gamemode",
	"kick": "kick", "say": "say", "time": "time",
	"toggledownfall": "toggledownfall", "tdf": "toggledownfall",
	"give": "give", "xp": "xp", "save": "save", "cmd": "cmd", "cmdlog": "cmdlog",
}

// runConsoleCommand handles msm <server> <console command>. It reports
// handled=false for a word it does not own.
func runConsoleCommand(cmd *cobra.Command, d deps, o lifecycleOptions, target, word string, rest []string) (handled bool, err error) {
	word, ok := consoleWords[word]
	if !ok {
		return false, nil
	}
	op, perServer, err := planConsole(d, word, rest)
	if err != nil {
		return true, err
	}
	if err := o.validate(); err != nil {
		return true, err
	}
	m, err := d.console(cmd.ErrOrStderr(), o)
	if err != nil {
		return true, err
	}
	ctx := cmd.Context()
	out := console.Output{Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()}
	if target == "all" {
		if !perServer {
			return true, fmt.Errorf("%w: %q cannot be sent to all servers at once", console.ErrAmbiguous, word)
		}
		return true, m.ForAll(ctx, out, func(name string) error { return op(ctx, m, name, out) })
	}
	err = op(ctx, m, target, out)
	if errors.Is(err, servers.ErrNotFound) || errors.Is(err, servers.ErrInvalidName) {
		return true, fmt.Errorf("there is no server with the name %q: %w", target, err)
	}
	return true, err
}

// steps builds one step per name from a template, for the commands that
// act on several players.
func steps(command, done string, names []string, extra map[string]string, key string) []console.Step {
	out := make([]console.Step, len(names))
	for i, n := range names {
		args := map[string]string{key: n}
		for k, v := range extra {
			args[k] = v
		}
		out[i] = console.Step{Command: command, Args: args, Done: fmt.Sprintf(done, n)}
	}
	return out
}

// planConsole matches a command's arguments to the grammar and returns the
// operation. perServer is false for the commands that cannot address every
// server at once. A mismatch is the legacy "No such command".
func planConsole(d deps, word string, rest []string) (op consoleOp, perServer bool, err error) {
	do := func(s ...console.Step) (consoleOp, bool, error) {
		return func(ctx context.Context, m *console.Manager, name string, out console.Output) error {
			return m.Do(ctx, name, s, out)
		}, true, nil
	}
	bad := func() (consoleOp, bool, error) { return nil, false, errNoSuchCommand }
	n := len(rest)
	switch word {
	case "connected":
		if n != 0 {
			return bad()
		}
		return func(ctx context.Context, m *console.Manager, name string, out console.Output) error {
			return m.Connected(ctx, name, out)
		}, true, nil
	case "console":
		if n != 0 {
			return bad()
		}
		return func(ctx context.Context, m *console.Manager, name string, out console.Output) error {
			return m.Console(ctx, name, d.stdio(), os.Getenv("TERM"), out)
		}, false, nil
	case "whitelist":
		return planWhitelist(rest, do)
	case "blacklist":
		return planBlacklist(rest, do)
	case "operator":
		switch {
		case n == 1 && rest[0] == "list":
			return func(_ context.Context, m *console.Manager, name string, out console.Output) error {
				return m.Operators(name, out)
			}, true, nil
		case n >= 2 && rest[0] == "add":
			return do(steps(profiles.OpAdd, "Player %s is now an operator.", rest[1:], nil, "player")...)
		case n >= 2 && rest[0] == "remove":
			return do(steps(profiles.OpRemove, "Player %s is no longer an operator.", rest[1:], nil, "player")...)
		}
	case "gamemode":
		if n >= 2 {
			return do(steps(profiles.Gamemode, "Game mode of %s set.", rest[1:], map[string]string{"mode": rest[0]}, "player")...)
		}
	case "kick":
		if n >= 1 {
			return do(steps(profiles.Kick, "Player %s has been kicked.", rest, nil, "player")...)
		}
	case "say":
		if n >= 1 {
			return do(console.Step{Command: profiles.Say, Args: map[string]string{"message": strings.Join(rest, " ")},
				Done: "Message sent to players."})
		}
	case "time":
		if n == 2 && rest[0] == "set" {
			return do(console.Step{Command: profiles.TimeSet, Args: map[string]string{"time": rest[1]}, Done: "Time set to " + rest[1] + "."})
		}
		if n == 2 && rest[0] == "add" {
			return do(console.Step{Command: profiles.TimeAdd, Args: map[string]string{"time": rest[1]}, Done: "Time increased by " + rest[1] + "."})
		}
	case "toggledownfall":
		if n == 0 {
			return do(console.Step{Command: profiles.ToggleDownfall, Done: "Downfall toggled."})
		}
	case "give":
		if n >= 2 && n <= 4 {
			args := map[string]string{"player": rest[0], "item": rest[1]}
			amount := "x1"
			if n >= 3 {
				args["amount"], amount = rest[2], "x"+rest[2]
			}
			if n == 4 {
				args["damage"] = rest[3]
			}
			return do(console.Step{Command: profiles.Give, Args: args,
				Done: fmt.Sprintf("Given item %s %s to %s.", rest[1], amount, rest[0])})
		}
	case "xp":
		if n == 2 {
			return do(console.Step{Command: profiles.XP, Args: map[string]string{"player": rest[0], "amount": rest[1]},
				Done: fmt.Sprintf("Given %s experience to %s.", rest[1], rest[0])})
		}
	case "save":
		if n == 1 {
			switch rest[0] {
			case "on":
				return do(console.Step{Command: profiles.SaveOn, Doing: "Enabling level saving...", Done: "Done."})
			case "off":
				return do(console.Step{Command: profiles.SaveOff, Doing: "Disabling level saving...", Done: "Done.", Sync: true})
			case "all":
				return do(console.Step{Command: profiles.SaveAll, Doing: "Forcing save...", Done: "Done."})
			}
		}
	case "cmd":
		if n >= 1 {
			return func(ctx context.Context, m *console.Manager, name string, out console.Output) error {
				return m.Cmd(ctx, name, rest, out)
			}, true, nil
		}
	case "cmdlog":
		if n >= 1 {
			return func(ctx context.Context, m *console.Manager, name string, out console.Output) error {
				return m.Cmdlog(ctx, name, rest, out)
			}, false, nil
		}
	}
	return bad()
}

type doFunc func(...console.Step) (consoleOp, bool, error)

func planWhitelist(rest []string, do doFunc) (consoleOp, bool, error) {
	n := len(rest)
	switch {
	case n == 1 && (rest[0] == "on" || rest[0] == "off"):
		on := rest[0] == "on"
		return func(ctx context.Context, m *console.Manager, name string, out console.Output) error {
			return m.SetWhitelist(ctx, name, on, out)
		}, true, nil
	case n == 1 && rest[0] == "list":
		return func(_ context.Context, m *console.Manager, name string, out console.Output) error {
			return m.Whitelist(name, out)
		}, true, nil
	case n >= 2 && rest[0] == "add":
		return do(steps(profiles.WhitelistAdd, "Player %s is now whitelisted.", rest[1:], nil, "player")...)
	case n >= 2 && rest[0] == "remove":
		return do(steps(profiles.WhitelistRemove, "Player %s is no longer whitelisted.", rest[1:], nil, "player")...)
	}
	return nil, false, errNoSuchCommand
}

func planBlacklist(rest []string, do doFunc) (consoleOp, bool, error) {
	n := len(rest)
	switch {
	case n == 1 && rest[0] == "list":
		return func(_ context.Context, m *console.Manager, name string, out console.Output) error {
			return m.Blacklist(name, out)
		}, true, nil
	case n >= 3 && rest[0] == "player" && rest[1] == "add":
		return do(steps(profiles.BlacklistPlayerAdd, "Player %s is now blacklisted.", rest[2:], nil, "player")...)
	case n >= 3 && rest[0] == "player" && rest[1] == "remove":
		return do(steps(profiles.BlacklistPlayerRem, "Player %s is no longer blacklisted.", rest[2:], nil, "player")...)
	case n >= 3 && rest[0] == "ip" && rest[1] == "add":
		return do(steps(profiles.BlacklistIPAdd, "IP address %s is now blacklisted.", rest[2:], nil, "address")...)
	case n >= 3 && rest[0] == "ip" && rest[1] == "remove":
		return do(steps(profiles.BlacklistIPRemove, "IP address %s is no longer blacklisted.", rest[2:], nil, "address")...)
	}
	return nil, false, errNoSuchCommand
}
