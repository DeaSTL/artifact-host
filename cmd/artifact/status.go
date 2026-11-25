package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/deastl/artifact-host/internal/model"
)

type statusReport struct {
	*model.DaemonStatus
	SystemdInstalled bool   `json:"systemd_installed"`
	SystemdUnitPath  string `json:"systemd_unit_path,omitempty"`
	SystemdState     string `json:"systemd_state,omitempty"`
}

func newStatusCmd(app *appContext) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon health, storage metrics, Chrome availability, and systemd state",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			mgr := app.serviceManager()
			installed, _ := mgr.IsInstalled()
			var unitPath, systemdState string
			if installed {
				unitPath, _ = mgr.UnitPath()
				systemdState, _ = mgr.ActiveState()
			}

			st, err := app.client().Status(cmd.Context())
			if err != nil {
				if installed && systemdState != "" {
					return fmt.Errorf("daemon unreachable (%v); systemd unit state: %s", err, systemdState)
				}
				return err
			}

			if app.jsonOut {
				return writeJSON(app.out, statusReport{
					DaemonStatus:     st,
					SystemdInstalled: installed,
					SystemdUnitPath:  unitPath,
					SystemdState:     systemdState,
				})
			}

			tw := newTabWriter(app.out)
			fmt.Fprintf(tw, "Status:\t%s\n", st.Status)
			fmt.Fprintf(tw, "Version:\t%s\n", st.Version)
			fmt.Fprintf(tw, "Uptime:\t%s\n", formatDuration(time.Duration(st.UptimeSeconds)*time.Second))
			fmt.Fprintf(tw, "Address:\t%s\n", st.Addr)
			fmt.Fprintf(tw, "Data Dir:\t%s\n", st.DataDir)
			fmt.Fprintf(tw, "Artifacts:\t%d\n", st.ArtifactCount)
			fmt.Fprintf(tw, "Versions:\t%d\n", st.VersionCount)
			fmt.Fprintf(tw, "Submissions:\t%d\n", st.SubmissionCount)
			fmt.Fprintf(tw, "Preview Cache:\t%d files (%s)\n", st.PreviewCacheCount, formatBytes(st.PreviewCacheBytes))
			if st.ChromeAvailable {
				if st.ChromePath != "" {
					fmt.Fprintf(tw, "Chrome:\tavailable (%s)\n", st.ChromePath)
				} else {
					fmt.Fprintf(tw, "Chrome:\tavailable\n")
				}
			} else {
				fmt.Fprintf(tw, "Chrome:\tunavailable\n")
			}
			if installed {
				state := systemdState
				if state == "" {
					state = "unknown"
				}
				fmt.Fprintf(tw, "Systemd Unit:\tinstalled (%s)\n", state)
			} else {
				fmt.Fprintf(tw, "Systemd Unit:\tnot installed\n")
			}
			return tw.Flush()
		},
	}
}
