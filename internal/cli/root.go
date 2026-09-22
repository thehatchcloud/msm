package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/buildinfo"
	"github.com/thehatchcloud/msm/internal/config"
)

// NewRootCommand constructs a new command tree and a private Viper instance.
// No init hooks or package-level command/configuration singletons are used.
func NewRootCommand(info buildinfo.Info) (*cobra.Command, error) {
	v := config.New()
	var configFile string
	root := &cobra.Command{
		Use:   "msm",
		Short: "Manage Minecraft servers",
		Long: `Minecraft Server Manager: Go port foundation.

Server management is not implemented yet. This binary never invokes the
legacy Bash manager and is not a replacement for a production installation.`,
		Version:       info.String(),
		SilenceErrors: true,
		SilenceUsage:  true,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			settings, err := config.Read(v, configFile)
			if err != nil {
				return err
			}
			// Keep Viper at the application boundary. Future manager services
			// receive typed settings, not a mutable configuration singleton.
			if settings.Debug {
				_, err = fmt.Fprintln(cmd.ErrOrStderr(), "msm: debug: configuration loaded")
			}
			return err
		},
	}
	root.SetVersionTemplate("{{.Version}}\n")
	// Register these before command discovery so either ordering with a
	// value-taking persistent flag (for example --help --config path) works.
	root.InitDefaultHelpFlag()
	root.InitDefaultVersionFlag()
	root.PersistentFlags().StringVar(&configFile, "config", "", "configuration file (default: user config directory/msm/config.yaml)")
	root.PersistentFlags().Bool("debug", false, "enable diagnostic output")
	if err := v.BindPFlag("debug", root.PersistentFlags().Lookup("debug")); err != nil {
		return nil, fmt.Errorf("bind debug flag: %w", err)
	}
	root.AddCommand(newVersionCommand(info))
	return root, nil
}
