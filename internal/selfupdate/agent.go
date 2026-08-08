package selfupdate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

type AgentConfig struct {
	SocketPath    string
	StatusPath    string
	InstallerPath string
	BinaryPath    string
	Platform      string
	Group         string
	Repository    string
	StartDelay    time.Duration
	Logger        *slog.Logger
}

type Agent struct {
	config  AgentConfig
	groupID int
	busy    atomic.Bool
	log     *slog.Logger
}

func RunAgent(ctx context.Context, config AgentConfig) error {
	agent, err := newAgent(config)
	if err != nil {
		return err
	}
	return agent.serve(ctx)
}

func newAgent(config AgentConfig) (*Agent, error) {
	if config.Platform == "" {
		config.Platform = DetectPlatform()
	}
	if !isSupportedPlatform(config.Platform) {
		return nil, fmt.Errorf("unsupported update platform %q", config.Platform)
	}
	if config.Group == "" {
		if config.Platform == "openwrt" {
			config.Group = "root"
		} else {
			config.Group = "wwan-proxy"
		}
	}
	if config.Repository == "" {
		config.Repository = DefaultRepository
	}
	if err := validateRepository(config.Repository); err != nil {
		return nil, err
	}
	for label, path := range map[string]string{"socket": config.SocketPath, "status": config.StatusPath, "installer": config.InstallerPath, "binary": config.BinaryPath} {
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("update agent %s path must be absolute: %q", label, path)
		}
	}
	if err := validateRootExecutable(config.InstallerPath, "installer"); err != nil {
		return nil, err
	}
	if err := validateRootExecutable(config.BinaryPath, "binary"); err != nil {
		return nil, err
	}
	group, err := user.LookupGroup(config.Group)
	if err != nil {
		return nil, fmt.Errorf("look up update agent group %q: %w", config.Group, err)
	}
	groupID, err := strconv.Atoi(group.Gid)
	if err != nil {
		return nil, fmt.Errorf("parse update agent group ID %q: %w", group.Gid, err)
	}
	if config.StartDelay <= 0 {
		config.StartDelay = 1500 * time.Millisecond
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Agent{config: config, groupID: groupID, log: logger.With("component", "update-agent")}, nil
}

func validateRootExecutable(path, label string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect update agent %s: %w", label, err)
	}
	if !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return fmt.Errorf("update agent %s must be an executable regular file: %s", label, path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return fmt.Errorf("update agent %s must be owned by root: %s", label, path)
	}
	if info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("update agent %s must not be group/world writable: %s", label, path)
	}
	return nil
}

func (a *Agent) serve(ctx context.Context) error {
	if os.Geteuid() != 0 {
		return errors.New("update agent must run as root")
	}
	if err := a.prepareSocketDirectory(); err != nil {
		return err
	}
	if err := removeStaleSocket(a.config.SocketPath); err != nil {
		return err
	}
	listener, err := net.Listen("unix", a.config.SocketPath)
	if err != nil {
		return fmt.Errorf("listen on update socket: %w", err)
	}
	defer listener.Close()
	defer os.Remove(a.config.SocketPath)
	if err := os.Chown(a.config.SocketPath, 0, a.groupID); err != nil {
		return fmt.Errorf("set update socket ownership: %w", err)
	}
	if err := os.Chmod(a.config.SocketPath, 0660); err != nil {
		return fmt.Errorf("set update socket permissions: %w", err)
	}
	a.log.Info("update agent listening", "socket", a.config.SocketPath, "platform", a.config.Platform)
	go func() {
		<-ctx.Done()
		_ = listener.Close()
	}()
	for {
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			if ctx.Err() != nil || a.busy.Load() {
				return nil
			}
			return fmt.Errorf("accept update agent connection: %w", acceptErr)
		}
		go a.handleConnection(ctx, listener, conn)
	}
}

func (a *Agent) prepareSocketDirectory() error {
	directory := filepath.Dir(a.config.SocketPath)
	if err := os.MkdirAll(directory, 0750); err != nil {
		return fmt.Errorf("create update runtime directory: %w", err)
	}
	if err := os.Chown(directory, 0, a.groupID); err != nil {
		return fmt.Errorf("set update runtime directory ownership: %w", err)
	}
	if err := os.Chmod(directory, 0750); err != nil {
		return fmt.Errorf("set update runtime directory permissions: %w", err)
	}
	return nil
}

func removeStaleSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect existing update socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to replace non-socket update path %s", path)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != 0 {
		return fmt.Errorf("refusing to replace update socket not owned by root: %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale update socket: %w", err)
	}
	return nil
}

func (a *Agent) handleConnection(ctx context.Context, listener net.Listener, conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReader(io.LimitReader(conn, maxProtocolMessage))
	var request agentRequest
	if err := json.NewDecoder(reader).Decode(&request); err != nil {
		a.respond(conn, agentResponse{Error: "invalid update request"})
		return
	}
	switch request.Action {
	case "ping":
		if request.Interface != "" {
			a.respond(conn, agentResponse{Error: "ping does not accept an interface"})
			return
		}
		a.respond(conn, agentResponse{OK: true})
	case "update-latest":
		if err := ValidateDownloadInterface(request.Interface); err != nil {
			a.respond(conn, agentResponse{Error: err.Error()})
			return
		}
		if !a.busy.CompareAndSwap(false, true) {
			a.respond(conn, agentResponse{Error: "update already in progress"})
			return
		}
		status := OperationStatus{State: "queued", StartedAt: time.Now().UTC(), Interface: request.Interface, Message: "更新任务已进入队列"}
		if err := writeOperationStatus(a.config.StatusPath, a.groupID, status); err != nil {
			a.busy.Store(false)
			a.respond(conn, agentResponse{Error: err.Error()})
			return
		}
		a.respond(conn, agentResponse{OK: true})
		go func() {
			time.Sleep(a.config.StartDelay)
			a.performUpdate(ctx, status)
			_ = listener.Close()
		}()
	default:
		a.respond(conn, agentResponse{Error: "unsupported update action"})
	}
}

func (a *Agent) respond(conn net.Conn, response agentResponse) {
	_ = json.NewEncoder(conn).Encode(response)
}

func (a *Agent) performUpdate(ctx context.Context, status OperationStatus) {
	status.State = "running"
	status.Message = "正在下载、校验并安装最新版本"
	_ = writeOperationStatus(a.config.StatusPath, a.groupID, status)
	a.log.Warn("automatic update started", "platform", a.config.Platform, "repository", a.config.Repository, "download_interface", status.Interface)

	updateContext, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	args := []string{"--version", "latest", "--repo", a.config.Repository}
	if status.Interface != "" {
		args = append(args, "--download-interface", status.Interface)
	}
	switch a.config.Platform {
	case "alpine":
		args = append(args, "--skip-firewall")
	case "ubuntu":
		// The Web listener is stored in SQLite and may no longer be 9090. The
		// installer still verifies that systemd keeps the service active.
		args = append(args, "--skip-health-check")
	}
	command := exec.CommandContext(updateContext, a.config.InstallerPath, args...)
	command.Env = append(os.Environ(), "WWAN_PROXY_UPDATE_AGENT=1")
	output, err := command.CombinedOutput()
	now := time.Now().UTC()
	status.FinishedAt = &now
	status.Message = tailMessage(output)
	if err != nil {
		status.State = "failed"
		if status.Message == "" {
			status.Message = err.Error()
		} else {
			status.Message = status.Message + "\n" + err.Error()
		}
		a.log.Error("automatic update failed", "error", err)
	} else {
		status.State = "succeeded"
		versionOutput, versionErr := exec.Command(a.config.BinaryPath, "-version").Output()
		if versionErr == nil {
			status.Version = strings.TrimSpace(string(versionOutput))
		}
		if status.Message == "" {
			status.Message = "更新安装完成，服务已重新启动"
		}
		a.log.Info("automatic update completed", "version", status.Version)
	}
	if writeErr := writeOperationStatus(a.config.StatusPath, a.groupID, status); writeErr != nil {
		a.log.Error("write final update status failed", "error", writeErr)
	}
}

func tailMessage(output []byte) string {
	const limit = 8000
	if len(output) > limit {
		output = output[len(output)-limit:]
	}
	return strings.TrimSpace(string(output))
}
