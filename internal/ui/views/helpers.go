package views

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"
	"github.com/deastl/artifact-host/internal/model"
	"github.com/deastl/artifact-host/internal/ui/components/icon"
)

var iconAliases = map[string]string{
	"file-code-2":  "file-code",
	"alert-circle": "circle-alert",
	"check-circle": "circle-check",
	"help-circle":  "circle-help",
	"info-circle":  "info",
	"bar-chart":    "chart-bar",
	"bar-chart-2":  "chart-column",
	"pie-chart":    "chart-pie",
	"line-chart":   "chart-line",
}

// ArtifactIcon safely resolves a Lucide icon by name, falling back to icon.FileCode
// when name is empty or unrecognized so template rendering never fails.
func ArtifactIcon(name string, props ...icon.Props) templ.Component {
	normalized := strings.TrimSpace(strings.ToLower(name))
	if alias, ok := iconAliases[normalized]; ok {
		normalized = alias
	}
	if normalized == "" {
		return icon.FileCode(props...)
	}

	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		var buf bytes.Buffer
		if err := icon.Icon(normalized)(props...).Render(ctx, &buf); err == nil {
			_, writeErr := w.Write(buf.Bytes())
			return writeErr
		}
		return icon.FileCode(props...).Render(ctx, w)
	})
}

// RelativeTime formats a timestamp as a concise human-readable relative string
// (e.g. "just now", "2m ago", "3h ago", "Nov 25").
func RelativeTime(t time.Time) string {
	return relativeTimeFrom(t, time.Now())
}

func relativeTimeFrom(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	case t.Year() == now.Year():
		return t.Format("Jan 2")
	default:
		return t.Format("Jan 2, 2006")
	}
}

// FormatBytes formats a byte count into a compact human-readable string.
func FormatBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case n < kb:
		return fmt.Sprintf("%d B", n)
	case n < mb:
		return fmt.Sprintf("%.1f KB", float64(n)/kb)
	case n < gb:
		return fmt.Sprintf("%.1f MB", float64(n)/mb)
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/gb)
	}
}

// Pluralize returns "<n> <singular>" when n == 1, otherwise "<n> <plural>".
func Pluralize(n int, singular, plural string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, singular)
	}
	return fmt.Sprintf("%d %s", n, plural)
}

func displayTitle(title, slug string) string {
	if strings.TrimSpace(title) != "" {
		return title
	}
	if strings.TrimSpace(slug) != "" {
		return slug
	}
	return "Untitled Artifact"
}

