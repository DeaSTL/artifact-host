package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/deastl/artifact-host/internal/model"
	"github.com/deastl/artifact-host/internal/preview"
	"github.com/deastl/artifact-host/internal/store"
)

const (
	maxPushBodyBytes       = 256 << 20 // 256 MiB
	maxSubmissionBodyBytes = 64 << 20  // 64 MiB
	maxFormFieldBytes      = 1 << 20   // 1 MiB
	defaultLongPollSec     = 30
	minLongPollSec         = 1
	maxLongPollSec         = 120
)

// submissionHub broadcasts submission creation events per artifact slug to wake long-poll waiters.
type submissionHub struct {
	mu       sync.Mutex
	channels map[string]chan struct{}
}

func newSubmissionHub() *submissionHub {
	return &submissionHub{
		channels: make(map[string]chan struct{}),
	}
}

func (h *submissionHub) WaitChan(slug string) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	ch, ok := h.channels[slug]
	if !ok {
		ch = make(chan struct{})
		h.channels[slug] = ch
	}
	return ch
}

func (h *submissionHub) Notify(slug string) {
	h.mu.Lock()
	ch, ok := h.channels[slug]
	if ok {
		delete(h.channels, slug)
	}
	h.mu.Unlock()
	if ok {
		close(ch)
	}
}

func (s *Server) handleAPIStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildStatus(r.Context()))
}

func (s *Server) handleAPIListArtifacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	filter := model.ListFilter{
		Search: strings.TrimSpace(q.Get("q")),
		Tag:    strings.TrimSpace(q.Get("tag")),
	}
	if rawLimit := strings.TrimSpace(q.Get("limit")); rawLimit != "" {
		n, err := strconv.Atoi(rawLimit)
		if err != nil || n < 0 {
			writeAPIError(w, http.StatusBadRequest, "invalid limit parameter")
			return
		}
		filter.Limit = n
	}
	if rawOffset := strings.TrimSpace(q.Get("offset")); rawOffset != "" {
		n, err := strconv.Atoi(rawOffset)
		if err != nil || n < 0 {
			writeAPIError(w, http.StatusBadRequest, "invalid offset parameter")
			return
		}
		filter.Offset = n
	}

	artifacts, tags, err := s.store.ListArtifacts(r.Context(), filter)
	if err != nil {
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("list artifacts: %v", err))
		return
	}
	if artifacts == nil {
		artifacts = []model.Artifact{}
	}
	if tags == nil {
		tags = []string{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"artifacts": artifacts,
		"tags":      tags,
	})
}

