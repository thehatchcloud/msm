package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/clock"
	"github.com/thehatchcloud/msm/internal/config"
	"github.com/thehatchcloud/msm/internal/console"
	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/jars"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/lifecycle"
	"github.com/thehatchcloud/msm/internal/process"
	"github.com/thehatchcloud/msm/internal/screen"
	"github.com/thehatchcloud/msm/internal/servers"
	"github.com/thehatchcloud/msm/internal/worlds"
)

// systemMSMConf is the legacy global configuration file.
const systemMSMConf = "/etc/msm.conf"

// deps are the command tree's connections to the host, replaced in tests.
type deps struct {
	// isTerminal reports whether r is an interactive terminal.
	isTerminal func(r io.Reader) bool
	// servers builds the server manager, writing import warnings to w.
	servers func(w io.Writer) (*servers.Manager, error)
	// lifecycle builds the lifecycle manager over servers.
	lifecycle func(w io.Writer, o lifecycleOptions) (*lifecycle.Manager, error)
	// console builds the console manager over servers.
	console func(w io.Writer, o lifecycleOptions) (*console.Manager, error)
	// stdio is the terminal a console attach connects.
	stdio func() process.Stdio
	// jars builds the JAR store manager.
	jars func(w io.Writer) (*jars.Manager, error)
	// worlds builds the world manager over servers.
	worlds func(w io.Writer) (*worlds.Manager, error)
	// program is how the legacy messages name the executable ($0).
	program string
}

func defaultDeps() deps {
	d := deps{isTerminal: readerIsTerminal, servers: hostServers, jars: hostJars, program: programPath(os.Args[0])}
	d.worlds = hostWorlds(d)
	d.lifecycle = hostLifecycle(d)
	d.console = hostConsole(d)
	d.stdio = hostStdio
	return d
}

func readerIsTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	return ok && isTerminal(f)
}

// hostServers configures the manager from the host: the legacy msm.conf
// (MSM_CONF, else /etc/msm.conf) when there is one, otherwise the default
// layout, which is rootless for an unprivileged user.
func hostServers(w io.Writer) (*servers.Manager, error) {
	cfg, _, err := hostConfig(w)
	if err != nil {
		return nil, err
	}
	cfg.Prober = hostProber()
	return servers.New(cfg)
}

// jarStore is where the host keeps JAR groups.
type jarStore struct {
	root, target, downloadDir string
	createRoot                bool
}

func hostConfig(w io.Writer) (servers.Config, jarStore, error) {
	cfg := servers.Config{PropertiesFile: legacyconf.DefaultServerPropertiesFile}
	js := jarStore{target: legacyconf.DefaultJargroupTarget, downloadDir: legacyconf.DefaultJargroupDownloadDir}
	euid := os.Geteuid()
	if path, ok := legacyconf.Discover("", os.Getenv("MSM_CONF"), systemMSMConf); ok {
		f, err := legacyconf.Load(path)
		if err != nil {
			return cfg, js, err
		}
		g := legacyconf.Global(f)
		for _, warning := range g.Warnings {
			fmt.Fprintf(w, "msm: warning: %s\n", warning)
		}
		owner, err := identity.Lookup(g.Username)
		if err != nil {
			return cfg, js, fmt.Errorf("manager user USERNAME=%s from %s: %w", g.Username, path, err)
		}
		cfg.Root, cfg.PropertiesFile, cfg.Global, cfg.Owner = g.ServerStoragePath, g.ServerPropertiesFile, f, owner
		js.root, js.target, js.downloadDir = g.JarStoragePath, g.JargroupTarget, g.JargroupDownloadDir
	} else {
		roots, err := config.ResolveDataRoots(nil, euid)
		if err != nil {
			return cfg, js, err
		}
		cfg.Root, js.root = roots.ServerStoragePath, roots.JarStoragePath
		if euid == 0 {
			if cfg.Owner, err = identity.Lookup(legacyconf.DefaultUsername); err != nil {
				return cfg, js, fmt.Errorf("default manager user: %w", err)
			}
		} else {
			if cfg.Owner, err = identity.Current(); err != nil {
				return cfg, js, err
			}
			// Rootless: servers without a configured owner are the
			// invoking user's, in data directories made on demand.
			cfg.DefaultOwner, cfg.CreateRoot, js.createRoot = &cfg.Owner, true, true
		}
	}
	return cfg, js, nil
}

