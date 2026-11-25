# artifact-host

A single-binary Go daemon and CLI for hosting versioned interactive HTML artifacts (decision sheets, reports, prototypes, visualizers) with static assets, cached headless-Chrome page previews, and a server-side rendered UI.

## Features

- **Single static binary**: Pure-Go SQLite (`modernc.org/sqlite`, zero CGo) with all CSS, JS, and systemd templates embedded via `//go:embed`.
- **Versioned artifacts + static assets**: Every push to a slug creates an immutable version (`v1`, `v2`, ...) with nested static assets served cleanly at `/raw/<slug>/v/<N>/...`. Flip back and forth between versions or restore any prior version as latest.
- **Cached page previews**: Headless Chrome renders 1280x800 snapshots, box-downscales them to 640x400 PNG thumbnails, and caches them on disk with immutable HTTP headers (`ETag` + `max-age=31536000`).
- **SSR UI with dark mode**: Built with `a-h/templ`, `shadcn-templ`, Lucide SVG icons, and Tailwind CSS v4. Automatically follows `prefers-color-scheme` with a System / Light / Dark toggle.
- **Browser submissions bridge**: Automatically injects `/_artifact/bridge.js` into hosted HTML entry files, implementing `window.artifact.save(...)` and `window.claude.use('downloads')` so interactive pages (such as decision sheets) save JSON/files directly back to the daemon.
- **Systemd user service**: Built-in `artifact service install|start|stop|restart|status|logs` management.

## Quick Start

### Build & Install

Generated `*_templ.go` files and compiled `internal/ui/static/app.css` are committed, so building requires only Go 1.26+:

```bash
go build -o ~/.local/bin/artifact ./cmd/artifact
```

Install and start the `systemd --user` background service (default `http://127.0.0.1:7337`):

```bash
artifact service install
artifact status
```

### Push an Artifact

```bash
# Single HTML file + companion static assets
artifact push my-sheet ./index.html \
  --asset questions.json=./questions.json \
  --title "Planning Sheet" \
  --description "Architecture decisions" \
  --icon list-checks \
  --tag planning \
  --note "Initial version"

# Or push an entire directory (entry defaults to index.html)
artifact push my-report ./dist --title "Benchmark Report"
```

Pushing again to the same slug creates `v2`, `v3`, etc. Open the gallery at `http://127.0.0.1:7337/` or jump straight to `http://127.0.0.1:7337/a/my-sheet`.

## CLI Reference

```text
artifact serve                         Run the HTTP daemon in the foreground
artifact push <slug> <file-or-dir>     Push HTML + static assets as a new version
artifact list [--search Q] [--tag T]   List hosted artifacts
artifact info <slug> [--version N]     Show artifact metadata, files, and submissions
artifact versions <slug>               List all versions of an artifact
artifact rollback <slug> <version>     Restore a prior version as a new latest version
artifact pull <slug> [--out DIR]       Download an artifact's files back to disk
artifact preview <slug> [--refresh]    Inspect or re-render a cached page preview
artifact submissions <slug> [--wait]   List, fetch, or wait for browser-saved files
artifact delete <slug> [--version N]   Delete an artifact or single version
artifact open <slug> [--version N]     Open an artifact in the default browser
artifact status                        Show daemon, cache, Chrome, and systemd status
artifact service <action>              install | uninstall | start | stop | restart | status | logs
artifact completion <shell>            Generate bash, zsh, fish, or powershell completions
```

Every query and mutation command supports `--json` for scripting and agent skills.

## Development

To regenerate `templ` views and Tailwind CSS after UI edits:

```bash
npm install
make templ
make css
go test -race ./...
```
