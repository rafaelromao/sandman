package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type portalCommandRecord struct {
	ID          string     `json:"id"`
	Command     string     `json:"command"`
	Status      string     `json:"status"`
	StartedAt   time.Time  `json:"startedAt"`
	FinishedAt  *time.Time `json:"finishedAt,omitempty"`
	Duration    string     `json:"duration,omitempty"`
	PID         int        `json:"pid,omitempty"`
	ExitCode    *int       `json:"exitCode,omitempty"`
	LogPath     string     `json:"logPath,omitempty"`
	LogURL      string     `json:"logUrl,omitempty"`
	Output      string     `json:"output,omitempty"`
	RelaunchOf  string     `json:"relaunchOf,omitempty"`
	WorkingDir  string     `json:"workingDir"`
	FinishedMsg string     `json:"finishedMsg,omitempty"`
}

type portalCommandStore struct {
	Path string
}

func (s *portalCommandStore) Read() ([]portalCommandRecord, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read command store: %w", err)
	}

	var records []portalCommandRecord
	if err := json.Unmarshal(data, &records); err != nil {
		return nil, fmt.Errorf("decode command store: %w", err)
	}
	return records, nil
}

func (s *portalCommandStore) Write(records []portalCommandRecord) error {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0755); err != nil {
		return fmt.Errorf("create command store dir: %w", err)
	}

	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		return fmt.Errorf("encode command store: %w", err)
	}
	data = append(data, '\n')

	tmp, err := os.CreateTemp(filepath.Dir(s.Path), filepath.Base(s.Path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create command store temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write command store temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close command store temp file: %w", err)
	}
	if err := os.Rename(tmpName, s.Path); err != nil {
		return fmt.Errorf("replace command store: %w", err)
	}
	return nil
}

type portalCommandProcess struct {
	cmd     *exec.Cmd
	logFile *os.File
}

type portalLauncher struct {
	repoRoot string
	store    *portalCommandStore
	mu       sync.Mutex
	records  []portalCommandRecord
	running  map[string]*portalCommandProcess
}

func newPortalLauncher(repoRoot string) (*portalLauncher, error) {
	launcher := &portalLauncher{
		repoRoot: repoRoot,
		store:    &portalCommandStore{Path: filepath.Join(repoRoot, ".sandman", "portal", "commands.json")},
		running:  make(map[string]*portalCommandProcess),
	}
	records, err := launcher.store.Read()
	if err != nil {
		return nil, err
	}
	launcher.records = records
	launcher.sortRecordsLocked()
	return launcher, nil
}

func (l *portalLauncher) list() ([]portalCommandRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.refreshLoadedRecordsLocked(); err != nil {
		return nil, err
	}
	return l.decoratedRecordsLocked(), nil
}

func (l *portalLauncher) launch(command string) (portalCommandRecord, error) {
	return l.launchWithRelaunchOf(command, "")
}

func (l *portalLauncher) launchWithRelaunchOf(command, relaunchOf string) (portalCommandRecord, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return portalCommandRecord{}, fmt.Errorf("missing command")
	}

	record := portalCommandRecord{
		ID:         l.nextID(),
		Command:    command,
		Status:     "running",
		StartedAt:  time.Now(),
		WorkingDir: l.repoRoot,
		RelaunchOf: relaunchOf,
	}
	record.LogPath = portalLogPath(l.repoRoot, 0, "portal-"+record.ID)
	record.LogURL = portalLogDownloadURL(l.repoRoot, 0, "portal-"+record.ID)

	if err := os.MkdirAll(filepath.Dir(record.LogPath), 0755); err != nil {
		return portalCommandRecord{}, fmt.Errorf("create command log dir: %w", err)
	}
	logFile, err := os.OpenFile(record.LogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		return portalCommandRecord{}, fmt.Errorf("open command log: %w", err)
	}

	cmd := exec.Command("sh", "-lc", command)
	cmd.Dir = l.repoRoot
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return portalCommandRecord{}, fmt.Errorf("start command: %w", err)
	}
	record.PID = cmd.Process.Pid

	l.mu.Lock()
	l.records = append([]portalCommandRecord{record}, l.records...)
	l.running[record.ID] = &portalCommandProcess{cmd: cmd, logFile: logFile}
	if err := l.saveLocked(); err != nil {
		delete(l.running, record.ID)
		l.records = removePortalCommandRecord(l.records, record.ID)
		l.mu.Unlock()
		_ = l.killCommandProcess(cmd.Process.Pid)
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
		return portalCommandRecord{}, err
	}
	decorated := l.decorateRecordLocked(record)
	l.mu.Unlock()

	go l.waitForCommand(record.ID, cmd, logFile)
	return decorated, nil
}

func (l *portalLauncher) stop(id string) (portalCommandRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	idx := l.findRecordIndexLocked(id)
	if idx < 0 {
		return portalCommandRecord{}, fmt.Errorf("command %s not found", id)
	}
	record := l.records[idx]
	if record.Status != "running" {
		return l.decorateRecordLocked(record), nil
	}

	pid := record.PID
	if proc := l.running[id]; proc != nil && proc.cmd != nil && proc.cmd.Process != nil {
		pid = proc.cmd.Process.Pid
	}
	if pid > 0 {
		if err := signalPortalProcessGroup(pid, syscall.SIGTERM); err != nil {
			if !processAlive(pid) {
				finishedAt := time.Now()
				record.Status = "exited"
				record.FinishedAt = &finishedAt
				record.Duration = time.Since(record.StartedAt).Round(time.Second).String()
				l.records[idx] = record
				_ = l.saveLocked()
				return l.decorateRecordLocked(record), fmt.Errorf("stop command: %w", err)
			}
			return l.decorateRecordLocked(record), fmt.Errorf("stop command: %w", err)
		}
	}
	finishedAt := time.Now()
	record.Status = "stopped"
	record.FinishedAt = &finishedAt
	record.Duration = time.Since(record.StartedAt).Round(time.Second).String()
	l.records[idx] = record
	_ = l.saveLocked()
	return l.decorateRecordLocked(record), nil
}

