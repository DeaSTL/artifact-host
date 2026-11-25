package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a requested artifact, version, asset, or submission does not exist.
var ErrNotFound = errors.New("not found")

// AssetInput describes a single file to store in an artifact version.
type AssetInput struct {
	Path        string
	ContentType string
	Data        []byte
}

// PushInput specifies the metadata and files for pushing a new artifact version.
type PushInput struct {
	Slug        string
	Title       string
	Description string
	Icon        string
	Note        string
	EntryFile   string
	Tags        []string
	SetTags     bool
	Assets      []AssetInput
}

// Store manages persistent SQLite metadata and on-disk artifact files and previews.
type Store struct {
	dataDir string
	db      *sql.DB
	writeMu sync.Mutex
}

// timeFormat is fixed-width UTC RFC3339Nano so TEXT columns sort monotonically in SQLite.
const timeFormat = "2006-01-02T15:04:05.000000000Z"

const schemaSQL = `
CREATE TABLE IF NOT EXISTS artifacts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	slug TEXT NOT NULL UNIQUE,
	title TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	icon TEXT NOT NULL DEFAULT '',
	tags_json TEXT NOT NULL DEFAULT '[]',
	latest_version INTEGER NOT NULL DEFAULT 0,
	active_version INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS versions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	artifact_id INTEGER NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
	number INTEGER NOT NULL,
	title TEXT NOT NULL DEFAULT '',
	description TEXT NOT NULL DEFAULT '',
	note TEXT NOT NULL DEFAULT '',
	entry_file TEXT NOT NULL DEFAULT 'index.html',
	content_hash TEXT NOT NULL,
	total_size INTEGER NOT NULL DEFAULT 0,
	file_count INTEGER NOT NULL DEFAULT 0,
	preview_status TEXT NOT NULL DEFAULT 'pending',
	preview_error TEXT NOT NULL DEFAULT '',
	preview_hash TEXT NOT NULL DEFAULT '',
	preview_updated_at TEXT,
	created_at TEXT NOT NULL,
	UNIQUE(artifact_id, number)
);

CREATE TABLE IF NOT EXISTS assets (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	version_id INTEGER NOT NULL REFERENCES versions(id) ON DELETE CASCADE,
	path TEXT NOT NULL,
	size INTEGER NOT NULL,
	content_type TEXT NOT NULL,
	sha256 TEXT NOT NULL,
	UNIQUE(version_id, path)
);

CREATE TABLE IF NOT EXISTS submissions (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	artifact_id INTEGER NOT NULL REFERENCES artifacts(id) ON DELETE CASCADE,
	version_number INTEGER NOT NULL,
	filename TEXT NOT NULL DEFAULT '',
	content_type TEXT NOT NULL DEFAULT 'application/octet-stream',
	size INTEGER NOT NULL,
	data BLOB NOT NULL,
	created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_artifacts_updated_at ON artifacts(updated_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_versions_artifact_number ON versions(artifact_id, number DESC);
CREATE INDEX IF NOT EXISTS idx_assets_version_id ON assets(version_id);
CREATE INDEX IF NOT EXISTS idx_submissions_artifact_created ON submissions(artifact_id, created_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_submissions_artifact_version ON submissions(artifact_id, version_number, created_at DESC, id DESC);
`

// Open initializes the data directory layout, opens the SQLite database with WAL,
// busy_timeout=5000, and foreign_keys=ON, and runs automatic schema migrations.
func Open(dataDir string) (*Store, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("dataDir must not be empty")
	}

	absDir, err := filepath.Abs(dataDir)
	if err != nil {
		return nil, fmt.Errorf("resolve dataDir: %w", err)
	}

	for _, dir := range []string{
		absDir,
		filepath.Join(absDir, "artifacts"),
		filepath.Join(absDir, "previews"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create directory %s: %w", dir, err)
		}
	}

	dbPath := filepath.Join(absDir, "artifact-host.db")
	dsnURL := url.URL{
		Scheme:   "file",
		Path:     filepath.ToSlash(dbPath),
		RawQuery: "_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)",
	}

	db, err := sql.Open("sqlite", dsnURL.String())
	if err != nil {
		return nil, fmt.Errorf("open sqlite database: %w", err)
	}

	// SQLite handles concurrent readers across processes under WAL; serializing
	// connections within a single Store prevents transient SQLITE_BUSY write locks.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA busy_timeout = 5000;",
		"PRAGMA foreign_keys = ON;",
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			_ = db.Close()
			return nil, fmt.Errorf("execute %s: %w", p, err)
		}
	}

	if _, err := db.Exec(schemaSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate schema: %w", err)
	}

	return &Store{
		dataDir: absDir,
		db:      db,
	}, nil
}

// Close closes the underlying SQLite database.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// DataDir returns the absolute root data directory path.
func (s *Store) DataDir() string {
	return s.dataDir
}