func (s *Server) handleAPIPushVersion(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if err := model.ValidateSlug(slug); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxPushBodyBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("expected multipart/form-data: %v", err))
		return
	}

	in := store.PushInput{
		Slug: slug,
	}

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				writeAPIError(w, http.StatusRequestEntityTooLarge, "upload exceeds maximum size limit")
				return
			}
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("read multipart stream: %v", err))
			return
		}

		// Parse Content-Disposition directly so nested relative filenames (e.g. "assets/logo.png")
		// are preserved rather than stripped by multipart.Part.FileName().
		_, dispParams, dispErr := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if dispErr != nil {
			_ = part.Close()
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("invalid part Content-Disposition: %v", dispErr))
			return
		}
		fieldName := dispParams["name"]

		if fieldName == "file" {
			rawFilename := dispParams["filename"]
			if strings.TrimSpace(rawFilename) == "" {
				_ = part.Close()
				writeAPIError(w, http.StatusBadRequest, "file part is missing filename")
				return
			}
			cleanPath, pathErr := model.SanitizeAssetPath(rawFilename)
			if pathErr != nil {
				_ = part.Close()
				writeAPIError(w, http.StatusBadRequest, pathErr.Error())
				return
			}

			data, readErr := io.ReadAll(part)
			_ = part.Close()
			if readErr != nil {
				var maxErr *http.MaxBytesError
				if errors.As(readErr, &maxErr) {
					writeAPIError(w, http.StatusRequestEntityTooLarge, "upload exceeds maximum size limit")
					return
				}
				writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("read file %q: %v", cleanPath, readErr))
				return
			}

			// multipart.Writer.CreateFormFile sets generic application/octet-stream;
			// leave ContentType empty in that case so store detects MIME type from extension/content.
			partCT := strings.TrimSpace(part.Header.Get("Content-Type"))
			if strings.EqualFold(partCT, "application/octet-stream") {
				partCT = ""
			}

			in.Assets = append(in.Assets, store.AssetInput{
				Path:        cleanPath,
				ContentType: partCT,
				Data:        data,
			})
			continue
		}

		valBytes, readErr := io.ReadAll(io.LimitReader(part, maxFormFieldBytes+1))
		_ = part.Close()
		if readErr != nil {
			var maxErr *http.MaxBytesError
			if errors.As(readErr, &maxErr) {
				writeAPIError(w, http.StatusRequestEntityTooLarge, "upload exceeds maximum size limit")
				return
			}
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("read form field %q: %v", fieldName, readErr))
			return
		}
		if len(valBytes) > maxFormFieldBytes {
			writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("form field %q is too large", fieldName))
			return
		}
		val := string(valBytes)

		switch fieldName {
		case "title":
			in.Title = val
		case "description":
			in.Description = val
		case "icon":
			in.Icon = val
		case "note":
			in.Note = val
		case "entry":
			in.EntryFile = val
		case "tag":
			in.Tags = append(in.Tags, val)
			in.SetTags = true
		}
	}

	if len(in.Assets) == 0 {
		writeAPIError(w, http.StatusBadRequest, "at least one file part is required")
		return
	}

	art, ver, err := s.store.PushVersion(r.Context(), in)
	if err != nil {
		if isClientValidationErr(err) {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("push version: %v", err))
		return
	}

	if s.previews != nil && !s.cfg.NoPreviews {
		s.previews.Enqueue(art.Slug, ver.Number, ver.ContentHash, false)
	}

	writeJSON(w, http.StatusCreated, s.buildPushResponse(r, *art, *ver))
}

func (s *Server) handleAPIGetArtifact(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if err := model.ValidateSlug(slug); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	versionNum := 0
	if rawVer := strings.TrimSpace(r.URL.Query().Get("version")); rawVer != "" {
		v, err := parsePositiveInt(rawVer)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid version query parameter")
			return
		}
		versionNum = v
	}

	detail, err := s.store.GetArtifactDetail(r.Context(), slug, versionNum)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "artifact or version not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("get artifact: %v", err))
		return
	}
	if detail.Versions == nil {
		detail.Versions = []model.Version{}
	}
	if detail.Submissions == nil {
		detail.Submissions = []model.SubmissionMeta{}
	}

	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) handleAPIDeleteArtifact(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if err := model.ValidateSlug(slug); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	if err := s.store.DeleteArtifact(r.Context(), slug); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "artifact not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("delete artifact: %v", err))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPIListVersions(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if err := model.ValidateSlug(slug); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	versions, err := s.store.ListVersions(r.Context(), slug)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "artifact not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("list versions: %v", err))
		return
	}
	if versions == nil {
		versions = []model.Version{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"versions": versions})
}

func (s *Server) handleAPIGetVersion(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if err := model.ValidateSlug(slug); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	verNum, err := parsePositiveInt(chi.URLParam(r, "version"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid version number")
		return
	}

	ver, err := s.store.GetVersion(r.Context(), slug, verNum)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "version not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("get version: %v", err))
		return
	}

	writeJSON(w, http.StatusOK, ver)
}

