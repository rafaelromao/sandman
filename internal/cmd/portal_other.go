//go:build !linux

package cmd

import "fmt"

func resolvePortalPeerPID(sockPath string) (int, error) {
	return 0, fmt.Errorf("resolve run process id: unsupported on this platform")
}

func portalStopSupported() bool { return false }
