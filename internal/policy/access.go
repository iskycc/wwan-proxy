package policy

import (
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"

	"wwan-proxy/internal/config"
)

var ErrTargetDenied = errors.New("target denied by access policy")

// Access is an immutable, compiled view of the source-admission and target
// authorization configuration. Target rules are evaluated in declaration
// order; the first matching rule wins.
type Access struct {
	admission    []*net.IPNet
	rules        []targetRule
	defaultAllow bool
}

type targetRule struct {
	allow   bool
	target  string
	ip      net.IP
	network *net.IPNet
	portMin int
	portMax int
}

func NewAccess(cfg config.AccessControl) (*Access, error) {
	p := &Access{defaultAllow: cfg.TargetDefault != "deny"}
	for _, value := range cfg.AdmissionCIDRs {
		_, network, err := net.ParseCIDR(strings.TrimSpace(value))
		if err != nil {
			return nil, fmt.Errorf("parse admission CIDR %q: %w", value, err)
		}
		p.admission = append(p.admission, network)
	}
	for _, value := range cfg.TargetRules {
		rule, err := config.ParseTargetACLRule(value)
		if err != nil {
			return nil, err
		}
		compiled := targetRule{
			allow: rule.Action == "allow", target: strings.ToLower(strings.TrimSuffix(rule.Target, ".")),
			portMin: rule.PortMin, portMax: rule.PortMax,
		}
		if ip := net.ParseIP(rule.Target); ip != nil {
			compiled.ip = ip
		} else if _, network, err := net.ParseCIDR(rule.Target); err == nil {
			compiled.network = network
		}
		p.rules = append(p.rules, compiled)
	}
	return p, nil
}

func (p *Access) AllowClient(addr net.Addr) bool {
	if p == nil || len(p.admission) == 0 {
		return true
	}
	host := addrStringHost(addr)
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, network := range p.admission {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// AllowTarget evaluates both the original requested host and a resolved IP.
// Passing a nil IP is appropriate for an IP literal only when host contains
// that literal; domain requests should be checked again for every resolved IP.
func (p *Access) AllowTarget(host string, ip net.IP, port int) bool {
	if p == nil {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip == nil {
		ip = net.ParseIP(host)
	}
	for _, rule := range p.rules {
		if !rule.matchesPort(port) || !rule.matchesTarget(host, ip) {
			continue
		}
		return rule.allow
	}
	return p.defaultAllow
}

// AllowTargetOnAnyPort reports whether at least one valid TCP/UDP port would
// be allowed for this host/IP. ACL decisions only change at configured range
// boundaries, so a finite boundary set exactly covers all 1..65535 ports.
func (p *Access) AllowTargetOnAnyPort(host string, ip net.IP) bool {
	if p == nil {
		return true
	}
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if ip == nil {
		ip = net.ParseIP(host)
	}
	candidates := map[int]struct{}{1: {}}
	for _, rule := range p.rules {
		if !rule.matchesTarget(host, ip) || rule.portMin == 0 {
			continue
		}
		candidates[rule.portMin] = struct{}{}
		if rule.portMax < 65535 {
			candidates[rule.portMax+1] = struct{}{}
		}
	}
	for port := range candidates {
		if p.AllowTarget(host, ip, port) {
			return true
		}
	}
	return false
}

func (r targetRule) matchesPort(port int) bool {
	return r.portMin == 0 || port >= r.portMin && port <= r.portMax
}

func (r targetRule) matchesTarget(host string, ip net.IP) bool {
	switch {
	case r.target == "*":
		return true
	case r.ip != nil:
		return ip != nil && r.ip.Equal(ip)
	case r.network != nil:
		return ip != nil && r.network.Contains(ip)
	case strings.HasPrefix(r.target, "*."):
		suffix := strings.TrimPrefix(r.target, "*")
		return strings.HasSuffix(host, suffix) && len(host) > len(suffix)
	default:
		return host == r.target
	}
}

func addrStringHost(addr net.Addr) string {
	if addr == nil {
		return ""
	}
	if host, _, err := net.SplitHostPort(addr.String()); err == nil {
		return host
	}
	return addr.String()
}

// IPLimiter provides a small in-process concurrent-session limit keyed by
// source IP. Acquire returns a release function on success.
type IPLimiter struct {
	max    int
	mu     sync.Mutex
	counts map[string]int
}

type Limiter struct {
	max    int
	mu     sync.Mutex
	active int
}

func NewLimiter(max int) *Limiter { return &Limiter{max: max} }

func (l *Limiter) Acquire() (func(), bool) {
	if l == nil {
		return func() {}, true
	}
	l.mu.Lock()
	if l.max > 0 && l.active >= l.max {
		l.mu.Unlock()
		return func() {}, false
	}
	l.active++
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			l.active--
			l.mu.Unlock()
		})
	}, true
}

// SetMax changes the admission ceiling without resetting the number of
// currently held slots. This lets listener generations share one hard budget
// while a hot reload drains established sessions.
func (l *Limiter) SetMax(max int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.max = max
	l.mu.Unlock()
}

func NewIPLimiter(max int) *IPLimiter {
	return &IPLimiter{max: max, counts: make(map[string]int)}
}

func (l *IPLimiter) Acquire(addr net.Addr) (func(), bool) {
	if l == nil {
		return func() {}, true
	}
	key := addrStringHost(addr)
	l.mu.Lock()
	if key == "" && l.max <= 0 {
		l.mu.Unlock()
		return func() {}, true
	}
	if key == "" {
		l.mu.Unlock()
		return func() {}, false
	}
	if l.max > 0 && l.counts[key] >= l.max {
		l.mu.Unlock()
		return func() {}, false
	}
	l.counts[key]++
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if l.counts[key] <= 1 {
				delete(l.counts, key)
			} else {
				l.counts[key]--
			}
			l.mu.Unlock()
		})
	}, true
}

// SetMax updates the per-key ceiling while retaining counts already acquired.
func (l *IPLimiter) SetMax(max int) {
	if l == nil {
		return
	}
	l.mu.Lock()
	l.max = max
	l.mu.Unlock()
}
