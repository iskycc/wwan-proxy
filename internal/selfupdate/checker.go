package selfupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"
)

const (
	DefaultRepository = "iskycc/wwan-proxy"
	defaultAPIBase    = "https://api.github.com"
	maxMetadataBytes  = 1 << 20
)

var (
	ErrNoUpdate          = errors.New("no update is available")
	ErrUpdateUnsupported = errors.New("automatic update is unavailable on this installation")
	ErrUpdateInProgress  = errors.New("an update is already in progress")
)

type Release struct {
	Tag         string    `json:"tag"`
	Version     string    `json:"version"`
	URL         string    `json:"url"`
	PublishedAt time.Time `json:"published_at"`
	AssetName   string    `json:"asset_name"`
}

type Info struct {
	CurrentVersion   string           `json:"current_version"`
	Platform         string           `json:"platform"`
	Architecture     string           `json:"architecture"`
	DevelopmentBuild bool             `json:"development_build"`
	Checked          bool             `json:"checked"`
	UpdateAvailable  bool             `json:"update_available"`
	InstallSupported bool             `json:"install_supported"`
	InstallMessage   string           `json:"install_message,omitempty"`
	Latest           *Release         `json:"latest,omitempty"`
	Operation        *OperationStatus `json:"operation,omitempty"`
}

type Checker struct {
	repository     string
	currentVersion string
	apiBase        string
	client         *http.Client
}

func NewChecker(repository, currentVersion string) (*Checker, error) {
	return newChecker(repository, currentVersion, defaultAPIBase, &http.Client{Timeout: 20 * time.Second})
}

func newChecker(repository, currentVersion, apiBase string, client *http.Client) (*Checker, error) {
	if err := validateRepository(repository); err != nil {
		return nil, err
	}
	if client == nil {
		return nil, errors.New("update HTTP client is required")
	}
	return &Checker{repository: repository, currentVersion: strings.TrimSpace(currentVersion), apiBase: strings.TrimRight(apiBase, "/"), client: client}, nil
}

func (c *Checker) LocalInfo() Info {
	platform := DetectPlatform()
	_, assetSupported := releaseAssetName(runtime.GOARCH)
	development := isDevelopmentVersion(c.currentVersion)
	info := Info{
		CurrentVersion:   c.currentVersion,
		Platform:         platform,
		Architecture:     runtime.GOARCH,
		DevelopmentBuild: development,
		InstallSupported: assetSupported && isSupportedPlatform(platform) && !development,
	}
	if development {
		info.InstallMessage = "开发构建没有可比较的 Release 版本，请先通过安装脚本安装正式构建"
	} else if !assetSupported {
		info.InstallMessage = "当前 CPU 架构没有可用的 Release 安装包"
	} else if !isSupportedPlatform(platform) {
		info.InstallMessage = "当前系统不在 Web 自动更新支持范围内"
	}
	return info
}

