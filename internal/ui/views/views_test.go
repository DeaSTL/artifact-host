package views

import (
	"bytes"
	"context"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/deastl/artifact-host/internal/model"
	"github.com/deastl/artifact-host/internal/ui"
)

func renderComponent(t *testing.T, c templ.Component) string {
	t.Helper()
	var buf bytes.Buffer
	if err := c.Render(context.Background(), &buf); err != nil {
		t.Fatalf("render failed: %v", err)
	}
	return buf.String()
}

func assertNoEmojis(t *testing.T, label, s string) {
	t.Helper()
	for _, r := range s {
		if isEmojiRune(r) {
			t.Errorf("%s contains disallowed emoji rune U+%04X (%q)", label, r, r)
		}
	}
}

func isEmojiRune(r rune) bool {
	switch {
	case r >= 0x1F300 && r <= 0x1FAFF:
		return true
	case r >= 0x2600 && r <= 0x27BF:
		return true
	case r == 0xFE0F || r == 0x200D:
		return true
	default:
		return false
	}
}

func TestGalleryRender(t *testing.T) {
	now := time.Now()
	data := GalleryData{
		Artifacts: []model.Artifact{
			{
				ID:                  1,
				Slug:                "decision-sheet",
				Title:               "Architecture Decision Sheet",
				Description:         "Interactive tradeoff checklist for service boundaries.",
				Icon:                "file-code-2", // legacy alias -> file-code
				Tags:                []string{"architecture", "interactive"},
				LatestVersion:       3,
				VersionCount:        3,
				SubmissionCount:     2,
				UpdatedAt:           now.Add(-5 * time.Minute),
				LatestPreviewStatus: model.PreviewReady,
				LatestContentHash:   "abcdef1234567890",
			},
			{
				ID:                  2,
				Slug:                "perf-report",
				Title:               "Latency Breakdown",
				Description:         "P99 flamegraph and breakdown.",
				Icon:                "nonexistent-icon-name", // must safely fall back
				Tags:                []string{"metrics"},
				LatestVersion:       1,
				VersionCount:        1,
				SubmissionCount:     0,
				UpdatedAt:           now.Add(-2 * time.Hour),
				LatestPreviewStatus: model.PreviewPending,
				LatestContentHash:   "1122334455667788",
			},
		},
		AllTags:   []string{"architecture", "interactive", "metrics"},
		Query:     "",
		ActiveTag: "architecture",
		Status: model.DaemonStatus{
			Status:            "ok",
			Addr:              "127.0.0.1:7777",
			ArtifactCount:     2,
			VersionCount:      4,
			PreviewCacheCount: 1,
		},
	}

	html := renderComponent(t, Gallery(data))
	assertNoEmojis(t, "Gallery", html)

	wants := []string{
		"<!doctype html>",
		`<link rel="stylesheet" href="/static/app.css">`,
		`<script defer src="/static/ui.js"></script>`,
		"Artifact Host",
		"127.0.0.1:7777",
		"2 artifacts",
		"4 versions",
		"1 preview",
		"Architecture Decision Sheet",
		`/previews/decision-sheet/v3.png?h=abcdef123456`,
		`/previews/perf-report/v1.png?h=112233445566`,
		`data-preview-pending="true"`,
		`data-copy-url="/a/decision-sheet"`,
		`href="/a/decision-sheet"`,
		"3 versions",
		"1 version",
		`href="/?tag=metrics"`,
		"lucide-file-code",
	}
	for _, want := range wants {
		if !strings.Contains(strings.ToLower(html), strings.ToLower(want)) {
			t.Errorf("Gallery HTML missing expected substring %q", want)
		}
	}
}

func TestGalleryEmptyStates(t *testing.T) {
	unfiltered := renderComponent(t, Gallery(GalleryData{}))
	assertNoEmojis(t, "GalleryEmptyUnfiltered", unfiltered)
	if !strings.Contains(unfiltered, "No artifacts hosted yet") {
		t.Errorf("expected unfiltered empty state heading")
	}
	if !strings.Contains(unfiltered, "artifact push") {
		t.Errorf("expected CLI example in unfiltered empty state")
	}

	filtered := renderComponent(t, Gallery(GalleryData{Query: "missing"}))
	assertNoEmojis(t, "GalleryEmptyFiltered", filtered)
	if !strings.Contains(filtered, "No matching artifacts") {
		t.Errorf("expected filtered empty state heading")
	}
	if !strings.Contains(filtered, "Clear filters") {
		t.Errorf("expected clear filters action in filtered empty state")
	}
}

