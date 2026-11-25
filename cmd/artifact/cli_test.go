package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deastl/artifact-host/internal/client"
	"github.com/deastl/artifact-host/internal/model"
	"github.com/deastl/artifact-host/internal/service"
)

func runCLI(t *testing.T, app *appContext, args ...string) (string, string, error) {
	t.Helper()
	var outBuf, errBuf bytes.Buffer
	app.out = &outBuf
	app.errOut = &errBuf
	if app.in == nil {
		app.in = strings.NewReader("")
	}
	if app.openURL == nil {
		app.openURL = func(string) error { return nil }
	}

	cmd := newRootCmd(app)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

func TestServeCommandHook(t *testing.T) {
	orig := RunServer
	defer func() { RunServer = orig }()

	RunServer = nil
	_, _, err := runCLI(t, &appContext{}, "serve")
	if err == nil || !strings.Contains(err.Error(), "not wired") {
		t.Fatalf("expected not wired error, got %v", err)
	}

	var gotCfg ServeConfig
	RunServer = func(_ context.Context, cfg ServeConfig) error {
		gotCfg = cfg
		return nil
	}

	_, _, err = runCLI(t, &appContext{},
		"serve",
		"--addr", "127.0.0.1:9999",
		"--data-dir", "/tmp/ah-test",
		"--chrome-bin", "/usr/bin/chromium",
		"--no-previews",
		"--workers", "4",
	)
	if err != nil {
		t.Fatalf("serve failed: %v", err)
	}
	if gotCfg.Addr != "127.0.0.1:9999" || gotCfg.DataDir != "/tmp/ah-test" || gotCfg.ChromeBin != "/usr/bin/chromium" || !gotCfg.NoPreviews || gotCfg.Workers != 4 {
		t.Fatalf("unexpected ServeConfig: %+v", gotCfg)
	}
}

func TestPushCommandSingleFileAndDir(t *testing.T) {
	work := t.TempDir()
	entryFile := filepath.Join(work, "main.html")
	if err := os.WriteFile(entryFile, []byte("<html>ok</html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	extraAsset := filepath.Join(work, "questions.json")
	if err := os.WriteFile(extraAsset, []byte(`{"q":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	staticDir := filepath.Join(work, "static")
	if err := os.MkdirAll(filepath.Join(staticDir, "assets"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staticDir, "assets", "style.css"), []byte("body{}"), 0o644); err != nil {
		t.Fatal(err)
	}

	gotFiles := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/artifacts/quiz-1/versions" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			data, _ := io.ReadAll(part)
			_, params, _ := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			if params["name"] == "file" {
				gotFiles[params["filename"]] = string(data)
			}
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(model.PushResponse{
			Artifact: model.Artifact{Slug: "quiz-1", LatestVersion: 1},
			Version:  model.Version{Slug: "quiz-1", Number: 1, FileCount: len(gotFiles), TotalSize: 40, PreviewStatus: model.PreviewPending},
			URL:      "http://127.0.0.1:7337/a/quiz-1",
			RawURL:   "http://127.0.0.1:7337/raw/quiz-1/v/1/index.html",
		})
	}))
	defer srv.Close()

	var opened string
	app := &appContext{
		openURL: func(u string) error {
			opened = u
			return nil
		},
	}

	out, _, err := runCLI(t, app,
		"--addr", srv.URL,
		"push", "quiz-1", entryFile,
		"--dir", staticDir,
		"--asset", "data/questions.json="+extraAsset,
		"--title", "Quiz 1",
		"--note", "v1",
		"--tag", "quiz",
		"--open",
	)
	if err != nil {
		t.Fatalf("push failed: %v", err)
	}
	if !strings.Contains(out, "quiz-1") || !strings.Contains(out, "v1") {
		t.Fatalf("unexpected push output:\n%s", out)
	}
	if opened != "http://127.0.0.1:7337/a/quiz-1" {
		t.Fatalf("expected browser open URL, got %q", opened)
	}
	if gotFiles["index.html"] != "<html>ok</html>" || gotFiles["data/questions.json"] != `{"q":1}` || gotFiles["assets/style.css"] != "body{}" {
		t.Fatalf("uploaded files mismatch: %+v", gotFiles)
	}
}

func TestArtifactLifecycleCommands(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	var deletedArtifact atomic.Bool
	var deletedVersion atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/artifacts":
			_ = json.NewEncoder(w).Encode(client.ListResponse{
				Artifacts: []model.Artifact{
					{
						Slug:                "board",
						Title:               "Kanban Board",
						LatestVersion:       2,
						SubmissionCount:     1,
						LatestPreviewStatus: model.PreviewReady,
						Tags:                []string{"ui", "tools"},
						UpdatedAt:           now,
					},
				},
				Tags: []string{"ui", "tools"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/artifacts/board":
			v := model.Version{
				Slug:          "board",
				Number:        2,
				EntryFile:     "index.html",
				FileCount:     1,
				TotalSize:     12,
				PreviewStatus: model.PreviewReady,
				CreatedAt:     now,
				Assets:        []model.Asset{{Path: "index.html", Size: 12, ContentType: "text/html"}},
			}
			_ = json.NewEncoder(w).Encode(model.ArtifactDetail{
				Artifact: model.Artifact{
					Slug:          "board",
					Title:         "Kanban Board",
					LatestVersion: 2,
					VersionCount:  2,
					UpdatedAt:     now,
				},
				CurrentVersion: &v,
				Versions:       []model.Version{v},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/artifacts/board/versions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"versions": []model.Version{
					{Number: 2, FileCount: 1, TotalSize: 12, PreviewStatus: model.PreviewReady, Note: "fix", CreatedAt: now},
					{Number: 1, FileCount: 1, TotalSize: 10, PreviewStatus: model.PreviewReady, Note: "init", CreatedAt: now},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/artifacts/board/rollback":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(model.PushResponse{
				Artifact: model.Artifact{Slug: "board", LatestVersion: 3},
				Version:  model.Version{Slug: "board", Number: 3},
				URL:      "http://127.0.0.1:7337/a/board",
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/artifacts/board/versions/2/preview/refresh":
			_ = json.NewEncoder(w).Encode(client.PreviewRefreshResponse{
				Status:     model.PreviewReady,
				PreviewURL: "/previews/board/v/2.png",
			})
		case r.Method == http.MethodGet && r.URL.Path == "/raw/board/v/2/index.html":
			_, _ = w.Write([]byte("<h1>OK</h1>"))
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/artifacts/board/versions/1":
			deletedVersion.Store(1)
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/artifacts/board":
			deletedArtifact.Store(true)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// list
	out, _, err := runCLI(t, &appContext{}, "--addr", srv.URL, "list")
	if err != nil || !strings.Contains(out, "board") || !strings.Contains(out, "Kanban Board") {
		t.Fatalf("list failed: err=%v out=%q", err, out)
	}

	// info
	out, _, err = runCLI(t, &appContext{}, "--addr", srv.URL, "info", "board")
	if err != nil || !strings.Contains(out, "index.html") {
		t.Fatalf("info failed: err=%v out=%q", err, out)
	}

	// versions
	out, _, err = runCLI(t, &appContext{}, "--addr", srv.URL, "versions", "board")
	if err != nil || !strings.Contains(out, "v2") || !strings.Contains(out, "init") {
		t.Fatalf("versions failed: err=%v out=%q", err, out)
	}

	// rollback
	out, _, err = runCLI(t, &appContext{}, "--addr", srv.URL, "rollback", "board", "1", "--note", "restore")
	if err != nil || !strings.Contains(out, "v3") {
		t.Fatalf("rollback failed: err=%v out=%q", err, out)
	}

	// preview --refresh
	out, _, err = runCLI(t, &appContext{}, "--addr", srv.URL, "preview", "board", "--refresh")
	if err != nil || !strings.Contains(out, "/previews/board/v/2.png") {
		t.Fatalf("preview --refresh failed: err=%v out=%q", err, out)
	}

	// pull
	pullDir := filepath.Join(t.TempDir(), "pulled")
	out, _, err = runCLI(t, &appContext{}, "--addr", srv.URL, "pull", "board", "--out", pullDir)
	if err != nil || !strings.Contains(out, pullDir) {
		t.Fatalf("pull failed: err=%v out=%q", err, out)
	}
	b, err := os.ReadFile(filepath.Join(pullDir, "index.html"))
	if err != nil || string(b) != "<h1>OK</h1>" {
		t.Fatalf("pulled file mismatch: %q err=%v", string(b), err)
	}

	// open --raw
	var openedURL string
	_, _, err = runCLI(t, &appContext{
		openURL: func(u string) error {
			openedURL = u
			return nil
		},
	}, "--addr", srv.URL, "open", "board", "--raw")
	if err != nil || !strings.HasSuffix(openedURL, "/raw/board/v/2/index.html") {
		t.Fatalf("open --raw failed: err=%v opened=%q", err, openedURL)
	}

	// delete aborted by prompt
	out, _, err = runCLI(t, &appContext{in: strings.NewReader("n\n")}, "--addr", srv.URL, "delete", "board")
	if err != nil || !strings.Contains(out, "Aborted") || deletedArtifact.Load() {
		t.Fatalf("delete abort failed: err=%v out=%q deleted=%v", err, out, deletedArtifact.Load())
	}

	// delete version with --yes
	_, _, err = runCLI(t, &appContext{}, "--addr", srv.URL, "delete", "board", "--version", "1", "--yes")
	if err != nil || deletedVersion.Load() != 1 {
		t.Fatalf("delete version failed: err=%v deletedVersion=%d", err, deletedVersion.Load())
	}

	// delete artifact confirmed interactively
	_, _, err = runCLI(t, &appContext{in: strings.NewReader("yes\n")}, "--addr", srv.URL, "delete", "board")
	if err != nil || !deletedArtifact.Load() {
		t.Fatalf("delete confirm failed: err=%v deleted=%v", err, deletedArtifact.Load())
	}
}

func TestSubmissionsCommands(t *testing.T) {
	now := time.Date(2026, 11, 25, 13, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/artifacts/form/submissions":
			if r.URL.Query().Get("wait") == "1" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"submissions": []model.SubmissionMeta{
						{ID: 6, Slug: "form", Version: 1, Filename: "new.json", ContentType: "application/json", Size: 11, CreatedAt: now},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"submissions": []model.SubmissionMeta{
					{ID: 5, Slug: "form", Version: 1, Filename: "old.json", ContentType: "application/json", Size: 10, CreatedAt: now},
				},
			})
		case "/api/v1/submissions/5":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Submission-Filename", "old.json")
			_, _ = w.Write([]byte(`{"a":true}`))
		case "/api/v1/submissions/6":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Submission-Filename", "new.json")
			_, _ = w.Write([]byte(`{"b":false}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	// list
	out, _, err := runCLI(t, &appContext{}, "--addr", srv.URL, "submissions", "form")
	if err != nil || !strings.Contains(out, "old.json") {
		t.Fatalf("submissions list failed: err=%v out=%q", err, out)
	}

	// --latest to stdout
	out, _, err = runCLI(t, &appContext{}, "--addr", srv.URL, "submissions", "form", "--latest")
	if err != nil || out != `{"a":true}` {
		t.Fatalf("submissions --latest failed: err=%v out=%q", err, out)
	}

	// --wait to file
	dest := filepath.Join(t.TempDir(), "out.json")
	out, _, err = runCLI(t, &appContext{}, "--addr", srv.URL, "submissions", "form", "--wait", "--timeout", "2s", "--out", dest)
	if err != nil || !strings.Contains(out, "Saved submission 6") {
		t.Fatalf("submissions --wait failed: err=%v out=%q", err, out)
	}
	b, err := os.ReadFile(dest)
	if err != nil || string(b) != `{"b":false}` {
		t.Fatalf("saved submission mismatch: %q err=%v", string(b), err)
	}
}

func TestStatusServiceVersionAndCompletion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/status" {
			_ = json.NewEncoder(w).Encode(model.DaemonStatus{
				Status:            "ok",
				Version:           "0.1.0",
				UptimeSeconds:     125,
				Addr:              "127.0.0.1:7337",
				DataDir:           "/tmp/data",
				ArtifactCount:     2,
				VersionCount:      5,
				SubmissionCount:   1,
				PreviewCacheCount: 4,
				PreviewCacheBytes: 2048,
				ChromeAvailable:   true,
				ChromePath:        "/usr/bin/chromium",
			})
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	cfgHome := t.TempDir()
	mgr := &service.Manager{
		ConfigHome: cfgHome,
		Run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			if len(args) >= 2 && args[1] == "is-active" {
				return []byte("active\n"), nil
			}
			if len(args) >= 2 && args[1] == "status" {
				return []byte("active (running)\n"), nil
			}
			return []byte("ok\n"), nil
		},
		Stream: func(_ context.Context, stdout, _ io.Writer, _ string, _ ...string) error {
			_, _ = io.WriteString(stdout, "journal line\n")
			return nil
		},
	}

	// service install
	out, _, err := runCLI(t, &appContext{svcMgr: mgr},
		"service", "install",
		"--binary", "/usr/local/bin/artifact",
		"--addr", "127.0.0.1:7337",
		"--data-dir", "/tmp/data",
	)
	if err != nil || !strings.Contains(out, "Installed systemd user unit") {
		t.Fatalf("service install failed: err=%v out=%q", err, out)
	}

	// status (daemon + systemd)
	out, _, err = runCLI(t, &appContext{svcMgr: mgr}, "--addr", srv.URL, "status")
	if err != nil {
		t.Fatalf("status failed: %v", err)
	}
	for _, want := range []string{"ok", "2.0 KiB", "available (/usr/bin/chromium)", "installed (active)"} {
		if !strings.Contains(out, want) {
			t.Errorf("status output missing %q:\n%s", want, out)
		}
	}

	// service start/stop/restart/status/logs/uninstall
	for _, sub := range [][]string{
		{"service", "start"},
		{"service", "stop"},
		{"service", "restart"},
		{"service", "status"},
		{"service", "logs", "-n", "10"},
		{"service", "uninstall"},
	} {
		var logBuf bytes.Buffer
		mgr.Stdout = &logBuf
		if _, _, err := runCLI(t, &appContext{svcMgr: mgr}, sub...); err != nil {
			t.Fatalf("%v failed: %v", sub, err)
		}
	}

	// version
	out, _, err = runCLI(t, &appContext{}, "version")
	if err != nil || !strings.Contains(out, "artifact ") {
		t.Fatalf("version failed: err=%v out=%q", err, out)
	}

	// built-in completion
	out, _, err = runCLI(t, &appContext{}, "completion", "bash")
	if err != nil || !strings.Contains(out, "artifact") {
		t.Fatalf("completion bash failed: err=%v", err)
	}
}
