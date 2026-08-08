package selfupdate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCheckerFindsVerifiedReleaseAssets(t *testing.T) {
	assetName, supported := releaseAssetName(runtime.GOARCH)
	if !supported {
		t.Skip("test runner architecture has no release asset")
	}
	published := time.Date(2026, time.August, 8, 10, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/iskycc/wwan-proxy/releases/latest" {
			t.Fatalf("unexpected request path %q", r.URL.Path)
		}
		if r.Header.Get("Accept") != "application/vnd.github+json" || !strings.HasPrefix(r.Header.Get("User-Agent"), "wwan-proxy/") {
			t.Fatalf("missing GitHub API headers: %+v", r.Header)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":     "build-bbbbbbbbbbbb",
			"html_url":     "https://github.com/iskycc/wwan-proxy/releases/tag/build-bbbbbbbbbbbb",
			"published_at": published.Format(time.RFC3339),
			"assets":       []map[string]string{{"name": assetName}, {"name": "SHA256SUMS"}},
		})
	}))
	defer server.Close()

	checker, err := newChecker("iskycc/wwan-proxy", "aaaaaaaaaaaa", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Checked || !info.UpdateAvailable || info.Latest == nil {
		t.Fatalf("unexpected update info: %+v", info)
	}
	if info.Latest.Tag != "build-bbbbbbbbbbbb" || info.Latest.Version != "bbbbbbbbbbbb" || info.Latest.AssetName != assetName || !info.Latest.PublishedAt.Equal(published) {
		t.Fatalf("unexpected release: %+v", info.Latest)
	}
}

func TestCheckerTreatsBuildTagAsCurrentVersion(t *testing.T) {
	assetName, supported := releaseAssetName(runtime.GOARCH)
	if !supported {
		t.Skip("test runner architecture has no release asset")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"tag_name":"build-abcdef123456","html_url":"https://example.invalid/release","published_at":"2026-08-08T10:00:00Z","assets":[{"name":"` + assetName + `"},{"name":"SHA256SUMS"}]}`))
	}))
	defer server.Close()
	checker, err := newChecker("iskycc/wwan-proxy", "abcdef123456", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	info, err := checker.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !info.Checked || info.UpdateAvailable {
		t.Fatalf("same build was reported as an update: %+v", info)
	}
}

func TestCheckerRejectsIncompleteAndUnsafeReleaseMetadata(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "missing checksum", body: `{"tag_name":"build-abcdef123456","published_at":"2026-08-08T10:00:00Z","assets":[]}`, want: "SHA256SUMS"},
		{name: "unsafe tag", body: `{"tag_name":"../../bad","published_at":"2026-08-08T10:00:00Z","assets":[]}`, want: "invalid release tag"},
		{name: "prerelease", body: `{"tag_name":"build-abcdef123456","published_at":"2026-08-08T10:00:00Z","prerelease":true,"assets":[]}`, want: "not a stable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(test.body)) }))
			defer server.Close()
			checker, err := newChecker("iskycc/wwan-proxy", "aaaaaaaaaaaa", server.URL, server.Client())
			if err != nil {
				t.Fatal(err)
			}
			_, err = checker.Check(context.Background())
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestRepositoryAndDevelopmentVersionValidation(t *testing.T) {
	for _, repository := range []string{"missing-slash", "owner/repo/extra", "owner/../repo", "owner/repo?bad"} {
		if _, err := NewChecker(repository, "abcdef123456"); err == nil {
			t.Fatalf("repository %q was accepted", repository)
		}
	}
	checker, err := NewChecker(DefaultRepository, "dev")
	if err != nil {
		t.Fatal(err)
	}
	info := checker.LocalInfo()
	if !info.DevelopmentBuild || info.InstallSupported || info.InstallMessage == "" {
		t.Fatalf("development build should not be automatically updatable: %+v", info)
	}
}
