package legacyconf

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/thehatchcloud/msm/internal/identity"
	"github.com/thehatchcloud/msm/internal/safepath"
	"github.com/thehatchcloud/msm/internal/serverprops"
)

// serverSettings are the per-server settings init/msm registers with
// register_server_setting, in registration order, with their built-in
// defaults. compatibility/settings.tsv is the reviewed inventory;
// TestServerSettingsMatchInventory keeps the two identical.
var serverSettings = []struct{ Name, Default string }{
	{"USERNAME", "minecraft"},
	{"SCREEN_NAME", "msm-{SERVER_NAME}"},
	{"VERSION", "unknown"},
	{"WORLD_STORAGE_PATH", "worldstorage"},
	{"WORLD_STORAGE_INACTIVE_PATH", "worldstorage_inactive"},
	{"LOG_PATH", "server.log"},
	{"WHITELIST_PATH", "whitelist.json"},
	{"BANNED_PLAYERS_PATH", "banned-players.json"},
	{"BANNED_IPS_PATH", "banned-ips.json"},
	{"OPS_PATH", "ops.json"},
	{"OPS_LIST", ""},
	{"JAR_PATH", "server.jar"},
	{"FLAG_ACTIVE_PATH", "active"},
	{"COMPLETE_BACKUP_FOLLOW_SYMLINKS", "false"},
	{"WORLDS_FLAG_INRAM", "inram"},
	{"RAM", "1024"},
	{"INVOCATION", "java -Xms{RAM}M -Xmx{RAM}M -jar {JAR} nogui"},
	{"STOP_DELAY", "10"},
	{"RESTART_DELAY", "10"},
	{"MESSAGE_STOP", "SERVER SHUTTING DOWN IN {DELAY} SECONDS!"},
	{"MESSAGE_STOP_ABORT", "Server shut down aborted."},
	{"MESSAGE_RESTART", "SERVER REBOOT IN {DELAY} SECONDS!"},
	{"MESSAGE_RESTART_ABORT", "Server reboot aborted."},
	{"MESSAGE_WORLD_BACKUP_STARTED", "Backing up world."},
	{"MESSAGE_WORLD_BACKUP_FINISHED", "Backup complete."},
	{"MESSAGE_COMPLETE_BACKUP_STARTED", "Backing up entire server."},
	{"MESSAGE_COMPLETE_BACKUP_FINISHED", "Backup complete."},
	{"CONFIRM_SAVE_ON", ""},
	{"CONFIRM_SAVE_OFF", ""},
	{"CONFIRM_SAVE_ALL", ""},
	{"CONFIRM_START", ""},
	{"CONFIRM_KICK", ""},
	{"CONFIRM_TIME_SET", ""},
	{"CONFIRM_TIME_ADD", ""},
	{"CONFIRM_TOGGLEDOWNFALL", ""},
	{"CONFIRM_GAMEMODE", ""},
	{"CONFIRM_GIVE", ""},
	{"CONFIRM_XP", ""},
}

// Source is the layer a server setting's effective value came from.
type Source int

const (
	// FromDefault: the built-in default init/msm registers.
	FromDefault Source = iota
	// FromGlobal: DEFAULT_<NAME> in msm.conf.
	FromGlobal
	// FromProfile: the server's version profile (P08).
	FromProfile
	// FromServer: msm-<lowercase-dash-name> in the server's properties file.
	FromServer
)

func (s Source) String() string {
	return [...]string{"default", "msm.conf", "version profile", "server.properties"}[s]
}

// ServerInput is everything that determines one server's settings.
type ServerInput struct {
	// Name is the server's directory name under SERVER_STORAGE_PATH.
	Name string
	// Dir is the server's absolute directory.
	Dir string
	// Global is the parsed msm.conf; nil means no legacy configuration,
	// so only built-in defaults apply below the server's own overrides.
	Global *File
	// Properties is the server's parsed properties file; nil means the
	// server has none yet.
	Properties *serverprops.Document
	// Profile holds values the server's version profile supplies, keyed
	// by setting name. P08 fills it for LOG_PATH and the other
	// profile-dependent settings; nil means none.
	Profile map[string]string
}

// ServerSettings are one server's effective settings.
type ServerSettings struct {
	Name, Dir string
	values    map[string]string
	sources   map[string]Source
	// Warnings are non-fatal notes, such as an msm-* override that names
	// no registered setting and is therefore ignored, as it is by the
	// legacy manager.
	Warnings []string
}

