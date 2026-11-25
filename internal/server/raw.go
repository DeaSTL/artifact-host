package server

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/deastl/artifact-host/internal/model"
	"github.com/deastl/artifact-host/internal/store"
)

func (s *Server) handleRawRedirectSlash(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	rawVer := chi.URLParam(r, "version")
	if err := model.ValidateSlug(slug); err != nil {
		http.NotFound(w, r)
		return
	}
	verNum, err := parsePositiveInt(rawVer)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	target := fmt.Sprintf("/raw/%s/v/%d/", url.PathEscape(slug), verNum)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) handleRawRedirectCompact(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	verSeg := chi.URLParam(r, "verSeg")
	if err := model.ValidateSlug(slug); err != nil {
		http.NotFound(w, r)
		return
	}
	verNum, ok := parseCompactVersionSegment(verSeg)
	if !ok {
		http.NotFound(w, r)
		return
	}
	target := fmt.Sprintf("/raw/%s/v%d/", url.PathEscape(slug), verNum)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) handleRawSlash(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	rawVer := chi.URLParam(r, "version")
	verNum, err := parsePositiveInt(rawVer)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	s.serveRawArtifact(w, r, slug, verNum, chi.URLParam(r, "*"))
}

func (s *Server) handleRawCompact(w http.ResponseWriter, r *http.Request) {
	slug := chi.URLParam(r, "slug")
	verSeg := chi.URLParam(r, "verSeg")
	verNum, ok := parseCompactVersionSegment(verSeg)
	if !ok {
		http.NotFound(w, r)
		return
	}
	s.serveRawArtifact(w, r, slug, verNum, chi.URLParam(r, "*"))
}

func (s *Server) serveRawArtifact(w http.ResponseWriter, r *http.Request, slug string, verNum int, wildcard string) {
	if err := model.ValidateSlug(slug); err != nil {
		http.NotFound(w, r)
		return
	}

	ver, err := s.store.GetVersion(r.Context(), slug, verNum)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "failed to load artifact version", http.StatusInternalServerError)
		return
	}

	entryFile := strings.TrimSpace(ver.EntryFile)
	if entryFile == "" {
		entryFile = "index.html"
	}

	trimmedWildcard := strings.TrimPrefix(wildcard, "/")
	isEntryRoot := trimmedWildcard == ""
	relPath := trimmedWildcard
	if isEntryRoot {
		relPath = entryFile
	}

	absPath, contentType, err := s.store.ResolveAssetFile(slug, verNum, relPath)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "invalid asset path", http.StatusBadRequest)
		return
	}

	isEntry := isEntryRoot || relPath == entryFile
	isHTML := isHTMLContentType(contentType, relPath)
	q := r.URL.Query()
	skipBridge := q.Get("raw") == "1" || q.Get("nobridge") == "1"

	if isEntry && isHTML {
		w.Header().Set("Cache-Control", "no-cache")
	}

	if isHTML && !skipBridge {
		data, err := os.ReadFile(absPath)
		if err != nil {
			http.Error(w, "failed to read artifact file", http.StatusInternalServerError)
			return
		}
		injected := injectBridgeScript(data, slug, verNum)
		if contentType == "" || !strings.HasPrefix(strings.ToLower(contentType), "text/html") {
			contentType = "text/html; charset=utf-8"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Length", strconv.Itoa(len(injected)))
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = w.Write(injected)
		}
		return
	}

	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	http.ServeFile(w, r, absPath)
}

func parseCompactVersionSegment(seg string) (int, bool) {
	if len(seg) < 2 || seg[0] != 'v' {
		return 0, false
	}
	digits := seg[1:]
	for i := 0; i < len(digits); i++ {
		if digits[i] < '0' || digits[i] > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(digits)
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

func isHTMLContentType(contentType, relPath string) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if strings.HasPrefix(ct, "text/html") {
		return true
	}
	ext := strings.ToLower(path.Ext(relPath))
	return ext == ".html" || ext == ".htm"
}

func injectBridgeScript(doc []byte, slug string, version int) []byte {
	tag := fmt.Sprintf(`<script src="/_artifact/bridge.js" data-slug="%s" data-version="%d"></script>`, html.EscapeString(slug), version)
	tagBytes := []byte(tag)

	if idx := indexFoldASCII(doc, "</head>"); idx >= 0 {
		out := make([]byte, 0, len(doc)+len(tagBytes)+1)
		out = append(out, doc[:idx]...)
		out = append(out, tagBytes...)
		out = append(out, '\n')
		out = append(out, doc[idx:]...)
		return out
	}
	if idx := indexFoldASCII(doc, "</body>"); idx >= 0 {
		out := make([]byte, 0, len(doc)+len(tagBytes)+1)
		out = append(out, doc[:idx]...)
		out = append(out, tagBytes...)
		out = append(out, '\n')
		out = append(out, doc[idx:]...)
		return out
	}

	out := make([]byte, 0, len(doc)+len(tagBytes)+1)
	out = append(out, tagBytes...)
	out = append(out, '\n')
	out = append(out, doc...)
	return out
}

// indexFoldASCII returns the byte index of the first ASCII case-insensitive occurrence
// of lowercase ASCII needle in haystack, or -1 if not present.
func indexFoldASCII(haystack []byte, needle string) int {
	n := len(needle)
	if n == 0 {
		return 0
	}
	if len(haystack) < n {
		return -1
	}
	limit := len(haystack) - n
	for i := 0; i <= limit; i++ {
		matched := true
		for j := 0; j < n; j++ {
			b := haystack[i+j]
			if b >= 'A' && b <= 'Z' {
				b += 'a' - 'A'
			}
			if b != needle[j] {
				matched = false
				break
			}
		}
		if matched {
			return i
		}
	}
	return -1
}