// hostJars configures the JAR store from the same host configuration.
func hostJars(w io.Writer) (*jars.Manager, error) {
	cfg, js, err := hostConfig(w)
	if err != nil {
		return nil, err
	}
	cfg.Prober = hostProber()
	sm, err := servers.New(cfg)
	if err != nil {
		return nil, err
	}
	return jars.New(jars.Config{Root: js.root, Target: js.target, DownloadDir: js.downloadDir,
		Owner: cfg.Owner, Servers: sm, CreateRoot: js.createRoot, Clock: clock.Real{}})
}

func hostProber() servers.ScreenProber {
	p := servers.ScreenProber{
		Runner: process.ExecRunner{}, Terminal: process.ExecRunner{},
		Processes: screen.SystemProcesses{}, Clock: clock.Real{},
		Env: []string{"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8"},
	}
	if dir := os.Getenv("SCREENDIR"); dir != "" {
		p.Env = append(p.Env, "SCREENDIR="+dir)
	}
	// Without screen, no server can be running in a screen session.
	if path, err := exec.LookPath("screen"); err == nil {
		if abs, err := filepath.Abs(path); err == nil {
			p.Screen = abs
		}
	}
	return p
}

func newServerCommand(d deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "server",
		Short: "List, create, rename and delete servers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(
		newServerListCommand(d),
		newServerCreateCommand(d),
		newServerDeleteCommand(d),
		newServerRenameCommand(d),
	)
	return cmd
}

func warn(w io.Writer, warnings []string) {
	for _, warning := range warnings {
		fmt.Fprintf(w, "msm: warning: %s\n", warning)
	}
}

