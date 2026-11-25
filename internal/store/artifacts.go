package store

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/deastl/artifact-host/internal/model"
)

type preparedAsset struct {
	Path        string
	ContentType string
	Data        []byte
	Size        int64
	SHA256      string
}

const artifactSelectQuery = `
SELECT
	a.id,
	a.slug,
	a.title,
	a.description,
	a.icon,
	a.tags_json,
	a.latest_version,
	a.active_version,
	(SELECT COUNT(*) FROM versions v WHERE v.artifact_id = a.id) AS version_count,
	(SELECT COUNT(*) FROM submissions s WHERE s.artifact_id = a.id) AS submission_count,
	a.created_at,
	a.updated_at,
	COALESCE((
		SELECT v.preview_status
		FROM versions v
		WHERE v.artifact_id = a.id
		  AND v.number = CASE WHEN a.active_version > 0 THEN a.active_version ELSE a.latest_version END
	), '') AS latest_preview_status,
	COALESCE((
		SELECT v.content_hash
		FROM versions v
		WHERE v.artifact_id = a.id
		  AND v.number = CASE WHEN a.active_version > 0 THEN a.active_version ELSE a.latest_version END
	), '') AS latest_content_hash
FROM artifacts a
`

// PushVersion validates, writes files atomically, and records a new immutable artifact version.
func (s *Store) PushVersion(ctx context.Context, in PushInput) (*model.Artifact, *model.Version, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	return s.pushVersionLocked(ctx, in)
}

