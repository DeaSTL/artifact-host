(function () {
  "use strict";

  function copyText(text) {
    if (navigator.clipboard && typeof navigator.clipboard.writeText === "function") {
      return navigator.clipboard.writeText(text).catch(function () {
        return fallbackCopy(text);
      });
    }
    return fallbackCopy(text);
  }

  function fallbackCopy(text) {
    return new Promise(function (resolve, reject) {
      try {
        var area = document.createElement("textarea");
        area.value = text;
        area.setAttribute("readonly", "");
        area.style.position = "fixed";
        area.style.top = "-9999px";
        area.style.left = "-9999px";
        document.body.appendChild(area);
        area.select();
        var ok = document.execCommand("copy");
        document.body.removeChild(area);
        if (ok) {
          resolve();
        } else {
          reject(new Error("copy failed"));
        }
      } catch (err) {
        reject(err);
      }
    });
  }

  function flashButtonLabel(btn, temporaryText) {
    var label = btn.querySelector("[data-copy-label]");
    var originalTitle = btn.getAttribute("title") || "";
    btn.setAttribute("data-copied", "true");
    btn.setAttribute("title", temporaryText);

    if (label) {
      if (!label.dataset.originalText) {
        label.dataset.originalText = label.textContent;
      }
      label.textContent = temporaryText;
    }

    if (btn._copyResetTimer) {
      clearTimeout(btn._copyResetTimer);
    }
    btn._copyResetTimer = setTimeout(function () {
      btn.removeAttribute("data-copied");
      if (originalTitle) {
        btn.setAttribute("title", originalTitle);
      }
      if (label && label.dataset.originalText) {
        label.textContent = label.dataset.originalText;
      }
    }, 1800);
  }

  function initCopyURLButtons() {
    document.addEventListener("click", function (event) {
      var btn = event.target.closest("[data-copy-url]");
      if (!btn) return;
      event.preventDefault();

      var raw = btn.getAttribute("data-copy-url") || "";
      var fullURL;
      try {
        fullURL = new URL(raw, window.location.origin).toString();
      } catch (_) {
        fullURL = raw;
      }

      copyText(fullURL).then(function () {
        flashButtonLabel(btn, "Copied");
      });
    });
  }

  function initVersionSelect() {
    document.addEventListener("change", function (event) {
      var select = event.target.closest("[data-version-select]");
      if (!select) return;
      var slug = select.getAttribute("data-slug");
      var val = select.value;
      if (!slug || !val) return;
      window.location.href = "/a/" + encodeURIComponent(slug) + "/v/" + encodeURIComponent(val);
    });
  }

  function initKeyboardVersionNav() {
    document.addEventListener("keydown", function (event) {
      if (event.metaKey || event.ctrlKey || event.altKey) return;
      var target = event.target;
      if (target) {
        var tag = (target.tagName || "").toLowerCase();
        if (tag === "input" || tag === "textarea" || tag === "select" || target.isContentEditable) {
          return;
        }
      }

      if (event.key === "[") {
        var prev = document.querySelector("[data-prev-version-href]");
        if (prev) {
          var prevHref = prev.getAttribute("data-prev-version-href");
          if (prevHref) {
            event.preventDefault();
            window.location.href = prevHref;
          }
        }
      } else if (event.key === "]") {
        var next = document.querySelector("[data-next-version-href]");
        if (next) {
          var nextHref = next.getAttribute("data-next-version-href");
          if (nextHref) {
            event.preventDefault();
            window.location.href = nextHref;
          }
        }
      }
    });
  }

  function pollPendingPreview(img) {
    var slug = img.getAttribute("data-slug");
    var version = img.getAttribute("data-version");
    if (!slug || !version) return;

    var attempts = 0;
    var maxAttempts = 15;
    var intervalMs = 2000;

    function tick() {
      attempts++;
      var url =
        "/api/v1/artifacts/" +
        encodeURIComponent(slug) +
        "/versions/" +
        encodeURIComponent(version);

      fetch(url, { headers: { Accept: "application/json" } })
        .then(function (res) {
          if (!res.ok) throw new Error("HTTP " + res.status);
          return res.json();
        })
        .then(function (data) {
          var status = data && data.preview_status;
          if (status === "ready") {
            var hash = (data.preview_hash || data.content_hash || "").slice(0, 16);
            var nextSrc =
              "/previews/" +
              encodeURIComponent(slug) +
              "/v" +
              encodeURIComponent(version) +
              ".png?h=" +
              encodeURIComponent(hash || String(Date.now())) +
              "&t=" +
              Date.now();
            img.src = nextSrc;
            img.classList.remove("opacity-0");
            img.removeAttribute("data-preview-pending");
            var frame = img.closest("[data-preview-frame]");
            if (frame) {
              var badge = frame.querySelector("[data-preview-status-badge]");
              if (badge) badge.remove();
            }
            return;
          }
          if (status === "failed" || status === "disabled") {
            img.removeAttribute("data-preview-pending");
            return;
          }
          if (attempts < maxAttempts) {
            setTimeout(tick, intervalMs);
          }
        })
        .catch(function () {
          if (attempts < maxAttempts) {
            setTimeout(tick, intervalMs);
          }
        });
    }

    setTimeout(tick, intervalMs);
  }

  function initPendingPreviews() {
    var pending = document.querySelectorAll("img[data-preview-pending]");
    pending.forEach(function (img) {
      pollPendingPreview(img);
    });

    var previewImgs = document.querySelectorAll("img[data-preview-img]");
    previewImgs.forEach(function (img) {
      img.addEventListener("error", function () {
        img.classList.add("opacity-0");
      });
      img.addEventListener("load", function () {
        img.classList.remove("opacity-0");
      });
    });
  }

  function initRefreshPreviewButtons() {
    document.addEventListener("click", function (event) {
      var btn = event.target.closest("[data-refresh-preview]");
      if (!btn || btn.disabled) return;
      event.preventDefault();

      var slug = btn.getAttribute("data-slug");
      var version = btn.getAttribute("data-version");
      if (!slug || !version) return;

      var svg = btn.querySelector("svg");
      btn.disabled = true;
      if (svg) svg.classList.add("animate-spin");

      var url =
        "/api/v1/artifacts/" +
        encodeURIComponent(slug) +
        "/versions/" +
        encodeURIComponent(version) +
        "/preview/refresh";

      fetch(url, { method: "POST" })
        .then(function (res) {
          if (!res.ok) throw new Error("HTTP " + res.status);
          var matchingImgs = document.querySelectorAll(
            'img[data-preview-img][data-slug="' + CSS.escape(slug) + '"][data-version="' + CSS.escape(version) + '"]'
          );
          matchingImgs.forEach(function (img) {
            img.setAttribute("data-preview-pending", "true");
            pollPendingPreview(img);
          });
        })
        .catch(function () {
          // Keep UI calm; button resets below.
        })
        .finally(function () {
          setTimeout(function () {
            btn.disabled = false;
            if (svg) svg.classList.remove("animate-spin");
          }, 500);
        });
    });
  }

  function initDeleteAndRollbackButtons() {
    document.addEventListener("click", function (event) {
      var delBtn = event.target.closest("[data-delete-artifact]");
      if (delBtn) {
        event.preventDefault();
        var delSlug = delBtn.getAttribute("data-slug");
        if (!delSlug) return;
        var delMsg =
          delBtn.getAttribute("data-confirm") ||
          'Delete artifact "' + delSlug + '" and all of its versions? This cannot be undone.';
        if (!window.confirm(delMsg)) return;

        delBtn.disabled = true;
        fetch("/api/v1/artifacts/" + encodeURIComponent(delSlug), {
          method: "DELETE"
        })
          .then(function (res) {
            if (!res.ok) throw new Error("HTTP " + res.status);
            window.location.href = "/";
          })
          .catch(function () {
            delBtn.disabled = false;
            window.alert("Failed to delete artifact.");
          });
        return;
      }

      var rbBtn = event.target.closest("[data-rollback-version]");
      if (rbBtn) {
        event.preventDefault();
        var rbSlug = rbBtn.getAttribute("data-slug");
        var rbVersion = rbBtn.getAttribute("data-version");
        if (!rbSlug || !rbVersion) return;
        var rbMsg =
          rbBtn.getAttribute("data-confirm") ||
          "Restore v" + rbVersion + " as the latest active version?";
        if (!window.confirm(rbMsg)) return;

        rbBtn.disabled = true;
        fetch(
          "/api/v1/artifacts/" +
            encodeURIComponent(rbSlug) +
            "/versions/" +
            encodeURIComponent(rbVersion) +
            "/rollback",
          { method: "POST" }
        )
          .then(function (res) {
            if (!res.ok) throw new Error("HTTP " + res.status);
            window.location.href = "/a/" + encodeURIComponent(rbSlug);
          })
          .catch(function () {
            rbBtn.disabled = false;
            window.alert("Failed to restore version.");
          });
      }
    });
  }

  function initCopySubmissionButtons() {
    document.addEventListener("click", function (event) {
      var btn = event.target.closest("[data-copy-submission]");
      if (!btn) return;
      event.preventDefault();

      var url = btn.getAttribute("data-submission-url");
      if (!url) return;

      btn.disabled = true;
      fetch(url)
        .then(function (res) {
          if (!res.ok) throw new Error("HTTP " + res.status);
          return res.text();
        })
        .then(function (payload) {
          return copyText(payload);
        })
        .then(function () {
          flashButtonLabel(btn, "Copied");
        })
        .catch(function () {
          flashButtonLabel(btn, "Failed");
        })
        .finally(function () {
          btn.disabled = false;
        });
    });
  }

  var THEME_STORAGE_KEY = "artifact-host-theme";
  var THEME_ORDER = ["system", "light", "dark"];
  var THEME_LABELS = {
    system: "Theme: System (click to switch to Light)",
    light: "Theme: Light (click to switch to Dark)",
    dark: "Theme: Dark (click to switch to System)"
  };

  function getStoredThemePref() {
    try {
      var pref = localStorage.getItem(THEME_STORAGE_KEY) || "system";
      if (pref === "light" || pref === "dark" || pref === "system") return pref;
    } catch (_) {}
    return "system";
  }

  function applyTheme(pref) {
    var mql = window.matchMedia ? window.matchMedia("(prefers-color-scheme: dark)") : null;
    var isDark = pref === "dark" || (pref === "system" && mql && mql.matches);
    var root = document.documentElement;
    root.setAttribute("data-theme-pref", pref);
    root.classList.toggle("dark", !!isDark);
    root.classList.toggle("light", !isDark);

    var label = THEME_LABELS[pref] || THEME_LABELS.system;
    document.querySelectorAll("[data-theme-toggle]").forEach(function (btn) {
      btn.setAttribute("title", label);
      btn.setAttribute("aria-label", label);
    });
  }

  function initThemeToggle() {
    applyTheme(getStoredThemePref());

    document.addEventListener("click", function (event) {
      var btn = event.target.closest("[data-theme-toggle]");
      if (!btn) return;
      event.preventDefault();
      var current = getStoredThemePref();
      var next = THEME_ORDER[(THEME_ORDER.indexOf(current) + 1) % THEME_ORDER.length];
      try {
        if (next === "system") {
          localStorage.removeItem(THEME_STORAGE_KEY);
        } else {
          localStorage.setItem(THEME_STORAGE_KEY, next);
        }
      } catch (_) {}
      applyTheme(next);
    });

    if (window.matchMedia) {
      var mql = window.matchMedia("(prefers-color-scheme: dark)");
      var onChange = function () {
        if (getStoredThemePref() === "system") {
          applyTheme("system");
        }
      };
      if (typeof mql.addEventListener === "function") {
        mql.addEventListener("change", onChange);
      } else if (typeof mql.addListener === "function") {
        mql.addListener(onChange);
      }
    }
  }

  document.addEventListener("DOMContentLoaded", function () {
    initThemeToggle();
    initCopyURLButtons();
    initVersionSelect();
    initKeyboardVersionNav();
    initPendingPreviews();
    initRefreshPreviewButtons();
    initDeleteAndRollbackButtons();
    initCopySubmissionButtons();
  });
})();
