package webui

import (
	"strings"
	"testing"
)

func TestFrontendUsesWebSocketAndCoalescesRefresh(t *testing.T) {
	content, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(content)
	checks := []string{
		"if(refreshPromise)return refreshPromise",
		"new WebSocket(websocketURL())",
		"overviewSocket.send('refresh')",
		"reconnectTimer=setTimeout(()=>connectOverview(),delay)",
		"stopOverviewSocket()",
		"state.servers=overview.servers||[]",
		"button.disabled=true",
		"heap_live_bytes||state.overview.process.heap_bytes",
		"else if(state.page==='performance'){renderPerformance(a)}",
		"samplePerformance(overview)",
		"HISTORY_STORAGE_KEY='wwan-proxy.performance-history.v3'",
		"state.history.serviceID!==serviceID",
		"previous.generation!==latest.generation",
		"latest.up<previous.up||latest.down<previous.down",
		"sampledAt-state.history.lastAt",
		"if(elapsed<0){state.history.points=state.history.points.filter(point=>point.at<=sampledAt)",
		"if(elapsed===0)return",
		"patchHTML($('#runtime-grid')",
		"patchHTML(root,state.servers.map",
		"const staging=root.cloneNode(false)",
		"if(value===before)continue",
		"const updateVisiblePage=!overlayOpen()",
		"if(updateVisiblePage){render()",
		"renderAfterOverlayClose()",
		"window.addEventListener('pagehide',saveHistory)",
	}
	for _, check := range checks {
		if !strings.Contains(js, check) {
			t.Fatalf("frontend refresh guard %q is missing", check)
		}
	}
	for _, forbidden := range []string{"api('/api/overview')", "setInterval(refresh", "pollTimer"} {
		if strings.Contains(js, forbidden) {
			t.Fatalf("frontend must not poll overview data; found %q", forbidden)
		}
	}
	if strings.Contains(js, "api('/api/servers')") {
		t.Fatal("refresh must not duplicate server configuration already delivered over WebSocket")
	}
	for _, forbidden := range []string{"$('#runtime-grid').innerHTML", "const now=Date.now(),totalUp", "state.history.up.push", "if(state.page==='overview')['#kpi-active'"} {
		if strings.Contains(js, forbidden) {
			t.Fatalf("live performance rendering must retain history and DOM state; found %q", forbidden)
		}
	}
}

func TestFrontendHasLargeDisplayBreakpoints(t *testing.T) {
	content, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(content)
	for _, breakpoint := range []string{"@media(min-width:1600px)", "@media(min-width:2200px)", "@media(min-width:3000px)"} {
		if !strings.Contains(css, breakpoint) {
			t.Fatalf("large-display breakpoint %q is missing", breakpoint)
		}
	}
}

func TestFrontendExposesHTTPProxyConfigurationAndMetrics(t *testing.T) {
	jsContent, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	htmlContent, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, check := range []string{"http_proxy_enabled", "http_proxy_listen", "http_metrics", "HTTP Proxy"} {
		if !strings.Contains(string(jsContent)+string(htmlContent), check) {
			t.Fatalf("HTTP proxy frontend binding %q is missing", check)
		}
	}
}

func TestFrontendExposesIPv4OnlyDNSConfiguration(t *testing.T) {
	jsContent, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	htmlContent, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	content := string(jsContent) + string(htmlContent)
	for _, check := range []string{"dns_ipv4_only", "ipv4_only", "不请求 AAAA", "· IPv4"} {
		if !strings.Contains(content, check) {
			t.Fatalf("IPv4-only DNS frontend binding %q is missing", check)
		}
	}
}

func TestFrontendExposesAndSavesRuntimeLogLevel(t *testing.T) {
	jsContent, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	htmlContent, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, html := string(jsContent), string(htmlContent)
	for _, check := range []string{"f.elements.log_level.value=settings.log_level||'WARN'", "log_level:f.elements.log_level.value", "syncCustomSelects()"} {
		if !strings.Contains(js, check) {
			t.Fatalf("runtime log-level frontend binding %q is missing", check)
		}
	}
	for _, check := range []string{"name=\"log_level\"", "value=\"DEBUG\"", "value=\"INFO\"", "value=\"WARN\"", "value=\"ERROR\"", "控制台与 SQLite"} {
		if !strings.Contains(html, check) {
			t.Fatalf("runtime log-level setting %q is missing", check)
		}
	}
}