func (s *Store) pushVersionLocked(ctx context.Context, in PushInput) (*model.Artifact, *model.Version, error) {
	if err := model.ValidateSlug(in.Slug); err != nil {
		return nil, nil, err
	}

	entryFile := strings.TrimSpace(in.EntryFile)
	if entryFile == "" {
		entryFile = "index.html"
	}
	cleanEntry, err := model.SanitizeAssetPath(entryFile)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid entry file: %w", err)
	}

	if len(in.Assets) == 0 {
		return nil, nil, errors.New("at least one asset is required")
	}

	seenPaths := make(map[string]struct{}, len(in.Assets))
	prepared := make([]preparedAsset, 0, len(in.Assets))
	hasEntry := false
	var totalSize int64

	for _, rawAsset := range in.Assets {
		cleanPath, err := model.SanitizeAssetPath(rawAsset.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid asset path %q: %w", rawAsset.Path, err)
		}
		if _, dup := seenPaths[cleanPath]; dup {
			return nil, nil, fmt.Errorf("duplicate asset path %q", cleanPath)
		}
		seenPaths[cleanPath] = struct{}{}
		if cleanPath == cleanEntry {
			hasEntry = true
		}

		ct := strings.TrimSpace(rawAsset.ContentType)
		if ct == "" {
			ct = detectContentType(cleanPath, rawAsset.Data)
		}

		sum := sha256.Sum256(rawAsset.Data)
		size := int64(len(rawAsset.Data))
		totalSize += size

		prepared = append(prepared, preparedAsset{
			Path:        cleanPath,
			ContentType: ct,
			Data:        rawAsset.Data,
			Size:        size,
			SHA256:      fmt.Sprintf("%x", sum),
		})
	}

	if !hasEntry {
		return nil, nil, fmt.Errorf("entry file %q is missing from assets", cleanEntry)
	}

	slices.SortFunc(prepared, func(a, b preparedAsset) int {
		return cmp.Compare(a.Path, b.Path)
	})

	composite := sha256.New()
	for _, a := range prepared {
		_, _ = fmt.Fprintf(composite, "%s\x00%s\n", a.Path, a.SHA256)
	}
	contentHash := fmt.Sprintf("%x", composite.Sum(nil))

	now := time.Now().UTC()
	nowStr := formatTime(now)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var (
		artifactID int64
		title      string
		desc       string
		icon       string
		tags       []string
		createdAt  time.Time
		exists     bool
	)

	var existingTagsJSON, existingCreatedStr string
	err = tx.QueryRowContext(ctx, `
		SELECT id, title, description, icon, tags_json, created_at
		FROM artifacts
		WHERE slug = ?
	`, in.Slug).Scan(&artifactID, &title, &desc, &icon, &existingTagsJSON, &existingCreatedStr)
	if err == nil {
		exists = true
		if tags, err = unmarshalTags(existingTagsJSON); err != nil {
			return nil, nil, err
		}
		if createdAt, err = parseTime(existingCreatedStr); err != nil {
			return nil, nil, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, fmt.Errorf("lookup artifact %q: %w", in.Slug, err)
	}

	var nextVersion int
	if exists {
		if in.Title != "" {
			title = in.Title
		}
		if in.Description != "" {
			desc = in.Description
		}
		if in.Icon != "" {
			icon = in.Icon
		}
		if in.SetTags || len(in.Tags) > 0 {
			tags = normalizeTags(in.Tags)
		}

		var maxVer int
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(number), 0) FROM versions WHERE artifact_id = ?`, artifactID).Scan(&maxVer); err != nil {
			return nil, nil, fmt.Errorf("query max version: %w", err)
		}
		nextVersion = maxVer + 1
	} else {
		title = in.Title
		desc = in.Description
		icon = in.Icon
		tags = normalizeTags(in.Tags)
		createdAt = now
		nextVersion = 1
	}

	tagsJSON, err := marshalTags(tags)
	if err != nil {
		return nil, nil, err
	}

	finalDir, err := s.writeVersionFilesAtomic(in.Slug, nextVersion, prepared)
	if err != nil {
		return nil, nil, err
	}
	cleanupDir := true
	defer func() {
		if cleanupDir {
			_ = os.RemoveAll(finalDir)
		}
	}()

	if exists {
		_, err = tx.ExecContext(ctx, `
			UPDATE artifacts
			SET title = ?, description = ?, icon = ?, tags_json = ?,
				latest_version = ?, active_version = 0, updated_at = ?
			WHERE id = ?
		`, title, desc, icon, tagsJSON, nextVersion, nowStr, artifactID)
		if err != nil {
			return nil, nil, fmt.Errorf("update artifact: %w", err)
		}
	} else {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO artifacts (
				slug, title, description, icon, tags_json,
				latest_version, active_version, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?)
		`, in.Slug, title, desc, icon, tagsJSON, nextVersion, nowStr, nowStr)
		if err != nil {
			return nil, nil, fmt.Errorf("insert artifact: %w", err)
		}
		artifactID, err = res.LastInsertId()
		if err != nil {
			return nil, nil, fmt.Errorf("artifact insert id: %w", err)
		}
	}

	verRes, err := tx.ExecContext(ctx, `
		INSERT INTO versions (
			artifact_id, number, title, description, note, entry_file,
			content_hash, total_size, file_count, preview_status, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, artifactID, nextVersion, title, desc, in.Note, cleanEntry,
		contentHash, totalSize, len(prepared), model.PreviewPending, nowStr)
	if err != nil {
		return nil, nil, fmt.Errorf("insert version: %w", err)
	}
	versionID, err := verRes.LastInsertId()
	if err != nil {
		return nil, nil, fmt.Errorf("version insert id: %w", err)
	}

	modelAssets := make([]model.Asset, 0, len(prepared))
	for _, a := range prepared {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO assets (version_id, path, size, content_type, sha256)
			VALUES (?, ?, ?, ?, ?)
		`, versionID, a.Path, a.Size, a.ContentType, a.SHA256); err != nil {
			return nil, nil, fmt.Errorf("insert asset %q: %w", a.Path, err)
		}
		modelAssets = append(modelAssets, model.Asset{
			Path:        a.Path,
			Size:        a.Size,
			ContentType: a.ContentType,
			SHA256:      a.SHA256,
		})
	}

	var versionCount, submissionCount int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM versions WHERE artifact_id = ?`, artifactID).Scan(&versionCount); err != nil {
		return nil, nil, fmt.Errorf("count versions: %w", err)
	}
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM submissions WHERE artifact_id = ?`, artifactID).Scan(&submissionCount); err != nil {
		return nil, nil, fmt.Errorf("count submissions: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("commit push transaction: %w", err)
	}
	cleanupDir = false

	art := &model.Artifact{
		ID:                  artifactID,
		Slug:                in.Slug,
		Title:               title,
		Description:         desc,
		Icon:                icon,
		Tags:                tags,
		LatestVersion:       nextVersion,
		ActiveVersion:       0,
		VersionCount:        versionCount,
		SubmissionCount:     submissionCount,
		CreatedAt:           createdAt,
		UpdatedAt:           now,
		LatestPreviewStatus: model.PreviewPending,
		LatestContentHash:   contentHash,
	}

	ver := &model.Version{
		ID:            versionID,
		ArtifactID:    artifactID,
		Slug:          in.Slug,
		Number:        nextVersion,
		Title:         title,
		Description:   desc,
		Note:          in.Note,
		EntryFile:     cleanEntry,
		ContentHash:   contentHash,
		TotalSize:     totalSize,
		FileCount:     len(prepared),
		PreviewStatus: model.PreviewPending,
		CreatedAt:     now,
		Assets:        modelAssets,
	}

	return art, ver, nil
}

