//go:build linux

package cmd

import (
	"fmt"
	"net"
	"syscall"
	"time"
)

func resolvePortalPeerPID(sockPath string) (int, error) {
	conn, err := net.DialTimeout("unix", sockPath, time.Second)
	if err != nil {
		return 0, err
	}
	defer conn.Close()

	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("connect to run socket: unexpected connection type")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return 0, err
	}

	var (
		cred    *syscall.Ucred
		ctrlErr error
	)
	if err := raw.Control(func(fd uintptr) {
		cred, ctrlErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if ctrlErr != nil {
		return 0, ctrlErr
	}
	if cred == nil || cred.Pid == 0 {
		return 0, fmt.Errorf("resolve run process id: empty peer credentials")
	}
	return int(cred.Pid), nil
}

func portalStopSupported() bool { return true }
