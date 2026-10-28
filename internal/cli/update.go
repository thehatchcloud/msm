package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/buildinfo"
	updater "github.com/thehatchcloud/msm/internal/update"
)

// updateEnv is what msm update needs from the host, replaceable in tests.
type updateEnv struct {
	source       func(info buildinfo.Info) (updater.Source, error)
	executable   func() (string, error)
	goos, goarch string
	// check runs on the staged program; nil uses update.SmokeCheck.
	check func(staged string, rel updater.Release) error
}

func defaultUpdateEnv() updateEnv {
	return updateEnv{
		source: func(info buildinfo.Info) (updater.Source, error) {
			if info.UpdateURL == "" {
				return nil, errors.New("this build has no release location")
			}
			return updater.NewHTTPSource(info.UpdateURL, nil)
		},
		executable: os.Executable,
		goos:       runtime.GOOS, goarch: runtime.GOARCH,
	}
}

// errUpdateNeedsConfirmation refuses an update nobody can confirm.
var errUpdateNeedsConfirmation = causedError{
	"standard input is not a terminal; pass --noinput to confirm the update",
	errNeedsConfirmation,
}

// newUpdateCommand is msm update [--noinput]: replace this program with the
// latest release whose manifest is signed by a key compiled into it
// (DEV-006). It never fetches or runs a script.
func newUpdateCommand(info buildinfo.Info, d deps) *cobra.Command {
	var noinput, checkOnly, rollback bool
	cmd := &cobra.Command{
		Use:   "update [--noinput]",
		Short: "Replace msm with the latest signed release for this platform",
		Long: `Download the release manifest and its signature from the release location
compiled into this build, verify the signature against the release keys
compiled into this build, select the archive for exactly this operating
system and architecture, verify its size and SHA-256 against the signed
manifest, check that the archive's own checksums and release identifiers
match, stage the new program next to the old one, run its "version" command,
and only then atomically rename it into place. The previous program is kept
as <msm>.previous; "msm update --rollback" restores it.

Only the msm executable is replaced. Configuration, servers, worlds and
backups are not read or written. No shell, downloaded script or remote code
runs before the signature has been verified.

It asks first on a terminal ([y/N]); without a terminal it refuses unless
--noinput is given. --check only reports whether a newer release exists.

It refuses to touch an executable that a package manager owns (it tells you
to update through that), a development build (which has no signing key), a
signed release older than the installed version, and an update that another
msm update is running. A symlinked msm is followed: the real file is replaced
and the link is kept.`,
		Args: exactArgs(0),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUpdate(cmd, d, info, noinput, checkOnly, rollback)
		},
	}
	cmd.Flags().BoolVar(&noinput, "noinput", false, "answer yes to the update prompt (accepted for compatibility)")
	cmd.Flags().BoolVar(&checkOnly, "check", false, "only report whether a newer signed release exists; change nothing")
	cmd.Flags().BoolVar(&rollback, "rollback", false, "restore the program kept by the last update")
	return cmd
}

func runUpdate(cmd *cobra.Command, d deps, info buildinfo.Info, noinput, checkOnly, rollback bool) error {
	env := d.update
	if env.executable == nil {
		env = defaultUpdateEnv()
	}
	out := cmd.OutOrStdout()
	if checkOnly && rollback {
		return usageError{errors.New("--check and --rollback cannot be combined")}
	}
	exe, err := env.executable()
	if err != nil {
		return fmt.Errorf("find the msm executable: %w", err)
	}
	if rollback {
		t, err := updater.ResolveTarget(exe)
		if err != nil {
			return err
		}
		if err := t.Rollback(); err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "Restored the previous msm at %s.\nThe program it replaced is kept as %s; run msm update --rollback again to undo this.\n", t.Path, t.PreviousPath())
		return err
	}
	if len(info.UpdateKeys) == 0 {
		return fmt.Errorf("update is %w: this build has no trusted release signing key (it is a development build); nothing was changed, so install a release build of msm", errUnavailable)
	}
	keys, err := updater.ParseKeys(info.UpdateKeys)
	if err != nil {
		return fmt.Errorf("%w: %v", errUnavailable, err)
	}
	src, err := env.source(info)
	if err != nil {
		return fmt.Errorf("update is %w: %v", errUnavailable, err)
	}
	opts := updater.Options{
		Keys: keys, Source: src, Current: info.Version, GOOS: env.goos, GOARCH: env.goarch,
		Exe: exe, CheckOnly: checkOnly, Check: env.check,
		Confirm: func(p updater.Plan) (bool, error) {
			if noinput {
				return true, nil
			}
			return confirmUpdate(cmd, d, p)
		},
	}
	res, err := updater.Run(cmd.Context(), opts)
	if err != nil {
		return updateError(err)
	}
	switch res.Status {
	case updater.UpToDate:
		_, err = fmt.Fprintf(out, "msm %s is up to date.\n", res.Plan.To)
	case updater.Available:
		_, err = fmt.Fprintf(out, "msm %s is available (installed %s, commit %s). Run msm update to install it.\n", res.Plan.To, res.Plan.From, res.Plan.Commit)
	case updater.Declined:
		_, err = fmt.Fprintln(out, "Update declined; nothing was changed.")
	default:
		_, err = fmt.Fprintf(out, "Updated msm from %s to %s (commit %s) at %s.\nThe previous program is kept as %s; msm update --rollback restores it.\nConfiguration, servers and worlds were not touched.\n",
			res.Plan.From, res.Plan.To, res.Plan.Commit, res.Plan.Target, res.Previous)
	}
	return err
}

func updateError(err error) error {
	if errors.Is(err, updater.ErrBadSignature) {
		return fmt.Errorf("%w; nothing was changed. The release location or the network path may be serving a forged or damaged release", err)
	}
	return err
}

// confirmUpdate asks the update question on a terminal; without one it
// refuses rather than guessing.
func confirmUpdate(cmd *cobra.Command, d deps, p updater.Plan) (bool, error) {
	in := cmd.InOrStdin()
	if !d.isTerminal(in) {
		return false, errUpdateNeedsConfirmation
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Verified release %s (commit %s), signed by a trusted key. Replace msm %s at %s? [y/N] ", p.To, p.Commit, p.From, p.Target)
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	return confirmAnswer.MatchString(strings.TrimRight(answer, "\r\n")), nil
}
