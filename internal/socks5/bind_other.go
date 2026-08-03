//go:build !linux

package socks5

import (
	"fmt"
	"syscall"
)

func bindToDevice(iface string) func(string, string, syscall.RawConn) error {
	if iface == "" {
		return nil
	}
	return func(_, _ string, _ syscall.RawConn) error {
		return fmt.Errorf("binding traffic to interface %q is only supported on Linux", iface)
	}
}