// Stats returns counts of artifacts, versions, submissions, and cached preview PNG files/bytes.
func (s *Store) Stats(ctx context.Context) (artifacts, versions, submissions, previewCount int, previewBytes int64, err error) {
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM artifacts`).Scan(&artifacts); err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("count artifacts: %w", err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM versions`).Scan(&versions); err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("count versions: %w", err)
	}
	if err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM submissions`).Scan(&submissions); err != nil {
		return 0, 0, 0, 0, 0, fmt.Errorf("count submissions: %w", err)
	}

	previewsDir := filepath.Join(s.dataDir, "previews")
	entries, readErr := os.ReadDir(previewsDir)
	if readErr != nil {
		if errors.Is(readErr, os.ErrNotExist) {
			return artifacts, versions, submissions, 0, 0, nil
		}
		return 0, 0, 0, 0, 0, fmt.Errorf("read previews directory: %w", readErr)
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if !strings.HasSuffix(strings.ToLower(entry.Name()), ".png") {
			continue
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			if errors.Is(infoErr, os.ErrNotExist) {
				continue
			}
			return 0, 0, 0, 0, 0, fmt.Errorf("stat preview %s: %w", entry.Name(), infoErr)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		previewCount++
		previewBytes += info.Size()
	}

	return artifacts, versions, submissions, previewCount, previewBytes, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeFormat)
}

func parseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parse timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

func normalizeTags(tags []string) []string {
	if len(tags) == 0 {
		return []string{}
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, raw := range tags {
		tag := strings.TrimSpace(raw)
		if tag == "" {
			continue
		}
		if _, ok := seen[tag]; ok {
			continue
		}
		seen[tag] = struct{}{}
		out = append(out, tag)
	}
	return out
}

func marshalTags(tags []string) (string, error) {
	norm := normalizeTags(tags)
	b, err := json.Marshal(norm)
	if err != nil {
		return "", fmt.Errorf("marshal tags: %w", err)
	}
	return string(b), nil
}

func unmarshalTags(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{}, nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return nil, fmt.Errorf("unmarshal tags: %w", err)
	}
	return normalizeTags(tags), nil
}

func detectContentType(filename string, data []byte) string {
	ext := strings.ToLower(path.Ext(filename))
	if ext != "" {
		if ct := mime.TypeByExtension(ext); ct != "" {
			return ct
		}
	}
	if len(data) > 0 {
		return http.DetectContentType(data)
	}
	return "application/octet-stream"
}

func artifactDir(dataDir, slug string) string {
	return filepath.Join(dataDir, "artifacts", slug)
}

func versionDir(dataDir, slug string, versionNum int) string {
	return filepath.Join(dataDir, "artifacts", slug, fmt.Sprintf("v%d", versionNum))
}

func sanitizeHashPrefix(contentHash string) string {
	var b strings.Builder
	b.Grow(12)
	for i := 0; i < len(contentHash) && b.Len() < 12; i++ {
		c := contentHash[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') || (c >= 'g' && c <= 'z') || (c >= 'G' && c <= 'Z')
		if isHex {
			b.WriteByte(c)
		}
	}
	if b.Len() == 0 {
		return "none"
	}
	return strings.ToLower(b.String())
}

func sanitizePreviewSlug(slug string) string {
	var b strings.Builder
	b.Grow(len(slug))
	for i := 0; i < len(slug); i++ {
		c := slug[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == '.' {
			b.WriteByte(c)
		}
	}
	out := b.String()
	out = strings.ReplaceAll(out, "..", "")
	out = strings.Trim(out, ".")
	if out == "" {
		return "artifact"
	}
	return out
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// removeArtifactPreviews removes preview PNG files belonging to slug (and optionally a specific version).
// If versionNum <= 0, removes previews for all versions of slug.
func (s *Store) removeArtifactPreviews(slug string, versionNum int) error {
	previewsDir := filepath.Join(s.dataDir, "previews")
	entries, err := os.ReadDir(previewsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	safeSlug := sanitizePreviewSlug(slug)
	var prefix string
	if versionNum > 0 {
		prefix = fmt.Sprintf("%s-v%d-", safeSlug, versionNum)
	} else {
		prefix = safeSlug + "-v"
	}

	var firstErr error
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".png") || !strings.HasPrefix(name, prefix) {
			continue
		}

		trimmed := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".png")
		if versionNum > 0 {
			// Match <slug>-v<N>-<hash>.png where <hash> contains no '-'.
			if trimmed == "" || strings.ContainsRune(trimmed, '-') {
				continue
			}
		} else {
			// Match <slug>-v<digits>-<hash>.png where <hash> contains no '-'.
			dashIdx := strings.IndexByte(trimmed, '-')
			if dashIdx <= 0 || dashIdx == len(trimmed)-1 {
				continue
			}
			verPart := trimmed[:dashIdx]
			hashPart := trimmed[dashIdx+1:]
			if !isDigits(verPart) || strings.ContainsRune(hashPart, '-') {
				continue
			}
		}

		if err := os.Remove(filepath.Join(previewsDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