func newServerListCommand(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List servers with their active state and whether they are running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			m, err := d.servers(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			list, warnings, err := m.List(cmd.Context())
			if err != nil {
				return err
			}
			warn(cmd.ErrOrStderr(), warnings)
			out := cmd.OutOrStdout()
			if len(list) == 0 {
				_, err = fmt.Fprintln(out, "[There are no servers]")
				return err
			}
			for _, s := range list {
				if _, err := fmt.Fprintln(out, listLine(s)); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

// listLine keeps the legacy server list wording, and says plainly when the
// manager cannot tell whether a server is running.
func listLine(s servers.Entry) string {
	intent := "[INACTIVE]"
	if s.Active {
		intent = "[ ACTIVE ]"
	}
	var status string
	switch {
	case s.State.Kind == servers.Occupied:
		status = "has a screen session that is not verifiably this server: " + s.State.Detail
	case s.State.Kind == servers.Unknown:
		status = "is in an unknown state: " + s.State.Detail
	case s.Active && s.State.Kind == servers.Running:
		status = "is running. Everything is OK."
	case s.Active:
		status = "is stopped. Server is down!"
	case s.State.Kind == servers.Running:
		status = "is running. It should not be running!"
	default:
		status = "is stopped. Everything is OK."
	}
	return fmt.Sprintf("%s %q %s", intent, s.Name, status)
}

func newServerCreateCommand(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "create <name>",
		Short: "Create a new server",
		Long: `Create a new server directory with empty player lists, an empty
properties file and a world storage directory.

The Minecraft EULA is never accepted on your behalf: after choosing a server
JAR, read the EULA and set eula=true in the server's eula.txt yourself.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := d.servers(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			c, err := m.Create(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			warn(cmd.ErrOrStderr(), c.Warnings)
			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"Created server %q in %s.\nBefore starting it, choose a server JAR and accept the Minecraft EULA in %s.\n",
				c.Name, c.Dir, filepath.Join(c.Dir, "eula.txt"))
			return err
		},
	}
}

func newServerRenameCommand(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "rename <name> <new-name>",
		Short: "Rename a stopped server",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := d.servers(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			r, err := m.Rename(cmd.Context(), args[0], args[1])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Renamed server %q to %q.\n", r.From, r.To)
			for _, note := range r.Notes {
				fmt.Fprintf(out, "Note: %s.\n", note)
			}
			return nil
		},
	}
}

// confirmAnswer is the legacy delete prompt's accepted answer.
var confirmAnswer = regexp.MustCompile(`^(y|Y|yes)$`)

func newServerDeleteCommand(d deps) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a stopped server and its worlds",
		Long: `Delete a stopped server and its worlds after showing what will be removed.

Confirm at the prompt, or pass --yes when standard input is not a terminal.
Backups, archived logs and the shared JAR store are not touched. Symbolic
links inside the server are removed; their targets are kept. A running
server is refused: stop it first.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := d.servers(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			p, err := m.PlanDelete(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			printPlan(out, p)
			if p.State.Kind != servers.Stopped {
				return fmt.Errorf("server %q is %s (%s); stop it before deleting it", p.Name, p.State.Kind, p.State.Detail)
			}
			if !yes {
				ok, err := confirm(cmd, d, fmt.Sprintf("server %q", p.Name),
					fmt.Sprintf("Are you sure you want to delete server %q and its worlds? (note: backups are preserved) [y/N]: ", p.Name),
					"Server was NOT deleted.")
				if err != nil || !ok {
					return err
				}
			}
			if err := m.Delete(cmd.Context(), p); err != nil {
				return err
			}
			_, err = fmt.Fprintln(out, "Server deleted.")
			return err
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "delete without prompting (required when standard input is not a terminal)")
	return cmd
}

func printPlan(w io.Writer, p *servers.DeletePlan) {
	fmt.Fprintf(w, "Server %q will be deleted:\n", p.Name)
	fmt.Fprintf(w, "  Directory: %s\n", p.Dir)
	fmt.Fprintf(w, "  Contents: %d files, %s\n", p.Files, humanBytes(p.Bytes))
	if len(p.Entries) > 0 {
		fmt.Fprintf(w, "  Top level: %s\n", strings.Join(p.Entries, ", "))
	}
	if len(p.ExternalLinks) > 0 {
		fmt.Fprintln(w, "  Symbolic links to outside the server (the links are removed, their targets are kept):")
		for _, l := range p.ExternalLinks {
			fmt.Fprintf(w, "    %s\n", l)
		}
	}
	fmt.Fprintln(w, "Backups, archived logs and the shared JAR store are not affected.")
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// errNeedsConfirmation refuses a delete that nobody can confirm, instead of
// waiting on a pipe or treating end-of-input as an answer.
var errNeedsConfirmation = errors.New("standard input is not a terminal; pass --yes to confirm")

// confirm asks a legacy delete question on a terminal; what names the
// thing for the refusal ("server %q"). Anything but y, Y or yes declines,
// which is not an error: the declined line is printed instead.
func confirm(cmd *cobra.Command, d deps, what, question, declined string) (bool, error) {
	in := cmd.InOrStdin()
	if !d.isTerminal(in) {
		return false, fmt.Errorf("refusing to delete %s without confirmation: %w", what, errNeedsConfirmation)
	}
	fmt.Fprint(cmd.OutOrStdout(), question)
	answer, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	if !confirmAnswer.MatchString(strings.TrimRight(answer, "\r\n")) {
		_, err := fmt.Fprintln(cmd.OutOrStdout(), declined)
		return false, err
	}
	return true, nil
}
