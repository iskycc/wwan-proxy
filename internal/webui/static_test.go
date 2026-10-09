package webui

import (
	"io/fs"
	"regexp"
	"strings"
	"testing"
)

func frontendAssetPaths(t *testing.T) []string {
	t.Helper()
	body, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	matches := regexp.MustCompile("(?i)<(?:script|link)[^>]*(?:src|href)=\"([^\"]+)\"").FindAllStringSubmatch(string(body), -1)
	var paths []string
	for _, match := range matches {
		paths = append(paths, match[1])
	}
	return paths
}

func frontendScriptPath(t *testing.T) string {
	t.Helper()
	for _, path := range frontendAssetPaths(t) {
		if strings.HasSuffix(path, ".js") {
			return path
		}
	}
	t.Fatal("frontend build does not reference a JavaScript entry")
	return ""
}

func TestFrontendBuildIsEmbeddedAndSelfContained(t *testing.T) {
	body, err := assets.ReadFile("static/index.html")
	if err != nil {
		t.Fatal(err)
	}
	html := string(body)
	if !strings.Contains(html, "id=\"root\"") || !strings.Contains(html, "lang=\"zh-CN\"") {
		t.Fatal("frontend entry is missing its application root or locale")
	}
	paths := frontendAssetPaths(t)
	if len(paths) == 0 {
		t.Fatal("frontend build has no assets; run make frontend")
	}
	for _, path := range paths {
		if !strings.HasPrefix(path, "/") || strings.HasPrefix(path, "//") {
			t.Fatalf("frontend must use bundled local assets: %s", path)
		}
		content, err := assets.ReadFile("static" + path)
		if err != nil || len(content) == 0 {
			t.Fatalf("frontend asset is not embedded: %s: %v", path, err)
		}
	}
	// Include lazily loaded page chunks in the deployed binary. Browser
	// behavior is covered by the regression suite in web/tests.
	var scripts int
	err = fs.WalkDir(assets, "static", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && strings.HasSuffix(path, ".js") {
			scripts++
		}
		return nil
	})
	if err != nil || scripts < 2 {
		t.Fatalf("frontend chunks are missing: count=%d err=%v", scripts, err)
	}
}
