package preview

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/deastl/artifact-host/internal/model"
)

type statusUpdate struct {
	slug        string
	version     int
	status      string
	previewHash string
	errMsg      string
}

type mockStore struct {
	mu       sync.Mutex
	dir      string
	versions map[string]*model.Version
	updates  []statusUpdate
}

func newMockStore(dir string) *mockStore {
	return &mockStore{
		dir:      dir,
		versions: make(map[string]*model.Version),
	}
}

func (s *mockStore) versionKey(slug string, versionNum int) string {
	return fmt.Sprintf("%s/%d", slug, versionNum)
}

func (s *mockStore) putVersion(v *model.Version) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.versions[s.versionKey(v.Slug, v.Number)] = v
}

func (s *mockStore) GetVersion(_ context.Context, slug string, versionNum int) (*model.Version, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.versions[s.versionKey(slug, versionNum)]
	if !ok {
		return nil, fmt.Errorf("version %s v%d not found", slug, versionNum)
	}
	cp := *v
	return &cp, nil
}

func (s *mockStore) UpdatePreviewStatus(_ context.Context, slug string, versionNum int, status, previewHash, errMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.updates = append(s.updates, statusUpdate{
		slug:        slug,
		version:     versionNum,
		status:      status,
		previewHash: previewHash,
		errMsg:      errMsg,
	})
	if v, ok := s.versions[s.versionKey(slug, versionNum)]; ok {
		v.PreviewStatus = status
		v.PreviewHash = previewHash
		v.PreviewError = errMsg
	}
	return nil
}

func (s *mockStore) PreviewFilePath(slug string, versionNum int, contentHash string) string {
	return filepath.Join(s.dir, "previews", slug, fmt.Sprintf("v%d-%s.png", versionNum, contentHash))
}

func (s *mockStore) lastUpdate() (statusUpdate, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.updates) == 0 {
		return statusUpdate{}, false
	}
	return s.updates[len(s.updates)-1], true
}

func (s *mockStore) updateCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.updates)
}

func writeSamplePNG(t *testing.T, path string, w, h int, c color.RGBA) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetRGBA(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create png %s: %v", path, err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode png %s: %v", path, err)
	}
}

func containsEmoji(s string) bool {
	for _, r := range s {
		if (r >= 0x1F300 && r <= 0x1FAFF) ||
			(r >= 0x2600 && r <= 0x27BF) ||
			(r >= 0x1F1E6 && r <= 0x1F1FF) ||
			r == 0xFE0F || r == 0x200D {
			return true
		}
	}
	return false
}

