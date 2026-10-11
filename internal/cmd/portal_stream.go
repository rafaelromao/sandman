package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
	"github.com/rafaelromao/sandman/internal/paths"
)

var portalStreamHeartbeat = 15 * time.Second

const portalLogPollInterval = 200 * time.Millisecond
const portalLogResolveInterval = time.Second

var portalSavedLogRegistry = newPortalLogRegistry()

type portalLogTicker interface {
	Chan() <-chan time.Time
	Stop()
}

type portalWallLogTicker struct{ *time.Ticker }

func (t portalWallLogTicker) Chan() <-chan time.Time { return t.C }

// The resolved run, relocation resolver, fresh lifecycle predicate and ticker
// are the HTTP seam shared by production and real EventSource fixtures. Neither
// a portal status label nor EOF/socket state can authorize a terminal drain.
type portalLogStreamOptions struct {
	Resolve   func(context.Context) (portalRun, error)
	Terminal  func(context.Context) (bool, error)
	NewTicker func() portalLogTicker
	Registry  *portalLogRegistry
	Scope     string
}

func servePortalRunStream(w http.ResponseWriter, r *http.Request, repoRoot string) {
	runKey := strings.TrimSpace(r.URL.Query().Get("runKey"))
	if runKey == "" {
		writeJSONError(w, "missing runKey", http.StatusBadRequest)
		return
	}
	idx := getPortalRunsIndex(repoRoot)
	run, err := idx.FindByKey(r.Context(), runKey)
	if err != nil {
		status := http.StatusInternalServerError
		var abortErr *portalAbortError
		if errors.As(err, &abortErr) {
			status = abortErr.status
		}
		writeJSONError(w, err.Error(), status)
		return
	}
	if err := validatePortalSavedLogPath(repoRoot, run); err != nil {
		writeJSONError(w, "invalid saved log source", http.StatusConflict)
		return
	}
	servePortalLogStream(w, r, run, portalLogStreamOptions{
		Scope: repoRoot,
		Resolve: func(ctx context.Context) (portalRun, error) {
			// Only path disappearance invokes the full artifact resolver. Normal
			// idle ticks do not hydrate the view, index or any daemon socket.
			idx.Invalidate()
			relocated, err := idx.FindByKey(ctx, runKey)
			if err == nil {
				err = validatePortalSavedLogPath(repoRoot, relocated)
			}
			return relocated, err
		},
		Terminal: portalSavedLogTerminal(repoRoot, run.RunID),
	})
}

func validatePortalSavedLogPath(repoRoot string, run portalRun) error {
	if run.RunID == "" {
		return fmt.Errorf("missing RunID")
	}
	if run.LogPath == "" {
		return nil
	}
	layout := paths.NewLayout(nil, repoRoot)
	if err := validateOwnedPath(run.LogPath, layout.BatchesDir, layout.ArchiveDir); err != nil {
		return err
	}
	if filepath.Base(run.LogPath) != "run.log" || filepath.Base(filepath.Dir(run.LogPath)) != run.RunID || filepath.Base(filepath.Dir(filepath.Dir(run.LogPath))) != "runs" {
		return fmt.Errorf("saved log identity does not match RunID")
	}
	return nil
}

// Stat on every observation, fold fresh authoritative events only when they
// change. This is independent of cached Portal metadata and socket liveness.
func portalSavedLogTerminal(repoRoot, runID string) func(context.Context) (bool, error) {
	path := paths.NewLayout(nil, repoRoot).EventsLogPath
	var previous os.FileInfo
	var terminal bool
	return func(ctx context.Context) (bool, error) {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		info, err := os.Stat(path)
		if err != nil {
			previous, terminal = nil, false
			return false, err
		}
		if previous != nil && os.SameFile(previous, info) && previous.Size() == info.Size() && previous.ModTime().Equal(info.ModTime()) {
			return terminal, nil
		}
		// Use a read-only, promptly closed descriptor. JSONLLogger owns a
		// long-lived writable handle and quarantine sidecars, neither of which
		// belongs to an HTTP subscription. The canonical event fold still owns
		// the lifecycle decision, including legacy queued/abort semantics.
		data, err := os.ReadFile(path)
		if err != nil {
			return false, err
		}
		var list []events.Event
		for _, line := range bytes.Split(data, []byte{'\n'}) {
			var event events.Event
			if json.Unmarshal(line, &event) == nil {
				list = append(list, event)
			}
		}
		terminal = false
		for _, state := range events.ProjectRunStates(list) {
			if state.RunID == runID {
				terminal = state.IsTerminal()
				break
			}
		}
		previous = info
		return terminal, nil
	}
}

