package manager

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"wwan-proxy/internal/config"
)

func TestHeartbeatTraceAndFailureDetails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ip=203.0.113.8\ncolo=SJC\nwarp=off\n"))
	}))
	defer server.Close()
	h := performHeartbeatAt(context.Background(), server.Client(), 7, server.URL)
	if !h.Healthy || h.PublicIP != "203.0.113.8" || h.Colo != "SJC" || h.ServerID != 7 {
		t.Fatalf("unexpected heartbeat %+v", h)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream down", http.StatusServiceUnavailable)
	}))
	defer failing.Close()
	h = performHeartbeatAt(context.Background(), failing.Client(), 7, failing.URL)
	if h.Healthy || h.StatusCode != http.StatusServiceUnavailable || !strings.HasPrefix(h.Error, "http_status:") {
		t.Fatalf("expected detailed failure %+v", h)
	}
}

func TestHeartbeatFailureIsClassifiedAndEndpointIsRedacted(t *testing.T) {
	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ENETUNREACH}
	}}
	defer transport.CloseIdleConnections()
	endpoint := "https://probe-user:probe-secret@example.test:8443/private-path-token/check?token=private#private-fragment"
	probe := executeHeartbeat(context.Background(), &http.Client{Transport: transport}, 9, endpoint, 200*time.Millisecond)
	if probe.heartbeat.Healthy || classifyHeartbeatFailure(probe.failureStage, probe.cause) != "network_unreachable" {
		t.Fatalf("unexpected probe result: %+v cause=%v", probe, probe.cause)
	}
	combined := probe.heartbeat.Error + strings.Join(heartbeatErrorChain(probe.cause, endpoint), " ") + sanitizeHeartbeatEndpoint(endpoint)
	for _, secret := range []string{"probe-user", "probe-secret", "private-path-token", "token=private", "private-fragment"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("heartbeat diagnostics leaked %q: %s", secret, combined)
		}
	}
	if sanitized := sanitizeHeartbeatEndpoint(endpoint); sanitized != "https://example.test:8443" {
		t.Fatalf("sanitized endpoint=%q", sanitized)
	}
	if target := heartbeatTargetHost(endpoint); target != "example.test" {
		t.Fatalf("target host=%q", target)
	}

	errorText := "request " + endpoint + " failed"
	withoutFragment := strings.TrimSuffix(endpoint, "#private-fragment")
	chain := heartbeatErrorChain(errors.Join(errors.New(errorText), errors.New("retry "+withoutFragment)), endpoint)
	combined = sanitizeHeartbeatErrorText(errorText, endpoint) + " " + strings.Join(chain, " ")
	if strings.Count(combined, "https://example.test:8443") < 3 {
		t.Fatalf("full endpoints were not replaced with the safe origin: %s", combined)
	}
	for _, secret := range []string{"probe-user", "probe-secret", "private-path-token", "token=private", "private-fragment"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("heartbeat error diagnostics leaked %q: %s", secret, combined)
		}
	}
}

func TestHeartbeatDoHEndpointsOnlyLogOrigin(t *testing.T) {
	endpoint := "https://doh-user:doh-password@resolver.example:4443/private-doh-token/dns-query?api_key=private#private-fragment"
	attrs := heartbeatDNSLogAttrs(config.Server{DNS: config.DNS{DoH: &config.DoH{URLs: []string{endpoint}}}})
	logged := make(map[string]any)
	for index := 0; index+1 < len(attrs); index += 2 {
		logged[attrs[index].(string)] = attrs[index+1]
	}
	endpoints, ok := logged["dns_endpoints"].([]string)
	if !ok || len(endpoints) != 1 || endpoints[0] != "https://resolver.example:4443" {
		t.Fatalf("unexpected DoH diagnostic endpoints: %#v", logged["dns_endpoints"])
	}
	combined := strings.Join(endpoints, " ")
	for _, secret := range []string{"doh-user", "doh-password", "private-doh-token", "api_key=private", "private-fragment"} {
		if strings.Contains(combined, secret) {
			t.Fatalf("DoH diagnostics leaked %q: %s", secret, combined)
		}
	}
}

