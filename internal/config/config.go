package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"sort"
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
	ID             int64         `json:"id,omitempty"`
	Enabled        bool          `json:"enabled"`
	Name           string        `json:"name"`
	Listen         string        `json:"listen"`
	Interface      string        `json:"interface"`
	DNS            DNS           `json:"dns"`
	ConnectTimeout Duration      `json:"connect_timeout"`
	IdleTimeout    Duration      `json:"idle_timeout"`
	BindTimeout    Duration      `json:"bind_timeout"`
	Bind           SOCKS5Bind    `json:"bind"`
	MaxConnections int           `json:"max_connections"`
	Auth           Auth          `json:"auth"`
	Access         AccessControl `json:"access"`
	HTTPProxy      HTTPProxy     `json:"http_proxy"`
	Upstream       Upstream      `json:"upstream"`
	UDP            UDP           `json:"udp"`
	Heartbeat      Heartbeat     `json:"heartbeat"`
	VohiveDeviceID string        `json:"vohive_device_id"`
}

// Clone returns a structurally independent configuration snapshot. Server is
// passed by value throughout the runtime, but several nested maps, slices and
// pointers otherwise remain shared. In particular, persistence validates and
// rewrites credential maps in place, so runtime configurations must be cloned
// before they are handed back to the store.
func (s Server) Clone() Server {
	clone := s
	clone.Auth.Users = cloneMap(s.Auth.Users)
	clone.Auth.PasswordUnchanged = cloneSlice(s.Auth.PasswordUnchanged)
	clone.Access.AdmissionCIDRs = cloneSlice(s.Access.AdmissionCIDRs)
	clone.Access.TargetRules = cloneSlice(s.Access.TargetRules)
	clone.DNS.Servers = cloneSlice(s.DNS.Servers)
	if s.DNS.DoH != nil {
		doh := *s.DNS.DoH
		doh.URLs = cloneSlice(s.DNS.DoH.URLs)
		doh.BootstrapIPs = cloneSlice(s.DNS.DoH.BootstrapIPs)
		doh.Headers = cloneMap(s.DNS.DoH.Headers)
		clone.DNS.DoH = &doh
	}
	clone.UDP.AdvertiseMap = cloneMap(s.UDP.AdvertiseMap)
	clone.UDP.RelayPorts = cloneSlice(s.UDP.RelayPorts)
	return clone
}

func cloneSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	clone := make([]T, len(values))
	copy(clone, values)
	return clone
}

