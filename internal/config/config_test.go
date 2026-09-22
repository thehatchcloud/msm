package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/pflag"
)

func isolatedHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("MSM_DEBUG", "")
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(configDir, "msm")
}

func writeConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestPrecedence(t *testing.T) {
	isolatedHome(t)
	for _, tt := range []struct {
		name, env, content string
		args               []string
		want               bool
	}{
		{name: "default"},
		{name: "file", content: "debug: true\n", want: true},
		{name: "environment without file", env: "true", want: true},
		{name: "environment overrides file", env: "false", content: "debug: true\n"},
		{name: "unchanged flag does not override environment", env: "true", want: true},
		{name: "changed false flag overrides environment and file", env: "true", content: "debug: true\n", args: []string{"--debug=false"}},
		{name: "changed true flag overrides environment", env: "false", args: []string{"--debug"}, want: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("MSM_DEBUG", tt.env)
			v := New()
			flags := pflag.NewFlagSet("test", pflag.ContinueOnError)
			flags.Bool("debug", false, "diagnostics")
			if err := v.BindPFlag("debug", flags.Lookup("debug")); err != nil {
				t.Fatal(err)
			}
			if err := flags.Parse(tt.args); err != nil {
				t.Fatal(err)
			}
			var file string
			if tt.content != "" {
				file = filepath.Join(t.TempDir(), "config.yaml")
				writeConfig(t, file, tt.content)
			}
			got, err := Read(v, file)
			if err != nil || got.Debug != tt.want {
				t.Fatalf("settings=%+v err=%v; want debug=%v", got, err, tt.want)
			}
		})
	}
}

func TestDiscoveryAndIsolation(t *testing.T) {
	dir := isolatedHome(t)
	path := filepath.Join(dir, "config.yaml")
	content := "debug: true\n"
	writeConfig(t, path, content)
	got, err := Read(New(), "")
	if err != nil || !got.Debug {
		t.Fatalf("discovered settings=%+v err=%v", got, err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != content {
		t.Fatalf("configuration was changed: %q err=%v", data, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	got, err = Read(New(), "")
	if err != nil || got.Debug {
		t.Fatalf("a fresh instance leaked configuration: %+v err=%v", got, err)
	}
}

func TestInvalidConfiguration(t *testing.T) {
	isolatedHome(t)
	for _, tt := range []struct {
		name, content, want string
	}{
		{"malformed", "debug: [\n", "read configuration"},
		{"invalid type", "debug: not-a-boolean\n", "decode configuration"},
		{"unknown setting", "debig: true\n", "decode configuration"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.yaml")
			writeConfig(t, file, tt.content)
			if _, err := Read(New(), file); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error=%v; want %q", err, tt.want)
			}
		})
	}
	if _, err := Read(New(), filepath.Join(t.TempDir(), "missing.yaml")); err == nil {
		t.Fatal("explicit missing file must fail")
	}
	t.Setenv("MSM_DEBUG", "not-a-boolean")
	if _, err := Read(New(), ""); err == nil {
		t.Fatal("invalid environment setting must fail")
	}
}

func TestMalformedDiscoveredConfiguration(t *testing.T) {
	dir := isolatedHome(t)
	writeConfig(t, filepath.Join(dir, "config.yaml"), "debug: [\n")
	if _, err := Read(New(), ""); err == nil {
		t.Fatal("only absent optional configuration may be ignored")
	}
}

func TestLegacyConfigurationIsNotImported(t *testing.T) {
	isolatedHome(t)
	file := filepath.Join(t.TempDir(), "msm.conf")
	writeConfig(t, file, "MSM_DEBUG=true\n")
	t.Setenv("MSM_CONF", file)
	got, err := Read(New(), "")
	if err != nil || got.Debug {
		t.Fatalf("legacy MSM_CONF must not be interpreted: %+v err=%v", got, err)
	}
	if _, err := Read(New(), file); err == nil {
		t.Fatal("Bash-style configuration is not native configuration")
	}
}
