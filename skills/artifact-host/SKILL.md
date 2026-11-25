---
name: artifact-host
description: Publish interactive HTML artifacts (decision sheets, UI prototypes, benchmark dashboards, diagrams, reports, visualizers) with static assets to the local artifact-host daemon using the `artifact` CLI. Use this whenever an agent needs to show an interactive HTML page to the user in a browser, iterate across artifact versions (v1, v2, v3), or collect structured JSON/file answers back from browser artifacts (compatible with Claude's `window.claude.use('downloads')` API) in environments like Antigravity, Codex, OpenCode, or Claude Code.
---

# Artifact Host (`artifact`)

`artifact-host` gives any coding agent with shell access a persistent local host for interactive HTML artifacts (default `http://127.0.0.1:7337`, managed by `systemd --user`). Every push to a slug creates an immutable version (`v1`, `v2`, ...), renders a cached headless-Chrome preview thumbnail for the gallery, serves relative static assets cleanly, and injects a browser bridge (`window.artifact.save` and `window.claude.use('downloads')`) so interactive pages can send JSON or files back to the agent.

## 1. Check Daemon Availability

Before pushing, verify the daemon is running (or check status):

```bash
artifact status --json
```

If the daemon is not running and systemd is installed:

```bash
artifact service start
```

## 2. Push an HTML Artifact (Single File, Assets, or Directory)

Choose a short, kebab-case `<slug>` (letters, digits, dashes, underscores, dots). Reusing the same `<slug>` on a revision creates the next version (`v2`, `v3`, ...) and lets the user flip back and forth between versions in the UI.

```bash
# Single HTML file + companion static assets
artifact push my-dashboard ./index.html \
  --asset data.json=./data.json \
  --asset assets/chart.js=./chart.js \
  --title "Benchmark Dashboard" \
  --description "Before/after incremental compile timings" \
  --icon bar-chart-3 \
  --tag benchmarks \
  --tag performance \
  --note "Initial baseline" \
  --json

# Entire directory (defaults to index.html as entry point)
artifact push my-prototype ./dist \
  --title "Settings Screen Prototype" \
  --icon layout-dashboard \
  --tag prototype \
  --json
```

Guidelines for agents:
- Always pass `--json` when scripting so you can read `url`, `raw_url`, and `version.number` reliably.
- On revisions (`v2`, `v3`), you only need `--note "What changed"` — omitted `--title`, `--description`, `--icon`, and `--tag` values are preserved from the artifact.
- `--icon` accepts any Lucide icon name in kebab-case (for example: `list-checks`, `bar-chart-3`, `network`, `layers`, `file-code`, `palette`, `layout-dashboard`, `file-text`).
- Return the clickable `url` (`http://127.0.0.1:7337/a/<slug>`) to the user.

## 3. Interactive Browser-to-Agent Submissions (Decision Sheets & Forms)

Every HTML entry file served at `/raw/<slug>/v/<N>/` automatically receives `/_artifact/bridge.js`, which exposes:

```js
// Native API
await window.artifact.save("answers.json", JSON.stringify(payload), "application/json");

// Drop-in compatibility with Claude's Artifact downloads capability
const downloads = await window.claude.use("downloads");
await downloads.save({ filename: "sheet-answers.json", data: JSON.stringify(payload) });
```

Calling either API saves the payload to the daemon AND triggers a local browser download.

### Decision Sheet Workflow
1. Generate `index.html` and `questions.json` (e.g. via `decision-sheet`'s `build_sheet.py`).
2. Push both files to `artifact-host`:
   ```bash
   artifact push <sheet-id> <dir>/index.html \
     --asset questions.json=<dir>/questions.json \
     --title "<Sheet Title>" \
     --description "<One-sentence summary>" \
     --icon list-checks \
     --tag decision-sheet \
     --json
   ```
3. Give the user the returned `url` and ask them to click **Save JSON file** when done.
4. Read the submitted answers back into the agent:
   ```bash
   # Fetch the most recent submission immediately
   artifact submissions <sheet-id> --latest

   # Or block until the user submits
   artifact submissions <sheet-id> --wait --timeout 10m --out /tmp/answers.json
   ```

## 4. Manage Versions, Previews, and Artifacts

```bash
artifact list [--search QUERY] [--tag TAG] [--json]
artifact info <slug> [--version N] [--json]
artifact versions <slug> [--json]
artifact rollback <slug> <version> [--note "Restored v1"] [--json]
artifact pull <slug> [--version N] --out <dir>
artifact preview <slug> [--version N] --refresh [--json]
artifact submissions <slug> [--version N] [--latest | --id ID | --wait] [--out FILE] [--json]
artifact delete <slug> [--version N] --yes
artifact open <slug> [--version N] [--raw]
artifact service install|uninstall|start|stop|restart|status|logs
```