func TestViewerLatestVersion(t *testing.T) {
	now := time.Now()
	art := model.Artifact{
		ID:            10,
		Slug:          "decision-sheet",
		Title:         "Decision Sheet",
		Description:   "Interactive decision matrix",
		Icon:          "layers",
		Tags:          []string{"rfc"},
		LatestVersion: 3,
		VersionCount:  3,
	}
	versions := []model.Version{
		{ID: 1, Slug: art.Slug, Number: 1, Note: "Initial draft", FileCount: 1, TotalSize: 2048, CreatedAt: now.Add(-48 * time.Hour)},
		{ID: 2, Slug: art.Slug, Number: 2, Note: "Added export", FileCount: 2, TotalSize: 4096, CreatedAt: now.Add(-24 * time.Hour)},
		{
			ID:          3,
			Slug:        art.Slug,
			Number:      3,
			Note:        "Final polish",
			EntryFile:   "index.html",
			ContentHash: "deadbeef12345678",
			FileCount:   2,
			TotalSize:   5120,
			CreatedAt:   now.Add(-10 * time.Minute),
			Assets: []model.Asset{
				{Path: "index.html", Size: 4096, ContentType: "text/html"},
				{Path: "assets/app.js", Size: 1024, ContentType: "application/javascript"},
			},
		},
	}
	subs := []model.SubmissionMeta{
		{ID: 42, Slug: art.Slug, Version: 3, Filename: "decisions.json", ContentType: "application/json", Size: 512, CreatedAt: now.Add(-3 * time.Minute)},
	}

	html := renderComponent(t, Viewer(ViewerData{
		Artifact:    art,
		Current:     versions[2],
		Versions:    versions,
		Submissions: subs,
	}))
	assertNoEmojis(t, "ViewerLatest", html)

	wants := []string{
		`data-version-select="true"`,
		`data-slug="decision-sheet"`,
		`data-prev-version-href="/a/decision-sheet/v/2"`,
		`src="/raw/decision-sheet/v/3/"`,
		`href="/raw/decision-sheet/v/3/"`,
		`data-refresh-preview="true"`,
		`data-copy-url="/a/decision-sheet/v/3"`,
		`data-delete-artifact="true"`,
		`data-copy-submission="true"`,
		`data-submission-url="/api/v1/artifacts/decision-sheet/submissions/42"`,
		`decisions.json`,
		`assets/app.js`,
		`/raw/decision-sheet/v/3/assets/app.js`,
		`v3 (latest)`,
		`Versions (3)`,
		`Files (2)`,
		`Submissions (1)`,
	}
	for _, want := range wants {
		if !strings.Contains(html, want) {
			t.Errorf("Viewer (latest) HTML missing expected substring %q", want)
		}
	}

	if strings.Contains(html, `data-older-version-banner`) {
		t.Errorf("Viewer (latest) should not render older-version notice banner")
	}
	if strings.Contains(html, `data-next-version-href`) {
		t.Errorf("Viewer (latest) should disable next-version button without data-next-version-href")
	}
}

func TestViewerOlderVersion(t *testing.T) {
	now := time.Now()
	art := model.Artifact{
		ID:            10,
		Slug:          "decision-sheet",
		Title:         "Decision Sheet",
		LatestVersion: 3,
		VersionCount:  3,
	}
	versions := []model.Version{
		{ID: 3, Slug: art.Slug, Number: 3, CreatedAt: now.Add(-1 * time.Hour)},
		{ID: 2, Slug: art.Slug, Number: 2, CreatedAt: now.Add(-2 * time.Hour)},
		{ID: 1, Slug: art.Slug, Number: 1, CreatedAt: now.Add(-3 * time.Hour)},
	}

	html := renderComponent(t, Viewer(ViewerData{
		Artifact: art,
		Current:  versions[2], // v1
		Versions: versions,
	}))
	assertNoEmojis(t, "ViewerOlder", html)

	wants := []string{
		`data-older-version-banner="true"`,
		`Viewing v1 — latest is v3`,
		`Jump to latest`,
		`Restore as latest`,
		`data-rollback-version="true"`,
		`data-version="1"`,
		`data-next-version-href="/a/decision-sheet/v/2"`,
		`src="/raw/decision-sheet/v/1/"`,
	}
	for _, want := range wants {
		if !strings.Contains(html, want) {
			t.Errorf("Viewer (older) HTML missing expected substring %q", want)
		}
	}
	if strings.Contains(html, `data-prev-version-href`) {
		t.Errorf("Viewer (v1) should disable previous-version button without data-prev-version-href")
	}
}

