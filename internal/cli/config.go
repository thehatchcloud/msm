package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/servers"
)

// noSuchServer is the message every command gives for a name that is not a
// server, and keeps the cause for the exit status.
func noSuchServer(target string, err error) error {
	if errors.Is(err, servers.ErrNotFound) || errors.Is(err, servers.ErrInvalidName) {
		return fmt.Errorf("there is no server with the name %q: %w", target, err)
	}
	return err
}

// hostGlobalConfig lists the global settings from the host's configuration:
// MSM_CONF or /etc/msm.conf when there is one, the rootless defaults
// otherwise.
func hostGlobalConfig(w io.Writer) ([]legacyconf.ConfigEntry, error) {
	cfg, js, err := hostConfig(w)
	if err != nil {
		return nil, err
	}
	resolved := map[string]string{
		"USERNAME": cfg.Owner.Username, "SERVER_STORAGE_PATH": cfg.Root, "JAR_STORAGE_PATH": js.root,
		"SERVER_PROPERTIES": cfg.PropertiesFile, "JARGROUP_TARGET": js.target, "JARGROUP_DOWNLOAD_DIR": js.downloadDir,
	}
	return legacyconf.GlobalEntries(cfg.Global, resolved), nil
}

// newConfigCommand is msm config: the global settings the manager uses.
func newConfigCommand(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "config",
		Short: "List the global settings MSM uses",
		Long: `List every global setting, then every DEFAULT_ setting that a server's own
msm-* properties override, as NAME="value" lines. A value is what msm.conf
sets, or else the default. The data directories of an installation with no
msm.conf are the ones msm detected for the invoking user.

With --format json the same settings are printed with the place each value
came from (msm.conf, default or detected). To read or change one server's
settings, use "msm <server> config".`,
		Args:        exactArgs(0),
		Annotations: map[string]string{annJSON: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			entries, err := d.globalConfig(cmd.ErrOrStderr())
			if err != nil {
				return err
			}
			return writeGlobalConfig(cmd.OutOrStdout(), entries, jsonRequested(cmd))
		},
	}
}

// runServerConfig is msm <server|all> config [<setting> [<value>]].
func runServerConfig(cmd *cobra.Command, d deps, target string, rest []string) error {
	if len(rest) > 2 {
		return errNoSuchCommand
	}
	m, err := d.servers(cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	one := func(name string, w io.Writer) error {
		var err error
		switch len(rest) {
		case 0:
			err = listServerConfig(cmd, m, name, w)
		case 1:
			var v string
			if v, err = m.ConfigGet(name, rest[0]); err == nil {
				_, err = fmt.Fprintln(w, v)
			}
		default:
			var c *servers.ConfigChange
			if c, err = m.ConfigSet(cmd.Context(), name, rest[0], rest[1]); err == nil {
				err = reportConfigChange(w, name, c)
			}
		}
		return noSuchServer(name, err)
	}
	if target != "all" {
		return one(target, out)
	}
	names, err := m.Names()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		_, err = fmt.Fprintln(out, "[There are no servers]")
		return err
	}
	var failed []error
	for _, name := range names {
		if err := one(name, &prefixWriter{w: out, prefix: name + ": "}); err != nil {
			failed = append(failed, fmt.Errorf("server %q: %w", name, err))
		}
	}
	return errors.Join(failed...)
}

func listServerConfig(cmd *cobra.Command, m *servers.Manager, name string, w io.Writer) error {
	list, warnings, err := m.ConfigList(name)
	if err != nil {
		return err
	}
	warn(cmd.ErrOrStderr(), warnings)
	if jsonRequested(cmd) {
		doc := serverConfigDocument{SchemaVersion: SchemaVersion, Server: name, Settings: make([]serverSetting, 0, len(list))}
		for _, c := range list {
			doc.Settings = append(doc.Settings, serverSetting{c.Key, c.Value, c.Source.String()})
		}
		return writeJSON(w, doc)
	}
	for _, c := range list {
		if _, err := fmt.Fprintf(w, "%s=\"%s\"\n", c.Key, c.Value); err != nil {
			return err
		}
	}
	return nil
}

// reportConfigChange says what was written and, plainly, when it takes
// effect, so a restart that is still due is never a surprise.
func reportConfigChange(w io.Writer, name string, c *servers.ConfigChange) error {
	var b strings.Builder
	if c.Existed {
		fmt.Fprintf(&b, "Changed %s from %q to %q in %s.\n", c.Key, c.Old, c.New, c.File)
	} else {
		fmt.Fprintf(&b, "Added %s=%q to %s.\n", c.Key, c.New, c.File)
	}
	switch c.Effect {
	case servers.Immediate:
		b.WriteString("The change applies to the next msm command; no restart is needed.\n")
	default:
		switch c.State.Kind {
		case servers.Stopped:
			b.WriteString("The server is stopped; the change takes effect the next time it starts.\n")
		case servers.Running:
			fmt.Fprintf(&b, "Restart required: server %q is running and keeps the old value until it restarts: msm %s restart\n", name, name)
		default:
			fmt.Fprintf(&b, "Restart required if server %q is running (the manager could not tell: %s): msm %s restart\n", name, c.State.Detail, name)
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// prefixWriter puts a prefix before every line written through it, so the
// output of one server in a bulk command is attributable.
type prefixWriter struct {
	w      io.Writer
	prefix string
	mid    bool
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	for _, line := range strings.SplitAfter(string(b), "\n") {
		if line == "" {
			continue
		}
		if !p.mid {
			if _, err := io.WriteString(p.w, p.prefix); err != nil {
				return 0, err
			}
		}
		if _, err := io.WriteString(p.w, line); err != nil {
			return 0, err
		}
		p.mid = !strings.HasSuffix(line, "\n")
	}
	return len(b), nil
}

// newUpdateCommand is msm update [--noinput]. The self-updater is replaced
// by verified release artifacts in P15 (DEV-006); until then the command is
// recognised and says so, rather than being "No such command".
func newUpdateCommand() *cobra.Command {
	var noinput bool
	cmd := &cobra.Command{
		Use:   "update [--noinput]",
		Short: "Replace MSM with the latest release (not yet available)",
		Long: `The legacy update downloaded scripts with certificate checks disabled and
replaced them in place. The Go port will update only from a verified release
artifact for its platform (deviation DEV-006, task P15). Until then this
command changes nothing and exits with status 6; install a new release of msm
yourself.`,
		Args: exactArgs(0),
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("update is %w: self-update is replaced by verified release artifacts in a later task (DEV-006); nothing was changed, so install a new release of msm yourself", errUnavailable)
		},
	}
	cmd.Flags().BoolVar(&noinput, "noinput", false, "answer yes to the update prompt (accepted for compatibility)")
	return cmd
}
