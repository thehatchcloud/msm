// Package config owns Viper setup and typed application configuration.
// Legacy msm.conf and server.properties migration belong to P03.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/viper"
)

// Settings is the currently supported native configuration schema.
// Add mapstructure tags and defaults for new settings as they are implemented.
type Settings struct {
	Debug bool `mapstructure:"debug"`
}

// New creates an independent instance. Viper precedence is retained:
// changed flags > environment > configuration file > defaults.
func New() *viper.Viper {
	v := viper.New()
	v.SetDefault("debug", false)
	v.SetEnvPrefix("MSM")
	v.SetEnvKeyReplacer(strings.NewReplacer("-", "_", ".", "_"))
	v.AutomaticEnv()
	v.SetConfigName("config")
	v.SetConfigType("yaml")
	// Discovery is optional if the OS cannot resolve a user configuration
	// directory. Do not search the working directory or legacy /etc/msm.conf.
	if dir, err := os.UserConfigDir(); err == nil {
		v.AddConfigPath(filepath.Join(dir, "msm"))
	}
	return v
}

// Read must run after Cobra parses flags. Missing optional configuration is
// allowed; explicit missing files, malformed files and unknown settings are
// errors. The caller owns v and must not mutate/read it concurrently.
func Read(v *viper.Viper, file string) (Settings, error) {
	if file != "" {
		v.SetConfigFile(file)
	}
	if err := v.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if file != "" || !errors.As(err, &notFound) {
			return Settings{}, fmt.Errorf("read configuration: %w", err)
		}
	}
	var settings Settings
	if err := v.UnmarshalExact(&settings); err != nil {
		return Settings{}, fmt.Errorf("decode configuration: %w", err)
	}
	return settings, nil
}
