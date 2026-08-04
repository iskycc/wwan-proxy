package policy

import (
	"net"
	"testing"

	"wwan-proxy/internal/config"
)

func TestAccessAdmissionAndOrderedTargetRules(t *testing.T) {
	p, err := NewAccess(config.AccessControl{
		AdmissionCIDRs: []string{"192.0.2.0/24", "2001:db8::/32"},
		TargetDefault:  "deny",
		TargetRules: []string{
			"deny 10.0.0.0/8",
			"allow *.example.com:443",
			"allow [2001:db8:1::/48]:53",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !p.AllowClient(&net.TCPAddr{IP: net.ParseIP("192.0.2.5"), Port: 1234}) ||
		p.AllowClient(&net.TCPAddr{IP: net.ParseIP("198.51.100.5"), Port: 1234}) {
		t.Fatal("unexpected admission decision")
	}
	if !p.AllowTarget("api.example.com", net.ParseIP("203.0.113.8"), 443) {
		t.Fatal("expected hostname allow")
	}
	if p.AllowTarget("api.example.com", net.ParseIP("10.1.2.3"), 443) {
		t.Fatal("earlier CIDR deny must win over hostname allow")
	}
	if p.AllowTarget("example.com", net.ParseIP("203.0.113.8"), 443) {
		t.Fatal("wildcard must not match the zone apex")
	}
}

func TestIPLimiter(t *testing.T) {
	limiter := NewIPLimiter(1)
	addr := &net.TCPAddr{IP: net.ParseIP("192.0.2.1"), Port: 1}
	release, ok := limiter.Acquire(addr)
	if !ok {
		t.Fatal("first acquire failed")
	}
	if _, ok := limiter.Acquire(&net.TCPAddr{IP: addr.IP, Port: 2}); ok {
		t.Fatal("second acquire for same IP succeeded")
	}
	release()
	if _, ok := limiter.Acquire(addr); !ok {
		t.Fatal("acquire after release failed")
	}
}

func TestLimiterReleaseIsIdempotent(t *testing.T) {
	limiter := NewLimiter(1)
	release, ok := limiter.Acquire()
	if !ok {
		t.Fatal("first acquire failed")
	}
	if _, ok := limiter.Acquire(); ok {
		t.Fatal("limit was not enforced")
	}
	release()
	release()
	if _, ok := limiter.Acquire(); !ok {
		t.Fatal("capacity was not restored exactly once")
	}
}

func TestLimiterSetMaxRetainsActiveSlots(t *testing.T) {
	limiter := NewLimiter(2)
	release, ok := limiter.Acquire()
	if !ok {
		t.Fatal("initial acquire failed")
	}
	limiter.SetMax(1)
	if _, ok := limiter.Acquire(); ok {
		t.Fatal("updated limit ignored an active slot")
	}
	release()
	if _, ok := limiter.Acquire(); !ok {
		t.Fatal("slot did not become available after release")
	}
}

func TestUnlimitedLimitersTrackForLaterFiniteLimit(t *testing.T) {
	limiter := NewLimiter(0)
	release, ok := limiter.Acquire()
	if !ok {
		t.Fatal("unlimited acquire failed")
	}
	limiter.SetMax(1)
	if _, ok := limiter.Acquire(); ok {
		t.Fatal("unlimited active slot was forgotten when limit became finite")
	}
	release()

	ipLimiter := NewIPLimiter(0)
	addr := &net.TCPAddr{IP: net.ParseIP("192.0.2.20"), Port: 1}
	releaseIP, ok := ipLimiter.Acquire(addr)
	if !ok {
		t.Fatal("unlimited per-IP acquire failed")
	}
	ipLimiter.SetMax(1)
	if _, ok := ipLimiter.Acquire(&net.TCPAddr{IP: addr.IP, Port: 2}); ok {
		t.Fatal("unlimited per-IP slot was forgotten when limit became finite")
	}
	releaseIP()
}

func TestAllowTargetOnAnyPortRespectsOrderedRanges(t *testing.T) {
	p, err := NewAccess(config.AccessControl{TargetDefault: "deny", TargetRules: []string{
		"deny 192.0.2.0/24:1-1024",
		"allow 192.0.2.0/24:1025-2048",
		"allow [2001:db8::/32]:443",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !p.AllowTargetOnAnyPort("peer.example", net.ParseIP("192.0.2.10")) ||
		!p.AllowTargetOnAnyPort("peer.example", net.ParseIP("2001:db8::10")) ||
		p.AllowTargetOnAnyPort("peer.example", net.ParseIP("198.51.100.10")) {
		t.Fatal("any-port ACL evaluation returned an invalid decision")
	}
	denyAll, err := NewAccess(config.AccessControl{TargetDefault: "allow", TargetRules: []string{"deny *:1-65535"}})
	if err != nil {
		t.Fatal(err)
	}
	if denyAll.AllowTargetOnAnyPort("peer.example", net.ParseIP("192.0.2.1")) {
		t.Fatal("fully covered deny range left an allowed port")
	}
}
