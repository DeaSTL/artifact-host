package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/deastl/artifact-host/internal/model"
)

// DefaultAddr is the default base URL for the local artifact-host daemon.
const DefaultAddr = "http://127.0.0.1:7337"

const (
	defaultHTTPTimeout = 30 * time.Second
	maxLongPollTimeout = 30 * time.Second
)

// APIError represents an HTTP error response returned by the artifact-host daemon.
type APIError struct {
	StatusCode int
	Message    string
	Body       string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("api error (%d): %s", e.StatusCode, e.Message)
	}
	return fmt.Sprintf("api error (%d): %s", e.StatusCode, http.StatusText(e.StatusCode))
}

// IsNotFound reports whether err is a 404 APIError.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

// Client communicates with the artifact-host daemon HTTP API.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NormalizeAddr normalizes an address or URL into a canonical HTTP(S) base URL
// without a trailing slash.
func NormalizeAddr(addr string) string {
	addr = strings.TrimSpace(addr)
	if addr == "" {
		return DefaultAddr
	}
	if strings.HasPrefix(addr, ":") {
		addr = "http://127.0.0.1" + addr
	} else if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	return strings.TrimRight(addr, "/")
}

// New creates a new Client targeting baseURL.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: NormalizeAddr(baseURL),
		HTTPClient: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
	}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: defaultHTTPTimeout}
}

func (c *Client) longPollHTTPClient() *http.Client {
	base := c.httpClient()
	cloned := *base
	cloned.Timeout = 0
	return &cloned
}

// ArtifactURL returns the web viewer URL for an artifact (and optional version).
func (c *Client) ArtifactURL(slug string, version int) string {
	if version > 0 {
		return fmt.Sprintf("%s/a/%s/v/%d", c.BaseURL, url.PathEscape(slug), version)
	}
	return fmt.Sprintf("%s/a/%s", c.BaseURL, url.PathEscape(slug))
}

// RawAssetURL returns the raw download URL for an asset inside a specific artifact version.
func (c *Client) RawAssetURL(slug string, version int, assetPath string) string {
	return fmt.Sprintf("%s/raw/%s/v/%d/%s", c.BaseURL, url.PathEscape(slug), version, escapeAssetPath(assetPath))
}