func TestFrontendExposesBindAccessControlAndInterfaceDiscovery(t *testing.T) {
	jsContent, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	htmlContent, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	content := string(jsContent) + string(htmlContent)
	for _, check := range []string{
		"bind_enabled", "bind:{enabled", "admission_cidrs", "target_default", "target_rules",
		"max_connections_per_ip", "max_udp_associations_per_ip", "/api/interfaces", "network-interfaces",
		"udp_relay_ports", "relay_ports", "relay_port", "udpRelayPortList", "物理 / 虚拟均可", "proxy-security-warning",
		"udp_max_associations", "max_associations", "http_proxy_listen.addEventListener('input',updateSecurityWarning)",
		"password_unchanged", "passwordUnchanged", "bind_advertise", "strict_endpoint", "udp_strict_endpoint",
		"ports.length>4096", "最多 4096 个",
	} {
		if !strings.Contains(content, check) {
			t.Fatalf("BIND/access/interface frontend binding %q is missing", check)
		}
	}
}

func TestFrontendUsesCustomSelectsAndReliableAuthVisibility(t *testing.T) {
	jsContent, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	cssContent, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	htmlContent, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, css, html := string(jsContent), string(cssContent), string(htmlContent)
	for _, check := range []string{"initCustomSelects()", "role','combobox", "aria-selected", "syncCustomSelects()"} {
		if !strings.Contains(js, check) {
			t.Fatalf("custom select behavior %q is missing", check)
		}
	}
	if strings.Count(html, "<select") != strings.Count(html, "<select class=\"native-select\"") {
		t.Fatal("every native select must be hidden and replaced by the custom component")
	}
	if !strings.Contains(css, ".hidden{display:none!important}") || !strings.Contains(css, ".custom-select-menu") {
		t.Fatal("overlay visibility or custom select styling is missing")
	}
	if strings.Contains(js, "confirm('") || !strings.Contains(js, "confirmAction(") || !strings.Contains(html, "id=\"confirm-modal\"") {
		t.Fatal("native browser confirmation must be replaced by the styled dialog")
	}
}

func TestFrontendEnforcesSessionExpiryAndBoundsCharts(t *testing.T) {
	jsContent, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	cssContent, err := assets.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	htmlContent, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	js, css, html := string(jsContent), string(cssContent), string(htmlContent)
	for _, check := range []string{"setSessionExpiry(status.expires_at)", "sessionExpiryTimer=setTimeout", "syncSessionExpiry()", "drawTimeSeriesChart", "niceChartMax", "step=range==='7d'?'hour':'minute'", "udpErrors", "UDP 丢弃 / 错误"} {
		if !strings.Contains(js, check) {
			t.Fatalf("session/chart behavior %q is missing", check)
		}
	}
	for _, check := range []string{"api(updateCheckPath())", "api('/api/update',{method:'POST'", "JSON.stringify({interface:route})", "syncUpdateInterfaces()", "rebuildCustomSelect(select)", "setCustomSelectDisabled($('#update-interface'),running)", "waitForUpdate()", "renderUpdate(update)"} {
		if !strings.Contains(js, check) {
			t.Fatalf("automatic update behavior %q is missing", check)
		}
	}
	for _, check := range []string{".chart-canvas-wrap", "max-width:100%", "overflow:hidden", ".settings-layout{align-items:start}"} {
		if !strings.Contains(css, check) {
			t.Fatalf("bounded chart/security layout %q is missing", check)
		}
	}
	for _, check := range []string{"class=\"chart-canvas-wrap", "stats-traffic-caption", "已过期设备会立即退出"} {
		if !strings.Contains(html, check) {
			t.Fatalf("chart/session UI %q is missing", check)
		}
	}
	for _, check := range []string{"id=\"check-update\"", "id=\"install-update\"", "id=\"update-current-version\"", "class=\"native-select\" id=\"update-interface\"", "系统默认路由"} {
		if !strings.Contains(html, check) {
			t.Fatalf("automatic update UI %q is missing", check)
		}
	}
	if !strings.Contains(css, ".update-version-grid") || !strings.Contains(css, ".update-progress") {
		t.Fatal("automatic update responsive styles are missing")
	}
}
