package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"

	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/safepath"
	"github.com/thehatchcloud/msm/internal/serverprops"
	"github.com/thehatchcloud/msm/internal/servers"
)

// Severity ranks a finding. A blocker stops apply; a warning is something
// the administrator should read; info records what was found.
type Severity string

const (
	Blocker Severity = "blocker"
	Warning Severity = "warning"
	Info    Severity = "info"
)

// Finding codes an administrator can acknowledge with an apply flag. Every
// other blocker must be fixed.
const (
	CodeUnsupportedSchedule = "unsupported-schedule"
)

// Finding is one thing the preflight noticed.
type Finding struct {
	Severity Severity `json:"severity"`
	Area     string   `json:"area"`
	Subject  string   `json:"subject,omitempty"`
	Code     string   `json:"code,omitempty"`
	Message  string   `json:"message"`
}

// WorldInfo is one world directory of a server.
type WorldInfo struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
	InRAM  bool   `json:"in_ram"`
}

// ServerInfo is one server directory.
type ServerInfo struct {
	Name       string      `json:"name"`
	Dir        string      `json:"dir"`
	Owner      string      `json:"owner"`
	Active     bool        `json:"active"`
	State      string      `json:"state"`
	Detail     string      `json:"detail,omitempty"`
	ScreenName string      `json:"screen_name"`
	Version    string      `json:"version"`
	Jar        string      `json:"jar,omitempty"`
	Worlds     []WorldInfo `json:"worlds"`
}

// JarGroupInfo is one directory of the JAR store.
type JarGroupInfo struct {
	Name   string `json:"name"`
	Target string `json:"target,omitempty"`
	JARs   int    `json:"jars"`
}

// ArchiveInfo is one archive root and what the settings say about it.
type ArchiveInfo struct {
	Setting string `json:"setting"`
	Path    string `json:"path"`
	Exists  bool   `json:"exists"`
	Entries int    `json:"entries"`
}

// Inventory is everything the preflight read.
type Inventory struct {
	Conf        string         `json:"conf"`
	ManagerUser string         `json:"manager_user"`
	InitVersion string         `json:"init_version,omitempty"`
	Servers     []ServerInfo   `json:"servers"`
	JarGroups   []JarGroupInfo `json:"jar_groups"`
	Archives    []ArchiveInfo  `json:"archives"`
	Cron        []CronEntry    `json:"cron"`
	Findings    []Finding      `json:"findings"`

	// ServerRoot and JarRoot are logical paths, kept for the verification
	// after apply.
	ServerRoot string `json:"server_root"`
	JarRoot    string `json:"jar_root"`
	// File and settings are kept for the steps that follow.
	file     *legacyconf.File
	settings legacyconf.GlobalSettings
}

// Blocked lists the blockers that are not acknowledged. acks holds the codes
// the administrator accepted.
func (inv *Inventory) Blocked(acks map[string]bool) []Finding {
	var out []Finding
	for _, f := range inv.Findings {
		if f.Severity == Blocker && !(f.Code != "" && acks[f.Code]) {
			out = append(out, f)
		}
	}
	return out
}

func (inv *Inventory) add(sev Severity, area, subject, code, format string, args ...any) {
	inv.Findings = append(inv.Findings, Finding{sev, area, subject, code, fmt.Sprintf(format, args...)})
}

// Env is what the preflight reads from the host.
type Env struct {
	Layout Layout
	// Prober reports whether each server is running. Nil means nothing can
	// be proven, so every server blocks.
	Prober servers.Prober
	// Lookup resolves a user; identity.Lookup in production.
	Lookup func(string) (identity.Identity, error)
}

var initVersion = regexp.MustCompile(`(?m)^VERSION="([^"]*)"`)

