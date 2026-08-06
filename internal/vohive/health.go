package vohive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

type DeviceHealth struct {
	Healthy          bool `json:"healthy"`
	ModemOK          bool `json:"modem_ok"`
	IfaceUp          bool `json:"iface_up"`
	NetworkConnected bool `json:"network_connected"`
	Signal           int  `json:"signal"`
}

type HealthResponse struct {
	Status  string                  `json:"status"`
	Devices map[string]DeviceHealth `json:"devices"`
}

func (c *Client) GetHealth(ctx context.Context) (*HealthResponse, error) {
	return c.getHealth(ctx, true)
}

func (c *Client) getHealth(ctx context.Context, allowRetry bool) (*HealthResponse, error) {
	if err := c.ensureToken(ctx); err != nil {
		return nil, fmt.Errorf("vohive authenticate: %w", err)
	}

	c.mu.Lock()
	token := c.token
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/api/health", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// If the token was rejected, clear it and retry once after re-authenticating.
	if resp.StatusCode == http.StatusUnauthorized && allowRetry {
		c.mu.Lock()
		c.token = ""
		c.expiresAt = zeroTime
		c.mu.Unlock()
		return c.getHealth(ctx, false)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("vohive GET /api/health returned %d: %s", resp.StatusCode, string(body))
	}

	var health HealthResponse
	if err := json.Unmarshal(body, &health); err != nil {
		return nil, fmt.Errorf("decode vohive health response: %w", err)
	}
	return &health, nil
}
