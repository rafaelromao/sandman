package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/rafaelromao/sandman/internal/config"
)

type portalHandler struct {
	repoRoot     string
	launchData   portalLaunchFormData
	cfg          *config.Config
	launcher     *portalLauncher
	launcherErr  error
	runsIndex    *portalRunsIndex
	staleCleaner func(string) error
}

func newPortalHandler(repoRoot string, launchData portalLaunchFormData, cfg *config.Config) http.Handler {
	h := &portalHandler{
		repoRoot:     repoRoot,
		launchData:   launchData,
		cfg:          cfg,
		launcherErr:  nil,
		runsIndex:    getPortalRunsIndex(repoRoot),
		staleCleaner: portalStaleCleaner,
	}
	h.launcher, h.launcherErr = newPortalLauncher(repoRoot)

	mux := http.NewServeMux()
	mux.HandleFunc("/api/commands", h.handleCommands)
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

func (h *portalHandler) handleCommands(w http.ResponseWriter, r *http.Request) {
	if h.launcherErr != nil {
		http.Error(w, h.launcherErr.Error(), http.StatusInternalServerError)
		return
	}
	switch r.Method {
	case http.MethodGet:
		commands, err := h.launcher.list()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(w).Encode(map[string]any{"repoRoot": h.repoRoot, "commands": commands})
	case http.MethodPost:
		req, err := parsePortalUnifiedLaunchRequest(r)
		if err != nil {
			writeJSONError(w, "invalid command payload", http.StatusBadRequest)
			return
		}

		var (
			args []string
			resp any
		)
		switch strings.TrimSpace(req.Command) {
		case "run":
			args, err = buildPortalRunArgs(h.repoRoot, h.cfg, req.runRequest())
			if err != nil {
				writeJSONError(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := portalStartRun(r.Context(), h.repoRoot, args); err != nil {
				writeJSONError(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.runsIndex.Invalidate()
			if _, err := h.launcher.record(args); err != nil {
				writeJSONError(w, err.Error(), http.StatusInternalServerError)
				return
			}
			resp = portalLaunchResponse{Message: "Started sandman run.", Args: args}
		case "continue":
			commandReq := req.commandRequest()
			commandReq.Preset = req.Command
			args, err = buildPortalCommandArgs(commandReq)
			if err != nil {
				writeJSONError(w, err.Error(), http.StatusBadRequest)
				return
			}
			if err := portalStartRun(r.Context(), h.repoRoot, args); err != nil {
				writeJSONError(w, err.Error(), http.StatusInternalServerError)
				return
			}
			h.runsIndex.Invalidate()
			command, err := h.launcher.record(args)
			if err != nil {
				writeJSONError(w, err.Error(), http.StatusInternalServerError)
				return
			}
			resp = command
		case "status", "history", "clean", "config", "archive":
			commandReq := req.commandRequest()
			commandReq.Preset = req.Command
			args, err = buildPortalCommandArgs(commandReq)
			if err != nil {
				writeJSONError(w, err.Error(), http.StatusBadRequest)
				return
			}
			command, err := h.launcher.launch(r.Context(), args)
			if err != nil {
				writeJSONError(w, err.Error(), http.StatusInternalServerError)
				return
			}
			resp = command
		default:
			writeJSONError(w, "unknown command", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(resp)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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
	if err := archivePortalRunHandler(h.repoRoot, req.RunID); err != nil {
		var archiveErr *portalArchiveError
		if errors.As(err, &archiveErr) {
			writeJSONError(w, archiveErr.message, archiveErr.status)
			return
		}
		writeJSONError(w, "archive failed", http.StatusInternalServerError)
		return
	}
	h.runsIndex.Invalidate()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"runId": req.RunID, "status": "archived"})
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
	logDir := filepath.Join(h.repoRoot, ".sandman", "logs")
	logPrefix := filepath.Join(".sandman", "logs")
	name := strings.TrimPrefix(relPath, logPrefix)
	name = strings.TrimPrefix(name, string(filepath.Separator))
	if name == "" {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	if filepath.IsAbs(name) {
		http.Error(w, "invalid path", http.StatusBadRequest)
		return
	}
	info, err := fs.Stat(os.DirFS(logDir), name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", info.Name()))
	http.ServeFileFS(w, r, os.DirFS(logDir), name)
}

func (h *portalHandler) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	data, err := buildPortalPageData(h.repoRoot, h.launchData)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := portalPageTemplate.Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
