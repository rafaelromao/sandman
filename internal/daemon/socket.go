package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
)

type ControlSocket struct {
	dir         string
	name        string
	listener    net.Listener
	broadcaster *Broadcaster
	isAbstract  bool
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
		if isPathTooLong(err) {
			return s.startWithShortSockName()
		}
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

func isPathTooLong(err error) bool {
	if opErr, ok := err.(*net.OpError); ok {
		if sysErr, ok := opErr.Err.(*os.SyscallError); ok {
			return sysErr.Err == syscall.EINVAL
		}
	}
	return false
}

func (s *ControlSocket) startWithShortSockName() error {
	batchName := filepath.Base(s.dir)
	abstractName := "@sandman-" + fmt.Sprintf("%x", hashString(batchName))
	listener, err := net.Listen("unix", abstractName)
	if err != nil {
		return fmt.Errorf("create abstract control socket: %w", err)
	}
	s.listener = listener
	s.isAbstract = true

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

func hashString(s string) uint64 {
	h := uint64(0)
	for i, c := range s {
		h = h*31 + uint64(c) + uint64(i)
	}
	return h
}

func (s *ControlSocket) Stop() error {
	var closeErr error
	if s.listener != nil {
		closeErr = s.listener.Close()
		s.listener = nil
	}
	s.broadcaster.Close()
	if !s.isAbstract {
		_ = os.Remove(s.Path())
	}
	return closeErr
}
