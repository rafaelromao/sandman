(function () {
  "use strict";

  var REPO = "rafaelromao/sandman";
  var DEFAULT_REF = "HEAD";
  var DOCS_PREFIX = "docs/";
  var CACHE_KEY = "sandman-docs-tree-v3";
  var CACHE_TTL_MS = 60 * 1000;

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
    "get-started/get-started-overview.md",
    "get-started/quickstart.md",
    "get-started/install.md",
    "get-started/concepts.md",
    "usage/README.md",
    "usage/commands.md",
    "usage/scaffolding.md",
    "usage/configuration.md",
    "usage/workflows.md",
    "usage/reviews.md",
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
    "development/local-development-setup.md",
    "development/project-structure.md",
    "development/architecture-guidelines.md",
    "development/testing.md",
    "development/test-infrastructure.md",
    "development/docs-and-embedded-skills.md",
  ];

  var sidebar = document.getElementById("sidebar");
  var sidebarToggle = document.getElementById("sidebar-toggle");
  var fileNav = document.getElementById("file-nav");
  var contentDiv = document.getElementById("content");
  var isMobile = window.matchMedia("(max-width: 768px)").matches;

  var activeRef = null;
  var refReady = discoverRef().then(function (r) { activeRef = r; writeCachedRef(r); return r; });

  function isLocalPreview() {
    return location.hostname === "localhost" ||
      location.hostname === "127.0.0.1" ||
      location.hostname === "0.0.0.0";
  }

  function purgeStaleCacheKeys() {
    try {
      var legacy = ["sandman-docs-tree-v1", "sandman-docs-tree-v2"];
      var toRemove = [];
      for (var i = 0; i < localStorage.length; i++) {
        var k = localStorage.key(i);
        if (!k) continue;
        for (var j = 0; j < legacy.length; j++) {
          if (k === legacy[j] || k.indexOf(legacy[j] + ":") === 0) {
            toRemove.push(k);
          }
        }
      }
      for (var i2 = 0; i2 < toRemove.length; i2++) {
        localStorage.removeItem(toRemove[i2]);
      }
    } catch (err) { /* ignore */ }
  }
  purgeStaleCacheKeys();

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
    if (isLocalPreview()) {
      var localFiles = filterFiles(FALLBACK_FILES);
      emitFiles(localFiles);
      return localFiles;
    }

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
      return top !== "adr" && top !== "landing-prototypes";
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
    return name.split("-").map(function (w) { return w.charAt(0).toUpperCase() + w.slice(1); }).join(" ");
  }

  function titleRank(title) {
    var lower = title.toLowerCase();
    if (lower === "documentation") return 0;
    if (lower.indexOf("overview") !== -1) return 1;
    if (lower.indexOf("quick start") !== -1) return 2;
    if (lower.indexOf("installation") !== -1) return 3;
    if (lower.indexOf("concepts") !== -1) return 4;
    if (lower.indexOf("commands") !== -1) return 6;
    if (lower.indexOf("scaffolding") !== -1) return 7;
    if (lower.indexOf("configuration") !== -1) return 8;
    if (lower.indexOf("workflows") !== -1) return 9;
    if (lower === "reviews") return 10;
    if (lower.indexOf("sandbox") !== -1) return 11;
    if (lower.indexOf("agent compatibility") !== -1) return 12;
    if (lower.indexOf("portal") !== -1) return 13;
    if (lower.indexOf("monitoring") !== -1) return 14;
    if (lower.indexOf("skills") !== -1) return 15;
    if (lower.indexOf("default task prompt") !== -1) return 16;
    if (lower === "badge") return 17;
    if (lower.indexOf("local development setup") !== -1) return 6;
    if (lower.indexOf("project structure") !== -1) return 7;
    if (lower.indexOf("architecture guidelines") !== -1) return 8;
    if (lower === "testing") return 9;
    if (lower.indexOf("test infrastructure") !== -1) return 10;
    if (lower.indexOf("docs and embedded skills") !== -1) return 11;
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

    annotateCodeBlocks();
    interceptLinks(path);
    window.scrollTo(0, 0);
  }

  function annotateCodeBlocks() {
    contentDiv.querySelectorAll("pre > code").forEach(function (code) {
      var pre = code.parentElement;
      var frame = pre.parentElement;
      if (!frame.classList.contains("code-frame")) {
        var frame = document.createElement("div");
        frame.className = "code-frame";
        pre.parentElement.insertBefore(frame, pre);
        frame.appendChild(pre);
      }
      var lang = languageForCodeBlock(code) || sniffCodeLanguage(code);
      pre.classList.add("code-panel");
      if (lang) {
        pre.setAttribute("data-lang", lang);
      }
      if (isTerminalLanguage(lang)) {
        pre.classList.add("terminal-panel");
        frame.classList.add("terminal-frame");
      }
      code.innerHTML = highlightCode(code.textContent, lang);
    });
  }

  function languageForCodeBlock(code) {
    var className = code.className || "";
    var m = /(?:^|\s)language-([^\s]+)/.exec(className);
    return m ? m[1].toLowerCase() : "";
  }

  function sniffCodeLanguage(code) {
    var text = code.textContent || "";
    if (!text) return "";
    if (/^\s*[\{\[]/.test(text) && /[}\]]\s*$/.test(text)) return "json";
    if (/^\s*\w[\w-]*\s*:\s+\S/m.test(text) && /:\s*$/.test(text)) return "yaml";
    if (/^( {0,3}#{1,6} | {0,3}> | {0,3}([-*+]|\d+\.) )/m.test(text)) return "markdown";
    return "";
  }

  function isTerminalLanguage(lang) {
    return lang === "bash" || lang === "sh" || lang === "shell" || lang === "console" || lang === "text";
  }

  function highlightCode(text, lang) {
    var html = escapeHtml(text);
    var stash = [];
    function keep(re, cls) {
      html = html.replace(re, function (match) {
        var key = stashKey(stash.length);
        stash.push('<span class="tok-' + cls + '">' + match + "</span>");
        return key;
      });
    }
    function restore() {
      html = restoreStash(html, stash);
    }

    if (lang === "json") {
      return highlightJSON(text);
    }

    if (lang === "yaml" || lang === "yml") {
      keep(/#.*/g, "comment");
      keep(/(&quot;[^&]*?&quot;|'[^']*?')/g, "string");
      html = html.replace(/^([\w.-]+)(:)/gm, '<span class="tok-key">$1</span>$2');
      html = html.replace(/\b(true|false|null|podman|docker|worktree)\b/g, '<span class="tok-keyword">$1</span>');
      html = html.replace(/\b(-?\d+(?:\.\d+)?)\b/g, '<span class="tok-number">$1</span>');
      restore();
      return html;
    }

    if (lang === "go") {
      keep(/\/\/.*|\/\*[\s\S]*?\*\//g, "comment");
      keep(/`[\s\S]*?`|&quot;[^&]*?&quot;|'[^']*?'/g, "string");
      html = html.replace(/\b(func|type|struct|interface|package|import|return|if|else|for|range|var|const|map|chan|defer|go|select|case|default)\b/g, '<span class="tok-keyword">$1</span>');
      html = html.replace(/\b(-?\d+(?:\.\d+)?)\b/g, '<span class="tok-number">$1</span>');
      restore();
      return html;
    }

    if (lang === "markdown" || lang === "md") {
      return highlightMarkdown(text);
    }

    if (isTerminalLanguage(lang)) {
      return highlightTerminal(text);
    }

    keep(/(&quot;[^&]*?&quot;|'[^']*?')/g, "string");
    html = html.replace(/\b(true|false|null|nil|default|latest|lts|repo)\b/g, '<span class="tok-keyword">$1</span>');
    html = html.replace(/\b(-?\d+(?:\.\d+)?)\b/g, '<span class="tok-number">$1</span>');
    restore();
    return html;
  }

  function highlightJSON(text) {
    var out = "";
    var token = /"(?:\\.|[^"\\])*"(?=\s*:)|"(?:\\.|[^"\\])*"|\b(?:true|false|null)\b|-?\b\d+(?:\.\d+)?(?:[eE][+-]?\d+)?\b/g;
    var last = 0;
    text.replace(token, function (match, offset) {
      out += escapeHtml(text.substring(last, offset));
      var next = text.substring(offset + match.length).match(/^\s*:/);
      var cls = "number";
      if (match[0] === '"') {
        cls = next ? "key" : "string";
      } else if (/^(true|false|null)$/.test(match)) {
        cls = "keyword";
      }
      out += '<span class="tok-' + cls + '">' + escapeHtml(match) + "</span>";
      last = offset + match.length;
      return match;
    });
    out += escapeHtml(text.substring(last));
    return out;
  }

  function highlightMarkdown(text) {
    return text.split("\n").map(highlightMarkdownLine).join("\n");
  }

  function highlightMarkdownLine(line) {
    if (/^\s*$/.test(line)) return line;

    var heading = /^( {0,3})(#{1,6})(\s+)(.*)$/.exec(line);
    if (heading) {
      return escapeHtml(heading[1]) +
        '<span class="tok-heading">' + escapeHtml(heading[2]) + "</span>" +
        escapeHtml(heading[3]) +
        highlightMarkdownInline(heading[4]);
    }

    var blockquote = /^( {0,3})>(.*)$/.exec(line);
    if (blockquote) {
      return escapeHtml(blockquote[1]) +
        '<span class="tok-prompt">&gt;</span>' +
        highlightMarkdownInline(blockquote[2]);
    }

    var hr = /^( {0,3})(?:[-*_][ \t]*){3,}$/.exec(line);
    if (hr) {
      return '<span class="tok-muted">' + escapeHtml(line) + "</span>";
    }

    var listItem = /^( {0,3})((?:[-*+]|\d+\.))(\s+)(.*)$/.exec(line);
    if (listItem) {
      return escapeHtml(listItem[1]) +
        '<span class="tok-flag">' + escapeHtml(listItem[2]) + "</span>" +
        escapeHtml(listItem[3]) +
        highlightMarkdownInline(listItem[4]);
    }

    return highlightMarkdownInline(line);
  }

  function highlightMarkdownInline(text) {
    var result = "";
    var i = 0;
    while (i < text.length) {
      var ch = text.charAt(i);
      var next = i + 1 < text.length ? text.charAt(i + 1) : "";

      if (ch === "`") {
        var runEnd = i;
        while (runEnd < text.length && text.charAt(runEnd) === "`") runEnd++;
        var backticks = text.substring(i, runEnd);
        var closeStart = text.indexOf(backticks, runEnd);
        if (closeStart > runEnd) {
          result += '<span class="tok-code">' + escapeHtml(text.substring(i, closeStart + backticks.length)) + "</span>";
          i = closeStart + backticks.length;
          continue;
        }
      }

      if (ch === "!" && next === "[") {
        var imgRange = findMarkdownLink(text, i + 1);
        if (imgRange) {
          var imgSrc = text.substring(imgRange.labelStart + 1, imgRange.labelEnd);
          var imgUrl = text.substring(imgRange.urlStart, imgRange.urlEnd);
          result +=
            '<span class="tok-image">' +
            '<span class="tok-punct">!</span>' +
            '<span class="tok-link-text">[' + escapeHtml(imgSrc) + "]</span>" +
            '<span class="tok-punct">(</span>' +
            '<span class="tok-link-url">' + escapeHtml(imgUrl) + "</span>" +
            '<span class="tok-punct">)</span>' +
            "</span>";
          i = imgRange.urlEnd + 1;
          continue;
        }
      }

      if (ch === "[") {
        var linkRange = findMarkdownLink(text, i);
        if (linkRange) {
          var labelText = text.substring(linkRange.labelStart + 1, linkRange.labelEnd);
          var urlText = text.substring(linkRange.urlStart, linkRange.urlEnd);
          result +=
            '<span class="tok-link-text">[' + escapeHtml(labelText) + ']</span>' +
            '<span class="tok-punct">(</span>' +
            '<span class="tok-link-url">' + escapeHtml(urlText) + "</span>" +
            '<span class="tok-punct">)</span>';
          i = linkRange.urlEnd + 1;
          continue;
        }
      }

      if (ch === "<") {
        var gt = text.indexOf(">", i + 1);
        if (gt > 0) {
          var inner = text.substring(i + 1, gt);
          if (/^[A-Za-z][A-Za-z0-9+.\-]*:[^\s>]*$/.test(inner) || /^[^\s>]+@[^\s>]+$/.test(inner)) {
            result +=
              '<span class="tok-punct">&lt;</span>' +
              '<span class="tok-link-url">' + escapeHtml(inner) + "</span>" +
              '<span class="tok-punct">&gt;</span>';
            i = gt + 1;
            continue;
          }
        }
      }

      if ((ch === "*" || ch === "_") && next === ch) {
        var endRun = findCloserPair(text, i + 2, ch + ch);
        if (endRun > 0) {
          var innerRun = text.substring(i + 2, endRun);
          result +=
            '<span class="tok-strong">' +
            escapeHtml(ch + ch) +
            highlightMarkdownInline(innerRun) +
            escapeHtml(ch + ch) +
            "</span>";
          i = endRun + 2;
          continue;
        }
      }

      if (ch === "*" || ch === "_") {
        var endSingle = findCloserSingle(text, i + 1, ch);
        if (endSingle > 0 && isMarkdownItalicBoundary(text, i, endSingle)) {
          var innerSingle = text.substring(i + 1, endSingle);
          result +=
            '<span class="tok-emphasis">' +
            escapeHtml(ch) +
            highlightMarkdownInline(innerSingle) +
            escapeHtml(ch) +
            "</span>";
          i = endSingle + 1;
          continue;
        }
      }

      if (ch === "\\" && i + 1 < text.length) {
        result += escapeHtml(text.substring(i, i + 2));
        i += 2;
        continue;
      }

      result += escapeHtml(ch);
      i += 1;
    }
    return result;
  }

  function findMarkdownLink(text, start) {
    if (text.charAt(start) !== "[") return null;
    var depth = 0;
    var i = start;
    while (i < text.length) {
      var ch = text.charAt(i);
      if (ch === "\\" && i + 1 < text.length) { i += 2; continue; }
      if (ch === "[") depth++;
      else if (ch === "]") { depth--; if (depth === 0) break; }
      i++;
    }
    if (depth !== 0) return null;
    var labelEnd = i;
    if (text.charAt(labelEnd) !== "]") return null;
    if (text.charAt(labelEnd + 1) !== "(") return null;
    var urlStart = labelEnd + 2;
    var urlEnd = text.indexOf(")", urlStart);
    if (urlEnd < 0) return null;
    return { labelStart: start, labelEnd: labelEnd, urlStart: urlStart, urlEnd: urlEnd };
  }

  function findCloserPair(text, start, pair) {
    var i = start;
    while (i + 1 < text.length) {
      if (text.charAt(i) === pair.charAt(0) && text.charAt(i + 1) === pair.charAt(1)) {
        if (i > start) return i;
      }
      i++;
    }
    return -1;
  }

  function findCloserSingle(text, start, marker) {
    var i = start;
    while (i < text.length) {
      if (text.charAt(i) === marker) {
        if (i > start) return i;
      }
      i++;
    }
    return -1;
  }

  function isMarkdownItalicBoundary(text, openIdx, closeIdx) {
    var before = openIdx > 0 ? text.charAt(openIdx - 1) : "";
    var after = closeIdx + 1 < text.length ? text.charAt(closeIdx + 1) : "";
    var openChar = text.charAt(openIdx);
    var closeChar = text.charAt(closeIdx);
    if (before && /[A-Za-z0-9]/.test(before) && /[A-Za-z0-9]/.test(closeChar)) return false;
    if (after && /[A-Za-z0-9]/.test(after) && /[A-Za-z0-9]/.test(openChar)) return false;
    if (/[A-Za-z0-9]/.test(before) || /[A-Za-z0-9]/.test(after)) return false;
    return true;
  }

  function highlightTerminal(text) {
    return text.split("\n").map(function (line) {
      var promptMatch = /^(\s*)([$#>])\s+/.exec(line);
      var prefix = "";
      var rest = line;
      if (promptMatch) {
        prefix = escapeHtml(promptMatch[1]) + '<span class="tok-prompt">' + escapeHtml(promptMatch[2]) + "</span> ";
        rest = line.substring(promptMatch[0].length);
      }

      var trimmed = rest.trimStart();
      var leading = rest.substring(0, rest.length - trimmed.length);
      if (trimmed.startsWith("#")) {
        return prefix + escapeHtml(leading) + '<span class="tok-comment">' + escapeHtml(trimmed) + "</span>";
      }

      var commandMatch = /^((?:[A-Za-z_][A-Za-z0-9_]*=(?:"[^"]*"|'[^']*'|\S+)\s+)*)((?:\.\/)?[A-Za-z0-9_.-]+)/.exec(trimmed);
      if (!commandMatch) {
        return prefix + highlightTerminalRemainder(rest);
      }

      var assignments = commandMatch[1] || "";
      var command = commandMatch[2];
      var after = trimmed.substring(commandMatch[0].length);
      return prefix +
        escapeHtml(leading) +
        highlightAssignments(assignments) +
        '<span class="tok-command">' + escapeHtml(command) + "</span>" +
        highlightTerminalRemainder(after);
    }).join("\n");
  }

  function highlightAssignments(text) {
    if (!text) return "";
    return escapeHtml(text).replace(/\b([A-Za-z_][A-Za-z0-9_]*)(=)/g, '<span class="tok-key">$1</span>$2');
  }

  function highlightTerminalRemainder(text) {
    var html = escapeHtml(text);
    var stash = [];
    html = html.replace(/(&quot;[^&]*?&quot;|'[^']*?')/g, function (match) {
      var key = stashKey(stash.length);
      stash.push('<span class="tok-string">' + match + "</span>");
      return key;
    });
    html = html.replace(/(\s)(#[^\n]*)/g, function (_, ws, comment) {
      return ws + '<span class="tok-comment">' + comment + "</span>";
    });
    html = html.replace(/(^|\s)(--?[A-Za-z0-9][\w-]*)/g, '$1<span class="tok-flag">$2</span>');
    return restoreStash(html, stash);
  }

  function stashKey(index) {
    var alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ";
    var value = "";
    do {
      value = alphabet[index % alphabet.length] + value;
      index = Math.floor(index / alphabet.length) - 1;
    } while (index >= 0);
    return "\uE000" + value + "\uE001";
  }

  function restoreStash(html, stash) {
    return html.replace(/\uE000([A-Za-z]+)\uE001/g, function (_, key) {
      var alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ";
      var index = 0;
      for (var i = 0; i < key.length; i++) {
        index = index * alphabet.length + alphabet.indexOf(key[i]) + 1;
      }
      return stash[index - 1] || "";
    });
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
