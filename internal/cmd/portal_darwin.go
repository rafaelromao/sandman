//go:build darwin

package cmd

import (
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"
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
		pid     int
		credErr error
	)
	if err := raw.Control(func(fd uintptr) {
		pid, credErr = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
	}); err != nil {
		return 0, err
	}
	if credErr == nil && pid > 0 {
		return pid, nil
	}

	var (
		xucred *unix.Xucred
		cred2  error
	)
	if err := raw.Control(func(fd uintptr) {
		xucred, cred2 = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if cred2 == nil && xucred != nil {
		return 0, fmt.Errorf("resolve run process id: peer identity confirmed via LOCAL_PEERCRED but no numeric PID is exposed")
	}
	if credErr != nil {
		return 0, fmt.Errorf("resolve run process id: %w", credErr)
	}
	return 0, fmt.Errorf("resolve run process id: empty peer credentials")
}

func portalAbortSupported() bool { return true }
