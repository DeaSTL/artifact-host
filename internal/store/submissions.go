package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/deastl/artifact-host/internal/model"
)

// SaveSubmission stores a payload submitted from an interactive artifact.
// If versionNum <= 0, the artifact's effective version is recorded.
func (s *Store) SaveSubmission(ctx context.Context, slug string, versionNum int, filename, contentType string, data []byte) (*model.Submission, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	var artifactID int64
	var latestVer, activeVer int
	err := s.db.QueryRowContext(ctx, `
		SELECT id, latest_version, active_version
		FROM artifacts
		WHERE slug = ?
	`, slug).Scan(&artifactID, &latestVer, &activeVer)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("lookup artifact %q: %w", slug, err)
	}

	resolvedVersion := versionNum
	if resolvedVersion <= 0 {
		if activeVer > 0 {
			resolvedVersion = activeVer
		} else {
			resolvedVersion = latestVer
		}
	}

	cleanFilename := strings.TrimSpace(filename)
	ct := strings.TrimSpace(contentType)
	if ct == "" {
		ct = detectContentType(cleanFilename, data)
	}

	payload := []byte{}
	if len(data) > 0 {
		payload = make([]byte, len(data))
		copy(payload, data)
	}
	size := int64(len(payload))
	now := time.Now().UTC()

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO submissions (artifact_id, version_number, filename, content_type, size, data, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, artifactID, resolvedVersion, cleanFilename, ct, size, payload, formatTime(now))
	if err != nil {
		return nil, fmt.Errorf("insert submission: %w", err)
	}

	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("submission insert id: %w", err)
	}

	return &model.Submission{
		ID:          id,
		ArtifactID:  artifactID,
		Slug:        slug,
		Version:     resolvedVersion,
		Filename:    cleanFilename,
		ContentType: ct,
		Size:        size,
		Data:        payload,
		CreatedAt:   now,
	}, nil
}

// ListSubmissions lists submission metadata for slug ordered by created_at DESC, id DESC.
// If versionNum <= 0, submissions for all versions of slug are returned.
func (s *Store) ListSubmissions(ctx context.Context, slug string, versionNum int) ([]model.SubmissionMeta, error) {
	var artifactID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM artifacts WHERE slug = ?`, slug).Scan(&artifactID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("lookup artifact %q: %w", slug, err)
	}

	var (
		rows *sql.Rows
		err  error
	)
	if versionNum > 0 {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, version_number, filename, content_type, size, created_at
			FROM submissions
			WHERE artifact_id = ? AND version_number = ?
			ORDER BY created_at DESC, id DESC
		`, artifactID, versionNum)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, version_number, filename, content_type, size, created_at
			FROM submissions
			WHERE artifact_id = ?
			ORDER BY created_at DESC, id DESC
		`, artifactID)
	}
	if err != nil {
		return nil, fmt.Errorf("query submissions: %w", err)
	}
	defer rows.Close()

	out := make([]model.SubmissionMeta, 0)
	for rows.Next() {
		var (
			m          model.SubmissionMeta
			createdStr string
		)
		m.Slug = slug
		if err := rows.Scan(&m.ID, &m.Version, &m.Filename, &m.ContentType, &m.Size, &createdStr); err != nil {
			return nil, fmt.Errorf("scan submission metadata: %w", err)
		}
		createdAt, err := parseTime(createdStr)
		if err != nil {
			return nil, err
		}
		m.CreatedAt = createdAt
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate submissions: %w", err)
	}

	return out, nil
}

// GetSubmission retrieves a single submission including its raw payload by ID.
func (s *Store) GetSubmission(ctx context.Context, id int64) (*model.Submission, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT s.id, s.artifact_id, a.slug, s.version_number, s.filename, s.content_type, s.size, s.data, s.created_at
		FROM submissions s
		JOIN artifacts a ON a.id = s.artifact_id
		WHERE s.id = ?
	`, id)

	sub, err := scanSubmission(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &sub, nil
}

// LatestSubmission returns the most recent submission for slug (optionally filtered by versionNum > 0).
func (s *Store) LatestSubmission(ctx context.Context, slug string, versionNum int) (*model.Submission, error) {
	var artifactID int64
	if err := s.db.QueryRowContext(ctx, `SELECT id FROM artifacts WHERE slug = ?`, slug).Scan(&artifactID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("lookup artifact %q: %w", slug, err)
	}

	var row *sql.Row
	if versionNum > 0 {
		row = s.db.QueryRowContext(ctx, `
			SELECT s.id, s.artifact_id, a.slug, s.version_number, s.filename, s.content_type, s.size, s.data, s.created_at
			FROM submissions s
			JOIN artifacts a ON a.id = s.artifact_id
			WHERE s.artifact_id = ? AND s.version_number = ?
			ORDER BY s.created_at DESC, s.id DESC
			LIMIT 1
		`, artifactID, versionNum)
	} else {
		row = s.db.QueryRowContext(ctx, `
			SELECT s.id, s.artifact_id, a.slug, s.version_number, s.filename, s.content_type, s.size, s.data, s.created_at
			FROM submissions s
			JOIN artifacts a ON a.id = s.artifact_id
			WHERE s.artifact_id = ?
			ORDER BY s.created_at DESC, s.id DESC
			LIMIT 1
		`, artifactID)
	}

	sub, err := scanSubmission(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &sub, nil
}

func scanSubmission(sc rowScanner) (model.Submission, error) {
	var (
		sub        model.Submission
		createdStr string
	)
	err := sc.Scan(
		&sub.ID,
		&sub.ArtifactID,
		&sub.Slug,
		&sub.Version,
		&sub.Filename,
		&sub.ContentType,
		&sub.Size,
		&sub.Data,
		&createdStr,
	)
	if err != nil {
		return model.Submission{}, err
	}
	createdAt, err := parseTime(createdStr)
	if err != nil {
		return model.Submission{}, err
	}
	sub.CreatedAt = createdAt
	if sub.Data == nil {
		sub.Data = []byte{}
	}
	return sub, nil
}
