package selfupdate

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

const maxProtocolMessage = 4096

type agentRequest struct {
	Action    string `json:"action"`
	Interface string `json:"interface,omitempty"`
}

type agentResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

type Client struct {
	SocketPath string
	Timeout    time.Duration
}

func (c Client) Ping(ctx context.Context) error {
	return c.exchange(ctx, agentRequest{Action: "ping"})
}

func (c Client) Trigger(ctx context.Context, downloadInterface string) error {
	return c.exchange(ctx, agentRequest{Action: "update-latest", Interface: downloadInterface})
}

func (c Client) exchange(ctx context.Context, request agentRequest) error {
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	dialer := net.Dialer{Timeout: timeout}
	conn, err := dialer.DialContext(ctx, "unix", c.SocketPath)
	if err != nil {
		return fmt.Errorf("connect to update agent: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)
	if err := json.NewEncoder(conn).Encode(request); err != nil {
		return fmt.Errorf("send update agent request: %w", err)
	}
	reader := bufio.NewReader(io.LimitReader(conn, maxProtocolMessage))
	var response agentResponse
	if err := json.NewDecoder(reader).Decode(&response); err != nil {
		return fmt.Errorf("read update agent response: %w", err)
	}
	if !response.OK {
		if response.Error == "update already in progress" {
			return ErrUpdateInProgress
		}
		if response.Error == "" {
			response.Error = "request rejected"
		}
		return errors.New(response.Error)
	}
	return nil
}
