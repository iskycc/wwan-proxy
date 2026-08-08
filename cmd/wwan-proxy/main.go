package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"wwan-proxy/internal/manager"
	"wwan-proxy/internal/selfupdate"
	"wwan-proxy/internal/store"
	"wwan-proxy/internal/webui"
)

var version = "dev"

func main() {
	dbPath := flag.String("db", "wwan-proxy.db", "path to SQLite database")
	webAddress := flag.String("web", "", "override the persisted WebUI listen address")
	webDefault := flag.String("web-default", "", "WebUI listen address used only when SQLite has no saved value")
	printWebListen := flag.Bool("print-web-listen", false, "print the effective WebUI listen address and exit")
	showVersion := flag.Bool("version", false, "print version and exit")
	updateRepository := flag.String("update-repo", selfupdate.DefaultRepository, "GitHub OWNER/REPO used for update checks")
	updateSocket := flag.String("update-socket", "/run/wwan-proxy/update.sock", "path to the privileged update agent socket")
	updateStatus := flag.String("update-status", "/run/wwan-proxy/update-status.json", "path to the automatic update status file")
	updateAgent := flag.Bool("update-agent", false, "run the privileged local update agent")
	updateInstaller := flag.String("update-installer", "", "platform installer used by update-agent")
	updatePlatform := flag.String("update-platform", "", "platform override used by update-agent")
	updateGroup := flag.String("update-group", "", "group allowed to access the update agent")
	updateBinary := flag.String("update-binary", "", "installed binary path checked after an automatic update")
	updateDownloadURL := flag.String("update-download-url", "", "restricted GitHub URL downloaded for the platform installer")
	updateDownloadOutput := flag.String("update-download-output", "", "absolute output path for the restricted update downloader")
	updateDownloadInterface := flag.String("update-download-interface", "", "network interface used by the restricted update downloader")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if *updateDownloadURL != "" || *updateDownloadOutput != "" || *updateDownloadInterface != "" {
		runUpdateDownload(*updateDownloadURL, *updateDownloadOutput, *updateDownloadInterface)
		return
	}
	if *updateAgent {
		runUpdateAgent(*updateRepository, *updateSocket, *updateStatus, *updateInstaller, *updatePlatform, *updateGroup, *updateBinary)
		return
	}

	// PersistentHandler owns the live minimum level for both destinations. Keep
	// the downstream console open to DEBUG so raising verbosity at runtime does
	// not require rebuilding the logger or restarting the service.
	console := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	st, err := store.OpenWithWebDefault(*dbPath, *webDefault)
	if err != nil {
		fmt.Fprintln(os.Stderr, "open SQLite database:", err)
		os.Exit(2)
	}
	settings, err := st.SystemSettings(context.Background())
	if err != nil {
		fmt.Fprintln(os.Stderr, "load system settings:", err)
		_ = st.Close()
		os.Exit(2)
	}
	effectiveWebAddress := settings.WebListen
	if *webAddress != "" {
		effectiveWebAddress = *webAddress
	}
	if *printWebListen {
		fmt.Println(effectiveWebAddress)
		_ = st.Close()
		return
	}
	handler, flushLogs := store.NewPersistentHandler(console, st)
	if err := handler.SetLevel(settings.LogLevel); err != nil {
		fmt.Fprintln(os.Stderr, "invalid persisted log level; using WARN:", err)
	}
	logger := slog.New(handler)
	_ = st.PruneLogs(context.Background(), time.Now().AddDate(0, 0, -settings.LogRetentionDays))
	if err := st.ApplySessionLifetime(context.Background(), time.Now(), time.Duration(settings.SessionLifetime)); err != nil {
		logger.Error("apply login session lifetime failed", "component", "startup", "error", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go maintenanceLoop(ctx, st, logger)
	mgr := manager.New(ctx, st, logger)
	if err := mgr.StartAll(ctx); err != nil {
		logger.Error("load SQLite configuration failed", "component", "startup", "error", err)
		flushLogs()
		_ = st.Close()
		os.Exit(2)
	}
	ui := webui.New(effectiveWebAddress, st, mgr, logger, handler)
	updates, updateErr := selfupdate.NewController(*updateRepository, version, *updateSocket, *updateStatus)
	if updateErr != nil {
		logger.Error("configure automatic updates failed", "component", "startup", "error", updateErr)
	} else {
		ui.ConfigureUpdates(updates)
	}
	errCh := make(chan error, 1)
	go func() { errCh <- ui.ListenAndServe() }()
	logger.Info("wwan-proxy started", "component", "startup", "database", st.Path(), "web", effectiveWebAddress)

	select {
	case <-ctx.Done():
	case err = <-errCh:
		if err != nil {
			logger.Error("WebUI stopped unexpectedly", "component", "webui", "error", err)
		}
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	_ = ui.Shutdown(shutdownCtx)
	cancel()
	mgr.Close()
	logger.Info("shutdown complete", "component", "startup")
	flushLogs()
	_ = st.Close()
	if err != nil {
		os.Exit(1)
	}
}

func runUpdateDownload(rawURL, outputPath, downloadInterface string) {
	if rawURL == "" || outputPath == "" {
		fmt.Fprintln(os.Stderr, "update downloader requires both -update-download-url and -update-download-output")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := selfupdate.DownloadGitHubFile(ctx, rawURL, outputPath, downloadInterface); err != nil {
		fmt.Fprintln(os.Stderr, "update download failed:", err)
		os.Exit(1)
	}
}

func runUpdateAgent(repository, socketPath, statusPath, installerPath, platform, group, binaryPath string) {
	if platform == "" {
		platform = selfupdate.DetectPlatform()
	}
	if installerPath == "" {
		switch platform {
		case "openwrt":
			installerPath = "/opt/wwan-proxy/install-openwrt.sh"
		case "alpine":
			installerPath = "/usr/local/libexec/wwan-proxy/install-alpine.sh"
		case "ubuntu":
			installerPath = "/usr/local/libexec/wwan-proxy/install-ubuntu.sh"
		}
	}
	if binaryPath == "" {
		binaryPath = "/usr/local/bin/wwan-proxy"
		if platform == "openwrt" {
			binaryPath = "/opt/wwan-proxy/wwan-proxy"
		}
	}
	for _, path := range []string{socketPath, statusPath, installerPath, binaryPath} {
		if !filepath.IsAbs(path) {
			fmt.Fprintln(os.Stderr, "update agent paths must be absolute:", path)
			os.Exit(2)
		}
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	err := selfupdate.RunAgent(ctx, selfupdate.AgentConfig{
		SocketPath: socketPath, StatusPath: statusPath, InstallerPath: installerPath,
		BinaryPath: binaryPath, Platform: platform, Group: group, Repository: repository, Logger: logger,
	})
	if err != nil {
		logger.Error("update agent stopped", "error", err)
		os.Exit(1)
	}
}

func maintenanceLoop(ctx context.Context, st *store.Store, logger *slog.Logger) {
	ticker := time.NewTicker(6 * time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			settings, err := st.SystemSettings(ctx)
			if err != nil {
				logger.Error("load maintenance settings failed", "component", "maintenance", "error", err)
				continue
			}
			if err := st.PruneLogs(ctx, time.Now().AddDate(0, 0, -settings.LogRetentionDays)); err != nil {
				logger.Error("prune expired logs failed", "component", "maintenance", "error", err)
			}
			if err := st.ApplySessionLifetime(ctx, time.Now(), time.Duration(settings.SessionLifetime)); err != nil {
				logger.Error("apply login session lifetime failed", "component", "maintenance", "error", err)
			}
		}
	}
}
