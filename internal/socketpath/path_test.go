package socketpath

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestPath_ShortLogicalPathIsReturnedVerbatim(t *testing.T) {
	cases := []string{
		"/tmp/sandman.sock",
		filepath.Join("/home", "user", ".sandman", "batches", "abc", "batch.sock"),
		"/var/folders/ab/T/sandman-test-123/batch.sock",
	}
	for _, in := range cases {
		if got := Path(in); got != in {
			t.Errorf("Path(%q) = %q, want %q (short path should be returned verbatim)", in, got, in)
		}
	}
}

func TestPath_LongLogicalPathIsShortenedOnEveryOS(t *testing.T) {
	long := "/some/" + strings.Repeat("long-path-segment-", 6) + "/.sandman/batches/abc/batch.sock"
	if len(long) <= sunPathLimit {
		t.Fatalf("test fixture is too short to exercise the long-path case: len=%d", len(long))
	}

	got := Path(long)
	if got == long {
		t.Fatalf("Path(%q) returned the long path verbatim on %s; want short filesystem path", long, runtime.GOOS)
	}
	if !strings.HasPrefix(got, "/tmp/") {
		t.Errorf("Path(%q) = %q, want prefix /tmp/", long, got)
	}
	if !strings.HasSuffix(got, ".sock") {
		t.Errorf("Path(%q) = %q, want .sock suffix", long, got)
	}
	if len(got) > sunPathLimit {
		t.Errorf("Path(%q) = %q, length %d exceeds sun_path %d", long, got, len(got), sunPathLimit)
	}
}

func TestPath_LongLogicalPathIsDeterministic(t *testing.T) {
	long := "/some/" + strings.Repeat("long-path-segment-", 6) + "/.sandman/batches/abc/batch.sock"

	first := Path(long)
	second := Path(long)
	third := Path(long)
	if first != second || second != third {
		t.Fatalf("Path(%q) is not deterministic: %q, %q, %q", long, first, second, third)
	}
}

func TestPath_DistinctLongPathsProduceDistinctShortPaths(t *testing.T) {
	a := "/some/" + strings.Repeat("alpha-", 20) + "/.sandman/batches/abc/batch.sock"
	b := "/some/" + strings.Repeat("beta-", 20) + "/.sandman/batches/abc/batch.sock"
	if len(a) <= sunPathLimit || len(b) <= sunPathLimit {
		t.Fatalf("test fixture is too short: a=%d b=%d", len(a), len(b))
	}

	sa := Path(a)
	sb := Path(b)
	if sa == sb {
		t.Fatalf("Path(%q) == Path(%q) = %q; expected distinct collision-resistant short paths", a, b, sa)
	}
}

func TestPath_LongLogicalPathHashesFullPath(t *testing.T) {
	commonPrefix := "/var/folders/very-long-prefix-with-padding-to-overflow-sunpath-zzzzzzzzzzzz/"
	differentTailA := commonPrefix + "branch-A/" + strings.Repeat("x", 30) + "/run.sock"
	differentTailB := commonPrefix + "branch-B/" + strings.Repeat("x", 30) + "/run.sock"
	if len(differentTailA) <= sunPathLimit || len(differentTailB) <= sunPathLimit {
		t.Fatalf("test fixture is too short: a=%d b=%d", len(differentTailA), len(differentTailB))
	}

	sa := Path(differentTailA)
	sb := Path(differentTailB)
	if sa == sb {
		t.Fatalf("Path(%q) == Path(%q) = %q; the resolver hashed the directory basename, not the full path (issue #1547 collision risk)", differentTailA, differentTailB, sa)
	}
}
