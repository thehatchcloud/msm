package cli

import (
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/jars"
	"github.com/thehatchcloud/msm/internal/servers"
)

// exactArgs answers a wrong argument count with the legacy "No such
// command", as call_command does when no signature matches.
func exactArgs(n int) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != n {
			return errNoSuchCommand
		}
		return nil
	}
}

func newJargroupCommand(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jargroup",
		Short: "Manage groups of server JARs and their downloads",
		Args:  cobra.ArbitraryArgs,
		RunE:  func(*cobra.Command, []string) error { return errNoSuchCommand },
	}
	run := func(f func(cmd *cobra.Command, m *jars.Manager, args []string) error) func(*cobra.Command, []string) error {
		return func(cmd *cobra.Command, args []string) error {
			m, err := d.jars(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			return f(cmd, m, args)
		}
	}
	var yes bool
	del := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a jar group that no server uses",
		Long: `Delete a jar group and every JAR in it. A group that a server still uses
is refused. Confirm at the prompt, or pass --yes when standard input is not
a terminal.`,
		Args: exactArgs(1),
		RunE: run(func(cmd *cobra.Command, m *jars.Manager, args []string) error {
			refs, err := m.References(args[0])
			if err != nil {
				return err
			}
			if _, err := m.Target(args[0]); errors.Is(err, jars.ErrNotFound) {
				return causedError{fmt.Sprintf("there is no jar group with the name %q", args[0]), jars.ErrNotFound}
			}
			if len(refs) > 0 {
				return m.Delete(args[0]) // refuses, naming the servers
			}
			if !yes {
				ok, err := confirm(cmd, d, fmt.Sprintf("jar group %q", args[0]),
					"Are you sure you want to delete this jar group [y/N]: ", "Jar group was NOT deleted.")
				if err != nil || !ok {
					return err
				}
			}
			if err := m.Delete(args[0]); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "Jar group deleted.")
			return err
		}),
	}
	del.Flags().BoolVarP(&yes, "yes", "y", false, "delete without prompting (required when standard input is not a terminal)")

	cmd.AddCommand(
		&cobra.Command{
			Use:   "list",
			Short: "List the jar groups and their downloaded JARs",
			Args:  exactArgs(0),
			RunE: run(func(cmd *cobra.Command, m *jars.Manager, _ []string) error {
				groups, err := m.List()
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				for _, g := range groups {
					fmt.Fprintln(out, g.Name)
					for _, j := range g.JARs {
						fmt.Fprintf(out, "    %s\n", j)
					}
				}
				return nil
			}),
		},
		&cobra.Command{
			Use:   "create <name> <download-url>",
			Short: "Create a jar group that downloads from an https URL, minecraft or minecraft-snapshot",
			Args:  exactArgs(2),
			RunE: run(func(cmd *cobra.Command, m *jars.Manager, args []string) error {
				if err := m.Create(args[0], args[1]); err != nil {
					return err
				}
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "Creating jar group... Done.")
				return err
			}),
		},
		del,
		&cobra.Command{
			Use:   "rename <name> <new-name>",
			Short: "Rename a jar group and re-point the servers that use it",
			Args:  exactArgs(2),
			RunE: run(func(cmd *cobra.Command, m *jars.Manager, args []string) error {
				refs, err := m.Rename(args[0], args[1])
				if err != nil {
					return err
				}
				out := cmd.OutOrStdout()
				fmt.Fprintf(out, "Renamed jar group %q to %q.\n", args[0], args[1])
				for _, r := range refs {
					fmt.Fprintf(out, "Server %q is now using %q.\n", r.Server, r.JAR)
				}
				return nil
			}),
		},
		&cobra.Command{
			Use:   "changeurl <name> <download-url>",
			Short: "Change a jar group's download URL",
			Args:  exactArgs(2),
			RunE: run(func(cmd *cobra.Command, m *jars.Manager, args []string) error {
				if err := m.ChangeURL(args[0], args[1]); err != nil {
					return err
				}
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "Changing target URL... Done.")
				return err
			}),
		},
		&cobra.Command{
			Use:   "getlatest <name>",
			Short: "Download the newest JAR for a jar group",
			Args:  exactArgs(1),
			RunE: run(func(cmd *cobra.Command, m *jars.Manager, args []string) error {
				dl, err := m.GetLatest(cmd.Context(), args[0])
				if err != nil {
					return err
				}
				printDownload(cmd.OutOrStdout(), dl)
				return nil
			}),
		},
	)
	return cmd
}

// printDownload reports a getlatest result with the legacy wording.
func printDownload(w io.Writer, dl jars.Download) {
	fmt.Fprintln(w, "Downloading latest version... Done.")
	switch {
	case dl.Unchanged:
		fmt.Fprintf(w, "Existing version %q was already up to date.\n", dl.Path)
	case dl.Previous != "":
		fmt.Fprintf(w, "Downloaded version was different to previous latest. Saved as %q.\n", dl.Path)
	default:
		fmt.Fprintf(w, "Saved as %q.\n", dl.Path)
	}
}

// runJarCommand is msm <server> jar <jargroup> [<file>], or msm all jar
// ..., which selects the JAR for every server in turn.
func runJarCommand(cmd *cobra.Command, d deps, target string, args []string) error {
	if len(args) < 1 || len(args) > 2 {
		return errNoSuchCommand
	}
	group, file := args[0], ""
	if len(args) == 2 {
		file = args[1]
	}
	m, err := d.jars(cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	names := []string{target}
	if target == "all" {
		sm, err := d.servers(io.Discard)
		if err != nil {
			return err
		}
		if names, err = sm.Names(); err != nil {
			return err
		}
	}
	out := cmd.OutOrStdout()
	var failed []string
	var last error
	for _, name := range names {
		sel, err := m.Select(cmd.Context(), name, group, file)
		if sel.Download != nil {
			printDownload(out, *sel.Download)
		}
		switch {
		case errors.Is(err, servers.ErrNotFound) || errors.Is(err, servers.ErrInvalidName):
			return fmt.Errorf("there is no server with the name %q: %w", name, err)
		case err != nil:
			if target != "all" {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "msm: %s: %v\n", name, err)
			failed, last = append(failed, name), err
			continue
		}
		fmt.Fprintf(out, "Server %q is now using %q.\n", sel.Server, sel.JAR)
	}
	if len(failed) > 0 {
		return fmt.Errorf("jar: %d of %d servers failed: %v (last: %w)", len(failed), len(names), failed, last)
	}
	return nil
}