// Collect inventories the legacy installation. It only reads: it takes no
// lock, creates no file and runs nothing but the prober's read of the
// screen session list.
func Collect(ctx context.Context, env Env) (*Inventory, error) {
	l := env.Layout
	inv := &Inventory{Conf: l.Conf}
	conf := l.P(l.Conf)
	f, err := legacyconf.Load(conf)
	var syntax *legacyconf.SyntaxError
	switch {
	case errors.Is(err, fs.ErrNotExist):
		inv.add(Blocker, "config", conf, "", "there is no legacy configuration file here; nothing to migrate (a fresh installation needs \"msm install config\")")
		return inv, nil
	case errors.As(err, &syntax):
		inv.add(Blocker, "config", fmt.Sprintf("%s:%d", l.Conf, syntax.Line), "",
			"%s: %q. The Go port reads msm.conf as data and never evaluates shell; rewrite the assignment as a plain value", syntax.Reason, strings.TrimSpace(syntax.Raw))
		return inv, nil
	case err != nil:
		return nil, err
	}
	inv.file = f
	inv.settings = legacyconf.Global(f)
	g := inv.settings
	inv.ManagerUser = g.Username
	inv.ServerRoot, inv.JarRoot = g.ServerStoragePath, g.JarStoragePath
	for _, w := range g.Warnings {
		inv.add(Warning, "config", l.Conf, "", "%s", w)
	}
	unknown := []string{}
	for name := range f.Values {
		if !legacyconf.IsGlobalSetting(name) {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)
	for _, name := range unknown {
		inv.add(Warning, "config", l.Conf, "", "%s (line %d) is not a setting the manager reads and is ignored, by the legacy manager and the Go port alike", name, f.Values[name].Line)
	}

	for _, r := range []struct{ setting, path string }{
		{"SERVER_STORAGE_PATH", g.ServerStoragePath}, {"JAR_STORAGE_PATH", g.JarStoragePath},
	} {
		if err := safepath.ValidateConfiguredRoot(r.path); err != nil {
			inv.add(Blocker, "config", r.setting, "", "%v; the Go port needs an absolute, clean path", err)
			return inv, nil
		}
	}
	owner, ownerErr := lookup(env, g.Username)
	if ownerErr != nil {
		inv.add(Blocker, "users", g.Username, "", "the manager user USERNAME=%s does not exist: %v", g.Username, ownerErr)
	}

	inv.collectService(l)
	inv.collectServers(ctx, env, owner)
	inv.collectJars(l)
	inv.collectArchives(l)
	inv.collectSchedule(l)
	inv.collectCustomizations(l)

	sort.SliceStable(inv.Findings, func(i, j int) bool { return rank(inv.Findings[i].Severity) < rank(inv.Findings[j].Severity) })
	return inv, nil
}

func rank(s Severity) int {
	switch s {
	case Blocker:
		return 0
	case Warning:
		return 1
	}
	return 2
}

func lookup(env Env, name string) (identity.Identity, error) {
	if env.Lookup == nil {
		return identity.Lookup(name)
	}
	return env.Lookup(name)
}

func (inv *Inventory) collectService(l Layout) {
	if data, err := os.ReadFile(l.P(l.Init)); err == nil {
		if m := initVersion.FindSubmatch(data); m != nil {
			inv.InitVersion = string(m[1])
		}
		inv.add(Info, "service", l.Init, "", "the legacy manager is installed (version %s)", orUnknown(inv.InitVersion))
	} else {
		inv.add(Warning, "service", l.Init, "", "the legacy init script is not here; the command link and service cannot be disabled by moving it, and rollback cannot restore it")
	}
	switch target, err := os.Readlink(l.P(l.Command)); {
	case err == nil && target == l.Init:
		inv.add(Info, "service", l.Command, "", "the msm command is a link to %s; the Go executable takes its place", l.Init)
	case err == nil:
		inv.add(Blocker, "service", l.Command, "", "the msm command links to %s, not to the legacy init script %s; point it there, or give the Go executable another --dest", target, l.Init)
	case errors.Is(err, fs.ErrNotExist):
		inv.add(Warning, "service", l.Command, "", "there is no msm command at this path")
	default:
		inv.add(Blocker, "service", l.Command, "", "this path is not the legacy link but a file or directory; if the Go executable is already installed there, this installation may already be migrated")
	}
	if _, err := os.Lstat(l.P(l.LegacyUnit)); err == nil {
		inv.add(Info, "service", l.LegacyUnit, "", "a systemd unit for the legacy manager exists; it is disabled, backed up and replaced by the Go unit")
	}
	if _, err := os.Lstat(l.P(l.GoUnit)); err == nil && l.GoUnit != l.LegacyUnit {
		inv.add(Blocker, "service", l.GoUnit, "", "a unit already exists where the Go unit would be written")
	}
	if _, err := os.Lstat(l.P(l.GoCron)); err == nil {
		inv.add(Blocker, "schedule", l.GoCron, "", "the Go scheduler file already exists; this installation may already be migrated")
	}
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (inv *Inventory) collectServers(ctx context.Context, env Env, manager identity.Identity) {
	l := env.Layout
	root := l.P(inv.ServerRoot)
	entries, err := os.ReadDir(root)
	if errors.Is(err, fs.ErrNotExist) {
		inv.add(Blocker, "servers", inv.ServerRoot, "", "SERVER_STORAGE_PATH does not exist")
		return
	}
	if err != nil {
		inv.add(Blocker, "servers", inv.ServerRoot, "", "cannot read SERVER_STORAGE_PATH: %v", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() || safepath.ValidateName(e.Name()) != nil {
			continue
		}
		inv.collectServer(ctx, env, e.Name(), filepath.Join(root, e.Name()), manager)
	}
	if len(inv.Servers) == 0 {
		inv.add(Warning, "servers", inv.ServerRoot, "", "there are no servers; only the manager and its schedule would move")
	}
}

func (inv *Inventory) collectServer(ctx context.Context, env Env, name, dir string, manager identity.Identity) {
	l := env.Layout
	// The settings are resolved against the logical directory, so the paths
	// they produce are the ones the running manager would use; the files are
	// read through P.
	logical := filepath.Join(inv.ServerRoot, name)
	var props *serverprops.Document
	if p, err := serverprops.Load(filepath.Join(dir, inv.settings.ServerPropertiesFile)); err == nil {
		props = p
	}
	set, err := legacyconf.ResolveServer(legacyconf.ServerInput{Name: name, Dir: logical, Global: inv.file, Properties: props})
	if err != nil {
		inv.add(Blocker, "servers", name, "", "settings cannot be resolved: %v", err)
		return
	}
	s := ServerInfo{Name: name, Dir: logical, Owner: set.Username(), ScreenName: set.Get("SCREEN_NAME"), Version: set.Get("VERSION"), Worlds: []WorldInfo{}}
	for _, w := range set.Warnings {
		inv.add(Warning, "servers", name, "", "%s", w)
	}
	if _, err := os.Lstat(l.P(set.Get("FLAG_ACTIVE_PATH"))); err == nil {
		s.Active = true
	}
	owner, ownerErr := lookup(env, s.Owner)
	if ownerErr != nil {
		inv.add(Blocker, "users", name, "", "the server's owner %s does not exist: %v", s.Owner, ownerErr)
	} else if info, err := os.Lstat(dir); err == nil {
		if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != owner.UID && l.Root == "" {
			inv.add(Warning, "users", name, "", "the directory belongs to uid %d, not to its owner %s (uid %d); the Go manager acts as %s", st.Uid, s.Owner, owner.UID, s.Owner)
		}
	}
	if s.Owner != inv.ManagerUser {
		inv.add(Info, "users", name, "", "owned by %s, not by the manager user %s", s.Owner, inv.ManagerUser)
	}

	// The invocation: the Go port refuses what it cannot split into plain words.
	if _, err := set.Invocation(); err != nil {
		inv.add(Blocker, "customization", name, "", "%v. The Go port starts a server from an argument list and never through a shell", err)
	} else if set.Source("INVOCATION") != legacyconf.FromDefault {
		inv.add(Info, "customization", name, "", "INVOCATION is customized (%s); the Go port runs it as an argument list", set.Source("INVOCATION"))
	}

	// Running state: screen sessions, read-only.
	switch {
	case env.Prober == nil || ownerErr != nil:
		s.State, s.Detail = "unknown", "no way to inspect screen sessions"
	default:
		st := env.Prober.Probe(ctx, set, owner)
		s.State, s.Detail = st.Kind.String(), st.Detail
	}
	switch s.State {
	case "stopped":
	case "running":
		inv.add(Blocker, "screen", name, "", "the server is running in screen session %s; migration needs a stopped maintenance window (\"msm %s stop\", or \"msm stop\")", s.ScreenName, name)
	default:
		inv.add(Blocker, "screen", name, "", "the server cannot be proven stopped (%s %s); migration needs a stopped maintenance window", s.State, s.Detail)
	}

	// JAR.
	if target, err := os.Readlink(l.P(set.Get("JAR_PATH"))); err == nil {
		s.Jar = target
	} else if _, statErr := os.Lstat(l.P(set.Get("JAR_PATH"))); statErr == nil {
		s.Jar = "(a file in the server directory)"
	} else {
		inv.add(Warning, "jars", name, "", "JAR_PATH %s does not exist; the server cannot start until a JAR is selected", set.Get("JAR_PATH"))
	}

	// Worlds: active ones live in WORLD_STORAGE_PATH, inactive ones beside it.
	flag := set.Get("WORLDS_FLAG_INRAM")
	for _, w := range []struct {
		dir    string
		active bool
	}{{set.Get("WORLD_STORAGE_PATH"), true}, {set.Get("WORLD_STORAGE_INACTIVE_PATH"), false}} {
		ents, err := os.ReadDir(l.P(w.dir))
		if err != nil {
			continue
		}
		for _, e := range ents {
			if !e.IsDir() {
				continue
			}
			_, flagErr := os.Lstat(filepath.Join(l.P(w.dir), e.Name(), flag))
			s.Worlds = append(s.Worlds, WorldInfo{Name: e.Name(), Active: w.active, InRAM: flagErr == nil})
		}
	}
	for _, w := range s.Worlds {
		if w.InRAM {
			inv.add(Warning, "worlds", name, "", "world %s carries the RAM flag. The Go port keeps the flag, but serves the world from disk until the RAM volume is validated (docs/ram-worlds.md); if the RAM copy is newer than the disk copy, run the legacy \"msm %s worlds todisk\" before the window", w.Name, name)
		}
	}
	inv.Servers = append(inv.Servers, s)
}

func (inv *Inventory) collectJars(l Layout) {
	root := l.P(inv.JarRoot)
	ents, err := os.ReadDir(root)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			inv.add(Warning, "jars", inv.JarRoot, "", "cannot read JAR_STORAGE_PATH: %v", err)
		}
		return
	}
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		g := JarGroupInfo{Name: e.Name()}
		files, _ := os.ReadDir(filepath.Join(root, e.Name()))
		for _, f := range files {
			if f.Type().IsRegular() && strings.HasSuffix(f.Name(), ".jar") {
				g.JARs++
			}
		}
		if data, err := os.ReadFile(filepath.Join(root, e.Name(), inv.settings.JargroupTarget)); err == nil {
			g.Target = strings.TrimSpace(string(data))
			if strings.HasPrefix(g.Target, "http://") {
				inv.add(Warning, "jars", e.Name(), "", "the group downloads over http (%s); the Go port refuses insecure downloads, so \"jargroup getlatest\" fails until it is changed to https", g.Target)
			}
		}
		inv.JarGroups = append(inv.JarGroups, g)
	}
}