func (s *Store) writeVersionFilesAtomic(slug string, versionNum int, assets []preparedAsset) (string, error) {
	slugDir := artifactDir(s.dataDir, slug)
	if err := os.MkdirAll(slugDir, 0o755); err != nil {
		return "", fmt.Errorf("create artifact directory: %w", err)
	}

	finalDir := versionDir(s.dataDir, slug, versionNum)
	if _, err := os.Stat(finalDir); err == nil {
		if err := os.RemoveAll(finalDir); err != nil {
			return "", fmt.Errorf("clean stale version directory: %w", err)
		}
	}

	tmpDir, err := os.MkdirTemp(slugDir, fmt.Sprintf(".tmp-v%d-*", versionNum))
	if err != nil {
		return "", fmt.Errorf("create staging directory: %w", err)
	}
	if err := os.Chmod(tmpDir, 0o755); err != nil {
		_ = os.RemoveAll(tmpDir)
		return "", fmt.Errorf("chmod staging directory: %w", err)
	}

	succeeded := false
	defer func() {
		if !succeeded {
			_ = os.RemoveAll(tmpDir)
		}
	}()

	for _, a := range assets {
		dst := filepath.Join(tmpDir, filepath.FromSlash(a.Path))
		if !isPathStrictlyInside(tmpDir, dst) {
			return "", fmt.Errorf("asset path %q escapes staging directory", a.Path)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return "", fmt.Errorf("create parent directory for %q: %w", a.Path, err)
		}
		if err := os.WriteFile(dst, a.Data, 0o644); err != nil {
			return "", fmt.Errorf("write asset %q: %w", a.Path, err)
		}
	}

	if err := os.Rename(tmpDir, finalDir); err != nil {
		return "", fmt.Errorf("commit version directory: %w", err)
	}
	succeeded = true
	return finalDir, nil
}

