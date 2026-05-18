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

const defaultPollInterval = 2 * time.Second

func NewDBPoller(issue int, events chan<- Event) *DBPoller {
	return &DBPoller{
		runner:    opencodeDBRunner,
		events:    events,
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
	started  map[string]SessionOutput
	stopped  bool
	wg       sync.WaitGroup
}

type childSessionRow struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Agent       string `json:"agent"`
	TimeCreated string `json:"time_created"`
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
	p.started = make(map[string]SessionOutput)
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
		Type:      EventSubagentFinish,
		Agent:     s.Agent,
		Timestamp: time.Now(),
	}:
	default:
	}
}

func (p *DBPoller) Stop() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.wg.Wait()

	p.mu.Lock()
	for id := range p.started {
		p.emitSubagentFinishLocked(id)
	}
	p.mu.Unlock()
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
		if p.seen[s.SessionID] {
			p.mu.Unlock()
			continue
		}
		p.seen[s.SessionID] = true
		p.mu.Unlock()

		p.processChildSession(s)
	}
}

func (p *DBPoller) processChildSession(s SessionOutput) {
	p.mu.Lock()
	if _, ok := p.started[s.SessionID]; ok {
		p.mu.Unlock()
	} else {
		p.started[s.SessionID] = s
		p.mu.Unlock()
		p.emitSubagentStart(s)
	}

	messages, err := p.extractSession(s.SessionID)
	if err != nil {
		log.Printf("subagent DB: failed to extract session %s: %v", s.SessionID, err)
		return
	}

	for _, m := range messages {
		for _, part := range m.Parts {
			p.dispatchEvent(s.SessionID, part)
		}
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
	query := fmt.Sprintf("SELECT id, title, agent, time_created FROM session WHERE parent_id = '%s' ORDER BY time_created", escapeSQLString(parentID))
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
			SessionID: r.ID,
			Title:     r.Title,
			Agent:     r.Agent,
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
