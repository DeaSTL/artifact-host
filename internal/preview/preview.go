package preview

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/deastl/artifact-host/internal/model"
)

const (
	defaultWorkers   = 2
	defaultTimeout   = 15 * time.Second
	minQueueCapacity = 64
	maxErrMsgLen     = 500
)

var (
	// ErrPreviewDisabled is returned when preview rendering is explicitly disabled in Config.
	ErrPreviewDisabled = errors.New("preview rendering is disabled")
	// ErrChromeUnavailable is returned when no headless Chrome/Chromium binary can be found.
	ErrChromeUnavailable = errors.New("headless chrome binary not found")
)

var chromeCandidates = []string{
	"google-chrome",
	"chromium",
	"chromium-browser",
	"google-chrome-stable",
}

// StoreBackend defines the narrow storage contract required by the preview manager.
type StoreBackend interface {
	GetVersion(ctx context.Context, slug string, versionNum int) (*model.Version, error)
	UpdatePreviewStatus(ctx context.Context, slug string, versionNum int, status, previewHash, errMsg string) error
	PreviewFilePath(slug string, versionNum int, contentHash string) string
}

// Config configures the headless browser preview manager.
type Config struct {
	BaseURL   string
	ChromeBin string
	Workers   int
	Timeout   time.Duration
	Disabled  bool
}

type jobKey struct {
	slug        string
	version     int
	contentHash string
}

type renderJob struct {
	slug        string
	version     int
	contentHash string
	force       bool
}

// Manager coordinates asynchronous and synchronous headless-browser preview rendering and caching.
type Manager struct {
	store      StoreBackend
	workers    int
	timeout    time.Duration
	disabled   bool
	chromePath string

	mu       sync.Mutex
	baseURL  string
	started  bool
	stopped  bool
	cancel   context.CancelFunc
	inFlight map[jobKey]struct{}

	queue chan renderJob
	wg    sync.WaitGroup

	// renderFn executes a single capture from targetURL into dstPath.
	// Defaults to m.renderWithChrome; overridable in tests.
	renderFn func(ctx context.Context, targetURL, dstPath string) error
}

// NewManager creates a preview Manager, resolving defaults and discovering a Chrome binary if enabled.
func NewManager(cfg Config, store StoreBackend) *Manager {
	workers := cfg.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	var chromePath string
	if !cfg.Disabled {
		chromePath = resolveChromeBin(cfg.ChromeBin)
	}

	queueCap := workers * 32
	if queueCap < minQueueCapacity {
		queueCap = minQueueCapacity
	}

	m := &Manager{
		store:      store,
		workers:    workers,
		timeout:    timeout,
		disabled:   cfg.Disabled,
		chromePath: chromePath,
		baseURL:    normalizeBaseURL(cfg.BaseURL),
		inFlight:   make(map[jobKey]struct{}),
		queue:      make(chan renderJob, queueCap),
	}
	m.renderFn = m.renderWithChrome
	return m
}

func resolveChromeBin(override string) string {
	if override = strings.TrimSpace(override); override != "" {
		if p, err := exec.LookPath(override); err == nil {
			return p
		}
		return ""
	}
	for _, candidate := range chromeCandidates {
		if p, err := exec.LookPath(candidate); err == nil {
			return p
		}
	}
	return ""
}

func normalizeBaseURL(raw string) string {
	return strings.TrimRight(strings.TrimSpace(raw), "/")
}

// ChromeInfo reports whether headless Chrome rendering is available and the resolved executable path.
func (m *Manager) ChromeInfo() (available bool, path string) {
	if m.disabled || m.chromePath == "" {
		return false, ""
	}
	return true, m.chromePath
}

// SetBaseURL updates the base HTTP URL used to reach rendered artifact pages.
func (m *Manager) SetBaseURL(rawURL string) {
	m.mu.Lock()
	m.baseURL = normalizeBaseURL(rawURL)
	m.mu.Unlock()
}

func (m *Manager) getBaseURL() string {
	m.mu.Lock()
	u := m.baseURL
	m.mu.Unlock()
	return u
}

// Start launches the background preview worker pool. Calling Start on an already-started manager is a no-op.
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	workerCtx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.started = true
	m.stopped = false
	m.mu.Unlock()

	for i := 0; i < m.workers; i++ {
		m.wg.Add(1)
		go m.worker(workerCtx)
	}
}

// Stop signals all background workers to exit and blocks until in-flight renders finish.
func (m *Manager) Stop() {
	m.mu.Lock()
	m.stopped = true
	cancel := m.cancel
	m.cancel = nil
	m.started = false
	m.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	m.wg.Wait()

	m.mu.Lock()
	clear(m.inFlight)
	m.mu.Unlock()
}

func (m *Manager) worker(ctx context.Context) {
	defer m.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case job := <-m.queue:
			m.processJob(ctx, job)
		}
	}
}

func (m *Manager) processJob(ctx context.Context, job renderJob) {
	key := jobKey{
		slug:        job.slug,
		version:     job.version,
		contentHash: job.contentHash,
	}
	defer func() {
		m.mu.Lock()
		delete(m.inFlight, key)
		m.mu.Unlock()
	}()

	if ctx.Err() != nil {
		return
	}
	_, _ = m.RenderSync(ctx, job.slug, job.version, job.contentHash, job.force)
}

// GetCached returns the cached preview PNG path if it already exists on disk and is non-empty.
func (m *Manager) GetCached(slug string, version int, contentHash string) (string, bool) {
	if m.store == nil || slug == "" || version <= 0 || contentHash == "" {
		return "", false
	}
	p := m.store.PreviewFilePath(slug, version, contentHash)
	if p == "" {
		return "", false
	}
	info, err := os.Stat(p)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return "", false
	}
	return p, true
}

