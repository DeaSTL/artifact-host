package model

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
)

const (
	PreviewPending  = "pending"
	PreviewReady    = "ready"
	PreviewFailed   = "failed"
	PreviewDisabled = "disabled"
)

// Artifact represents a hosted artifact and its aggregate metadata.
type Artifact struct {
	ID                  int64     `json:"id"`
	Slug                string    `json:"slug"`
	Title               string    `json:"title"`
	Description         string    `json:"description"`
	Icon                string    `json:"icon"`
	Tags                []string  `json:"tags"`
	LatestVersion       int       `json:"latest_version"`
	ActiveVersion       int       `json:"active_version"`
	VersionCount        int       `json:"version_count"`
	SubmissionCount     int       `json:"submission_count"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
	LatestPreviewStatus string    `json:"latest_preview_status"`
	LatestContentHash   string    `json:"latest_content_hash"`
}

// EffectiveVersion returns ActiveVersion if explicitly pinned (> 0), otherwise LatestVersion.
func (a Artifact) EffectiveVersion() int {
	if a.ActiveVersion > 0 {
		return a.ActiveVersion
	}
	return a.LatestVersion
}

// Version represents a specific immutable snapshot of an artifact.
type Version struct {
	ID               int64      `json:"id"`
	ArtifactID       int64      `json:"artifact_id"`
	Slug             string     `json:"slug"`
	Number           int        `json:"number"`
	Title            string     `json:"title"`
	Description      string     `json:"description"`
	Note             string     `json:"note"`
	EntryFile        string     `json:"entry_file"`
	ContentHash      string     `json:"content_hash"`
	TotalSize        int64      `json:"total_size"`
	FileCount        int        `json:"file_count"`
	PreviewStatus    string     `json:"preview_status"`
	PreviewError     string     `json:"preview_error,omitempty"`
	PreviewHash      string     `json:"preview_hash,omitempty"`
	PreviewUpdatedAt *time.Time `json:"preview_updated_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
	Assets           []Asset    `json:"assets,omitempty"`
}

// Asset represents a single file stored inside an artifact version.
type Asset struct {
	Path        string `json:"path"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
}

// Submission represents a payload submitted back from an interactive artifact.
type Submission struct {
	ID          int64     `json:"id"`
	ArtifactID  int64     `json:"artifact_id"`
	Slug        string    `json:"slug"`
	Version     int       `json:"version"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	Data        []byte    `json:"data,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Meta returns the submission metadata without its raw payload.
func (s Submission) Meta() SubmissionMeta {
	return SubmissionMeta{
		ID:          s.ID,
		Slug:        s.Slug,
		Version:     s.Version,
		Filename:    s.Filename,
		ContentType: s.ContentType,
		Size:        s.Size,
		CreatedAt:   s.CreatedAt,
	}
}

// SubmissionMeta is the lightweight DTO for listing submissions without raw payloads.
type SubmissionMeta struct {
	ID          int64     `json:"id"`
	Slug        string    `json:"slug"`
	Version     int       `json:"version"`
	Filename    string    `json:"filename"`
	ContentType string    `json:"content_type"`
	Size        int64     `json:"size"`
	CreatedAt   time.Time `json:"created_at"`
}

// PushResponse is returned after pushing a new artifact version.
type PushResponse struct {
	Artifact   Artifact `json:"artifact"`
	Version    Version  `json:"version"`
	URL        string   `json:"url"`
	RawURL     string   `json:"raw_url"`
	PreviewURL string   `json:"preview_url"`
}

// ArtifactDetail aggregates an artifact with its versions and submission metadata.
type ArtifactDetail struct {
	Artifact       Artifact         `json:"artifact"`
	Versions       []Version        `json:"versions"`
	CurrentVersion *Version         `json:"current_version,omitempty"`
	Submissions    []SubmissionMeta `json:"submissions"`
}

// DaemonStatus reports runtime and storage diagnostics for the hosting daemon.
type DaemonStatus struct {
	Status            string    `json:"status"`
	Version           string    `json:"version"`
	UptimeSeconds     int64     `json:"uptime_seconds"`
	StartedAt         time.Time `json:"started_at"`
	Addr              string    `json:"addr"`
	DataDir           string    `json:"data_dir"`
	ArtifactCount     int       `json:"artifact_count"`
	VersionCount      int       `json:"version_count"`
	SubmissionCount   int       `json:"submission_count"`
	PreviewCacheCount int       `json:"preview_cache_count"`
	PreviewCacheBytes int64     `json:"preview_cache_bytes"`
	ChromeAvailable   bool      `json:"chrome_available"`
	ChromePath        string    `json:"chrome_path,omitempty"`
}

// ListFilter specifies filtering and pagination for listing artifacts.
type ListFilter struct {
	Search string `json:"search,omitempty"`
	Tag    string `json:"tag,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	Offset int    `json:"offset,omitempty"`
}

// ValidateSlug checks that slug contains only lowercase ASCII alphanumeric characters,
// dashes, underscores, or dots, is 1 to 120 characters long, and contains no path traversal or slashes.
func ValidateSlug(slug string) error {
	if len(slug) == 0 {
		return errors.New("slug must not be empty")
	}
	if len(slug) > 120 {
		return fmt.Errorf("slug length %d exceeds maximum of 120 characters", len(slug))
	}
	if slug == "." || strings.Contains(slug, "..") {
		return fmt.Errorf("invalid slug %q: must not be '.' or contain '..'", slug)
	}
	for i := 0; i < len(slug); i++ {
		c := slug[i]
		isLower := c >= 'a' && c <= 'z'
		isDigit := c >= '0' && c <= '9'
		isPunct := c == '-' || c == '_' || c == '.'
		if !isLower && !isDigit && !isPunct {
			return fmt.Errorf("invalid slug %q: character %q is not allowed (use lowercase alphanumeric, '-', '_', '.')", slug, c)
		}
	}
	return nil
}

// SanitizeAssetPath validates and cleans a relative asset path, rejecting absolute paths,
// backslashes, NUL bytes, and any directory traversal segments.
func SanitizeAssetPath(p string) (string, error) {
	if p == "" {
		return "", errors.New("asset path must not be empty")
	}
	if strings.ContainsRune(p, '\x00') {
		return "", errors.New("asset path must not contain NUL bytes")
	}
	if strings.ContainsRune(p, '\\') {
		return "", fmt.Errorf("invalid asset path %q: backslashes are not allowed", p)
	}
	if strings.HasPrefix(p, "/") {
		return "", fmt.Errorf("invalid asset path %q: must be relative", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return "", fmt.Errorf("invalid asset path %q: path traversal is not allowed", p)
		}
	}
	cleaned := path.Clean(p)
	if cleaned == "" || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "/") || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("invalid asset path %q", p)
	}
	return cleaned, nil
}
