package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/servers"
)

// Tab completion is Cobra's: `msm completion bash|zsh|fish|powershell`
// prints a script that calls `msm __complete` for every candidate. The
// legacy script sourced the init script into the interactive shell and
// listed directories with ls, cat and find; here the shell only runs the
// msm binary, which reads the same places in Go and prints names that pass
// completableName, so a file name, player name or JSON field cannot inject
// anything into the shell or the candidate list.

// completableName is what a candidate may look like. Tabs, newlines and
// other control characters would break Cobra's candidate/description
// lines, and shell metacharacters would reach the user's command line.
var completableName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:@+=,-]{0,127}$`)

// maxListBytes bounds the player lists completion reads.
const maxListBytes = 4 << 20

// serverCommandWords are the per-server command words, in the order help lists
// them. Aliases (wl, bl, op, gm, tdf) are accepted but not offered, as in
// the legacy script.
var serverCommandWords = []string{
	"start", "stop", "restart", "status", "connected", "worlds", "logroll", "backup", "jar",
	"whitelist", "blacklist", "operator", "gamemode", "kick", "say", "time", "toggledownfall",
	"give", "xp", "save", "cmd", "cmdlog", "console", "config",
}

var aliasOf = map[string]string{"wl": "whitelist", "bl": "blacklist", "op": "operator", "gm": "gamemode", "tdf": "toggledownfall"}

type completer struct {
	d deps
}

// completeServerFirst completes the server-first grammar the root command
// takes: msm <server|all> <command> ...
func completeServerFirst(d deps) cobra.CompletionFunc {
	c := completer{d}
	return func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return filter(c.candidates(args), toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

func filter(candidates []string, prefix string) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range candidates {
		if strings.HasPrefix(c, prefix) && !seen[c] && completableName.MatchString(c) {
			seen[c] = true
			out = append(out, c)
		}
	}
	return out
}

// serverNames lists the servers, or nothing when the configuration cannot
// be read: completion must stay silent, never print an error into a prompt.
func (c completer) serverNames() []string {
	m, err := c.d.servers(io.Discard)
	if err != nil {
		return nil
	}
	names, _ := m.Names()
	return names
}

func (c completer) candidates(args []string) []string {
	if len(args) == 0 {
		return append(c.serverNames(), "all")
	}
	target := args[0]
	if target != "all" && !contains(c.serverNames(), target) {
		return nil
	}
	if len(args) == 1 {
		return serverCommandWords
	}
	word := args[1]
	if full, ok := aliasOf[word]; ok {
		word = full
	}
	rest := args[2:]
	switch word {
	case "stop", "restart":
		if len(rest) == 0 {
			return []string{"now"}
		}
	case "worlds":
		return c.worlds(target, rest)
	case "jar":
		return c.jar(rest)
	case "whitelist":
		return c.whitelist(target, rest)
	case "blacklist":
		return c.blacklist(target, rest)
	case "operator":
		switch {
		case len(rest) == 0:
			return []string{"add", "remove", "list"}
		case rest[0] == "remove":
			return c.playerList(target, "OPS_PATH", "name")
		}
	case "gamemode":
		if len(rest) == 0 {
			return []string{"survival", "creative", "adventure", "spectator"}
		}
	case "time":
		if len(rest) == 0 {
			return []string{"set", "add"}
		}
	case "save":
		if len(rest) == 0 {
			return []string{"on", "off", "all"}
		}
	case "config":
		if len(rest) == 0 && target != "all" {
			return c.configKeys(target)
		}
	}
	return nil
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func (c completer) worlds(target string, rest []string) []string {
	if len(rest) == 0 {
		return []string{"list", "load", "ram", "todisk", "backup", "on", "off"}
	}
	if len(rest) != 1 || target == "all" {
		return nil
	}
	var wantActive bool
	switch rest[0] {
	case "ram", "off":
		wantActive = true
	case "on":
	default:
		return nil
	}
	sm, err := c.d.servers(io.Discard)
	if err != nil {
		return nil
	}
	s, err := sm.Settings(target)
	if err != nil {
		return nil
	}
	wm, err := c.d.worlds(io.Discard)
	if err != nil {
		return nil
	}
	inv, err := wm.Inventory(s)
	if err != nil {
		return nil
	}
	var names []string
	for _, w := range inv.Worlds {
		if w.Active == wantActive && !inv.Ambiguous[w.Name] {
			names = append(names, w.Name)
		}
	}
	return names
}

func (c completer) jar(rest []string) []string {
	jm, err := c.d.jars(io.Discard)
	if err != nil {
		return nil
	}
	groups, err := jm.List()
	if err != nil {
		return nil
	}
	var out []string
	for _, g := range groups {
		switch {
		case len(rest) == 0:
			out = append(out, g.Name)
		case len(rest) == 1 && g.Name == rest[0]:
			out = append(out, g.JARs...)
		}
	}
	return out
}

func (c completer) whitelist(target string, rest []string) []string {
	switch {
	case len(rest) == 0:
		return []string{"on", "off", "add", "remove", "list"}
	case rest[0] == "remove":
		return c.playerList(target, "WHITELIST_PATH", "name")
	}
	return nil
}

func (c completer) blacklist(target string, rest []string) []string {
	switch {
	case len(rest) == 0:
		return []string{"player", "ip", "list"}
	case len(rest) == 1 && (rest[0] == "player" || rest[0] == "ip"):
		return []string{"add", "remove"}
	case len(rest) >= 2 && rest[1] == "remove" && rest[0] == "player":
		return c.playerList(target, "BANNED_PLAYERS_PATH", "name")
	case len(rest) >= 2 && rest[1] == "remove" && rest[0] == "ip":
		return c.playerList(target, "BANNED_IPS_PATH", "ip")
	}
	return nil
}

// playerList reads the names in one of a server's JSON player lists
// (whitelist.json, banned-players.json, banned-ips.json, ops.json). The
// legacy script cat'ed the file; Minecraft 1.7.6 and later write a JSON
// array of objects, so this reads the field of each. A file that is not
// that, or that is too large, completes nothing.
func (c completer) playerList(target, setting, field string) []string {
	if target == "all" {
		return nil
	}
	sm, err := c.d.servers(io.Discard)
	if err != nil {
		return nil
	}
	s, err := sm.Settings(target)
	if err != nil {
		return nil
	}
	return jsonField(s.Get(setting), field)
}

// openList opens a list file for completion. Only a regular file is read: a
// FIFO or device put at the path would otherwise make every Tab wait.
func openList(path string) (*os.File, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	return os.Open(path)
}

func jsonField(path, field string) []string {
	f, err := openList(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxListBytes+1))
	if err != nil || len(data) > maxListBytes {
		return nil
	}
	var entries []map[string]any
	if json.Unmarshal(data, &entries) != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if v, ok := e[field].(string); ok {
			out = append(out, v)
		}
	}
	sort.Strings(out)
	return out
}

// configKeys offers the msm-* settings and the properties the server's
// properties file already sets.
func (c completer) configKeys(target string) []string {
	sm, err := c.d.servers(io.Discard)
	if err != nil {
		return nil
	}
	list, _, err := sm.ConfigList(target)
	if err != nil {
		return nil
	}
	var out []string
	for _, s := range list {
		out = append(out, s.Key)
	}
	s, err := sm.Settings(target)
	if err != nil {
		return out
	}
	return append(out, propertyKeys(servers.PropertiesPath(s, sm.PropertiesFile()))...)
}

func propertyKeys(path string) []string {
	f, err := openList(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxListBytes))
	if err != nil {
		return nil
	}
	var keys []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' || line[0] == '!' {
			continue
		}
		if key, _, ok := strings.Cut(line, "="); ok && !strings.HasPrefix(strings.ToLower(key), "msm-") {
			keys = append(keys, key)
		}
	}
	return keys
}

// completeServerNames completes the first argument of a command that takes
// an existing server, as server delete and server rename do.
func completeServerNames(d deps, position int) cobra.CompletionFunc {
	c := completer{d}
	return func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) != position {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return filter(c.serverNames(), toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// completeJarGroups completes the first argument of the jargroup commands
// that name an existing group.
func completeJarGroups(d deps) cobra.CompletionFunc {
	c := completer{d}
	return func(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) != 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return filter(c.jar(nil), toComplete), cobra.ShellCompDirectiveNoFileComp
	}
}

// completeNow completes the optional "now" of the global stop and restart.
func completeNow(_ *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) == 0 && strings.HasPrefix("now", toComplete) {
		return []string{"now"}, cobra.ShellCompDirectiveNoFileComp
	}
	return nil, cobra.ShellCompDirectiveNoFileComp
}

// attachCompletions gives the commands that take an existing server or JAR
// group (or the optional "now") their candidates.
func attachCompletions(root *cobra.Command, d deps) {
	for _, c := range root.Commands() {
		switch c.Name() {
		case "stop", "restart":
			c.ValidArgsFunction = completeNow
		case "server":
			for _, sub := range c.Commands() {
				if sub.Name() == "delete" || sub.Name() == "rename" {
					sub.ValidArgsFunction = completeServerNames(d, 0)
				}
			}
		case "jargroup":
			for _, sub := range c.Commands() {
				switch sub.Name() {
				case "delete", "rename", "changeurl", "getlatest":
					sub.ValidArgsFunction = completeJarGroups(d)
				}
			}
		}
	}
}
