package subagent

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DiscoverDBPath discovers the opencode database path by running "opencode db path".
func DiscoverDBPath() (string, error) {
	out, err := exec.Command("opencode", "db", "path").Output()
	if err != nil {
		return "", fmt.Errorf("discover opencode db path: %w", err)
	}
	path := strings.TrimSpace(string(out))
	if path == "" {
		return "", fmt.Errorf("opencode db path returned empty")
	}
	return path, nil
}

const defaultPollInterval = 2 * time.Second

func NewDBPoller(issue int) *DBPoller {
	return &DBPoller{
		runner:    opencodeDBRunner,
		issue:     issue,
		writeFile: osWriteFile,
	}
}

func opencodeDBRunner(args ...string) ([]byte, error) {
	return exec.Command("opencode", args...).Output()
}

func escapeSQLString(s string) string {
	return strings.ReplaceAll(s, "'", "''")
}

func osWriteFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

type DBPoller struct {
	runner       func(args ...string) ([]byte, error)
	events       chan<- Event
	issue        int
	writeFile    func(path string, data []byte) error
	pollInterval time.Duration

	mu       sync.Mutex
	dbPath   string
	parentID string
	seen     map[string]bool
	lastSeen map[string]int64
	finished map[string]bool
	started  map[string]SessionOutput
	order    []string
	stopped  bool
	wg       sync.WaitGroup
}

type childSessionRow struct {
	ID          string          `json:"id"`
	Title       string          `json:"title"`
	Agent       string          `json:"agent"`
	TimeCreated json.RawMessage `json:"time_created"`
	TimeUpdated int64           `json:"time_updated"`
}

type messageRow struct {
	ID        string `json:"id"`
	SessionID string `json:"session_id"`
	Data      string `json:"data"`
}

type partRow struct {
	ID        string `json:"id"`
	MessageID string `json:"message_id"`
	SessionID string `json:"session_id"`
	Data      string `json:"data"`
}

type messageData struct {
	Role string `json:"role"`
}

type partData struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Tool   string `json:"tool"`
	Input  string `json:"input"`
	Output string `json:"output"`
}

func (p *DBPoller) Start(parentID string) {
	p.mu.Lock()
	p.parentID = parentID
	p.seen = make(map[string]bool)
	p.lastSeen = make(map[string]int64)
	p.finished = make(map[string]bool)
	p.started = make(map[string]SessionOutput)
	p.order = nil
	p.mu.Unlock()

	_, err := p.discoverDBPath()
	if err != nil {
		log.Printf("subagent DB: failed to discover DB path: %v — disabling subagent capture", err)
		return
	}

	p.wg.Add(1)
	go p.pollLoop()
}

func (p *DBPoller) emitSubagentStart(s SessionOutput) {
	if p.events == nil {
		return
	}
	select {
	case p.events <- Event{
		SessionID: s.SessionID,
		ParentID:  p.parentID,
		Type:      EventSubagentStart,
		Agent:     s.Agent,
		Title:     s.Title,
		Timestamp: time.Now(),
	}:
	default:
	}
}

func (p *DBPoller) emitSubagentFinishLocked(id string) {
	if p.events == nil {
		return
	}
	s, ok := p.started[id]
	if !ok {
		return
	}
	select {
	case p.events <- Event{
		SessionID: id,
		ParentID:  p.parentID,
		Type:      EventSubagentFinish,
		Agent:     s.Agent,
		Timestamp: time.Now(),
	}:
	default:
	}
}

func (p *DBPoller) Stop() []SessionOutput {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.wg.Wait()

	p.mu.Lock()
	outputs := make([]SessionOutput, 0, len(p.order))
	for _, id := range p.order {
		if s, ok := p.started[id]; ok {
			outputs = append(outputs, s)
		}
	}
	p.mu.Unlock()
	return outputs
}

func (p *DBPoller) pollLoop() {
	defer p.wg.Done()

	interval := p.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for range ticker.C {
		p.mu.Lock()
		if p.stopped {
			p.mu.Unlock()
			return
		}
		parentID := p.parentID
		p.mu.Unlock()

		p.pollOnce(parentID)
	}
}

func (p *DBPoller) pollOnce(parentID string) {
	sessions, err := p.querySessions(parentID)
	if err != nil {
		log.Printf("subagent DB: failed to query child sessions: %v", err)
		return
	}

	for _, s := range sessions {
		p.mu.Lock()
		firstSeen := !p.seen[s.SessionID]
		previousUpdated := p.lastSeen[s.SessionID]
		alreadyFinished := p.finished[s.SessionID]
		p.mu.Unlock()

		p.processChildSession(s, s.TimeUpdated, firstSeen, previousUpdated == s.TimeUpdated && !firstSeen, alreadyFinished)
	}
}

