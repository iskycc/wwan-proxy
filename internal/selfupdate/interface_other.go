//go:build !linux

package selfupdate

import (
	"fmt"
	"syscall"
)

func bindToDownloadInterface(name string) func(string, string, syscall.RawConn) error {
	return func(_, _ string, _ syscall.RawConn) error {
		return fmt.Errorf("binding update downloads to interface %q is only supported on Linux", name)
	}
}
