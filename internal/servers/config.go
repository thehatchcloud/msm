package servers

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/thehatchcloud/msm/internal/legacyconf"
	"github.com/thehatchcloud/msm/internal/screen"
	"github.com/thehatchcloud/msm/internal/serverprops"
)

var (
	// ErrUnknownSetting reports an msm-* key that no registered setting
	// answers to; the manager would ignore it.
	ErrUnknownSetting = errors.New("servers: no such MSM setting")
	// ErrNoProperty reports a property the server's properties file does
	// not set.
	ErrNoProperty = errors.New("servers: property is not set")
	// ErrBadSetting reports a value the manager would not be able to use.
	ErrBadSetting = errors.New("servers: invalid setting value")
	// ErrNoProperties reports a server without a properties file.
	ErrNoProperties = errors.New("servers: the server has no properties file")
)

// ConfigSetting is one effective per-server MSM setting.
type ConfigSetting struct {
	// Key is the server.properties key that overrides it, such as
	// "msm-stop-delay".
	Key   string
	Value string
	// Source is the layer the effective value came from.
	Source legacyconf.Source
}

// Effect says when a change to a setting takes hold.
type Effect int

const (
	// Immediate: the next msm command uses the new value.
	Immediate Effect = iota
	// OnRestart: a running server keeps its old value until it restarts.
	OnRestart
	// WhileStopped: the manager recognises a running server by this value,
	// so it can only be changed while the server is stopped.
	WhileStopped
)

func (e Effect) String() string {
	return [...]string{"immediate", "restart", "stopped"}[e]
}

// SettingEffect classifies an msm-* setting by name (for example "RAM").
func SettingEffect(name string) Effect {
	switch name {
	case "SCREEN_NAME", "USERNAME", "INVOCATION", "RAM", "JAR_PATH":
		// These decide which screen session is "this server" (or which
		// command it runs); changing them under a live server would make
		// the manager lose track of it.
		return WhileStopped
	case "WORLD_STORAGE_PATH", "WORLD_STORAGE_INACTIVE_PATH", "WORLDS_FLAG_INRAM", "LOG_PATH":
		return OnRestart
	}
	return Immediate
}

// KeyEffect classifies a server.properties key: every ordinary Minecraft
// property is read once, when the server starts.
func KeyEffect(key string) Effect {
	if name, isMSM, registered := legacyconf.SettingFromKey(key); isMSM && registered {
		return SettingEffect(name)
	}
	return OnRestart
}

// confirmIgnored explains DEV-034: the CONFIRM_* regular expressions of the
// legacy manager are replaced by the version profiles' own data.
const confirmIgnored = "the Go port takes the lines that confirm a console command from the server's version profile (msm-version), not from this setting (DEV-034)"

// ConfigList returns the effective value of every per-server setting in
// registration order, as `msm <server> config` lists them, and the
// settings' warnings.
func (m *Manager) ConfigList(name string) ([]ConfigSetting, []string, error) {
	s, err := m.Settings(name)
	if err != nil {
		return nil, nil, err
	}
	var out []ConfigSetting
	warnings := slices.Clone(s.Warnings)
	for _, n := range legacyconf.ServerSettingNames() {
		src := s.Source(n)
		out = append(out, ConfigSetting{Key: legacyconf.OverrideKey(n), Value: s.Get(n), Source: src})
		if strings.HasPrefix(n, "CONFIRM_") && (src == legacyconf.FromServer || src == legacyconf.FromGlobal) {
			warnings = append(warnings, fmt.Sprintf("server %q: %s is set in %s but has no effect: %s", name, legacyconf.OverrideKey(n), src, confirmIgnored))
		}
	}
	return out, warnings, nil
}

// ConfigGet returns one value: the effective value of an msm-* setting, or
// a Minecraft property as written in the properties file.
func (m *Manager) ConfigGet(name, key string) (string, error) {
	s, err := m.Settings(name)
	if err != nil {
		return "", err
	}
	if setting, isMSM, registered := legacyconf.SettingFromKey(key); isMSM {
		if !registered {
			return "", unknownSetting(key)
		}
		return s.Get(setting), nil
	}
	doc, err := serverprops.Load(m.propertiesPath(s))
	if errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("%w: %q in server %q", ErrNoProperty, key, name)
	}
	if err != nil {
		return "", err
	}
	v, ok := doc.GetExact(key)
	if !ok {
		return "", fmt.Errorf("%w: %q in server %q", ErrNoProperty, key, name)
	}
	return v, nil
}

func unknownSetting(key string) error {
	return fmt.Errorf("%w: %q (run \"config\" without a setting to list them)", ErrUnknownSetting, key)
}

func (m *Manager) propertiesPath(s *legacyconf.ServerSettings) string {
	return filepath.Join(s.Dir, m.cfg.PropertiesFile)
}

// ConfigChange describes a completed write.
type ConfigChange struct {
	Key, Old, New string
	// Existed is whether the key was already set in the file.
	Existed bool
	File    string
	Effect  Effect
	// State is the server's state when the value was written.
	State State
}

