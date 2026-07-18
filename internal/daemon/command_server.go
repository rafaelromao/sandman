package daemon

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"
)

// IssueCommander is the seam the command socket uses to abort a single
// in-flight AgentRun without disturbing siblings.
type IssueCommander interface {
	AbortIssue(issueNumber int) error
}

const (
	maxCommandRequestBytes = 1 << 20
	commandReadDeadline    = 5 * time.Second
)

// CommandRequest is the JSON wire format the Command Server accepts from
// external clients on the run.sock Unix socket at
// .sandman/batches/<batch-id>/runs/<run-id>/run.sock. It is the daemon's public wire
// contract, not an internal helper the Portal (or any other client) can
// depend on casually — every field and tag below is observable on the
// wire. The Daemon Process is the only reader; the Portal is one of
// several potential clients, and other sandman subcommands or external
// scripts may construct their own requests. Any change to the JSON
// field shape is a wire-format break for every client, whether they
// import this type or decode the bytes themselves. Currently the only
// Action is "abort", which targets a single in-flight AgentRun by
// GitHub issue number via the IssueCommander seam.
type CommandRequest struct {
	Action string `json:"action"`
	Issue  int    `json:"issue"`
}

// CommandResponse is the JSON wire format the Command Server writes
// back to clients. It is the daemon's public wire contract, not an
// internal helper the Portal (or any other client) can depend on
// casually — every field and tag below is observable on the wire. The
// Daemon Process is the only writer; the Portal (and any future client)
// decodes it. Any change to the JSON field shape is a wire-format break
// for every client. Status is "ok" on success and "error" on failure.
// When Status is "error", Message carries a stable machine-readable
// code (currently "invalid request", "unknown action: <name>", or
// "abort_failed") so clients can branch on it without parsing free-form
// text. The Command Server is distinct from the Control Socket at
// run.sock, which streams daemon output to Attach clients.
type CommandResponse struct {
	Status  string `json:"status"`
	Message string `json:"message,omitempty"`
}

// CommandSocketPath returns the unix socket path for the per-run command
// server inside a run folder.
func CommandSocketPath(dir string) string {
	return filepath.Join(dir, "run.sock")
}

// CommandServer accepts one-shot JSON command requests on a unix socket
// and dispatches them to an IssueCommander. It owns the listener
// lifecycle and removes the filesystem socket path when the listener is
// path-based.
type CommandServer struct {
	dir         string
	commander   IssueCommander
	targetIssue int
	targetBound bool
	listener    net.Listener
	isAbstract  bool
}

// NewCommandServer wires a CommandServer to the given run directory and
// orchestrator. Start must be called to begin accepting connections.
func NewCommandServer(dir string, commander IssueCommander) *CommandServer {
	return &CommandServer{dir: dir, commander: commander}
}

// NewCommandServerForIssue binds abort requests to the issue represented by
// this run's socket. Requests for another issue are rejected before reaching
// the shared commander.
func NewCommandServerForIssue(dir string, commander IssueCommander, issueNumber int) *CommandServer {
	return &CommandServer{
		dir:         dir,
		commander:   commander,
		targetIssue: issueNumber,
		targetBound: true,
	}
}

// Start creates the command socket and begins accepting connections.
// It removes any stale socket at the same path before listening.
func (s *CommandServer) Start() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return fmt.Errorf("chmod run dir: %w", err)
	}
	sockPath := CommandSocketPath(s.dir)
	_ = os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		if shouldFallbackToAbstractSocket(sockPath, err) {
			listener, err = net.Listen("unix", abstractCommandSocketName(s.dir))
			if err != nil {
				return fmt.Errorf("create abstract command socket: %w", err)
			}
			s.listener = listener
			s.isAbstract = true
			go s.acceptLoop()
			return nil
		}
		if platformErr := nonLinuxPlatformError(sockPath); platformErr != nil {
			return platformErr
		}
		return fmt.Errorf("create command socket: %w", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		listener.Close()
		return fmt.Errorf("chmod command socket: %w", err)
	}
	s.listener = listener
	go s.acceptLoop()
	return nil
}

func (s *CommandServer) startWithShortSockName() error {
	abstractName := abstractCommandSocketName(s.dir)
	listener, err := net.Listen("unix", abstractName)
	if err != nil {
		return fmt.Errorf("create abstract command socket: %w", err)
	}
	s.listener = listener
	s.isAbstract = true
	go s.acceptLoop()
	return nil
}

func abstractCommandSocketName(dir string) string {
	return "@sandman-cmd-" + fmt.Sprintf("%x", hashString(filepath.Base(dir)))
}

// Stop closes the listener and removes the socket file. It is safe to
// call Stop multiple times.
func (s *CommandServer) Stop() error {
	var err error
	if s.listener != nil {
		err = s.listener.Close()
	}
	if !s.isAbstract {
		if rmErr := os.Remove(CommandSocketPath(s.dir)); rmErr != nil && !os.IsNotExist(rmErr) && err == nil {
			err = rmErr
		}
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
	if err := conn.SetReadDeadline(time.Now().Add(commandReadDeadline)); err != nil {
		writeResponse(conn, CommandResponse{Status: "error", Message: "invalid request"})
		return
	}
	dec := json.NewDecoder(io.LimitReader(conn, maxCommandRequestBytes))
	dec.DisallowUnknownFields()
	var req CommandRequest
	if err := dec.Decode(&req); err != nil {
		writeResponse(conn, CommandResponse{Status: "error", Message: "invalid request"})
		return
	}
	switch req.Action {
	case "abort":
		if s.targetBound && (s.targetIssue <= 0 || req.Issue != s.targetIssue) {
			writeResponse(conn, CommandResponse{Status: "error", Message: "abort_failed"})
			return
		}
		if s.commander == nil {
			writeResponse(conn, CommandResponse{Status: "error", Message: "abort_failed"})
			return
		}
		if err := s.commander.AbortIssue(req.Issue); err != nil {
			writeResponse(conn, CommandResponse{Status: "error", Message: "abort_failed"})
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