func TestErrorPage(t *testing.T) {
	html := renderComponent(t, ErrorPage(ErrorData{
		StatusCode: 404,
		Detail:     `slug "missing-doc" was not found`,
	}))
	assertNoEmojis(t, "ErrorPage", html)

	wants := []string{
		"HTTP 404",
		"Artifact not found",
		"Back to artifacts",
		`href="/"`,
		`missing-doc`,
		"lucide-circle-alert",
	}
	for _, want := range wants {
		if !strings.Contains(html, want) {
			t.Errorf("ErrorPage HTML missing expected substring %q", want)
		}
	}
}

func TestStaticFSEmbedsAssets(t *testing.T) {
	required := []string{
		"static/app.css",
		"static/ui.js",
		"static/bridge.js",
	}
	for _, path := range required {
		data, err := fs.ReadFile(ui.StaticFS, path)
		if err != nil {
			t.Fatalf("StaticFS missing %s: %v", path, err)
		}
		if len(data) == 0 {
			t.Errorf("StaticFS file %s is empty", path)
		}
		if strings.HasSuffix(path, ".js") {
			assertNoEmojis(t, path, string(data))
		}
	}

	bridgeBytes, _ := fs.ReadFile(ui.StaticFS, "static/bridge.js")
	bridgeSrc := string(bridgeBytes)
	for _, want := range []string{"window.artifact", "window.claude", "downloads", "Saved ", "to artifact host"} {
		if !strings.Contains(bridgeSrc, want) {
			t.Errorf("bridge.js missing expected contract token %q", want)
		}
	}

	jsEntries, err := fs.ReadDir(ui.StaticFS, "static/js")
	if err != nil {
		t.Fatalf("StaticFS missing static/js directory: %v", err)
	}
	foundBundle := false
	for _, e := range jsEntries {
		if strings.HasPrefix(e.Name(), "shadcn-templ-") && strings.HasSuffix(e.Name(), ".js") {
			foundBundle = true
			break
		}
	}
	if !foundBundle {
		t.Errorf("StaticFS static/js missing shadcn-templ-*.js bundle")
	}
}

func TestHelpers(t *testing.T) {
	now := time.Date(2026, time.November, 25, 15, 0, 0, 0, time.UTC)

	if got := relativeTimeFrom(now.Add(-20*time.Second), now); got != "just now" {
		t.Errorf("got %q, want just now", got)
	}
	if got := relativeTimeFrom(now.Add(-7*time.Minute), now); got != "7m ago" {
		t.Errorf("got %q, want 7m ago", got)
	}
	if got := relativeTimeFrom(now.Add(-4*time.Hour), now); got != "4h ago" {
		t.Errorf("got %q, want 4h ago", got)
	}
	if got := relativeTimeFrom(now.Add(-3*24*time.Hour), now); got != "3d ago" {
		t.Errorf("got %q, want 3d ago", got)
	}
	if got := relativeTimeFrom(time.Date(2026, time.October, 10, 12, 0, 0, 0, time.UTC), now); got != "Oct 10" {
		t.Errorf("got %q, want Oct 10", got)
	}
	if got := relativeTimeFrom(time.Date(2025, time.December, 5, 12, 0, 0, 0, time.UTC), now); got != "Dec 5, 2025" {
		t.Errorf("got %q, want Dec 5, 2025", got)
	}

	if got := FormatBytes(512); got != "512 B" {
		t.Errorf("got %q, want 512 B", got)
	}
	if got := FormatBytes(2048); got != "2.0 KB" {
		t.Errorf("got %q, want 2.0 KB", got)
	}
	if got := FormatBytes(3 * 1024 * 1024); got != "3.0 MB" {
		t.Errorf("got %q, want 3.0 MB", got)
	}

	if got := galleryFilterURL("perf", "metrics"); got != "/?q=perf&tag=metrics" {
		t.Errorf("got %q, want /?q=perf&tag=metrics", got)
	}
	if got := galleryFilterURL("", ""); got != "/" {
		t.Errorf("got %q, want /", got)
	}

	svg := renderComponent(t, ArtifactIcon("does-not-exist"))
	if !strings.Contains(svg, "lucide-file-code") {
		t.Errorf("ArtifactIcon fallback expected lucide-file-code, got %q", svg)
	}
}
