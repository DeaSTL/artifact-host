package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestRenderUnit(t *testing.T) {
	_, err := RenderUnit(UnitConfig{})
	if err == nil {
		t.Fatal("expected error when BinaryPath is empty")
	}

	unit, err := RenderUnit(UnitConfig{
		BinaryPath: "/usr/local/bin/artifact",
		Addr:       "127.0.0.1:8080",
		DataDir:    "/var/lib/artifact host",
		ChromeBin:  "/usr/bin/chromium",
	})
	if err != nil {
		t.Fatalf("RenderUnit failed: %v", err)
	}

	wantSubstrings := []string{
		"[Unit]",
		"[Service]",
		"[Install]",
		`ExecStart=/usr/local/bin/artifact serve --addr 127.0.0.1:8080 --data-dir "/var/lib/artifact host" --chrome-bin /usr/bin/chromium`,
		"Restart=on-failure",
		"WantedBy=default.target",
	}
	for _, sub := range wantSubstrings {
		if !strings.Contains(unit, sub) {
			t.Errorf("rendered unit missing %q:\n%s", sub, unit)
		}
	}
}

func TestUnitPathRespectsXDGConfigHome(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", tmp)

	p, err := UnitPath()
	if err != nil {
		t.Fatalf("UnitPath failed: %v", err)
	}
	want := filepath.Join(tmp, "systemd", "user", UnitName)
	if p != want {
		t.Fatalf("UnitPath() = %q, want %q", p, want)
	}
}

func TestManagerInstallUninstallLifecycle(t *testing.T) {
	cfgHome := t.TempDir()
	var calls [][]string

	mgr := &Manager{
		ConfigHome: cfgHome,
		Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
			calls = append(calls, append([]string{name}, args...))
			if len(args) >= 2 && args[1] == "status" {
				return []byte("active (running)\n"), nil
			}
			if len(args) >= 2 && args[1] == "is-active" {
				return []byte("active\n"), nil
			}
			return []byte("ok\n"), nil
		},
	}

	installed, err := mgr.IsInstalled()
	if err != nil || installed {
		t.Fatalf("expected not installed, got installed=%v err=%v", installed, err)
	}

	unitPath, err := mgr.Install(UnitConfig{
		BinaryPath: "/opt/bin/artifact",
		Addr:       "127.0.0.1:7337",
		DataDir:    "/tmp/artifacts",
	}, true)
	if err != nil {
		t.Fatalf("Install failed: %v", err)
	}

	data, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatalf("unit file not written: %v", err)
	}
	if !strings.Contains(string(data), "ExecStart=/opt/bin/artifact serve --addr 127.0.0.1:7337 --data-dir /tmp/artifacts") {
		t.Fatalf("unexpected unit file contents:\n%s", string(data))
	}

	installed, err = mgr.IsInstalled()
	if err != nil || !installed {
		t.Fatalf("expected installed=true, got %v, err=%v", installed, err)
	}

	wantInstallCalls := [][]string{
		{"systemctl", "--user", "daemon-reload"},
		{"systemctl", "--user", "enable", "--now", UnitName},
	}
	if !reflect.DeepEqual(calls, wantInstallCalls) {
		t.Fatalf("install calls mismatch:\ngot:  %v\nwant: %v", calls, wantInstallCalls)
	}

	calls = nil
	if err := mgr.Start(); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	if err := mgr.Stop(); err != nil {
		t.Fatalf("Stop failed: %v", err)
	}
	if err := mgr.Restart(); err != nil {
		t.Fatalf("Restart failed: %v", err)
	}
	st, err := mgr.Status()
	if err != nil || st != "active (running)" {
		t.Fatalf("Status failed: st=%q err=%v", st, err)
	}
	active, err := mgr.ActiveState()
	if err != nil || active != "active" {
		t.Fatalf("ActiveState failed: active=%q err=%v", active, err)
	}

	wantControlCalls := [][]string{
		{"systemctl", "--user", "start", UnitName},
		{"systemctl", "--user", "stop", UnitName},
		{"systemctl", "--user", "restart", UnitName},
		{"systemctl", "--user", "status", "--no-pager", UnitName},
		{"systemctl", "--user", "is-active", UnitName},
	}
	if !reflect.DeepEqual(calls, wantControlCalls) {
		t.Fatalf("control calls mismatch:\ngot:  %v\nwant: %v", calls, wantControlCalls)
	}

	calls = nil
	if err := mgr.Uninstall(); err != nil {
		t.Fatalf("Uninstall failed: %v", err)
	}
	if _, err := os.Stat(unitPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected unit file removed, stat err=%v", err)
	}
	wantUninstallCalls := [][]string{
		{"systemctl", "--user", "disable", "--now", UnitName},
		{"systemctl", "--user", "daemon-reload"},
	}
	if !reflect.DeepEqual(calls, wantUninstallCalls) {
		t.Fatalf("uninstall calls mismatch:\ngot:  %v\nwant: %v", calls, wantUninstallCalls)
	}
}

func TestManagerLogs(t *testing.T) {
	var gotName string
	var gotArgs []string
	var outBuf bytes.Buffer

	mgr := &Manager{
		Stdout: &outBuf,
		Stderr: &bytes.Buffer{},
		Stream: func(_ context.Context, stdout, _ io.Writer, name string, args ...string) error {
			gotName = name
			gotArgs = args
			_, _ = io.WriteString(stdout, "log line 1\n")
			return nil
		},
	}

	if err := mgr.Logs(true, 100); err != nil {
		t.Fatalf("Logs failed: %v", err)
	}
	if gotName != "journalctl" {
		t.Fatalf("expected journalctl, got %q", gotName)
	}
	wantArgs := []string{"--user", "-u", UnitName, "--no-pager", "-n", "100", "-f"}
	if !reflect.DeepEqual(gotArgs, wantArgs) {
		t.Fatalf("args = %v, want %v", gotArgs, wantArgs)
	}
	if outBuf.String() != "log line 1\n" {
		t.Fatalf("unexpected log output: %q", outBuf.String())
	}
}