func (inv *Inventory) collectArchives(l Layout) {
	f := inv.file
	enabled := func(name string) bool { v, ok := f.Lookup(name); return ok && v == "true" }
	for _, a := range []struct{ setting, def string }{
		{"WORLD_ARCHIVE_PATH", "/opt/msm/archives/worlds"}, {"BACKUP_ARCHIVE_PATH", "/opt/msm/archives/backups"},
		{"LOG_ARCHIVE_PATH", "/opt/msm/archives/logs"},
	} {
		p, ok := f.Lookup(a.setting)
		if !ok {
			p = a.def
		}
		info := ArchiveInfo{Setting: a.setting, Path: p}
		if safepath.ValidateConfiguredRoot(p) != nil {
			inv.add(Warning, "archives", a.setting, "", "%s is not an absolute clean path and is not inspected; the Go port refuses it too", p)
			inv.Archives = append(inv.Archives, info)
			continue
		}
		if ents, err := os.ReadDir(l.P(p)); err == nil {
			info.Exists, info.Entries = true, len(ents)
		} else {
			inv.add(Info, "archives", a.setting, "", "%s does not exist yet; the Go port creates it on first use", p)
		}
		inv.Archives = append(inv.Archives, info)
	}
	if enabled("RDIFF_BACKUP_ENABLED") {
		inv.add(Info, "archives", "RDIFF_BACKUP_ENABLED", "", "rdiff-backup repositories stay readable; the Go port still needs the rdiff-backup program for that mode")
	}
	if enabled("RSYNC_BACKUP_ENABLED") {
		inv.add(Info, "archives", "RSYNC_BACKUP_ENABLED", "", "rsync-style snapshots are done by the Go port itself; existing snapshots are kept")
	}
}

