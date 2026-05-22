package daemon

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// LiveRun describes one live daemon instance.
type LiveRun struct {
	RunID     string    `json:"run_id"`
	PID       int       `json:"pid"`
	Issues    []int     `json:"issues"`
	StartedAt time.Time `json:"started_at"`
}

// LiveRunStore persists live daemon metadata under .sandman/runs.
type LiveRunStore struct {
	baseDir string
}

// NewLiveRunStore creates a store rooted at the .sandman directory.
func NewLiveRunStore(baseDir string) *LiveRunStore {
	return &LiveRunStore{baseDir: baseDir}
}

func (s *LiveRunStore) runsDir() string {
	return filepath.Join(s.baseDir, "runs")
}

// RunDir returns directory for one live run.
func (s *LiveRunStore) RunDir(runID string) string {
	return filepath.Join(s.runsDir(), runID)
}

// RunFile returns metadata file path for one live run.
func (s *LiveRunStore) RunFile(runID string) string {
	return filepath.Join(s.RunDir(runID), "run.json")
}

// SocketPath returns control socket path for one live run.
func (s *LiveRunStore) SocketPath(runID string) string {
	return filepath.Join(s.RunDir(runID), "run.sock")
}

// Register writes live metadata to disk.
func (s *LiveRunStore) Register(run LiveRun) error {
	if strings.TrimSpace(run.RunID) == "" {
		return fmt.Errorf("run id is required")
	}
	if err := os.MkdirAll(s.RunDir(run.RunID), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal live run: %w", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(s.RunFile(run.RunID), data, 0644); err != nil {
		return fmt.Errorf("write live run: %w", err)
	}
	return nil
}

// Load reads metadata for one live run.
func (s *LiveRunStore) Load(runID string) (*LiveRun, error) {
	data, err := os.ReadFile(s.RunFile(runID))
	if err != nil {
		return nil, err
	}
	var run LiveRun
	if err := json.Unmarshal(data, &run); err != nil {
		return nil, fmt.Errorf("unmarshal live run: %w", err)
	}
	if !s.pidAlive(run.PID) {
		_ = s.Remove(runID)
		return nil, os.ErrNotExist
	}
	return &run, nil
}

// List returns all live runs still backed by active processes.
func (s *LiveRunStore) List() ([]LiveRun, error) {
	entries, err := os.ReadDir(s.runsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var runs []LiveRun
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		run, err := s.Load(entry.Name())
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		runs = append(runs, *run)
	}

	sort.Slice(runs, func(i, j int) bool {
		if !runs[i].StartedAt.Equal(runs[j].StartedAt) {
			return runs[i].StartedAt.Before(runs[j].StartedAt)
		}
		return runs[i].RunID < runs[j].RunID
	})

	return runs, nil
}

// Remove deletes one live run directory.
func (s *LiveRunStore) Remove(runID string) error {
	if err := os.RemoveAll(s.RunDir(runID)); err != nil {
		return err
	}
	return nil
}

func (s *LiveRunStore) pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	return true
}

// NewRunID reuses Sandman's existing run-id format.
func NewRunID(issues []int) string {
	issueNum := 0
	if len(issues) > 0 {
		issueNum = issues[0]
	}
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "run-" + strconv.Itoa(issueNum) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return "run-" + strconv.Itoa(issueNum) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10) + "-" + hex.EncodeToString(buf)
}
