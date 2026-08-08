package selfupdate

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type OperationStatus struct {
	State      string     `json:"state"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	Version    string     `json:"version,omitempty"`
	Interface  string     `json:"interface,omitempty"`
	Message    string     `json:"message,omitempty"`
}

func ReadOperationStatus(path string) (*OperationStatus, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read update status: %w", err)
	}
	var status OperationStatus
	if err := json.Unmarshal(data, &status); err != nil {
		return nil, fmt.Errorf("decode update status: %w", err)
	}
	return &status, nil
}

func writeOperationStatus(path string, groupID int, status OperationStatus) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0750); err != nil {
		return fmt.Errorf("create update status directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".update-status-*")
	if err != nil {
		return fmt.Errorf("create update status file: %w", err)
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetEscapeHTML(true)
	if err := encoder.Encode(status); err != nil {
		cleanup()
		return fmt.Errorf("encode update status: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("sync update status: %w", err)
	}
	if err := temporary.Chmod(0640); err != nil {
		cleanup()
		return fmt.Errorf("set update status permissions: %w", err)
	}
	if err := temporary.Chown(0, groupID); err != nil {
		cleanup()
		return fmt.Errorf("set update status ownership: %w", err)
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("close update status: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return fmt.Errorf("publish update status: %w", err)
	}
	return nil
}
