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

func TestDoHDomainRequiresBootstrapIP(t *testing.T) {
	cfg := Server{Name: "test", Listen: "127.0.0.1:1080", Interface: "lo", DNS: DNS{DoH: &DoH{URL: "https://dns.example/dns-query"}}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
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
