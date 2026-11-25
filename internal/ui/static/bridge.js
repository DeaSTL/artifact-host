(function () {
  "use strict";

  function resolveMeta() {
    var script = document.currentScript;
    if (!script) {
      script = document.querySelector("script[data-slug][data-version]");
    }
    var slug = script ? script.getAttribute("data-slug") || "" : "";
    var version = script ? script.getAttribute("data-version") || "" : "";

    if (!slug || !version) {
      var match = window.location.pathname.match(/^\/raw\/([^/]+)\/v\/([^/]+)/);
      if (match) {
        if (!slug) slug = decodeURIComponent(match[1]);
        if (!version) version = decodeURIComponent(match[2]);
      }
    }
    return { slug: slug, version: version };
  }

  function isPreviewMode() {
    try {
      return new URLSearchParams(window.location.search).get("preview") === "1";
    } catch (_) {
      return false;
    }
  }

  function inferContentType(filename, data) {
    var lower = String(filename || "").toLowerCase();
    if (lower.endsWith(".json")) return "application/json";
    if (lower.endsWith(".csv")) return "text/csv;charset=utf-8";
    if (lower.endsWith(".md") || lower.endsWith(".markdown")) return "text/markdown;charset=utf-8";
    if (lower.endsWith(".html") || lower.endsWith(".htm")) return "text/html;charset=utf-8";
    if (lower.endsWith(".svg")) return "image/svg+xml";
    if (lower.endsWith(".xml")) return "application/xml";
    if (lower.endsWith(".txt")) return "text/plain;charset=utf-8";
    if (data !== null && typeof data === "object" && !(data instanceof Blob) && !(data instanceof ArrayBuffer) && !ArrayBuffer.isView(data)) {
      return "application/json";
    }
    return "text/plain;charset=utf-8";
  }

  function normalizePayload(data, contentType) {
    if (data instanceof Blob) {
      return data.text().then(function (text) {
        return { text: text, blob: data };
      });
    }
    if (data instanceof ArrayBuffer || ArrayBuffer.isView(data)) {
      var bytes = data instanceof ArrayBuffer ? new Uint8Array(data) : new Uint8Array(data.buffer, data.byteOffset, data.byteLength);
      var text = new TextDecoder("utf-8").decode(bytes);
      return Promise.resolve({
        text: text,
        blob: new Blob([bytes], { type: contentType })
      });
    }
    var serialized;
    if (typeof data === "string") {
      serialized = data;
    } else if (data === undefined || data === null) {
      serialized = "";
    } else {
      serialized = JSON.stringify(data, null, 2);
    }
    return Promise.resolve({
      text: serialized,
      blob: new Blob([serialized], { type: contentType })
    });
  }

  function triggerDownload(filename, blob) {
    try {
      var url = URL.createObjectURL(blob);
      var link = document.createElement("a");
      link.href = url;
      link.download = filename || "download.txt";
      link.style.display = "none";
      document.body.appendChild(link);
      link.click();
      document.body.removeChild(link);
      setTimeout(function () {
        URL.revokeObjectURL(url);
      }, 1000);
    } catch (_) {
      // Ignore download errors in restricted contexts.
    }
  }

  var toastTimer = null;

  function showToast(message, isError) {
    if (isPreviewMode() || !document.body) {
      return;
    }
    var id = "artifact-host-bridge-toast";
    var el = document.getElementById(id);
    if (!el) {
      el = document.createElement("div");
      el.id = id;
      el.setAttribute("role", "status");
      el.setAttribute("aria-live", "polite");
      el.style.position = "fixed";
      el.style.right = "16px";
      el.style.bottom = "16px";
      el.style.zIndex = "2147483647";
      el.style.display = "flex";
      el.style.alignItems = "center";
      el.style.gap = "8px";
      el.style.padding = "10px 14px";
      el.style.borderRadius = "8px";
      el.style.fontFamily = "ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif";
      el.style.fontSize = "13px";
      el.style.fontWeight = "500";
      el.style.lineHeight = "1.3";
      el.style.boxShadow = "0 10px 25px -5px rgba(0, 0, 0, 0.2), 0 4px 10px -2px rgba(0, 0, 0, 0.1)";
      el.style.transition = "opacity 180ms ease, transform 180ms ease";
      el.style.pointerEvents = "none";
      document.body.appendChild(el);
    }

    el.style.backgroundColor = isError ? "#991b1b" : "#18181b";
    el.style.color = "#fafafa";
    el.style.border = isError ? "1px solid #ef4444" : "1px solid #27272a";

    var iconSvg = isError
      ? '<svg xmlns="http://www.w3.org/2000/svg" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="flex-shrink:0"><circle cx="12" cy="12" r="10"/><line x1="12" x2="12" y1="8" y2="12"/><line x1="12" x2="12.01" y1="16" y2="16"/></svg>'
      : '<svg xmlns="http://www.w3.org/2000/svg" width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round" style="flex-shrink:0"><path d="M20 6 9 17l-5-5"/></svg>';

    el.innerHTML = iconSvg;
    var span = document.createElement("span");
    span.textContent = message;
    el.appendChild(span);

    el.style.opacity = "1";
    el.style.transform = "translateY(0)";

    if (toastTimer) {
      clearTimeout(toastTimer);
    }
    toastTimer = setTimeout(function () {
      el.style.opacity = "0";
      el.style.transform = "translateY(6px)";
    }, 3500);
  }

  var meta = resolveMeta();

  function saveSubmission(filenameOrOpts, dataArg, contentTypeArg) {
    var filename;
    var data;
    var contentType;

    if (filenameOrOpts && typeof filenameOrOpts === "object" && !Array.isArray(filenameOrOpts)) {
      filename = filenameOrOpts.filename || "submission.json";
      data = filenameOrOpts.data;
      contentType = filenameOrOpts.contentType || filenameOrOpts.content_type;
    } else {
      filename = String(filenameOrOpts || "submission.json");
      data = dataArg;
      contentType = contentTypeArg;
    }

    if (!contentType) {
      contentType = inferContentType(filename, data);
    }

    return normalizePayload(data, contentType).then(function (normalized) {
      triggerDownload(filename, normalized.blob);

      if (!meta.slug || !meta.version) {
        showToast("Saved " + filename + " locally", false);
        return { ok: true, localOnly: true, filename: filename };
      }

      var endpoint =
        "/api/v1/artifacts/" +
        encodeURIComponent(meta.slug) +
        "/versions/" +
        encodeURIComponent(meta.version) +
        "/submissions";

      return fetch(endpoint, {
        method: "POST",
        headers: {
          "Content-Type": "application/json"
        },
        body: JSON.stringify({
          filename: filename,
          content_type: contentType,
          data: normalized.text
        })
      })
        .then(function (res) {
          if (!res.ok) {
            throw new Error("HTTP " + res.status);
          }
          showToast("Saved " + filename + " to artifact host", false);
          return res.json().catch(function () {
            return { ok: true, filename: filename };
          });
        })
        .catch(function (err) {
          showToast("Downloaded " + filename + " (host save failed)", true);
          throw err;
        });
    });
  }

  window.artifact = {
    slug: meta.slug,
    version: meta.version,
    save: function (filename, data, contentType) {
      return saveSubmission(filename, data, contentType);
    }
  };

  window.claude = window.claude || {};
  var prevUse = typeof window.claude.use === "function" ? window.claude.use.bind(window.claude) : null;
  window.claude.use = async function (cap) {
    if (cap === "downloads") {
      return {
        save: function (optsOrFilename, data, contentType) {
          return saveSubmission(optsOrFilename, data, contentType);
        }
      };
    }
    if (prevUse) {
      return prevUse(cap);
    }
    throw new Error("Unsupported capability: " + String(cap));
  };
})();
