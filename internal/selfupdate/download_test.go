package selfupdate

import "testing"

func TestValidateGitHubDownloadURL(t *testing.T) {
	for _, value := range []string{
		"https://api.github.com/repos/iskycc/wwan-proxy/releases/latest",
		"https://github.com/iskycc/wwan-proxy/releases/download/build-test/file.tar.gz",
		"https://release-assets.githubusercontent.com/example",
	} {
		if err := validateGitHubDownloadURL(value); err != nil {
			t.Fatalf("valid GitHub URL %q rejected: %v", value, err)
		}
	}
	for _, value := range []string{
		"http://github.com/file",
		"https://github.com.evil.example/file",
		"https://user:pass@github.com/file",
		"file:///etc/passwd",
	} {
		if err := validateGitHubDownloadURL(value); err == nil {
			t.Fatalf("unsafe update URL %q was accepted", value)
		}
	}
}
