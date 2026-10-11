package cmd

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelromao/sandman/internal/events"
)

type portalManualLogTicker struct {
	ch      chan time.Time
	stopped chan struct{}
}

func TestPortalLogProtocol_NativeHeaderResumePrecedesQuery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("old\n"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := testPortalLogRegistry(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servePortalLogStream(w, r, portalRun{RunID: "row", LogPath: path}, portalLogStreamOptions{
			Registry: registry, Terminal: func(context.Context) (bool, error) { return true, nil },
		})
	}))
	defer server.Close()
	fetch := func(query, header string, useHeader bool) *portalLogFrame {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, server.URL+"?cursor="+url.QueryEscape(query), nil)
		if useHeader {
			req.Header.Set("Last-Event-ID", header)
		}
		resp, err := (&http.Client{Timeout: time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return readPortalLogFrame(t, bufio.NewReader(resp.Body))
	}
	initial := fetch("", "", false)
	appendSavedLog(t, path, "middle\n")
	middle := fetch(initial.Cursor, "", false)
	appendSavedLog(t, path, "same\n\nsame\n")
	latest := fetch(initial.Cursor, middle.Cursor, true)
	if latest.Type != "append" || latest.Start != middle.End || latest.Generation != initial.Generation || !reflect.DeepEqual(recordTexts(latest), []string{"same", "", "same"}) {
		t.Fatalf("native resume = %#v", latest)
	}
	// A valid older header also wins over a newer URL cursor.
	olderHeader := fetch(latest.Cursor, middle.Cursor, true)
	if olderHeader.Start != middle.End || !reflect.DeepEqual(recordTexts(olderHeader), []string{"same", "", "same"}) {
		t.Fatalf("header must take precedence regardless of offset: %#v", olderHeader)
	}
	for _, invalid := range []string{"", "invalid", initial.Cursor + "corrupt"} {
		frame := fetch(latest.Cursor, invalid, true)
		if frame.Type != "reset" || frame.Start != 0 || !reflect.DeepEqual(recordTexts(frame), []string{"old", "middle", "same", "", "same"}) {
			t.Fatalf("invalid header %q fell back to query cursor: %#v", invalid, frame)
		}
	}
}