func (p *DBPoller) processChildSession(s SessionOutput, updated int64, firstSeen, unchanged, alreadyFinished bool) {
	messages, err := p.extractSession(s.SessionID)
	if err != nil {
		log.Printf("subagent DB: failed to extract session %s: %v", s.SessionID, err)
		return
	}

	if p.writeFile != nil {
		data, err := json.MarshalIndent(messages, "", "  ")
		if err != nil {
			log.Printf("subagent DB: failed to marshal session %s debug data: %v", s.SessionID, err)
		} else {
			path := fmt.Sprintf(".sandman/logs/%d/subagents/%s.json", p.issue, s.SessionID)
			if err := p.writeFile(path, data); err != nil {
				log.Printf("subagent DB: failed to write debug file %s: %v", path, err)
			}
		}
	}

	p.mu.Lock()
	previous, ok := p.started[s.SessionID]
	if !ok {
		previous = s
	}
	if firstSeen {
		p.started[s.SessionID] = SessionOutput{SessionID: s.SessionID, Title: s.Title, Agent: s.Agent, Messages: messages}
		p.order = append(p.order, s.SessionID)
		p.seen[s.SessionID] = true
		p.lastSeen[s.SessionID] = updated
		p.mu.Unlock()
		p.emitSubagentStart(s)
		p.emitNewParts(s.SessionID, nil, messages)
		return
	}

	if alreadyFinished {
		p.mu.Unlock()
		return
	}

	if unchanged {
		p.finished[s.SessionID] = true
		p.mu.Unlock()
		p.emitSubagentFinishLocked(s.SessionID)
		return
	}

	p.started[s.SessionID] = SessionOutput{SessionID: s.SessionID, Title: previous.Title, Agent: previous.Agent, Messages: messages}
	p.lastSeen[s.SessionID] = updated
	p.mu.Unlock()

	p.emitNewParts(s.SessionID, previous.Messages, messages)
}

func (p *DBPoller) emitNewParts(childID string, previous, current []Message) {
	if p.events == nil {
		return
	}

	for i, msg := range current {
		prevCount := 0
		if i < len(previous) {
			prevCount = len(previous[i].Parts)
		}
		for _, part := range msg.Parts[prevCount:] {
			p.dispatchEvent(childID, part)
		}
	}
}

func (p *DBPoller) dispatchEvent(childID string, part Part) {
	if p.events == nil {
		return
	}

	p.mu.Lock()
	parentID := p.parentID
	p.mu.Unlock()

	ev := Event{
		SessionID: childID,
		ParentID:  parentID,
		Timestamp: time.Now(),
	}

	switch part.Type {
	case PartTypeText:
		ev.Type = EventText
		ev.Content = part.Text
	case PartTypeReasoning:
		ev.Type = EventReasoning
		ev.Content = part.Text
	case PartTypeTool:
		ev.Type = EventTool
		ev.Title = part.ToolName
		ev.Content = part.ToolOutput
	default:
		return
	}

	select {
	case p.events <- ev:
	default:
	}
}

func (p *DBPoller) discoverDBPath() (string, error) {
	p.mu.Lock()
	cached := p.dbPath
	p.mu.Unlock()
	if cached != "" {
		return cached, nil
	}

	out, err := p.runner("db", "path")
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(out))

	p.mu.Lock()
	p.dbPath = path
	p.mu.Unlock()
	return path, nil
}

func (p *DBPoller) querySessions(parentID string) ([]SessionOutput, error) {
	query := fmt.Sprintf("SELECT id, title, agent, time_updated FROM session WHERE parent_id = '%s' ORDER BY time_created", escapeSQLString(parentID))
	out, err := p.runner("db", query, "--format", "json")
	if err != nil {
		return nil, err
	}
	var rows []childSessionRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	sessions := make([]SessionOutput, len(rows))
	for i, r := range rows {
		sessions[i] = SessionOutput{
			SessionID:   r.ID,
			Title:       r.Title,
			Agent:       r.Agent,
			TimeUpdated: r.TimeUpdated,
		}
	}
	return sessions, nil
}

func (p *DBPoller) extractSession(sessionID string) ([]Message, error) {
	messages, err := p.queryMessages(sessionID)
	if err != nil {
		return nil, err
	}
	parts, err := p.queryParts(sessionID)
	if err != nil {
		return nil, err
	}
	return p.groupPartsByMessage(messages, parts), nil
}

func (p *DBPoller) queryMessages(sessionID string) ([]messageRow, error) {
	query := fmt.Sprintf("SELECT m.id, m.session_id, m.data FROM message m WHERE m.session_id = '%s' ORDER BY m.id", escapeSQLString(sessionID))
	out, err := p.runner("db", query, "--format", "json")
	if err != nil {
		return nil, err
	}
	var rows []messageRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func (p *DBPoller) queryParts(sessionID string) ([]partRow, error) {
	query := fmt.Sprintf("SELECT p.id, p.message_id, p.session_id, p.data FROM part p WHERE p.session_id = '%s' ORDER BY p.id", escapeSQLString(sessionID))
	out, err := p.runner("db", query, "--format", "json")
	if err != nil {
		return nil, err
	}
	var rows []partRow
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	return rows, nil
}

func (p *DBPoller) groupPartsByMessage(messages []messageRow, parts []partRow) []Message {
	partsByMessage := make(map[string][]Part)
	for _, pr := range parts {
		part := parsePartData(pr.Data)
		partsByMessage[pr.MessageID] = append(partsByMessage[pr.MessageID], part)
	}

	result := make([]Message, len(messages))
	for i, mr := range messages {
		var md messageData
		if err := json.Unmarshal([]byte(mr.Data), &md); err != nil {
			log.Printf("subagent DB: failed to parse message.data JSON for message %s: %v", mr.ID, err)
		}

		result[i] = Message{
			Role:  md.Role,
			Parts: partsByMessage[mr.ID],
		}
	}
	return result
}

func parsePartData(raw string) Part {
	var pd partData
	if err := json.Unmarshal([]byte(raw), &pd); err != nil {
		log.Printf("subagent DB: failed to parse part.data JSON: %v — raw: %s", err, raw)
		return Part{Type: PartTypeText, Text: raw}
	}

	switch pd.Type {
	case "text":
		return Part{Type: PartTypeText, Text: pd.Text}
	case "reasoning":
		return Part{Type: PartTypeReasoning, Text: pd.Text}
	case "tool":
		return Part{Type: PartTypeTool, ToolName: pd.Tool, ToolInput: pd.Input, ToolOutput: pd.Output}
	default:
		return Part{Type: PartTypeText, Text: pd.Text}
	}
}