func escapeAssetPath(p string) string {
	parts := strings.Split(p, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return strings.Join(parts, "/")
}

// ListResponse is returned by Client.List.
type ListResponse struct {
	Artifacts []model.Artifact `json:"artifacts"`
	Tags      []string         `json:"tags"`
}

// PushFile represents a single asset to upload as part of a Push request.
// If SourcePath is non-empty, its contents are streamed from disk; otherwise Data is used.
type PushFile struct {
	Path       string
	SourcePath string
	Data       []byte
}

// PushRequest describes metadata and files for pushing a new artifact version.
type PushRequest struct {
	Title       string
	Description string
	Icon        string
	Note        string
	Entry       string
	Tags        []string
	Files       []PushFile
}

// PullResult summarizes the assets downloaded by Client.Pull.
type PullResult struct {
	Slug       string   `json:"slug"`
	Version    int      `json:"version"`
	OutDir     string   `json:"out_dir"`
	Files      []string `json:"files"`
	TotalBytes int64    `json:"total_bytes"`
}

// PreviewRefreshResponse is returned by Client.RefreshPreview.
type PreviewRefreshResponse struct {
	Status     string `json:"status"`
	PreviewURL string `json:"preview_url"`
}

// SubmissionsOptions configures filtering and long-polling for Client.Submissions.
type SubmissionsOptions struct {
	Version int
	AfterID int64
	Wait    bool
	Timeout time.Duration
}

// SubmissionDownload holds the raw payload and headers for a downloaded submission.
type SubmissionDownload struct {
	ID          int64
	Filename    string
	ContentType string
	Data        []byte
}

// Status fetches daemon diagnostics from GET /api/v1/status.
func (c *Client) Status(ctx context.Context) (*model.DaemonStatus, error) {
	var out model.DaemonStatus
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/status", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// List fetches artifacts matching filter from GET /api/v1/artifacts.
func (c *Client) List(ctx context.Context, filter model.ListFilter) (*ListResponse, error) {
	q := url.Values{}
	if filter.Search != "" {
		q.Set("q", filter.Search)
	}
	if filter.Tag != "" {
		q.Set("tag", filter.Tag)
	}
	if filter.Limit > 0 {
		q.Set("limit", strconv.Itoa(filter.Limit))
	}
	if filter.Offset > 0 {
		q.Set("offset", strconv.Itoa(filter.Offset))
	}
	endpoint := "/api/v1/artifacts"
	if encoded := q.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}
	var out ListResponse
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Push uploads a new version of an artifact via multipart POST /api/v1/artifacts/{slug}/versions.
func (c *Client) Push(ctx context.Context, slug string, req PushRequest) (*model.PushResponse, error) {
	if err := model.ValidateSlug(slug); err != nil {
		return nil, err
	}
	if len(req.Files) == 0 {
		return nil, errors.New("at least one file is required to push")
	}

	cleanedFiles := make([]PushFile, len(req.Files))
	for i, f := range req.Files {
		cleanPath, err := model.SanitizeAssetPath(f.Path)
		if err != nil {
			return nil, fmt.Errorf("invalid file path %q: %w", f.Path, err)
		}
		f.Path = cleanPath
		cleanedFiles[i] = f
	}

	entry := req.Entry
	if entry == "" {
		entry = "index.html"
	}
	cleanEntry, err := model.SanitizeAssetPath(entry)
	if err != nil {
		return nil, fmt.Errorf("invalid entry path %q: %w", entry, err)
	}

	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	contentType := mw.FormDataContentType()

	go func() {
		writeErr := func() error {
			writeField := func(name, val string) error {
				if val == "" {
					return nil
				}
				return mw.WriteField(name, val)
			}
			if err := writeField("title", req.Title); err != nil {
				return err
			}
			if err := writeField("description", req.Description); err != nil {
				return err
			}
			if err := writeField("icon", req.Icon); err != nil {
				return err
			}
			if err := writeField("note", req.Note); err != nil {
				return err
			}
			if err := mw.WriteField("entry", cleanEntry); err != nil {
				return err
			}
			for _, tag := range req.Tags {
				if strings.TrimSpace(tag) == "" {
					continue
				}
				if err := mw.WriteField("tag", tag); err != nil {
					return err
				}
			}
			for _, f := range cleanedFiles {
				part, err := mw.CreateFormFile("file", f.Path)
				if err != nil {
					return err
				}
				if f.SourcePath != "" {
					file, err := os.Open(f.SourcePath)
					if err != nil {
						return fmt.Errorf("open %s: %w", f.SourcePath, err)
					}
					_, copyErr := io.Copy(part, file)
					closeErr := file.Close()
					if copyErr != nil {
						return fmt.Errorf("read %s: %w", f.SourcePath, copyErr)
					}
					if closeErr != nil {
						return fmt.Errorf("close %s: %w", f.SourcePath, closeErr)
					}
				} else {
					if _, err := part.Write(f.Data); err != nil {
						return err
					}
				}
			}
			return mw.Close()
		}()
		_ = pw.CloseWithError(writeErr)
	}()

	endpoint := fmt.Sprintf("/api/v1/artifacts/%s/versions", url.PathEscape(slug))
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+endpoint, pr)
	if err != nil {
		_ = pr.Close()
		return nil, err
	}
	httpReq.Header.Set("Content-Type", contentType)

	resp, err := c.httpClient().Do(httpReq)
	if err != nil {
		_ = pr.Close()
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}

	var out model.PushResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode push response: %w", err)
	}
	return &out, nil
}

// Info fetches detailed artifact metadata from GET /api/v1/artifacts/{slug}.
func (c *Client) Info(ctx context.Context, slug string, version int) (*model.ArtifactDetail, error) {
	if err := model.ValidateSlug(slug); err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("/api/v1/artifacts/%s", url.PathEscape(slug))
	if version > 0 {
		endpoint += "?version=" + strconv.Itoa(version)
	}
	var out model.ArtifactDetail
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Versions fetches all versions for an artifact from GET /api/v1/artifacts/{slug}/versions.
func (c *Client) Versions(ctx context.Context, slug string) ([]model.Version, error) {
	if err := model.ValidateSlug(slug); err != nil {
		return nil, err
	}
	endpoint := fmt.Sprintf("/api/v1/artifacts/%s/versions", url.PathEscape(slug))
	var out struct {
		Versions []model.Version `json:"versions"`
	}
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}
	return out.Versions, nil
}

