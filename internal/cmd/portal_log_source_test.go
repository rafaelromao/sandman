package cmd

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPortalLogSource_SnapshotTailSharesRawBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	raw := "[row] 23:59:59 \x1b[32msame\x1b[0m\r\n\n[row] 23:59:59 same\npartial \xe2\x82"
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	source := newPortalLogSource("row", path, "", testPortalLogRegistry(t), "")
	defer source.Close()
	frame, err := source.Poll(false)
	if err != nil {
		t.Fatal(err)
	}
	if frame.Type != "snapshot" || frame.Start != 0 || frame.End != 51 || frame.Cursor == "" || frame.Generation == "" {
		t.Fatalf("snapshot = %#v", frame)
	}
	want := []portalLogRecord{{Start: 0, End: 30, Text: "23:59:59 same"}, {Start: 30, End: 31, Text: ""}, {Start: 31, End: 51, Text: "23:59:59 same"}}
	if !reflect.DeepEqual(frame.Records, want) {
		t.Fatalf("records = %#v, want %#v", frame.Records, want)
	}
	appendSavedLog(t, path, "\xac\nnext\n")
	appendFrame, err := source.Poll(false)
	if err != nil {
		t.Fatal(err)
	}
	if appendFrame.Type != "append" || appendFrame.Start != frame.End || appendFrame.Generation != frame.Generation {
		t.Fatalf("append = %#v", appendFrame)
	}
	if got := recordTexts(appendFrame); !reflect.DeepEqual(got, []string{"partial €", "next"}) {
		t.Fatalf("tail = %q", got)
	}
	if idle, err := source.Poll(false); err != nil || idle != nil {
		t.Fatalf("idle EOF must remain unfinished: %#v, %v", idle, err)
	}
}

func TestPortalLogSource_ReplacementAfterUnlinkAndRegistryEviction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("same\n"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := testPortalLogRegistry(t)
	source := newPortalLogSource("row", path, "", registry, "")
	first, err := source.Poll(false)
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("same\n"), 0600); err != nil {
		t.Fatal(err)
	}
	replacement := newPortalLogSource("row", path, first.Cursor, registry, "")
	replaced, err := replacement.Poll(false)
	replacement.Close()
	if err != nil || replaced.Type != "reset" || replaced.Generation == first.Generation {
		t.Fatalf("identical replacement reused original generation: %#v, %v", replaced, err)
	}
	for n := 0; n < portalLogRegistryLimit; n++ {
		other := newPortalLogSource(fmt.Sprintf("other-%d", n), path, "", registry, "")
		if _, err := other.Poll(false); err != nil {
			t.Fatal(err)
		}
		other.Close()
	}
	evicted := newPortalLogSource("row", path, replaced.Cursor, registry, "")
	defer evicted.Close()
	frame, err := evicted.Poll(false)
	if err != nil || frame.Type != "reset" || frame.Generation == replaced.Generation {
		t.Fatalf("evicted identity proof must reset safely: %#v, %v", frame, err)
	}
}

func TestPortalLogSource_SnapshotHandoffWritesBeforeDuringAfter(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("before\n"), 0600); err != nil {
		t.Fatal(err)
	}
	appendSavedLog(t, path, "before acquisition\n")
	source := newPortalLogSource("row", path, "", testPortalLogRegistry(t), "")
	defer source.Close()
	source.onHighwater = func(end int64) {
		if end != 26 {
			t.Fatalf("captured highwater = %d", end)
		}
		appendSavedLog(t, path, "during acquisition\n")
		source.onHighwater = nil
	}
	snapshot, err := source.Poll(false)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.End != 26 || !reflect.DeepEqual(recordTexts(snapshot), []string{"before", "before acquisition"}) {
		t.Fatalf("snapshot crossed its descriptor highwater: %#v", snapshot)
	}
	appendSavedLog(t, path, "after acquisition\n")
	frame, err := source.Poll(false)
	if err != nil || frame.Start != snapshot.End || !reflect.DeepEqual(recordTexts(frame), []string{"during acquisition", "after acquisition"}) {
		t.Fatalf("handoff lost or duplicated records: %#v, %v", frame, err)
	}
}

func TestPortalLogSource_ConcurrentAcquisitionCannotMistakeAppendForShrink(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := testPortalLogRegistry(t)
	first := newPortalLogSource("row", path, "", registry, "")
	initial, err := first.Poll(false)
	first.Close()
	if err != nil {
		t.Fatal(err)
	}
	slow := newPortalLogSource("row", path, initial.Cursor, registry, "")
	defer slow.Close()
	slow.onHighwater = func(int64) {
		appendSavedLog(t, path, "concurrent\n")
		other := newPortalLogSource("row", path, "", registry, "")
		frame, err := other.Poll(false)
		other.Close()
		if err != nil || frame.Generation != initial.Generation || frame.End != 19 {
			t.Fatalf("concurrent connection = %#v, %v", frame, err)
		}
		slow.onHighwater = nil
	}
	frame, err := slow.Poll(false)
	if err != nil || frame.Type != "append" || frame.Start != initial.End || frame.End != initial.End || frame.Generation != initial.Generation {
		t.Fatalf("older captured highwater invented shrink: %#v, %v", frame, err)
	}
	frame, err = slow.Poll(false)
	if err != nil || frame.Type != "append" || frame.Generation != initial.Generation || !reflect.DeepEqual(recordTexts(frame), []string{"concurrent"}) {
		t.Fatalf("concurrent append lost at handoff: %#v, %v", frame, err)
	}
}

