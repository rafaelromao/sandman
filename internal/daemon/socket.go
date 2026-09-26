package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/rafaelromao/sandman/internal/socketpath"
)

type ControlSocket struct {
	dir         string
	name        string
	listener    net.Listener
	broadcaster *Broadcaster
}

func removeSocketPath(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func NewControlSocket(dir string, broadcaster *Broadcaster) *ControlSocket {
	return NewControlSocketWithName(dir, "batch.sock", broadcaster)
}

func NewControlSocketWithName(dir, name string, broadcaster *Broadcaster) *ControlSocket {
	return &ControlSocket{dir: dir, name: name, broadcaster: broadcaster}
}

func (s *ControlSocket) Broadcaster() *Broadcaster {
	return s.broadcaster
}

// Path returns the address the socket binds to. For long logical paths
// the resolver maps to a short deterministic filesystem path under
// /tmp so the bind always succeeds and consumers see a single
// effective path on every host. For short paths Path returns the
// logical path verbatim.
func (s *ControlSocket) Path() string {
	return socketpath.Path(filepath.Join(s.dir, s.name))
}

func (s *ControlSocket) Start() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return fmt.Errorf("chmod run dir: %w", err)
	}

	sockPath := s.Path()
	_ = os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("create control socket: %w", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		listener.Close()
		return fmt.Errorf("chmod control socket: %w", err)
	}
	s.listener = listener

	go s.acceptLoop(listener)

	return nil
}

func (s *ControlSocket) acceptLoop(listener net.Listener) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go s.addClient(conn)
	}
}

func (s *ControlSocket) addClient(conn net.Conn) {
	// Portal sends a one-byte opt-in immediately after connecting. Ordinary
	// attach clients remain read-only; the short deadline lets them proceed
	// without changing their wire format or waiting for the run to finish.
	_ = conn.SetReadDeadline(time.Now().Add(25 * time.Millisecond))
	var handshake [1]byte
	n, _ := conn.Read(handshake[:])
	_ = conn.SetReadDeadline(time.Time{})
	if n == 1 && handshake[0] == PortalStreamHandshake {
		s.broadcaster.AddPortalClient(conn)
		return
	}
	s.broadcaster.AddClient(conn)
}

func (s *ControlSocket) Stop() error {
	var closeErr error
	if s.listener != nil {
		closeErr = s.listener.Close()
		s.listener = nil
	}
	s.broadcaster.Close()
	if rmErr := removeSocketPath(s.Path()); rmErr != nil {
		return rmErr
	}
	return closeErr
}
