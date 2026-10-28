package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/buildinfo"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/servers"
)

// SchemaVersion is the version of every JSON document `--format json`
// prints. A change that removes or renames a field, or changes what one
// means, increments it; adding a field does not (docs/cli.md).
const SchemaVersion = 1

const (
	formatText = "text"
	formatJSON = "json"
)

// annJSON marks a command that has a JSON form.
const annJSON = "msm-json"

// jsonRequested reports whether the user asked for --format json.
func jsonRequested(cmd *cobra.Command) bool {
	f, err := cmd.Flags().GetString("format")
	return err == nil && f == formatJSON
}

// checkFormat validates --format and refuses json where a command has none,
// instead of printing text a program would then fail to parse. args are the
// arguments of the command line after the program name.
func checkFormat(cmd *cobra.Command, args []string) error {
	f, err := cmd.Flags().GetString("format")
	if err != nil || f == formatText {
		return nil
	}
	if f != formatJSON {
		return usageError{fmt.Errorf("--format must be %q or %q, not %q", formatText, formatJSON, f)}
	}
	if cmd.Annotations[annJSON] == "true" || cmd.Parent() == nil && serverJSONSupported(args) {
		return nil
	}
	return usageError{fmt.Errorf("--format json is not supported by this command; it applies to: version, config, <server> config (listing), server list, <server> status")}
}

// serverJSONSupported is whether a server-first command line has a JSON
// form: <server|all> status, and <server> config with no setting named.
func serverJSONSupported(args []string) bool {
	switch {
	case len(args) == 2 && args[1] == "status":
		return true
	case len(args) == 2 && args[1] == "config" && args[0] != "all":
		return true
	}
	return false
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// serverStatus is one server in the status document.
type serverStatus struct {
	Name string `json:"name"`
	// Active is whether the server is meant to be running (its active marker).
	Active bool `json:"active"`
	// State is what the manager can prove: running, stopped, occupied (a
	// screen session holds the name but is not verifiably this server) or
	// unknown (the sessions could not be inspected).
	State string `json:"state"`
	// OK is true when State is running for an active server, or stopped for
	// an inactive one: nothing needs attention.
	OK bool `json:"ok"`
	// Detail explains an occupied or unknown state.
	Detail string `json:"detail,omitempty"`
}

// statusDocument is the JSON of `status` and `server list` (schema version 1).
type statusDocument struct {
	SchemaVersion int            `json:"schema_version"`
	Servers       []serverStatus `json:"servers"`
}

func statusOf(e servers.Entry) serverStatus {
	st := serverStatus{Name: e.Name, Active: e.Active, State: e.State.Kind.String(), Detail: e.State.Detail}
	switch e.State.Kind {
	case servers.Running:
		st.OK = e.Active
	case servers.Stopped:
		st.OK = !e.Active
	}
	if e.State.Kind == servers.Running || e.State.Kind == servers.Stopped {
		st.Detail = ""
	}
	return st
}

func writeStatus(w io.Writer, entries []servers.Entry) error {
	doc := statusDocument{SchemaVersion: SchemaVersion, Servers: make([]serverStatus, 0, len(entries))}
	for _, e := range entries {
		doc.Servers = append(doc.Servers, statusOf(e))
	}
	return writeJSON(w, doc)
}

// runStatusJSON is `msm <server|all> status --format json`.
func runStatusJSON(cmd *cobra.Command, d deps, target string) error {
	m, err := d.servers(cmd.ErrOrStderr())
	if err != nil {
		return err
	}
	var entries []servers.Entry
	if target == "all" {
		var warnings []string
		if entries, warnings, err = m.List(cmd.Context()); err != nil {
			return err
		}
		warn(cmd.ErrOrStderr(), warnings)
	} else {
		e, err := m.Entry(cmd.Context(), target)
		if err != nil {
			return noSuchServer(target, err)
		}
		entries = []servers.Entry{e}
	}
	return writeStatus(cmd.OutOrStdout(), entries)
}

type versionDocument struct {
	SchemaVersion int    `json:"schema_version"`
	Version       string `json:"version"`
	Commit        string `json:"commit"`
}

func writeVersionJSON(w io.Writer, info buildinfo.Info) error {
	return writeJSON(w, versionDocument{SchemaVersion, info.Version, info.Commit})
}

type globalSetting struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	// Source is msm.conf, default or detected.
	Source string `json:"source"`
}

type globalConfigDocument struct {
	SchemaVersion int             `json:"schema_version"`
	Settings      []globalSetting `json:"settings"`
}

type serverSetting struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	// Source is default, msm.conf, version profile or server.properties.
	Source string `json:"source"`
}

type serverConfigDocument struct {
	SchemaVersion int             `json:"schema_version"`
	Server        string          `json:"server"`
	Settings      []serverSetting `json:"settings"`
}

func writeGlobalConfig(w io.Writer, entries []legacyconf.ConfigEntry, asJSON bool) error {
	if asJSON {
		doc := globalConfigDocument{SchemaVersion: SchemaVersion, Settings: make([]globalSetting, 0, len(entries))}
		for _, e := range entries {
			doc.Settings = append(doc.Settings, globalSetting{e.Name, e.Value, e.Source})
		}
		return writeJSON(w, doc)
	}
	var b strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&b, "%s=\"%s\"\n", e.Name, e.Value)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
