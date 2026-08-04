package store

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
)

type logSink struct {
	store *Store
	queue chan LogEntry
	wg    sync.WaitGroup
	once  sync.Once
}

func newLogSink(store *Store) *logSink {
	s := &logSink{store: store, queue: make(chan LogEntry, 4096)}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for entry := range s.queue {
			if err := store.InsertLog(context.Background(), entry); err != nil {
				fmt.Fprintf(os.Stderr, "sqlite log insert failed: %v\n", err)
			}
		}
	}()
	return s
}

func (s *logSink) close() {
	s.once.Do(func() { close(s.queue); s.wg.Wait() })
}

type PersistentHandler struct {
	console slog.Handler
	sink    *logSink
	level   *slog.LevelVar
	attrs   []slog.Attr
	groups  []string
}

func NewPersistentHandler(console slog.Handler, store *Store) (*PersistentHandler, func()) {
	sink := newLogSink(store)
	level := new(slog.LevelVar)
	level.Set(slog.LevelWarn)
	return &PersistentHandler{console: console, sink: sink, level: level}, sink.close
}

func (h *PersistentHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level.Level()
}

func (h *PersistentHandler) Handle(ctx context.Context, record slog.Record) error {
	// Enabled is normally called by slog.Logger before Handle. Re-check here so
	// an in-flight record cannot slip into either sink after a stricter level is
	// applied through the settings API.
	if !h.Enabled(ctx, record.Level) {
		return nil
	}
	var consoleErr error
	if h.console.Enabled(ctx, record.Level) {
		consoleErr = h.console.Handle(ctx, record)
	}
	details := make(map[string]any)
	for _, attr := range h.attrs {
		addLogAttr(details, h.groups, attr)
	}
	record.Attrs(func(attr slog.Attr) bool { addLogAttr(details, h.groups, attr); return true })
	entry := LogEntry{Timestamp: record.Time, Level: record.Level.String(), Message: record.Message, Details: details}
	if v, ok := details["component"].(string); ok {
		entry.Component = v
	}
	if v, ok := details["server"].(string); ok {
		entry.ServerName = v
	}
	select {
	case h.sink.queue <- entry:
	default:
		fmt.Fprintln(os.Stderr, "sqlite log queue full; dropping log entry")
	}
	return consoleErr
}

// SetLevel changes the shared minimum level used by this handler and every
// logger derived from it through With/WithGroup. The filter runs before both
// the console handler and the SQLite queue, so disabled records are not stored
// in either destination. It is safe to call while logs are being emitted.
func (h *PersistentHandler) SetLevel(name string) error {
	level, err := parseLogLevel(name)
	if err != nil {
		return err
	}
	h.level.Set(level)
	return nil
}

func (h *PersistentHandler) Level() string { return h.level.Level().String() }

func parseLogLevel(name string) (slog.Level, error) {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case "DEBUG":
		return slog.LevelDebug, nil
	case "INFO":
		return slog.LevelInfo, nil
	case "WARN":
		return slog.LevelWarn, nil
	case "ERROR":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("log level must be DEBUG, INFO, WARN, or ERROR")
	}
}

func (h *PersistentHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := *h
	clone.console = h.console.WithAttrs(attrs)
	clone.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &clone
}

func (h *PersistentHandler) WithGroup(name string) slog.Handler {
	clone := *h
	clone.console = h.console.WithGroup(name)
	clone.groups = append(append([]string(nil), h.groups...), name)
	return &clone
}

func addLogAttr(dst map[string]any, groups []string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	if attr.Equal(slog.Attr{}) {
		return
	}
	key := attr.Key
	if len(groups) > 0 {
		for i := len(groups) - 1; i >= 0; i-- {
			key = groups[i] + "." + key
		}
	}
	if attr.Value.Kind() == slog.KindGroup {
		for _, nested := range attr.Value.Group() {
			addLogAttr(dst, append(groups, attr.Key), nested)
		}
		return
	}
	if attr.Value.Kind() == slog.KindAny {
		if err, ok := attr.Value.Any().(error); ok {
			dst[key] = err.Error()
			return
		}
	}
	dst[key] = attr.Value.Any()
}
