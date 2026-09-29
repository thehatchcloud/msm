package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/buildinfo"
	"github.com/thehatchcloud/msm/internal/config"
	"github.com/thehatchcloud/msm/internal/lifecycle"
)

// NewRootCommand constructs a new command tree and a private Viper instance.
// No init hooks or package-level command/configuration singletons are used.
func NewRootCommand(info buildinfo.Info) (*cobra.Command, error) {
	return newRootCommand(info, defaultDeps())
}

func newRootCommand(info buildinfo.Info, d deps) (*cobra.Command, error) {
	v := config.New()
	var configFile string
	var o lifecycleOptions
	root := &cobra.Command{
		// Help is the legacy command list (help.go), not Cobra's usage.
		Use:   programName,
		Short: "Manage Minecraft servers (Go port, alpha)",
		Args:  cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServerCommand(cmd, d, o, args)
		},
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
	// msm help and msm --help print the legacy command list; help for a
	// subcommand (msm help server, msm server delete --help) stays Cobra's.
	defaultHelp := root.HelpFunc()
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if cmd == root {
			printLegacyHelp(cmd.OutOrStdout())
			return
		}
		defaultHelp(cmd, args)
	})
	// Register these before command discovery so either ordering with a
	// value-taking persistent flag (for example --help --config path) works.
	root.InitDefaultHelpFlag()
	root.InitDefaultVersionFlag()
	root.PersistentFlags().StringVar(&configFile, "config", "", "configuration file (default: user config directory/msm/config.yaml)")
	root.PersistentFlags().Bool("debug", false, "enable diagnostic output")
	if err := v.BindPFlag("debug", root.PersistentFlags().Lookup("debug")); err != nil {
		return nil, fmt.Errorf("bind debug flag: %w", err)
	}
	o.register(root)
	root.AddCommand(newVersionCommand(info), newServerCommand(d),
		newGlobalCommand(d, lifecycle.VerbStart), newGlobalCommand(d, lifecycle.VerbStop),
		newGlobalCommand(d, lifecycle.VerbRestart))
	return root, nil
}
