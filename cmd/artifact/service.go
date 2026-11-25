package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/deastl/artifact-host/internal/service"
)

func newServiceCmd(app *appContext) *cobra.Command {
	svcCmd := &cobra.Command{
		Use:   "service",
		Short: "Manage the artifact-host systemd user service",
	}

	svcCmd.AddCommand(
		newServiceInstallCmd(app),
		newServiceUninstallCmd(app),
		newServiceStartCmd(app),
		newServiceStopCmd(app),
		newServiceRestartCmd(app),
		newServiceStatusCmd(app),
		newServiceLogsCmd(app),
	)

	return svcCmd
}

func newServiceInstallCmd(app *appContext) *cobra.Command {
	var (
		addr      string
		dataDir   string
		binary    string
		chromeBin string
		noStart   bool
	)

	cmd := &cobra.Command{
		Use:   "install",
		Short: "Install and optionally start the systemd user unit",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			binPath := binary
			if binPath == "" {
				exe, err := os.Executable()
				if err != nil {
					return fmt.Errorf("resolve executable path: %w", err)
				}
				if resolved, err := filepath.EvalSymlinks(exe); err == nil {
					exe = resolved
				}
				binPath = exe
			} else {
				if abs, err := filepath.Abs(binPath); err == nil {
					binPath = abs
				}
			}

			unitPath, err := app.serviceManager().Install(service.UnitConfig{
				BinaryPath: binPath,
				Addr:       addr,
				DataDir:    dataDir,
				ChromeBin:  chromeBin,
			}, !noStart)
			if err != nil {
				return err
			}
			if app.jsonOut {
				return writeJSON(app.out, map[string]any{
					"installed": true,
					"unit_path": unitPath,
					"started":   !noStart,
				})
			}
			fmt.Fprintf(app.out, "Installed systemd user unit at %s\n", unitPath)
			if !noStart {
				fmt.Fprintf(app.out, "Enabled and started %s\n", service.UnitName)
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&addr, "addr", service.DefaultServeAddr, "Listen address for the daemon")
	cmd.Flags().StringVar(&dataDir, "data-dir", defaultDataDir(), "Data directory for the daemon")
	cmd.Flags().StringVar(&binary, "binary", "", "Path to the artifact binary (defaults to current executable)")
	cmd.Flags().StringVar(&chromeBin, "chrome-bin", "", "Path to Chromium/Chrome binary for previews")
	cmd.Flags().BoolVar(&noStart, "no-start", false, "Write and reload unit without enabling or starting it")

	return cmd
}

func newServiceUninstallCmd(app *appContext) *cobra.Command {
	return &cobra.Command{
		Use:   "uninstall",
		Short: "Stop, disable, and remove the systemd user unit",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := app.serviceManager().Uninstall(); err != nil {
				return err
			}
			if app.jsonOut {
				return writeJSON(app.out, map[string]any{"uninstalled": true})
			}
			fmt.Fprintf(app.out, "Uninstalled %s\n", service.UnitName)
			return nil
		},
	}
}

func newServiceStartCmd(app *appContext) *cobra.Command {
	return &cobra.Command{
		Use:   "start",
		Short: "Start the systemd user service",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := app.serviceManager().Start(); err != nil {
				return err
			}
			if app.jsonOut {
				return writeJSON(app.out, map[string]any{"started": true})
			}
			fmt.Fprintf(app.out, "Started %s\n", service.UnitName)
			return nil
		},
	}
}

func newServiceStopCmd(app *appContext) *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the systemd user service",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := app.serviceManager().Stop(); err != nil {
				return err
			}
			if app.jsonOut {
				return writeJSON(app.out, map[string]any{"stopped": true})
			}
			fmt.Fprintf(app.out, "Stopped %s\n", service.UnitName)
			return nil
		},
	}
}

func newServiceRestartCmd(app *appContext) *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Restart the systemd user service",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if err := app.serviceManager().Restart(); err != nil {
				return err
			}
			if app.jsonOut {
				return writeJSON(app.out, map[string]any{"restarted": true})
			}
			fmt.Fprintf(app.out, "Restarted %s\n", service.UnitName)
			return nil
		},
	}
}

func newServiceStatusCmd(app *appContext) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show systemd status for artifact-host.service",
		Args:  cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			out, err := app.serviceManager().Status()
			if out != "" {
				fmt.Fprintln(app.out, out)
			}
			return err
		},
	}
}

func newServiceLogsCmd(app *appContext) *cobra.Command {
	var (
		follow bool
		lines  int
	)

	cmd := &cobra.Command{
		Use:   "logs",
		Short: "Show journald logs for artifact-host.service",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return app.serviceManager().LogsContext(cmd.Context(), follow, lines)
		},
	}

	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow log output")
	cmd.Flags().IntVarP(&lines, "lines", "n", 50, "Number of recent log lines to show")

	return cmd
}