// Enqueue schedules a non-blocking preview render job.
// It skips work when !force and a cached thumbnail already exists, and deduplicates
// concurrent jobs sharing the same (slug, version, contentHash) key.
func (m *Manager) Enqueue(slug string, version int, contentHash string, force bool) {
	if slug == "" || version <= 0 {
		return
	}
	if !force {
		if _, ok := m.GetCached(slug, version, contentHash); ok {
			return
		}
	}
	if unavailableErr := m.availabilityError(); unavailableErr != nil {
		m.updateStatus(context.Background(), slug, version, model.PreviewDisabled, "", unavailableErr.Error())
		return
	}

	key := jobKey{
		slug:        slug,
		version:     version,
		contentHash: contentHash,
	}

	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	if _, exists := m.inFlight[key]; exists {
		m.mu.Unlock()
		return
	}
	m.inFlight[key] = struct{}{}
	select {
	case m.queue <- renderJob{
		slug:        slug,
		version:     version,
		contentHash: contentHash,
		force:       force,
	}:
	default:
		delete(m.inFlight, key)
	}
	m.mu.Unlock()
}

// RenderSync returns the cached preview path immediately when !force and a cached file exists.
// Otherwise it renders synchronously and updates the version's preview status in the store.
func (m *Manager) RenderSync(ctx context.Context, slug string, version int, contentHash string, force bool) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if m.store == nil {
		return "", errors.New("preview store backend is not configured")
	}
	if slug == "" || version <= 0 {
		return "", fmt.Errorf("invalid preview target: slug=%q version=%d", slug, version)
	}

	if contentHash == "" {
		v, err := m.store.GetVersion(ctx, slug, version)
		if err != nil {
			return "", fmt.Errorf("load version for preview: %w", err)
		}
		if v == nil || v.ContentHash == "" {
			return "", fmt.Errorf("version %s v%d has no content hash", slug, version)
		}
		contentHash = v.ContentHash
	}

	if !force {
		if cachedPath, ok := m.GetCached(slug, version, contentHash); ok {
			return cachedPath, nil
		}
	}

	if unavailableErr := m.availabilityError(); unavailableErr != nil {
		m.updateStatus(ctx, slug, version, model.PreviewDisabled, "", unavailableErr.Error())
		return "", unavailableErr
	}

	baseURL := m.getBaseURL()
	if baseURL == "" {
		err := errors.New("preview base URL is not configured")
		m.updateStatus(ctx, slug, version, model.PreviewFailed, "", err.Error())
		return "", err
	}

	dstPath := m.store.PreviewFilePath(slug, version, contentHash)
	if dstPath == "" {
		err := errors.New("preview file path is empty")
		m.updateStatus(ctx, slug, version, model.PreviewFailed, "", err.Error())
		return "", err
	}

	targetURL := fmt.Sprintf("%s/raw/%s/v%d/?preview=1", baseURL, url.PathEscape(slug), version)

	renderCtx, cancel := context.WithTimeout(ctx, m.timeout)
	defer cancel()

	if err := m.renderFn(renderCtx, targetURL, dstPath); err != nil {
		m.updateStatus(ctx, slug, version, model.PreviewFailed, "", truncateErrMsg(err.Error()))
		return "", err
	}

	m.updateStatus(ctx, slug, version, model.PreviewReady, contentHash, "")
	return dstPath, nil
}

func (m *Manager) availabilityError() error {
	if m.disabled {
		return ErrPreviewDisabled
	}
	if m.chromePath == "" {
		return ErrChromeUnavailable
	}
	return nil
}

func (m *Manager) updateStatus(ctx context.Context, slug string, version int, status, previewHash, errMsg string) {
	if m.store == nil {
		return
	}
	statusCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = m.store.UpdatePreviewStatus(statusCtx, slug, version, status, previewHash, errMsg)
}

func (m *Manager) renderWithChrome(ctx context.Context, targetURL, dstPath string) error {
	tmpDir, err := os.MkdirTemp("", "artifact-preview-*")
	if err != nil {
		return fmt.Errorf("create temp preview workspace: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	profileDir := filepath.Join(tmpDir, "profile")
	rawPNG := filepath.Join(tmpDir, "raw.png")

	args := []string{
		"--headless=new",
		"--disable-gpu",
		"--no-sandbox",
		"--disable-dev-shm-usage",
		"--hide-scrollbars",
		fmt.Sprintf("--window-size=%d,%d", viewportWidth, viewportHeight),
		"--virtual-time-budget=2000",
		"--user-data-dir=" + profileDir,
		"--screenshot=" + rawPNG,
		targetURL,
	}

	cmd := exec.CommandContext(ctx, m.chromePath, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second

	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("chrome screenshot timed out or canceled: %w", ctx.Err())
		}
		outStr := strings.TrimSpace(string(output))
		if outStr != "" {
			return fmt.Errorf("chrome screenshot failed: %w: %s", err, truncateErrMsg(outStr))
		}
		return fmt.Errorf("chrome screenshot failed: %w", err)
	}

	info, err := os.Stat(rawPNG)
	if err != nil || info.Size() == 0 {
		return fmt.Errorf("chrome did not produce screenshot output at %s", rawPNG)
	}

	if err := downscaleAndWritePNG(rawPNG, dstPath, thumbWidth, thumbHeight); err != nil {
		return fmt.Errorf("downscale screenshot: %w", err)
	}
	return nil
}

func truncateErrMsg(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) <= maxErrMsgLen {
		return msg
	}
	return msg[:maxErrMsgLen-3] + "..."
}
