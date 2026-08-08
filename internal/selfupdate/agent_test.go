package selfupdate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidateRootExecutable(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("ownership validation requires a root test process")
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "installer")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := validateRootExecutable(path, "installer"); err != nil {
		t.Fatalf("secure executable rejected: %v", err)
	}
	if err := os.Chmod(path, 0775); err != nil {
		t.Fatal(err)
	}
	if err := validateRootExecutable(path, "installer"); err == nil {
		t.Fatal("group-writable executable was accepted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/bin/sh", path); err != nil {
		t.Fatal(err)
	}
	if err := validateRootExecutable(path, "installer"); err == nil {
		t.Fatal("symbolic-link executable was accepted")
	}
}

func TestAgentRunsFixedInstallerAndPublishesStatus(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("update agent requires root")
	}
	directory := t.TempDir()
	installer := filepath.Join(directory, "installer")
	binary := filepath.Join(directory, "wwan-proxy")
	marker := filepath.Join(directory, "installed")
	if err := os.WriteFile(installer, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >\"$UPDATE_TEST_MARKER\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' abcdef123456\n"), 0755); err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(directory, "update.sock")
	statusPath := filepath.Join(directory, "update-status.json")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	t.Setenv("UPDATE_TEST_MARKER", marker)
	go func() {
		done <- RunAgent(ctx, AgentConfig{
			SocketPath: socketPath, StatusPath: statusPath, InstallerPath: installer,
			BinaryPath: binary, Platform: "openwrt", Group: "root", StartDelay: 10 * time.Millisecond,
		})
	}()
	deadline := time.Now().Add(time.Second)
	for {
		if _, err := os.Stat(socketPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("agent socket was not created")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := (Client{SocketPath: socketPath, Timeout: time.Second}).Trigger(ctx, "lo"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("installer did not run: %v", err)
	}
	arguments, err := os.ReadFile(marker)
	if err != nil || !strings.Contains(string(arguments), "--download-interface lo") {
		t.Fatalf("installer did not receive download interface: %q err=%v", arguments, err)
	}
	status, err := ReadOperationStatus(statusPath)
	if err != nil {
		t.Fatal(err)
	}
	if status == nil || status.State != "succeeded" || status.Version != "abcdef123456" || status.Interface != "lo" || status.FinishedAt == nil {
		t.Fatalf("unexpected final operation status: %+v", status)
	}
}
