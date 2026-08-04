package manager

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"wwan-proxy/internal/config"
)

// interfaceDiagnostic is deliberately a bounded snapshot. Heartbeat failures
// are often caused by link state, address loss, policy routing, or modem/driver
// packet errors; recording these values at failure time makes the event useful
// even after the interface has recovered.
type interfaceDiagnostic struct {
	Index       int
	MTU         int
	Flags       string
	Up          bool
	Running     bool
	Addresses   []string
	OperState   string
	Carrier     string
	LinkStats   map[string]string
	Routes      []string
	LookupError string
	AddrError   string
	RouteErrors []string
}

func collectInterfaceDiagnostic(name string) interfaceDiagnostic {
	var result interfaceDiagnostic
	iface, err := net.InterfaceByName(name)
	if err != nil {
		result.LookupError = err.Error()
		return result
	}
	result.Index = iface.Index
	result.MTU = iface.MTU
	result.Flags = iface.Flags.String()
	result.Up = iface.Flags&net.FlagUp != 0
	result.Running = iface.Flags&net.FlagRunning != 0
	if addresses, addrErr := iface.Addrs(); addrErr != nil {
		result.AddrError = addrErr.Error()
	} else {
		for _, address := range addresses {
			result.Addresses = appendUniqueLimited(result.Addresses, address.String(), 32)
		}
		sort.Strings(result.Addresses)
	}

	interfacePath := filepath.Join("/sys/class/net", name)
	result.OperState = readTrimmedFile(filepath.Join(interfacePath, "operstate"))
	result.Carrier = readTrimmedFile(filepath.Join(interfacePath, "carrier"))
	result.LinkStats = make(map[string]string)
	for _, counter := range []string{"rx_errors", "tx_errors", "rx_dropped", "tx_dropped"} {
		if value := readTrimmedFile(filepath.Join(interfacePath, "statistics", counter)); value != "" {
			result.LinkStats[counter] = value
		}
	}
	if len(result.LinkStats) == 0 {
		result.LinkStats = nil
	}
	result.Routes, result.RouteErrors = kernelRoutesForInterface(name)
	return result
}

func (d interfaceDiagnostic) signature() string {
	// Packet counters are captured in the event but intentionally excluded from
	// the deduplication key: monotonically increasing drops must not turn one
	// persistent outage into a new ERROR every heartbeat interval.
	return fmt.Sprintf("%d|%d|%s|%t|%t|%v|%s|%s|%v|%s|%s|%v",
		d.Index, d.MTU, d.Flags, d.Up, d.Running, d.Addresses, d.OperState, d.Carrier,
		d.Routes, d.LookupError, d.AddrError, d.RouteErrors)
}

func (d interfaceDiagnostic) logAttrs() []any {
	return []any{
		"interface_index", d.Index,
		"interface_mtu", d.MTU,
		"interface_flags", d.Flags,
		"interface_up", d.Up,
		"interface_running", d.Running,
		"interface_operstate", d.OperState,
		"interface_carrier", d.Carrier,
		"interface_addresses", d.Addresses,
		"interface_link_stats", d.LinkStats,
		"interface_routes", d.Routes,
		"interface_lookup_error", d.LookupError,
		"interface_address_error", d.AddrError,
		"interface_route_errors", d.RouteErrors,
	}
}

func readTrimmedFile(path string) string {
	contents, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(contents))
}

func kernelRoutesForInterface(name string) ([]string, []string) {
	routes := make([]string, 0, 8)
	var readErrors []string
	if values, err := ipv4RoutesForInterface(name); err != nil {
		readErrors = append(readErrors, "ipv4: "+err.Error())
	} else {
		routes = append(routes, values...)
	}
	if values, err := ipv6RoutesForInterface(name); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			readErrors = append(readErrors, "ipv6: "+err.Error())
		}
	} else {
		routes = append(routes, values...)
	}
	sort.Strings(routes)
	if len(routes) > 16 {
		omitted := len(routes) - 16
		routes = append(routes[:16], fmt.Sprintf("... %d additional routes omitted", omitted))
	}
	return routes, readErrors
}

