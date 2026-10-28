package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/thehatchcloud/msm/internal/buildinfo"
)

func newVersionCommand(info buildinfo.Info) *cobra.Command {
	return &cobra.Command{
		Use:         "version",
		Short:       "Show the Go-port version and source commit",
		Args:        cobra.NoArgs,
		Annotations: map[string]string{annJSON: "true"},
		RunE: func(cmd *cobra.Command, _ []string) error {
			if jsonRequested(cmd) {
				return writeVersionJSON(cmd.OutOrStdout(), info)
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), info.String())
			return err
		},
	}
}
