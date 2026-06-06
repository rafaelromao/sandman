package batch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeFileInfo struct {
	mtime time.Time
}

func (f fakeFileInfo) Name() string       { return "log" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() os.FileMode  { return 0644 }
func (f fakeFileInfo) ModTime() time.Time { return f.mtime }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

type statStub struct {
	mu       sync.Mutex
	mtime    time.Time
	err      error
	calls    int
	advancer func() time.Time
}

func (s *statStub) stat(path string) (os.FileInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	mtime := s.mtime
	if s.advancer != nil {
		mtime = s.advancer()
	}
	return fakeFileInfo{mtime: mtime}, nil
}

func (s *statStub) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func TestHeartbeat_DisabledWhenIdleTimeoutZero(t *testing.T) {
	stat := &statStub{mtime: time.Now()}
	hb := &Heartbeat{
		LogPath:      "/tmp/log",
		IdleTimeout:  0,
		TickInterval: 10 * time.Millisecond,
		stat:         stat.stat,
	}
	kill := atomic.Bool{}

	if err := hb.Run(context.Background(), func() error { kill.Store(true); return nil }); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if kill.Load() {
		t.Fatal("expected processKill to NOT be called when IdleTimeout is zero")
	}
	if stat.Calls() != 0 {
		t.Errorf("expected no stat calls, got %d", stat.Calls())
	}
}

func TestHeartbeat_DisabledWhenLogPathEmpty(t *testing.T) {
	hb := &Heartbeat{
		LogPath:      "",
		IdleTimeout:  50 * time.Millisecond,
		TickInterval: 10 * time.Millisecond,
		stat:         (&statStub{}).stat,
	}
	kill := atomic.Bool{}
	if err := hb.Run(context.Background(), func() error { kill.Store(true); return nil }); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if kill.Load() {
		t.Fatal("expected processKill to NOT be called when LogPath is empty")
	}
}

func TestHeartbeat_FiresAfterIdle(t *testing.T) {
	stat := &statStub{mtime: time.Now().Add(-time.Hour)}
	hb := &Heartbeat{
		LogPath:      "/tmp/log",
		IdleTimeout:  50 * time.Millisecond,
		TickInterval: 10 * time.Millisecond,
		stat:         stat.stat,
	}
	var (
		mu        sync.Mutex
		idleSeen  time.Duration
		onIdleHit bool
	)
	hb.OnIdle = func(idle time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		onIdleHit = true
		idleSeen = idle
	}
	kill := atomic.Bool{}
	start := time.Now()
	if err := hb.Run(context.Background(), func() error { kill.Store(true); return nil }); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	elapsed := time.Since(start)
	if !kill.Load() {
		t.Fatal("expected processKill to be called after idle timeout")
	}
	if !onIdleHit {
		t.Fatal("expected OnIdle to be invoked before processKill")
	}
	mu.Lock()
	got := idleSeen
	mu.Unlock()
	if got < 50*time.Millisecond {
		t.Errorf("OnIdle idle duration = %v, want >= 50ms", got)
	}
	if got > 250*time.Millisecond {
		t.Errorf("OnIdle idle duration = %v, want ~ idle threshold", got)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("heartbeat took %v to fire, want < 500ms", elapsed)
	}
}

func TestHeartbeat_DoesNotFireWhenLogAdvances(t *testing.T) {
	mtime := time.Now().Add(-time.Hour)
	stat := &statStub{
		advancer: func() time.Time {
			mtime = mtime.Add(5 * time.Millisecond)
			return mtime
		},
	}
	hb := &Heartbeat{
		LogPath:      "/tmp/log",
		IdleTimeout:  200 * time.Millisecond,
		TickInterval: 10 * time.Millisecond,
		stat:         stat.stat,
	}
	kill := atomic.Bool{}
	hb.OnIdle = func(time.Duration) { kill.Store(true) }

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := hb.Run(ctx, func() error { kill.Store(true); return nil }); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if kill.Load() {
		t.Fatal("expected processKill to NOT be called when log keeps advancing")
	}
}

func TestHeartbeat_StopsOnContextCancel(t *testing.T) {
	stat := &statStub{mtime: time.Now().Add(-time.Hour)}
	hb := &Heartbeat{
		LogPath:      "/tmp/log",
		IdleTimeout:  10 * time.Second,
		TickInterval: 10 * time.Millisecond,
		stat:         stat.stat,
	}
	kill := atomic.Bool{}
	hb.OnIdle = func(time.Duration) { kill.Store(true) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hb.Run(ctx, func() error { kill.Store(true); return nil }) }()

	time.Sleep(40 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned %v, want nil", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return after context cancel")
	}
	if kill.Load() {
		t.Fatal("expected processKill to NOT be called when ctx cancels before idle")
	}
}