func (s *Server) handleAPIDeleteVersion(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if err := model.ValidateSlug(slug); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	verNum, err := parsePositiveInt(chi.URLParam(r, "version"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid version number")
		return
	}

	if err := s.store.DeleteVersion(r.Context(), slug, verNum); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "version not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("delete version: %v", err))
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAPIRollback(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if err := model.ValidateSlug(slug); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	var req struct {
		Version int    `json:"version"`
		Note    string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxFormFieldBytes)).Decode(&req); err != nil {
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}
	if req.Version <= 0 {
		writeAPIError(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}

	s.executeRollback(w, r, slug, req.Version, req.Note)
}

func (s *Server) handleAPIRollbackVersion(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if err := model.ValidateSlug(slug); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	verNum, err := parsePositiveInt(chi.URLParam(r, "version"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid version number")
		return
	}

	var note string
	if r.Body != nil {
		body, readErr := io.ReadAll(io.LimitReader(r.Body, maxFormFieldBytes))
		if readErr == nil && len(bytes.TrimSpace(body)) > 0 {
			var req struct {
				Note string `json:"note"`
			}
			if err := json.Unmarshal(body, &req); err != nil {
				writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
				return
			}
			note = req.Note
		}
	}

	s.executeRollback(w, r, slug, verNum, note)
}

func (s *Server) executeRollback(w http.ResponseWriter, r *http.Request, slug string, targetVersion int, note string) {
	art, ver, err := s.store.RollbackToVersion(r.Context(), slug, targetVersion, note)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "artifact or target version not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("rollback version: %v", err))
		return
	}

	if s.previews != nil && !s.cfg.NoPreviews {
		s.previews.Enqueue(art.Slug, ver.Number, ver.ContentHash, false)
	}

	writeJSON(w, http.StatusCreated, s.buildPushResponse(r, *art, *ver))
}

func (s *Server) handleAPIRefreshPreview(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if err := model.ValidateSlug(slug); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	verNum, err := parsePositiveInt(chi.URLParam(r, "version"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid version number")
		return
	}

	ver, err := s.store.GetVersion(r.Context(), slug, verNum)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "version not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("get version: %v", err))
		return
	}

	previewPath := fmt.Sprintf("/previews/%s/v%d.png", url.PathEscape(slug), verNum)
	if s.previews == nil || s.cfg.NoPreviews {
		writeJSON(w, http.StatusOK, map[string]string{
			"status":      model.PreviewDisabled,
			"preview_url": previewPath,
		})
		return
	}

	if r.URL.Query().Get("async") == "1" {
		s.previews.Enqueue(slug, verNum, ver.ContentHash, true)
		status := model.PreviewPending
		if avail, _ := s.previews.ChromeInfo(); !avail {
			status = model.PreviewDisabled
		}
		writeJSON(w, http.StatusAccepted, map[string]string{
			"status":      status,
			"preview_url": previewPath,
		})
		return
	}

	_, renderErr := s.previews.RenderSync(r.Context(), slug, verNum, ver.ContentHash, true)
	status := model.PreviewReady
	if renderErr != nil {
		if errors.Is(renderErr, preview.ErrPreviewDisabled) || errors.Is(renderErr, preview.ErrChromeUnavailable) {
			status = model.PreviewDisabled
		} else {
			status = model.PreviewFailed
		}
	}

	resp := map[string]string{
		"status":      status,
		"preview_url": previewPath,
	}
	if renderErr != nil {
		resp["error"] = renderErr.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAPICreateSubmission(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if err := model.ValidateSlug(slug); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}
	verNum, err := parsePositiveInt(chi.URLParam(r, "version"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid version number")
		return
	}

	if _, err := s.store.GetVersion(r.Context(), slug, verNum); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "artifact or version not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("lookup version: %v", err))
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxSubmissionBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "submission exceeds maximum size limit")
			return
		}
		writeAPIError(w, http.StatusBadRequest, fmt.Sprintf("read submission body: %v", err))
		return
	}

	filename, contentType, payload, parseErr := parseSubmissionPayload(r, body)
	if parseErr != nil {
		writeAPIError(w, http.StatusBadRequest, parseErr.Error())
		return
	}

	sub, err := s.store.SaveSubmission(r.Context(), slug, verNum, filename, contentType, payload)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "artifact not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("save submission: %v", err))
		return
	}

	s.subsHub.Notify(slug)
	writeJSON(w, http.StatusCreated, sub.Meta())
}

