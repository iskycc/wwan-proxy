package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"wwan-proxy/internal/manager"
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
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
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
	_ = st.PruneSessions(context.Background(), time.Now())

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
			if err := st.PruneSessions(ctx, time.Now()); err != nil {
				logger.Error("prune expired sessions failed", "component", "maintenance", "error", err)
			}
		}
	}
}
