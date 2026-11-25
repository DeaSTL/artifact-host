package preview

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/deastl/artifact-host/internal/model"
)

// PlaceholderSVG returns a clean, minimal 640x400 neutral slate/zinc SVG placeholder
// suitable for artifacts whose preview is pending, failed, or unavailable.
func PlaceholderSVG(title string, status string) []byte {
	displayTitle := strings.TrimSpace(title)
	if displayTitle == "" {
		displayTitle = "Untitled Artifact"
	}
	displayTitle = truncateRunes(displayTitle, 48)

	caption := statusCaption(status)
	iconMarkup := statusIconSVG(status)

	escapedTitle := escapeXML(displayTitle)
	escapedCaption := escapeXML(caption)

	var buf bytes.Buffer
	buf.Grow(2048)

	fmt.Fprintf(&buf, `<svg xmlns="http://www.w3.org/2000/svg" width="640" height="400" viewBox="0 0 640 400" role="img" aria-label="%s - %s">`, escapedTitle, escapedCaption)
	buf.WriteString(`
  <style>
    .bg { fill: #f4f4f5; }
    .win { fill: #ffffff; stroke: #e4e4e7; }
    .bar { fill: #fafafa; }
    .line { stroke: #e4e4e7; }
    .dot { fill: #d4d4d8; }
    .pill { fill: #f4f4f5; stroke: #e4e4e7; }
    .skel { fill: #f4f4f5; }
    .rule { stroke: #f4f4f5; }
    .ico-box { fill: #f4f4f5; stroke: #e4e4e7; }
    .ico { stroke: #71717a; }
    .title { fill: #18181b; }
    .cap { fill: #71717a; }
    @media (prefers-color-scheme: dark) {
      .bg { fill: #18181b; }
      .win { fill: #202024; stroke: #2e2e33; }
      .bar { fill: #27272a; }
      .line { stroke: #2e2e33; }
      .dot { fill: #3f3f46; }
      .pill { fill: #1f1f23; stroke: #2e2e33; }
      .skel { fill: #27272a; }
      .rule { stroke: #27272a; }
      .ico-box { fill: #27272a; stroke: #3f3f46; }
      .ico { stroke: #a1a1aa; }
      .title { fill: #fafafa; }
      .cap { fill: #a1a1aa; }
    }
  </style>
  <rect width="640" height="400" class="bg"/>
  <rect x="32" y="28" width="576" height="344" rx="10" class="win" stroke-width="1"/>
  <path d="M32 38a10 10 0 0 1 10-10h556a10 10 0 0 1 10 10v24H32V38z" class="bar"/>
  <line x1="32" y1="62" x2="608" y2="62" class="line" stroke-width="1"/>
  <circle cx="52" cy="45" r="4" class="dot"/>
  <circle cx="68" cy="45" r="4" class="dot"/>
  <circle cx="84" cy="45" r="4" class="dot"/>
  <rect x="112" y="38" width="220" height="14" rx="4" class="pill" stroke-width="0.75"/>
  <rect x="56" y="84" width="128" height="10" rx="3" class="skel"/>
  <rect x="56" y="102" width="84" height="8" rx="3" class="skel"/>
  <rect x="496" y="84" width="88" height="10" rx="3" class="skel"/>
  <line x1="56" y1="336" x2="584" y2="336" class="rule" stroke-width="1"/>
  <rect x="56" y="346" width="96" height="8" rx="3" class="skel"/>
  <rect x="512" y="346" width="72" height="8" rx="3" class="skel"/>
  <rect x="292" y="140" width="56" height="56" rx="12" class="ico-box" stroke-width="1"/>
  <g transform="translate(308, 156)" fill="none" class="ico" stroke-width="1.75" stroke-linecap="round" stroke-linejoin="round">`)
	buf.WriteString(iconMarkup)
	buf.WriteString(`</g>`)
	fmt.Fprintf(&buf, `
  <text x="320" y="232" text-anchor="middle" font-family="ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif" font-size="16" font-weight="600" class="title">%s</text>
  <text x="320" y="256" text-anchor="middle" font-family="ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif" font-size="13" font-weight="400" class="cap">%s</text>
</svg>`, escapedTitle, escapedCaption)

	return buf.Bytes()
}

func statusCaption(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case model.PreviewPending:
		return "Generating preview..."
	case model.PreviewFailed:
		return "Preview failed"
	case model.PreviewDisabled, "":
		return "Preview unavailable"
	case model.PreviewReady:
		return "Preview ready"
	default:
		return truncateRunes(strings.TrimSpace(status), 64)
	}
}

// statusIconSVG returns clean 24x24 Lucide-style geometric SVG primitives.
func statusIconSVG(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case model.PreviewPending:
		// Lucide-style clock icon
		return `<circle cx="12" cy="12" r="9"/><polyline points="12 7 12 12 15.5 14"/>`
	case model.PreviewFailed:
		// Lucide-style triangle-alert icon
		return `<path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/>`
	case model.PreviewDisabled:
		// Lucide-style monitor-off style geometric icon
		return `<rect x="2" y="4" width="20" height="13" rx="2"/><line x1="8" y1="21" x2="16" y2="21"/><line x1="12" y1="17" x2="12" y2="21"/><line x1="2" y1="2" x2="22" y2="22"/>`
	default:
		// Lucide-style app-window / layout icon
		return `<rect x="2" y="4" width="20" height="16" rx="2"/><line x1="2" y1="9" x2="22" y2="9"/><line x1="9" y1="9" x2="9" y2="20"/>`
	}
}

func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}

func escapeXML(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