func TestPortalLogProtocol_PendingCreationDisappearanceAndArchive(t *testing.T) {
	for _, scenario := range []string{"creation", "disappearance", "archive"} {
		t.Run(scenario, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "run.log")
			if scenario != "creation" {
				if err := os.WriteFile(path, []byte("saved\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			tick := &portalManualLogTicker{ch: make(chan time.Time), stopped: make(chan struct{})}
			terminal := make(chan bool, 10)
			for n := 0; n < 6; n++ {
				terminal <- false
			}
			resolved := make(chan string, 2)
			registry := testPortalLogRegistry(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				servePortalLogStream(w, r, portalRun{RunID: "row", LogPath: path}, portalLogStreamOptions{
					Registry: registry, NewTicker: func() portalLogTicker { return tick },
					Terminal: func(context.Context) (bool, error) { return <-terminal, nil },
					Resolve: func(context.Context) (portalRun, error) {
						select {
						case next := <-resolved:
							return portalRun{RunID: "row", LogPath: next}, nil
						default:
							return portalRun{}, os.ErrNotExist
						}
					},
				})
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			req, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			first := readPortalLogFrame(t, reader)
			if scenario == "creation" {
				if first.Type != "pending" || first.Cursor != "" {
					t.Fatalf("missing live source = %#v", first)
				}
				if err := os.WriteFile(path, []byte("created\n"), 0600); err != nil {
					t.Fatal(err)
				}
				tick.ch <- time.Now()
				created := readPortalLogFrame(t, reader)
				if created.Type != "snapshot" || !reflect.DeepEqual(recordTexts(created), []string{"created"}) {
					t.Fatalf("created = %#v", created)
				}
			} else {
				archived := path + ".archive"
				if err := os.Rename(path, archived); err != nil {
					t.Fatal(err)
				}
				if scenario == "archive" {
					resolved <- archived
					appendSavedLog(t, archived, "after archive\n")
					tick.ch <- time.Now()
					frame := readPortalLogFrame(t, reader)
					if frame.Type != "append" || frame.Generation != first.Generation || frame.Start != first.End || !reflect.DeepEqual(recordTexts(frame), []string{"after archive"}) {
						t.Fatalf("archive relocation = %#v", frame)
					}
				} else {
					tick.ch <- time.Now()
					pending := readPortalLogFrame(t, reader)
					if pending.Type != "pending" || pending.Cursor != first.Cursor || pending.End != first.End {
						t.Fatalf("pending must retain accepted state = %#v", pending)
					}
					// The old descriptor still exists, but no validated relocation
					// proved continuity. Even reattaching it must explicitly reset.
					resolved <- archived
					appendSavedLog(t, archived, "reattached\n")
					tick.ch <- time.Now().Add(2 * time.Second)
					frame := readPortalLogFrame(t, reader)
					if frame.Type != "reset" || frame.Generation == first.Generation || !reflect.DeepEqual(recordTexts(frame), []string{"saved", "reattached"}) {
						t.Fatalf("unproven disappearance reattached silently = %#v", frame)
					}
				}
			}
			cancel()
			select {
			case <-tick.stopped:
			case <-time.After(time.Second):
				t.Fatal("request cancellation did not stop polling")
			}
		})
	}
}

func TestPortalLogProtocol_TerminalUnavailableAndLifecycleReadFailure(t *testing.T) {
	for _, readFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(readFailure), func(t *testing.T) {
			tick := &portalManualLogTicker{ch: make(chan time.Time), stopped: make(chan struct{})}
			path := filepath.Join(t.TempDir(), "missing.log")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				servePortalLogStream(w, r, portalRun{RunID: "row", LogPath: path, Kind: "completed"}, portalLogStreamOptions{
					NewTicker: func() portalLogTicker { return tick },
					Terminal: func(context.Context) (bool, error) {
						if readFailure {
							return true, fmt.Errorf("event read failed")
						}
						return true, nil
					},
				})
			}))
			defer server.Close()
			resp, err := (&http.Client{Timeout: time.Second}).Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			reader := bufio.NewReader(resp.Body)
			frame := readPortalLogFrame(t, reader)
			if readFailure {
				if frame.Type != "pending" {
					t.Fatalf("failed lifecycle read must not authorize end: %#v", frame)
				}
			} else if frame.Type != "unavailable" || readPortalLogFrame(t, reader).Type != "end" {
				t.Fatalf("terminal missing source = %#v", frame)
			}
		})
	}
}

func TestPortalLogProtocol_FreshEventLifecycleSurvivesWaitRetryContinuation(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".sandman"), 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "run.log")
	if err := os.WriteFile(path, []byte("initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	logger := &events.JSONLLogger{Path: filepath.Join(root, ".sandman", "events.jsonl")}
	if err := logger.Log(events.Event{Type: "run.started", RunID: "row", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	fresh := portalSavedLogTerminal(root, "row")
	observed := make(chan bool, 2)
	tick := &portalManualLogTicker{ch: make(chan time.Time), stopped: make(chan struct{})}
	var resolutions int
	registry := testPortalLogRegistry(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servePortalLogStream(w, r, portalRun{RunID: "row", LogPath: path, Kind: "completed", Status: "success"}, portalLogStreamOptions{
			Registry: registry, NewTicker: func() portalLogTicker { return tick },
			Terminal: func(ctx context.Context) (bool, error) {
				terminal, err := fresh(ctx)
				observed <- terminal
				return terminal, err
			},
			Resolve: func(context.Context) (portalRun, error) {
				resolutions++
				return portalRun{}, fmt.Errorf("unexpected resolve")
			},
		})
	}))
	defer server.Close()
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	frame := readPortalLogFrame(t, reader)
	if <-observed || frame.Type != "snapshot" {
		t.Fatalf("metadata must not authorize completion: %#v", frame)
	}
	for _, kind := range []string{"run.await", "run.capacity_queued", "run.continued", "run.retry"} {
		if err := logger.Log(events.Event{Type: kind, RunID: "row", Timestamp: time.Now(), Payload: map[string]any{"reason": "review", "batch_id": "next", "attempt": 2}}); err != nil {
			t.Fatal(err)
		}
		appendSavedLog(t, path, kind+"\n")
		tick.ch <- time.Now()
		next := readPortalLogFrame(t, reader)
		if <-observed || next.Type != "append" || next.Generation != frame.Generation || next.Start != frame.End || !reflect.DeepEqual(recordTexts(next), []string{kind}) {
			t.Fatalf("same-RunID %s lost continuity: %#v", kind, next)
		}
		frame = next
	}
	appendSavedLog(t, path, "flushed final")
	if err := logger.Log(events.Event{Type: "run.aborted", RunID: "row", Timestamp: time.Now()}); err != nil {
		t.Fatal(err)
	}
	tick.ch <- time.Now()
	final := readPortalLogFrame(t, reader)
	end := readPortalLogFrame(t, reader)
	if !<-observed || final.Type != "append" || end.Type != "end" || end.End != final.End || !reflect.DeepEqual(recordTexts(final), []string{"flushed final"}) {
		t.Fatalf("fresh abort did not drain: %#v; %#v", final, end)
	}
	<-tick.stopped
	if resolutions != 0 {
		t.Fatalf("healthy tail rehydrated full Portal metadata %d times", resolutions)
	}
}

func TestPortalLogProtocol_MissingResumedSourceRetainsAcceptedControlPosition(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("accepted\n"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := testPortalLogRegistry(t)
	source := newPortalLogSource("row", path, "", registry, "")
	accepted, err := source.Poll(false)
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servePortalLogStream(w, r, portalRun{RunID: "row", LogPath: path}, portalLogStreamOptions{Registry: registry, Terminal: func(context.Context) (bool, error) { return true, nil }})
	}))
	defer server.Close()
	resp, err := (&http.Client{Timeout: time.Second}).Get(server.URL + "?cursor=" + url.QueryEscape(accepted.Cursor))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	unavailable, end := readPortalLogFrame(t, reader), readPortalLogFrame(t, reader)
	if unavailable.Type != "unavailable" || end.Type != "end" || end.Cursor != accepted.Cursor || end.Generation != accepted.Generation || end.End != accepted.End || unavailable.Cursor != accepted.Cursor {
		t.Fatalf("missing resume changed accepted position: unavailable=%#v end=%#v accepted=%#v", unavailable, end, accepted)
	}
}

