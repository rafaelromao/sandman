package cmd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPortalLogSource_SnapshotAndTailShareRawPositions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	initial := "[run-1] 10:00:00 same\r\n[run-1] 10:00:01 same\n[run-1] \n"
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}
	source, err := newPortalLogSource(path, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	snapshot, err := source.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records) != 3 {
		t.Fatalf("snapshot records = %d, want 3", len(snapshot.Records))
	}
	if snapshot.Records[0].Text != "10:00:00 same" || snapshot.Records[1].Text != "10:00:01 same" || snapshot.Records[2].Text != "" {
		t.Fatalf("snapshot text = %#v, want cleaned repeated and blank records", snapshot.Records)
	}
	if snapshot.Records[0].Start != 0 || snapshot.Records[0].End != int64(len("[run-1] 10:00:00 same\r\n")) {
		t.Fatalf("first raw range = [%d,%d), want record-aligned byte range", snapshot.Records[0].Start, snapshot.Records[0].End)
	}
	if snapshot.Cursor.Offset != snapshot.End || snapshot.End != int64(len(initial)) {
		t.Fatalf("snapshot cursor = %#v, end=%d, file size=%d", snapshot.Cursor, snapshot.End, len(initial))
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[run-1] 10:00:02 same\n"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	_ = f.Close()

	batch, changed, err := source.appendBatch(false)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(batch.Records) != 1 {
		t.Fatalf("tail batch = %#v, changed=%t, want one appended record", batch, changed)
	}
	if batch.Records[0].Start != snapshot.End || batch.Records[0].Text != "10:00:02 same" {
		t.Fatalf("tail record = %#v, want contiguous raw position and repeated text", batch.Records[0])
	}
}

func TestPortalLogSource_ResumeRejectsNonBoundaryCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("[run-1] first\n[run-1] second\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	source, err := newPortalLogSource(path, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	_, end, err := source.records(0, 5, false)
	if err != nil {
		t.Fatal(err)
	}
	if end == 5 {
		t.Fatal("test cursor unexpectedly landed on a record boundary")
	}
	_, boundaryEnd, err := source.records(0, int64(len("[run-1] first\n")), false)
	if err != nil || boundaryEnd != int64(len("[run-1] first\n")) {
		t.Fatalf("expected first record boundary, got end=%d err=%v", boundaryEnd, err)
	}
}

func TestPortalLogSource_SnapshotUsesExplicitRecordAlignedBoundedRange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	var content strings.Builder
	for i := 0; i < 40000; i++ {
		content.WriteString("[run-1] record-")
		content.WriteString(strings.Repeat("x", 8))
		content.WriteByte('\n')
	}
	if err := os.WriteFile(path, []byte(content.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	source, err := newPortalLogSource(path, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	snapshot, err := source.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Bounded || snapshot.Start <= 0 || snapshot.Start != snapshot.Records[0].Start {
		t.Fatalf("snapshot range = %#v, want explicit record-aligned bounded start", snapshot)
	}
	if snapshot.End != snapshot.Cursor.Offset {
		t.Fatalf("snapshot end=%d cursor=%d, want same committed position", snapshot.End, snapshot.Cursor.Offset)
	}
}

func TestPortalLogSource_ResumeRejectsCursorBeyondFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("[run-1] first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	source, err := newPortalLogSource(path, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	initial := &portalLogCursor{RunID: "run-1", Generation: source.gen, Offset: int64(len("[run-1] first\n")) + 1}
	var output strings.Builder
	if err := streamPortalSavedLog(context.Background(), &output, source, initial, func() bool { return true }); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "event: reset") || !strings.Contains(output.String(), "event: snapshot") {
		t.Fatalf("out-of-range cursor did not produce reset and snapshot: %s", output.String())
	}
}

func TestPortalLogSource_TerminalDrainAcceptsFinalUnterminatedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("[run-1] first\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	source, err := newPortalLogSource(path, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	snapshot, err := source.snapshot()
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("[run-1] final"); err != nil {
		_ = f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	batch, changed, err := source.appendBatch(true)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(batch.Records) != 1 || batch.Records[0].Start != snapshot.End || batch.Records[0].Text != "final" {
		t.Fatalf("terminal drain batch = %#v, changed=%t, want final unterminated record", batch, changed)
	}
}

func TestStreamPortalSavedLog_UsesOneTerminalObservationForDrainAndEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "run.log")
	if err := os.WriteFile(path, []byte("[run-1] first\n[run-1] final"), 0o644); err != nil {
		t.Fatal(err)
	}
	source, err := newPortalLogSource(path, "run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()

	var output strings.Builder
	observations := 0
	terminal := func() bool {
		observations++
		return observations >= 2
	}
	if err := streamPortalSavedLog(context.Background(), &output, source, nil, terminal); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"text":"final"`) {
		t.Fatalf("final record missing from terminal stream: %s", output.String())
	}
	finalIndex := strings.Index(output.String(), `"text":"final"`)
	endIndex := strings.Index(output.String(), "event: end\n")
	if endIndex <= finalIndex {
		t.Fatalf("terminal stream ended before final drain: %s", output.String())
	}
}
