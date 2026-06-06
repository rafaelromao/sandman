package batch

import (
	"context"
	"os"
	"time"
)

const defaultHeartbeatTickInterval = 30 * time.Second

// Heartbeat monitors a worktree log file's mtime. When the mtime stops
// advancing for IdleTimeout, the heartbeat calls processKill and exits.
//
// OnIdle is invoked once with the elapsed idle duration immediately before
// processKill. Both OnIdle and processKill may be nil for unit tests that
// only want to observe behavior.
//
// Each call to Run reads the current mtime at startup so callers can spawn
// a fresh Heartbeat per retry attempt without inheriting stale state.
type Heartbeat struct {
	LogPath      string
	IdleTimeout  time.Duration
	TickInterval time.Duration
	OnIdle       func(idle time.Duration)

	now  func() time.Time
	stat func(string) (os.FileInfo, error)
}

func (h *Heartbeat) tickInterval() time.Duration {
	if h.TickInterval > 0 {
		return h.TickInterval
	}
	return defaultHeartbeatTickInterval
}

func (h *Heartbeat) clock() func() time.Time {
	if h.now != nil {
		return h.now
	}
	return time.Now
}

func (h *Heartbeat) statFn() func(string) (os.FileInfo, error) {
	if h.stat != nil {
		return h.stat
	}
	return os.Stat
}

func (h *Heartbeat) readMtime() time.Time {
	info, err := h.statFn()(h.LogPath)
	if err != nil {
		return time.Time{}
	}
	return info.ModTime()
}

func (h *Heartbeat) mtimeAdvanced(last time.Time) (time.Time, bool) {
	current := h.readMtime()
	if current.After(last) {
		return current, true
	}
	return last, false
}

func (h *Heartbeat) Run(ctx context.Context, processKill func() error) error {
	if h == nil || h.LogPath == "" || h.IdleTimeout <= 0 {
		return nil
	}

	now := h.clock()
	tick := time.NewTicker(h.tickInterval())
	defer tick.Stop()

	lastMtime := h.readMtime()
	lastAdvanced := now()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			if mtime, advanced := h.mtimeAdvanced(lastMtime); advanced {
				lastMtime = mtime
				lastAdvanced = now()
				continue
			}
			idle := now().Sub(lastAdvanced)
			if idle < h.IdleTimeout {
				continue
			}
			if h.OnIdle != nil {
				h.OnIdle(idle)
			}
			if processKill != nil {
				_ = processKill()
			}
			return nil
		}
	}
}