func ipv4RoutesForInterface(name string) ([]string, error) {
	file, err := os.Open("/proc/net/route")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var routes []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 8 || fields[0] != name || fields[1] == "Destination" {
			continue
		}
		destination, destinationErr := decodeProcIPv4(fields[1])
		gateway, gatewayErr := decodeProcIPv4(fields[2])
		mask, maskErr := decodeProcIPv4(fields[7])
		if destinationErr != nil || gatewayErr != nil || maskErr != nil {
			continue
		}
		ones, _ := net.IPMask(mask.To4()).Size()
		via := "dev"
		if !gateway.Equal(net.IPv4zero) {
			via = "via " + gateway.String()
		}
		routes = append(routes, fmt.Sprintf("ipv4 %s/%d %s metric %s flags 0x%s", destination, ones, via, fields[6], fields[3]))
	}
	return routes, scanner.Err()
}

func decodeProcIPv4(value string) (net.IP, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != net.IPv4len {
		return nil, fmt.Errorf("invalid IPv4 route value %q", value)
	}
	return net.IPv4(decoded[3], decoded[2], decoded[1], decoded[0]), nil
}

func ipv6RoutesForInterface(name string) ([]string, error) {
	file, err := os.Open("/proc/net/ipv6_route")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var routes []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 10 || fields[9] != name {
			continue
		}
		destination, destinationErr := decodeProcIPv6(fields[0])
		nextHop, nextHopErr := decodeProcIPv6(fields[4])
		prefix, prefixErr := strconv.ParseUint(fields[1], 16, 8)
		metric, metricErr := strconv.ParseUint(fields[5], 16, 32)
		if destinationErr != nil || nextHopErr != nil || prefixErr != nil || metricErr != nil {
			continue
		}
		via := "dev"
		if !nextHop.IsUnspecified() {
			via = "via " + nextHop.String()
		}
		routes = append(routes, fmt.Sprintf("ipv6 %s/%d %s metric %d flags 0x%s", destination, prefix, via, metric, fields[8]))
	}
	return routes, scanner.Err()
}

func decodeProcIPv6(value string) (net.IP, error) {
	decoded, err := hex.DecodeString(value)
	if err != nil || len(decoded) != net.IPv6len {
		return nil, fmt.Errorf("invalid IPv6 route value %q", value)
	}
	return net.IP(decoded), nil
}

func heartbeatDNSLogAttrs(cfg config.Server) []any {
	mode := "system"
	var endpoints []string
	if cfg.DNS.DoH != nil {
		mode = "doh"
		for _, endpoint := range cfg.DNS.DoH.Endpoints() {
			endpoints = append(endpoints, sanitizeHeartbeatEndpoint(endpoint))
		}
	} else if len(cfg.DNS.Servers) > 0 {
		mode = "custom"
		endpoints = append(endpoints, cfg.DNS.Servers...)
	} else {
		endpoints = systemNameServers()
	}
	return []any{"dns_mode", mode, "dns_ipv4_only", cfg.DNS.IPv4Only, "dns_endpoints", endpoints}
}

func systemNameServers() []string {
	file, err := os.Open("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	defer file.Close()
	var servers []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 2 && fields[0] == "nameserver" {
			servers = appendUniqueLimited(servers, fields[1], 8)
		}
	}
	return servers
}

func sanitizeHeartbeatEndpoint(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "<invalid heartbeat endpoint>"
	}
	// Paths can contain API keys just as readily as userinfo or query strings
	// (for example, /dns-query/<token>). Diagnostics only need the network
	// origin to identify the target, so never retain any URL component beyond
	// scheme and host[:port].
	return (&url.URL{Scheme: parsed.Scheme, Host: parsed.Host}).String()
}

func heartbeatTargetHost(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return parsed.Hostname()
}

