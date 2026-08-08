package selfupdate

import (
	"strings"
	"testing"
)

func TestValidateDownloadInterface(t *testing.T) {
	if err := ValidateDownloadInterface(""); err != nil {
		t.Fatalf("default route rejected: %v", err)
	}
	if err := ValidateDownloadInterface("lo"); err != nil {
		t.Fatalf("loopback interface rejected: %v", err)
	}
	for _, name := range []string{"bad interface", "../../eth0", strings.Repeat("a", 16), "interface-that-does-not-exist"} {
		if err := ValidateDownloadInterface(name); err == nil {
			t.Fatalf("invalid interface %q was accepted", name)
		}
	}
}
