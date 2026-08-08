package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type Controller struct {
	checker    *Checker
	client     Client
	statusPath string
}

func NewController(repository, currentVersion, socketPath, statusPath string) (*Controller, error) {
	checker, err := NewChecker(repository, currentVersion)
	if err != nil {
		return nil, err
	}
	return &Controller{checker: checker, client: Client{SocketPath: socketPath, Timeout: 2 * time.Second}, statusPath: statusPath}, nil
}

func (c *Controller) Status(ctx context.Context, checkRemote bool, downloadInterface string) (Info, error) {
	info := c.checker.LocalInfo()
	operation, err := ReadOperationStatus(c.statusPath)
	if err != nil {
		return info, err
	}
	info.Operation = operation
	if info.InstallSupported {
		pingContext, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
		err = c.client.Ping(pingContext)
		cancel()
		if err != nil {
			info.InstallSupported = false
			info.InstallMessage = "未检测到特权更新代理，请重新运行当前系统的安装脚本以启用 Web 更新"
		}
	}
	if !checkRemote {
		return info, nil
	}
	checked, err := c.checker.Check(ctx, downloadInterface)
	if err != nil {
		return info, err
	}
	checked.InstallSupported = info.InstallSupported
	checked.InstallMessage = info.InstallMessage
	checked.Operation = operation
	return checked, nil
}

func (c *Controller) Trigger(ctx context.Context, downloadInterface string) (Info, error) {
	info, err := c.Status(ctx, true, downloadInterface)
	if err != nil {
		return info, err
	}
	if !info.InstallSupported {
		return info, fmt.Errorf("%w: %s", ErrUpdateUnsupported, info.InstallMessage)
	}
	if !info.UpdateAvailable {
		return info, ErrNoUpdate
	}
	if info.Operation != nil && (info.Operation.State == "queued" || info.Operation.State == "running") {
		return info, ErrUpdateInProgress
	}
	if err := c.client.Trigger(ctx, downloadInterface); err != nil {
		if errors.Is(err, ErrUpdateInProgress) {
			return info, err
		}
		return info, fmt.Errorf("schedule update: %w", err)
	}
	return info, nil
}
