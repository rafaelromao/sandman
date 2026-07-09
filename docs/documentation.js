(function () {
  "use strict";

  var REPO = "rafaelromao/sandman";
  var DEFAULT_REF = "HEAD";
  var DOCS_PREFIX = "docs/";
  var CACHE_KEY = "sandman-docs-tree-v1";
  var CACHE_TTL_MS = 5 * 60 * 1000;

  function api(path) {
    return fetch("https://api.github.com/repos/" + REPO + path, {
      headers: { "Accept": "application/vnd.github+json" },
    });
  }

  var GROUP_LABELS = {
    "root": "Documentation",
    "get-started": "Get Started",
    "usage": "Guides",
    "architecture": "Architecture",
    "help": "Help",
    "development": "Development",
  };

  var GROUP_ORDER = ["root", "get-started", "usage", "architecture", "help", "development"];

  var FALLBACK_FILES = [
    "README.md",
    "get-started/README.md",
    "get-started/overview.md",
    "get-started/quickstart.md",
    "get-started/install.md",
    "get-started/concepts.md",
    "usage/README.md",
    "usage/commands.md",
    "usage/configuration.md",
    "usage/workflows.md",
    "usage/portal.md",
    "usage/monitoring.md",
    "usage/skills.md",
    "usage/sandbox-modes.md",
    "usage/badge.md",
    "usage/default-task-prompt.md",
    "usage/agent-compatibility.md",
    "architecture/README.md",
    "architecture/overview.md",
    "architecture/disk-layout.md",
    "help/README.md",
    "help/troubleshooting.md",
    "help/faq.md",
    "help/positioning.md",
    "development/README.md",
    "development/testing.md",
  ];

  var sidebar = document.getElementById("sidebar");
  var sidebarToggle = document.getElementById("sidebar-toggle");
  var fileNav = document.getElementById("file-nav");
  var contentDiv = document.getElementById("content");
  var isMobile = window.matchMedia("(max-width: 768px)").matches;

  var activeRef = null;
  var refReady = discoverRef().then(function (r) { activeRef = r; writeCachedRef(r); return r; });

  // ── File discovery ──
  //
  // The sidebar rebuilds from the repository tree on every load. We keep a
  // short-TTL cache in localStorage and use GitHub's ETag so revalidation is
  // free when nothing changed. New markdown files added under docs/ become
  // visible automatically after the cache expires.
  //
  // Ref resolution order:
  //   1. `?ref=<branch>` query parameter (explicit override)
  //   2. Cached ref from the last successful tree fetch
  //   3. `HEAD` (the repository's default branch)
  // If HEAD returns a tree without any of the files we know exist locally
  // (because the page is served from a non-default branch preview), we
  // discover a better ref from the latest merged-into branches.

  function readOverrideRef() {
    var m = /[?&]ref=([^&]+)/.exec(location.search);
    return m ? decodeURIComponent(m[1]) : null;
  }

  function readCachedRef() {
    try {
      var raw = localStorage.getItem(CACHE_KEY + ":ref");
      if (!raw) return null;
      var payload = JSON.parse(raw);
      if (!payload || !payload.ref || !payload.timestamp) return null;
      if (Date.now() - payload.timestamp > CACHE_TTL_MS) return null;
      return payload.ref;
    } catch (e) {
      return null;
    }
  }

  function writeCachedRef(ref) {
    try {
      localStorage.setItem(CACHE_KEY + ":ref", JSON.stringify({
        timestamp: Date.now(),
        ref: ref,
      }));
    } catch (e) {}
  }

  function scoreBranch(name) {
    if (name === "main") return -1;
    if (/^landing-/.test(name)) return 5;
    if (/^feat(ure)?\//.test(name)) return 4;
    if (/^docs\//.test(name)) return 3;
    if (/^fix\//.test(name)) return 2;
    if (/^sandman\/[a-z]/.test(name)) return 1;
    return 0;
  }

  async function discoverRef() {
    var override = readOverrideRef();
    if (override) return override;

    var cachedRef = readCachedRef();
    if (cachedRef) return cachedRef;

    try {
      var resp = await api("/branches?per_page=30");
      if (!resp.ok) return DEFAULT_REF;
      var branches = await resp.json();
      var ranked = branches
        .map(function (b) { return b.name; })
        .sort(function (a, b) { return scoreBranch(b) - scoreBranch(a); });
      return ranked[0] || DEFAULT_REF;
    } catch (e) {
      return DEFAULT_REF;
    }
  }

  var treeListeners = [];
  var currentFiles = null;

  function getCachedTree() {
    try {
      var raw = localStorage.getItem(CACHE_KEY + ":" + activeRef);
      if (!raw) return null;
      return JSON.parse(raw);
    } catch (e) {
      return null;
    }
  }

  function setCachedTree(payload) {
    try {
      localStorage.setItem(CACHE_KEY + ":" + activeRef, JSON.stringify(payload));
    } catch (e) { /* ignore */ }
  }

  function treeUrl() {
    return "https://api.github.com/repos/" + REPO + "/git/trees/" + encodeURIComponent(activeRef) + "?recursive=1";
  }

  function parseTree(data) {
    return data.tree
      .filter(function (item) {
        return item.type === "blob" &&
          item.path.startsWith(DOCS_PREFIX) &&
          item.path.endsWith(".md") &&
          item.path.indexOf("landing-prototypes") === -1;
      })
      .map(function (item) { return item.path.substring(DOCS_PREFIX.length); })
      .sort();
  }

  function emitFiles(files) {
    var filtered = filterFiles(files);
    if (currentFiles && currentFiles.length === filtered.length &&
        currentFiles.every(function (f, i) { return f === filtered[i]; })) {
      return;
    }
    currentFiles = filtered;
    treeListeners.forEach(function (fn) { try { fn(filtered); } catch (e) {} });
  }

  function subscribeTree(fn) {
    treeListeners.push(fn);
    if (currentFiles) {
      try { fn(currentFiles); } catch (e) {}
    }
  }

  async function fetchTree() {
    var cached = getCachedTree();
    var headers = { "Accept": "application/vnd.github+json" };
    if (cached && cached.etag) headers["If-None-Match"] = cached.etag;

    var resp = await fetch(treeUrl(), { headers: headers });
    if (resp.status === 404) {
      try { localStorage.removeItem(CACHE_KEY + ":ref"); } catch (e) {}
      throw new Error("ref gone");
    }
    if (resp.status === 304 && cached && cached.files) {
      cached.timestamp = Date.now();
      setCachedTree(cached);
      emitFiles(cached.files);
      return cached.files;
    }
    if (!resp.ok) throw new Error("API " + resp.status);

    var data = await resp.json();
    var files = parseTree(data);
    setCachedTree({
      timestamp: Date.now(),
      etag: resp.headers.get("ETag"),
      sha: data.sha,
      files: files,
    });
    emitFiles(files);
    return files;
  }

  async function discoverFiles() {
    var cached = getCachedTree();

    if (cached && Date.now() - cached.timestamp < CACHE_TTL_MS && cached.files) {
      emitFiles(cached.files);
      fetchTree().catch(function () {});
      return filterFiles(cached.files);
    }

    try {
      return await fetchTree();
    } catch (e) {
      if (cached && cached.files) {
        emitFiles(cached.files);
        return filterFiles(cached.files);
      }
      try { localStorage.removeItem(CACHE_KEY + ":ref"); } catch (err) {}
      try {
        var resp = await api("/branches?per_page=30");
        if (resp.ok) {
          var branches = await resp.json();
          var ranked = branches
            .map(function (b) { return b.name; })
            .sort(function (a, b) { return scoreBranch(b) - scoreBranch(a); });
          for (var i = 0; i < ranked.length; i++) {
            activeRef = ranked[i];
            writeCachedRef(activeRef);
            try {
              return await fetchTree();
            } catch (err) { /* try next */ }
          }
        }
      } catch (err2) { /* fall through */ }
      var fb = filterFiles(FALLBACK_FILES);
      currentFiles = fb;
      return fb;
    }
  }

  function filterFiles(files) {
    return files.filter(function (f) {
      var top = f.split("/")[0];
      return top !== "adr" && top !== "agents" && top !== "landing-prototypes";
    });
  }

  // ── Title derivation ──

  function deriveTitle(path) {
    var name = path.split("/").pop().replace(/\.md$/, "");
    if (name === "README") {
      var dir = path.includes("/") ? path.split("/")[0] : "";
      if (!dir) return "Documentation";
      return dir.charAt(0).toUpperCase() + dir.slice(1) + " Overview";
    }
    return name.replace(/-/g, " ");
  }

  function titleRank(title) {
    var lower = title.toLowerCase();
    if (lower === "documentation") return 0;
    if (lower.indexOf("overview") !== -1) return 1;
    if (lower.indexOf("quick start") !== -1) return 2;
    if (lower.indexOf("installation") !== -1) return 3;
    if (lower.indexOf("concepts") !== -1) return 4;
    return 5;
  }

  // ── Sidebar building ──

  function buildSidebar(files) {
    var groups = {};
    files.forEach(function (f) {
      var dir = f.includes("/") ? f.split("/")[0] : "root";
      if (!groups[dir]) groups[dir] = [];
      groups[dir].push(f);
    });

    var html = "";
    var allDirs = Object.keys(groups).sort(function (a, b) {
      var ia = GROUP_ORDER.indexOf(a);
      var ib = GROUP_ORDER.indexOf(b);
      if (ia === -1) ia = 99;
      if (ib === -1) ib = 99;
      return ia - ib;
    });

    allDirs.forEach(function (dir) {
      var label = GROUP_LABELS[dir] || dir;
      var items = groups[dir].sort(function (a, b) {
        var ta = deriveTitle(a).toLowerCase();
        var tb = deriveTitle(b).toLowerCase();
        var ra = titleRank(ta), rb = titleRank(tb);
        if (ra !== rb) return ra - rb;
        return ta < tb ? -1 : 1;
      });

      html += '<div class="nav-group">';
      html += '<div class="nav-group-title">' + escapeHtml(label) + "</div>";
      items.forEach(function (f) {
        var title = deriveTitle(f);
        var display = title.charAt(0).toUpperCase() + title.slice(1);
        html +=
          '<a class="nav-item" data-file="' + escapeHtml(f) +
          '" href="#/' + escapeHtml(f) + '">' +
          escapeHtml(display) + "</a>";
      });
      html += "</div>";
    });

    fileNav.innerHTML = html;
  }

  // ── Markdown loading and rendering ──

  async   function loadFile(path) {
    contentDiv.innerHTML = '<p class="loading-msg">Loading documentation</p>';

    document.querySelectorAll(".nav-item").forEach(function (el) {
      el.classList.toggle("active", el.getAttribute("data-file") === path);
    });

    try {
      var resp = await fetch(path);
      if (!resp.ok) throw new Error("HTTP " + resp.status);
      var text = await resp.text();
      renderMarkdown(text, path);
    } catch (e) {
      contentDiv.innerHTML =
        '<div class="doc-blank">' +
        '<span class="doc-kicker">Not found</span>' +
        "<h1>Could not load this page.</h1>" +
        '<p>The file <code>' + escapeHtml(path) +
        "</code> is unreachable. Pick a document from the sidebar or return to the landing page.</p>" +
        "</div>";
    }
  }

  function renderMarkdown(text, path) {
    var firstH1Match = text.match(/^#\s+(.+)$/m);
    var bodyHtml = marked.parse(text);

    if (firstH1Match) {
      var title = firstH1Match[1];
      document.title = title + " - Sandman Docs";
      bodyHtml = bodyHtml.replace(/^<h1[^>]*>.*?<\/h1>/, "");

      var dir = path.includes("/") ? path.split("/")[0] : "";
      var groupLabel = GROUP_LABELS[dir] || dir;
      var metaHtml = '<div class="doc-meta">' + escapeHtml(groupLabel) + '</div>';

      contentDiv.innerHTML = "<h1>" + escapeHtml(title) + "</h1>" + metaHtml + bodyHtml;
    } else {
      contentDiv.innerHTML = bodyHtml;
    }

    interceptLinks(path);
    window.scrollTo(0, 0);
  }

  // ── Link interception ──

  function interceptLinks(currentPath) {
    var currentDir = currentPath.includes("/")
      ? currentPath.substring(0, currentPath.lastIndexOf("/"))
      : "";

    contentDiv.querySelectorAll("a[href]").forEach(function (a) {
      var href = a.getAttribute("href");
      if (!href || href.startsWith("http") || href.startsWith("#") || href.startsWith("mailto:")) {
        return;
      }

      if (href.endsWith(".md")) {
        a.addEventListener("click", function (e) {
          e.preventDefault();
          var resolved = resolvePath(href, currentDir);
          location.hash = "#/" + resolved;
        });
      }
    });
  }

  function resolvePath(href, currentDir) {
    if (href.startsWith("/")) return href.substring(1);

    var parts = (currentDir ? currentDir + "/" : "") + href;
    var resolved = [];
    parts.split("/").forEach(function (seg) {
      if (seg === "..") resolved.pop();
      else if (seg !== "." && seg !== "") resolved.push(seg);
    });
    return resolved.join("/");
  }

  // ── Routing ──

  function handleHashChange() {
    var hash = location.hash;
    if (hash.startsWith("#/")) {
      var path = hash.substring(2);
      if (path) {
        loadFile(path);
        return;
      }
    }

    var firstItem = document.querySelector(".nav-item");
    if (firstItem) {
      loadFile(firstItem.getAttribute("data-file"));
    } else {
      contentDiv.innerHTML =
        '<div class="doc-blank">' +
        '<span class="doc-kicker">Documentation</span>' +
        '<h1>Sandman Documentation</h1>' +
        '<p>Select a document from the sidebar to begin. The same articles power the in-repo <code>docs/</code> folder.</p>' +
        "</div>";
    }
  }

  // ── Sidebar toggle ──

  sidebarToggle.addEventListener("click", function (e) {
    e.stopPropagation();
    if (isMobile) {
      sidebar.classList.toggle("mobile-open");
    } else {
      sidebar.classList.toggle("collapsed");
      var area = document.getElementById("content-area");
      area.style.marginLeft = sidebar.classList.contains("collapsed") ? "0" : "";
    }
  });

  var refreshBtn = null;

  if (isMobile) {
    document.addEventListener("click", function (e) {
      if (!sidebar.contains(e.target) && !sidebarToggle.contains(e.target)) {
        sidebar.classList.remove("mobile-open");
      }
    });
  }

  window.matchMedia("(max-width: 768px)").addEventListener("change", function (e) {
    isMobile = e.matches;
    if (!isMobile) sidebar.classList.remove("mobile-open");
  });

  // ── Utils ──

  function escapeHtml(str) {
    var div = document.createElement("div");
    div.textContent = str;
    return div.innerHTML;
  }

  // ── Init ──

  window.addEventListener("hashchange", handleHashChange);

  subscribeTree(function (files) {
    buildSidebar(files);
    var active = document.querySelector(".nav-item.active");
    if (!active) {
      var first = document.querySelector(".nav-item");
      if (first && !location.hash.startsWith("#/")) {
        location.hash = "#/" + first.getAttribute("data-file");
      }
    }
  });

  (async function init() {
    try {
      await refReady;
      var files = await discoverFiles();
      buildSidebar(files);
    } catch (e) {
      fileNav.innerHTML = '<div class="nav-group-title">Could not load docs</div>';
    }
    handleHashChange();
  })();
})();