func TestPlaceholderSVG(t *testing.T) {
	cases := []struct {
		name           string
		title          string
		status         string
		wantCaption    string
		wantContains   []string
		wantNotContain []string
	}{
		{
			name:         "pending status",
			title:        "Quarterly Metrics",
			status:       model.PreviewPending,
			wantCaption:  "Generating preview...",
			wantContains: []string{`width="640"`, `height="400"`, `viewBox="0 0 640 400"`, "Quarterly Metrics", "Generating preview..."},
		},
		{
			name:         "disabled status",
			title:        "Release Notes",
			status:       model.PreviewDisabled,
			wantCaption:  "Preview unavailable",
			wantContains: []string{"Release Notes", "Preview unavailable"},
		},
		{
			name:         "failed status",
			title:        "Broken Demo",
			status:       model.PreviewFailed,
			wantCaption:  "Preview failed",
			wantContains: []string{"Broken Demo", "Preview failed"},
		},
		{
			name:           "xml escaping",
			title:          `<script>alert("x&y")</script> & 'demo'`,
			status:         `Custom <status> & "info"`,
			wantContains:   []string{"&lt;script&gt;", "&amp;", "&#34;", "&#39;", "Custom &lt;status&gt; &amp; &#34;info&#34;"},
			wantNotContain: []string{"<script>", "<status>"},
		},
		{
			name:         "empty title and status",
			title:        "   ",
			status:       "",
			wantContains: []string{"Untitled Artifact", "Preview unavailable"},
		},
		{
			name:         "long title truncation with entities",
			title:        strings.Repeat("A&B", 30),
			status:       model.PreviewPending,
			wantContains: []string{"...", "&amp;"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := PlaceholderSVG(tc.title, tc.status)
			svgStr := string(raw)

			if containsEmoji(svgStr) {
				t.Fatalf("PlaceholderSVG output contains emoji characters: %s", svgStr)
			}

			var parsed struct {
				XMLName xml.Name `xml:"svg"`
				Width   string   `xml:"width,attr"`
				Height  string   `xml:"height,attr"`
			}
			if err := xml.Unmarshal(raw, &parsed); err != nil {
				t.Fatalf("PlaceholderSVG returned invalid XML: %v\n%s", err, svgStr)
			}
			if parsed.Width != "640" || parsed.Height != "400" {
				t.Fatalf("unexpected svg dimensions: %sx%s", parsed.Width, parsed.Height)
			}

			for _, want := range tc.wantContains {
				if !strings.Contains(svgStr, want) {
					t.Errorf("expected SVG to contain %q, got:\n%s", want, svgStr)
				}
			}
			for _, forbid := range tc.wantNotContain {
				if strings.Contains(svgStr, forbid) {
					t.Errorf("expected SVG NOT to contain %q, got:\n%s", forbid, svgStr)
				}
			}
		})
	}
}

type genericImageWrapper struct {
	image.Image
}

