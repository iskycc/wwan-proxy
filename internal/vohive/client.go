package vohive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

type NetworkStatus struct {
	Device           string `json:"device"`
	Message          string `json:"message"`
	NetworkConnected bool   `json:"network_connected"`
	PrivateIP        string `json:"private_ip"`
	PrivateIPv6      string `json:"private_ipv6"`
	PublicIP         string `json:"public_ip"`
	PublicIPv6       string `json:"public_ipv6"`
	Status           string `json:"status"`
}

func NewClient(baseURL, token string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: timeout},
	}
}

func (c *Client) RestartDevice(ctx context.Context, deviceID string) (NetworkStatus, error) {
	if _, err := c.patchEnabled(ctx, deviceID, false); err != nil {
		return NetworkStatus{}, fmt.Errorf("disable device network: %w", err)
	}
	return c.patchEnabled(ctx, deviceID, true)
}

func (c *Client) GetNetworkStatus(ctx context.Context, deviceID string) (NetworkStatus, error) {
	return c.request(ctx, http.MethodGet, deviceNetworkPath(deviceID), nil)
}

func (c *Client) patchEnabled(ctx context.Context, deviceID string, enabled bool) (NetworkStatus, error) {
	body, _ := json.Marshal(map[string]bool{"enabled": enabled})
	return c.request(ctx, http.MethodPatch, deviceNetworkPath(deviceID), body)
}

func deviceNetworkPath(deviceID string) string {
	return "/api/devices/" + deviceID + "/network"
}

func (c *Client) request(ctx context.Context, method, path string, body []byte) (NetworkStatus, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return NetworkStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return NetworkStatus{}, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return NetworkStatus{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return NetworkStatus{}, fmt.Errorf("vohive %s %s returned %d: %s", method, path, resp.StatusCode, string(respBody))
	}

	var status NetworkStatus
	if err := json.Unmarshal(respBody, &status); err != nil {
		return NetworkStatus{}, fmt.Errorf("decode vohive response: %w", err)
	}
	return status, nil
}