func TestPortalLogProtocol_PendingDoesNotRehydrateMetadataOnEveryTick(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.log")
	tick := &portalManualLogTicker{ch: make(chan time.Time), stopped: make(chan struct{})}
	var resolutions atomic.Int32
	observations := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servePortalLogStream(w, r, portalRun{RunID: "row", LogPath: path}, portalLogStreamOptions{
			NewTicker: func() portalLogTicker { return tick },
			Terminal:  func(context.Context) (bool, error) { observations <- struct{}{}; return false, nil },
			Resolve: func(context.Context) (portalRun, error) {
				resolutions.Add(1)
				return portalRun{}, os.ErrNotExist
			},
		})
	}))
	defer server.Close()
	before := time.Now()
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if frame := readPortalLogFrame(t, bufio.NewReader(resp.Body)); frame.Type != "pending" {
		t.Fatalf("missing startup = %#v", frame)
	}
	<-observations
	for n := 1; n <= 4; n++ {
		tick.ch <- before.Add(time.Duration(n) * 200 * time.Millisecond)
		<-observations
	}
	resp.Body.Close()
	<-tick.stopped
	if count := resolutions.Load(); count != 1 {
		t.Fatalf("pending startup rehydrated full metadata %d times in four tail ticks", count)
	}
}

func TestPortalLogProtocol_TerminalRechecksPendingRelocationBeforeUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	archived := path + ".archive"
	var terminal, resolved atomic.Bool
	tick := &portalManualLogTicker{ch: make(chan time.Time), stopped: make(chan struct{})}
	registry := testPortalLogRegistry(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servePortalLogStream(w, r, portalRun{RunID: "row", LogPath: path}, portalLogStreamOptions{
			Registry: registry, NewTicker: func() portalLogTicker { return tick },
			Terminal: func(context.Context) (bool, error) { return terminal.Load(), nil },
			Resolve: func(context.Context) (portalRun, error) {
				if !resolved.Load() {
					return portalRun{}, os.ErrNotExist
				}
				return portalRun{RunID: "row", LogPath: archived}, nil
			},
		})
	}))
	defer server.Close()
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	initial := readPortalLogFrame(t, reader)
	if err := os.Rename(path, archived); err != nil {
		t.Fatal(err)
	}
	tick.ch <- time.Now()
	if frame := readPortalLogFrame(t, reader); frame.Type != "pending" || frame.Cursor != initial.Cursor {
		t.Fatalf("unresolved relocation = %#v", frame)
	}
	appendSavedLog(t, archived, "final fragment")
	resolved.Store(true)
	terminal.Store(true)
	tick.ch <- time.Now() // still inside the relocation backoff
	final, end := readPortalLogFrame(t, reader), readPortalLogFrame(t, reader)
	if final.Type != "reset" || final.Start != 0 || !reflect.DeepEqual(recordTexts(final), []string{"initial", "final fragment"}) || end.Type != "end" || end.End != final.End {
		t.Fatalf("available terminal output was discarded as unavailable: final=%#v end=%#v", final, end)
	}
}