func shortHash(h string) string {
	h = strings.TrimSpace(h)
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func artifactURL(slug string) string {
	return "/a/" + url.PathEscape(slug)
}

func artifactVersionURL(slug string, version int) string {
	return fmt.Sprintf("/a/%s/v/%d", url.PathEscape(slug), version)
}

func rawVersionURL(slug string, version int) string {
	return fmt.Sprintf("/raw/%s/v/%d/", url.PathEscape(slug), version)
}

func rawAssetURL(slug string, version int, assetPath string) string {
	parts := strings.Split(strings.TrimPrefix(assetPath, "/"), "/")
	escaped := make([]string, 0, len(parts))
	for _, seg := range parts {
		if seg == "" {
			continue
		}
		escaped = append(escaped, url.PathEscape(seg))
	}
	return fmt.Sprintf("/raw/%s/v/%d/%s", url.PathEscape(slug), version, strings.Join(escaped, "/"))
}

func previewURL(slug string, version int, hash string) string {
	base := fmt.Sprintf("/previews/%s/v%d.png", url.PathEscape(slug), version)
	if h := shortHash(hash); h != "" {
		return base + "?h=" + url.QueryEscape(h)
	}
	return base
}

func submissionURL(slug string, id int64) string {
	return fmt.Sprintf("/api/v1/artifacts/%s/submissions/%d", url.PathEscape(slug), id)
}

func galleryFilterURL(query, tag string) string {
	vals := url.Values{}
	if q := strings.TrimSpace(query); q != "" {
		vals.Set("q", q)
	}
	if t := strings.TrimSpace(tag); t != "" {
		vals.Set("tag", t)
	}
	if len(vals) == 0 {
		return "/"
	}
	return "/?" + vals.Encode()
}

func sortedVersionsDesc(versions []model.Version) []model.Version {
	out := slices.Clone(versions)
	slices.SortFunc(out, func(a, b model.Version) int {
		return b.Number - a.Number
	})
	return out
}

func prevVersionNumber(versionsDesc []model.Version, current int) int {
	best := 0
	for _, v := range versionsDesc {
		if v.Number < current && v.Number > best {
			best = v.Number
		}
	}
	return best
}

func nextVersionNumber(versionsDesc []model.Version, current int) int {
	best := 0
	for _, v := range versionsDesc {
		if v.Number > current && (best == 0 || v.Number < best) {
			best = v.Number
		}
	}
	return best
}

func isOlderVersion(art model.Artifact, current model.Version) bool {
	return art.LatestVersion > 0 && current.Number > 0 && current.Number < art.LatestVersion
}

func versionOptionLabel(art model.Artifact, v model.Version) string {
	var parts []string
	label := fmt.Sprintf("v%d", v.Number)
	if v.Number == art.LatestVersion {
		label += " (latest)"
	}
	parts = append(parts, label)

	if note := strings.TrimSpace(v.Note); note != "" {
		if len(note) > 28 {
			note = note[:25] + "..."
		}
		parts = append(parts, note)
	}
	if !v.CreatedAt.IsZero() {
		parts = append(parts, RelativeTime(v.CreatedAt))
	}
	return strings.Join(parts, " - ")
}

func viewerPageTitle(data ViewerData) string {
	title := displayTitle(data.Artifact.Title, data.Artifact.Slug)
	if data.Current.Number > 0 {
		return fmt.Sprintf("%s (v%d) - Artifact Host", title, data.Current.Number)
	}
	return title + " - Artifact Host"
}

func effectiveArtifactCount(data GalleryData) int {
	if data.Status.ArtifactCount > 0 {
		return data.Status.ArtifactCount
	}
	if data.TotalCount > 0 {
		return data.TotalCount
	}
	return len(data.Artifacts)
}

func effectiveVersionCount(data GalleryData) int {
	if data.Status.VersionCount > 0 {
		return data.Status.VersionCount
	}
	total := 0
	for _, a := range data.Artifacts {
		if a.VersionCount > 0 {
			total += a.VersionCount
		} else if a.LatestVersion > 0 {
			total += a.LatestVersion
		}
	}
	return total
}

func versionCountDisplay(art model.Artifact) int {
	if art.VersionCount > 0 {
		return art.VersionCount
	}
	if art.LatestVersion > 0 {
		return art.LatestVersion
	}
	return 1
}

func statusAddrLabel(status model.DaemonStatus) string {
	if strings.TrimSpace(status.Addr) != "" {
		return status.Addr
	}
	if strings.TrimSpace(status.Status) != "" {
		return status.Status
	}
	return "local"
}

func errorDefaults(data ErrorData) ErrorData {
	out := data
	if out.StatusCode == 0 {
		out.StatusCode = 500
	}
	if strings.TrimSpace(out.Title) == "" {
		if out.StatusCode == 404 {
			out.Title = "Artifact not found"
		} else {
			out.Title = "Something went wrong"
		}
	}
	if strings.TrimSpace(out.Message) == "" {
		if out.StatusCode == 404 {
			out.Message = "The artifact or version you requested does not exist or may have been removed."
		} else {
			out.Message = "An unexpected error occurred while processing your request."
		}
	}
	if strings.TrimSpace(out.BackURL) == "" {
		out.BackURL = "/"
	}
	if strings.TrimSpace(out.BackLabel) == "" {
		out.BackLabel = "Back to artifacts"
	}
	return out
}

func itoa(n int) string {
	return strconv.Itoa(n)
}
