package servers

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
)

// worldStorageReadme is the text the legacy manager writes into a new
// server's world storage directory.
const worldStorageReadme = "MSM requires all your worlds be moved into this directory.\n"

// Created describes a new server.
type Created struct {
	Name, Dir string
	// Warnings are non-fatal notes, such as a configured list file that
	// lies outside the instance and so was not created.
	Warnings []string
}

// Create makes a new server with the legacy layout: empty JSON player
// lists, ops.txt seeded from DEFAULT_OPS_LIST, an empty properties file,
// and a world storage directory with its readme. It never writes eula.txt:
// accepting the Minecraft EULA is the administrator's decision.
//
// The instance is assembled in a staging directory in the storage root and
// published with one rename, so an interrupted create never leaves a
// half-built server under the new name.
func (m *Manager) Create(ctx context.Context, name string) (*Created, error) {
	if err := m.actAs(); err != nil {
		return nil, err
	}
	if err := m.available(name); err != nil {
		return nil, err
	}
	lock, err := m.lockRoot()
	if err != nil {
		return nil, err
	}
	defer lock.Release()
	if err := m.drop(); err != nil {
		return nil, err
	}
	// Another manager may have created it while this one waited.
	if err := m.available(name); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dir := filepath.Join(m.cfg.Root, name)
	// Resolve settings as they will be for the finished server, before it
	// has a properties file: msm.conf DEFAULT_* values, else built-ins.
	s, err := m.settings(name, dir)
	if err != nil {
		return nil, err
	}
	suffix, err := randomSuffix()
	if err != nil {
		return nil, err
	}
	stage := filepath.Join(m.cfg.Root, stagePrefix+name+"-"+suffix)
	if err := m.ops.Mkdir(stage, 0o755); err != nil {
		return nil, fmt.Errorf("servers: create staging directory: %w", err)
	}
	created := &Created{Name: name, Dir: dir}
	if err := m.populate(stage, dir, s.Get, created); err != nil {
		return nil, m.abandon(stage, err)
	}
	if err := m.ops.Rename(stage, dir); err != nil {
		return nil, m.abandon(stage, fmt.Errorf("servers: publish %s: %w", dir, err))
	}
	syncDir(m.cfg.Root)
	return created, nil
}

// populate writes the new instance's files into stage. Paths are resolved
// against the final directory, then mapped into stage.
func (m *Manager) populate(stage, dir string, get func(string) string, c *Created) error {
	inStage := func(setting, path string) (string, bool) {
		rel, err := filepath.Rel(dir, path)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			c.Warnings = append(c.Warnings, fmt.Sprintf(
				"DEFAULT_%s is %s, outside the new server directory; it was not created", setting, path))
			return "", false
		}
		return filepath.Join(stage, rel), true
	}
	write := func(setting, path string, data []byte) error {
		dest, ok := inStage(setting, path)
		if !ok {
			return nil
		}
		if err := m.mkdirs(stage, filepath.Dir(dest)); err != nil {
			return err
		}
		if err := m.ops.WriteFile(dest, data, 0o644); err != nil {
			return fmt.Errorf("servers: write %s: %w", filepath.Base(dest), err)
		}
		return nil
	}

	for _, setting := range []string{"WHITELIST_PATH", "BANNED_IPS_PATH", "BANNED_PLAYERS_PATH", "OPS_PATH"} {
		if err := write(setting, get(setting), []byte("[]\n")); err != nil {
			return err
		}
	}
	// Names in ops.txt are converted to ops.json by Minecraft on first
	// start, as in the legacy manager.
	if ops := opsList(get("OPS_LIST")); ops != "" {
		if err := write("OPS_LIST", filepath.Join(dir, "ops.txt"), []byte(ops)); err != nil {
			return err
		}
	}
	if err := write("SERVER_PROPERTIES", filepath.Join(dir, m.cfg.PropertiesFile), nil); err != nil {
		return err
	}
	worlds, ok := inStage("WORLD_STORAGE_PATH", get("WORLD_STORAGE_PATH"))
	if !ok {
		return nil
	}
	if err := m.mkdirs(stage, worlds); err != nil {
		return err
	}
	if err := m.ops.WriteFile(filepath.Join(worlds, "readme.txt"), []byte(worldStorageReadme), 0o644); err != nil {
		return fmt.Errorf("servers: write world storage readme: %w", err)
	}
	return nil
}

// opsList formats DEFAULT_OPS_LIST as the legacy manager does: split on
// commas, spaces removed, one name per line.
func opsList(list string) string {
	var b strings.Builder
	for _, name := range strings.Split(list, ",") {
		if name = strings.ReplaceAll(name, " ", ""); name != "" {
			b.WriteString(name + "\n")
		}
	}
	return b.String()
}

// mkdirs creates dir and any missing parents, none above stage.
func (m *Manager) mkdirs(stage, dir string) error {
	if dir == stage {
		return nil
	}
	if err := m.mkdirs(stage, filepath.Dir(dir)); err != nil {
		return err
	}
	if err := m.ops.Mkdir(dir, 0o755); err != nil && !isExist(err) {
		return fmt.Errorf("servers: create %s: %w", filepath.Base(dir), err)
	}
	return nil
}

// abandon removes a staging directory after a failed create. If even that
// fails, the error names the directory, which List keeps reporting.
func (m *Manager) abandon(stage string, cause error) error {
	if err := m.ops.RemoveAll(stage); err != nil {
		return fmt.Errorf("%w; the incomplete server remains at %s and should be removed by hand (%v)", cause, stage, err)
	}
	return cause
}
