//go:build linux

package socks5

import (
	"fmt"
	"syscall"
)

func setSocketDevice(fd int, iface string) error {
	return syscall.SetsockoptString(fd, syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, iface)
}

// PreflightDeviceBinding verifies the exact SO_BINDTODEVICE operation used by
// production dialers on a minimal temporary TCP socket, without sending data.
func PreflightDeviceBinding(iface string) error {
	if iface == "" {
		return fmt.Errorf("interface is empty")
	}
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, syscall.IPPROTO_TCP)
	if err != nil {
		return fmt.Errorf("create preflight socket: %w", err)
	}
	defer syscall.Close(fd)
	if err := setSocketDevice(fd, iface); err != nil {
		return fmt.Errorf("SO_BINDTODEVICE %q: %w", iface, err)
	}
	return nil
}

func bindToDevice(iface string) func(string, string, syscall.RawConn) error {
	if iface == "" {
		return nil
	}
	return func(_, _ string, raw syscall.RawConn) error {
		var sockErr error
		err := raw.Control(func(fd uintptr) {
			sockErr = setSocketDevice(int(fd), iface)
		})
		if err != nil {
			return err
		}
		return sockErr
	}
}