func TestPortalLogSource_InvalidCursorIdentitiesBoundsAndRecordBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("first\nsecond €\n"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := testPortalLogRegistry(t)
	source := newPortalLogSource("row", path, "", registry, "")
	initial, err := source.Poll(false)
	if err != nil {
		t.Fatal(err)
	}
	source.Close()
	proof, ok := registry.decode(initial.Cursor)
	if !ok {
		t.Fatal("server cursor cannot be decoded by its issuing process")
	}
	// Signed malformed payloads exercise bounds/boundary validation separately
	// from transport corruption. These cannot be minted by a browser.
	for _, invalid := range []string{"wrong-run", "wrong-generation", "negative-start", "past-eof", "midrecord-start", "midrecord-end", "utf8-split", "shortened-proof", "bad-digest"} {
		t.Run(invalid, func(t *testing.T) {
			bad := proof
			switch invalid {
			case "wrong-run":
				bad.RunID = "other"
			case "wrong-generation":
				bad.Generation = "other"
			case "negative-start":
				bad.Start = -1
			case "past-eof":
				bad.End += 100
			case "midrecord-start":
				bad.Start = 1
			case "midrecord-end":
				bad.End = 3
			case "utf8-split":
				bad.End = 15
			case "shortened-proof":
				bad.Start = 6
			case "bad-digest":
				bad.Digest = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
			}
			reconnected := newPortalLogSource("row", path, registry.encode(bad), registry, "")
			defer reconnected.Close()
			frame, err := reconnected.Poll(false)
			if err != nil || frame.Type != "reset" || frame.Start != 0 || !reflect.DeepEqual(recordTexts(frame), []string{"first", "second €"}) {
				t.Fatalf("bad proof silently accepted: %#v, %v", frame, err)
			}
		})
	}
}

func TestPortalLogSource_CatchupBeforeWithinAfterRetainedRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("initial\n"), 0600); err != nil {
		t.Fatal(err)
	}
	registry := testPortalLogRegistry(t)
	source := newPortalLogSource("row", path, "", registry, "")
	defer source.Close()
	old, err := source.Poll(false)
	if err != nil {
		t.Fatal(err)
	}
	appendSavedLog(t, path, strings.Repeat("record\n", 40*1024))
	reset, err := source.Poll(false)
	if err != nil || reset.Type != "reset" || reset.Reason != "catchup-bounded" || reset.Start <= old.End || reset.End-reset.Start > portalLogRetainedBytes {
		t.Fatalf("bounded gap must replace coherently: %#v, %v", reset, err)
	}
	appendSavedLog(t, path, "new\n")
	for _, tc := range []struct{ cursor, kind string }{{old.Cursor, "reset"}, {reset.Cursor, "append"}} {
		reconnected := newPortalLogSource("row", path, tc.cursor, registry, "")
		frame, err := reconnected.Poll(false)
		reconnected.Close()
		if err != nil || frame.Type != tc.kind {
			t.Fatalf("catchup = %#v, %v", frame, err)
		}
		if tc.kind == "append" && (frame.Start != reset.End || !reflect.DeepEqual(recordTexts(frame), []string{"new"})) {
			t.Fatalf("within retained range lost continuity: %#v", frame)
		}
		if tc.kind == "append" {
			idle := newPortalLogSource("row", path, frame.Cursor, registry, "")
			atEnd, err := idle.Poll(false)
			idle.Close()
			if err != nil || atEnd.Type != "append" || atEnd.Start != atEnd.End || len(atEnd.Records) != 0 {
				t.Fatalf("resume at highwater = %#v, %v", atEnd, err)
			}
		}
	}
}

func TestPortalLogSource_LiveGenerationResetAndTerminalFragment(t *testing.T) {
	for _, mutation := range []string{"replacement", "shrink-regrow", "rewrite"} {
		t.Run(mutation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "run.log")
			if err := os.WriteFile(path, []byte("first\nlast\n"), 0600); err != nil {
				t.Fatal(err)
			}
			source := newPortalLogSource("row", path, "", testPortalLogRegistry(t), "")
			defer source.Close()
			initial, err := source.Poll(false)
			if err != nil {
				t.Fatal(err)
			}
			original, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if mutation == "replacement" {
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
			} else if mutation == "shrink-regrow" {
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, []byte("other\nlast\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if mutation == "rewrite" {
				if err := os.Chtimes(path, original.ModTime(), original.ModTime()); err != nil {
					t.Fatal(err)
				}
			}
			reset, err := source.Poll(false)
			if err != nil || reset.Type != "reset" || reset.Generation == initial.Generation || !reflect.DeepEqual(recordTexts(reset), []string{"other", "last"}) {
				t.Fatalf("changed source appended to old generation: %#v, %v", reset, err)
			}
			appendSavedLog(t, path, "final")
			final, err := source.Poll(true)
			if err != nil || final.Start != reset.End || !reflect.DeepEqual(recordTexts(final), []string{"final"}) {
				t.Fatalf("terminal fragment = %#v, %v", final, err)
			}
		})
	}
}