// ListArtifacts returns artifacts ordered by updated_at DESC filtered by Search and Tag,
// along with a sorted list of all distinct tags across all stored artifacts.
func (s *Store) ListArtifacts(ctx context.Context, f model.ListFilter) ([]model.Artifact, []string, error) {
	rows, err := s.db.QueryContext(ctx, artifactSelectQuery+` ORDER BY a.updated_at DESC, a.id DESC`)
	if err != nil {
		return nil, nil, fmt.Errorf("query artifacts: %w", err)
	}
	defer rows.Close()

	needle := strings.ToLower(strings.TrimSpace(f.Search))
	tagFilter := strings.TrimSpace(f.Tag)

	tagSet := make(map[string]struct{})
	matched := make([]model.Artifact, 0)

	for rows.Next() {
		art, err := scanArtifact(rows)
		if err != nil {
			return nil, nil, err
		}
		for _, t := range art.Tags {
			tagSet[t] = struct{}{}
		}

		if tagFilter != "" && !slices.Contains(art.Tags, tagFilter) {
			continue
		}
		if needle != "" {
			inSlug := strings.Contains(strings.ToLower(art.Slug), needle)
			inTitle := strings.Contains(strings.ToLower(art.Title), needle)
			inDesc := strings.Contains(strings.ToLower(art.Description), needle)
			if !inSlug && !inTitle && !inDesc {
				continue
			}
		}

		matched = append(matched, art)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate artifacts: %w", err)
	}

	allTags := make([]string, 0, len(tagSet))
	for t := range tagSet {
		allTags = append(allTags, t)
	}
	slices.Sort(allTags)

	if f.Offset > 0 {
		if f.Offset >= len(matched) {
			matched = []model.Artifact{}
		} else {
			matched = matched[f.Offset:]
		}
	}
	if f.Limit > 0 && f.Limit < len(matched) {
		matched = matched[:f.Limit]
	}

	return matched, allTags, nil
}

