package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

type ControlSocket struct {
	dir         string
	name        string
	listener    net.Listener
	broadcaster *Broadcaster
}

func NewControlSocket(dir string, broadcaster *Broadcaster) *ControlSocket {
	return NewControlSocketWithName(dir, "run.sock", broadcaster)
}

func NewControlSocketWithName(dir, name string, broadcaster *Broadcaster) *ControlSocket {
	return &ControlSocket{dir: dir, name: name, broadcaster: broadcaster}
}

func (s *ControlSocket) Broadcaster() *Broadcaster {
	return s.broadcaster
}

func (s *ControlSocket) Path() string {
	return filepath.Join(s.dir, s.name)
}

func (s *ControlSocket) Start() error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(s.dir, 0o700); err != nil {
		return fmt.Errorf("chmod run dir: %w", err)
	}

	sockPath := s.Path()
	os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("create control socket: %w", err)
	}
	if err := os.Chmod(sockPath, 0o600); err != nil {
		listener.Close()
		return fmt.Errorf("chmod control socket: %w", err)
	}
	s.listener = listener

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			s.broadcaster.AddClient(conn)
		}
	}()

	return nil
}

func (s *ControlSocket) Stop() error {
	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			s.broadcaster.Close()
			return err
		}
	}
	s.broadcaster.Close()
	return nil
}
