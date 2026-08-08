package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const maxUpdateDownloadBytes = 512 << 20

// DownloadGitHubFile is the constrained downloader used by stock OpenWrt when
// curl is unavailable. It accepts only GitHub HTTPS endpoints and publishes a
// new regular file; the installer still performs the release SHA-256 check.
func DownloadGitHubFile(ctx context.Context, rawURL, outputPath, downloadInterface string) error {
	if err := validateGitHubDownloadURL(rawURL); err != nil {
		return err
	}
	if !filepath.IsAbs(outputPath) {
		return errors.New("update download output path must be absolute")
	}
	if err := ValidateDownloadInterface(downloadInterface); err != nil {
		return err
	}
	baseClient := &http.Client{
		Timeout: 5 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many update download redirects")
			}
			return validateGitHubDownloadURL(req.URL.String())
		},
	}
	client, err := httpClientForInterface(baseClient, downloadInterface)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return fmt.Errorf("create update download request: %w", err)
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", "wwan-proxy-update-downloader")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("download update file: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("download update file: HTTP %d", response.StatusCode)
	}
	if response.ContentLength > maxUpdateDownloadBytes {
		return fmt.Errorf("update download exceeds %d bytes", maxUpdateDownloadBytes)
	}
	output, err := os.OpenFile(outputPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("create update download output: %w", err)
	}
	keep := false
	defer func() {
		_ = output.Close()
		if !keep {
			_ = os.Remove(outputPath)
		}
	}()
	written, err := io.Copy(output, io.LimitReader(response.Body, maxUpdateDownloadBytes+1))
	if err != nil {
		return fmt.Errorf("write update download: %w", err)
	}
	if written > maxUpdateDownloadBytes {
		return fmt.Errorf("update download exceeds %d bytes", maxUpdateDownloadBytes)
	}
	if err := output.Sync(); err != nil {
		return fmt.Errorf("sync update download: %w", err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close update download: %w", err)
	}
	keep = true
	return nil
}

func validateGitHubDownloadURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Host == "" {
		return errors.New("update downloader accepts only GitHub HTTPS URLs")
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "github.com" && host != "api.github.com" && !strings.HasSuffix(host, ".githubusercontent.com") {
		return fmt.Errorf("update downloader rejected host %q", host)
	}
	return nil
}