func (l *portalLauncher) relaunch(id string) (portalCommandRecord, error) {
	l.mu.Lock()
	idx := l.findRecordIndexLocked(id)
	if idx < 0 {
		l.mu.Unlock()
		return portalCommandRecord{}, fmt.Errorf("command %s not found", id)
	}
	command := l.records[idx].Command
	l.mu.Unlock()

	return l.launchWithRelaunchOf(command, id)
}

func (l *portalLauncher) waitForCommand(id string, cmd *exec.Cmd, logFile *os.File) {
	defer logFile.Close()
	err := cmd.Wait()

	l.mu.Lock()
	defer l.mu.Unlock()
	idx := l.findRecordIndexLocked(id)
	if idx < 0 {
		delete(l.running, id)
		return
	}
	record := l.records[idx]
	record.FinishedAt = ptrTime(time.Now())
	record.Duration = time.Since(record.StartedAt).Round(time.Second).String()
	record.PID = cmd.Process.Pid
	status, exitCode := portalCommandStatusFromWait(err)
	record.Status = status
	record.ExitCode = exitCode
	l.records[idx] = record
	delete(l.running, id)
	_ = l.saveLocked()
}

func (l *portalLauncher) refreshLoadedRecordsLocked() error {
	changed := false
	for i := range l.records {
		record := &l.records[i]
		if record.Status != "running" || record.PID <= 0 {
			continue
		}
		if processAlive(record.PID) {
			continue
		}
		finishedAt := time.Now()
		record.Status = "exited"
		record.FinishedAt = &finishedAt
		record.Duration = time.Since(record.StartedAt).Round(time.Second).String()
		changed = true
	}
	if changed {
		if err := l.saveLocked(); err != nil {
			return err
		}
	}
	return nil
}

func (l *portalLauncher) decoratedRecordsLocked() []portalCommandRecord {
	records := make([]portalCommandRecord, len(l.records))
	for i, record := range l.records {
		records[i] = l.decorateRecordLocked(record)
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].Status == "running" && records[j].Status != "running" {
			return true
		}
		if records[i].Status != "running" && records[j].Status == "running" {
			return false
		}
		if !records[i].StartedAt.Equal(records[j].StartedAt) {
			return records[i].StartedAt.After(records[j].StartedAt)
		}
		return records[i].ID > records[j].ID
	})
	return records
}

func (l *portalLauncher) decorateRecordLocked(record portalCommandRecord) portalCommandRecord {
	record.LogURL = portalLogDownloadURL(l.repoRoot, 0, "portal-"+record.ID)
	record.LogPath = portalLogPath(l.repoRoot, 0, "portal-"+record.ID)
	record.Output = readPortalTextFile(record.LogPath)
	if record.Status == "running" && record.StartedAt.IsZero() == false && record.Duration == "" {
		record.Duration = time.Since(record.StartedAt).Round(time.Second).String()
	}
	return record
}

func (l *portalLauncher) saveLocked() error {
	records := make([]portalCommandRecord, len(l.records))
	for i, record := range l.records {
		record.Output = ""
		record.LogURL = ""
		records[i] = record
	}
	return l.store.Write(records)
}

func (l *portalLauncher) findRecordIndexLocked(id string) int {
	for i := range l.records {
		if l.records[i].ID == id {
			return i
		}
	}
	return -1
}

func (l *portalLauncher) sortRecordsLocked() {
	sort.SliceStable(l.records, func(i, j int) bool {
		if !l.records[i].StartedAt.Equal(l.records[j].StartedAt) {
			return l.records[i].StartedAt.After(l.records[j].StartedAt)
		}
		return l.records[i].ID > l.records[j].ID
	})
}

func (l *portalLauncher) nextID() string {
	return "cmd-" + strconv.FormatInt(time.Now().UnixNano(), 36) + "-" + strconv.Itoa(os.Getpid())
}

func (l *portalLauncher) killCommandProcess(pid int) error {
	if pid <= 0 {
		return nil
	}
	return signalPortalProcessGroup(pid, syscall.SIGKILL)
}

func signalPortalProcessGroup(pid int, sig syscall.Signal) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, sig); err == nil || errors.Is(err, os.ErrProcessDone) {
		return nil
	} else {
		return err
	}
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}

func portalCommandStatusFromWait(err error) (string, *int) {
	if err == nil {
		code := 0
		return "success", &code
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return "failure", nil
	}
	if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
		if status.Signaled() {
			sig := status.Signal()
			if sig == syscall.SIGTERM || sig == syscall.SIGKILL {
				code := -int(sig)
				return "stopped", &code
			}
			code := 128 + int(sig)
			return "failure", &code
		}
		code := status.ExitStatus()
		if code == 0 {
			return "success", &code
		}
		return "failure", &code
	}
	return "failure", nil
}

func ptrTime(t time.Time) *time.Time { return &t }

func removePortalCommandRecord(records []portalCommandRecord, id string) []portalCommandRecord {
	for i := range records {
		if records[i].ID == id {
			return append(records[:i], records[i+1:]...)
		}
	}
	return records
}