func TestDownscaleImage(t *testing.T) {
	t.Run("exact 2x RGBA 1280x800 to 640x400", func(t *testing.T) {
		src := image.NewRGBA(image.Rect(0, 0, 1280, 800))
		// Left half: checkerboard of (0,0,0) and (200,100,50) -> average should be (100,50,25)
		// Right half: solid (20, 40, 220)
		for y := 0; y < 800; y++ {
			for x := 0; x < 1280; x++ {
				if x < 640 {
					if (x+y)%2 == 0 {
						src.SetRGBA(x, y, color.RGBA{R: 200, G: 100, B: 50, A: 255})
					} else {
						src.SetRGBA(x, y, color.RGBA{R: 0, G: 0, B: 0, A: 255})
					}
				} else {
					src.SetRGBA(x, y, color.RGBA{R: 20, G: 40, B: 220, A: 255})
				}
			}
		}

		dst := downscaleImage(src, 640, 400)
		if dst.Bounds().Dx() != 640 || dst.Bounds().Dy() != 400 {
			t.Fatalf("expected 640x400, got %dx%d", dst.Bounds().Dx(), dst.Bounds().Dy())
		}

		leftPx := dst.RGBAAt(10, 10)
		wantLeft := color.RGBA{R: 100, G: 50, B: 25, A: 255}
		if leftPx != wantLeft {
			t.Fatalf("left average pixel = %+v, want %+v", leftPx, wantLeft)
		}

		rightPx := dst.RGBAAt(500, 200)
		wantRight := color.RGBA{R: 20, G: 40, B: 220, A: 255}
		if rightPx != wantRight {
			t.Fatalf("right solid pixel = %+v, want %+v", rightPx, wantRight)
		}
	})

	t.Run("exact 2x NRGBA opaque and translucent", func(t *testing.T) {
		src := image.NewNRGBA(image.Rect(0, 0, 1280, 800))
		for y := 0; y < 800; y++ {
			for x := 0; x < 1280; x++ {
				if y < 400 {
					src.SetNRGBA(x, y, color.NRGBA{R: 80, G: 160, B: 240, A: 255})
				} else {
					src.SetNRGBA(x, y, color.NRGBA{R: 200, G: 100, B: 0, A: 128})
				}
			}
		}
		dst := downscaleImage(src, 640, 400)
		if dst.Bounds().Dx() != 640 || dst.Bounds().Dy() != 400 {
			t.Fatalf("expected 640x400, got %dx%d", dst.Bounds().Dx(), dst.Bounds().Dy())
		}
		topPx := dst.RGBAAt(100, 50)
		if topPx != (color.RGBA{R: 80, G: 160, B: 240, A: 255}) {
			t.Fatalf("top NRGBA pixel = %+v", topPx)
		}
		botPx := dst.RGBAAt(100, 300)
		if botPx.A != 128 || botPx.R < 99 || botPx.R > 101 {
			t.Fatalf("translucent NRGBA pixel = %+v", botPx)
		}
	})

	t.Run("exact 2x generic image", func(t *testing.T) {
		base := image.NewRGBA(image.Rect(0, 0, 1280, 800))
		for y := 0; y < 800; y++ {
			for x := 0; x < 1280; x++ {
				base.SetRGBA(x, y, color.RGBA{R: 60, G: 120, B: 180, A: 255})
			}
		}
		dst := downscaleImage(genericImageWrapper{base}, 640, 400)
		if dst.Bounds().Dx() != 640 || dst.Bounds().Dy() != 400 {
			t.Fatalf("expected 640x400, got %dx%d", dst.Bounds().Dx(), dst.Bounds().Dy())
		}
		px := dst.RGBAAt(320, 200)
		if px != (color.RGBA{R: 60, G: 120, B: 180, A: 255}) {
			t.Fatalf("generic pixel = %+v", px)
		}
	})

	t.Run("non-2x area average 960x600 to 640x400", func(t *testing.T) {
		src := image.NewRGBA(image.Rect(0, 0, 960, 600))
		for y := 0; y < 600; y++ {
			for x := 0; x < 960; x++ {
				src.SetRGBA(x, y, color.RGBA{R: 120, G: 180, B: 60, A: 255})
			}
		}
		dst := downscaleImage(src, 640, 400)
		if dst.Bounds().Dx() != 640 || dst.Bounds().Dy() != 400 {
			t.Fatalf("expected 640x400, got %dx%d", dst.Bounds().Dx(), dst.Bounds().Dy())
		}
		px := dst.RGBAAt(300, 200)
		if px != (color.RGBA{R: 120, G: 180, B: 60, A: 255}) {
			t.Fatalf("area average pixel = %+v", px)
		}
	})

	t.Run("downscaleAndWritePNG writes atomically", func(t *testing.T) {
		tmp := t.TempDir()
		srcPath := filepath.Join(tmp, "raw.png")
		dstPath := filepath.Join(tmp, "nested", "thumb.png")
		writeSamplePNG(t, srcPath, 1280, 800, color.RGBA{R: 30, G: 144, B: 255, A: 255})

		if err := downscaleAndWritePNG(srcPath, dstPath, thumbWidth, thumbHeight); err != nil {
			t.Fatalf("downscaleAndWritePNG failed: %v", err)
		}

		f, err := os.Open(dstPath)
		if err != nil {
			t.Fatalf("open output png: %v", err)
		}
		defer f.Close()
		img, err := png.Decode(f)
		if err != nil {
			t.Fatalf("decode output png: %v", err)
		}
		if img.Bounds().Dx() != 640 || img.Bounds().Dy() != 400 {
			t.Fatalf("expected 640x400 output, got %dx%d", img.Bounds().Dx(), img.Bounds().Dy())
		}

		entries, err := os.ReadDir(filepath.Dir(dstPath))
		if err != nil {
			t.Fatalf("readdir: %v", err)
		}
		if len(entries) != 1 || entries[0].Name() != "thumb.png" {
			t.Fatalf("unexpected leftover files in preview dir: %+v", entries)
		}
	})
}

