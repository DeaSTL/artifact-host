package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deastl/artifact-host/internal/model"
)

func TestNormalizeAddr(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", DefaultAddr},
		{"   ", DefaultAddr},
		{"127.0.0.1:7337", "http://127.0.0.1:7337"},
		{"127.0.0.1:7337/", "http://127.0.0.1:7337"},
		{":7337", "http://127.0.0.1:7337"},
		{"http://localhost:8080/", "http://localhost:8080"},
		{"https://artifacts.local///", "https://artifacts.local"},
	}
	for _, tc := range tests {
		if got := NormalizeAddr(tc.in); got != tc.want {
			t.Errorf("NormalizeAddr(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestClientStatusAndErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/status":
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			_ = json.NewEncoder(w).Encode(model.DaemonStatus{
				Status:            "ok",
				Version:           "1.2.3",
				UptimeSeconds:     42,
				Addr:              "127.0.0.1:7337",
				DataDir:           "/tmp/data",
				ArtifactCount:     3,
				VersionCount:      7,
				SubmissionCount:   2,
				PreviewCacheCount: 5,
				PreviewCacheBytes: 123456,
				ChromeAvailable:   true,
				ChromePath:        "/usr/bin/chromium",
			})
		case "/api/v1/artifacts/missing":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"artifact not found"}`))
		case "/api/v1/artifacts/plainerr":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("internal failure"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	st, err := c.Status(context.Background())
	if err != nil {
		t.Fatalf("Status failed: %v", err)
	}
	if st.Status != "ok" || st.ArtifactCount != 3 || !st.ChromeAvailable {
		t.Fatalf("unexpected status: %+v", st)
	}

	_, err = c.Info(context.Background(), "missing", 0)
	if err == nil {
		t.Fatal("expected error for missing artifact")
	}
	if !IsNotFound(err) {
		t.Fatalf("expected IsNotFound=true, got %v", err)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "artifact not found" {
		t.Fatalf("unexpected APIError: %#v", err)
	}

	_, err = c.Info(context.Background(), "plainerr", 0)
	if err == nil {
		t.Fatal("expected error for plainerr")
	}
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 500 || apiErr.Message != "internal failure" {
		t.Fatalf("unexpected plain APIError: %#v", err)
	}
}

func TestClientList(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/artifacts" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
		q := r.URL.Query()
		if q.Get("q") != "quiz" || q.Get("tag") != "interactive" || q.Get("limit") != "10" || q.Get("offset") != "2" {
			t.Fatalf("unexpected query params: %v", q)
		}
		_ = json.NewEncoder(w).Encode(ListResponse{
			Artifacts: []model.Artifact{
				{Slug: "my-quiz", Title: "My Quiz", LatestVersion: 2},
			},
			Tags: []string{"interactive", "forms"},
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	resp, err := c.List(context.Background(), model.ListFilter{
		Search: "quiz",
		Tag:    "interactive",
		Limit:  10,
		Offset: 2,
	})
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}
	if len(resp.Artifacts) != 1 || resp.Artifacts[0].Slug != "my-quiz" {
		t.Fatalf("unexpected artifacts: %+v", resp.Artifacts)
	}
	if !reflect.DeepEqual(resp.Tags, []string{"interactive", "forms"}) {
		t.Fatalf("unexpected tags: %+v", resp.Tags)
	}
}

func TestClientPushMultipart(t *testing.T) {
	tmpDir := t.TempDir()
	diskFile := filepath.Join(tmpDir, "logo.svg")
	if err := os.WriteFile(diskFile, []byte("<svg></svg>"), 0o644); err != nil {
		t.Fatal(err)
	}

	var gotFields map[string][]string
	var gotFiles map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/artifacts/demo-app/versions" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		mr, err := r.MultipartReader()
		if err != nil {
			t.Fatalf("MultipartReader: %v", err)
		}
		gotFields = map[string][]string{}
		gotFiles = map[string]string{}
		for {
			part, err := mr.NextPart()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("NextPart: %v", err)
			}
			body, err := io.ReadAll(part)
			if err != nil {
				t.Fatalf("ReadAll: %v", err)
			}
			_, params, err := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
			if err != nil {
				t.Fatalf("ParseMediaType: %v", err)
			}
			name := params["name"]
			if name == "file" {
				gotFiles[params["filename"]] = string(body)
			} else {
				gotFields[name] = append(gotFields[name], string(body))
			}
		}

		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(model.PushResponse{
			Artifact: model.Artifact{Slug: "demo-app", Title: "Demo App", LatestVersion: 1},
			Version:  model.Version{Slug: "demo-app", Number: 1, EntryFile: "index.html", FileCount: 2},
			URL:      "http://127.0.0.1:7337/a/demo-app",
			RawURL:   "http://127.0.0.1:7337/raw/demo-app/v/1/index.html",
		})
	}))
	defer srv.Close()

	c := New(srv.URL)
	resp, err := c.Push(context.Background(), "demo-app", PushRequest{
		Title:       "Demo App",
		Description: "Sample artifact",
		Icon:        "file-code",
		Note:        "initial release",
		Entry:       "index.html",
		Tags:        []string{"ui", "demo"},
		Files: []PushFile{
			{Path: "index.html", Data: []byte("<h1>Hello</h1>")},
			{Path: "assets/logo.svg", SourcePath: diskFile},
		},
	})
	if err != nil {
		t.Fatalf("Push failed: %v", err)
	}
	if resp.Version.Number != 1 || resp.URL == "" {
		t.Fatalf("unexpected push response: %+v", resp)
	}
	if !reflect.DeepEqual(gotFields["tag"], []string{"ui", "demo"}) {
		t.Fatalf("tags mismatch: %v", gotFields["tag"])
	}
	if gotFields["title"][0] != "Demo App" || gotFields["note"][0] != "initial release" || gotFields["entry"][0] != "index.html" {
		t.Fatalf("fields mismatch: %v", gotFields)
	}
	wantFiles := map[string]string{
		"index.html":      "<h1>Hello</h1>",
		"assets/logo.svg": "<svg></svg>",
	}
	if !reflect.DeepEqual(gotFiles, wantFiles) {
		t.Fatalf("files mismatch: got %v, want %v", gotFiles, wantFiles)
	}
}

func TestClientVersionsRollbackDeleteRefresh(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/artifacts/app/versions":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"versions": []model.Version{{Number: 2}, {Number: 1}},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/artifacts/app/rollback":
			var req struct {
				Version int    `json:"version"`
				Note    string `json:"note"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatal(err)
			}
			if req.Version != 1 || req.Note != "revert bug" {
				t.Fatalf("unexpected rollback body: %+v", req)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(model.PushResponse{
				Artifact: model.Artifact{Slug: "app", LatestVersion: 3},
				Version:  model.Version{Slug: "app", Number: 3, Note: "revert bug"},
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/artifacts/app":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/artifacts/app/versions/2":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/artifacts/app/versions/3/preview/refresh":
			_ = json.NewEncoder(w).Encode(PreviewRefreshResponse{
				Status:     model.PreviewReady,
				PreviewURL: "/previews/app/v/3.png",
			})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.String())
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx := context.Background()

	vers, err := c.Versions(ctx, "app")
	if err != nil || len(vers) != 2 {
		t.Fatalf("Versions failed: %v, %+v", err, vers)
	}

	rb, err := c.Rollback(ctx, "app", 1, "revert bug")
	if err != nil || rb.Version.Number != 3 {
		t.Fatalf("Rollback failed: %v, %+v", err, rb)
	}

	pr, err := c.RefreshPreview(ctx, "app", 3)
	if err != nil || pr.Status != model.PreviewReady || pr.PreviewURL != "/previews/app/v/3.png" {
		t.Fatalf("RefreshPreview failed: %v, %+v", err, pr)
	}

	if err := c.DeleteVersion(ctx, "app", 2); err != nil {
		t.Fatalf("DeleteVersion failed: %v", err)
	}
	if err := c.Delete(ctx, "app"); err != nil {
		t.Fatalf("Delete failed: %v", err)
	}
}

func TestClientPull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/artifacts/site":
			v := model.Version{
				Slug:   "site",
				Number: 2,
				Assets: []model.Asset{
					{Path: "index.html", Size: 11},
					{Path: "assets/data.json", Size: 10},
				},
			}
			_ = json.NewEncoder(w).Encode(model.ArtifactDetail{
				Artifact:       model.Artifact{Slug: "site", LatestVersion: 2},
				CurrentVersion: &v,
				Versions:       []model.Version{v},
			})
		case "/raw/site/v/2/index.html":
			_, _ = w.Write([]byte("<p>Test</p>"))
		case "/raw/site/v/2/assets/data.json":
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	outDir := filepath.Join(t.TempDir(), "pulled")
	c := New(srv.URL)
	res, err := c.Pull(context.Background(), "site", 0, outDir)
	if err != nil {
		t.Fatalf("Pull failed: %v", err)
	}
	if res.Version != 2 || len(res.Files) != 2 || res.TotalBytes != 22 {
		t.Fatalf("unexpected PullResult: %+v", res)
	}

	b1, err := os.ReadFile(filepath.Join(outDir, "index.html"))
	if err != nil || string(b1) != "<p>Test</p>" {
		t.Fatalf("unexpected index.html: %q, err=%v", string(b1), err)
	}
	b2, err := os.ReadFile(filepath.Join(outDir, "assets", "data.json"))
	if err != nil || string(b2) != `{"ok":true}` {
		t.Fatalf("unexpected assets/data.json: %q, err=%v", string(b2), err)
	}
}

func TestClientSubmissionsAndWait(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/artifacts/survey/submissions":
			q := r.URL.Query()
			if q.Get("wait") == "1" {
				n := polls.Add(1)
				if n == 1 {
					_ = json.NewEncoder(w).Encode(map[string]any{"submissions": []model.SubmissionMeta{}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"submissions": []model.SubmissionMeta{
						{ID: 11, Slug: "survey", Version: 1, Filename: "result.json", ContentType: "application/json", Size: 12},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"submissions": []model.SubmissionMeta{
					{ID: 10, Slug: "survey", Version: 1, Filename: "prev.json", ContentType: "application/json", Size: 8},
				},
			})
		case "/api/v1/submissions/11":
			if r.URL.Query().Get("meta") == "1" {
				_ = json.NewEncoder(w).Encode(model.Submission{
					ID:          11,
					Slug:        "survey",
					Version:     1,
					Filename:    "result.json",
					ContentType: "application/json",
					Size:        12,
					Data:        []byte(`{"score":99}`),
				})
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Submission-Filename", "result.json")
			_, _ = w.Write([]byte(`{"score":99}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx := context.Background()

	list, err := c.Submissions(ctx, "survey", SubmissionsOptions{Version: 1})
	if err != nil || len(list) != 1 || list[0].ID != 10 {
		t.Fatalf("Submissions failed: %v, %+v", err, list)
	}

	waited, err := c.WaitSubmission(ctx, "survey", 1, 10, 5*time.Second)
	if err != nil || len(waited) != 1 || waited[0].ID != 11 {
		t.Fatalf("WaitSubmission failed: %v, %+v", err, waited)
	}
	if polls.Load() < 2 {
		t.Fatalf("expected at least 2 poll attempts, got %d", polls.Load())
	}

	dl, err := c.GetSubmission(ctx, 11)
	if err != nil {
		t.Fatalf("GetSubmission failed: %v", err)
	}
	if dl.Filename != "result.json" || dl.ContentType != "application/json" || string(dl.Data) != `{"score":99}` {
		t.Fatalf("unexpected download: %+v", dl)
	}

	meta, err := c.GetSubmissionMeta(ctx, 11)
	if err != nil || meta.ID != 11 || string(meta.Data) != `{"score":99}` {
		t.Fatalf("GetSubmissionMeta failed: %v, %+v", err, meta)
	}
}

func TestClientWaitSubmissionTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(100 * time.Millisecond):
			_ = json.NewEncoder(w).Encode(map[string]any{"submissions": []model.SubmissionMeta{}})
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	_, err := c.WaitSubmission(context.Background(), "survey", 0, 0, 150*time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected DeadlineExceeded, got %v", err)
	}
}
