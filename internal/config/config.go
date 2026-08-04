package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	if bytes.Equal(b, []byte("null")) || bytes.Equal(b, []byte(`""`)) {
		*d = 0
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string: %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

func (d Duration) Value(def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return time.Duration(d)
}

type Server struct {
	ID             int64     `json:"id,omitempty"`
	Enabled        bool      `json:"enabled"`
	Name           string    `json:"name"`
	Listen         string    `json:"listen"`
	Interface      string    `json:"interface"`
	DNS            DNS       `json:"dns"`
	ConnectTimeout Duration  `json:"connect_timeout"`
	IdleTimeout    Duration  `json:"idle_timeout"`
	BindTimeout    Duration  `json:"bind_timeout"`
	MaxConnections int       `json:"max_connections"`
	Auth           Auth      `json:"auth"`
	HTTPProxy      HTTPProxy `json:"http_proxy"`
	UDP            UDP       `json:"udp"`
	Heartbeat      Heartbeat `json:"heartbeat"`
}

type Auth struct {
	Method string            `json:"method"`
	Users  map[string]string `json:"users,omitempty"`
}

type HTTPProxy struct {
	Enabled bool   `json:"enabled"`
	Listen  string `json:"listen"`
}

type DNS struct {
	IPv4Only bool     `json:"ipv4_only"`
	Servers  []string `json:"servers,omitempty"`
	DoH      *DoH     `json:"doh,omitempty"`
}

type DoH struct {
	// URL is retained for backward compatibility with existing SQLite data.
	URL                string            `json:"url,omitempty"`
	URLs               []string          `json:"urls,omitempty"`
	BootstrapIPs       []string          `json:"bootstrap_ips"`
	Timeout            Duration          `json:"timeout"`
	Headers            map[string]string `json:"headers,omitempty"`
	InsecureSkipVerify bool              `json:"insecure_skip_verify"`
}

func (d DoH) Endpoints() []string {
	values := d.URLs
	if len(values) == 0 && d.URL != "" {
		values = []string{d.URL}
	}
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

type UDP struct {
	Enabled      bool              `json:"enabled"`
	BindIP       string            `json:"bind_ip"`
	Advertise    string            `json:"advertise"`
	AdvertiseMap map[string]string `json:"advertise_map,omitempty"`
	IdleTimeout  Duration          `json:"idle_timeout"`
	PortMin      int               `json:"port_min"`
	PortMax      int               `json:"port_max"`
}

type Heartbeat struct {
	URL      string   `json:"url"`
	Interval Duration `json:"interval"`
	Timeout  Duration `json:"timeout"`
}

type SystemSettings struct {
	WebListen        string   `json:"web_listen"`
	DatabasePath     string   `json:"database_path"`
	LogRetentionDays int      `json:"log_retention_days"`
	SessionLifetime  Duration `json:"session_lifetime"`
}

func (s *SystemSettings) ApplyDefaults() {
	if s.WebListen == "" {
		s.WebListen = "127.0.0.1:9090"
	}
	if s.LogRetentionDays == 0 {
		s.LogRetentionDays = 30
	}
	if s.SessionLifetime == 0 {
		s.SessionLifetime = Duration(24 * time.Hour)
	}
}

func (s *SystemSettings) Validate() error {
	s.ApplyDefaults()
	if _, _, err := net.SplitHostPort(s.WebListen); err != nil {
		return fmt.Errorf("web_listen: %w", err)
	}
	if s.DatabasePath != "" && !filepath.IsAbs(s.DatabasePath) {
		return fmt.Errorf("database_path must be an absolute path")
	}
	if s.LogRetentionDays < 1 || s.LogRetentionDays > 3650 {
		return fmt.Errorf("log_retention_days must be between 1 and 3650")
	}
	lifetime := time.Duration(s.SessionLifetime)
	if lifetime < 5*time.Minute || lifetime > 30*24*time.Hour {
		return fmt.Errorf("session_lifetime must be between 5m and 720h")
	}
	return nil
}

func (s *Server) ApplyDefaults() {
	if s.Name == "" {
		s.Name = s.Interface
	}
	if s.ConnectTimeout == 0 {
		s.ConnectTimeout = Duration(10 * time.Second)
	}
	if s.IdleTimeout == 0 {
		s.IdleTimeout = Duration(5 * time.Minute)
	}
	if s.BindTimeout == 0 {
		s.BindTimeout = Duration(2 * time.Minute)
	}
	if s.Auth.Method == "" {
		s.Auth.Method = "none"
	}
	if s.UDP.BindIP == "" {
		s.UDP.BindIP = "0.0.0.0"
	}
	if s.UDP.Advertise == "" {
		s.UDP.Advertise = "auto"
	}
	if s.UDP.IdleTimeout == 0 {
		s.UDP.IdleTimeout = Duration(2 * time.Minute)
	}
	if s.UDP.PortMin == 0 {
		s.UDP.PortMin = 10000
	}
	if s.UDP.PortMax == 0 {
		s.UDP.PortMax = 65535
	}
	if s.Heartbeat.URL == "" {
		s.Heartbeat.URL = "https://1.1.1.1/cdn-cgi/trace"
	}
	if s.Heartbeat.Interval == 0 {
		s.Heartbeat.Interval = Duration(30 * time.Second)
	}
	if s.Heartbeat.Timeout == 0 {
		s.Heartbeat.Timeout = Duration(12 * time.Second)
	}
	if s.DNS.DoH != nil && s.DNS.DoH.Timeout == 0 {
		s.DNS.DoH.Timeout = Duration(10 * time.Second)
	}
}

func (s *Server) Validate() error {
	s.ApplyDefaults()
	if s.Name == "" {
		return fmt.Errorf("name is required")
	}
	if s.Listen == "" {
		return fmt.Errorf("listen is required")
	}
	if _, _, err := net.SplitHostPort(s.Listen); err != nil {
		return fmt.Errorf("listen: %w", err)
	}
	if s.Interface == "" {
		return fmt.Errorf("interface is required")
	}
	if s.MaxConnections < 0 {
		return fmt.Errorf("max_connections must not be negative")
	}
	if s.Auth.Method != "none" && s.Auth.Method != "username_password" {
		return fmt.Errorf("auth.method must be none or username_password")
	}
	if s.Auth.Method == "username_password" && len(s.Auth.Users) == 0 {
		return fmt.Errorf("auth.users must not be empty")
	}
	if s.HTTPProxy.Enabled {
		if s.HTTPProxy.Listen == "" {
			return fmt.Errorf("http_proxy.listen is required when enabled")
		}
		if _, _, err := net.SplitHostPort(s.HTTPProxy.Listen); err != nil {
			return fmt.Errorf("http_proxy.listen: %w", err)
		}
		if s.HTTPProxy.Listen == s.Listen {
			return fmt.Errorf("http_proxy.listen must differ from the SOCKS5 listen address")
		}
	}
	for user, password := range s.Auth.Users {
		if len(user) == 0 || len(user) > 255 || len(password) == 0 || len(password) > 255 {
			return fmt.Errorf("auth usernames and passwords must each contain 1..255 bytes")
		}
	}
	if len(s.DNS.Servers) > 0 && s.DNS.DoH != nil {
		return fmt.Errorf("dns.servers and dns.doh are mutually exclusive")
	}
	for i, server := range s.DNS.Servers {
		if _, _, err := net.SplitHostPort(server); err != nil {
			return fmt.Errorf("dns.servers[%d] must include a port: %w", i, err)
		}
	}
	if s.DNS.DoH != nil {
		doh := s.DNS.DoH
		endpoints := doh.Endpoints()
		if len(endpoints) == 0 {
			return fmt.Errorf("dns.doh.urls must contain at least one DoH URL")
		}
		for i, endpoint := range endpoints {
			u, err := url.Parse(endpoint)
			if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil {
				return fmt.Errorf("dns.doh.urls[%d] must be a valid https URL", i)
			}
			if net.ParseIP(u.Hostname()) == nil && len(doh.BootstrapIPs) == 0 {
				return fmt.Errorf("dns.doh.bootstrap_ips is required when a DoH URL uses a domain")
			}
			if s.DNS.IPv4Only {
				if endpointIP := net.ParseIP(u.Hostname()); endpointIP != nil && endpointIP.To4() == nil {
					return fmt.Errorf("dns.doh.urls[%d] cannot use an IPv6 literal when dns.ipv4_only is enabled", i)
				}
			}
		}
		for i, server := range doh.BootstrapIPs {
			if _, err := NormalizeBootstrapDNSAddress(server); err != nil {
				return fmt.Errorf("dns.doh.bootstrap_ips[%d]: %w", i, err)
			}
		}
		if time.Duration(doh.Timeout) < time.Second || time.Duration(doh.Timeout) > 2*time.Minute {
			return fmt.Errorf("dns.doh.timeout must be between 1s and 2m")
		}
	}
	if net.ParseIP(s.UDP.BindIP) == nil {
		return fmt.Errorf("udp.bind_ip must be an IP address without a port")
	}
	if s.UDP.Advertise != "auto" && net.ParseIP(s.UDP.Advertise) == nil {
		return fmt.Errorf("udp.advertise must be auto or an IP address")
	}
	for local, relay := range s.UDP.AdvertiseMap {
		if net.ParseIP(local) == nil || net.ParseIP(relay) == nil {
			return fmt.Errorf("udp.advertise_map must contain IP-to-IP entries")
		}
	}
	if s.UDP.PortMin < 1024 || s.UDP.PortMax > 65535 || s.UDP.PortMin > s.UDP.PortMax {
		return fmt.Errorf("udp port range must be within 1024..65535 and min must not exceed max")
	}
	heartbeatURL, err := url.Parse(s.Heartbeat.URL)
	if err != nil || (heartbeatURL.Scheme != "http" && heartbeatURL.Scheme != "https") || heartbeatURL.Hostname() == "" {
		return fmt.Errorf("heartbeat.url must be a valid http or https URL")
	}
	if time.Duration(s.Heartbeat.Interval) < 5*time.Second || time.Duration(s.Heartbeat.Interval) > 24*time.Hour {
		return fmt.Errorf("heartbeat.interval must be between 5s and 24h")
	}
	if time.Duration(s.Heartbeat.Timeout) < time.Second || time.Duration(s.Heartbeat.Timeout) > time.Duration(s.Heartbeat.Interval) {
		return fmt.Errorf("heartbeat.timeout must be between 1s and heartbeat.interval")
	}
	return nil
}

// NormalizeBootstrapDNSAddress accepts an IP literal with an optional port.
// Bootstrap values are traditional DNS servers, not fixed addresses for the
// DoH HTTPS endpoint.
func NormalizeBootstrapDNSAddress(server string) (string, error) {
	if ip := net.ParseIP(server); ip != nil {
		return net.JoinHostPort(ip.String(), "53"), nil
	}
	host, port, err := net.SplitHostPort(server)
	if err != nil || net.ParseIP(host) == nil {
		return "", fmt.Errorf("must be a DNS server IP with optional port")
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return "", fmt.Errorf("must use a port between 1 and 65535")
	}
	return net.JoinHostPort(net.ParseIP(host).String(), port), nil
}