// GetArtifact fetches a single artifact by slug with aggregate counts and latest metadata.
func (s *Store) GetArtifact(ctx context.Context, slug string) (*model.Artifact, error) {
	row := s.db.QueryRowContext(ctx, artifactSelectQuery+` WHERE a.slug = ?`, slug)
	art, err := scanArtifact(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &art, nil
}

// GetArtifactDetail returns the artifact, all its versions (DESC), the selected CurrentVersion
// (with Assets), and submission metadata (DESC). If versionNum <= 0, art.EffectiveVersion() is used.
func (s *Store) GetArtifactDetail(ctx context.Context, slug string, versionNum int) (*model.ArtifactDetail, error) {
	art, err := s.GetArtifact(ctx, slug)
	if err != nil {
		return nil, err
	}

	targetNum := versionNum
	if targetNum <= 0 {
		targetNum = art.EffectiveVersion()
	}

	versions, err := s.ListVersions(ctx, slug)
	if err != nil {
		return nil, err
	}

	var currentVer *model.Version
	if targetNum > 0 {
		v, err := s.GetVersion(ctx, slug, targetNum)
		if err != nil {
			return nil, err
		}
		currentVer = v
	}

	subs, err := s.ListSubmissions(ctx, slug, 0)
	if err != nil {
		return nil, err
	}

	return &model.ArtifactDetail{
		Artifact:       *art,
		Versions:       versions,
		CurrentVersion: currentVer,
		Submissions:    subs,
	}, nil
}

// GetVersion returns a specific artifact version along with its sorted Assets.
func (s *Store) GetVersion(ctx context.Context, slug string, versionNum int) (*model.Version, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT
			v.id, v.artifact_id, a.slug, v.number, v.title, v.description, v.note,
			v.entry_file, v.content_hash, v.total_size, v.file_count,
			v.preview_status, v.preview_error, v.preview_hash, v.preview_updated_at, v.created_at
		FROM versions v
		JOIN artifacts a ON a.id = v.artifact_id
		WHERE a.slug = ? AND v.number = ?
	`, slug, versionNum)

	ver, err := scanVersion(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	assets, err := s.listAssetsByVersionID(ctx, ver.ID)
	if err != nil {
		return nil, err
	}
	ver.Assets = assets
	return &ver, nil
}

// ListVersions returns all versions for slug ordered by version number DESC.
func (s *Store) ListVersions(ctx context.Context, slug string) ([]model.Version, error) {
	var artifactID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM artifacts WHERE slug = ?`, slug).Scan(&artifactID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("lookup artifact %q: %w", slug, err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT
			v.id, v.artifact_id, a.slug, v.number, v.title, v.description, v.note,
			v.entry_file, v.content_hash, v.total_size, v.file_count,
			v.preview_status, v.preview_error, v.preview_hash, v.preview_updated_at, v.created_at
		FROM versions v
		JOIN artifacts a ON a.id = v.artifact_id
		WHERE v.artifact_id = ?
		ORDER BY v.number DESC
	`, artifactID)
	if err != nil {
		return nil, fmt.Errorf("query versions: %w", err)
	}
	defer rows.Close()

	out := make([]model.Version, 0)
	for rows.Next() {
		v, err := scanVersion(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate versions: %w", err)
	}
	return out, nil
}

// RollbackToVersion copies all files from targetVersion into a new version v(latest+1),
// preserving append-only history.
func (s *Store) RollbackToVersion(ctx context.Context, slug string, targetVersion int, note string) (*model.Artifact, *model.Version, error) {
	if err := model.ValidateSlug(slug); err != nil {
		return nil, nil, err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	target, err := s.GetVersion(ctx, slug, targetVersion)
	if err != nil {
		return nil, nil, err
	}

	srcDir := versionDir(s.dataDir, slug, targetVersion)
	assets := make([]AssetInput, 0, len(target.Assets))
	for _, a := range target.Assets {
		cleanPath, err := model.SanitizeAssetPath(a.Path)
		if err != nil {
			return nil, nil, fmt.Errorf("sanitize stored asset path %q: %w", a.Path, err)
		}
		srcPath := filepath.Join(srcDir, filepath.FromSlash(cleanPath))
		if !isPathStrictlyInside(srcDir, srcPath) {
			return nil, nil, fmt.Errorf("stored asset path %q escapes version directory", a.Path)
		}
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return nil, nil, fmt.Errorf("read asset %q from v%d: %w", a.Path, targetVersion, err)
		}
		assets = append(assets, AssetInput{
			Path:        cleanPath,
			ContentType: a.ContentType,
			Data:        data,
		})
	}

	resolvedNote := strings.TrimSpace(note)
	if resolvedNote == "" {
		resolvedNote = fmt.Sprintf("Restored from v%d", targetVersion)
	}

	return s.pushVersionLocked(ctx, PushInput{
		Slug:        slug,
		Title:       target.Title,
		Description: target.Description,
		Note:        resolvedNote,
		EntryFile:   target.EntryFile,
		Assets:      assets,
	})
}

// DeleteArtifact removes the artifact database record (cascading versions, assets, submissions),
// its on-disk files, and any cached preview PNGs.
func (s *Store) DeleteArtifact(ctx context.Context, slug string) error {
	if err := model.ValidateSlug(slug); err != nil {
		return err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	res, err := s.db.ExecContext(ctx, `DELETE FROM artifacts WHERE slug = ?`, slug)
	if err != nil {
		return fmt.Errorf("delete artifact %q: %w", slug, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check deleted artifact rows: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}

	if err := os.RemoveAll(artifactDir(s.dataDir, slug)); err != nil {
		return fmt.Errorf("remove artifact files for %q: %w", slug, err)
	}
	if err := s.removeArtifactPreviews(slug, 0); err != nil {
		return fmt.Errorf("remove previews for %q: %w", slug, err)
	}
	return nil
}

// DeleteVersion removes a single version row, its directory, and its cached preview.
// If it is the last remaining version, the artifact itself is deleted as well; otherwise
// latest_version is recalculated and active_version is cleared if it pointed to versionNum.
func (s *Store) DeleteVersion(ctx context.Context, slug string, versionNum int) error {
	if err := model.ValidateSlug(slug); err != nil {
		return err
	}
	if versionNum <= 0 {
		return ErrNotFound
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin delete version tx: %w", err)
	}
	defer func() {
		_ = tx.Rollback()
	}()

	var artifactID int64
	var activeVersion int
	if err := tx.QueryRowContext(ctx, `SELECT id, active_version FROM artifacts WHERE slug = ?`, slug).Scan(&artifactID, &activeVersion); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		return fmt.Errorf("lookup artifact %q: %w", slug, err)
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM versions WHERE artifact_id = ? AND number = ?`, artifactID, versionNum)
	if err != nil {
		return fmt.Errorf("delete version v%d: %w", versionNum, err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check deleted version rows: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}

	var remainingMax int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(number), 0) FROM versions WHERE artifact_id = ?`, artifactID).Scan(&remainingMax); err != nil {
		return fmt.Errorf("query remaining max version: %w", err)
	}

	deletedWholeArtifact := false
	if remainingMax == 0 {
		deletedWholeArtifact = true
		if _, err := tx.ExecContext(ctx, `DELETE FROM artifacts WHERE id = ?`, artifactID); err != nil {
			return fmt.Errorf("delete empty artifact %q: %w", slug, err)
		}
	} else {
		newActive := activeVersion
		if newActive == versionNum {
			newActive = 0
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE artifacts
			SET latest_version = ?, active_version = ?, updated_at = ?
			WHERE id = ?
		`, remainingMax, newActive, formatTime(time.Now().UTC()), artifactID); err != nil {
			return fmt.Errorf("update artifact after version delete: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit delete version tx: %w", err)
	}

	if deletedWholeArtifact {
		if err := os.RemoveAll(artifactDir(s.dataDir, slug)); err != nil {
			return fmt.Errorf("remove artifact directory: %w", err)
		}
		if err := s.removeArtifactPreviews(slug, 0); err != nil {
			return fmt.Errorf("remove artifact previews: %w", err)
		}
		return nil
	}

	if err := os.RemoveAll(versionDir(s.dataDir, slug, versionNum)); err != nil {
		return fmt.Errorf("remove version directory: %w", err)
	}
	if err := s.removeArtifactPreviews(slug, versionNum); err != nil {
		return fmt.Errorf("remove version previews: %w", err)
	}
	return nil
}