func TestCacheHitAndForceRerender(t *testing.T) {
	store := newMockStore(t.TempDir())
	m := NewManager(Config{BaseURL: "http://127.0.0.1:7337/"}, store)
	m.chromePath = "/fake/chrome"

	var renderCalls atomic.Int32
	var capturedURL atomic.Value
	m.renderFn = func(_ context.Context, targetURL, dstPath string) error {
		renderCalls.Add(1)
		capturedURL.Store(targetURL)
		writeSamplePNG(t, dstPath, thumbWidth, thumbHeight, color.RGBA{R: 10, G: 20, B: 30, A: 255})
		return nil
	}

	// Pre-populate cached file.
	cachedPath := store.PreviewFilePath("demo-app", 1, "hash-abc")
	writeSamplePNG(t, cachedPath, thumbWidth, thumbHeight, color.RGBA{R: 255, G: 0, B: 0, A: 255})

	gotPath, ok := m.GetCached("demo-app", 1, "hash-abc")
	if !ok || gotPath != cachedPath {
		t.Fatalf("GetCached = (%q, %v), want (%q, true)", gotPath, ok, cachedPath)
	}

	// RenderSync without force must return cached file immediately without rendering.
	p, err := m.RenderSync(context.Background(), "demo-app", 1, "hash-abc", false)
	if err != nil {
		t.Fatalf("RenderSync cache hit failed: %v", err)
	}
	if p != cachedPath {
		t.Fatalf("RenderSync path = %q, want %q", p, cachedPath)
	}
	if renderCalls.Load() != 0 {
		t.Fatalf("expected 0 render calls on cache hit, got %d", renderCalls.Load())
	}

	// Enqueue without force must also skip rendering.
	m.Start(context.Background())
	m.Enqueue("demo-app", 1, "hash-abc", false)
	time.Sleep(50 * time.Millisecond)
	m.Stop()
	if renderCalls.Load() != 0 {
		t.Fatalf("expected 0 render calls on Enqueue cache hit, got %d", renderCalls.Load())
	}

	// RenderSync with force=true must re-render and update status to ready.
	p, err = m.RenderSync(context.Background(), "demo-app", 1, "hash-abc", true)
	if err != nil {
		t.Fatalf("RenderSync force failed: %v", err)
	}
	if p != cachedPath {
		t.Fatalf("RenderSync force path = %q, want %q", p, cachedPath)
	}
	if renderCalls.Load() != 1 {
		t.Fatalf("expected 1 render call on force, got %d", renderCalls.Load())
	}
	wantURL := "http://127.0.0.1:7337/raw/demo-app/v1/?preview=1"
	if gotURL, _ := capturedURL.Load().(string); gotURL != wantURL {
		t.Fatalf("targetURL = %q, want %q", gotURL, wantURL)
	}
	upd, ok := store.lastUpdate()
	if !ok || upd.status != model.PreviewReady || upd.previewHash != "hash-abc" || upd.errMsg != "" {
		t.Fatalf("unexpected store update: %+v (ok=%v)", upd, ok)
	}
}

func TestEnqueueDeduplication(t *testing.T) {
	store := newMockStore(t.TempDir())
	m := NewManager(Config{BaseURL: "http://127.0.0.1:7337", Workers: 2}, store)
	m.chromePath = "/fake/chrome"

	started := make(chan struct{}, 10)
	release := make(chan struct{})
	var calls atomic.Int32

	m.renderFn = func(_ context.Context, _ string, dstPath string) error {
		calls.Add(1)
		started <- struct{}{}
		<-release
		writeSamplePNG(t, dstPath, thumbWidth, thumbHeight, color.RGBA{R: 50, G: 50, B: 50, A: 255})
		return nil
	}

	m.Start(context.Background())

	// Enqueue the same key many times concurrently while render is blocked.
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Enqueue("dedup-slug", 2, "hash-dup", false)
		}()
	}
	wg.Wait()

	// Wait for the single render to begin, then enqueue again while in-flight.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for worker to start render")
	}
	m.Enqueue("dedup-slug", 2, "hash-dup", true)

	close(release)

	// Wait for the store update before stopping so the worker finishes cleanly.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if store.updateCount() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	m.Stop()

	if got := calls.Load(); got != 1 {
		t.Fatalf("expected deduplicated render count 1, got %d", got)
	}
	upd, ok := store.lastUpdate()
	if !ok || upd.status != model.PreviewReady || upd.previewHash != "hash-dup" {
		t.Fatalf("unexpected final status: %+v (ok=%v)", upd, ok)
	}
}

