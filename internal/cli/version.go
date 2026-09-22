package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/buildinfo"
)

func newVersionCommand(info buildinfo.Info) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the Go-port version and source commit",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), info.String())
			return err
		},
	}
}