// Rollback creates a new version rolled back to target version via POST /api/v1/artifacts/{slug}/rollback.
func (c *Client) Rollback(ctx context.Context, slug string, version int, note string) (*model.PushResponse, error) {
	if err := model.ValidateSlug(slug); err != nil {
		return nil, err
	}
	if version <= 0 {
		return nil, errors.New("version must be a positive integer")
	}
	endpoint := fmt.Sprintf("/api/v1/artifacts/%s/rollback", url.PathEscape(slug))
	payload := struct {
		Version int    `json:"version"`
		Note    string `json:"note,omitempty"`
	}{
		Version: version,
		Note:    note,
	}
	var out model.PushResponse
	if err := c.doJSON(ctx, http.MethodPost, endpoint, payload, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Delete removes an artifact and all its versions via DELETE /api/v1/artifacts/{slug}.
func (c *Client) Delete(ctx context.Context, slug string) error {
	if err := model.ValidateSlug(slug); err != nil {
		return err
	}
	endpoint := fmt.Sprintf("/api/v1/artifacts/%s", url.PathEscape(slug))
	return c.doJSON(ctx, http.MethodDelete, endpoint, nil, nil)
}

// DeleteVersion removes a specific version via DELETE /api/v1/artifacts/{slug}/versions/{version}.
func (c *Client) DeleteVersion(ctx context.Context, slug string, version int) error {
	if err := model.ValidateSlug(slug); err != nil {
		return err
	}
	if version <= 0 {
		return errors.New("version must be a positive integer")
	}
	endpoint := fmt.Sprintf("/api/v1/artifacts/%s/versions/%d", url.PathEscape(slug), version)
	return c.doJSON(ctx, http.MethodDelete, endpoint, nil, nil)
}

// Pull downloads every asset of the specified artifact version (or effective version if version == 0)
// into outDir. If outDir is empty, it defaults to "./<slug>-v<N>".
func (c *Client) Pull(ctx context.Context, slug string, version int, outDir string) (*PullResult, error) {
	detail, err := c.Info(ctx, slug, version)
	if err != nil {
		return nil, err
	}

	target, err := resolveVersion(detail, version)
	if err != nil {
		return nil, err
	}
	if len(target.Assets) == 0 {
		return nil, fmt.Errorf("artifact %q version %d has no downloadable assets", slug, target.Number)
	}

	if outDir == "" {
		outDir = fmt.Sprintf("./%s-v%d", slug, target.Number)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("create output directory %s: %w", outDir, err)
	}
	absOutDir, err := filepath.Abs(outDir)
	if err != nil {
		return nil, fmt.Errorf("resolve output directory %s: %w", outDir, err)
	}

	result := &PullResult{
		Slug:    slug,
		Version: target.Number,
		OutDir:  outDir,
		Files:   make([]string, 0, len(target.Assets)),
	}

	for _, asset := range target.Assets {
		cleanPath, err := model.SanitizeAssetPath(asset.Path)
		if err != nil {
			return nil, fmt.Errorf("unsafe asset path %q: %w", asset.Path, err)
		}
		destPath := filepath.Join(absOutDir, filepath.FromSlash(cleanPath))
		rel, err := filepath.Rel(absOutDir, destPath)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return nil, fmt.Errorf("asset path %q escapes output directory", asset.Path)
		}

		rawURL := c.RawAssetURL(slug, target.Number, cleanPath)
		n, err := c.downloadToFile(ctx, rawURL, destPath)
		if err != nil {
			return nil, fmt.Errorf("download %s: %w", cleanPath, err)
		}
		result.Files = append(result.Files, cleanPath)
		result.TotalBytes += n
	}

	return result, nil
}

func resolveVersion(detail *model.ArtifactDetail, requested int) (*model.Version, error) {
	if detail == nil {
		return nil, errors.New("artifact detail is empty")
	}
	if detail.CurrentVersion != nil && (requested == 0 || detail.CurrentVersion.Number == requested) {
		return detail.CurrentVersion, nil
	}
	targetNum := requested
	if targetNum == 0 {
		targetNum = detail.Artifact.EffectiveVersion()
	}
	for i := range detail.Versions {
		if detail.Versions[i].Number == targetNum {
			return &detail.Versions[i], nil
		}
	}
	if targetNum == 0 {
		return nil, fmt.Errorf("artifact %q has no versions", detail.Artifact.Slug)
	}
	return nil, fmt.Errorf("artifact %q version %d not found", detail.Artifact.Slug, targetNum)
}

func (c *Client) downloadToFile(ctx context.Context, rawURL, destPath string) (int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return 0, err
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return 0, err
	}
	f, err := os.Create(destPath)
	if err != nil {
		return 0, err
	}
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		_ = os.Remove(destPath)
		return 0, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(destPath)
		return 0, closeErr
	}
	return n, nil
}