func TestDisabledModeAndMissingChrome(t *testing.T) {
	t.Run("disabled via config", func(t *testing.T) {
		store := newMockStore(t.TempDir())
		m := NewManager(Config{BaseURL: "http://127.0.0.1:7337", Disabled: true}, store)

		avail, path := m.ChromeInfo()
		if avail || path != "" {
			t.Fatalf("ChromeInfo() = (%v, %q), want (false, \"\")", avail, path)
		}

		_, err := m.RenderSync(context.Background(), "disabled-app", 1, "hash-1", false)
		if !errors.Is(err, ErrPreviewDisabled) {
			t.Fatalf("RenderSync error = %v, want ErrPreviewDisabled", err)
		}
		upd, ok := store.lastUpdate()
		if !ok || upd.status != model.PreviewDisabled {
			t.Fatalf("unexpected store update: %+v (ok=%v)", upd, ok)
		}

		// Enqueue in disabled mode should mark PreviewDisabled without panicking or blocking.
		m.Enqueue("disabled-app", 2, "hash-2", false)
		upd2, ok := store.lastUpdate()
		if !ok || upd2.version != 2 || upd2.status != model.PreviewDisabled {
			t.Fatalf("unexpected Enqueue update: %+v (ok=%v)", upd2, ok)
		}
	})

	t.Run("missing chrome binary", func(t *testing.T) {
		store := newMockStore(t.TempDir())
		m := NewManager(Config{
			BaseURL:   "http://127.0.0.1:7337",
			ChromeBin: "/nonexistent/definitely-missing-chrome-binary",
		}, store)

		avail, path := m.ChromeInfo()
		if avail || path != "" {
			t.Fatalf("ChromeInfo() = (%v, %q), want (false, \"\")", avail, path)
		}

		_, err := m.RenderSync(context.Background(), "no-chrome", 1, "hash-x", false)
		if !errors.Is(err, ErrChromeUnavailable) {
			t.Fatalf("RenderSync error = %v, want ErrChromeUnavailable", err)
		}
		upd, ok := store.lastUpdate()
		if !ok || upd.status != model.PreviewDisabled {
			t.Fatalf("unexpected store update: %+v (ok=%v)", upd, ok)
		}
	})
}

func TestRenderSyncFailureAndVersionLookup(t *testing.T) {
	store := newMockStore(t.TempDir())
	store.putVersion(&model.Version{
		Slug:        "lookup-app",
		Number:      3,
		ContentHash: "resolved-hash-3",
	})

	m := NewManager(Config{}, store)
	m.chromePath = "/fake/chrome"
	m.SetBaseURL("http://127.0.0.1:9999/")

	m.renderFn = func(_ context.Context, _ string, _ string) error {
		return errors.New("simulated chrome crash")
	}

	// Pass empty contentHash so Manager resolves it via StoreBackend.GetVersion.
	_, err := m.RenderSync(context.Background(), "lookup-app", 3, "", false)
	if err == nil || !strings.Contains(err.Error(), "simulated chrome crash") {
		t.Fatalf("expected simulated chrome crash error, got %v", err)
	}
	upd, ok := store.lastUpdate()
	if !ok || upd.status != model.PreviewFailed || !strings.Contains(upd.errMsg, "simulated chrome crash") {
		t.Fatalf("unexpected failure status update: %+v (ok=%v)", upd, ok)
	}
}