func (c *Checker) Check(ctx context.Context, downloadInterface ...string) (Info, error) {
	info := c.LocalInfo()
	assetName, ok := releaseAssetName(runtime.GOARCH)
	if !ok {
		return info, nil
	}

	endpoint := fmt.Sprintf("%s/repos/%s/releases/latest", c.apiBase, c.repository)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return info, fmt.Errorf("create release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "wwan-proxy/"+displayVersion(c.currentVersion))
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	interfaceName := ""
	if len(downloadInterface) > 0 {
		interfaceName = downloadInterface[0]
	}
	client, err := c.clientForInterface(interfaceName)
	if err != nil {
		return info, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return info, fmt.Errorf("query latest GitHub release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return info, fmt.Errorf("query latest GitHub release: HTTP %d", resp.StatusCode)
	}
	var payload struct {
		TagName     string `json:"tag_name"`
		HTMLURL     string `json:"html_url"`
		PublishedAt string `json:"published_at"`
		Draft       bool   `json:"draft"`
		Prerelease  bool   `json:"prerelease"`
		Assets      []struct {
			Name string `json:"name"`
		} `json:"assets"`
	}
	dec := json.NewDecoder(io.LimitReader(resp.Body, maxMetadataBytes))
	if err := dec.Decode(&payload); err != nil {
		return info, fmt.Errorf("decode latest GitHub release: %w", err)
	}
	if payload.Draft || payload.Prerelease {
		return info, errors.New("GitHub latest release is not a stable published release")
	}
	if !safeReleaseTag(payload.TagName) {
		return info, fmt.Errorf("GitHub returned an invalid release tag %q", payload.TagName)
	}
	hasAsset, hasChecksums := false, false
	for _, asset := range payload.Assets {
		switch asset.Name {
		case assetName:
			hasAsset = true
		case "SHA256SUMS":
			hasChecksums = true
		}
	}
	if !hasAsset || !hasChecksums {
		return info, fmt.Errorf("latest release does not contain %s and SHA256SUMS", assetName)
	}
	publishedAt, err := time.Parse(time.RFC3339, payload.PublishedAt)
	if err != nil {
		return info, fmt.Errorf("parse latest release time: %w", err)
	}
	latestVersion := strings.TrimPrefix(payload.TagName, "build-")
	info.Checked = true
	info.Latest = &Release{Tag: payload.TagName, Version: latestVersion, URL: payload.HTMLURL, PublishedAt: publishedAt, AssetName: assetName}
	info.UpdateAvailable = !info.DevelopmentBuild && normalizeVersion(c.currentVersion) != normalizeVersion(latestVersion)
	return info, nil
}

func (c *Checker) clientForInterface(interfaceName string) (*http.Client, error) {
	return httpClientForInterface(c.client, interfaceName)
}

func httpClientForInterface(baseClient *http.Client, interfaceName string) (*http.Client, error) {
	if interfaceName == "" {
		return baseClient, nil
	}
	if err := ValidateDownloadInterface(interfaceName); err != nil {
		return nil, err
	}
	baseTransport := baseClient.Transport
	if baseTransport == nil {
		baseTransport = http.DefaultTransport
	}
	transport, ok := baseTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("update HTTP transport cannot bind a download interface")
	}
	boundTransport := transport.Clone()
	dialer := &net.Dialer{Timeout: 20 * time.Second, KeepAlive: 30 * time.Second, Control: bindToDownloadInterface(interfaceName)}
	boundTransport.DialContext = dialer.DialContext
	client := *baseClient
	client.Transport = boundTransport
	return &client, nil
}

func DetectPlatform() string {
	if _, err := os.Stat("/etc/openwrt_release"); err == nil {
		return "openwrt"
	}
	if _, err := os.Stat("/etc/openwrt_version"); err == nil {
		return "openwrt"
	}
	if _, err := os.Stat("/etc/alpine-release"); err == nil {
		return "alpine"
	}
	data, err := os.ReadFile("/etc/os-release")
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			key, value, found := strings.Cut(line, "=")
			if found && key == "ID" {
				value = strings.Trim(strings.TrimSpace(value), "\"'")
				if value == "ubuntu" || value == "alpine" || value == "openwrt" {
					return value
				}
			}
		}
	}
	return runtime.GOOS
}

func isSupportedPlatform(platform string) bool {
	return platform == "openwrt" || platform == "alpine" || platform == "ubuntu"
}

func releaseAssetName(architecture string) (string, bool) {
	switch architecture {
	case "amd64", "arm64":
		return "wwan-proxy-linux-" + architecture + "-musl.tar.gz", true
	default:
		return "", false
	}
}

func validateRepository(repository string) error {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") || !safeName(owner) || !safeName(name) {
		return fmt.Errorf("invalid GitHub repository %q; expected OWNER/REPO", repository)
	}
	return nil
}

func safeName(value string) bool {
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' || r == '.' {
			continue
		}
		return false
	}
	return value != ""
}

func safeReleaseTag(value string) bool {
	return len(value) > 0 && len(value) <= 128 && safeName(value)
}

func isDevelopmentVersion(version string) bool {
	version = strings.TrimSpace(version)
	return version == "" || version == "dev" || strings.Contains(version, "dirty")
}

func normalizeVersion(version string) string {
	return strings.TrimPrefix(strings.TrimSpace(version), "build-")
}

func displayVersion(version string) string {
	if strings.TrimSpace(version) == "" {
		return "dev"
	}
	return strings.TrimSpace(version)
}