func formatHeartbeatError(stage string, err error, endpoint string) string {
	if stage == "" {
		stage = "unknown"
	}
	message := sanitizeHeartbeatErrorText(err.Error(), endpoint)
	return stage + ": " + message
}

func heartbeatErrorChain(err error, endpoint string) []string {
	if err == nil {
		return nil
	}
	result := make([]string, 0, 4)
	seen := make(map[string]struct{})
	var walk func(error)
	walk = func(current error) {
		if current == nil || len(result) >= 12 {
			return
		}
		if requestErr, ok := current.(*url.Error); ok {
			item := fmt.Sprintf("%T: HTTP %s", current, requestErr.Op)
			if _, exists := seen[item]; !exists {
				seen[item] = struct{}{}
				result = append(result, item)
			}
			walk(requestErr.Err)
			return
		}
		item := fmt.Sprintf("%T: %s", current, sanitizeHeartbeatErrorText(current.Error(), endpoint))
		if _, exists := seen[item]; !exists {
			seen[item] = struct{}{}
			result = append(result, item)
		}
		if joined, ok := current.(interface{ Unwrap() []error }); ok {
			for _, nested := range joined.Unwrap() {
				walk(nested)
			}
			return
		}
		if wrapped, ok := current.(interface{ Unwrap() error }); ok {
			walk(wrapped.Unwrap())
		}
	}
	walk(err)
	return result
}

func sanitizeHeartbeatErrorText(message, endpoint string) string {
	safeEndpoint := sanitizeHeartbeatEndpoint(endpoint)
	if endpoint == "" {
		return message
	}

	// net/http and wrapped errors do not always render an URL identically: a
	// fragment may be dropped and url.URL.String can normalize escaping. Cover
	// those full-URL forms before removing userinfo on its own. Longest-first
	// prevents an origin+path variant from leaving a query or fragment behind.
	variants := []string{endpoint}
	if parsed, err := url.Parse(endpoint); err == nil {
		variants = append(variants, parsed.String())
		withoutFragment := *parsed
		withoutFragment.Fragment = ""
		variants = append(variants, withoutFragment.String())
		withoutQuery := withoutFragment
		withoutQuery.RawQuery = ""
		withoutQuery.ForceQuery = false
		variants = append(variants, withoutQuery.String())
	}
	sort.Slice(variants, func(i, j int) bool { return len(variants[i]) > len(variants[j]) })
	for _, variant := range variants {
		if variant != "" {
			message = strings.ReplaceAll(message, variant, safeEndpoint)
		}
	}
	if parsed, err := url.Parse(endpoint); err == nil && parsed.User != nil {
		message = strings.ReplaceAll(message, parsed.User.String()+"@", "")
	}
	return message
}

func sanitizeHeartbeatErrorTexts(messages []string, endpoint string) []string {
	if len(messages) == 0 {
		return nil
	}
	sanitized := make([]string, len(messages))
	for index, message := range messages {
		sanitized[index] = sanitizeHeartbeatErrorText(message, endpoint)
	}
	return sanitized
}

func classifyHeartbeatFailure(stage string, err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return stage + "_timeout"
	}
	var networkError net.Error
	if errors.As(err, &networkError) && networkError.Timeout() {
		return stage + "_timeout"
	}
	switch {
	case errors.Is(err, syscall.ENETUNREACH):
		return "network_unreachable"
	case errors.Is(err, syscall.EHOSTUNREACH):
		return "host_unreachable"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection_refused"
	case errors.Is(err, os.ErrPermission):
		return "permission_denied"
	}
	switch stage {
	case "dns":
		return "dns_failure"
	case "tcp_connect":
		return "connect_failure"
	case "tls_handshake":
		return "tls_failure"
	case "request_write":
		return "request_write_failure"
	case "response_headers":
		return "response_header_failure"
	case "response_body":
		return "response_body_failure"
	case "http_status":
		return "http_status_failure"
	default:
		return "heartbeat_failure"
	}
}
