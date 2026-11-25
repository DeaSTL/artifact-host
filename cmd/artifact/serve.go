package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newServeCmd(_ *appContext) *cobra.Command {
	var (
		listenAddr string
		dataDir    string
		chromeBin  string
		noPreviews bool
		workers    int
	)

	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the artifact-host HTTP daemon",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if RunServer == nil {
				return errors.New("server runtime is not wired into this build")
			}
			if workers < 0 {
				return fmt.Errorf("invalid --workers value %d: must be >= 0", workers)
			}
			addr := strings.TrimSpace(listenAddr)
			addr = strings.TrimPrefix(addr, "http://")
			addr = strings.TrimPrefix(addr, "https://")
			addr = strings.TrimRight(addr, "/")

			return RunServer(cmd.Context(), ServeConfig{
				Addr:       addr,
				DataDir:    dataDir,
				ChromeBin:  chromeBin,
				NoPreviews: noPreviews,
				Workers:    workers,
				Version:    Version,
			})
		},
	}

	cmd.Flags().StringVar(&listenAddr, "addr", "127.0.0.1:7337", "HTTP listen address")
	cmd.Flags().StringVar(&dataDir, "data-dir", defaultDataDir(), "Directory for SQLite database, blobs, and preview cache")
	cmd.Flags().StringVar(&chromeBin, "chrome-bin", "", "Path to Chromium/Chrome binary for headless previews")
	cmd.Flags().BoolVar(&noPreviews, "no-previews", false, "Disable headless browser preview generation")
	cmd.Flags().IntVar(&workers, "workers", 2, "Number of concurrent preview renderer workers")

	return cmd
}
