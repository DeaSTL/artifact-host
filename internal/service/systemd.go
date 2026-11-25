package service

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

// UnitName is the systemd user unit name for the artifact-host daemon.
const UnitName = "artifact-host.service"

// DefaultServeAddr is the default listen address used when rendering the unit file.
const DefaultServeAddr = "127.0.0.1:7337"

//go:embed artifact-host.service.tmpl
var unitTemplateSource string

var unitTemplate = template.Must(template.New(UnitName).Parse(unitTemplateSource))

// UnitConfig configures the generated systemd user service unit.
type UnitConfig struct {
	BinaryPath string
	Addr       string
	DataDir    string
	ChromeBin  string
}

// CommandRunner executes a command and returns combined output.
type CommandRunner func(ctx context.Context, name string, args ...string) ([]byte, error)

// StreamRunner executes a command streaming output to stdout and stderr.
type StreamRunner func(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error

// Manager manages the systemd user unit with injectable command runners for testing.
type Manager struct {
	Run    CommandRunner
	Stream StreamRunner
	Stdout io.Writer
	Stderr io.Writer
	// ConfigHome overrides XDG_CONFIG_HOME when non-empty.
	ConfigHome string
}

// DefaultManager returns a Manager backed by os/exec and standard streams.
func DefaultManager() *Manager {
	return &Manager{
		Run:    execRun,
		Stream: execStream,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	}
}

func execRun(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	return cmd.CombinedOutput()
}

func execStream(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// UnitPath returns the path to ~/.config/systemd/user/artifact-host.service,
// respecting XDG_CONFIG_HOME.
func UnitPath() (string, error) {
	return DefaultManager().UnitPath()
}

// UnitPath returns the resolved unit file path for this manager.
func (m *Manager) UnitPath() (string, error) {
	configHome := strings.TrimSpace(m.ConfigHome)
	if configHome == "" {
		configHome = strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME"))
	}
	if configHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		configHome = filepath.Join(home, ".config")
	}
	return filepath.Join(configHome, "systemd", "user", UnitName), nil
}

// IsInstalled reports whether the systemd user unit file exists on disk.
func IsInstalled() (bool, error) {
	return DefaultManager().IsInstalled()
}

// IsInstalled reports whether the unit file exists at UnitPath.
func (m *Manager) IsInstalled() (bool, error) {
	p, err := m.UnitPath()
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// RenderUnit renders the systemd user service unit file contents for cfg.
func RenderUnit(cfg UnitConfig) (string, error) {
	bin := strings.TrimSpace(cfg.BinaryPath)
	if bin == "" {
		return "", errors.New("binary path is required")
	}
	addr := strings.TrimSpace(cfg.Addr)
	if addr == "" {
		addr = DefaultServeAddr
	}
	addr = strings.TrimPrefix(addr, "http://")
	addr = strings.TrimPrefix(addr, "https://")
	addr = strings.TrimRight(addr, "/")

	args := []string{
		systemdQuote(bin),
		"serve",
		"--addr", systemdQuote(addr),
	}
	if dataDir := strings.TrimSpace(cfg.DataDir); dataDir != "" {
		args = append(args, "--data-dir", systemdQuote(dataDir))
	}
	if chromeBin := strings.TrimSpace(cfg.ChromeBin); chromeBin != "" {
		args = append(args, "--chrome-bin", systemdQuote(chromeBin))
	}

	var buf bytes.Buffer
	err := unitTemplate.Execute(&buf, struct {
		ExecStart string
	}{
		ExecStart: strings.Join(args, " "),
	})
	if err != nil {
		return "", fmt.Errorf("render unit template: %w", err)
	}
	return buf.String(), nil
}

func systemdQuote(arg string) string {
	needsQuote := false
	for _, r := range arg {
		if r == ' ' || r == '\t' || r == '"' || r == '\'' || r == '\\' || r == '%' {
			needsQuote = true
			break
		}
	}
	if !needsQuote {
		return arg
	}
	replacer := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		`%`, `%%`,
	)
	return `"` + replacer.Replace(arg) + `"`
}

// Install writes the unit file, reloads the user systemd daemon, and optionally
// enables and starts artifact-host.service.
func Install(cfg UnitConfig, enableAndStart bool) (string, error) {
	return DefaultManager().Install(cfg, enableAndStart)
}

// Install writes the unit file and runs systemctl daemon-reload (and optionally enable --now).
func (m *Manager) Install(cfg UnitConfig, enableAndStart bool) (string, error) {
	unitPath, err := m.UnitPath()
	if err != nil {
		return "", err
	}
	content, err := RenderUnit(cfg)
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(filepath.Dir(unitPath), 0o755); err != nil {
		return "", fmt.Errorf("create systemd user directory: %w", err)
	}
	if err := os.WriteFile(unitPath, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("write unit file %s: %w", unitPath, err)
	}

	if out, err := m.runSystemctl(context.Background(), "daemon-reload"); err != nil {
		return unitPath, fmt.Errorf("systemctl --user daemon-reload: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	if enableAndStart {
		if out, err := m.runSystemctl(context.Background(), "enable", "--now", UnitName); err != nil {
			return unitPath, fmt.Errorf("systemctl --user enable --now %s: %w (%s)", UnitName, err, strings.TrimSpace(string(out)))
		}
	}
	return unitPath, nil
}

// Uninstall disables and stops the unit if present, removes the unit file, and reloads systemd.
func Uninstall() error {
	return DefaultManager().Uninstall()
}

// Uninstall disables and stops the unit, deletes its unit file, and reloads systemd.
func (m *Manager) Uninstall() error {
	unitPath, err := m.UnitPath()
	if err != nil {
		return err
	}

	// Best-effort disable and stop; ignore failure if the unit is not currently active/loaded.
	_, _ = m.runSystemctl(context.Background(), "disable", "--now", UnitName)

	if err := os.Remove(unitPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove unit file %s: %w", unitPath, err)
	}

	if out, err := m.runSystemctl(context.Background(), "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Start starts artifact-host.service via systemctl --user.
func Start() error {
	return DefaultManager().Start()
}

// Start starts the user service.
func (m *Manager) Start() error {
	if out, err := m.runSystemctl(context.Background(), "start", UnitName); err != nil {
		return fmt.Errorf("start %s: %w (%s)", UnitName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Stop stops artifact-host.service via systemctl --user.
func Stop() error {
	return DefaultManager().Stop()
}

// Stop stops the user service.
func (m *Manager) Stop() error {
	if out, err := m.runSystemctl(context.Background(), "stop", UnitName); err != nil {
		return fmt.Errorf("stop %s: %w (%s)", UnitName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Restart restarts artifact-host.service via systemctl --user.
func Restart() error {
	return DefaultManager().Restart()
}

// Restart restarts the user service.
func (m *Manager) Restart() error {
	if out, err := m.runSystemctl(context.Background(), "restart", UnitName); err != nil {
		return fmt.Errorf("restart %s: %w (%s)", UnitName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Status returns the output of systemctl --user status artifact-host.service.
func Status() (string, error) {
	return DefaultManager().Status()
}

// Status invokes systemctl --user status --no-pager artifact-host.service.
func (m *Manager) Status() (string, error) {
	out, err := m.runSystemctl(context.Background(), "status", "--no-pager", UnitName)
	return strings.TrimRight(string(out), "\n"), err
}

// ActiveState returns the concise systemd active state (e.g. "active", "inactive", "failed").
func ActiveState() (string, error) {
	return DefaultManager().ActiveState()
}

// ActiveState invokes systemctl --user is-active artifact-host.service.
func (m *Manager) ActiveState() (string, error) {
	out, err := m.runSystemctl(context.Background(), "is-active", UnitName)
	state := strings.TrimSpace(string(out))
	if state != "" {
		return state, nil
	}
	return "unknown", err
}

// Logs streams logs for artifact-host.service via journalctl --user.
func Logs(follow bool, lines int) error {
	return DefaultManager().Logs(follow, lines)
}

// Logs invokes journalctl --user -u artifact-host.service.
func (m *Manager) Logs(follow bool, lines int) error {
	return m.LogsContext(context.Background(), follow, lines)
}

// LogsContext invokes journalctl --user -u artifact-host.service with context cancellation.
func (m *Manager) LogsContext(ctx context.Context, follow bool, lines int) error {
	args := []string{"--user", "-u", UnitName, "--no-pager"}
	if lines > 0 {
		args = append(args, "-n", strconv.Itoa(lines))
	}
	if follow {
		args = append(args, "-f")
	}
	stream := m.Stream
	if stream == nil {
		stream = execStream
	}
	stdout := m.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := m.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}
	return stream(ctx, stdout, stderr, "journalctl", args...)
}

func (m *Manager) runSystemctl(ctx context.Context, args ...string) ([]byte, error) {
	run := m.Run
	if run == nil {
		run = execRun
	}
	fullArgs := append([]string{"--user"}, args...)
	return run(ctx, "systemctl", fullArgs...)
}
