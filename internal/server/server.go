package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/deastl/artifact-host/internal/model"
	"github.com/deastl/artifact-host/internal/preview"
	"github.com/deastl/artifact-host/internal/store"
	"github.com/deastl/artifact-host/internal/ui"
)

const (
	defaultListenAddr = "127.0.0.1:7337"
	shutdownTimeout   = 5 * time.Second
)

// Config configures the artifact-host HTTP server and runtime.
type Config struct {
	Addr       string
	DataDir    string
	ChromeBin  string
	Version    string
	NoPreviews bool
	Workers    int
}

// Server serves the web UI, raw artifacts, preview thumbnails, and REST API.
type Server struct {
	cfg       Config
	store     *store.Store
	previews  *preview.Manager
	startedAt time.Time
	router    chi.Router
	subsHub   *submissionHub

	addrMu     sync.RWMutex
	listenAddr string
}

// New constructs a Server with the given configuration, store, and optional preview manager.
func New(cfg Config, st *store.Store, pm *preview.Manager) *Server {
	s := &Server{
		cfg:        cfg,
		store:      st,
		previews:   pm,
		startedAt:  time.Now().UTC(),
		subsHub:    newSubmissionHub(),
		listenAddr: strings.TrimSpace(cfg.Addr),
	}
	s.router = s.buildRouter()
	return s
}

// Handler returns the root HTTP handler for the server.
func (s *Server) Handler() http.Handler {
	return s.router
}

func (s *Server) setListenAddr(addr string) {
	s.addrMu.Lock()
	s.listenAddr = addr
	s.addrMu.Unlock()
}

func (s *Server) effectiveAddr() string {
	s.addrMu.RLock()
	addr := s.listenAddr
	s.addrMu.RUnlock()
	if addr != "" {
		return addr
	}
	if strings.TrimSpace(s.cfg.Addr) != "" {
		return strings.TrimSpace(s.cfg.Addr)
	}
	return defaultListenAddr
}

// Run opens the store, starts the preview manager and HTTP listener, serves until ctx is canceled,
// and shuts down all components cleanly.
func Run(ctx context.Context, cfg Config) error {
	if ctx == nil {
		ctx = context.Background()
	}

	addr := strings.TrimSpace(cfg.Addr)
	if addr == "" {
		addr = defaultListenAddr
	}
	cfg.Addr = addr

	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer func() {
		_ = st.Close()
	}()

	pm := preview.NewManager(preview.Config{
		ChromeBin: cfg.ChromeBin,
		Workers:   cfg.Workers,
		Disabled:  cfg.NoPreviews,
	}, st)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", addr, err)
	}

	boundAddr := ln.Addr().String()
	pm.SetBaseURL(loopbackBaseURL(ln.Addr()))
	pm.Start(ctx)
	defer pm.Stop()

	srv := New(cfg, st, pm)
	srv.setListenAddr(boundAddr)

	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	serveErrCh := make(chan error, 1)
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErrCh <- err
			return
		}
		serveErrCh <- nil
	}()

	select {
	case err := <-serveErrCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		shutdownErr := httpSrv.Shutdown(shutdownCtx)
		serveErr := <-serveErrCh
		if shutdownErr != nil {
			return fmt.Errorf("shutdown http server: %w", shutdownErr)
		}
		return serveErr
	}
}

