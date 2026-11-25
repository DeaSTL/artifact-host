package server

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"strconv"
	"strings"

	"github.com/a-h/templ"
	"github.com/go-chi/chi/v5"

	"github.com/deastl/artifact-host/internal/model"
	"github.com/deastl/artifact-host/internal/preview"
	"github.com/deastl/artifact-host/internal/store"
	"github.com/deastl/artifact-host/internal/ui"
	"github.com/deastl/artifact-host/internal/ui/views"
)

func (s *Server) handleBridgeJS(w http.ResponseWriter, r *http.Request) {
	data, err := fs.ReadFile(ui.StaticFS, "static/bridge.js")
	if err != nil {
		http.Error(w, "bridge script unavailable", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}

func (s *Server) handleGallery(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	activeTag := strings.TrimSpace(r.URL.Query().Get("tag"))

	artifacts, allTags, err := s.store.ListArtifacts(ctx, model.ListFilter{
		Search: query,
		Tag:    activeTag,
	})
	if err != nil {
		s.renderErrorPage(w, r, views.ErrorData{
			StatusCode: http.StatusInternalServerError,
			Title:      "Failed to load artifacts",
			Message:    "Could not read the artifact catalog from storage.",
			Detail:     err.Error(),
		})
		return
	}

	status := s.buildStatus(ctx)
	data := views.GalleryData{
		Artifacts:  artifacts,
		AllTags:    allTags,
		Query:      query,
		ActiveTag:  activeTag,
		Status:     status,
		TotalCount: status.ArtifactCount,
	}
	s.renderComponent(w, r, http.StatusOK, views.Gallery(data))
}

func (s *Server) handleViewer(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	s.renderViewer(w, r, slug, 0)
}

func (s *Server) handleViewerVersion(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	rawVer := chi.URLParam(r, "version")
	verNum, err := parsePositiveInt(rawVer)
	if err != nil {
		s.renderErrorPage(w, r, views.ErrorData{
			StatusCode: http.StatusNotFound,
			Title:      "Version not found",
			Message:    "The requested artifact version identifier is invalid.",
			Detail:     fmt.Sprintf("slug=%s version=%s", slug, rawVer),
		})
		return
	}
	s.renderViewer(w, r, slug, verNum)
}

func (s *Server) renderViewer(w http.ResponseWriter, r *http.Request, slug string, versionNum int) {
	if err := model.ValidateSlug(slug); err != nil {
		s.renderErrorPage(w, r, views.ErrorData{
			StatusCode: http.StatusNotFound,
			Title:      "Artifact not found",
			Message:    "The requested artifact slug is invalid.",
			Detail:     slug,
		})
		return
	}

	ctx := r.Context()
	detail, err := s.store.GetArtifactDetail(ctx, slug, versionNum)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			detailMsg := slug
			if versionNum > 0 {
				detailMsg = fmt.Sprintf("%s v%d", slug, versionNum)
			}
			s.renderErrorPage(w, r, views.ErrorData{
				StatusCode: http.StatusNotFound,
				Title:      "Artifact not found",
				Message:    "The artifact or version you requested does not exist or may have been removed.",
				Detail:     detailMsg,
			})
			return
		}
		s.renderErrorPage(w, r, views.ErrorData{
			StatusCode: http.StatusInternalServerError,
			Title:      "Failed to load artifact",
			Message:    "An error occurred while loading artifact metadata.",
			Detail:     err.Error(),
		})
		return
	}

	if detail.CurrentVersion == nil {
		s.renderErrorPage(w, r, views.ErrorData{
			StatusCode: http.StatusNotFound,
			Title:      "Version not found",
			Message:    "This artifact does not have any published versions.",
			Detail:     slug,
		})
		return
	}

	data := views.ViewerData{
		Artifact:    detail.Artifact,
		Current:     *detail.CurrentVersion,
		Versions:    detail.Versions,
		Submissions: detail.Submissions,
		Status:      s.buildStatus(ctx),
	}
	s.renderComponent(w, r, http.StatusOK, views.Viewer(data))
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	verFile := chi.URLParam(r, "verFile")

	if err := model.ValidateSlug(slug); err != nil {
		http.NotFound(w, r)
		return
	}
	verNum, ok := parsePreviewVersionFile(verFile)
	if !ok {
		http.NotFound(w, r)
		return
	}

	ver, err := s.store.GetVersion(r.Context(), slug, verNum)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "failed to load version", http.StatusInternalServerError)
		return
	}

	if s.previews != nil {
		if cachedPath, hit := s.previews.GetCached(slug, verNum, ver.ContentHash); hit {
			etag := fmt.Sprintf("\"%s-v%d-%s\"", slug, verNum, shortHash(ver.ContentHash))
			w.Header().Set("Content-Type", "image/png")
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			w.Header().Set("ETag", etag)
			if ifNoneMatchMatches(r.Header.Get("If-None-Match"), etag) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			http.ServeFile(w, r, cachedPath)
			return
		}
	}

	title := strings.TrimSpace(ver.Title)
	if title == "" {
		title = slug
	}

	status := ver.PreviewStatus
	if s.previews == nil || s.cfg.NoPreviews {
		status = model.PreviewDisabled
	} else {
		s.previews.Enqueue(slug, verNum, ver.ContentHash, false)
		avail, _ := s.previews.ChromeInfo()
		if !avail {
			status = model.PreviewDisabled
		} else if status != model.PreviewFailed {
			status = model.PreviewPending
		}
	}

	svg := preview.PlaceholderSVG(title, status)
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(svg)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(svg)
	}
}

func (s *Server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/") {
		writeAPIError(w, http.StatusNotFound, "not found")
		return
	}
	s.renderErrorPage(w, r, views.ErrorData{
		StatusCode: http.StatusNotFound,
		Title:      "Page not found",
		Message:    "The page or resource you requested does not exist.",
		Detail:     r.URL.Path,
	})
}

func (s *Server) renderComponent(w http.ResponseWriter, r *http.Request, statusCode int, c templ.Component) {
	var buf bytes.Buffer
	if err := c.Render(r.Context(), &buf); err != nil {
		http.Error(w, "failed to render page", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(statusCode)
	if r.Method != http.MethodHead {
		_, _ = w.Write(buf.Bytes())
	}
}

func (s *Server) renderErrorPage(w http.ResponseWriter, r *http.Request, data views.ErrorData) {
	if data.StatusCode == 0 {
		data.StatusCode = http.StatusInternalServerError
	}
	s.renderComponent(w, r, data.StatusCode, views.ErrorPage(data))
}

func parsePreviewVersionFile(verFile string) (int, bool) {
	if !strings.HasSuffix(verFile, ".png") {
		return 0, false
	}
	stem := strings.TrimSuffix(verFile, ".png")
	stem = strings.TrimPrefix(stem, "v")
	if stem == "" {
		return 0, false
	}
	for i := 0; i < len(stem); i++ {
		if stem[i] < '0' || stem[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(stem)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func ifNoneMatchMatches(headerVal, etag string) bool {
	if strings.TrimSpace(headerVal) == "" {
		return false
	}
	for _, candidate := range strings.Split(headerVal, ",") {
		c := strings.TrimSpace(candidate)
		if c == "*" || c == etag || strings.TrimPrefix(c, "W/") == etag {
			return true
		}
	}
	return false
}