// ConfigSet writes key=value into the server's properties file. The file's
// comments, blank lines and every other key are kept as written, the write
// is atomic, and it happens under the server's lock. A value or key that
// could add or change another line is refused (DEV-016), as is an msm-*
// key no setting answers to or a value the manager could not use, and a
// setting the manager identifies a running server by while the server is
// not proven stopped.
func (m *Manager) ConfigSet(ctx context.Context, name, key, value string) (*ConfigChange, error) {
	if err := checkKeyValue(key, value); err != nil {
		return nil, err
	}
	setting, isMSM, registered := legacyconf.SettingFromKey(key)
	if isMSM {
		if !registered {
			return nil, unknownSetting(key)
		}
		key = legacyconf.OverrideKey(setting)
		if err := checkSettingValue(setting, value); err != nil {
			return nil, err
		}
	}
	lock, s, err := m.LockServer(name)
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	owner, err := m.owner(s)
	if err != nil {
		return nil, err
	}
	change := &ConfigChange{Key: key, New: value, File: m.propertiesPath(s), Effect: KeyEffect(key)}
	change.State = m.state(ctx, s)
	if change.Effect == WhileStopped {
		if err := requireStopped(name, change.State); err != nil {
			return nil, fmt.Errorf("%w: %s decides which process is this server, so change it only while the server is stopped", err, key)
		}
	}
	if err := m.ActAsOwner(owner); err != nil {
		return nil, err
	}
	doc, err := serverprops.Load(change.File)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w: %s does not exist", ErrNoProperties, change.File)
	}
	if err != nil {
		return nil, err
	}
	if isMSM {
		change.Old, change.Existed = doc.Get(key)
	} else {
		change.Old, change.Existed = doc.GetExact(key)
	}
	if err := doc.Set(key, value); err != nil {
		return nil, err
	}
	if isMSM {
		if err := validateResolved(name, s.Dir, m.cfg.Global, doc, setting); err != nil {
			return nil, err
		}
	}
	if err := doc.Save(change.File, 0o644); err != nil {
		return nil, err
	}
	return change, nil
}

// checkKeyValue refuses what Document.Set would, before anything is locked.
func checkKeyValue(key, value string) error {
	probe := serverprops.Parse(nil)
	return probe.Set(key, value)
}

func nonNegative(v string) bool {
	if v == "" || len(v) > 9 {
		return false
	}
	return strings.Trim(v, "0123456789") == ""
}

// checkSettingValue rejects a value of a numeric or boolean setting that the
// manager would only discover at the next start or stop.
func checkSettingValue(setting, value string) error {
	if strings.HasPrefix(setting, "CONFIRM_") {
		return fmt.Errorf("%w: %s would have no effect: %s", ErrBadSetting, legacyconf.OverrideKey(setting), confirmIgnored)
	}
	bad := func(want string) error {
		return fmt.Errorf("%w: %s must be %s, not %q", ErrBadSetting, legacyconf.OverrideKey(setting), want, value)
	}
	if value == "" {
		// An empty override falls through to the default, which is how
		// a setting is unset without deleting its line.
		return nil
	}
	switch setting {
	case "RAM":
		if !nonNegative(value) || strings.Trim(value, "0") == "" {
			return bad("a positive whole number of megabytes")
		}
	case "STOP_DELAY", "RESTART_DELAY":
		if !nonNegative(value) {
			return bad("a whole number of seconds")
		}
	case "COMPLETE_BACKUP_FOLLOW_SYMLINKS":
		if value != "true" && value != "false" {
			return bad(`"true" or "false"`)
		}
	}
	return nil
}

// validateResolved re-resolves the server with the edited document, so a
// change that would leave it without a usable launch command or session
// name is refused instead of found at the next start.
func validateResolved(name, dir string, global *legacyconf.File, doc *serverprops.Document, setting string) error {
	resolved, err := legacyconf.ResolveServer(legacyconf.ServerInput{Name: name, Dir: dir, Global: global, Properties: doc})
	if err != nil {
		return err
	}
	switch setting {
	case "INVOCATION", "RAM", "JAR_PATH":
		if _, err := resolved.Invocation(); err != nil {
			return fmt.Errorf("%w: %v", ErrBadSetting, err)
		}
	case "SCREEN_NAME":
		if err := screen.ValidateName(resolved.Get("SCREEN_NAME")); err != nil {
			return fmt.Errorf("%w: %v", ErrBadSetting, err)
		}
	}
	return nil
}

// Entry returns one server's listing entry: its intent and what the manager
// can prove about its process.
func (m *Manager) Entry(ctx context.Context, name string) (Entry, error) {
	s, err := m.Settings(name)
	if err != nil {
		return Entry{}, err
	}
	_, statErr := os.Lstat(s.Get("FLAG_ACTIVE_PATH"))
	return Entry{Name: name, Dir: s.Dir, Active: statErr == nil, State: m.state(ctx, s)}, nil
}

// PropertiesPath is the path of a server's properties file given the
// properties file name (Manager.PropertiesFile).
func PropertiesPath(s *legacyconf.ServerSettings, file string) string {
	return filepath.Join(s.Dir, file)
}