func parseSubmissionPayload(r *http.Request, body []byte) (filename, contentType string, data []byte, err error) {
	rawCT := strings.TrimSpace(r.Header.Get("Content-Type"))
	mediaType := ""
	if rawCT != "" {
		if mt, _, parseErr := mime.ParseMediaType(rawCT); parseErr == nil {
			mediaType = strings.ToLower(mt)
		}
	}

	headerFilename := strings.TrimSpace(r.URL.Query().Get("filename"))
	if headerFilename == "" {
		headerFilename = strings.TrimSpace(r.Header.Get("X-Submission-Filename"))
	}

	if mediaType == "application/json" {
		trimmed := bytes.TrimSpace(body)
		if len(trimmed) == 0 {
			return "", "", nil, errors.New("empty JSON submission body")
		}

		var obj map[string]json.RawMessage
		if jsonErr := json.Unmarshal(trimmed, &obj); jsonErr == nil && obj != nil {
			if rawData, hasData := obj["data"]; hasData {
				if rawName, ok := obj["filename"]; ok {
					_ = json.Unmarshal(rawName, &filename)
				}
				if rawContentType, ok := obj["content_type"]; ok {
					_ = json.Unmarshal(rawContentType, &contentType)
				}

				dataTrimmed := bytes.TrimSpace(rawData)
				switch {
				case len(dataTrimmed) == 0 || bytes.Equal(dataTrimmed, []byte("null")):
					data = []byte{}
				case dataTrimmed[0] == '"':
					var str string
					if unmarshalErr := json.Unmarshal(dataTrimmed, &str); unmarshalErr != nil {
						return "", "", nil, fmt.Errorf("invalid data string: %w", unmarshalErr)
					}
					data = []byte(str)
				default:
					data = dataTrimmed
					if strings.TrimSpace(contentType) == "" {
						contentType = "application/json"
					}
				}

				filename = strings.TrimSpace(filename)
				if filename == "" {
					filename = headerFilename
				}
				if filename == "" {
					filename = "submission.json"
				}
				return filename, strings.TrimSpace(contentType), data, nil
			}
		}

		// Valid JSON value without an envelope object: store raw JSON bytes directly.
		if !json.Valid(trimmed) {
			return "", "", nil, errors.New("invalid JSON body")
		}
		filename = headerFilename
		if filename == "" {
			filename = "submission.json"
		}
		return filename, "application/json", body, nil
	}

	filename = headerFilename
	if filename == "" {
		if strings.HasPrefix(mediaType, "text/") {
			filename = "submission.txt"
		} else {
			filename = "submission.bin"
		}
	}
	return filename, rawCT, body, nil
}

func (s *Server) handleAPIListSubmissions(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	if err := model.ValidateSlug(slug); err != nil {
		writeAPIError(w, http.StatusBadRequest, err.Error())
		return
	}

	q := r.URL.Query()
	versionNum := 0
	if rawVer := strings.TrimSpace(q.Get("version")); rawVer != "" {
		v, err := strconv.Atoi(rawVer)
		if err != nil || v < 0 {
			writeAPIError(w, http.StatusBadRequest, "invalid version parameter")
			return
		}
		versionNum = v
	}

	var afterID int64
	if rawAfter := strings.TrimSpace(q.Get("after_id")); rawAfter != "" {
		id, err := strconv.ParseInt(rawAfter, 10, 64)
		if err != nil || id < 0 {
			writeAPIError(w, http.StatusBadRequest, "invalid after_id parameter")
			return
		}
		afterID = id
	}

	waitParam := strings.ToLower(strings.TrimSpace(q.Get("wait")))
	wait := waitParam == "1" || waitParam == "true"

	timeoutSec := defaultLongPollSec
	if rawTimeout := strings.TrimSpace(q.Get("timeout")); rawTimeout != "" {
		n, err := strconv.Atoi(rawTimeout)
		if err != nil {
			writeAPIError(w, http.StatusBadRequest, "invalid timeout parameter")
			return
		}
		timeoutSec = n
	}
	if timeoutSec < minLongPollSec {
		timeoutSec = minLongPollSec
	}
	if timeoutSec > maxLongPollSec {
		timeoutSec = maxLongPollSec
	}

	ctx := r.Context()
	subs, err := s.listSubmissionsAfter(ctx, slug, versionNum, afterID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "artifact not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("list submissions: %v", err))
		return
	}

	if !wait || len(subs) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{"submissions": subs})
		return
	}

	timer := time.NewTimer(time.Duration(timeoutSec) * time.Second)
	defer timer.Stop()

	for {
		// Subscribe before re-querying so a concurrent submission cannot be missed.
		notifyCh := s.subsHub.WaitChan(slug)

		subs, err = s.listSubmissionsAfter(ctx, slug, versionNum, afterID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeAPIError(w, http.StatusNotFound, "artifact not found")
				return
			}
			writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("list submissions: %v", err))
			return
		}
		if len(subs) > 0 {
			writeJSON(w, http.StatusOK, map[string]any{"submissions": subs})
			return
		}

		select {
		case <-notifyCh:
			continue
		case <-timer.C:
			writeJSON(w, http.StatusOK, map[string]any{"submissions": []model.SubmissionMeta{}})
			return
		case <-ctx.Done():
			return
		}
	}
}