func TestPortalLogSource_RecordAlignedBoundAndOversizedException(t *testing.T) {
	for _, tc := range []struct {
		name, raw, want, reason string
		start                   int64
	}{
		{"bounded suffix", strings.Repeat("old\n", 80*1024) + "new\n", strings.Repeat("old\n", 64*1024-1) + "new\n", "", 64*1024 + 4},
		{"oversized complete record", "old\n" + strings.Repeat("x", 300*1024) + "\n", strings.Repeat("x", 300*1024) + "\n", "oversized-record", 4},
		{"oversized incomplete record", "old\n" + strings.Repeat("x", 300*1024), "old\n", "", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "run.log")
			if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			source := newPortalLogSource("row", path, "", testPortalLogRegistry(t), "")
			defer source.Close()
			frame, err := source.Poll(false)
			if err != nil {
				t.Fatal(err)
			}
			if frame.Start != tc.start || frame.End != tc.start+int64(len(tc.want)) || frame.Reason != tc.reason || strings.Join(recordTexts(frame), "\n")+"\n" != tc.want {
				t.Fatalf("range [%d,%d) reason %q, records %d; want [%d,%d), reason %q", frame.Start, frame.End, frame.Reason, len(frame.Records), tc.start, tc.start+int64(len(tc.want)), tc.reason)
			}
		})
	}
}

func TestPortalLogSource_ResumeProvesWholeRetainedRange(t *testing.T) {
	for _, mutation := range []string{"append", "rename", "replace", "shrink", "truncate-regrow", "rewrite-unchanged-last-line", "restart", "invalid-token", "other-run"} {
		t.Run(mutation, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "run.log")
			if err := os.WriteFile(path, []byte("first\nunchanged\n"), 0600); err != nil {
				t.Fatal(err)
			}
			registry := testPortalLogRegistry(t)
			source := newPortalLogSource("row", path, "", registry, "repo")
			initial, err := source.Poll(false)
			if err != nil {
				t.Fatal(err)
			}
			source.Close()
			resume, runID := initial.Cursor, "row"
			wantReset := true
			switch mutation {
			case "append":
				appendSavedLog(t, path, "next\n")
				wantReset = false
			case "rename":
				newPath := path + ".archived"
				if err := os.Rename(path, newPath); err != nil {
					t.Fatal(err)
				}
				path = newPath
				appendSavedLog(t, path, "next\n")
				wantReset = false
			case "replace":
				if err := os.Rename(path, path+".old"); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("first\nunchanged\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "shrink":
				if err := os.Truncate(path, 6); err != nil {
					t.Fatal(err)
				}
			case "truncate-regrow", "rewrite-unchanged-last-line":
				if err := os.WriteFile(path, []byte("other\nunchanged\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "restart":
				registry = testPortalLogRegistry(t)
			case "invalid-token":
				resume += "invalid"
			case "other-run":
				runID = "sibling"
			}
			reconnected := newPortalLogSource(runID, path, resume, registry, "repo")
			defer reconnected.Close()
			frame, err := reconnected.Poll(false)
			if err != nil {
				t.Fatal(err)
			}
			if wantReset {
				if frame.Type != "reset" || frame.Start != 0 || frame.Reason == "" {
					t.Fatalf("invalid continuity must reset: %#v", frame)
				}
			} else if frame.Type != "append" || frame.Start != initial.End || frame.Generation != initial.Generation || !reflect.DeepEqual(recordTexts(frame), []string{"next"}) {
				t.Fatalf("valid resume = %#v", frame)
			}
		})
	}
}

func appendSavedLog(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func recordTexts(frame *portalLogFrame) []string {
	var texts []string
	for _, record := range frame.Records {
		texts = append(texts, record.Text)
	}
	return texts
}

func testPortalLogRegistry(t *testing.T) *portalLogRegistry {
	t.Helper()
	registry := newPortalLogRegistry()
	t.Cleanup(registry.Close)
	return registry
}

func BenchmarkPortalLogSource_IdleTail(b *testing.B) {
	path := filepath.Join(b.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("record\n", 300*1024)), 0600); err != nil {
		b.Fatal(err)
	}
	registry := newPortalLogRegistry()
	defer registry.Close()
	source := newPortalLogSource("row", path, "", registry, "")
	defer source.Close()
	if _, err := source.Poll(false); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		if frame, err := source.Poll(false); err != nil || frame != nil {
			b.Fatalf("idle tail emitted %#v: %v", frame, err)
		}
	}
}