func TestRealHeadlessChromeRender(t *testing.T) {
	store := newMockStore(t.TempDir())
	m := NewManager(Config{
		Workers: 1,
		Timeout: 20 * time.Second,
	}, store)

	avail, chromePath := m.ChromeInfo()
	if !avail {
		t.Skip("headless chrome binary not found on host; skipping real browser test")
	}
	t.Logf("using headless chrome at %s", chromePath)

	var hits atomic.Int32
	var sawPreviewParam atomic.Bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/raw/demo-card/v1/" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		if r.URL.Query().Get("preview") == "1" {
			sawPreviewParam.Store(true)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html>
<html>
<head>
<meta charset="utf-8">
<style>
  html, body { margin: 0; padding: 0; width: 1280px; height: 800px; overflow: hidden; }
  .top { height: 400px; background: #dc2626; display: flex; align-items: center; justify-content: center; color: #ffffff; font: 700 48px sans-serif; }
  .bottom { height: 400px; background: #2563eb; display: flex; align-items: center; justify-content: center; color: #ffffff; font: 600 32px sans-serif; }
</style>
</head>
<body>
  <div class="top">Artifact Preview Test</div>
  <div class="bottom">Rendered via Headless Chrome</div>
</body>
</html>`))
	}))
	defer srv.Close()

	m.SetBaseURL(srv.URL)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()

	outPath, err := m.RenderSync(ctx, "demo-card", 1, "sha256-test1", false)
	if err != nil {
		t.Fatalf("RenderSync with real Chrome failed: %v", err)
	}
	if !sawPreviewParam.Load() {
		t.Fatalf("expected Chrome request to include ?preview=1 query parameter")
	}
	if hits.Load() != 1 {
		t.Fatalf("expected 1 HTTP hit, got %d", hits.Load())
	}

	upd, ok := store.lastUpdate()
	if !ok || upd.status != model.PreviewReady || upd.previewHash != "sha256-test1" || upd.errMsg != "" {
		t.Fatalf("unexpected store status after Chrome render: %+v (ok=%v)", upd, ok)
	}

	f, err := os.Open(outPath)
	if err != nil {
		t.Fatalf("open rendered png: %v", err)
	}
	defer f.Close()

	img, err := png.Decode(f)
	if err != nil {
		t.Fatalf("decode rendered png: %v", err)
	}
	if img.Bounds().Dx() != 640 || img.Bounds().Dy() != 400 {
		t.Fatalf("rendered png bounds = %dx%d, want 640x400", img.Bounds().Dx(), img.Bounds().Dy())
	}

	// Verify actual rendered colors: top-left corner should be reddish (#dc2626), bottom-left bluish (#2563eb).
	topR, topG, topB, _ := img.At(20, 20).RGBA()
	botR, botG, botB, _ := img.At(20, 380).RGBA()
	if topR <= topB || topR <= topG {
		t.Fatalf("expected top region to be predominantly red, got RGBA=(%d,%d,%d)", topR>>8, topG>>8, topB>>8)
	}
	if botB <= botR || botB <= botG {
		t.Fatalf("expected bottom region to be predominantly blue, got RGBA=(%d,%d,%d)", botR>>8, botG>>8, botB>>8)
	}

	// Subsequent GetCached and non-force RenderSync should hit cache without hitting server again.
	cachedPath, ok := m.GetCached("demo-card", 1, "sha256-test1")
	if !ok || cachedPath != outPath {
		t.Fatalf("GetCached = (%q, %v), want (%q, true)", cachedPath, ok, outPath)
	}
	outPath2, err := m.RenderSync(ctx, "demo-card", 1, "sha256-test1", false)
	if err != nil {
		t.Fatalf("second RenderSync failed: %v", err)
	}
	if outPath2 != outPath {
		t.Fatalf("second RenderSync path = %q, want %q", outPath2, outPath)
	}
	if hits.Load() != 1 {
		t.Fatalf("expected cache hit to avoid HTTP request; hits = %d", hits.Load())
	}
}
