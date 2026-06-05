package daemon

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
)

// IssueCommander is the seam the command socket uses to abort a single
// in-flight AgentRun without disturbing siblings.
type IssueCommander interface {
	AbortIssue(issueNumber int) error
}

// CommandRequest is the JSON envelope accepted by the command socket.
type CommandRequest struct {
	Action string `json:"action"`
	Issue  int    `json:"issue"`
}

// CommandResponse is the JSON envelope written back to the client.
type CommandResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// CommandSocketPath returns the unix socket path used to send commands
// to a running sandman daemon.
func CommandSocketPath(dir string) string {
	return filepath.Join(dir, "cmd.sock")
}

// CommandServer accepts one-shot JSON command requests on a unix socket
// and dispatches them to an IssueCommander. It owns the lifetime of the
// cmd.sock file: a fresh Start removes any stale socket, and Stop removes
// the socket file again.
type CommandServer struct {
	dir       string
	commander IssueCommander
	listener  net.Listener
}

// NewCommandServer wires a CommandServer to the given run directory and
// orchestrator. Start must be called to begin accepting connections.
func NewCommandServer(dir string, commander IssueCommander) *CommandServer {
	return &CommandServer{dir: dir, commander: commander}
}

// Start creates the command socket and begins accepting connections.
// It removes any stale socket at the same path before listening.
func (s *CommandServer) Start() error {
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}
	sockPath := CommandSocketPath(s.dir)
	_ = os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("create command socket: %w", err)
	}
	s.listener = listener
	go s.acceptLoop()
	return nil
}

// Stop closes the listener and removes the socket file. It is safe to
// call Stop multiple times.
func (s *CommandServer) Stop() error {
	var err error
	if s.listener != nil {
		err = s.listener.Close()
	}
	if rmErr := os.Remove(CommandSocketPath(s.dir)); rmErr != nil && !os.IsNotExist(rmErr) && err == nil {
		err = rmErr
	}
	return err
}

func (s *CommandServer) acceptLoop() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *CommandServer) handle(conn net.Conn) {
	defer conn.Close()
	var req CommandRequest
	if err := json.NewDecoder(conn).Decode(&req); err != nil {
		writeResponse(conn, CommandResponse{Status: "error", Message: "invalid request"})
		return
	}
	switch req.Action {
	case "abort":
		if err := s.commander.AbortIssue(req.Issue); err != nil {
			writeResponse(conn, CommandResponse{Status: "error", Message: err.Error()})
			return
		}
		writeResponse(conn, CommandResponse{Status: "ok"})
	default:
		writeResponse(conn, CommandResponse{Status: "error", Message: fmt.Sprintf("unknown action: %s", req.Action)})
	}
}

func writeResponse(conn net.Conn, resp CommandResponse) {
	_ = json.NewEncoder(conn).Encode(resp)
}
