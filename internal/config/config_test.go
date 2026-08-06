package config

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestServerJSONAndDefaults(t *testing.T) {
	var cfg Server
	if err := json.Unmarshal([]byte(`{
      "enabled":true,"name":"test","listen":"127.0.0.1:1080","interface":"lo",
      "connect_timeout":"3s","auth":{"method":"none"},
      "udp":{"enabled":true,"bind_ip":"127.0.0.1","advertise":"auto"}
    }`), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.ConnectTimeout.Value(0) != 3*time.Second || cfg.IdleTimeout.Value(0) != 5*time.Minute {
		t.Fatalf("unexpected durations %+v", cfg)
	}
	if cfg.BindTimeout.Value(0) != 2*time.Minute || cfg.UDP.PortMin != 10000 || cfg.UDP.PortMax != 65535 {
		t.Fatalf("unexpected relay defaults %+v", cfg)
	}
	if cfg.Heartbeat.URL != "https://1.1.1.1/cdn-cgi/trace" || cfg.Heartbeat.Interval.Value(0) != 30*time.Second {
		t.Fatalf("unexpected heartbeat defaults %+v", cfg.Heartbeat)
	}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) == 0 {
		t.Fatal("empty JSON")
	}
}

func TestSystemSettingsLogLevelDefaultsAndNormalization(t *testing.T) {
	settings := SystemSettings{}
	settings.ApplyDefaults()
	if settings.LogLevel != "WARN" {
		t.Fatalf("default log level=%q", settings.LogLevel)
	}
	settings.LogLevel = " debug "
	settings.WebListen = "127.0.0.1:9090"
	if err := settings.Validate(); err != nil {
		t.Fatal(err)
	}
	if settings.LogLevel != "DEBUG" {
		t.Fatalf("normalized log level=%q", settings.LogLevel)
	}
}

func TestLegacyUDPRelayPortJSONCompatibility(t *testing.T) {
	var cfg Server
	if err := json.Unmarshal([]byte(`{
      "name":"legacy-port","listen":"127.0.0.1:1080","interface":"lo",
      "udp":{"enabled":true,"bind_ip":"127.0.0.1","advertise":"auto","relay_port":53000}
    }`), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("legacy relay_port config rejected: %v", err)
	}
	ports := cfg.UDP.FixedRelayPorts()
	if len(ports) != 1 || ports[0] != 53000 {
		t.Fatalf("legacy relay_port resolved to %v", ports)
	}
}

