package legacyconf

// globalSettings are the global settings init/msm registers with
// register_setting, in registration order, with the defaults the legacy
// manager documents. compatibility/settings.tsv is the reviewed inventory;
// TestGlobalSettingsMatchInventory keeps the two identical.
var globalSettings = []struct{ Name, Default string }{
	{"DEBUG", "false"},
	{"USERNAME", "minecraft"},
	{"SERVER_STORAGE_PATH", "/opt/msm/servers"},
	{"JAR_STORAGE_PATH", "/opt/msm/jars"},
	{"VERSIONING_STORAGE_PATH", "/opt/msm/versioning"},
	{"VERSIONING_FILE_EXTENSION", "sh"},
	{"RAMDISK_STORAGE_ENABLED", "true"},
	{"RAMDISK_STORAGE_PATH", "/dev/shm/msm"},
	{"WORLD_ARCHIVE_ENABLED", "true"},
	{"WORLD_RDIFF_PATH", "/opt/msm/rdiff-backup/worlds"},
	{"RDIFF_BACKUP_ENABLED", "false"},
	{"RDIFF_BACKUP_NICE", "19"},
	{"RDIFF_BACKUP_ROTATION", "7"},
	{"UPDATE_URL", "https://raw.githubusercontent.com/msmhq/msm/latest"},
	{"WORLD_ARCHIVE_PATH", "/opt/msm/archives/worlds"},
	{"LOG_ARCHIVE_PATH", "/opt/msm/archives/logs"},
	{"BACKUP_ARCHIVE_PATH", "/opt/msm/archives/backups"},
	{"RSYNC_BACKUP_ENABLED", "false"},
	{"WORLD_RSYNC_PATH", "/opt/msm/rsync/worlds"},
	{"JARGROUP_TARGET", "target.txt"},
	{"JARGROUP_DOWNLOAD_DIR", "downloads"},
	{"SERVER_PROPERTIES", "server.properties"},
}

// goDefaults are the global defaults the Go port deliberately changes
// (docs/compatibility/deviations.md): the setting is shown with the value
// the Go port acts on when msm.conf does not set it.
var goDefaults = map[string]string{
	// DEV-008: a missing RAMDISK_STORAGE_ENABLED means off.
	"RAMDISK_STORAGE_ENABLED": "false",
}

// ConfigEntry is one line of `msm config`.
type ConfigEntry struct {
	Name  string
	Value string
	// Source is "msm.conf" when the file sets the value, "default" for the
	// built-in default and "detected" for a value the Go port resolved
	// itself, such as the data directories of a rootless installation.
	Source string
}

// GlobalEntries returns every global setting, then every DEFAULT_<server
// setting>, in the order init/msm registers them, with the value the
// manager acts on. f is the parsed msm.conf (nil when there is none).
// resolved holds values the caller has already resolved by other rules
// (rootless data directories, the manager user, the properties file name);
// they win over the file and the defaults.
func GlobalEntries(f *File, resolved map[string]string) []ConfigEntry {
	entries := make([]ConfigEntry, 0, len(globalSettings)+len(serverSettings))
	add := func(name, def string) {
		if v, ok := resolved[name]; ok {
			source := "detected"
			if f != nil {
				if _, set := f.Lookup(name); set {
					source = "msm.conf"
				}
			}
			entries = append(entries, ConfigEntry{name, v, source})
			return
		}
		if f != nil {
			if v, ok := f.Lookup(name); ok {
				entries = append(entries, ConfigEntry{name, v, "msm.conf"})
				return
			}
		}
		entries = append(entries, ConfigEntry{name, def, "default"})
	}
	for _, s := range globalSettings {
		def := s.Default
		if g, ok := goDefaults[s.Name]; ok {
			def = g
		}
		add(s.Name, def)
	}
	for _, s := range serverSettings {
		add("DEFAULT_"+s.Name, s.Default)
	}
	return entries
}
