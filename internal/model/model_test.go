package model

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestValidateSlug(t *testing.T) {
	valid := []string{
		"a",
		"my-artifact",
		"report_2026",
		"v1.2.3",
		"abc_123-def.456",
		strings.Repeat("a", 120),
	}
	for _, slug := range valid {
		if err := ValidateSlug(slug); err != nil {
			t.Errorf("ValidateSlug(%q) unexpected error: %v", slug, err)
		}
	}

	invalid := []string{
		"",
		".",
		"..",
		"a..b",
		"../secret",
		"foo/bar",
		"foo\\bar",
		"MyArtifact",
		"hello world",
		"slug@1",
		strings.Repeat("a", 121),
	}
	for _, slug := range invalid {
		if err := ValidateSlug(slug); err == nil {
			t.Errorf("ValidateSlug(%q) expected error, got nil", slug)
		}
	}
}

func TestSanitizeAssetPath(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "index.html", want: "index.html"},
		{in: "./index.html", want: "index.html"},
		{in: "assets/app.js", want: "assets/app.js"},
		{in: "assets//nested/./style.css", want: "assets/nested/style.css"},
		{in: "dir/file.tar.gz", want: "dir/file.tar.gz"},
		{in: "", wantErr: true},
		{in: ".", wantErr: true},
		{in: "./", wantErr: true},
		{in: "..", wantErr: true},
		{in: "../secret.txt", wantErr: true},
		{in: "assets/../../etc/passwd", wantErr: true},
		{in: "assets/../index.html", wantErr: true},
		{in: "/etc/passwd", wantErr: true},
		{in: "assets\\app.js", wantErr: true},
		{in: "index\x00.html", wantErr: true},
	}

	for _, tc := range tests {
		got, err := SanitizeAssetPath(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("SanitizeAssetPath(%q) expected error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("SanitizeAssetPath(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("SanitizeAssetPath(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestArtifactEffectiveVersion(t *testing.T) {
	a := Artifact{LatestVersion: 5, ActiveVersion: 0}
	if got := a.EffectiveVersion(); got != 5 {
		t.Fatalf("EffectiveVersion() = %d, want 5", got)
	}
	a.ActiveVersion = 3
	if got := a.EffectiveVersion(); got != 3 {
		t.Fatalf("EffectiveVersion() = %d, want 3", got)
	}
}

func TestSubmissionMetaAndJSON(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	sub := Submission{
		ID:          42,
		ArtifactID:  7,
		Slug:        "demo-form",
		Version:     2,
		Filename:    "response.json",
		ContentType: "application/json",
		Size:        13,
		Data:        []byte(`{"ok":true}`),
		CreatedAt:   now,
	}
	meta := sub.Meta()
	if meta.ID != sub.ID || meta.Slug != sub.Slug || meta.Version != sub.Version || meta.Filename != sub.Filename {
		t.Fatalf("Submission.Meta() mismatch: %+v", meta)
	}

	raw, err := json.Marshal(ArtifactDetail{
		Artifact: Artifact{ID: 7, Slug: "demo-form", LatestVersion: 2, Tags: []string{"ui"}},
		Versions: []Version{{ID: 1, Number: 2, PreviewStatus: PreviewReady}},
		Submissions: []SubmissionMeta{meta},
	})
	if err != nil {
		t.Fatalf("json.Marshal(ArtifactDetail) error: %v", err)
	}
	var decoded ArtifactDetail
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("json.Unmarshal(ArtifactDetail) error: %v", err)
	}
	if decoded.Artifact.Slug != "demo-form" || len(decoded.Submissions) != 1 || decoded.Submissions[0].ID != 42 {
		t.Fatalf("decoded ArtifactDetail mismatch: %+v", decoded)
	}
}
