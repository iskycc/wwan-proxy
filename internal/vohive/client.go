package vohive

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sync"
	"time"
)

var zeroTime time.Time

const maxResponseBytes = 64 * 1024

type Client struct {
	baseURL  string
	username string
	password string
	http     *http.Client

	mu        sync.Mutex
	token     string
	expiresAt time.Time
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

type LoginResponse struct {
	ExpiresAt time.Time `json:"expires_at"`
	Status    string    `json:"status"`
	Token     string    `json:"token"`
}

func NewClient(baseURL, username, password string, timeout time.Duration) *Client {
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Client{
		baseURL:  baseURL,
		username: username,
		password: password,
		http:     &http.Client{Timeout: timeout},
	}
}

func (c *Client) RestartDevice(ctx context.Context, deviceID string) (NetworkStatus, error) {
	if _, err := c.patchEnabled(ctx, deviceID, false); err != nil {
		return NetworkStatus{}, fmt.Errorf("disable device network: %w", err)
	}

	status, err := c.patchEnabled(ctx, deviceID, true)
	if err != nil {
		// Best-effort: try to leave the device enabled even though the
		// restart's enable step failed.
		if _, reenableErr := c.patchEnabled(ctx, deviceID, true); reenableErr != nil {
			return NetworkStatus{}, fmt.Errorf("enable device network: %w (re-enable also failed: %v)", err, reenableErr)
		}
		return NetworkStatus{}, fmt.Errorf("enable device network: %w", err)
	}
	return status, nil
}

func (c *Client) GetNetworkStatus(ctx context.Context, deviceID string) (NetworkStatus, error) {
	return c.request(ctx, http.MethodGet, deviceNetworkPath(deviceID), nil)
}

func (c *Client) patchEnabled(ctx context.Context, deviceID string, enabled bool) (NetworkStatus, error) {
	body, err := json.Marshal(map[string]bool{"enabled": enabled})
	if err != nil {
		return NetworkStatus{}, err
	}
	return c.request(ctx, http.MethodPatch, deviceNetworkPath(deviceID), body)
}

func deviceNetworkPath(deviceID string) string {
	return "/api/devices/" + url.PathEscape(deviceID) + "/network"
}

func (c *Client) ensureToken(ctx context.Context) error {
	c.mu.Lock()
	needsLogin := c.token == "" || time.Until(c.expiresAt) <= 10*time.Second
	c.mu.Unlock()
	if !needsLogin {
		return nil
	}

	body, err := json.Marshal(map[string]string{"username": c.username, "password": c.password})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("vohive login returned %d: %s", resp.StatusCode, string(respBody))
	}

	var login LoginResponse
	if err := json.Unmarshal(respBody, &login); err != nil {
		return fmt.Errorf("decode vohive login response: %w", err)
	}
	if login.Token == "" {
		return fmt.Errorf("vohive login response did not contain a token")
	}

	c.mu.Lock()
	c.token = login.Token
	if !login.ExpiresAt.IsZero() {
		c.expiresAt = login.ExpiresAt
	} else {
		// Fallback: assume a 24-hour token if the server omits expiry.
		c.expiresAt = time.Now().Add(24 * time.Hour)
	}
	c.mu.Unlock()
	return nil
}

func (c *Client) request(ctx context.Context, method, path string, body []byte) (NetworkStatus, error) {
	return c.requestWithRetry(ctx, method, path, body, true)
}

func (c *Client) requestWithRetry(ctx context.Context, method, path string, body []byte, allowRetry bool) (NetworkStatus, error) {
	if err := c.ensureToken(ctx); err != nil {
		return NetworkStatus{}, fmt.Errorf("vohive authenticate: %w", err)
	}

	c.mu.Lock()
	token := c.token
	c.mu.Unlock()

	var bodyReader io.Reader
	if body != nil {
		bodyReader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, bodyReader)
	if err != nil {
		return NetworkStatus{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return NetworkStatus{}, err
	}
	defer resp.Body.Close()

	// If the token was rejected, clear it and retry once after re-authenticating.
	if resp.StatusCode == http.StatusUnauthorized && allowRetry {
		c.mu.Lock()
		c.token = ""
		c.expiresAt = zeroTime
		c.mu.Unlock()
		return c.requestWithRetry(ctx, method, path, body, false)
	}

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
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
