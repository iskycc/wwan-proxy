//go:build linux

package selfupdate

import "syscall"

func bindToDownloadInterface(name string) func(string, string, syscall.RawConn) error {
	return func(_, _ string, raw syscall.RawConn) error {
		var socketErr error
		err := raw.Control(func(fd uintptr) {
			socketErr = syscall.SetsockoptString(int(fd), syscall.SOL_SOCKET, syscall.SO_BINDTODEVICE, name)
		})
		if err != nil {
			return err
		}
		return socketErr
	}
}
