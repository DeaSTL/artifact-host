package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/deastl/artifact-host/internal/model"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := Open(dir)
	if err != nil {
		t.Fatalf("Open(%q): %v", dir, err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close(): %v", err)
		}
	})
	return st
}

func TestOpenPragmasAndLayout(t *testing.T) {
	st := openTestStore(t)

	if st.DataDir() == "" || !filepath.IsAbs(st.DataDir()) {
		t.Fatalf("DataDir() = %q, want absolute path", st.DataDir())
	}

	for _, rel := range []string{"artifact-host.db", "artifacts", "previews"} {
		if _, err := os.Stat(filepath.Join(st.DataDir(), rel)); err != nil {
			t.Fatalf("expected %s to exist: %v", rel, err)
		}
	}

	var journalMode string
	if err := st.db.QueryRow(`PRAGMA journal_mode;`).Scan(&journalMode); err != nil {
		t.Fatalf("PRAGMA journal_mode: %v", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	var busyTimeout int
	if err := st.db.QueryRow(`PRAGMA busy_timeout;`).Scan(&busyTimeout); err != nil {
		t.Fatalf("PRAGMA busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}

	var foreignKeys int
	if err := st.db.QueryRow(`PRAGMA foreign_keys;`).Scan(&foreignKeys); err != nil {
		t.Fatalf("PRAGMA foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}
}

func TestPushVersionsAndMetadataPreservation(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	htmlV1 := []byte("<!doctype html><html><body><h1>v1</h1></body></html>")
	cssV1 := []byte("body { color: #111; }")
	binV1 := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0x00, 0x00, 0x00, 0x0d}

	art1, ver1, err := st.PushVersion(ctx, PushInput{
		Slug:        "demo-app",
		Title:       "Demo App",
		Description: "Initial release",
		Icon:        "box",
		Note:        "first push",
		Tags:        []string{"ui", "demo", "ui", " "},
		Assets: []AssetInput{
			{Path: "assets/style.css", Data: cssV1},
			{Path: "index.html", Data: htmlV1},
			{Path: "assets/blob.unknownext", Data: binV1},
		},
	})
	if err != nil {
		t.Fatalf("PushVersion v1: %v", err)
	}

	if art1.Slug != "demo-app" || art1.Title != "Demo App" || art1.Description != "Initial release" || art1.Icon != "box" {
		t.Fatalf("unexpected artifact v1 metadata: %+v", art1)
	}
	if !slices.Equal(art1.Tags, []string{"ui", "demo"}) {
		t.Fatalf("art1.Tags = %v, want [ui demo]", art1.Tags)
	}
	if art1.LatestVersion != 1 || art1.ActiveVersion != 0 || art1.VersionCount != 1 {
		t.Fatalf("unexpected version counters: %+v", art1)
	}
	if art1.LatestPreviewStatus != model.PreviewPending || art1.LatestContentHash != ver1.ContentHash {
		t.Fatalf("unexpected preview/hash on artifact: %+v", art1)
	}

	if ver1.Number != 1 || ver1.EntryFile != "index.html" || ver1.FileCount != 3 {
		t.Fatalf("unexpected ver1: %+v", ver1)
	}
	if ver1.TotalSize != int64(len(htmlV1)+len(cssV1)+len(binV1)) {
		t.Fatalf("ver1.TotalSize = %d, want %d", ver1.TotalSize, len(htmlV1)+len(cssV1)+len(binV1))
	}
	if len(ver1.Assets) != 3 {
		t.Fatalf("len(ver1.Assets) = %d, want 3", len(ver1.Assets))
	}

	// Verify sorted order and detected content types + SHA-256.
	wantOrder := []string{"assets/blob.unknownext", "assets/style.css", "index.html"}
	for i, wantPath := range wantOrder {
		if ver1.Assets[i].Path != wantPath {
			t.Fatalf("ver1.Assets[%d].Path = %q, want %q", i, ver1.Assets[i].Path, wantPath)
		}
	}
	if !strings.HasPrefix(ver1.Assets[0].ContentType, "image/png") {
		t.Fatalf("fallback content type for png bytes = %q, want image/png", ver1.Assets[0].ContentType)
	}
	if !strings.HasPrefix(ver1.Assets[1].ContentType, "text/css") {
		t.Fatalf("css content type = %q, want text/css*", ver1.Assets[1].ContentType)
	}
	if !strings.HasPrefix(ver1.Assets[2].ContentType, "text/html") {
		t.Fatalf("html content type = %q, want text/html*", ver1.Assets[2].ContentType)
	}
	wantHTMLHash := fmt.Sprintf("%x", sha256.Sum256(htmlV1))
	if ver1.Assets[2].SHA256 != wantHTMLHash {
		t.Fatalf("index.html sha256 = %q, want %q", ver1.Assets[2].SHA256, wantHTMLHash)
	}

	// Pin active_version directly in DB to verify PushVersion resets it to 0.
	if _, err := st.db.ExecContext(ctx, `UPDATE artifacts SET active_version = 1 WHERE slug = ?`, "demo-app"); err != nil {
		t.Fatalf("pin active_version: %v", err)
	}

	// Push v2 with empty Title/Description/Icon and SetTags=false, Tags=nil -> must preserve v1 metadata.
	// Also pass assets in a different order with identical contents to confirm deterministic ContentHash.
	art2, ver2, err := st.PushVersion(ctx, PushInput{
		Slug: "demo-app",
		Note: "second push preserves metadata",
		Assets: []AssetInput{
			{Path: "index.html", Data: htmlV1},
			{Path: "assets/blob.unknownext", Data: binV1},
			{Path: "assets/style.css", Data: cssV1},
		},
	})
	if err != nil {
		t.Fatalf("PushVersion v2: %v", err)
	}

	if art2.Title != "Demo App" || art2.Description != "Initial release" || art2.Icon != "box" {
		t.Fatalf("v2 failed to preserve metadata: %+v", art2)
	}
	if !slices.Equal(art2.Tags, []string{"ui", "demo"}) {
		t.Fatalf("v2 failed to preserve tags: %v", art2.Tags)
	}
	if art2.LatestVersion != 2 || art2.ActiveVersion != 0 || art2.VersionCount != 2 {
		t.Fatalf("v2 version fields wrong: %+v", art2)
	}
	if ver2.ContentHash != ver1.ContentHash {
		t.Fatalf("ContentHash not deterministic across asset order: v1=%s v2=%s", ver1.ContentHash, ver2.ContentHash)
	}

	// Push v3 with SetTags=true and empty Tags -> explicitly clears tags, and overrides Title.
	art3, ver3, err := st.PushVersion(ctx, PushInput{
		Slug:    "demo-app",
		Title:   "Demo App Updated",
		SetTags: true,
		Tags:    nil,
		Assets: []AssetInput{
			{Path: "index.html", Data: []byte("<h1>v3</h1>")},
		},
	})
	if err != nil {
		t.Fatalf("PushVersion v3: %v", err)
	}
	if art3.Title != "Demo App Updated" || art3.Description != "Initial release" || art3.Icon != "box" {
		t.Fatalf("v3 partial override failed: %+v", art3)
	}
	if len(art3.Tags) != 0 {
		t.Fatalf("v3 SetTags=true with empty tags should clear tags, got %v", art3.Tags)
	}
	if ver3.Number != 3 || ver3.ContentHash == ver2.ContentHash {
		t.Fatalf("unexpected ver3: %+v", ver3)
	}
}

func TestPushValidationErrors(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	cases := []struct {
		name string
		in   PushInput
	}{
		{
			name: "invalid slug uppercase",
			in: PushInput{
				Slug:   "BadSlug",
				Assets: []AssetInput{{Path: "index.html", Data: []byte("hi")}},
			},
		},
		{
			name: "empty assets",
			in: PushInput{
				Slug: "valid-slug",
			},
		},
		{
			name: "missing default entry file",
			in: PushInput{
				Slug:   "valid-slug",
				Assets: []AssetInput{{Path: "app.html", Data: []byte("hi")}},
			},
		},
		{
			name: "path traversal in asset",
			in: PushInput{
				Slug: "valid-slug",
				Assets: []AssetInput{
					{Path: "index.html", Data: []byte("hi")},
					{Path: "../escape.txt", Data: []byte("nope")},
				},
			},
		},
		{
			name: "duplicate sanitized asset path",
			in: PushInput{
				Slug: "valid-slug",
				Assets: []AssetInput{
					{Path: "index.html", Data: []byte("a")},
					{Path: "./index.html", Data: []byte("b")},
				},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := st.PushVersion(ctx, tc.in); err == nil {
				t.Fatalf("expected error for %s, got nil", tc.name)
			}
		})
	}
}

func TestListArtifactsSearchTagOrderingAndPagination(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	push := func(slug, title, desc string, tags []string) {
		t.Helper()
		_, _, err := st.PushVersion(ctx, PushInput{
			Slug:        slug,
			Title:       title,
			Description: desc,
			Tags:        tags,
			Assets:      []AssetInput{{Path: "index.html", Data: []byte(slug)}},
		})
		if err != nil {
			t.Fatalf("push %s: %v", slug, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	push("alpha-chart", "Revenue Chart", "Quarterly sales dashboard", []string{"finance", "chart"})
	push("beta-form", "Intake Form", "Interactive survey", []string{"forms", "interactive"})
	push("gamma-viz", "Network Map", "Graph visualization tool", []string{"chart", "network"})

	// Update alpha-chart so it moves to the top of updated_at DESC.
	_, _, err := st.PushVersion(ctx, PushInput{
		Slug:   "alpha-chart",
		Assets: []AssetInput{{Path: "index.html", Data: []byte("alpha v2")}},
	})
	if err != nil {
		t.Fatalf("push alpha v2: %v", err)
	}

	// Add a submission to alpha-chart and update preview status to verify aggregate fields.
	if _, err := st.SaveSubmission(ctx, "alpha-chart", 2, "out.json", "application/json", []byte(`{"ok":true}`)); err != nil {
		t.Fatalf("SaveSubmission: %v", err)
	}
	if err := st.UpdatePreviewStatus(ctx, "alpha-chart", 2, model.PreviewReady, "abc123", ""); err != nil {
		t.Fatalf("UpdatePreviewStatus: %v", err)
	}

	all, tags, err := st.ListArtifacts(ctx, model.ListFilter{})
	if err != nil {
		t.Fatalf("ListArtifacts all: %v", err)
	}
	wantSlugs := []string{"alpha-chart", "gamma-viz", "beta-form"}
	if len(all) != len(wantSlugs) {
		t.Fatalf("len(all) = %d, want %d", len(all), len(wantSlugs))
	}
	for i, s := range wantSlugs {
		if all[i].Slug != s {
			t.Fatalf("all[%d].Slug = %q, want %q", i, all[i].Slug, s)
		}
	}
	wantTags := []string{"chart", "finance", "forms", "interactive", "network"}
	if !slices.Equal(tags, wantTags) {
		t.Fatalf("allTags = %v, want %v", tags, wantTags)
	}
	if all[0].VersionCount != 2 || all[0].SubmissionCount != 1 || all[0].LatestPreviewStatus != model.PreviewReady {
		t.Fatalf("alpha-chart aggregates wrong: %+v", all[0])
	}

	// Case-insensitive search across description.
	searched, tagsAfterSearch, err := st.ListArtifacts(ctx, model.ListFilter{Search: "SURVEY"})
	if err != nil {
		t.Fatalf("ListArtifacts search: %v", err)
	}
	if len(searched) != 1 || searched[0].Slug != "beta-form" {
		t.Fatalf("search SURVEY got %+v, want [beta-form]", searched)
	}
	if !slices.Equal(tagsAfterSearch, wantTags) {
		t.Fatalf("allTags should include all artifacts even when filtered, got %v", tagsAfterSearch)
	}

	// Exact tag filter.
	byTag, _, err := st.ListArtifacts(ctx, model.ListFilter{Tag: "chart"})
	if err != nil {
		t.Fatalf("ListArtifacts tag: %v", err)
	}
	if len(byTag) != 2 || byTag[0].Slug != "alpha-chart" || byTag[1].Slug != "gamma-viz" {
		t.Fatalf("tag=chart got %+v", byTag)
	}

	// Pagination.
	paged, _, err := st.ListArtifacts(ctx, model.ListFilter{Offset: 1, Limit: 1})
	if err != nil {
		t.Fatalf("ListArtifacts paged: %v", err)
	}
	if len(paged) != 1 || paged[0].Slug != "gamma-viz" {
		t.Fatalf("paged got %+v, want [gamma-viz]", paged)
	}
}

func TestGetArtifactDetailAndVersions(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if _, err := st.GetArtifact(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetArtifact missing err = %v, want ErrNotFound", err)
	}

	_, _, err := st.PushVersion(ctx, PushInput{
		Slug:      "docs-site",
		Title:     "Docs",
		EntryFile: "main.html",
		Assets: []AssetInput{
			{Path: "main.html", Data: []byte("<p>v1</p>")},
			{Path: "a.js", Data: []byte("console.log(1)")},
		},
	})
	if err != nil {
		t.Fatalf("push v1: %v", err)
	}
	_, _, err = st.PushVersion(ctx, PushInput{
		Slug:      "docs-site",
		EntryFile: "main.html",
		Assets: []AssetInput{
			{Path: "main.html", Data: []byte("<p>v2</p>")},
		},
	})
	if err != nil {
		t.Fatalf("push v2: %v", err)
	}
	if _, err := st.SaveSubmission(ctx, "docs-site", 1, "a.txt", "text/plain", []byte("one")); err != nil {
		t.Fatalf("SaveSubmission 1: %v", err)
	}
	if _, err := st.SaveSubmission(ctx, "docs-site", 2, "b.txt", "text/plain", []byte("two")); err != nil {
		t.Fatalf("SaveSubmission 2: %v", err)
	}

	detail, err := st.GetArtifactDetail(ctx, "docs-site", 0)
	if err != nil {
		t.Fatalf("GetArtifactDetail default: %v", err)
	}
	if detail.CurrentVersion == nil || detail.CurrentVersion.Number != 2 {
		t.Fatalf("CurrentVersion = %+v, want v2", detail.CurrentVersion)
	}
	if len(detail.CurrentVersion.Assets) != 1 || detail.CurrentVersion.Assets[0].Path != "main.html" {
		t.Fatalf("v2 assets = %+v", detail.CurrentVersion.Assets)
	}
	if len(detail.Versions) != 2 || detail.Versions[0].Number != 2 || detail.Versions[1].Number != 1 {
		t.Fatalf("Versions not DESC: %+v", detail.Versions)
	}
	if len(detail.Submissions) != 2 || detail.Submissions[0].Filename != "b.txt" || detail.Submissions[1].Filename != "a.txt" {
		t.Fatalf("Submissions not DESC: %+v", detail.Submissions)
	}

	detailV1, err := st.GetArtifactDetail(ctx, "docs-site", 1)
	if err != nil {
		t.Fatalf("GetArtifactDetail v1: %v", err)
	}
	if detailV1.CurrentVersion == nil || detailV1.CurrentVersion.Number != 1 || len(detailV1.CurrentVersion.Assets) != 2 {
		t.Fatalf("detailV1 CurrentVersion = %+v", detailV1.CurrentVersion)
	}

	if _, err := st.GetArtifactDetail(ctx, "docs-site", 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetArtifactDetail v99 err = %v, want ErrNotFound", err)
	}
}

func TestRollbackToVersion(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	_, v1, err := st.PushVersion(ctx, PushInput{
		Slug:        "widget",
		Title:       "Widget v1",
		Description: "First desc",
		Icon:        "layers",
		Tags:        []string{"tools"},
		Assets: []AssetInput{
			{Path: "index.html", Data: []byte("<h1>v1</h1>")},
			{Path: "lib/app.js", Data: []byte("export const v = 1;")},
		},
	})
	if err != nil {
		t.Fatalf("push v1: %v", err)
	}

	_, _, err = st.PushVersion(ctx, PushInput{
		Slug:  "widget",
		Title: "Widget v2",
		Assets: []AssetInput{
			{Path: "index.html", Data: []byte("<h1>v2 broken</h1>")},
		},
	})
	if err != nil {
		t.Fatalf("push v2: %v", err)
	}

	art, v3, err := st.RollbackToVersion(ctx, "widget", 1, "")
	if err != nil {
		t.Fatalf("RollbackToVersion: %v", err)
	}
	if art.LatestVersion != 3 || art.VersionCount != 3 || art.Icon != "layers" || !slices.Equal(art.Tags, []string{"tools"}) {
		t.Fatalf("unexpected artifact after rollback: %+v", art)
	}
	if v3.Number != 3 || v3.Note != "Restored from v1" || v3.ContentHash != v1.ContentHash {
		t.Fatalf("unexpected v3 after rollback: %+v", v3)
	}
	if len(v3.Assets) != 2 {
		t.Fatalf("v3 assets = %+v, want 2 assets", v3.Assets)
	}

	// Verify file contents on disk for v3 match v1, and v1/v2 still exist.
	jsPath, _, err := st.ResolveAssetFile("widget", 3, "lib/app.js")
	if err != nil {
		t.Fatalf("ResolveAssetFile v3 lib/app.js: %v", err)
	}
	gotJS, err := os.ReadFile(jsPath)
	if err != nil {
		t.Fatalf("ReadFile %s: %v", jsPath, err)
	}
	if string(gotJS) != "export const v = 1;" {
		t.Fatalf("v3 lib/app.js = %q, want v1 content", string(gotJS))
	}

	versions, err := st.ListVersions(ctx, "widget")
	if err != nil {
		t.Fatalf("ListVersions: %v", err)
	}
	if len(versions) != 3 {
		t.Fatalf("expected append-only 3 versions, got %d", len(versions))
	}

	if _, _, err := st.RollbackToVersion(ctx, "widget", 99, "bad"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rollback missing version err = %v, want ErrNotFound", err)
	}
}

func TestDeleteVersionAndDeleteArtifact(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	// Push two artifacts with overlapping slug prefixes to ensure preview cleanup is precise.
	_, v1, err := st.PushVersion(ctx, PushInput{
		Slug:   "app",
		Assets: []AssetInput{{Path: "index.html", Data: []byte("v1")}},
	})
	if err != nil {
		t.Fatalf("push app v1: %v", err)
	}
	_, v2, err := st.PushVersion(ctx, PushInput{
		Slug:   "app",
		Assets: []AssetInput{{Path: "index.html", Data: []byte("v2")}},
	})
	if err != nil {
		t.Fatalf("push app v2: %v", err)
	}
	_, sibV1, err := st.PushVersion(ctx, PushInput{
		Slug:   "app-v1",
		Assets: []AssetInput{{Path: "index.html", Data: []byte("sibling")}},
	})
	if err != nil {
		t.Fatalf("push app-v1: %v", err)
	}

	// Create preview files for all three.
	pAppV1 := st.PreviewFilePath("app", 1, v1.ContentHash)
	pAppV2 := st.PreviewFilePath("app", 2, v2.ContentHash)
	pSibV1 := st.PreviewFilePath("app-v1", 1, sibV1.ContentHash)
	for _, p := range []string{pAppV1, pAppV2, pSibV1} {
		if err := os.WriteFile(p, []byte("png-data"), 0o644); err != nil {
			t.Fatalf("write preview %s: %v", p, err)
		}
	}

	// Pin active_version=2 on "app" then delete v2 -> latest_version should become 1 and active_version 0.
	if _, err := st.db.ExecContext(ctx, `UPDATE artifacts SET active_version = 2 WHERE slug = 'app'`); err != nil {
		t.Fatalf("pin active_version: %v", err)
	}
	if err := st.DeleteVersion(ctx, "app", 2); err != nil {
		t.Fatalf("DeleteVersion app v2: %v", err)
	}
	art, err := st.GetArtifact(ctx, "app")
	if err != nil {
		t.Fatalf("GetArtifact app: %v", err)
	}
	if art.LatestVersion != 1 || art.ActiveVersion != 0 || art.VersionCount != 1 {
		t.Fatalf("after deleting v2, art = %+v", art)
	}
	if _, err := os.Stat(versionDir(st.DataDir(), "app", 2)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected v2 dir removed, stat err = %v", err)
	}
	if _, err := os.Stat(pAppV2); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected v2 preview removed, stat err = %v", err)
	}
	if _, err := os.Stat(pAppV1); err != nil {
		t.Fatalf("expected v1 preview to remain: %v", err)
	}
	if _, err := os.Stat(pSibV1); err != nil {
		t.Fatalf("expected sibling preview to remain: %v", err)
	}

	// Delete last remaining version (v1) of "app" -> deletes whole artifact too.
	if err := st.DeleteVersion(ctx, "app", 1); err != nil {
		t.Fatalf("DeleteVersion app v1: %v", err)
	}
	if _, err := st.GetArtifact(ctx, "app"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected app deleted after last version removed, got %v", err)
	}
	if _, err := os.Stat(artifactDir(st.DataDir(), "app")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected app dir removed, stat err = %v", err)
	}
	if _, err := os.Stat(pAppV1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected app v1 preview removed, stat err = %v", err)
	}
	// Sibling "app-v1" and its preview must remain completely untouched.
	if _, err := st.GetArtifact(ctx, "app-v1"); err != nil {
		t.Fatalf("sibling artifact app-v1 should remain: %v", err)
	}
	if _, err := os.Stat(pSibV1); err != nil {
		t.Fatalf("sibling preview %s should remain: %v", pSibV1, err)
	}

	// Add a submission to sibling, then DeleteArtifact and verify cascade + cleanup.
	sub, err := st.SaveSubmission(ctx, "app-v1", 1, "s.json", "application/json", []byte(`{}`))
	if err != nil {
		t.Fatalf("SaveSubmission: %v", err)
	}
	if err := st.DeleteArtifact(ctx, "app-v1"); err != nil {
		t.Fatalf("DeleteArtifact app-v1: %v", err)
	}
	if _, err := st.GetSubmission(ctx, sub.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("submission should cascade delete, got %v", err)
	}
	if _, err := os.Stat(pSibV1); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("sibling preview should be deleted, stat err = %v", err)
	}
	if err := st.DeleteArtifact(ctx, "app-v1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second DeleteArtifact should return ErrNotFound, got %v", err)
	}
}

func TestResolveAssetFileAndTraversalProtection(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	_, _, err := st.PushVersion(ctx, PushInput{
		Slug: "safe-app",
		Assets: []AssetInput{
			{Path: "index.html", ContentType: "text/html; charset=utf-8", Data: []byte("<html>ok</html>")},
			{Path: "nested/data.custom", ContentType: "application/x-custom", Data: []byte("custom-payload")},
		},
	})
	if err != nil {
		t.Fatalf("PushVersion: %v", err)
	}

	// Valid asset resolves with stored ContentType.
	absPath, ct, err := st.ResolveAssetFile("safe-app", 1, "nested/data.custom")
	if err != nil {
		t.Fatalf("ResolveAssetFile valid: %v", err)
	}
	if !filepath.IsAbs(absPath) || !strings.HasSuffix(absPath, filepath.Join("safe-app", "v1", "nested", "data.custom")) {
		t.Fatalf("unexpected absPath %q", absPath)
	}
	if ct != "application/x-custom" {
		t.Fatalf("contentType = %q, want application/x-custom", ct)
	}

	// Missing file and directory return ErrNotFound.
	if _, _, err := st.ResolveAssetFile("safe-app", 1, "missing.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing file err = %v, want ErrNotFound", err)
	}
	if _, _, err := st.ResolveAssetFile("safe-app", 1, "nested"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("directory path err = %v, want ErrNotFound", err)
	}
	if _, _, err := st.ResolveAssetFile("safe-app", 0, "index.html"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("version 0 err = %v, want ErrNotFound", err)
	}

	// Traversal attempts must be rejected.
	badPaths := []string{
		"../v1/index.html",
		"nested/../../artifact-host.db",
		"/etc/passwd",
		`nested\data.custom`,
		"index.html\x00.png",
		"",
		".",
	}
	for _, bp := range badPaths {
		if _, _, err := st.ResolveAssetFile("safe-app", 1, bp); err == nil {
			t.Fatalf("ResolveAssetFile(%q) unexpectedly succeeded", bp)
		}
	}

	// Symlink pointing outside version dir must be rejected.
	outsideFile := filepath.Join(st.DataDir(), "secret.txt")
	if err := os.WriteFile(outsideFile, []byte("secret"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	linkPath := filepath.Join(versionDir(st.DataDir(), "safe-app", 1), "escape.txt")
	if err := os.Symlink(outsideFile, linkPath); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if _, _, err := st.ResolveAssetFile("safe-app", 1, "escape.txt"); err == nil {
		t.Fatalf("ResolveAssetFile unexpectedly allowed symlink escaping version dir")
	}
}

func TestPreviewStatusAndPath(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	_, v1, err := st.PushVersion(ctx, PushInput{
		Slug:   "preview-test",
		Assets: []AssetInput{{Path: "index.html", Data: []byte("<h1>preview</h1>")}},
	})
	if err != nil {
		t.Fatalf("PushVersion: %v", err)
	}

	p1 := st.PreviewFilePath("preview-test", 1, v1.ContentHash)
	p2 := st.PreviewFilePath("preview-test", 1, v1.ContentHash)
	if p1 != p2 {
		t.Fatalf("PreviewFilePath not deterministic: %q vs %q", p1, p2)
	}
	previewsDir := filepath.Join(st.DataDir(), "previews")
	if !isPathStrictlyInside(previewsDir, p1) || !strings.HasSuffix(p1, ".png") {
		t.Fatalf("PreviewFilePath = %q, want PNG strictly inside %q", p1, previewsDir)
	}

	// Unsafe characters in hash/slug still produce a path strictly inside previews dir.
	unsafe := st.PreviewFilePath("../evil", 1, "../../bad/hash")
	if !isPathStrictlyInside(previewsDir, unsafe) {
		t.Fatalf("PreviewFilePath with unsafe input escaped previews dir: %q", unsafe)
	}

	if err := st.UpdatePreviewStatus(ctx, "preview-test", 1, model.PreviewFailed, "", "chrome crashed"); err != nil {
		t.Fatalf("UpdatePreviewStatus failed: %v", err)
	}
	gotV1, err := st.GetVersion(ctx, "preview-test", 1)
	if err != nil {
		t.Fatalf("GetVersion: %v", err)
	}
	if gotV1.PreviewStatus != model.PreviewFailed || gotV1.PreviewError != "chrome crashed" || gotV1.PreviewUpdatedAt == nil {
		t.Fatalf("unexpected preview status fields: %+v", gotV1)
	}

	if err := st.UpdatePreviewStatus(ctx, "preview-test", 1, model.PreviewReady, v1.ContentHash, ""); err != nil {
		t.Fatalf("UpdatePreviewStatus ready: %v", err)
	}
	art, err := st.GetArtifact(ctx, "preview-test")
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if art.LatestPreviewStatus != model.PreviewReady {
		t.Fatalf("LatestPreviewStatus = %q, want ready", art.LatestPreviewStatus)
	}

	if err := st.UpdatePreviewStatus(ctx, "nonexistent", 1, model.PreviewReady, "abc", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("UpdatePreviewStatus nonexistent err = %v, want ErrNotFound", err)
	}
}

func TestSubmissionsCRUDAndStats(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	if _, err := st.SaveSubmission(ctx, "missing", 1, "a.json", "", []byte("{}")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("SaveSubmission missing slug err = %v, want ErrNotFound", err)
	}

	_, v1, err := st.PushVersion(ctx, PushInput{
		Slug:   "form-app",
		Assets: []AssetInput{{Path: "index.html", Data: []byte("<form></form>")}},
	})
	if err != nil {
		t.Fatalf("push v1: %v", err)
	}
	_, _, err = st.PushVersion(ctx, PushInput{
		Slug:   "form-app",
		Assets: []AssetInput{{Path: "index.html", Data: []byte("<form>v2</form>")}},
	})
	if err != nil {
		t.Fatalf("push v2: %v", err)
	}

	s1, err := st.SaveSubmission(ctx, "form-app", 1, "first.json", "", []byte(`{"step":1}`))
	if err != nil {
		t.Fatalf("SaveSubmission s1: %v", err)
	}
	if !strings.HasPrefix(s1.ContentType, "application/json") || s1.Version != 1 || s1.Size != 10 {
		t.Fatalf("unexpected s1: %+v", s1)
	}
	time.Sleep(2 * time.Millisecond)

	// versionNum <= 0 defaults to EffectiveVersion (2).
	s2, err := st.SaveSubmission(ctx, "form-app", 0, "second.csv", "text/csv", []byte("a,b\n1,2\n"))
	if err != nil {
		t.Fatalf("SaveSubmission s2: %v", err)
	}
	if s2.Version != 2 || s2.ContentType != "text/csv" {
		t.Fatalf("unexpected s2: %+v", s2)
	}

	// List all submissions across versions.
	allSubs, err := st.ListSubmissions(ctx, "form-app", 0)
	if err != nil {
		t.Fatalf("ListSubmissions all: %v", err)
	}
	if len(allSubs) != 2 || allSubs[0].ID != s2.ID || allSubs[1].ID != s1.ID {
		t.Fatalf("ListSubmissions all = %+v", allSubs)
	}

	// Filter by version 1.
	v1Subs, err := st.ListSubmissions(ctx, "form-app", 1)
	if err != nil {
		t.Fatalf("ListSubmissions v1: %v", err)
	}
	if len(v1Subs) != 1 || v1Subs[0].ID != s1.ID {
		t.Fatalf("ListSubmissions v1 = %+v", v1Subs)
	}

	// GetSubmission returns raw bytes.
	gotS1, err := st.GetSubmission(ctx, s1.ID)
	if err != nil {
		t.Fatalf("GetSubmission: %v", err)
	}
	if !bytes.Equal(gotS1.Data, []byte(`{"step":1}`)) || gotS1.Slug != "form-app" {
		t.Fatalf("unexpected GetSubmission: %+v", gotS1)
	}
	if _, err := st.GetSubmission(ctx, 999999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetSubmission missing err = %v, want ErrNotFound", err)
	}

	// LatestSubmission overall and per version.
	latestAll, err := st.LatestSubmission(ctx, "form-app", 0)
	if err != nil {
		t.Fatalf("LatestSubmission all: %v", err)
	}
	if latestAll.ID != s2.ID || string(latestAll.Data) != "a,b\n1,2\n" {
		t.Fatalf("unexpected latestAll: %+v", latestAll)
	}
	latestV1, err := st.LatestSubmission(ctx, "form-app", 1)
	if err != nil {
		t.Fatalf("LatestSubmission v1: %v", err)
	}
	if latestV1.ID != s1.ID {
		t.Fatalf("unexpected latestV1: %+v", latestV1)
	}
	if _, err := st.LatestSubmission(ctx, "form-app", 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("LatestSubmission v99 err = %v, want ErrNotFound", err)
	}

	// Create two PNG previews and one ignored non-PNG file to verify Stats.
	p1 := st.PreviewFilePath("form-app", 1, v1.ContentHash)
	p2 := filepath.Join(st.DataDir(), "previews", "other-v1-deadbeef.png")
	ignored := filepath.Join(st.DataDir(), "previews", "readme.txt")
	if err := os.WriteFile(p1, []byte("12345"), 0o644); err != nil {
		t.Fatalf("write p1: %v", err)
	}
	if err := os.WriteFile(p2, []byte("1234567"), 0o644); err != nil {
		t.Fatalf("write p2: %v", err)
	}
	if err := os.WriteFile(ignored, []byte("ignored"), 0o644); err != nil {
		t.Fatalf("write ignored: %v", err)
	}

	artCnt, verCnt, subCnt, prevCnt, prevBytes, err := st.Stats(ctx)
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if artCnt != 1 || verCnt != 2 || subCnt != 2 || prevCnt != 2 || prevBytes != 12 {
		t.Fatalf("Stats = (art=%d ver=%d sub=%d prev=%d bytes=%d), want (1, 2, 2, 2, 12)",
			artCnt, verCnt, subCnt, prevCnt, prevBytes)
	}
}

func TestConcurrentPushes(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)

	const workers = 6
	var wg sync.WaitGroup
	errCh := make(chan error, workers)

	for i := range workers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, _, err := st.PushVersion(ctx, PushInput{
				Slug: "concurrent-app",
				Note: fmt.Sprintf("push %d", n),
				Assets: []AssetInput{
					{Path: "index.html", Data: []byte(fmt.Sprintf("<h1>%d</h1>", n))},
				},
			})
			if err != nil {
				errCh <- err
			}
		}(i)
	}
	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent PushVersion error: %v", err)
	}

	art, err := st.GetArtifact(ctx, "concurrent-app")
	if err != nil {
		t.Fatalf("GetArtifact: %v", err)
	}
	if art.LatestVersion != workers || art.VersionCount != workers {
		t.Fatalf("got latest=%d count=%d, want %d", art.LatestVersion, art.VersionCount, workers)
	}
}