func (s *Server) listSubmissionsAfter(ctx context.Context, slug string, versionNum int, afterID int64) ([]model.SubmissionMeta, error) {
	all, err := s.store.ListSubmissions(ctx, slug, versionNum)
	if err != nil {
		return nil, err
	}
	if afterID <= 0 {
		if all == nil {
			return []model.SubmissionMeta{}, nil
		}
		return all, nil
	}
	filtered := make([]model.SubmissionMeta, 0, len(all))
	for _, sub := range all {
		if sub.ID > afterID {
			filtered = append(filtered, sub)
		}
	}
	return filtered, nil
}

func (s *Server) handleAPIGetSubmission(w http.ResponseWriter, r *http.Request) {
	id, err := parsePositiveInt64(chi.URLParam(r, "id"))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "invalid submission id")
		return
	}

	routeSlug := chi.URLParam(r, "slug")
	if routeSlug != "" {
		if err := model.ValidateSlug(routeSlug); err != nil {
			writeAPIError(w, http.StatusBadRequest, err.Error())
			return
		}
	}

	sub, err := s.store.GetSubmission(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeAPIError(w, http.StatusNotFound, "submission not found")
			return
		}
		writeAPIError(w, http.StatusInternalServerError, fmt.Sprintf("get submission: %v", err))
		return
	}

	if routeSlug != "" && sub.Slug != routeSlug {
		writeAPIError(w, http.StatusNotFound, "submission not found")
		return
	}

	if r.URL.Query().Get("meta") == "1" {
		writeJSON(w, http.StatusOK, sub)
		return
	}

	ct := strings.TrimSpace(sub.ContentType)
	if ct == "" {
		ct = "application/octet-stream"
	}
	filename := sanitizeHeaderFilename(sub.Filename)
	if filename == "" {
		filename = fmt.Sprintf("submission-%d", sub.ID)
	}

	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Submission-Filename", filename)
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.Header().Set("Content-Length", strconv.Itoa(len(sub.Data)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(sub.Data)
	}
}

func (s *Server) buildPushResponse(r *http.Request, art model.Artifact, ver model.Version) model.PushResponse {
	escapedSlug := url.PathEscape(art.Slug)
	return model.PushResponse{
		Artifact:   art,
		Version:    ver,
		URL:        s.publicURL(r, fmt.Sprintf("/a/%s", escapedSlug)),
		RawURL:     s.publicURL(r, fmt.Sprintf("/raw/%s/v/%d/", escapedSlug, ver.Number)),
		PreviewURL: s.publicURL(r, fmt.Sprintf("/previews/%s/v%d.png", escapedSlug, ver.Number)),
	}
}

func isClientValidationErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "invalid slug") ||
		strings.Contains(msg, "slug ") ||
		strings.Contains(msg, "invalid entry file") ||
		strings.Contains(msg, "at least one asset") ||
		strings.Contains(msg, "invalid asset path") ||
		strings.Contains(msg, "duplicate asset path") ||
		strings.Contains(msg, "is missing from assets")
}

func sanitizeHeaderFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	var b strings.Builder
	b.Grow(len(name))
	for i := 0; i < len(name); i++ {
		c := name[i]
		if c < 0x20 || c == 0x7f || c == '"' || c == '\\' || c == '/' {
			continue
		}
		b.WriteByte(c)
	}
	return strings.TrimSpace(b.String())
}
