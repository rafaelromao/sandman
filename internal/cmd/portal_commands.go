package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type portalCommandRecord struct {
	ID        string    `json:"id"`
	Command   string    `json:"command"`
	Args      []string  `json:"args,omitempty"`
	Status    string    `json:"status"`
	StartedAt time.Time `json:"startedAt"`
	RepoRoot  string    `json:"repoRoot"`
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

type portalLauncher struct {
	repoRoot string
	store    *portalCommandStore
	mu       sync.Mutex
	records  []portalCommandRecord
}

var portalStartCommand = startPortalCommand

func newPortalLauncher(repoRoot string) (*portalLauncher, error) {
	launcher := &portalLauncher{
		repoRoot: repoRoot,
		store:    &portalCommandStore{Path: filepath.Join(repoRoot, ".sandman", "portal", "commands.json")},
	}
	records, err := launcher.store.Read()
	if err != nil {
		return nil, err
	}
	launcher.records = records
	sort.SliceStable(launcher.records, func(i, j int) bool {
		if !launcher.records[i].StartedAt.Equal(launcher.records[j].StartedAt) {
			return launcher.records[i].StartedAt.After(launcher.records[j].StartedAt)
		}
		return launcher.records[i].ID > launcher.records[j].ID
	})
	return launcher, nil
}

func (l *portalLauncher) list() ([]portalCommandRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]portalCommandRecord(nil), l.records...), nil
}

func (l *portalLauncher) launch(args []string) (portalCommandRecord, error) {
	if len(args) == 0 {
		return portalCommandRecord{}, fmt.Errorf("missing command args")
	}
	record := portalCommandRecord{
		ID:        nextPortalCommandID(),
		Command:   strings.Join(append([]string{"sandman"}, args...), " "),
		Args:      append([]string(nil), args...),
		Status:    "running",
		StartedAt: time.Now(),
		RepoRoot:  l.repoRoot,
	}
	if err := portalStartCommand(context.Background(), l.repoRoot, args); err != nil {
		return portalCommandRecord{}, err
	}
	l.mu.Lock()
	l.records = append([]portalCommandRecord{record}, l.records...)
	if err := l.store.Write(l.records); err != nil {
		l.mu.Unlock()
		return portalCommandRecord{}, err
	}
	l.mu.Unlock()
	return record, nil
}

func startPortalCommand(ctx context.Context, repoRoot string, args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve sandman executable: %w", err)
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = repoRoot
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sandman command: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func nextPortalCommandID() string {
	return "cmd-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}