func TestHeartbeatInterfaceDiagnosticCapturesLinkContext(t *testing.T) {
	diagnostic := collectInterfaceDiagnostic("lo")
	if diagnostic.LookupError != "" {
		t.Skipf("loopback interface unavailable: %s", diagnostic.LookupError)
	}
	if diagnostic.Index == 0 || diagnostic.MTU == 0 || len(diagnostic.Addresses) == 0 || diagnostic.Flags == "" {
		t.Fatalf("incomplete interface diagnostic: %+v", diagnostic)
	}
	attrs := diagnostic.logAttrs()
	if len(attrs) == 0 || diagnostic.signature() == "" {
		t.Fatal("interface diagnostic cannot be logged or compared")
	}
	logged := make(map[string]any)
	for index := 0; index+1 < len(attrs); index += 2 {
		logged[attrs[index].(string)] = attrs[index+1]
	}
	for _, required := range []string{"interface_index", "interface_mtu", "interface_flags", "interface_addresses", "interface_routes", "interface_operstate", "interface_link_stats"} {
		if _, exists := logged[required]; !exists {
			t.Fatalf("interface diagnostic is missing %q: %#v", required, logged)
		}
	}
}

func TestHeartbeatErrorChainPreservesJoinedCauses(t *testing.T) {
	err := errors.Join(syscall.ECONNREFUSED, context.DeadlineExceeded)
	chain := heartbeatErrorChain(err, "https://example.test/check")
	joined := strings.Join(chain, " ")
	if !strings.Contains(joined, "connection refused") || !strings.Contains(joined, "deadline exceeded") {
		t.Fatalf("error chain=%v", chain)
	}
}

func TestHeartbeatFailureClassifications(t *testing.T) {
	tests := []struct {
		stage string
		err   error
		want  string
	}{
		{stage: "dns", err: &net.DNSError{Err: "temporary lookup timeout", IsTimeout: true}, want: "dns_timeout"},
		{stage: "response_headers", err: context.DeadlineExceeded, want: "response_headers_timeout"},
		{stage: "http_status", err: errors.New("unexpected HTTP status 503 Service Unavailable"), want: "http_status_failure"},
	}
	for _, test := range tests {
		if got := classifyHeartbeatFailure(test.stage, test.err); got != test.want {
			t.Errorf("classify(%q, %v)=%q want %q", test.stage, test.err, got, test.want)
		}
	}
	endpoint := "https://diagnostic-user:diagnostic-secret@example.test/check?token=hidden"
	dnsError := &net.DNSError{Err: "temporary lookup timeout", Name: "example.test", IsTimeout: true}
	formatted := formatHeartbeatError("dns", dnsError, endpoint)
	if classifyHeartbeatFailure("dns", dnsError) != "dns_timeout" {
		t.Fatal("DNS timeout was not classified as dns_timeout")
	}
	for _, secret := range []string{"diagnostic-user", "diagnostic-secret", "token=hidden"} {
		if strings.Contains(formatted, secret) {
			t.Fatalf("DNS timeout diagnostic leaked %q: %s", secret, formatted)
		}
	}
}

func TestHeartbeatDNSFailureOverridesNestedResolverTraceStage(t *testing.T) {
	state := newHeartbeatTraceState()
	trace := state.clientTrace()
	trace.DNSStart(httptrace.DNSStartInfo{Host: "target.example"})
	trace.ConnectStart("tcp", "192.0.2.53:443")
	trace.TLSHandshakeStart()
	trace.TLSHandshakeDone(tls.ConnectionState{}, errors.New("nested DoH TLS handshake failed"))
	trace.DNSDone(httptrace.DNSDoneInfo{Err: &net.DNSError{Err: "DoH lookup failed", Name: "target.example"}})

	if stage := state.stage(); stage != "dns" {
		t.Fatalf("DNS failure stage=%q, want dns", stage)
	}
	snapshot := state.snapshot()
	if snapshot.DNSError == "" || snapshot.TLSError == "" || len(snapshot.ConnectAttempts) == 0 {
		t.Fatalf("nested resolver evidence was lost: %+v", snapshot)
	}
}

func TestHeartbeatDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	defer target.Close()
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer endpoint.Close()
	client := endpoint.Client()
	client.CheckRedirect = rejectHeartbeatRedirect
	h := performHeartbeatAt(context.Background(), client, 1, endpoint.URL)
	if h.Healthy || h.StatusCode != http.StatusFound || redirected.Load() != 0 {
		t.Fatalf("redirecting heartbeat=%+v redirected_requests=%d", h, redirected.Load())
	}
}