func (tick *portalManualLogTicker) Chan() <-chan time.Time { return tick.ch }
func (tick *portalManualLogTicker) Stop()                  { close(tick.stopped) }

func TestPortalLogProtocol_SavedSnapshotAppendAndTerminalDrain(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("same\n\nsame\n"), 0600); err != nil {
		t.Fatal(err)
	}
	tick := &portalManualLogTicker{ch: make(chan time.Time), stopped: make(chan struct{})}
	terminal := make(chan bool, 4)
	terminal <- false
	terminal <- false
	terminal <- true
	registry := testPortalLogRegistry(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		servePortalLogStream(w, r, portalRun{RunID: "row", LogPath: path, Kind: "completed", Status: "success"}, portalLogStreamOptions{
			Terminal:  func(context.Context) (bool, error) { return <-terminal, nil },
			NewTicker: func() portalLogTicker { return tick },
			Registry:  registry,
		})
	}))
	defer server.Close()
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	reader := bufio.NewReader(resp.Body)
	snapshot := readPortalLogFrame(t, reader)
	if snapshot.Type != "snapshot" || snapshot.End != 11 || !reflect.DeepEqual(recordTexts(snapshot), []string{"same", "", "same"}) {
		t.Fatalf("snapshot = %#v", snapshot)
	}
	appendSavedLog(t, path, "live\nfinal €")
	tick.ch <- time.Now()
	appendFrame := readPortalLogFrame(t, reader)
	if appendFrame.Type != "append" || appendFrame.Start != snapshot.End || !reflect.DeepEqual(recordTexts(appendFrame), []string{"live"}) {
		t.Fatalf("append = %#v", appendFrame)
	}
	tick.ch <- time.Now()
	final := readPortalLogFrame(t, reader)
	end := readPortalLogFrame(t, reader)
	if final.Type != "append" || final.Start != appendFrame.End || !reflect.DeepEqual(recordTexts(final), []string{"final €"}) || end.Type != "end" || end.End != final.End || end.Cursor != final.Cursor {
		t.Fatalf("final = %#v, end = %#v", final, end)
	}
	select {
	case <-tick.stopped:
	case <-time.After(time.Second):
		t.Fatal("terminal stream did not stop its ticker")
	}
}

// Read complete ordinary SSE messages. Native EventSource dispatches these to
// onmessage and resumes with the exact id; named events and text fallbacks fail.
func readPortalLogFrame(t *testing.T, reader *bufio.Reader) *portalLogFrame {
	t.Helper()
	var id, payload string
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read SSE frame: %v", err)
		}
		line = strings.TrimSuffix(line, "\n")
		switch {
		case strings.HasPrefix(line, "event:"):
			t.Fatalf("frame must be an ordinary onmessage SSE: %q", line)
		case strings.HasPrefix(line, "id: "):
			id = strings.TrimPrefix(line, "id: ")
		case strings.HasPrefix(line, "data: "):
			payload = strings.TrimPrefix(line, "data: ")
		case line == "" && payload != "":
			var frame portalLogFrame
			if err := json.Unmarshal([]byte(payload), &frame); err != nil {
				t.Fatalf("invalid JSON frame %q: %v", payload, err)
			}
			if frame.Records == nil || frame.RunID == "" {
				t.Fatalf("missing frame fields: %q", payload)
			}
			if frame.Type == "pending" || frame.Type == "unavailable" {
				if id != "" {
					t.Fatal("unavailable source must not advance EventSource ID")
				}
			} else if id != frame.Cursor || (id == "" && frame.Type != "end") {
				t.Fatalf("SSE id %q != cursor %q", id, frame.Cursor)
			}
			return &frame
		}
	}
}
