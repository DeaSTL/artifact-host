package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/deastl/artifact-host/internal/client"
	"github.com/deastl/artifact-host/internal/model"
	"github.com/deastl/artifact-host/internal/preview"
	"github.com/deastl/artifact-host/internal/store"
)

type testEnv struct {
	srv      *Server
	ts       *httptest.Server
	store    *store.Store
	previews *preview.Manager
	client   *client.Client
}

func newTestEnv(t *testing.T, noPreviews bool) *testEnv {
	t.Helper()

	dataDir := t.TempDir()
	st, err := store.Open(dataDir)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}

	pm := preview.NewManager(preview.Config{
		Disabled: noPreviews,
		Workers:  1,
	}, st)

	cfg := Config{
		Addr:       "127.0.0.1:0",
		DataDir:    dataDir,
		Version:    "1.2.3-test",
		NoPreviews: noPreviews,
		Workers:    1,
	}
	srv := New(cfg, st, pm)
	ts := httptest.NewServer(srv.Handler())
	pm.SetBaseURL(ts.URL)

	t.Cleanup(func() {
		ts.Close()
		pm.Stop()
		_ = st.Close()
	})

	return &testEnv{
		srv:      srv,
		ts:       ts,
		store:    st,
		previews: pm,
		client:   client.New(ts.URL),
	}
}

