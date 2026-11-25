package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/deastl/artifact-host/internal/client"
	"github.com/deastl/artifact-host/internal/service"
)

type appContext struct {
	addr    string
	jsonOut bool

	in      io.Reader
	out     io.Writer
	errOut  io.Writer
	openURL func(url string) error
	svcMgr  *service.Manager
}

func defaultApp() *appContext {
	return &appContext{
		in:      os.Stdin,
		out:     os.Stdout,
		errOut:  os.Stderr,
		openURL: openWithXDG,
		svcMgr:  service.DefaultManager(),
	}
}

func (a *appContext) client() *client.Client {
	return client.New(a.addr)
}

func (a *appContext) serviceManager() *service.Manager {
	if a.svcMgr != nil {
		return a.svcMgr
	}
	return service.DefaultManager()
}

func defaultAddrFromEnv() string {
	if v := strings.TrimSpace(os.Getenv("ARTIFACT_ADDR")); v != "" {
		return v
	}
	return client.DefaultAddr
}

func defaultDataDir() string {
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		return filepath.Join(xdg, "artifact-host")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "./artifact-host-data"
	}
	return filepath.Join(home, ".local", "share", "artifact-host")
}

func openWithXDG(targetURL string) error {
	cmd := exec.Command("xdg-open", targetURL)
	return cmd.Start()
}

func newRootCmd(app *appContext) *cobra.Command {
	if app == nil {
		app = defaultApp()
	}

	root := &cobra.Command{
		Use:           "artifact",
		Short:         "Local web artifact hosting daemon and CLI",
		Long:          "Host, version, preview, and inspect local interactive HTML/JS/CSS artifacts.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.SetIn(app.in)
	root.SetOut(app.out)
	root.SetErr(app.errOut)

	root.PersistentFlags().StringVar(&app.addr, "addr", defaultAddrFromEnv(), "Artifact daemon address (env: ARTIFACT_ADDR)")
	root.PersistentFlags().BoolVar(&app.jsonOut, "json", false, "Output machine-readable JSON")

	root.AddCommand(
		newServeCmd(app),
		newPushCmd(app),
		newListCmd(app),
		newInfoCmd(app),
		newVersionsCmd(app),
		newRollbackCmd(app),
		newDeleteCmd(app),
		newPullCmd(app),
		newPreviewCmd(app),
		newSubmissionsCmd(app),
		newOpenCmd(app),
		newStatusCmd(app),
		newServiceCmd(app),
		newVersionCmd(app),
	)

	return root
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newTabWriter(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
}

func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for v := n / unit; v >= unit; v /= unit {
		div *= unit
		exp++
	}
	suffixes := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	if exp >= len(suffixes) {
		exp = len(suffixes) - 1
	}
	return fmt.Sprintf("%.1f %s", float64(n)/float64(div), suffixes[exp])
}

func formatDuration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	secs := int64(d.Seconds())
	days := secs / 86400
	secs %= 86400
	hours := secs / 3600
	secs %= 3600
	mins := secs / 60
	secs %= 60

	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh %dm %ds", hours, mins, secs)
	case mins > 0:
		return fmt.Sprintf("%dm %ds", mins, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