func cloneMap[K comparable, V any](values map[K]V) map[K]V {
	if values == nil {
		return nil
	}
	clone := make(map[K]V, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

// SOCKS5Bind controls the SOCKS5 BIND command independently from CONNECT.
// It is deliberately disabled when omitted from legacy configuration JSON.
type SOCKS5Bind struct {
	Enabled   bool   `json:"enabled"`
	Advertise string `json:"advertise,omitempty"`
}

// AccessControl describes listener admission and destination authorization.
// An empty AdmissionCIDRs list admits every source. TargetDefault defaults to
// "allow" so configurations created before ACL support keep their CONNECT
// behavior. Rules are evaluated in order by consumers of this configuration.
type AccessControl struct {
	AdmissionCIDRs          []string `json:"admission_cidrs,omitempty"`
	TargetDefault           string   `json:"target_default"`
	TargetRules             []string `json:"target_rules,omitempty"`
	MaxConnectionsPerIP     int      `json:"max_connections_per_ip"`
	MaxUDPAssociationsPerIP int      `json:"max_udp_associations_per_ip"`
}

// TargetACLRule is the normalized form of an access target rule. The persisted
// form is a compact string: "allow|deny target[:port|port-port]".
type TargetACLRule struct {
	Action  string
	Target  string
	PortMin int
	PortMax int
}

type Auth struct {
	Method            string            `json:"method"`
	Users             map[string]string `json:"users,omitempty"`
	PasswordUnchanged []string          `json:"password_unchanged,omitempty"`
}

type HTTPProxy struct {
	Enabled bool   `json:"enabled"`
	Listen  string `json:"listen"`
}

// Upstream describes an optional upstream SOCKS5 proxy that this server
// uses for outbound CONNECT requests. When enabled, BIND and UDP ASSOCIATE
// are rejected because the upstream client only implements CONNECT.
type Upstream struct {
	Enabled    bool   `json:"enabled"`
	Address    string `json:"address"`
	AuthMethod string `json:"auth_method"`
	Username   string `json:"username"`
	Password   string `json:"password"`
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
	Enabled         bool              `json:"enabled"`
	MaxAssociations int               `json:"max_associations"`
	StrictEndpoint  bool              `json:"strict_endpoint"`
	BindIP          string            `json:"bind_ip"`
	Advertise       string            `json:"advertise"`
	AdvertiseMap    map[string]string `json:"advertise_map,omitempty"`
	IdleTimeout     Duration          `json:"idle_timeout"`
	// RelayPort is the legacy single fixed RFC 1928 UDP relay port. New
	// configurations should use RelayPorts, which supports a non-contiguous
	// pool. The two fields are mutually exclusive.
	RelayPort int `json:"relay_port,omitempty"`
	// RelayPorts is a fixed pool. Each active association atomically owns one
	// port by binding it, and releases that port when the association ends.
	RelayPorts []int `json:"relay_ports,omitempty"`
	PortMin    int   `json:"port_min"`
	PortMax    int   `json:"port_max"`
}

// MaxUDPRelayPortPoolSize bounds the number of bind attempts needed to acquire
// a fixed UDP relay port when the configured pool is occupied. A fixed pool can
// contain more entries than MaxAssociations to leave room for ports used by
// other processes, but accepting an effectively unbounded list would amplify a
// single UDP ASSOCIATE request into tens of thousands of bind syscalls.
const MaxUDPRelayPortPoolSize = 4096

// FixedRelayPorts returns the configured fixed relay pool while retaining
// compatibility with configurations that only contain the legacy relay_port.
// A nil result selects the random port_min..port_max mode.
func (u UDP) FixedRelayPorts() []int {
	if len(u.RelayPorts) != 0 {
		return cloneSlice(u.RelayPorts)
	}
	if u.RelayPort != 0 {
		return []int{u.RelayPort}
	}
	return nil
}

type Heartbeat struct {
	URL      string   `json:"url"`
	Interval Duration `json:"interval"`
	Timeout  Duration `json:"timeout"`
}

type VohiveSettings struct {
	Enabled             bool     `json:"enabled"`
	BaseURL             string   `json:"base_url"`
	Username            string   `json:"username"`
	Password            string   `json:"password"`
	ConsecutiveFailures int      `json:"consecutive_failures"`
	Cooldown            Duration `json:"cooldown"`
}

type SystemSettings struct {
	WebListen        string         `json:"web_listen"`
	DatabasePath     string         `json:"database_path"`
	LogLevel         string         `json:"log_level"`
	LogRetentionDays int            `json:"log_retention_days"`
	SessionLifetime  Duration       `json:"session_lifetime"`
	Vohive           VohiveSettings `json:"vohive"`
}

func (s *SystemSettings) ApplyDefaults() {
	if s.WebListen == "" {
		s.WebListen = "127.0.0.1:9090"
	}
	if s.LogRetentionDays == 0 {
		s.LogRetentionDays = 30
	}
	if s.LogLevel == "" {
		s.LogLevel = "WARN"
	} else {
		s.LogLevel = strings.ToUpper(strings.TrimSpace(s.LogLevel))
	}
	if s.SessionLifetime == 0 {
		s.SessionLifetime = Duration(24 * time.Hour)
	}
	if s.Vohive.ConsecutiveFailures == 0 {
		s.Vohive.ConsecutiveFailures = 2
	}
	if s.Vohive.Cooldown == 0 {
		s.Vohive.Cooldown = Duration(5 * time.Minute)
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
	switch s.LogLevel {
	case "DEBUG", "INFO", "WARN", "ERROR":
	default:
		return fmt.Errorf("log_level must be DEBUG, INFO, WARN, or ERROR")
	}
	lifetime := time.Duration(s.SessionLifetime)
	if lifetime < 5*time.Minute || lifetime > 30*24*time.Hour {
		return fmt.Errorf("session_lifetime must be between 5m and 720h")
	}
	if s.Vohive.Enabled {
		if s.Vohive.BaseURL == "" {
			return fmt.Errorf("vohive.base_url is required when enabled")
		}
		u, err := url.Parse(s.Vohive.BaseURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("vohive.base_url must be a valid http or https URL")
		}
		if s.Vohive.Username == "" {
			return fmt.Errorf("vohive.username is required when enabled")
		}
		if s.Vohive.Password == "" {
			return fmt.Errorf("vohive.password is required when enabled")
		}
		if s.Vohive.ConsecutiveFailures < 1 || s.Vohive.ConsecutiveFailures > 100 {
			return fmt.Errorf("vohive.consecutive_failures must be between 1 and 100")
		}
		cooldown := time.Duration(s.Vohive.Cooldown)
		if cooldown < time.Minute || cooldown > 24*time.Hour {
			return fmt.Errorf("vohive.cooldown must be between 1m and 24h")
		}
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
	if s.Bind.Advertise == "" {
		s.Bind.Advertise = "auto"
	}
	if s.Auth.Method == "" {
		s.Auth.Method = "none"
	}
	if s.Access.TargetDefault == "" {
		s.Access.TargetDefault = "allow"
	}
	if s.Upstream.AuthMethod == "" {
		s.Upstream.AuthMethod = "none"
	}
	if s.UDP.BindIP == "" {
		s.UDP.BindIP = "0.0.0.0"
	}
	if s.UDP.Advertise == "" {
		s.UDP.Advertise = "auto"
	}
	if bindIP := net.ParseIP(s.UDP.BindIP); bindIP != nil {
		s.UDP.BindIP = bindIP.String()
	}
	if s.UDP.Advertise != "auto" {
		if advertiseIP := net.ParseIP(s.UDP.Advertise); advertiseIP != nil {
			s.UDP.Advertise = advertiseIP.String()
		}
	}
	s.UDP.AdvertiseMap = normalizeAdvertiseMapDefaults(s.UDP.AdvertiseMap)
	if s.UDP.IdleTimeout == 0 {
		s.UDP.IdleTimeout = Duration(2 * time.Minute)
	}
	if s.UDP.MaxAssociations == 0 {
		s.UDP.MaxAssociations = 64
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
	if timeout := time.Duration(s.ConnectTimeout); timeout < 100*time.Millisecond || timeout > 10*time.Minute {
		return fmt.Errorf("connect_timeout must be between 100ms and 10m")
	}
	if timeout := time.Duration(s.IdleTimeout); timeout < time.Second || timeout > 24*time.Hour {
		return fmt.Errorf("idle_timeout must be between 1s and 24h")
	}
	if timeout := time.Duration(s.BindTimeout); timeout < time.Second || timeout > 24*time.Hour {
		return fmt.Errorf("bind_timeout must be between 1s and 24h")
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
	if s.Upstream.Enabled {
		if s.Upstream.Address == "" {
			return fmt.Errorf("upstream.address is required when upstream is enabled")
		}
		if _, _, err := net.SplitHostPort(s.Upstream.Address); err != nil {
			return fmt.Errorf("upstream.address: %w", err)
		}
		if s.Upstream.AuthMethod != "none" && s.Upstream.AuthMethod != "username_password" {
			return fmt.Errorf("upstream.auth_method must be none or username_password")
		}
		if s.Upstream.AuthMethod == "username_password" {
			if len(s.Upstream.Username) == 0 || len(s.Upstream.Username) > 255 {
				return fmt.Errorf("upstream.username must be 1..255 bytes")
			}
			if len(s.Upstream.Password) == 0 || len(s.Upstream.Password) > 255 {
				return fmt.Errorf("upstream.password must be 1..255 bytes")
			}
		}
	}
	if s.Bind.Advertise != "auto" {
		advertiseIP := net.ParseIP(s.Bind.Advertise)
		if advertiseIP == nil || !usableAdvertiseIP(advertiseIP) {
			return fmt.Errorf("bind.advertise must be auto or a usable unicast IP address")
		}
	}
	unchangedPasswords := make(map[string]struct{}, len(s.Auth.PasswordUnchanged))
	for _, user := range s.Auth.PasswordUnchanged {
		if _, exists := unchangedPasswords[user]; exists {
			return fmt.Errorf("auth.password_unchanged contains duplicate user %q", user)
		}
		if _, exists := s.Auth.Users[user]; !exists {
			return fmt.Errorf("auth.password_unchanged references unknown user %q", user)
		}
		unchangedPasswords[user] = struct{}{}
	}
	for user, password := range s.Auth.Users {
		_, unchanged := unchangedPasswords[user]
		if len(user) == 0 || len(user) > 255 || (!unchanged && len(password) == 0) || len(password) > 255 {
			return fmt.Errorf("auth usernames and passwords must each contain 1..255 bytes")
		}
	}
	for i, cidr := range s.Access.AdmissionCIDRs {
		if _, _, err := net.ParseCIDR(strings.TrimSpace(cidr)); err != nil {
			return fmt.Errorf("access.admission_cidrs[%d] must be an IPv4 or IPv6 CIDR", i)
		}
	}
	if s.Access.TargetDefault != "allow" && s.Access.TargetDefault != "deny" {
		return fmt.Errorf("access.target_default must be allow or deny")
	}
	if s.Access.MaxConnectionsPerIP < 0 {
		return fmt.Errorf("access.max_connections_per_ip must not be negative")
	}
	if s.Access.MaxUDPAssociationsPerIP < 0 {
		return fmt.Errorf("access.max_udp_associations_per_ip must not be negative")
	}
	for i, rule := range s.Access.TargetRules {
		if _, err := ParseTargetACLRule(rule); err != nil {
			return fmt.Errorf("access.target_rules[%d]: %w", i, err)
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
	bindIP := net.ParseIP(s.UDP.BindIP)
	if bindIP == nil {
		return fmt.Errorf("udp.bind_ip must be an IP address without a port")
	}
	if !usableBindIP(bindIP) {
		return fmt.Errorf("udp.bind_ip must be unspecified or a usable unicast IP address")
	}
	var advertiseIP net.IP
	if s.UDP.Advertise != "auto" {
		advertiseIP = net.ParseIP(s.UDP.Advertise)
		if advertiseIP == nil || !usableAdvertiseIP(advertiseIP) {
			return fmt.Errorf("udp.advertise must be auto or a usable unicast IP address")
		}
	}
	normalizedAdvertiseMap := make(map[string]string, len(s.UDP.AdvertiseMap))
	for local, relay := range s.UDP.AdvertiseMap {
		localIP, relayIP := net.ParseIP(local), net.ParseIP(relay)
		if localIP == nil || relayIP == nil {
			return fmt.Errorf("udp.advertise_map must contain IP-to-IP entries")
		}
		if !usableAdvertiseMapLocalIP(localIP) {
			return fmt.Errorf("udp.advertise_map local addresses must be usable unicast IP addresses")
		}
		if !usableAdvertiseIP(relayIP) {
			return fmt.Errorf("udp.advertise_map relay addresses must be usable unicast IP addresses")
		}
		if !sameIPFamily(bindIP, localIP) || !sameIPFamily(bindIP, relayIP) {
			return fmt.Errorf("udp.advertise_map entries must use the same address family as udp.bind_ip")
		}
		normalizedLocal := localIP.String()
		if _, exists := normalizedAdvertiseMap[normalizedLocal]; exists {
			return fmt.Errorf("udp.advertise_map contains duplicate normalized local IP %q", normalizedLocal)
		}
		normalizedAdvertiseMap[normalizedLocal] = relayIP.String()
	}
	s.UDP.AdvertiseMap = normalizedAdvertiseMap
	if s.UDP.PortMin < 1024 || s.UDP.PortMax > 65535 || s.UDP.PortMin > s.UDP.PortMax {
		return fmt.Errorf("udp port range must be within 1024..65535 and min must not exceed max")
	}
	if s.UDP.RelayPort != 0 && (s.UDP.RelayPort < 1024 || s.UDP.RelayPort > 65535) {
		return fmt.Errorf("udp.relay_port must be 0 or within 1024..65535")
	}
	if s.UDP.RelayPort != 0 && len(s.UDP.RelayPorts) != 0 {
		return fmt.Errorf("udp.relay_port and udp.relay_ports are mutually exclusive")
	}
	if len(s.UDP.RelayPorts) > MaxUDPRelayPortPoolSize {
		return fmt.Errorf("udp.relay_ports must contain at most %d ports", MaxUDPRelayPortPoolSize)
	}
	relayPorts := make(map[int]struct{}, len(s.UDP.RelayPorts))
	for i, port := range s.UDP.RelayPorts {
		if port < 1024 || port > 65535 {
			return fmt.Errorf("udp.relay_ports[%d] must be within 1024..65535", i)
		}
		if _, exists := relayPorts[port]; exists {
			return fmt.Errorf("udp.relay_ports contains duplicate port %d", port)
		}
		relayPorts[port] = struct{}{}
	}
	if s.UDP.MaxAssociations < 1 || s.UDP.MaxAssociations > 65535 {
		return fmt.Errorf("udp.max_associations must be between 1 and 65535")
	}
	if timeout := time.Duration(s.UDP.IdleTimeout); timeout < time.Second || timeout > 24*time.Hour {
		return fmt.Errorf("udp.idle_timeout must be between 1s and 24h")
	}
	if advertiseIP != nil && !sameIPFamily(bindIP, advertiseIP) {
		return fmt.Errorf("udp.advertise must use the same address family as udp.bind_ip")
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
	if len(s.VohiveDeviceID) > 64 {
		return fmt.Errorf("vohive_device_id must not exceed 64 characters")
	}
	return nil
}

func sameIPFamily(a, b net.IP) bool {
	return a != nil && b != nil && (a.To4() != nil) == (b.To4() != nil)
}

func usableAdvertiseIP(ip net.IP) bool {
	if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if ip.IsGlobalUnicast() || ip.IsLoopback() {
		return true
	}
	// IPv4 link-local addresses do not require an interface zone in a SOCKS5
	// reply. IPv6 link-local addresses do, but RFC 1928 cannot carry that zone.
	return ip.To4() != nil && ip.IsLinkLocalUnicast()
}

func usableBindIP(ip net.IP) bool {
	return ip != nil && (ip.IsUnspecified() || usableAdvertiseIP(ip))
}

func usableAdvertiseMapLocalIP(ip net.IP) bool {
	return ip != nil && !ip.IsUnspecified() && !ip.IsMulticast() && (ip.IsGlobalUnicast() || ip.IsLoopback() || ip.IsLinkLocalUnicast())
}

// normalizeAdvertiseMapDefaults upgrades legacy SQLite JSON before it reaches
// the runtime. Invalid or canonically ambiguous maps are left intact so
// Validate can reject them instead of choosing a value based on map order.
func normalizeAdvertiseMapDefaults(values map[string]string) map[string]string {
	if len(values) == 0 {
		return values
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	normalized := make(map[string]string, len(values))
	for _, key := range keys {
		localIP, relayIP := net.ParseIP(key), net.ParseIP(values[key])
		if localIP == nil || relayIP == nil {
			return values
		}
		canonical := localIP.String()
		if _, exists := normalized[canonical]; exists {
			return values
		}
		normalized[canonical] = relayIP.String()
	}
	return normalized
}

// ParseTargetACLRule validates and normalizes a persisted target ACL rule.
// IPv6 targets carrying a port must be bracketed, for example
// "deny [2001:db8::/32]:443". A zero normalized port range means all ports.
func ParseTargetACLRule(value string) (TargetACLRule, error) {
	var result TargetACLRule
	fields := strings.Fields(value)
	if len(fields) != 2 || (fields[0] != "allow" && fields[0] != "deny") {
		return result, fmt.Errorf("must use: allow|deny target[:port|port-port]")
	}
	result.Action = fields[0]
	target, portSpec, err := splitACLTargetPort(fields[1])
	if err != nil {
		return result, err
	}
	if !validACLTarget(target) {
		return result, fmt.Errorf("target must be *, an IP, CIDR, hostname, or *.hostname")
	}
	result.Target = target
	if portSpec == "" || portSpec == "*" {
		return result, nil
	}
	portParts := strings.Split(portSpec, "-")
	if len(portParts) > 2 {
		return result, fmt.Errorf("port must be 1..65535 or a valid range")
	}
	min, err := strconv.Atoi(portParts[0])
	if err != nil || min < 1 || min > 65535 {
		return result, fmt.Errorf("port must be 1..65535 or a valid range")
	}
	max := min
	if len(portParts) == 2 {
		max, err = strconv.Atoi(portParts[1])
		if err != nil || max < min || max > 65535 {
			return result, fmt.Errorf("port must be 1..65535 or a valid range")
		}
	}
	result.PortMin, result.PortMax = min, max
	return result, nil
}

func splitACLTargetPort(value string) (string, string, error) {
	if strings.HasPrefix(value, "[") {
		end := strings.LastIndex(value, "]")
		if end < 2 {
			return "", "", fmt.Errorf("invalid bracketed target")
		}
		target := value[1:end]
		rest := value[end+1:]
		if rest == "" {
			return target, "", nil
		}
		if !strings.HasPrefix(rest, ":") || len(rest) == 1 {
			return "", "", fmt.Errorf("invalid target port")
		}
		return target, rest[1:], nil
	}
	if validACLTarget(value) {
		return value, "", nil
	}
	if colon := strings.LastIndexByte(value, ':'); colon > 0 && colon < len(value)-1 {
		return value[:colon], value[colon+1:], nil
	}
	return value, "", nil
}

func validACLTarget(value string) bool {
	if value == "*" || net.ParseIP(value) != nil {
		return true
	}
	if _, _, err := net.ParseCIDR(value); err == nil {
		return true
	}
	if strings.HasPrefix(value, "*.") {
		value = value[2:]
	}
	value = strings.TrimSuffix(value, ".")
	if value == "" || len(value) > 253 {
		return false
	}
	for _, label := range strings.Split(value, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') && (char < '0' || char > '9') && char != '-' && char != '_' {
				return false
			}
		}
	}
	return true
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
