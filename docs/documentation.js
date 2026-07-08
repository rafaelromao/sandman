(function () {
  "use strict";

  var REPO = "rafaelromao/sandman";
  var BRANCH = "main";
  var DOCS_PREFIX = "docs/";

  var GROUP_LABELS = {
    "usage": "Usage",
    "adr": "Architecture Decision Records",
    "agents": "Agent Guidelines",
    "architecture": "Architecture",
  };

  var GROUP_ORDER = ["usage", "architecture", "agents", "adr"];

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
    "adr/README.md",
    "adr/0000-use-adr-template.md",
    "adr/0001-remove-pr-creation-from-agent-workflow.md",
    "adr/0002-make-shared-container-the-default-sandbox.md",
    "adr/0003-dependency-aware-batch-execution.md",
    "adr/0004-use-rest-gh-api-for-native-dependencies.md",
    "adr/0005-replace-isolated-container-toggle-with-container-capacity.md",
    "adr/0006-built-in-agent-presets-and-claude-code-naming.md",
    "adr/0007-buildtoolspreset-and-pinned-init-scaffolding.md",
    "adr/0008-config-mount-resolution-via-temp-copy.md",
    "adr/0009-stabilize-container-backed-smoke-and-e2e-tests-postmortem.md",
    "adr/0010-local-portal-command-and-repo-scoped-run-scan.md",
    "adr/0011-remove-interactive-agent-mode.md",
    "adr/0012-ralph-loop-agent-driven-issue-selection.md",
    "adr/0013-rename-delegate-review-to-pr-review.md",
    "adr/0014-sandman-review-daemon-and-guard.md",
    "adr/0015-store-container-config-snapshots-under-run-dir.md",
    "adr/0016-split-opencode-config-snapshot-from-mutable-state.md",
    "adr/0017-split-pi-config-snapshot-from-mutable-state.md",
    "adr/0018-unblock-dependents-from-same-batch-success.md",
    "adr/0019-canonical-test-env-vars-for-provider-allowlists-and-e2e-gates.md",
    "adr/0020-per-agent-env-vars-to-parameterize-the-model-used-by-smoke-and-e2e-tests.md",
    "adr/0021-portal-auto-runs-clean-stale-on-startup.md",
    "adr/0022-replace-end-of-session-continuation-with-checkpointed-handoffs.md",
    "adr/0023-handoff-points-to-rendered-prompt-and-captures-last-skill.md",
    "adr/0024-remove-pi-agent-support.md",
    "adr/0025-prd-expansion-to-child-issues.md",
    "adr/0026-rename-ralph-to-auto-mode.md",
    "adr/0027-reconcile-stranded-worktrees-auto-recovery.md",
    "adr/0028-pr-review-prompt-omit-previous-review-progress.md",
    "adr/0029-portal-secondary-row-chips-for-run-context.md",
    "adr/0030-standardize-run-id-and-run-dir.md",
    "adr/0031-portal-read-only-commands-panel-removed.md",
    "adr/0032-sandman-layout-redesign.md",
    "adr/0033-per-run-sockets-for-command-abort.md",
    "adr/0034-review-daemon-stateless-on-age-stateful-on-comment.md",
    "adr/0035-run-retry-payload-schema-and-reason-vocabulary.md",
    "adr/0036-batches-index-entry-id-equals-per-row-run-id.md",
    "adr/0037-hermetic-gh-in-prflow-e2e.md",
    "adr/0038-badge-marker-pagination.md",
  ];

  var sidebar = document.getElementById("sidebar");
  var sidebarToggle = document.getElementById("sidebar-toggle");
  var fileNav = document.getElementById("file-nav");
  var contentDiv = document.getElementById("content");
  var tocNav = document.getElementById("toc");
  var isMobile = window.matchMedia("(max-width: 768px)").matches;

  // ── File discovery ──

  function getCachedTree() {
    try {
      var raw = localStorage.getItem("sandman-docs-tree");
      if (!raw) return null;
      var parsed = JSON.parse(raw);
      return parsed;
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
    var cacheMaxAge = 3600000; // 1 hour

    if (cached && (Date.now() - cached.timestamp) < cacheMaxAge) {
      return cached.files;
    }

    try {
      var resp = await fetch(
        "https://api.github.com/repos/" + REPO + "/git/trees/" + BRANCH + "?recursive=1"
      );
      if (!resp.ok) throw new Error("API error " + resp.status);
      var data = await resp.json();
      var files = data.tree
        .filter(function (item) {
          return (
            item.type === "blob" &&
            item.path.startsWith(DOCS_PREFIX) &&
            item.path.endsWith(".md") &&
            item.path.indexOf("landing-prototypes") === -1
          );
        })
        .map(function (item) {
          return item.path.substring(DOCS_PREFIX.length);
        })
        .sort();

      setCachedTree(files);
      return files;
    } catch (e) {
      if (cached && cached.files) return cached.files;
      return FALLBACK_FILES;
    }
  }

  // ── Title derivation ──

  function deriveTitle(path) {
    var name = path.split("/").pop().replace(/\.md$/, "");
    if (name === "README") {
      var dir = path.includes("/") ? path.split("/")[0] : "";
      return dir.charAt(0).toUpperCase() + dir.slice(1) + " Overview";
    }
    if (/^\d{4}/.test(name)) {
      return name.substring(5).replace(/-/g, " ");
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
        if (dir === "adr" && a !== "adr/README.md" && b !== "adr/README.md") {
          return a < b ? -1 : 1;
        }
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
          '<a class="nav-item" data-file="' +
          escapeHtml(f) +
          '" href="#/' +
          escapeHtml(f) +
          '">' +
          escapeHtml(display) +
          "</a>";
      });
      html += "</div>";
    });

    fileNav.innerHTML = html;

    fileNav.querySelectorAll(".nav-item").forEach(function (el) {
      el.addEventListener("click", function (e) {
        if (isMobile) sidebar.classList.remove("mobile-open");
      });
    });
  }

  // ── Markdown loading and rendering ──

  async function loadFile(path) {
    contentDiv.innerHTML = '<p class="loading-msg">Loading...</p>';
    tocNav.innerHTML = "";

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
        '<p class="loading-msg">Could not load ' +
        escapeHtml(path) +
        ". " +
        escapeHtml(e.message) +
        "</p>";
    }
  }

  function renderMarkdown(text, path) {
    contentDiv.innerHTML = marked.parse(text);

    var firstH1 = contentDiv.querySelector("h1");
    if (firstH1) {
      document.title = firstH1.textContent + " - Sandman";
    }

    var headings = contentDiv.querySelectorAll("h1, h2, h3");
    var tocHtml = "";
    var usedIds = {};

    headings.forEach(function (h) {
      var slug = slugify(h.textContent);
      if (usedIds[slug]) {
        usedIds[slug]++;
        slug = slug + "-" + usedIds[slug];
      } else {
        usedIds[slug] = 1;
      }
      h.id = slug;

      var level = parseInt(h.tagName.substring(1), 10);
      var cls = level === 1 ? "" : " level-" + level;
      if (level === 1) return;

      tocHtml +=
        '<li><a class="' +
        cls.trim() +
        '" href="#' +
        slug +
        '">' +
        escapeHtml(h.textContent) +
        "</a></li>";
    });

    tocNav.innerHTML = tocHtml ? "<ul>" + tocHtml + "</ul>" : "";

    tocNav.querySelectorAll("a").forEach(function (a) {
      a.addEventListener("click", function (e) {
        e.preventDefault();
        var target = document.getElementById(this.getAttribute("href").substring(1));
        if (target) {
          var offset = target.getBoundingClientRect().top + window.pageYOffset - 70;
          window.scrollTo({ top: offset, behavior: "smooth" });
        }
      });
    });

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
      } else if (href.startsWith("../") || (!href.startsWith("http") && href.includes("/"))) {
        a.addEventListener("click", function (e) {
          e.preventDefault();
          var resolved = resolvePath(href, currentDir);
          window.open(resolved, "_blank");
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
        '<h1>Sandman Documentation</h1><p>Select a document from the sidebar.</p>';
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
      if (sidebar.classList.contains("collapsed")) {
        area.style.marginLeft = "0";
      } else {
        area.style.marginLeft = "";
      }
    }
  });

  if (!isMobile) {
    document.addEventListener("click", function (e) {
      if (
        !sidebar.contains(e.target) &&
        !sidebarToggle.contains(e.target) &&
        !sidebar.classList.contains("collapsed")
      ) {
        sidebar.classList.add("collapsed");
        document.getElementById("content-area").style.marginLeft = "0";
      }
    });
  }

  window.matchMedia("(max-width: 768px)").addEventListener("change", function (e) {
    isMobile = e.matches;
    if (!isMobile) {
      sidebar.classList.remove("mobile-open");
    }
  });

  // ── Utils ──

  function slugify(text) {
    return text
      .toLowerCase()
      .trim()
      .replace(/[^\w\s-]/g, "")
      .replace(/[\s_]+/g, "-")
      .replace(/-+/g, "-")
      .replace(/^-|-$/g, "");
  }

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
