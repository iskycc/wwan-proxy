package selfupdate

import (
	"fmt"
	"net"
)

// ValidateDownloadInterface restricts the privileged updater to a real Linux
// interface name. Linux IFNAMSIZ is 16 bytes including the trailing NUL.
func ValidateDownloadInterface(name string) error {
	if name == "" {
		return nil
	}
	if len(name) > 15 {
		return fmt.Errorf("download interface %q exceeds 15 bytes", name)
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' || r == ':' {
			continue
		}
		return fmt.Errorf("download interface %q contains unsupported characters", name)
	}
	if _, err := net.InterfaceByName(name); err != nil {
		return fmt.Errorf("download interface %q is unavailable: %w", name, err)
	}
	return nil
}
