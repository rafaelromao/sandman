package cmd

import (
	"bufio"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
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

// servePortalRunStream bridges a live run's Control Socket (run.sock) to an
// HTTP Server-Sent Events stream. The daemon's Broadcaster
// (daemon/broadcaster.go) replays its buffered output on connect, then
// tails live output, so the browser receives history + tail in one
// connection — replacing the per-poll 64KB socket snapshot used by
// /api/runs (see readPortalSocketOutput in portal_runs_view.go).
//
// Lifecycle:
//   - client disconnects (r.Context done) → the connection is force-closed,
//     which unblocks the read loop and the handler returns.
//   - daemon closes the socket (run finished/aborted) → the read returns
//     EOF and the stream ends cleanly.
//   - a read stalls past portalStreamReadTimeout → the loop re-arms and
//     continues; it is a safety net, not a hard stop.
//
// Each source line is emitted as its own SSE event so the client can append
// incrementally; ANSI and other control bytes are stripped server-side to
// match the cleaned run.log contract from cleanPortalText.
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
	if run.Kind != "active" || run.SocketPath == "" {
		writeJSONError(w, fmt.Sprintf("run %q is not active", runKey), http.StatusConflict)
		return
	}

	conn, err := net.DialTimeout("unix", run.SocketPath, portalReadTimeout)
	if err != nil {
		writeJSONError(w, fmt.Sprintf("could not connect to the agent daemon for run %q", runKey), http.StatusBadGateway)
		return
	}
	defer conn.Close()

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

	// Force-close the socket on client disconnect so a blocking ReadString
	// returns immediately instead of stranding the goroutine.
	go func() {
		<-r.Context().Done()
		_ = conn.Close()
	}()

	br := bufio.NewReader(conn)
	// http.ResponseWriter is not safe for concurrent writes. The heartbeat
	// goroutine and the main read loop both write to it; serialize them via
	// this mutex so concurrent Fprintf/Flush calls don't race (caught by
	// `-race` in CI). The mutex also protects against the http server's
	// own internal flush during finishRequest racing with our flush.
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
				if _, werr := fmt.Fprintf(w, ": keepalive\n\n"); werr != nil {
					writeMu.Unlock()
					return
				}
				_ = rc.Flush()
				writeMu.Unlock()
			}
		}
	}()
	for {
		select {
		case <-r.Context().Done():
			return
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(portalStreamReadTimeout))
		line, readErr := br.ReadString('\n')
		if line != "" && lineBelongsToRun(line, run.RunID) {
			cleaned := cleanPortalStreamLine(line)
			writeMu.Lock()
			if _, werr := fmt.Fprintf(w, "data: %s\n\n", cleaned); werr != nil {
				writeMu.Unlock()
				return
			}
			_ = rc.Flush()
			writeMu.Unlock()
		}
		if readErr != nil {
			// A timeout is a transient read deadline expiry, not a reason
			// to end the stream; anything else (EOF, closed) ends it.
			if netErr := (*net.OpError)(nil); errors.As(readErr, &netErr) && netErr.Timeout() {
				continue
			}
			return
		}
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