func loopbackBaseURL(addr net.Addr) string {
	host, port, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "http://" + addr.String()
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

func (s *Server) buildRouter() chi.Router {
	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	// Embedded static assets and artifact bridge script.
	if staticSub, err := fs.Sub(ui.StaticFS, "static"); err == nil {
		staticHandler := http.StripPrefix("/static/", http.FileServer(http.FS(staticSub)))
		r.Handle("/static/*", staticHandler)
	}
	r.Get("/_artifact/bridge.js", s.handleBridgeJS)
	r.Head("/_artifact/bridge.js", s.handleBridgeJS)

	// SSR UI routes.
	r.Get("/", s.handleGallery)
	r.Get("/a/{slug}", s.handleViewer)
	r.Get("/a/{slug}/v/{version}", s.handleViewerVersion)

	// Preview thumbnail routes (support both /previews/{slug}/v{version}.png and /previews/{slug}/v/{version}.png).
	r.Get("/previews/{slug}/v/{verFile}", s.handlePreview)
	r.Head("/previews/{slug}/v/{verFile}", s.handlePreview)
	r.Get("/previews/{slug}/{verFile}", s.handlePreview)
	r.Head("/previews/{slug}/{verFile}", s.handlePreview)

	// Raw artifact serving (support both /raw/{slug}/v/{version}[/*] and /raw/{slug}/v{version}[/*]).
	r.Get("/raw/{slug}/v/{version}", s.handleRawRedirectSlash)
	r.Head("/raw/{slug}/v/{version}", s.handleRawRedirectSlash)
	r.Get("/raw/{slug}/v/{version}/*", s.handleRawSlash)
	r.Head("/raw/{slug}/v/{version}/*", s.handleRawSlash)

	r.Get("/raw/{slug}/{verSeg}", s.handleRawRedirectCompact)
	r.Head("/raw/{slug}/{verSeg}", s.handleRawRedirectCompact)
	r.Get("/raw/{slug}/{verSeg}/*", s.handleRawCompact)
	r.Head("/raw/{slug}/{verSeg}/*", s.handleRawCompact)

	// REST API v1.
	r.Route("/api/v1", func(api chi.Router) {
		api.Get("/status", s.handleAPIStatus)
		api.Get("/artifacts", s.handleAPIListArtifacts)

		api.Route("/artifacts/{slug}", func(ar chi.Router) {
			ar.Get("/", s.handleAPIGetArtifact)
			ar.Delete("/", s.handleAPIDeleteArtifact)
			ar.Post("/rollback", s.handleAPIRollback)

			ar.Get("/versions", s.handleAPIListVersions)
			ar.Post("/versions", s.handleAPIPushVersion)
			ar.Get("/versions/{version}", s.handleAPIGetVersion)
			ar.Delete("/versions/{version}", s.handleAPIDeleteVersion)
			ar.Post("/versions/{version}/rollback", s.handleAPIRollbackVersion)
			ar.Post("/versions/{version}/preview/refresh", s.handleAPIRefreshPreview)
			ar.Post("/versions/{version}/submissions", s.handleAPICreateSubmission)

			ar.Get("/submissions", s.handleAPIListSubmissions)
			ar.Get("/submissions/{id}", s.handleAPIGetSubmission)
		})

		api.Get("/submissions/{id}", s.handleAPIGetSubmission)
	})

	r.NotFound(s.handleNotFound)
	return r
}

func (s *Server) buildStatus(ctx context.Context) model.DaemonStatus {
	version := strings.TrimSpace(s.cfg.Version)
	if version == "" {
		version = "dev"
	}

	st := model.DaemonStatus{
		Status:        "ok",
		Version:       version,
		UptimeSeconds: max(int64(time.Since(s.startedAt).Seconds()), 0),
		StartedAt:     s.startedAt,
		Addr:          s.effectiveAddr(),
		DataDir:       s.cfg.DataDir,
	}

	if s.store != nil {
		if dd := s.store.DataDir(); dd != "" {
			st.DataDir = dd
		}
		if arts, vers, subs, pCount, pBytes, err := s.store.Stats(ctx); err == nil {
			st.ArtifactCount = arts
			st.VersionCount = vers
			st.SubmissionCount = subs
			st.PreviewCacheCount = pCount
			st.PreviewCacheBytes = pBytes
		}
	}

	if s.previews != nil {
		avail, chromePath := s.previews.ChromeInfo()
		st.ChromeAvailable = avail
		st.ChromePath = chromePath
	}

	return st
}

func (s *Server) publicURL(r *http.Request, urlPath string) string {
	if r != nil && r.Host != "" {
		scheme := "http"
		if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
			scheme = "https"
		}
		return scheme + "://" + r.Host + urlPath
	}
	return urlPath
}

func parsePositiveInt(raw string) (int, error) {
	v, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid positive integer %q", raw)
	}
	return v, nil
}

func parsePositiveInt64(raw string) (int64, error) {
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid positive integer %q", raw)
	}
	return v, nil
}

func shortHash(h string) string {
	h = strings.TrimSpace(h)
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
