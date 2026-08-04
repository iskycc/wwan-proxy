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