// ResolveAssetFile sanitizes relPath, verifies the file is strictly inside the version directory,
// and returns the absolute file path and its content type.
func (s *Store) ResolveAssetFile(slug string, versionNum int, relPath string) (string, string, error) {
	if err := model.ValidateSlug(slug); err != nil {
		return "", "", err
	}
	if versionNum <= 0 {
		return "", "", ErrNotFound
	}

	cleanRel, err := model.SanitizeAssetPath(relPath)
	if err != nil {
		return "", "", err
	}

	vDir := versionDir(s.dataDir, slug, versionNum)
	absPath := filepath.Join(vDir, filepath.FromSlash(cleanRel))
	if !isPathStrictlyInside(vDir, absPath) {
		return "", "", fmt.Errorf("asset path %q escapes version directory", relPath)
	}

	info, err := os.Stat(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", ErrNotFound
		}
		return "", "", fmt.Errorf("stat asset %q: %w", cleanRel, err)
	}
	if info.IsDir() {
		return "", "", ErrNotFound
	}

	realVDir, err := filepath.EvalSymlinks(vDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", ErrNotFound
		}
		return "", "", fmt.Errorf("resolve version directory: %w", err)
	}
	realPath, err := filepath.EvalSymlinks(absPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", "", ErrNotFound
		}
		return "", "", fmt.Errorf("resolve asset path: %w", err)
	}
	if !isPathStrictlyInside(realVDir, realPath) {
		return "", "", fmt.Errorf("asset path %q escapes version directory via symlink", relPath)
	}

	var contentType string
	err = s.db.QueryRow(`
		SELECT ast.content_type
		FROM assets ast
		JOIN versions v ON v.id = ast.version_id
		JOIN artifacts art ON art.id = v.artifact_id
		WHERE art.slug = ? AND v.number = ? AND ast.path = ?
	`, slug, versionNum, cleanRel).Scan(&contentType)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", "", fmt.Errorf("query asset content type: %w", err)
	}

	if strings.TrimSpace(contentType) == "" {
		var sample []byte
		if f, openErr := os.Open(absPath); openErr == nil {
			buf := make([]byte, 512)
			n, _ := io.ReadFull(f, buf)
			_ = f.Close()
			sample = buf[:n]
		}
		contentType = detectContentType(cleanRel, sample)
	}

	return absPath, contentType, nil
}