func (inv *Inventory) collectSchedule(l Layout) {
	data, err := os.ReadFile(l.P(l.Cron))
	if errors.Is(err, fs.ErrNotExist) {
		inv.add(Warning, "schedule", l.Cron, "", "there is no legacy cron file; no scheduled maintenance will be installed")
		return
	}
	if err != nil {
		inv.add(Blocker, "schedule", l.Cron, "", "cannot read the legacy cron file: %v", err)
		return
	}
	inv.Cron = ParseLegacyCron(string(data))
	jobs := 0
	for _, e := range inv.Cron {
		switch e.Kind {
		case KindJob, KindRetention:
			jobs++
		case KindUnsupported:
			inv.add(Blocker, "schedule", fmt.Sprintf("%s:%d", l.Cron, e.Line), CodeUnsupportedSchedule,
				"%s: %s. It is not migrated; it is kept as a comment in the Go cron file and in the backup. Rewrite it, or accept the loss with --drop-unsupported", e.Why, e.Raw)
		}
	}
	if jobs == 0 {
		inv.add(Warning, "schedule", l.Cron, "", "the legacy cron file schedules none of the standard jobs")
	}
	// A retention line for a path the settings do not name prunes elsewhere.
	for _, e := range inv.Cron {
		if e.Kind != KindRetention {
			continue
		}
		fields := strings.Fields(e.Command)
		if len(fields) > 1 && !strings.HasPrefix(fields[1], inv.ServerRoot) && !strings.HasPrefix(fields[1], "/opt/msm/archives") {
			inv.add(Warning, "schedule", fmt.Sprintf("%s:%d", l.Cron, e.Line), "", "the retention line prunes %s; the Go prune job follows SERVER_STORAGE_PATH and the archive settings instead", fields[1])
		}
	}
}