func TestHeartbeat_ResetsLastMtimeOnEachRun(t *testing.T) {
	stat := &statStub{mtime: time.Now().Add(-time.Hour)}
	hb := &Heartbeat{
		LogPath:      "/tmp/log",
		IdleTimeout:  10 * time.Second,
		TickInterval: 10 * time.Millisecond,
		stat:         stat.stat,
	}
	kill := atomic.Bool{}
	hb.OnIdle = func(time.Duration) { kill.Store(true) }

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := hb.Run(ctx, func() error { kill.Store(true); return nil }); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if kill.Load() {
		t.Fatal("expected no kill for fresh run with fresh lastMtime")
	}
}

func TestHeartbeat_StatErrorTreatedAsNoAdvance(t *testing.T) {
	stat := &statStub{err: errors.New("stat boom")}
	hb := &Heartbeat{
		LogPath:      "/tmp/log",
		IdleTimeout:  50 * time.Millisecond,
		TickInterval: 10 * time.Millisecond,
		stat:         stat.stat,
	}
	kill := atomic.Bool{}
	hb.OnIdle = func(time.Duration) {}
	if err := hb.Run(context.Background(), func() error { kill.Store(true); return nil }); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if !kill.Load() {
		t.Fatal("expected processKill to be called when stat always errors (treated as no advance)")
	}
}

func TestHeartbeat_RealLogFileInTempDir(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	if err := os.WriteFile(logPath, []byte("first line\n"), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	if err := os.Chtimes(logPath, past, past); err != nil {
		t.Fatalf("chtimes: %v", err)
	}
	hb := &Heartbeat{
		LogPath:      logPath,
		IdleTimeout:  50 * time.Millisecond,
		TickInterval: 10 * time.Millisecond,
	}
	kill := atomic.Bool{}
	hb.OnIdle = func(time.Duration) {}
	if err := hb.Run(context.Background(), func() error { kill.Store(true); return nil }); err != nil {
		t.Fatalf("Run returned %v, want nil", err)
	}
	if !kill.Load() {
		t.Fatal("expected kill when real log file is stale")
	}
}

func TestHeartbeat_RealLogAdvancesSuppressesKill(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "agent.log")
	if err := os.WriteFile(logPath, []byte("first line\n"), 0644); err != nil {
		t.Fatalf("write log: %v", err)
	}
	hb := &Heartbeat{
		LogPath:      logPath,
		IdleTimeout:  300 * time.Millisecond,
		TickInterval: 10 * time.Millisecond,
	}
	kill := atomic.Bool{}
	hb.OnIdle = func(time.Duration) { kill.Store(true) }

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		_ = hb.Run(ctx, func() error { kill.Store(true); return nil })
		close(done)
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(20 * time.Millisecond):
				f, err := os.OpenFile(logPath, os.O_APPEND|os.O_WRONLY, 0644)
				if err == nil {
					_, _ = f.WriteString("more output\n")
					_ = f.Close()
				}
			}
		}
	}()
	time.Sleep(120 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("heartbeat did not stop on ctx cancel")
	}
	if kill.Load() {
		t.Fatal("expected no kill while log file keeps advancing")
	}
}
