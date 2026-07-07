package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var permittedLogPath = regexp.MustCompile(`^batches/[^/]+/runs/[^/]+/run\.log$|^archive/[^/]+/runs/[^/]+/run\.log$`)

type portalHandler struct {
	repoRoot     string
	runsIndex    *portalRunsIndex
	staleCleaner func(string) error
}

func newPortalHandler(repoRoot string) http.Handler {
	h := &portalHandler{
		repoRoot:     repoRoot,
		runsIndex:    getPortalRunsIndex(repoRoot),
		staleCleaner: portalStaleCleaner,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/instances", h.handleInstances)
	mux.HandleFunc("/api/runs", h.handleRuns)
	mux.HandleFunc("/api/runs/stream", h.handleRunStream)
	mux.HandleFunc("/api/runs/abort", h.handleRunAbort)
	mux.HandleFunc("/api/runs/archive", h.handleRunArchive)
	mux.HandleFunc("/api/logs", h.handleLogs)
	mux.HandleFunc("/", h.handlePage)
	h.startStaleCleaner()
	return mux
}

func (h *portalHandler) startStaleCleaner() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("portal: stale cleanup panicked: %v", r)
			}
		}()
		if err := h.staleCleaner(h.repoRoot); err != nil {
			log.Printf("portal: stale cleanup failed: %v", err)
		}
	}()
}

func (h *portalHandler) handleInstances(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	instances, err := discoverPortalInstances(h.repoRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"repoRoot": h.repoRoot, "instances": instances})
}

func (h *portalHandler) handleRuns(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	runKey := strings.TrimSpace(r.URL.Query().Get("runKey"))
	if runKey != "" {
		run, err := h.runsIndex.FindByKey(r.Context(), runKey)
		if err != nil {
			status := http.StatusInternalServerError
			var abortErr *portalAbortError
			if errors.As(err, &abortErr) {
				status = abortErr.status
			}
			writeJSONError(w, err.Error(), status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"repoRoot": h.repoRoot, "run": run})
		return
	}
	summary := strings.TrimSpace(r.URL.Query().Get("summary")) == "1"
	if summary {
		result, err := h.runsIndex.SummarySnapshot(r.Context(), r.Header.Get("If-None-Match"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		if result.ETag != "" {
			w.Header().Set("ETag", result.ETag)
		}
		if result.NotModified {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"repoRoot": h.repoRoot, "runs": result.Runs})
		return
	}
	runs, err := h.runsIndex.Snapshot(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"repoRoot": h.repoRoot, "runs": runs})
}

func (h *portalHandler) handleRunStream(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	servePortalRunStream(w, r, h.repoRoot)
}

func (h *portalHandler) handleRunAbort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !portalAbortSupported() {
		writeJSONError(w, "abort issue is unsupported on this platform", http.StatusNotImplemented)
		return
	}
	var req struct {
		RunKey string `json:"runKey"`
		Issue  int    `json:"issue"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, portalMaxBodyBytes)).Decode(&req); err != nil {
		writeJSONError(w, "invalid run payload", http.StatusBadRequest)
		return
	}
	req.RunKey = strings.TrimSpace(req.RunKey)
	if req.RunKey == "" {
		writeJSONError(w, "missing runKey", http.StatusBadRequest)
		return
	}
	if req.Issue <= 0 {
		writeJSONError(w, "missing issue", http.StatusBadRequest)
		return
	}
	if err := portalRunAborter(r.Context(), h.repoRoot, req.RunKey, req.Issue); err != nil {
		var abortErr *portalAbortError
		if errors.As(err, &abortErr) {
			writeJSONError(w, abortErr.Error(), abortErr.status)
			return
		}
		writeJSONError(w, "abort failed", http.StatusInternalServerError)
		return
	}
	h.runsIndex.Invalidate()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"runKey": req.RunKey, "issue": req.Issue, "status": "aborted", "scope": "issue"})
}

func (h *portalHandler) handleRunArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		RunID string `json:"runId"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, portalMaxBodyBytes)).Decode(&req); err != nil {
		writeJSONError(w, "invalid archive payload", http.StatusBadRequest)
		return
	}
	req.RunID = strings.TrimSpace(req.RunID)
	if req.RunID == "" {
		writeJSONError(w, "missing runId", http.StatusBadRequest)
		return
	}
	if _, err := archivePortalRunHandler(h.repoRoot, req.RunID); err != nil {
		var archiveErr *portalArchiveError
		if errors.As(err, &archiveErr) {
			writeJSONArchiveError(w, archiveErr.message, archiveErr.path, archiveErr.status)
			return
		}
		writeJSONError(w, "archive failed", http.StatusInternalServerError)
		return
	}
	h.runsIndex.Invalidate()
	w.WriteHeader(http.StatusOK)
}

func (h *portalHandler) handleLogs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	relPath := strings.TrimSpace(r.URL.Query().Get("path"))
	if relPath == "" {
		http.Error(w, "missing path", http.StatusBadRequest)
		return
	}
	if filepath.IsAbs(relPath) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if strings.Contains(relPath, "..") {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	sandmanPrefix := filepath.Join(".sandman") + string(filepath.Separator)
	if !strings.HasPrefix(relPath, sandmanPrefix) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	relPathInSandman := strings.TrimPrefix(relPath, sandmanPrefix)
	if !permittedLogPath.MatchString(relPathInSandman) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	absPath := filepath.Join(h.repoRoot, relPath)
	info, err := os.Stat(absPath)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", info.Name()))
	http.ServeFile(w, r, absPath)
}

func (h *portalHandler) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	data, err := buildPortalPageData(h.repoRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := portalPageTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