func servePortalLogStream(w http.ResponseWriter, r *http.Request, run portalRun, opts portalLogStreamOptions) {
	if opts.Registry == nil {
		opts.Registry = portalSavedLogRegistry
	}
	if opts.NewTicker == nil {
		opts.NewTicker = func() portalLogTicker { return portalWallLogTicker{time.NewTicker(portalLogPollInterval)} }
	}
	// Header presence takes precedence even if empty or invalid. Never fall
	// back to the original URL cursor after a failed native reconnection proof.
	resume := r.URL.Query().Get("cursor")
	if values, present := r.Header[http.CanonicalHeaderKey("Last-Event-ID")]; present {
		resume = "invalid-header"
		if len(values) == 1 && values[0] != "" {
			resume = values[0]
		}
	}
	source := newPortalLogSource(run.RunID, run.LogPath, resume, opts.Registry, opts.Scope)
	defer source.Close()
	ticker := opts.NewTicker()
	defer ticker.Stop()
	heartbeat := time.NewTicker(portalStreamHeartbeat)
	defer heartbeat.Stop()
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	if err := rc.Flush(); err != nil {
		return
	}
	emit := func(frame *portalLogFrame) bool {
		data, err := json.Marshal(frame)
		if err != nil {
			return false
		}
		if frame.Cursor != "" && frame.Type != "pending" && frame.Type != "unavailable" {
			if _, err := fmt.Fprintf(w, "id: %s\n", frame.Cursor); err != nil {
				return false
			}
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	var pending bool
	now, nextResolve := time.Now(), time.Time{}
	for {
		if r.Context().Err() != nil {
			return
		}
		terminal := false
		if opts.Terminal != nil {
			// Read errors fail closed: continuing to tail is safe, ending is not.
			observed, err := opts.Terminal(r.Context())
			terminal = err == nil && observed
		}
		frame, err := source.Poll(terminal)
		// Probe relocation immediately on disappearance, then back off while
		// missing. Pending startup must not rehydrate the full Portal view and
		// sockets at the much faster file-tail cadence.
		if os.IsNotExist(err) && opts.Resolve != nil && (terminal || !pending || !now.Before(nextResolve)) {
			nextResolve = now.Add(portalLogResolveInterval)
			if relocated, resolveErr := opts.Resolve(r.Context()); resolveErr == nil && relocated.RunID == run.RunID {
				source.path = relocated.LogPath
				frame, err = source.Poll(terminal)
			}
		}
		if err != nil {
			source.Suspend()
			kind, reason := "pending", "source-missing"
			if !os.IsNotExist(err) {
				reason = "source-unreadable"
			}
			if terminal {
				kind = "unavailable"
			}
			if !pending || terminal {
				if !emit(&portalLogFrame{Type: kind, RunID: run.RunID, Generation: source.generation, Start: source.end, End: source.end, Records: []portalLogRecord{}, Cursor: source.cursor, Reason: reason}) {
					return
				}
			}
			pending = true
		} else {
			pending = false
			if frame != nil && !emit(frame) {
				return
			}
		}
		if terminal {
			emit(&portalLogFrame{Type: "end", RunID: run.RunID, Generation: source.generation, Start: source.end, End: source.end, Records: []portalLogRecord{}, Cursor: source.cursor})
			return
		}
		select {
		case <-r.Context().Done():
			return
		case tick, open := <-ticker.Chan():
			if !open {
				return
			}
			now = tick
		case <-heartbeat.C:
			now = time.Now()
			if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// Retained for existing socket/cleaning compatibility tests. Saved Run Log
// records are already private to one RunID and must not be text-filtered.
func lineBelongsToRun(line, runID string) bool {
	if runID == "" {
		return false
	}
	stripped := portalANSISequence.ReplaceAllString(line, "")
	prefix := "[" + runID + "]"
	return strings.HasPrefix(stripped, prefix+" ")
}

func cleanPortalStreamLine(line string) string {
	line = portalANSISequence.ReplaceAllString(line, "")
	line = strings.TrimSuffix(line, "\n")
	line = strings.TrimSuffix(line, "\r")
	line = stripLogLabel(line)
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, line)
}