func pushMultipartRaw(t *testing.T, baseURL, slug string, fields map[string][]string, files map[string][]byte) *http.Response {
	t.Helper()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	for k, vals := range fields {
		for _, v := range vals {
			if err := mw.WriteField(k, v); err != nil {
				t.Fatalf("WriteField %s: %v", k, err)
			}
		}
	}
	for relPath, content := range files {
		part, err := mw.CreateFormFile("file", relPath)
		if err != nil {
			t.Fatalf("CreateFormFile %s: %v", relPath, err)
		}
		if _, err := part.Write(content); err != nil {
			t.Fatalf("part.Write %s: %v", relPath, err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("mw.Close: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, baseURL+"/api/v1/artifacts/"+slug+"/versions", &body)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do push: %v", err)
	}
	return resp
}

func TestPushGalleryViewerRawRollbackDelete(t *testing.T) {
	env := newTestEnv(t, true)
	ctx := context.Background()

	// 1. Push v1 with nested static asset assets/data.json via raw multipart upload.
	v1HTML := `<!doctype html><html><head><title>Demo v1</title></head><body><h1>Hello v1</h1></body></html>`
	v1Data := `{"version":1,"nested":true}`
	resp := pushMultipartRaw(t, env.ts.URL, "demo-app", map[string][]string{
		"title":       {"Demo App"},
		"description": {"Interactive demo dashboard"},
		"icon":        {"layout-dashboard"},
		"note":        {"initial v1"},
		"entry":       {"index.html"},
		"tag":         {"analytics", "demo"},
	}, map[string][]byte{
		"index.html":       []byte(v1HTML),
		"assets/data.json": []byte(v1Data),
	})
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("push v1 status = %d, want 201; body=%s", resp.StatusCode, string(b))
	}
	var pushV1 model.PushResponse
	if err := json.NewDecoder(resp.Body).Decode(&pushV1); err != nil {
		t.Fatalf("decode push v1: %v", err)
	}
	if pushV1.Artifact.Slug != "demo-app" || pushV1.Version.Number != 1 {
		t.Fatalf("unexpected pushV1: %+v", pushV1)
	}
	if pushV1.Version.FileCount != 2 {
		t.Fatalf("expected 2 files in v1, got %d", pushV1.Version.FileCount)
	}

	// Verify nested path assets/data.json was preserved and typed properly.
	foundNested := false
	for _, a := range pushV1.Version.Assets {
		if a.Path == "assets/data.json" {
			foundNested = true
			if !strings.Contains(a.ContentType, "json") {
				t.Fatalf("expected JSON content type for assets/data.json, got %q", a.ContentType)
			}
		}
	}
	if !foundNested {
		t.Fatalf("nested asset assets/data.json missing from pushed assets: %+v", pushV1.Version.Assets)
	}

	// 2. Push v2 via internal/client.
	v2HTML := `<!doctype html><html><HEAD><title>Demo v2</title></HEAD><body><h1>Hello v2</h1></body></html>`
	pushV2, err := env.client.Push(ctx, "demo-app", client.PushRequest{
		Note:  "second release",
		Entry: "index.html",
		Files: []client.PushFile{
			{Path: "index.html", Data: []byte(v2HTML)},
			{Path: "assets/data.json", Data: []byte(`{"version":2}`)},
		},
	})
	if err != nil {
		t.Fatalf("client.Push v2: %v", err)
	}
	if pushV2.Version.Number != 2 || pushV2.Artifact.LatestVersion != 2 {
		t.Fatalf("unexpected pushV2: %+v", pushV2)
	}

	// Push a second artifact to test gallery search and tag filtering.
	_, err = env.client.Push(ctx, "survey-form", client.PushRequest{
		Title:       "Feedback Survey",
		Description: "Collect user responses",
		Tags:        []string{"forms"},
		Files: []client.PushFile{
			{Path: "index.html", Data: []byte("<h1>Survey</h1>")},
		},
	})
	if err != nil {
		t.Fatalf("client.Push survey-form: %v", err)
	}

	// Verify REST API list, filter, pagination, and single version endpoint.
	t.Run("API list filter pagination and get version", func(t *testing.T) {
		listed, err := env.client.List(ctx, model.ListFilter{Tag: "analytics", Limit: 5, Offset: 0})
		if err != nil {
			t.Fatalf("client.List: %v", err)
		}
		if len(listed.Artifacts) != 1 || listed.Artifacts[0].Slug != "demo-app" {
			t.Fatalf("unexpected filtered artifacts: %+v", listed.Artifacts)
		}
		if len(listed.Tags) != 3 {
			t.Fatalf("expected 3 distinct tags across store, got %v", listed.Tags)
		}

		vResp, err := http.Get(env.ts.URL + "/api/v1/artifacts/demo-app/versions/1")
		if err != nil {
			t.Fatalf("GET version 1: %v", err)
		}
		defer vResp.Body.Close()
		if vResp.StatusCode != http.StatusOK {
			t.Fatalf("GET version 1 status = %d", vResp.StatusCode)
		}
		var ver1 model.Version
		if err := json.NewDecoder(vResp.Body).Decode(&ver1); err != nil {
			t.Fatalf("decode version 1: %v", err)
		}
		if ver1.Number != 1 || ver1.Note != "initial v1" || len(ver1.Assets) != 2 {
			t.Fatalf("unexpected version 1: %+v", ver1)
		}
	})

	// 3. Static assets and bridge.js.
	t.Run("static assets and bridge.js", func(t *testing.T) {
		r, err := http.Get(env.ts.URL + "/_artifact/bridge.js")
		if err != nil {
			t.Fatalf("GET bridge.js: %v", err)
		}
		defer r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("bridge.js status = %d", r.StatusCode)
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "application/javascript") {
			t.Fatalf("bridge.js Content-Type = %q", ct)
		}
		b, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(b), "window.artifact") {
			t.Fatalf("bridge.js missing window.artifact")
		}

		cssResp, err := http.Get(env.ts.URL + "/static/app.css")
		if err != nil {
			t.Fatalf("GET app.css: %v", err)
		}
		defer cssResp.Body.Close()
		if cssResp.StatusCode != http.StatusOK {
			t.Fatalf("app.css status = %d", cssResp.StatusCode)
		}
	})

	// 4. Gallery SSR rendering & search/tag filtering.
	t.Run("gallery SSR and filters", func(t *testing.T) {
		getBody := func(path string) string {
			t.Helper()
			r, err := http.Get(env.ts.URL + path)
			if err != nil {
				t.Fatalf("GET %s: %v", path, err)
			}
			defer r.Body.Close()
			if r.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d", path, r.StatusCode)
			}
			if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
				t.Fatalf("GET %s Content-Type = %q", path, ct)
			}
			b, _ := io.ReadAll(r.Body)
			return string(b)
		}

		allHTML := getBody("/")
		if !strings.Contains(allHTML, "Demo App") || !strings.Contains(allHTML, "Feedback Survey") {
			t.Fatalf("gallery missing expected artifacts")
		}

		searchHTML := getBody("/?q=survey")
		if !strings.Contains(searchHTML, "Feedback Survey") || strings.Contains(searchHTML, "Demo App") {
			t.Fatalf("search filter did not narrow gallery properly")
		}

		tagHTML := getBody("/?tag=analytics")
		if !strings.Contains(tagHTML, "Demo App") || strings.Contains(tagHTML, "Feedback Survey") {
			t.Fatalf("tag filter did not narrow gallery properly")
		}
	})

	// 5. Viewer /a/{slug} and /a/{slug}/v/1 + 404 ErrorPage.
	t.Run("viewer SSR and 404", func(t *testing.T) {
		rLatest, err := http.Get(env.ts.URL + "/a/demo-app")
		if err != nil {
			t.Fatalf("GET /a/demo-app: %v", err)
		}
		defer rLatest.Body.Close()
		if rLatest.StatusCode != http.StatusOK {
			t.Fatalf("/a/demo-app status = %d", rLatest.StatusCode)
		}
		latestBody, _ := io.ReadAll(rLatest.Body)
		if !strings.Contains(string(latestBody), "/raw/demo-app/v/2/") {
			t.Fatalf("latest viewer missing iframe src for v2")
		}

		rV1, err := http.Get(env.ts.URL + "/a/demo-app/v/1")
		if err != nil {
			t.Fatalf("GET /a/demo-app/v/1: %v", err)
		}
		defer rV1.Body.Close()
		if rV1.StatusCode != http.StatusOK {
			t.Fatalf("/a/demo-app/v/1 status = %d", rV1.StatusCode)
		}
		v1Body, _ := io.ReadAll(rV1.Body)
		if !strings.Contains(string(v1Body), "/raw/demo-app/v/1/") || !strings.Contains(string(v1Body), "data-older-version-banner") {
			t.Fatalf("v1 viewer missing v1 iframe or older version banner")
		}

		r404, err := http.Get(env.ts.URL + "/a/does-not-exist")
		if err != nil {
			t.Fatalf("GET /a/does-not-exist: %v", err)
		}
		defer r404.Body.Close()
		if r404.StatusCode != http.StatusNotFound {
			t.Fatalf("expected 404 for unknown artifact, got %d", r404.StatusCode)
		}
		errBody, _ := io.ReadAll(r404.Body)
		if !strings.Contains(string(errBody), "Artifact not found") {
			t.Fatalf("expected ErrorPage HTML for 404")
		}
	})

	// 6. Raw HTML serving (both /v/{N} and /v{N}), redirects, bridge injection, raw/nobridge bypass, static assets.
	t.Run("raw serving and bridge injection", func(t *testing.T) {
		noRedirectClient := &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}

		for _, tc := range []struct {
			path string
			want string
		}{
			{"/raw/demo-app/v/1?preview=1", "/raw/demo-app/v/1/?preview=1"},
			{"/raw/demo-app/v1?preview=1", "/raw/demo-app/v1/?preview=1"},
			{"/raw/demo-app/v/2", "/raw/demo-app/v/2/"},
			{"/raw/demo-app/v2", "/raw/demo-app/v2/"},
		} {
			r, err := noRedirectClient.Get(env.ts.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			_ = r.Body.Close()
			if r.StatusCode != http.StatusFound {
				t.Fatalf("GET %s status = %d, want 302", tc.path, r.StatusCode)
			}
			if loc := r.Header.Get("Location"); loc != tc.want {
				t.Fatalf("GET %s Location = %q, want %q", tc.path, loc, tc.want)
			}
		}

		// Entry HTML with bridge injection on both URL styles (and case-insensitive </HEAD> on v2).
		for _, tc := range []struct {
			path    string
			version string
		}{
			{"/raw/demo-app/v/1/", "1"},
			{"/raw/demo-app/v1/", "1"},
			{"/raw/demo-app/v/2/", "2"},
			{"/raw/demo-app/v2/", "2"},
		} {
			r, err := http.Get(env.ts.URL + tc.path)
			if err != nil {
				t.Fatalf("GET %s: %v", tc.path, err)
			}
			bodyBytes, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if r.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d", tc.path, r.StatusCode)
			}
			if cc := r.Header.Get("Cache-Control"); cc != "no-cache" {
				t.Fatalf("GET %s Cache-Control = %q, want no-cache", tc.path, cc)
			}
			wantTag := fmt.Sprintf(`<script src="/_artifact/bridge.js" data-slug="demo-app" data-version="%s"></script>`, tc.version)
			bodyStr := string(bodyBytes)
			if !strings.Contains(bodyStr, wantTag) {
				t.Fatalf("GET %s missing bridge tag %q in:\n%s", tc.path, wantTag, bodyStr)
			}
			headIdx := strings.Index(strings.ToLower(bodyStr), "</head>")
			tagIdx := strings.Index(bodyStr, wantTag)
			if headIdx < 0 || tagIdx > headIdx {
				t.Fatalf("GET %s bridge tag not injected before </head>", tc.path)
			}
		}

		// ?raw=1 and ?nobridge=1 omit bridge script.
		for _, q := range []string{"?raw=1", "?nobridge=1"} {
			r, err := http.Get(env.ts.URL + "/raw/demo-app/v/1/" + q)
			if err != nil {
				t.Fatalf("GET raw %s: %v", q, err)
			}
			b, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if string(b) != v1HTML {
				t.Fatalf("GET %s expected exact original HTML, got %q", q, string(b))
			}
		}

		// Relative static asset on both URL styles.
		for _, assetPath := range []string{"/raw/demo-app/v/1/assets/data.json", "/raw/demo-app/v1/assets/data.json"} {
			r, err := http.Get(env.ts.URL + assetPath)
			if err != nil {
				t.Fatalf("GET %s: %v", assetPath, err)
			}
			b, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			if r.StatusCode != http.StatusOK {
				t.Fatalf("GET %s status = %d", assetPath, r.StatusCode)
			}
			if string(b) != v1Data {
				t.Fatalf("GET %s body = %q, want %q", assetPath, string(b), v1Data)
			}
			if strings.Contains(string(b), "bridge.js") {
				t.Fatalf("GET %s unexpectedly injected bridge.js into JSON", assetPath)
			}
		}
	})

	// 7. Rollback to v1 -> creates v3, plus UI rollback route -> creates v4.
	t.Run("rollback creates new version", func(t *testing.T) {
		rb, err := env.client.Rollback(ctx, "demo-app", 1, "revert to v1")
		if err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if rb.Version.Number != 3 || rb.Artifact.LatestVersion != 3 || rb.Version.Note != "revert to v1" {
			t.Fatalf("unexpected rollback response: %+v", rb)
		}

		// Verify v3 serves v1's content.
		r, err := http.Get(env.ts.URL + "/raw/demo-app/v/3/?raw=1")
		if err != nil {
			t.Fatalf("GET v3 raw: %v", err)
		}
		b, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if string(b) != v1HTML {
			t.Fatalf("v3 HTML = %q, want v1 HTML", string(b))
		}

		// Also verify UI rollback endpoint POST /api/v1/artifacts/{slug}/versions/{version}/rollback with empty body.
		req, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/api/v1/artifacts/demo-app/versions/2/rollback", nil)
		uiResp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("UI rollback POST: %v", err)
		}
		defer uiResp.Body.Close()
		if uiResp.StatusCode != http.StatusCreated {
			t.Fatalf("UI rollback status = %d, want 201", uiResp.StatusCode)
		}
		var rb4 model.PushResponse
		if err := json.NewDecoder(uiResp.Body).Decode(&rb4); err != nil {
			t.Fatalf("decode rb4: %v", err)
		}
		if rb4.Version.Number != 4 {
			t.Fatalf("expected v4 from UI rollback, got v%d", rb4.Version.Number)
		}
	})

	// 8. Delete version & delete artifact.
	t.Run("delete version and artifact", func(t *testing.T) {
		if err := env.client.DeleteVersion(ctx, "demo-app", 2); err != nil {
			t.Fatalf("DeleteVersion v2: %v", err)
		}
		vers, err := env.client.Versions(ctx, "demo-app")
		if err != nil {
			t.Fatalf("Versions: %v", err)
		}
		for _, v := range vers {
			if v.Number == 2 {
				t.Fatalf("version 2 still present after DeleteVersion")
			}
		}

		if err := env.client.Delete(ctx, "demo-app"); err != nil {
			t.Fatalf("Delete artifact: %v", err)
		}
		_, err = env.client.Info(ctx, "demo-app", 0)
		if !client.IsNotFound(err) {
			t.Fatalf("expected IsNotFound after Delete, got %v", err)
		}
	})
}

