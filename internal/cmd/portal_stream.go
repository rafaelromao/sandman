package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/rafaelromao/sandman/internal/daemon"
)

// portalStreamReadTimeout caps a blocking read on the bridged Control
// Socket. A tail is normally expected to produce output or EOF well within
// this window; the deadline is a safety net so a wedged daemon cannot hold
// the stream (and its goroutine) open forever. The browser's EventSource
// reconnects transparently if the stream ends.
var portalStreamReadTimeout = 30 * time.Second

// portalStreamHeartbeat is the cadence at which the SSE bridge emits a
// `: keepalive` SSE comment when no data has flowed through the bridged
// Control Socket. SSE comments are ignored by the browser's onmessage
// handler but keep the TCP connection warm so intermediate proxies and
// the browser's own idle reaper do not silently close a healthy tail of
// a quiet agent. Set well below the typical 30s HTTP idle timeout.
var portalStreamHeartbeat = 15 * time.Second

// servePortalRunStream exposes the Saved Run Log as an HTTP Server-Sent Events
// stream. It sends position-bearing snapshot/append records from one open file
// descriptor; the Control Socket remains only as a pre-artifact compatibility
// fallback until the saved writer creates run.log.
//
// Lifecycle:
//   - client disconnects (r.Context done) → the connection is force-closed,
//     which unblocks the read loop and the handler returns.
//   - daemon closes the socket (run finished/aborted) → the read returns
//     EOF and the stream ends cleanly.
//   - a read stalls past portalStreamReadTimeout → the loop re-arms and
//     continues; it is a safety net, not a hard stop.
//
// Saved records are emitted in structured batches so the client can resume by
// raw byte cursor; ANSI and labels are stripped after positions are assigned.
//
// The server's global WriteTimeout (30s) would otherwise cut the stream at
// 30s; http.NewResponseController clears this response's write deadline so
// the tail can run as long as the run is live, without weakening the
// timeout for the rest of the portal's handlers.
func servePortalRunStream(w http.ResponseWriter, r *http.Request, repoRoot string) {
	runKey := strings.TrimSpace(r.URL.Query().Get("runKey"))
	if runKey == "" {
		writeJSONError(w, "missing runKey", http.StatusBadRequest)
		return
	}

	run, err := portalRunForKey(repoRoot, runKey)
	if err != nil {
		var abortErr *portalAbortError
		if errors.As(err, &abortErr) {
			writeJSONError(w, abortErr.Error(), abortErr.status)
			return
		}
		writeJSONError(w, "resolve run: "+err.Error(), http.StatusInternalServerError)
		return
	}
	logPath := portalLogSourcePath(run)
	if logPath == "" {
		if run.Kind == "active" && run.SocketPath != "" {
			serveLegacyPortalSocketStream(w, r, run)
			return
		}
	} else if _, statErr := os.Stat(logPath); errors.Is(statErr, os.ErrNotExist) && run.Kind == "active" && run.SocketPath != "" {
		serveLegacyPortalSocketStream(w, r, run)
		return
	}
	// Clear this response's write deadline so the server's 30s WriteTimeout
	// does not sever a long-lived tail. Falls back silently if the writer
	// does not support deadline control (ResponseController returns an
	// error only when the underlying connection cannot set a deadline,
	// which is not fatal for a local portal).
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()

	// The heartbeat and saved-log source share the response writer.
	var writeMu sync.Mutex
	locked := &portalLockedWriter{ResponseWriter: w, mu: &writeMu}
	heartbeat := time.NewTicker(portalStreamHeartbeat)
	defer heartbeat.Stop()
	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	defer func() {
		close(heartbeatStop)
		<-heartbeatDone
	}()
	go func() {
		defer close(heartbeatDone)
		for {
			select {
			case <-heartbeatStop:
				return
			case <-heartbeat.C:
				if _, werr := locked.Write([]byte(": keepalive\n\n")); werr != nil {
					return
				}
			}
		}
	}()

	if logPath == "" {
		_ = writePortalLogEvent(locked, "unavailable", map[string]any{"runId": run.RunID, "reason": "missing-saved-log"}, "")
		_ = writePortalLogEvent(locked, "end", map[string]any{"runId": run.RunID}, "")
		return
	}
	var cursor *portalLogCursor
	for _, encoded := range []string{r.Header.Get("Last-Event-ID"), r.URL.Query().Get("cursor")} {
		if strings.TrimSpace(encoded) == "" {
			continue
		}
		decoded, decodeErr := decodePortalLogCursor(encoded)
		if decodeErr == nil {
			cursor = &decoded
		} else {
			// Keep an invalid identity explicit so the source emits reset plus
			// a coherent replacement snapshot instead of silently joining it.
			invalid := portalLogCursor{}
			cursor = &invalid
		}
		break
	}
	for {
		source, sourceErr := newPortalLogSource(logPath, run.RunID)
		if sourceErr == nil {
			terminal := func() bool {
				latest, err := portalRunForKey(repoRoot, runKey)
				if err != nil {
					// Lifecycle observation failures are unknown, not terminal.
					return false
				}
				return portalLogTerminal(latest)
			}
			_ = streamPortalSavedLog(r.Context(), locked, source, cursor, terminal)
			return
		}
		if !errors.Is(sourceErr, os.ErrNotExist) || portalLogTerminal(run) {
			_ = writePortalLogEvent(locked, "unavailable", map[string]any{"runId": run.RunID, "reason": sourceErr.Error()}, "")
			_ = writePortalLogEvent(locked, "end", map[string]any{"runId": run.RunID}, "")
			return
		}
		if err := writePortalLogEvent(locked, "pending", map[string]any{"runId": run.RunID}, ""); err != nil {
			return
		}
		timer := time.NewTimer(portalLogPollInterval)
		select {
		case <-r.Context().Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

type portalLockedWriter struct {
	http.ResponseWriter
	mu *sync.Mutex
}

// serveLegacyPortalSocketStream keeps attach compatibility for a short
// pre-artifact window. Once run.log exists, Portal records always come from
// portalLogSource and never use broadcaster offsets as saved-log cursors.
func serveLegacyPortalSocketStream(w http.ResponseWriter, r *http.Request, run portalRun) {
	conn, err := net.DialTimeout("unix", run.SocketPath, portalReadTimeout)
	if err != nil {
		writeJSONError(w, fmt.Sprintf("could not connect to the agent daemon for run %q", run.Key), http.StatusBadGateway)
		return
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{daemon.PortalStreamHandshake}); err != nil {
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_ = rc.Flush()
	go func() {
		<-r.Context().Done()
		_ = conn.Close()
	}()
	var writeMu sync.Mutex
	heartbeat := time.NewTicker(portalStreamHeartbeat)
	defer heartbeat.Stop()
	heartbeatStop := make(chan struct{})
	heartbeatDone := make(chan struct{})
	defer func() {
		close(heartbeatStop)
		<-heartbeatDone
	}()
	go func() {
		defer close(heartbeatDone)
		for {
			select {
			case <-heartbeatStop:
				return
			case <-heartbeat.C:
				writeMu.Lock()
				_, writeErr := fmt.Fprint(w, ": keepalive\n\n")
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				writeMu.Unlock()
				if writeErr != nil {
					return
				}
			}
		}
	}()
	br := bufio.NewReader(conn)
	for {
		_ = conn.SetReadDeadline(time.Now().Add(portalStreamReadTimeout))
		line, readErr := br.ReadString('\n')
		if line == daemon.PortalReplayBoundary {
			writeMu.Lock()
			if _, err := fmt.Fprint(w, "event: replay-complete\ndata: replay-complete\n\n"); err != nil {
				writeMu.Unlock()
				return
			}
			_ = rc.Flush()
			writeMu.Unlock()
		}
		if line != "" && lineBelongsToRun(line, run.RunID) {
			writeMu.Lock()
			if _, err := fmt.Fprintf(w, "data: %s\n\n", cleanPortalStreamLine(line)); err != nil {
				writeMu.Unlock()
				return
			}
			_ = rc.Flush()
			writeMu.Unlock()
		}
		if readErr != nil {
			if netErr := (*net.OpError)(nil); errors.As(readErr, &netErr) && netErr.Timeout() {
				continue
			}
			return
		}
	}
}

func (w *portalLockedWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	n, err := w.ResponseWriter.Write(data)
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
	return n, err
}

func (w *portalLockedWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// lineBelongsToRun reports whether a raw socket line was produced by the
// requested run. Lines written by a daemon in a mixed batch are tagged
// with a `[<runID>] ` prefix; lines without that prefix cannot be
// attributed to any run and must not leak into the per-run stream. The
// check strips ANSI escapes from the head of the line first so a label
// wrapped in colour codes (e.g. `\x1b[32m[<runID>]\x1b[0m ...`) still
// matches; without that, an ANSI wrapper would let sibling output slip
// past the filter.
func lineBelongsToRun(line, runID string) bool {
	if runID == "" {
		return false
	}
	stripped := portalANSISequence.ReplaceAllString(line, "")
	prefix := "[" + runID + "]"
	if !strings.HasPrefix(stripped, prefix) {
		return false
	}
	// Reject lines that match only because a longer runID happens to
	// share the row's runID as a prefix (e.g. row "run-1" would
	// otherwise claim "[run-12] ..." lines). The character after the
	// closing bracket must be the canonical `[<runID>] ` space
	// delimiter defined in CONTEXT.md.
	rest := stripped[len(prefix):]
	if rest == "" {
		return false
	}
	return rest[0] == ' '
}

// cleanPortalStreamLine strips ANSI escapes and the trailing newline and
// removes control bytes other than tab, matching the run.log contract from
// cleanPortalText so a streamed line renders identically to a polled one.
func cleanPortalStreamLine(line string) string {
	line = portalANSISequence.ReplaceAllString(line, "")
	line = strings.TrimRight(line, "\r\n")
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