// UpdatePreviewStatus updates the preview generation state for a specific artifact version.
func (s *Store) UpdatePreviewStatus(ctx context.Context, slug string, versionNum int, status, previewHash, errMsg string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE versions
		SET preview_status = ?, preview_hash = ?, preview_error = ?, preview_updated_at = ?
		WHERE number = ? AND artifact_id = (SELECT id FROM artifacts WHERE slug = ?)
	`, status, previewHash, errMsg, formatTime(time.Now().UTC()), versionNum, slug)
	if err != nil {
		return fmt.Errorf("update preview status: %w", err)
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("check updated preview rows: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

// PreviewFilePath returns a deterministic safe PNG path under <dataDir>/previews/.
func (s *Store) PreviewFilePath(slug string, versionNum int, contentHash string) string {
	safeSlug := sanitizePreviewSlug(slug)
	shortHash := sanitizeHashPrefix(contentHash)
	if versionNum < 0 {
		versionNum = 0
	}
	filename := fmt.Sprintf("%s-v%d-%s.png", safeSlug, versionNum, shortHash)
	return filepath.Join(s.dataDir, "previews", filename)
}

func (s *Store) listAssetsByVersionID(ctx context.Context, versionID int64) ([]model.Asset, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT path, size, content_type, sha256
		FROM assets
		WHERE version_id = ?
		ORDER BY path ASC
	`, versionID)
	if err != nil {
		return nil, fmt.Errorf("query assets: %w", err)
	}
	defer rows.Close()

	out := make([]model.Asset, 0)
	for rows.Next() {
		var a model.Asset
		if err := rows.Scan(&a.Path, &a.Size, &a.ContentType, &a.SHA256); err != nil {
			return nil, fmt.Errorf("scan asset: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate assets: %w", err)
	}
	return out, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanArtifact(sc rowScanner) (model.Artifact, error) {
	var (
		art        model.Artifact
		tagsJSON   string
		createdStr string
		updatedStr string
	)
	err := sc.Scan(
		&art.ID,
		&art.Slug,
		&art.Title,
		&art.Description,
		&art.Icon,
		&tagsJSON,
		&art.LatestVersion,
		&art.ActiveVersion,
		&art.VersionCount,
		&art.SubmissionCount,
		&createdStr,
		&updatedStr,
		&art.LatestPreviewStatus,
		&art.LatestContentHash,
	)
	if err != nil {
		return model.Artifact{}, err
	}
	tags, err := unmarshalTags(tagsJSON)
	if err != nil {
		return model.Artifact{}, err
	}
	createdAt, err := parseTime(createdStr)
	if err != nil {
		return model.Artifact{}, err
	}
	updatedAt, err := parseTime(updatedStr)
	if err != nil {
		return model.Artifact{}, err
	}
	art.Tags = tags
	art.CreatedAt = createdAt
	art.UpdatedAt = updatedAt
	return art, nil
}

func scanVersion(sc rowScanner) (model.Version, error) {
	var (
		v                model.Version
		previewUpdatedAt sql.NullString
		createdStr       string
	)
	err := sc.Scan(
		&v.ID,
		&v.ArtifactID,
		&v.Slug,
		&v.Number,
		&v.Title,
		&v.Description,
		&v.Note,
		&v.EntryFile,
		&v.ContentHash,
		&v.TotalSize,
		&v.FileCount,
		&v.PreviewStatus,
		&v.PreviewError,
		&v.PreviewHash,
		&previewUpdatedAt,
		&createdStr,
	)
	if err != nil {
		return model.Version{}, err
	}
	createdAt, err := parseTime(createdStr)
	if err != nil {
		return model.Version{}, err
	}
	v.CreatedAt = createdAt
	if previewUpdatedAt.Valid && previewUpdatedAt.String != "" {
		t, err := parseTime(previewUpdatedAt.String)
		if err != nil {
			return model.Version{}, err
		}
		v.PreviewUpdatedAt = &t
	}
	return v, nil
}

func isPathStrictlyInside(baseDir, targetPath string) bool {
	rel, err := filepath.Rel(baseDir, targetPath)
	if err != nil {
		return false
	}
	if rel == "." || rel == ".." || filepath.IsAbs(rel) {
		return false
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}