func TestSubmissionsAndLongPollWakeup(t *testing.T) {
	env := newTestEnv(t, true)
	ctx := context.Background()

	_, err := env.client.Push(ctx, "quiz", client.PushRequest{
		Title: "Quiz",
		Files: []client.PushFile{
			{Path: "index.html", Data: []byte("<h1>Quiz</h1>")},
		},
	})
	if err != nil {
		t.Fatalf("Push quiz: %v", err)
	}

	// Start long-poll waiter expecting a submission with id > 0.
	type pollRes struct {
		subs []model.SubmissionMeta
		err  error
		dur  time.Duration
	}
	resCh := make(chan pollRes, 1)
	go func() {
		start := time.Now()
		subs, err := env.client.Submissions(ctx, "quiz", client.SubmissionsOptions{
			Version: 1,
			AfterID: 0,
			Wait:    true,
			Timeout: 5 * time.Second,
		})
		resCh <- pollRes{subs: subs, err: err, dur: time.Since(start)}
	}()

	// Give the long-poll request a moment to register on the hub.
	time.Sleep(80 * time.Millisecond)

	// POST a submission with JSON object data (non-string data value).
	subReqBody := `{"filename":"answers.json","content_type":"application/json","data":{"score":98,"passed":true}}`
	postResp, err := http.Post(
		env.ts.URL+"/api/v1/artifacts/quiz/versions/1/submissions",
		"application/json",
		strings.NewReader(subReqBody),
	)
	if err != nil {
		t.Fatalf("POST submission: %v", err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusCreated {
		b, _ := io.ReadAll(postResp.Body)
		t.Fatalf("POST submission status = %d, body = %s", postResp.StatusCode, string(b))
	}
	var created model.SubmissionMeta
	if err := json.NewDecoder(postResp.Body).Decode(&created); err != nil {
		t.Fatalf("decode created submission: %v", err)
	}
	if created.ID <= 0 || created.Filename != "answers.json" || created.Version != 1 {
		t.Fatalf("unexpected created submission meta: %+v", created)
	}

	// Long-poll waiter must wake up promptly.
	select {
	case pr := <-resCh:
		if pr.err != nil {
			t.Fatalf("long-poll error: %v", pr.err)
		}
		if len(pr.subs) != 1 || pr.subs[0].ID != created.ID {
			t.Fatalf("unexpected long-poll subs: %+v", pr.subs)
		}
		if pr.dur > 2*time.Second {
			t.Fatalf("long-poll took too long to wake up: %v", pr.dur)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for long-poll wakeup")
	}

	// Download raw submission via /api/v1/submissions/{id} and verify headers.
	dlResp, err := http.Get(fmt.Sprintf("%s/api/v1/submissions/%d", env.ts.URL, created.ID))
	if err != nil {
		t.Fatalf("GET submission raw: %v", err)
	}
	dlBytes, _ := io.ReadAll(dlResp.Body)
	_ = dlResp.Body.Close()
	if dlResp.StatusCode != http.StatusOK {
		t.Fatalf("GET submission status = %d", dlResp.StatusCode)
	}
	if got := dlResp.Header.Get("X-Submission-Filename"); got != "answers.json" {
		t.Fatalf("X-Submission-Filename = %q, want answers.json", got)
	}
	if got := dlResp.Header.Get("Content-Disposition"); got != `attachment; filename="answers.json"` {
		t.Fatalf("Content-Disposition = %q", got)
	}
	if string(dlBytes) != `{"score":98,"passed":true}` {
		t.Fatalf("raw submission payload = %q", string(dlBytes))
	}

	// Also test UI submission route /api/v1/artifacts/{slug}/submissions/{id}.
	uiDL, err := http.Get(fmt.Sprintf("%s/api/v1/artifacts/quiz/submissions/%d", env.ts.URL, created.ID))
	if err != nil {
		t.Fatalf("GET artifact submission raw: %v", err)
	}
	uiBytes, _ := io.ReadAll(uiDL.Body)
	_ = uiDL.Body.Close()
	if uiDL.StatusCode != http.StatusOK || string(uiBytes) != `{"score":98,"passed":true}` {
		t.Fatalf("UI submission route status=%d body=%q", uiDL.StatusCode, string(uiBytes))
	}

	// Test ?meta=1 via client.GetSubmissionMeta.
	meta, err := env.client.GetSubmissionMeta(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetSubmissionMeta: %v", err)
	}
	if meta.ID != created.ID || meta.Slug != "quiz" || string(meta.Data) != `{"score":98,"passed":true}` {
		t.Fatalf("unexpected GetSubmissionMeta: %+v", meta)
	}

	// Test raw plain-text submission and after_id filtering.
	rawReq, _ := http.NewRequest(http.MethodPost, env.ts.URL+"/api/v1/artifacts/quiz/versions/1/submissions?filename=notes.txt", strings.NewReader("hello plain text"))
	rawReq.Header.Set("Content-Type", "text/plain; charset=utf-8")
	rawResp, err := http.DefaultClient.Do(rawReq)
	if err != nil {
		t.Fatalf("POST raw submission: %v", err)
	}
	defer rawResp.Body.Close()
	if rawResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST raw submission status = %d", rawResp.StatusCode)
	}
	var second model.SubmissionMeta
	if err := json.NewDecoder(rawResp.Body).Decode(&second); err != nil {
		t.Fatalf("decode second submission: %v", err)
	}

	afterSubs, err := env.client.Submissions(ctx, "quiz", client.SubmissionsOptions{
		AfterID: created.ID,
	})
	if err != nil {
		t.Fatalf("Submissions after_id: %v", err)
	}
	if len(afterSubs) != 1 || afterSubs[0].ID != second.ID || afterSubs[0].Filename != "notes.txt" {
		t.Fatalf("unexpected after_id submissions: %+v", afterSubs)
	}

	// Test bridge.js JSON envelope where data is a string.
	bridgeBody := `{"filename":"export.csv","content_type":"text/csv;charset=utf-8","data":"a,b\n1,2\n"}`
	bResp, err := http.Post(
		env.ts.URL+"/api/v1/artifacts/quiz/versions/1/submissions",
		"application/json",
		strings.NewReader(bridgeBody),
	)
	if err != nil {
		t.Fatalf("POST bridge submission: %v", err)
	}
	defer bResp.Body.Close()
	if bResp.StatusCode != http.StatusCreated {
		t.Fatalf("POST bridge submission status = %d", bResp.StatusCode)
	}
	var third model.SubmissionMeta
	if err := json.NewDecoder(bResp.Body).Decode(&third); err != nil {
		t.Fatalf("decode third submission: %v", err)
	}
	dl3, err := env.client.GetSubmission(ctx, third.ID)
	if err != nil {
		t.Fatalf("GetSubmission third: %v", err)
	}
	if dl3.Filename != "export.csv" || string(dl3.Data) != "a,b\n1,2\n" {
		t.Fatalf("unexpected bridge submission download: %+v", dl3)
	}
}

func TestPreviewsAndStatus(t *testing.T) {
	env := newTestEnv(t, true)
	ctx := context.Background()

	pushed, err := env.client.Push(ctx, "chart", client.PushRequest{
		Title: "Revenue Chart",
		Files: []client.PushFile{
			{Path: "index.html", Data: []byte("<html><body>Chart</body></html>")},
		},
	})
	if err != nil {
		t.Fatalf("Push chart: %v", err)
	}

	// 1. Uncached preview returns placeholder SVG with no-store on both URL formats.
	for _, p := range []string{"/previews/chart/v1.png", "/previews/chart/v/1.png"} {
		r, err := http.Get(env.ts.URL + p)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		b, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		if r.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d", p, r.StatusCode)
		}
		if ct := r.Header.Get("Content-Type"); !strings.Contains(ct, "image/svg+xml") {
			t.Fatalf("GET %s Content-Type = %q, want image/svg+xml", p, ct)
		}
		if cc := r.Header.Get("Cache-Control"); cc != "no-store" {
			t.Fatalf("GET %s Cache-Control = %q, want no-store", p, cc)
		}
		if !strings.Contains(string(b), "<svg") || !strings.Contains(string(b), "Revenue Chart") {
			t.Fatalf("GET %s unexpected placeholder SVG", p)
		}
	}

	// 2. Preview refresh endpoint when previews are disabled.
	ref, err := env.client.RefreshPreview(ctx, "chart", 1)
	if err != nil {
		t.Fatalf("RefreshPreview: %v", err)
	}
	if ref.Status != model.PreviewDisabled || ref.PreviewURL != "/previews/chart/v1.png" {
		t.Fatalf("unexpected refresh response: %+v", ref)
	}

	// 3. Cached PNG preview serves PNG with immutable Cache-Control, ETag, and 304 on If-None-Match.
	pngPath := env.store.PreviewFilePath("chart", 1, pushed.Version.ContentHash)
	fakePNG := []byte("\x89PNG\r\n\x1a\nfake-png-bytes")
	if err := os.MkdirAll(filepath.Dir(pngPath), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(pngPath, fakePNG, 0o644); err != nil {
		t.Fatalf("WriteFile preview: %v", err)
	}

	rPNG, err := http.Get(env.ts.URL + "/previews/chart/v1.png")
	if err != nil {
		t.Fatalf("GET cached png: %v", err)
	}
	pngBytes, _ := io.ReadAll(rPNG.Body)
	_ = rPNG.Body.Close()
	if rPNG.StatusCode != http.StatusOK {
		t.Fatalf("cached png status = %d", rPNG.StatusCode)
	}
	if ct := rPNG.Header.Get("Content-Type"); ct != "image/png" {
		t.Fatalf("cached png Content-Type = %q", ct)
	}
	if cc := rPNG.Header.Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
		t.Fatalf("cached png Cache-Control = %q", cc)
	}
	etag := rPNG.Header.Get("ETag")
	if etag == "" {
		t.Fatal("cached png missing ETag")
	}
	if !bytes.Equal(pngBytes, fakePNG) {
		t.Fatalf("cached png bytes mismatch")
	}

	req304, _ := http.NewRequest(http.MethodGet, env.ts.URL+"/previews/chart/v1.png", nil)
	req304.Header.Set("If-None-Match", etag)
	r304, err := http.DefaultClient.Do(req304)
	if err != nil {
		t.Fatalf("GET 304: %v", err)
	}
	_ = r304.Body.Close()
	if r304.StatusCode != http.StatusNotModified {
		t.Fatalf("expected 304 Not Modified, got %d", r304.StatusCode)
	}

	// 4. Status endpoint reflects counts and preview cache.
	status, err := env.client.Status(ctx)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Status != "ok" || status.Version != "1.2.3-test" {
		t.Fatalf("unexpected status header: %+v", status)
	}
	if status.ArtifactCount != 1 || status.VersionCount != 1 || status.PreviewCacheCount != 1 {
		t.Fatalf("unexpected status counts: %+v", status)
	}
	if status.PreviewCacheBytes != int64(len(fakePNG)) {
		t.Fatalf("preview cache bytes = %d, want %d", status.PreviewCacheBytes, len(fakePNG))
	}
}

func TestInjectBridgeScriptVariants(t *testing.T) {
	wantTag := `<script src="/_artifact/bridge.js" data-slug="my-app" data-version="3"></script>`

	tests := []struct {
		name  string
		input string
		check func(t *testing.T, out string)
	}{
		{
			name:  "before uppercase HEAD close",
			input: "<html><HEAD><title>X</title></HEAD><body>hi</body></html>",
			check: func(t *testing.T, out string) {
				if !strings.Contains(out, wantTag+"\n</HEAD>") {
					t.Fatalf("unexpected output: %s", out)
				}
			},
		},
		{
			name:  "before body close when no head",
			input: "<html><body><p>hi</p></body></html>",
			check: func(t *testing.T, out string) {
				if !strings.Contains(out, wantTag+"\n</body>") {
					t.Fatalf("unexpected output: %s", out)
				}
			},
		},
		{
			name:  "prepend when fragment has neither head nor body",
			input: "<h1>Fragment</h1>",
			check: func(t *testing.T, out string) {
				if !strings.HasPrefix(out, wantTag+"\n<h1>Fragment</h1>") {
					t.Fatalf("unexpected output: %s", out)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := string(injectBridgeScript([]byte(tc.input), "my-app", 3))
			tc.check(t, out)
		})
	}
}

func TestRunGracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- Run(ctx, Config{
			Addr:       "127.0.0.1:0",
			DataDir:    t.TempDir(),
			Version:    "run-test",
			NoPreviews: true,
			Workers:    1,
		})
	}()

	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Fatalf("Run returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not shut down within timeout")
	}
}