func TestHTTPProxyValidation(t *testing.T) {
	cfg := Server{
		Name: "test", Listen: "127.0.0.1:1080", Interface: "lo", Auth: Auth{Method: "none"},
		HTTPProxy: HTTPProxy{Enabled: true, Listen: "127.0.0.1:8080"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.HTTPProxy.Listen = cfg.Listen
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted an HTTP proxy listener that conflicts with SOCKS5")
	}
	cfg.HTTPProxy.Listen = "missing-port"
	if err := cfg.Validate(); err == nil {
		t.Fatal("accepted an invalid HTTP proxy listen address")
	}
}

func TestCustomUDPRangeAndHeartbeatValidation(t *testing.T) {
	cfg := Server{
		Name: "test", Listen: "127.0.0.1:1080", Interface: "lo", Auth: Auth{Method: "none"},
		UDP:       UDP{Enabled: true, BindIP: "127.0.0.1", Advertise: "auto", PortMin: 12000, PortMax: 13000},
		Heartbeat: Heartbeat{URL: "https://example.test/health", Interval: Duration(45 * time.Second), Timeout: Duration(10 * time.Second)},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.UDP.PortMin = 14000
	if err := cfg.Validate(); err == nil {
		t.Fatal("reversed UDP range accepted")
	}
}

func TestDoHDomainRequiresBootstrapDNS(t *testing.T) {
	cfg := Server{Name: "test", Listen: "127.0.0.1:1080", Interface: "lo", DNS: DNS{DoH: &DoH{URL: "https://dns.example/dns-query"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestDoHMultipleEndpointsAndLegacyURL(t *testing.T) {
	legacy := DoH{URL: "https://legacy.example/dns-query"}
	if got := legacy.Endpoints(); len(got) != 1 || got[0] != legacy.URL {
		t.Fatalf("legacy DoH URL was not preserved: %v", got)
	}
	doh := DoH{
		URL:          "https://ignored.example/dns-query",
		URLs:         []string{" https://one.example/dns-query ", "https://two.example/dns-query", "https://one.example/dns-query"},
		BootstrapIPs: []string{"114.114.114.114"}, Timeout: Duration(time.Second),
	}
	got := doh.Endpoints()
	if len(got) != 2 || got[0] != "https://one.example/dns-query" || got[1] != "https://two.example/dns-query" {
		t.Fatalf("multiple DoH endpoints were not normalized: %v", got)
	}
	cfg := Server{Name: "test", Listen: "127.0.0.1:1080", Interface: "lo", DNS: DNS{DoH: &doh}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDoHTimeoutRange(t *testing.T) {
	for _, timeout := range []time.Duration{time.Millisecond, 3 * time.Minute} {
		cfg := Server{Name: "test", Listen: "127.0.0.1:1080", Interface: "lo", DNS: DNS{DoH: &DoH{
			URL: "https://dns.example/dns-query", BootstrapIPs: []string{"192.0.2.53"}, Timeout: Duration(timeout),
		}}}
		if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "dns.doh.timeout") {
			t.Fatalf("timeout=%v err=%v", timeout, err)
		}
	}
}

func TestIPv4OnlyDoHRejectsIPv6EndpointLiteral(t *testing.T) {
	doh := DoH{URL: "https://[2001:db8::53]/dns-query", Timeout: Duration(time.Second)}
	cfg := Server{Name: "test", Listen: "127.0.0.1:1080", Interface: "lo", DNS: DNS{IPv4Only: true, DoH: &doh}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "IPv6") {
		t.Fatalf("DoH=%+v err=%v", doh, err)
	}
}

func TestNormalizeBootstrapDNSAddress(t *testing.T) {
	tests := map[string]string{
		"114.114.114.114":     "114.114.114.114:53",
		"[2001:db8::53]:5353": "[2001:db8::53]:5353",
		"127.0.0.1:1053":      "127.0.0.1:1053",
	}
	for input, want := range tests {
		got, err := NormalizeBootstrapDNSAddress(input)
		if err != nil || got != want {
			t.Fatalf("NormalizeBootstrapDNSAddress(%q)=%q, %v; want %q", input, got, err, want)
		}
	}
	for _, invalid := range []string{"dns.example", "127.0.0.1:0", "127.0.0.1:65536"} {
		if _, err := NormalizeBootstrapDNSAddress(invalid); err == nil {
			t.Fatalf("invalid bootstrap DNS %q accepted", invalid)
		}
	}
}

func TestAccessDefaultsAndBindSafetyForLegacyJSON(t *testing.T) {
	var cfg Server
	if err := json.Unmarshal([]byte(`{"name":"legacy","listen":"127.0.0.1:1080","interface":"lo"}`), &cfg); err != nil {
		t.Fatal(err)
	}
	cfg.ApplyDefaults()
	if cfg.Bind.Enabled {
		t.Fatal("legacy configuration unexpectedly enabled SOCKS5 BIND")
	}
	if cfg.Access.TargetDefault != "allow" {
		t.Fatalf("legacy target default=%q, want allow", cfg.Access.TargetDefault)
	}
}

func TestAccessControlValidationAndTargetRuleParsing(t *testing.T) {
	cfg := Server{
		Name: "acl", Listen: "127.0.0.1:1080", Interface: "lo", Auth: Auth{Method: "none"},
		Access: AccessControl{
			AdmissionCIDRs: []string{"10.0.0.0/8", "2001:db8::/32"}, TargetDefault: "deny",
			TargetRules:         []string{"allow *.example.com:443", "deny 192.0.2.0/24:1-1023", "allow [2001:db8::/32]:8443"},
			MaxConnectionsPerIP: 12, MaxUDPAssociationsPerIP: 2,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	rule, err := ParseTargetACLRule("deny 192.0.2.0/24:20-21")
	if err != nil || rule.Action != "deny" || rule.Target != "192.0.2.0/24" || rule.PortMin != 20 || rule.PortMax != 21 {
		t.Fatalf("rule=%+v err=%v", rule, err)
	}
	for _, invalid := range []string{"permit *", "allow", "deny example.com:0", "allow bad..example", "deny [2001:db8::1]:2-1"} {
		if _, err := ParseTargetACLRule(invalid); err == nil {
			t.Fatalf("invalid rule %q accepted", invalid)
		}
	}
	cfg.Access.AdmissionCIDRs = []string{"10.0.0.1"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "admission_cidrs") {
		t.Fatalf("invalid admission CIDR err=%v", err)
	}
	cfg.Access.AdmissionCIDRs = nil
	cfg.Access.MaxConnectionsPerIP = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "max_connections_per_ip") {
		t.Fatalf("negative per-IP connection limit err=%v", err)
	}
}

func TestStructuredUnchangedPasswordValidation(t *testing.T) {
	cfg := Server{
		Name: "auth", Listen: "127.0.0.1:1080", Interface: "lo",
		Auth: Auth{Method: "username_password", Users: map[string]string{"alice": ""}, PasswordUnchanged: []string{"alice"}},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("structured unchanged password rejected: %v", err)
	}
	cfg.Auth.PasswordUnchanged = nil
	if err := cfg.Validate(); err == nil {
		t.Fatal("empty password without unchanged marker was accepted")
	}
	cfg.Auth.PasswordUnchanged = []string{"missing"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("unchanged marker for missing user was accepted")
	}
}

func TestBindAdvertiseValidation(t *testing.T) {
	for _, address := range []string{"not-an-ip", "0.0.0.0", "::", "fe80::1", "ff02::1", "255.255.255.255"} {
		cfg := Server{Name: "bind", Listen: "127.0.0.1:1080", Interface: "lo", Bind: SOCKS5Bind{Advertise: address}}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid BIND advertise address %q was accepted", address)
		}
	}
}

func TestVohiveSettingsValidation(t *testing.T) {
	base := SystemSettings{WebListen: "127.0.0.1:9090"}

	t.Run("disabled is valid with empty fields", func(t *testing.T) {
		s := base
		s.Vohive = VohiveSettings{Enabled: false}
		if err := s.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("enabled requires base_url", func(t *testing.T) {
		s := base
		s.Vohive = VohiveSettings{Enabled: true, Username: "user", Password: "pass"}
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "base_url") {
			t.Fatalf("expected base_url error, got %v", err)
		}
	})

	t.Run("enabled requires valid http or https base_url", func(t *testing.T) {
		s := base
		s.Vohive = VohiveSettings{Enabled: true, BaseURL: "ftp://example.com", Username: "user", Password: "pass"}
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "base_url") {
			t.Fatalf("expected base_url error, got %v", err)
		}
	})

	t.Run("enabled requires username and password", func(t *testing.T) {
		s := base
		s.Vohive = VohiveSettings{Enabled: true, BaseURL: "http://example.com"}
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "username") {
			t.Fatalf("expected username error, got %v", err)
		}
		s.Vohive.Username = "user"
		if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "password") {
			t.Fatalf("expected password error, got %v", err)
		}
	})

	t.Run("enabled with valid fields is valid", func(t *testing.T) {
		s := base
		s.Vohive = VohiveSettings{Enabled: true, BaseURL: "http://example.com", Username: "user", Password: "pass"}
		if err := s.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("consecutive_failures out of range", func(t *testing.T) {
		for _, value := range []int{-1, 101} {
			s := base
			s.Vohive = VohiveSettings{Enabled: true, BaseURL: "http://example.com", Username: "user", Password: "pass", ConsecutiveFailures: value}
			if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "consecutive_failures") {
				t.Fatalf("value=%d expected consecutive_failures error, got %v", value, err)
			}
		}
	})

	t.Run("consecutive_failures zero applies default", func(t *testing.T) {
		s := base
		s.Vohive = VohiveSettings{Enabled: true, BaseURL: "http://example.com", Username: "user", Password: "pass", ConsecutiveFailures: 0}
		if err := s.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if s.Vohive.ConsecutiveFailures != 2 {
			t.Fatalf("default consecutive_failures=%d, want 2", s.Vohive.ConsecutiveFailures)
		}
	})

	t.Run("cooldown out of range", func(t *testing.T) {
		for _, value := range []time.Duration{30 * time.Second, 25 * time.Hour} {
			s := base
			s.Vohive = VohiveSettings{Enabled: true, BaseURL: "http://example.com", Username: "user", Password: "pass", Cooldown: Duration(value)}
			if err := s.Validate(); err == nil || !strings.Contains(err.Error(), "cooldown") {
				t.Fatalf("value=%v expected cooldown error, got %v", value, err)
			}
		}
	})
}

func TestVohiveDeviceIDLength(t *testing.T) {
	cfg := Server{
		Name: "test", Listen: "127.0.0.1:1080", Interface: "lo", Auth: Auth{Method: "none"},
	}
	cfg.VohiveDeviceID = strings.Repeat("a", 64)
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	cfg.VohiveDeviceID = strings.Repeat("a", 65)
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "vohive_device_id") {
		t.Fatalf("expected vohive_device_id error, got %v", err)
	}
}

func TestUpstreamValidation(t *testing.T) {
	base := func() Server {
		return Server{
			Name: "test", Listen: "0.0.0.0:1080", Interface: "eth0",
		}
	}

	t.Run("disabled upstream is valid", func(t *testing.T) {
		s := base()
		s.Upstream = Upstream{Enabled: false}
		if err := s.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("enabled upstream requires address", func(t *testing.T) {
		s := base()
		s.Upstream = Upstream{Enabled: true}
		if err := s.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("enabled upstream with no auth", func(t *testing.T) {
		s := base()
		s.Upstream = Upstream{Enabled: true, Address: "10.0.0.1:1080", AuthMethod: "none"}
		if err := s.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("username_password requires credentials", func(t *testing.T) {
		s := base()
		s.Upstream = Upstream{Enabled: true, Address: "10.0.0.1:1080", AuthMethod: "username_password"}
		if err := s.Validate(); err == nil {
			t.Fatal("expected error")
		}
	})

	t.Run("username_password with credentials is valid", func(t *testing.T) {
		s := base()
		s.Upstream = Upstream{Enabled: true, Address: "10.0.0.1:1080", AuthMethod: "username_password", Username: "u", Password: "p"}
		if err := s.Validate(); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}
