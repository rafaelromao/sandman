(function () {
  "use strict";

  var REPO = "rafaelromao/sandman";
  var BRANCH = "main";
  var DOCS_PREFIX = "docs/";

  var GROUP_LABELS = {
    "usage": "Guides",
    "architecture": "Architecture",
    "agents": "Agent Guidelines",
  };

  var GROUP_ORDER = ["usage", "architecture", "agents"];

  var FALLBACK_FILES = [
    "usage/getting-started.md",
    "usage/commands.md",
    "usage/configuration.md",
    "usage/workflows.md",
    "usage/portal.md",
    "usage/monitoring.md",
    "usage/skills.md",
    "usage/sandbox-modes.md",
    "usage/testing.md",
    "usage/badge.md",
    "usage/default-task-prompt.md",
    "usage/agent-compatibility.md",
    "architecture/disk-layout.md",
    "agents/codeindex-strategy.md",
    "agents/domain.md",
    "agents/issue-tracker.md",
    "agents/portal-layout.md",
    "agents/quality-rules.md",
    "agents/testenv.md",
    "agents/triage-labels.md",
  ];

  var sidebar = document.getElementById("sidebar");
  var sidebarToggle = document.getElementById("sidebar-toggle");
  var fileNav = document.getElementById("file-nav");
  var contentDiv = document.getElementById("content");
  var isMobile = window.matchMedia("(max-width: 768px)").matches;

  // ── File discovery ──

  function getCachedTree() {
    try {
      var raw = localStorage.getItem("sandman-docs-tree");
      if (!raw) return null;
      return JSON.parse(raw);
    } catch (e) {
      return null;
    }
  }

  function setCachedTree(files) {
    try {
      localStorage.setItem("sandman-docs-tree", JSON.stringify({
        timestamp: Date.now(),
        files: files,
      }));
    } catch (e) { /* ignore */ }
  }

  async function discoverFiles() {
    var cached = getCachedTree();
    var cacheMaxAge = 3600000;

    if (cached && Date.now() - cached.timestamp < cacheMaxAge) {
      return filterFiles(cached.files);
    }

    try {
      var resp = await fetch(
        "https://api.github.com/repos/" + REPO + "/git/trees/" + BRANCH + "?recursive=1"
      );
      if (!resp.ok) throw new Error("API " + resp.status);
      var data = await resp.json();
      var files = data.tree
        .filter(function (item) {
          return item.type === "blob" &&
            item.path.startsWith(DOCS_PREFIX) &&
            item.path.endsWith(".md") &&
            item.path.indexOf("landing-prototypes") === -1;
        })
        .map(function (item) { return item.path.substring(DOCS_PREFIX.length); })
        .sort();

      setCachedTree(files);
      return filterFiles(files);
    } catch (e) {
      if (cached && cached.files) return filterFiles(cached.files);
      return filterFiles(FALLBACK_FILES);
    }
  }

  function filterFiles(files) {
    return files.filter(function (f) {
      return f.indexOf("adr/") === -1;
    });
  }

  // ── Title derivation ──

  function deriveTitle(path) {
    var name = path.split("/").pop().replace(/\.md$/, "");
    if (name === "README") {
      var dir = path.includes("/") ? path.split("/")[0] : "";
      return dir.charAt(0).toUpperCase() + dir.slice(1) + " Overview";
    }
    return name.replace(/-/g, " ");
  }

  function titleRank(title) {
    var lower = title.toLowerCase();
    if (lower.indexOf("getting started") !== -1) return 0;
    if (lower.indexOf("overview") !== -1) return 1;
    return 2;
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

  async function loadFile(path) {
    contentDiv.innerHTML = '<p class="loading-msg">Loading...</p>';

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
        '<h1>Not found</h1><p>Could not load <code>' + escapeHtml(path) +
        "</code>.</p>";
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
      contentDiv.innerHTML = "<h1>Sandman Documentation</h1><p>Select a document from the sidebar.</p>";
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

  (async function init() {
    var files = await discoverFiles();
    buildSidebar(files);
    handleHashChange();
  })();
})();
