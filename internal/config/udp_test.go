package config

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestUDPFixedRelayPortValidation(t *testing.T) {
	cfg := validUDPServerConfig()
	cfg.UDP.RelayPort = 53000
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid fixed relay port rejected: %v", err)
	}
	if got := cfg.UDP.FixedRelayPorts(); len(got) != 1 || got[0] != 53000 {
		t.Fatalf("legacy relay_port effective pool=%v, want [53000]", got)
	}
	if cfg.UDP.MaxAssociations != 64 {
		t.Fatalf("default max associations=%d, want 64", cfg.UDP.MaxAssociations)
	}

	for _, port := range []int{-1, 1, 1023, 65536} {
		cfg := validUDPServerConfig()
		cfg.UDP.RelayPort = port
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid fixed relay port %d accepted", port)
		}
	}
}

func TestUDPRelayPortPoolValidation(t *testing.T) {
	cfg := validUDPServerConfig()
	cfg.UDP.RelayPorts = []int{12000, 12007, 53000}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid non-contiguous relay port pool rejected: %v", err)
	}
	got := cfg.UDP.FixedRelayPorts()
	if len(got) != 3 || got[0] != 12000 || got[1] != 12007 || got[2] != 53000 {
		t.Fatalf("effective relay port pool=%v", got)
	}
	got[0] = 14000
	if cfg.UDP.RelayPorts[0] != 12000 {
		t.Fatal("FixedRelayPorts returned mutable configuration storage")
	}

	for _, ports := range [][]int{{1}, {1023}, {65536}, {12000, 12000}} {
		cfg := validUDPServerConfig()
		cfg.UDP.RelayPorts = ports
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid relay port pool %v accepted", ports)
		}
	}

	cfg = validUDPServerConfig()
	cfg.UDP.RelayPort = 12000
	cfg.UDP.RelayPorts = []int{12007}
	if err := cfg.Validate(); err == nil {
		t.Fatal("relay_port and relay_ports were accepted together")
	}

	portsAtLimit := make([]int, MaxUDPRelayPortPoolSize)
	for i := range portsAtLimit {
		portsAtLimit[i] = 1024 + i
	}
	cfg = validUDPServerConfig()
	cfg.UDP.RelayPorts = portsAtLimit
	if err := cfg.Validate(); err != nil {
		t.Fatalf("relay port pool at hard limit rejected: %v", err)
	}

	cfg = validUDPServerConfig()
	cfg.UDP.RelayPorts = append(portsAtLimit, 1024+MaxUDPRelayPortPoolSize)
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "at most 4096") {
		t.Fatalf("oversized relay port pool error=%v, want explicit hard limit", err)
	}
}

func TestUDPMaxAssociationsValidation(t *testing.T) {
	for _, limit := range []int{-1, 65536} {
		cfg := validUDPServerConfig()
		cfg.UDP.MaxAssociations = limit
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid UDP association limit %d accepted", limit)
		}
	}
}

func TestUDPIdleTimeoutValidation(t *testing.T) {
	for _, timeout := range []Duration{Duration(-time.Second), Duration(500 * time.Millisecond), Duration(25 * time.Hour)} {
		cfg := validUDPServerConfig()
		cfg.UDP.IdleTimeout = timeout
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid UDP idle timeout %s accepted", time.Duration(timeout))
		}
	}
}

func TestUDPAdvertiseAddressFamilyValidation(t *testing.T) {
	cfg := validUDPServerConfig()
	cfg.UDP.Advertise = "::1"
	if err := cfg.Validate(); err == nil {
		t.Fatal("IPv6 advertise address accepted for IPv4 relay bind")
	}

	cfg = validUDPServerConfig()
	cfg.UDP.AdvertiseMap = map[string]string{"127.0.0.1": "2001:db8::1"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("cross-family UDP advertise mapping accepted")
	}
}