// ResolveServer computes each registered per-server setting the way the
// legacy manager's server_property does. The first non-empty value wins,
// in this order:
//
//  1. msm-<lowercase-dash-name> in the server's properties file;
//  2. the version profile (in.Profile);
//  3. DEFAULT_<NAME> in msm.conf;
//  4. the built-in default.
//
// Lookups at each layer are case-insensitive, the last matching line
// wins, and an empty value falls through to the next layer. The winning
// value is then post-processed as server_set_property does: a relative
// *_PATH is joined to the server directory, {SERVER_NAME} is expanded in
// SCREEN_NAME, {DELAY} in MESSAGE_STOP and MESSAGE_RESTART (from STOP_DELAY
// and RESTART_DELAY), and {RAM} and {JAR} in INVOCATION (from RAM and the
// resolved JAR_PATH). Nothing is evaluated as shell.
func ResolveServer(in ServerInput) (*ServerSettings, error) {
	if err := safepath.ValidateName(in.Name); err != nil {
		return nil, fmt.Errorf("legacyconf: server name: %w", err)
	}
	if !filepath.IsAbs(in.Dir) {
		return nil, fmt.Errorf("legacyconf: server %q directory %q must be absolute", in.Name, in.Dir)
	}
	s := &ServerSettings{Name: in.Name, Dir: filepath.Clean(in.Dir),
		values: map[string]string{}, sources: map[string]Source{}}
	for _, setting := range serverSettings {
		value, source := setting.Default, FromDefault
		if in.Global != nil {
			if v, ok := in.Global.Lookup("DEFAULT_" + setting.Name); ok {
				value, source = v, FromGlobal
			}
		}
		if v := in.Profile[setting.Name]; v != "" {
			value, source = v, FromProfile
		}
		if in.Properties != nil {
			if v, ok := in.Properties.Get(OverrideKey(setting.Name)); ok && v != "" {
				value, source = v, FromServer
			}
		}
		s.values[setting.Name], s.sources[setting.Name] = value, source
	}
	s.postProcess()
	if in.Properties != nil {
		for name := range in.Properties.Overrides() {
			if _, ok := s.values[strings.ToUpper(strings.ReplaceAll(name, "-", "_"))]; !ok {
				s.Warnings = append(s.Warnings, fmt.Sprintf(
					"server %q: msm-%s is not a registered server setting and is ignored", in.Name, name))
			}
		}
		sort.Strings(s.Warnings)
	}
	return s, nil
}

// OverrideKey is the server.properties key that overrides a setting, for
// example "msm-stop-delay" for STOP_DELAY.
func OverrideKey(name string) string {
	return "msm-" + strings.ToLower(strings.ReplaceAll(name, "_", "-"))
}

func (s *ServerSettings) postProcess() {
	v := s.values
	for name, value := range v {
		// init/msm treats a value as absolute only when it matches ^/.+
		if strings.HasSuffix(name, "_PATH") && !(len(value) > 1 && value[0] == '/') {
			v[name] = filepath.Join(s.Dir, value)
		}
	}
	v["SCREEN_NAME"] = strings.ReplaceAll(v["SCREEN_NAME"], "{SERVER_NAME}", s.Name)
	v["MESSAGE_STOP"] = strings.ReplaceAll(v["MESSAGE_STOP"], "{DELAY}", v["STOP_DELAY"])
	v["MESSAGE_RESTART"] = strings.ReplaceAll(v["MESSAGE_RESTART"], "{DELAY}", v["RESTART_DELAY"])
	inv := strings.ReplaceAll(v["INVOCATION"], "{RAM}", v["RAM"])
	v["INVOCATION"] = strings.ReplaceAll(inv, "{JAR}", v["JAR_PATH"])
}

// Get returns a registered setting's effective value. It panics on a
// name that init/msm does not register, which is a programming error.
func (s *ServerSettings) Get(name string) string {
	v, ok := s.values[name]
	if !ok {
		panic(fmt.Sprintf("legacyconf: %q is not a registered server setting", name))
	}
	return v
}

// Source reports which layer supplied a setting's effective value.
func (s *ServerSettings) Source(name string) Source {
	s.Get(name) // panics on an unregistered name
	return s.sources[name]
}

// Username is the OS user that owns this server's files and process. It
// comes from msm-username or DEFAULT_USERNAME, not the manager's USERNAME:
// the legacy manager keeps those separate.
func (s *ServerSettings) Username() string { return s.Get("USERNAME") }

// ErrNoOwner reports a server whose configured owner does not exist.
var ErrNoOwner = errors.New("legacyconf: server owner does not exist")

// Owner resolves Username to an OS identity, so callers run the server's
// operations explicitly as that user (identity.DropTo, or a child process
// credential) instead of as whoever invoked the manager.
func (s *ServerSettings) Owner() (identity.Identity, error) {
	id, err := identity.Lookup(s.Username())
	if err != nil {
		return identity.Identity{}, fmt.Errorf("%w: server %q (%s from %s): %v",
			ErrNoOwner, s.Name, s.Username(), s.Source("USERNAME"), err)
	}
	return id, nil
}
