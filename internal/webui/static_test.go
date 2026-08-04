package webui

import (
	"strings"
	"testing"
)

func TestFrontendRefreshIsCoalesced(t *testing.T) {
	content, err := assets.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	js := string(content)
	checks := []string{
		"if(refreshPromise)return refreshPromise",
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
	if strings.Contains(js, "api('/api/servers')") {
		t.Fatal("refresh must not duplicate server configuration already returned by /api/overview")
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