// RefreshPreview triggers preview regeneration via POST /api/v1/artifacts/{slug}/versions/{version}/preview/refresh.
func (c *Client) RefreshPreview(ctx context.Context, slug string, version int) (*PreviewRefreshResponse, error) {
	if err := model.ValidateSlug(slug); err != nil {
		return nil, err
	}
	if version <= 0 {
		return nil, errors.New("version must be a positive integer")
	}
	endpoint := fmt.Sprintf("/api/v1/artifacts/%s/versions/%d/preview/refresh", url.PathEscape(slug), version)
	var out PreviewRefreshResponse
	if err := c.doJSON(ctx, http.MethodPost, endpoint, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Submissions lists submissions for an artifact via GET /api/v1/artifacts/{slug}/submissions.
func (c *Client) Submissions(ctx context.Context, slug string, opts SubmissionsOptions) ([]model.SubmissionMeta, error) {
	if err := model.ValidateSlug(slug); err != nil {
		return nil, err
	}
	q := url.Values{}
	if opts.Version > 0 {
		q.Set("version", strconv.Itoa(opts.Version))
	}
	if opts.AfterID > 0 {
		q.Set("after_id", strconv.FormatInt(opts.AfterID, 10))
	}
	if opts.Wait {
		q.Set("wait", "1")
		secs := int(opts.Timeout.Seconds())
		if secs <= 0 {
			secs = 30
		}
		q.Set("timeout", strconv.Itoa(secs))
	}

	endpoint := fmt.Sprintf("/api/v1/artifacts/%s/submissions", url.PathEscape(slug))
	if encoded := q.Encode(); encoded != "" {
		endpoint += "?" + encoded
	}

	httpClient := c.httpClient()
	if opts.Wait {
		httpClient = c.longPollHTTPClient()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}
	var out struct {
		Submissions []model.SubmissionMeta `json:"submissions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode submissions response: %w", err)
	}
	return out.Submissions, nil
}

// WaitSubmission long-polls GET /api/v1/artifacts/{slug}/submissions until at least one
// submission with ID > afterID arrives, or timeout elapses, or ctx is canceled.
func (c *Client) WaitSubmission(ctx context.Context, slug string, version int, afterID int64, timeout time.Duration) ([]model.SubmissionMeta, error) {
	if err := model.ValidateSlug(slug); err != nil {
		return nil, err
	}

	waitCtx := ctx
	var cancel context.CancelFunc
	if timeout > 0 {
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	for {
		if err := waitCtx.Err(); err != nil {
			return nil, err
		}

		pollTimeout := maxLongPollTimeout
		if deadline, ok := waitCtx.Deadline(); ok {
			remaining := time.Until(deadline)
			if remaining <= 0 {
				return nil, context.DeadlineExceeded
			}
			if remaining < pollTimeout {
				pollTimeout = remaining
				if pollTimeout < time.Second {
					pollTimeout = time.Second
				}
			}
		}

		subs, err := c.Submissions(waitCtx, slug, SubmissionsOptions{
			Version: version,
			AfterID: afterID,
			Wait:    true,
			Timeout: pollTimeout,
		})
		if err != nil {
			if waitCtx.Err() != nil {
				return nil, waitCtx.Err()
			}
			return nil, err
		}
		if len(subs) > 0 {
			return subs, nil
		}
	}
}

// GetSubmission downloads raw submission bytes from GET /api/v1/submissions/{id}.
func (c *Client) GetSubmission(ctx context.Context, id int64) (*SubmissionDownload, error) {
	if id <= 0 {
		return nil, errors.New("submission id must be a positive integer")
	}
	endpoint := fmt.Sprintf("/api/v1/submissions/%d", id)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read submission %d: %w", id, err)
	}
	return &SubmissionDownload{
		ID:          id,
		Filename:    resp.Header.Get("X-Submission-Filename"),
		ContentType: resp.Header.Get("Content-Type"),
		Data:        data,
	}, nil
}

// GetSubmissionMeta fetches full JSON metadata (and data) from GET /api/v1/submissions/{id}?meta=1.
func (c *Client) GetSubmissionMeta(ctx context.Context, id int64) (*model.Submission, error) {
	if id <= 0 {
		return nil, errors.New("submission id must be a positive integer")
	}
	endpoint := fmt.Sprintf("/api/v1/submissions/%d?meta=1", id)
	var out model.Submission
	if err := c.doJSON(ctx, http.MethodGet, endpoint, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) doJSON(ctx context.Context, method, endpoint string, body any, out any) error {
	var bodyReader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request body: %w", err)
		}
		bodyReader = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+endpoint, bodyReader)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if err := checkResponse(resp); err != nil {
		return err
	}

	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response from %s %s: %w", method, endpoint, err)
	}
	return nil
}

func checkResponse(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	bodyStr := strings.TrimSpace(string(bodyBytes))

	var errPayload struct {
		Error string `json:"error"`
	}
	msg := ""
	if err := json.Unmarshal(bodyBytes, &errPayload); err == nil && errPayload.Error != "" {
		msg = errPayload.Error
	} else if bodyStr != "" {
		msg = bodyStr
	} else {
		msg = http.StatusText(resp.StatusCode)
	}
	return &APIError{
		StatusCode: resp.StatusCode,
		Message:    msg,
		Body:       bodyStr,
	}
}