func TestUDPRejectsUnusableBindAndAdvertiseAddresses(t *testing.T) {
	for _, bindIP := range []string{"224.0.0.1", "ff02::1", "fe80::1"} {
		cfg := validUDPServerConfig()
		cfg.UDP.BindIP = bindIP
		if err := cfg.Validate(); err == nil {
			t.Fatalf("unusable UDP bind IP %q was accepted", bindIP)
		}
	}

	tests := []struct {
		bindIP    string
		advertise string
	}{
		{bindIP: "0.0.0.0", advertise: "0.0.0.0"},
		{bindIP: "0.0.0.0", advertise: "224.0.0.1"},
		{bindIP: "::", advertise: "::"},
		{bindIP: "::", advertise: "ff02::1"},
		{bindIP: "::", advertise: "fe80::1"},
	}
	for _, tt := range tests {
		cfg := validUDPServerConfig()
		cfg.UDP.BindIP = tt.bindIP
		cfg.UDP.Advertise = tt.advertise
		if err := cfg.Validate(); err == nil {
			t.Fatalf("unusable UDP advertise IP %q was accepted for bind %q", tt.advertise, tt.bindIP)
		}
	}
}

func TestUDPAdvertiseMapValidationAndNormalization(t *testing.T) {
	invalid := []struct {
		bindIP string
		local  string
		relay  string
	}{
		{bindIP: "0.0.0.0", local: "127.0.0.1", relay: "0.0.0.0"},
		{bindIP: "0.0.0.0", local: "127.0.0.1", relay: "224.0.0.1"},
		{bindIP: "::", local: "::1", relay: "fe80::1"},
		{bindIP: "0.0.0.0", local: "::1", relay: "::1"},
		{bindIP: "0.0.0.0", local: "127.0.0.1", relay: "::1"},
		{bindIP: "0.0.0.0", local: "0.0.0.0", relay: "127.0.0.1"},
		{bindIP: "0.0.0.0", local: "224.0.0.1", relay: "127.0.0.1"},
		{bindIP: "0.0.0.0", local: "255.255.255.255", relay: "127.0.0.1"},
	}
	for _, tt := range invalid {
		cfg := validUDPServerConfig()
		cfg.UDP.BindIP = tt.bindIP
		cfg.UDP.AdvertiseMap = map[string]string{tt.local: tt.relay}
		if err := cfg.Validate(); err == nil {
			t.Fatalf("invalid UDP advertise mapping %q -> %q was accepted for bind %q", tt.local, tt.relay, tt.bindIP)
		}
	}

	cfg := validUDPServerConfig()
	cfg.UDP.BindIP = "::"
	cfg.UDP.AdvertiseMap = map[string]string{
		"0:0:0:0:0:0:0:1": "2001:0db8:0:0:0:0:0:1",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid IPv6 advertise mapping rejected: %v", err)
	}
	if got := cfg.UDP.AdvertiseMap[net.ParseIP("::1").String()]; got != "2001:db8::1" || len(cfg.UDP.AdvertiseMap) != 1 {
		t.Fatalf("advertise map was not normalized: %+v", cfg.UDP.AdvertiseMap)
	}
	if cfg.UDP.BindIP != "::" {
		t.Fatalf("UDP bind IP was not normalized: %q", cfg.UDP.BindIP)
	}

	cfg = validUDPServerConfig()
	cfg.UDP.BindIP = "::"
	cfg.UDP.AdvertiseMap = map[string]string{
		"::1":             "2001:db8::1",
		"0:0:0:0:0:0:0:1": "2001:db8::2",
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("duplicate normalized UDP advertise map keys were accepted")
	}
}

func validUDPServerConfig() Server {
	return Server{
		Name:      "udp-test",
		Listen:    "127.0.0.1:1080",
		Interface: "lo",
		UDP: UDP{
			Enabled: true, BindIP: "127.0.0.1", Advertise: "auto",
			PortMin: 10000, PortMax: 65535,
		},
	}
}
