package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newVersionCmd(app *appContext) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print build version information",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if app.jsonOut {
				return writeJSON(app.out, map[string]string{"version": Version})
			}
			fmt.Fprintf(app.out, "artifact %s\n", Version)
			return nil
		},
	}
}
