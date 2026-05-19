package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
)

type ControlSocket struct {
	dir         string
	listener    net.Listener
	broadcaster *Broadcaster
}

func NewControlSocket(dir string, broadcaster *Broadcaster) *ControlSocket {
	return &ControlSocket{dir: dir, broadcaster: broadcaster}
}

func (s *ControlSocket) Broadcaster() *Broadcaster {
	return s.broadcaster
}

func (s *ControlSocket) Start() error {
	if err := os.MkdirAll(s.dir, 0755); err != nil {
		return err
	}

	sockPath := filepath.Join(s.dir, "run.sock")
	os.Remove(sockPath)
	listener, err := net.Listen("unix", sockPath)
	if err != nil {
		return fmt.Errorf("create control socket: %w", err)
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
	s.broadcaster.Close()
	if s.listener != nil {
		return s.listener.Close()
	}
	return nil
}