func (inv *Inventory) collectCustomizations(l Layout) {
	if data, err := os.ReadFile(l.P(l.Profile)); err == nil {
		inv.add(Warning, "customization", l.Profile, "", "the legacy init script sources this file (%d bytes); the Go port does not, so environment set here is not seen by servers. Put what servers need in INVOCATION or msm-invocation", len(data))
	}
	vers := l.P(inv.settingsOr("VERSIONING_STORAGE_PATH", "/opt/msm/versioning"))
	ext, _ := inv.file.Lookup("VERSIONING_FILE_EXTENSION")
	if ext == "" {
		ext = "sh"
	}
	var scripts []string
	_ = filepath.WalkDir(vers, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() && strings.HasSuffix(d.Name(), "."+ext) {
			rel, _ := filepath.Rel(vers, path)
			scripts = append(scripts, rel)
		}
		return nil
	})
	if len(scripts) > 0 {
		inv.add(Info, "customization", vers, "", "%d versioning scripts (%s ...) are shell code the Go port never runs; it uses built-in version profiles (docs/compatibility/deviations.md). Check that each server's VERSION selects the profile you need", len(scripts), scripts[0])
	}
}

func (inv *Inventory) settingsOr(name, def string) string {
	if v, ok := inv.file.Lookup(name); ok {
		return v
	}
	return def
}

// Fingerprint summarizes the world directories of a server so a later
// comparison can show nothing was written to them.
type Fingerprint struct {
	Files int64  `json:"files"`
	Bytes int64  `json:"bytes"`
	Hash  string `json:"hash"`
}

// FingerprintWorlds hashes the path, size, mode and modification time of
// every entry below each server's world directories, without following
// links and without reading file contents. Equal fingerprints mean no file
// was added, removed, resized or rewritten.
func FingerprintWorlds(l Layout, inv *Inventory) (map[string]Fingerprint, error) {
	out := map[string]Fingerprint{}
	for _, s := range inv.Servers {
		set, err := serverSettingsFor(l, inv, s)
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		var fp Fingerprint
		for _, dir := range []string{set.Get("WORLD_STORAGE_PATH"), set.Get("WORLD_STORAGE_INACTIVE_PATH")} {
			base := l.P(dir)
			err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
				if err != nil {
					if errors.Is(err, fs.ErrNotExist) && path == base {
						return nil
					}
					return err
				}
				info, err := d.Info()
				if err != nil {
					return err
				}
				rel, _ := filepath.Rel(base, path)
				fmt.Fprintf(h, "%s\x00%s\x00%d\x00%o\x00%d\n", dir, rel, info.Size(), info.Mode(), info.ModTime().UnixNano())
				if info.Mode().IsRegular() {
					fp.Files++
					fp.Bytes += info.Size()
				}
				return nil
			})
			if err != nil {
				return nil, fmt.Errorf("migrate: fingerprint %s: %w", base, err)
			}
		}
		fp.Hash = hex.EncodeToString(h.Sum(nil))
		out[s.Name] = fp
	}
	return out, nil
}

func serverSettingsFor(l Layout, inv *Inventory, s ServerInfo) (*legacyconf.ServerSettings, error) {
	var props *serverprops.Document
	if p, err := serverprops.Load(filepath.Join(l.P(s.Dir), inv.settings.ServerPropertiesFile)); err == nil {
		props = p
	}
	return legacyconf.ResolveServer(legacyconf.ServerInput{Name: s.Name, Dir: s.Dir, Global: inv.file, Properties: props})
}
