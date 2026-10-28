package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/backup"
	"github.com/thehatchcloud/msm/internal/console"
	"github.com/thehatchcloud/msm/internal/filelock"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/install"
	"github.com/thehatchcloud/msm/internal/jars"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/lifecycle"
	"github.com/thehatchcloud/msm/internal/maintenance"
	"github.com/thehatchcloud/msm/internal/profiles"
	"github.com/thehatchcloud/msm/internal/screen"
	"github.com/thehatchcloud/msm/internal/serverprops"
	"github.com/thehatchcloud/msm/internal/servers"
	updater "github.com/thehatchcloud/msm/internal/update"
	"github.com/thehatchcloud/msm/internal/worlds"
)

// The exit statuses of msm (DEV-004). The legacy manager exited 0 after
// nearly everything, and its test suite named 64 to 73 for failures its
// source never produced. These replace both; docs/cli.md lists them.
const (
	// ExitOK: the command did what was asked.
	ExitOK = 0
	// ExitError: the command failed (including when some of the servers a
	// bulk command handled failed); stderr says what and why.
	ExitError = 1
	// ExitUsage: the words typed match no command, or an argument or flag
	// is missing, extra or malformed. Nothing was done.
	ExitUsage = 2
	// ExitConfig: a configuration file, a setting or its value cannot be
	// used. Nothing was done.
	ExitConfig = 3
	// ExitNotFound: the server, world, JAR group or property named does not
	// exist. Nothing was done.
	ExitNotFound = 4
	// ExitRefused: the manager declined for safety: a confirmation is
	// missing, the server is not proven stopped, or another command holds
	// the lock. Nothing was done.
	ExitRefused = 5
	// ExitUnavailable: a program, feature or version the command needs
	// is not available here, or the command is not in this build.
	ExitUnavailable = 6
	// ExitInterrupted: the command was stopped by Ctrl+C or a termination
	// request (the shell convention 128 + SIGINT).
	ExitInterrupted = 130
)

// ExitName is the stable name of an exit status, as docs/cli.md lists it.
func ExitName(code int) string {
	switch code {
	case ExitOK:
		return "ok"
	case ExitError:
		return "error"
	case ExitUsage:
		return "usage"
	case ExitConfig:
		return "config"
	case ExitNotFound:
		return "not-found"
	case ExitRefused:
		return "refused"
	case ExitUnavailable:
		return "unavailable"
	case ExitInterrupted:
		return "interrupted"
	}
	return "unknown"
}

// errUnavailable marks a command that exists but is not in this build.
var errUnavailable = errors.New("not available in this build")

// usageError marks a failure to match the command grammar.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }
func (e usageError) Unwrap() error { return e.err }

// exitCode maps an error to the exit status that names its cause. Causes
// are checked from the most specific to the generic ExitError, so an
// aggregate of several failures reports the first cause in this order.
func exitCode(err error) int {
	if err == nil {
		return ExitOK
	}
	var syntax *legacyconf.SyntaxError
	var uerr usageError
	is := func(targets ...error) bool {
		for _, t := range targets {
			if errors.Is(err, t) {
				return true
			}
		}
		return false
	}
	switch {
	case is(context.Canceled, lifecycle.ErrInterrupted):
		return ExitInterrupted
	case errors.As(err, &uerr), is(errNoSuchCommand, profiles.ErrBadArgument, screen.ErrUnsafeInput,
		console.ErrAmbiguous, serverprops.ErrUnsafeProperty, jars.ErrInvalidName, maintenance.ErrOptions):
		return ExitUsage
	case errors.As(err, &syntax), is(legacyconf.ErrUnsafeInvocation, legacyconf.ErrNoOwner, lifecycle.ErrSettings,
		backup.ErrSettings, backup.ErrDisabled, worlds.ErrRAMConfig, jars.ErrInvalidTarget, jars.ErrNoTarget,
		jars.ErrInsecureURL, install.ErrUnsafe, servers.ErrBadSetting, servers.ErrUnknownSetting, servers.ErrNoProperties):
		return ExitConfig
	case is(servers.ErrNotFound, servers.ErrInvalidName, jars.ErrNotFound, jars.ErrNoJAR, worlds.ErrNoWorld, servers.ErrNoProperty):
		return ExitNotFound
	case is(errNeedsConfirmation, servers.ErrExists, servers.ErrCaseCollision, jars.ErrExists, servers.ErrRunning, servers.ErrNotStopped, servers.ErrChanged,
		filelock.ErrBusy, identity.ErrPrivilegeRequired, jars.ErrInUse, backup.ErrExternalLinks,
		backup.ErrNotProven, maintenance.ErrNotProven, lifecycle.ErrNotProven, console.ErrNotProven, worlds.ErrConflict,
		install.ErrExists, updater.ErrPackageManaged, updater.ErrNotReplaceable, updater.ErrPermission, updater.ErrBusy,
		updater.ErrNoRollback, updater.ErrDowngrade):
		return ExitRefused
	case is(errUnavailable, profiles.ErrUnsupported, worlds.ErrRAMUnsupported, backup.ErrToolMissing,
		backup.ErrToolVersion, lifecycle.ErrNoScreen, lifecycle.ErrNoJava, console.ErrNoScreen,
		screen.ErrUnsupportedVersion, updater.ErrNoTrustedKey, updater.ErrUnknownVersion, updater.ErrNoArtifact):
		return ExitUnavailable
	}
	return ExitError
}

// markUsage makes every argument-count and flag complaint of cmd and its
// subcommands a usage error, so Cobra's wording keeps its text and gains
// ExitUsage.
func markUsage(cmd *cobra.Command) {
	if args := cmd.Args; args != nil {
		cmd.Args = func(c *cobra.Command, a []string) error {
			if err := args(c, a); err != nil {
				if errors.Is(err, errNoSuchCommand) {
					return err
				}
				return usageError{err}
			}
			return nil
		}
	}
	for _, sub := range cmd.Commands() {
		markUsage(sub)
	}
}

// causedError prints msg and reports cause to errors.Is, so a message
// written for the user keeps the exit status of what went wrong.
type causedError struct {
	msg   string
	cause error
}

func (e causedError) Error() string { return e.msg }
func (e causedError) Unwrap() error { return e.cause }

func noSuchWorld(name string) error {
	return causedError{fmt.Sprintf("there is no world with the name %q", name), worlds.ErrNoWorld}
}
