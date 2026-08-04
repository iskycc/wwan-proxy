package manager

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/store"
)

func TestReloadHandsOffListenerWithoutClosingEstablishedSOCKSConnection(t *testing.T) {
	m, st, cfg, echo := newReloadTestManager(t)
	defer st.Close()
	defer echo.Close()
	defer m.Close()
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForSOCKSListener(t, cfg.Listen)
	oldConn := dialSOCKSEcho(t, cfg.Listen, echo.Addr().(*net.TCPAddr))
	defer oldConn.Close()
	m.mu.RLock()
	old := m.instances[cfg.ID]
	m.mu.RUnlock()

	cfg.MaxConnections = 64
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(context.Background(), cfg.ID); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	replacement := m.instances[cfg.ID]
	m.mu.RUnlock()
	if replacement == old {
		t.Fatal("reload retained the old listener instance")
	}
	assertEcho(t, oldConn, "old-session-survived")

	newConn := dialSOCKSEcho(t, cfg.Listen, echo.Addr().(*net.TCPAddr))
	assertEcho(t, newConn, "new-listener-ready")
	_ = newConn.Close()
	_ = oldConn.Close()
}

func TestReloadCompletesAfterCallerContextIsCanceled(t *testing.T) {
	m, st, cfg, echo := newReloadTestManager(t)
	defer st.Close()
	defer echo.Close()
	defer m.Close()
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForSOCKSListener(t, cfg.Listen)
	cfg.MaxConnections = 32
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	caller, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Reload(caller, cfg.ID); err != nil {
		t.Fatalf("reload inherited canceled HTTP request context: %v", err)
	}
	conn := dialSOCKSEcho(t, cfg.Listen, echo.Addr().(*net.TCPAddr))
	assertEcho(t, conn, "service-context-reload")
	_ = conn.Close()
}

func TestReloadPreflightFailureKeepsOldInstanceRunning(t *testing.T) {
	m, st, cfg, echo := newReloadTestManager(t)
	defer st.Close()
	defer echo.Close()
	defer m.Close()
	m.preflightDevice = func(string) error { return nil }
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForSOCKSListener(t, cfg.Listen)
	oldConn := dialSOCKSEcho(t, cfg.Listen, echo.Addr().(*net.TCPAddr))
	defer oldConn.Close()
	m.mu.RLock()
	old := m.instances[cfg.ID]
	m.mu.RUnlock()

	cfg.MaxConnections = 64
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	m.preflightDevice = func(string) error { return syscall.EPERM }
	err := m.Reload(context.Background(), cfg.ID)
	if err == nil || !strings.Contains(err.Error(), "operation not permitted") {
		t.Fatalf("reload preflight error=%v", err)
	}
	m.mu.RLock()
	current := m.instances[cfg.ID]
	m.mu.RUnlock()
	if current != old {
		t.Fatal("preflight failure replaced the working instance")
	}
	assertEcho(t, oldConn, "old-session-after-preflight-failure")
	restored, err := st.GetServer(context.Background(), cfg.ID)
	if err != nil || restored.MaxConnections != old.cfg.MaxConnections {
		t.Fatalf("stored config was not restored: max=%d err=%v", restored.MaxConnections, err)
	}
}

func TestReloadCredentialRollbackDoesNotMutateRunningAuthMap(t *testing.T) {
	m, st, cfg, echo := newReloadTestManager(t)
	defer st.Close()
	defer echo.Close()
	defer m.Close()
	cfg.Auth = config.Auth{
		Method: "username_password",
		Users:  map[string]string{"alice": "correct horse battery staple"},
	}
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	m.preflightDevice = func(string) error { return nil }
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForSOCKSListener(t, cfg.Listen)
	m.mu.RLock()
	old := m.instances[cfg.ID]
	m.mu.RUnlock()
	expectedHash := old.cfg.Auth.Users["alice"]
	if expectedHash == "" || expectedHash == "correct horse battery staple" {
		t.Fatalf("runtime credential was not loaded as a hash: %q", expectedHash)
	}

	cfg.MaxConnections = 64
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	m.preflightDevice = func(string) error { return syscall.EPERM }

	stopReaders := make(chan struct{})
	badRead := make(chan string, 1)
	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
				}
				if got := old.cfg.Auth.Users["alice"]; got != expectedHash {
					select {
					case badRead <- got:
					default:
					}
					return
				}
			}
		}()
	}
	for range 32 {
		err := m.Reload(context.Background(), cfg.ID)
		if err == nil || !strings.Contains(err.Error(), "operation not permitted") {
			close(stopReaders)
			readers.Wait()
			t.Fatalf("reload preflight error=%v", err)
		}
	}
	close(stopReaders)
	readers.Wait()
	select {
	case got := <-badRead:
		t.Fatalf("rollback mutated the running authentication map: got %q, want %q", got, expectedHash)
	default:
	}
	if got := old.cfg.Auth.Users["alice"]; got != expectedHash {
		t.Fatalf("runtime credential changed after rollbacks: got %q, want %q", got, expectedHash)
	}
}

func TestEnablePreflightFailureLeavesNoRunningInstance(t *testing.T) {
	m, st, cfg, echo := newReloadTestManager(t)
	defer st.Close()
	defer echo.Close()
	defer m.Close()
	cfg.Enabled = false
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg.Enabled = true
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	m.preflightDevice = func(string) error { return syscall.ENODEV }
	err := m.Reload(context.Background(), cfg.ID)
	if err == nil || !strings.Contains(err.Error(), "no such device") {
		t.Fatalf("enable preflight error=%v", err)
	}
	if snapshots := m.Snapshots(); len(snapshots) != 0 {
		t.Fatalf("failed enable left runtime instances: %+v", snapshots)
	}
	stored, err := st.GetServer(context.Background(), cfg.ID)
	if err != nil || stored.Enabled {
		t.Fatalf("failed enable was not disabled in storage: enabled=%v err=%v", stored.Enabled, err)
	}
}

func TestStartAllPreflightFailureRecordsStoppedInstance(t *testing.T) {
	m, st, cfg, echo := newReloadTestManager(t)
	defer st.Close()
	defer echo.Close()
	defer m.Close()
	m.preflightDevice = func(string) error { return syscall.EPERM }
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshots := m.Snapshots()
	if len(snapshots) != 1 || snapshots[0].Running || !strings.Contains(snapshots[0].LastError, "operation not permitted") {
		t.Fatalf("unexpected failed preflight snapshot: %+v", snapshots)
	}
	if !snapshots[0].StartedAt.IsZero() {
		t.Fatalf("preflight placeholder has a start time: %v", snapshots[0].StartedAt)
	}
	if conn, err := net.DialTimeout("tcp4", cfg.Listen, 50*time.Millisecond); err == nil {
		_ = conn.Close()
		t.Fatal("preflight failure still started a listener")
	}
}

func TestReloadListenerFailureDoesNotLaunchPreflightPlaceholder(t *testing.T) {
	m, st, cfg, echo := newReloadTestManager(t)
	defer st.Close()
	defer echo.Close()
	defer m.Close()

	const missingInterface = "missing-preflight-interface"
	cfg.Interface = missingInterface
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	m.preflightDevice = func(name string) error {
		if name == missingInterface {
			return syscall.ENODEV
		}
		return nil
	}
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	placeholder := m.instances[cfg.ID]
	m.mu.RUnlock()
	if placeholder == nil {
		t.Fatal("StartAll did not retain the failed preflight placeholder")
	}
	if err := m.Reload(context.Background(), cfg.ID); err == nil || !strings.Contains(err.Error(), "previous stopped configuration restored") {
		t.Fatalf("placeholder preflight retry returned misleading error: %v", err)
	}

	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	cfg.Interface = "lo"
	cfg.HTTPProxy = config.HTTPProxy{Enabled: true, Listen: occupied.Addr().String()}
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(context.Background(), cfg.ID); err == nil {
		t.Fatal("reload unexpectedly succeeded with an occupied HTTP listener")
	}

	m.mu.RLock()
	current := m.instances[cfg.ID]
	m.mu.RUnlock()
	if current != placeholder {
		t.Fatal("failed replacement did not restore the original stopped placeholder")
	}
	placeholder.mu.RLock()
	launched := placeholder.launched
	placeholder.mu.RUnlock()
	if launched {
		t.Fatal("rollback launched a preflight-failed placeholder")
	}
	snapshots := m.Snapshots()
	if len(snapshots) != 1 || snapshots[0].Running || !snapshots[0].StartedAt.IsZero() {
		t.Fatalf("restored placeholder is not stopped: %+v", snapshots)
	}
	if conn, dialErr := net.DialTimeout("tcp4", cfg.Listen, 50*time.Millisecond); dialErr == nil {
		_ = conn.Close()
		t.Fatal("restored placeholder unexpectedly has a listener")
	}
	stored, err := st.GetServer(context.Background(), cfg.ID)
	if err != nil || stored.Interface != missingInterface || stored.HTTPProxy.Enabled {
		t.Fatalf("stopped configuration was not restored: interface=%q HTTP=%+v err=%v", stored.Interface, stored.HTTPProxy, err)
	}
}

func TestStartAllAndCloseSerializeLifecycleTransitions(t *testing.T) {
	m, st, _, echo := newReloadTestManager(t)
	defer st.Close()
	defer echo.Close()
	preflightStarted := make(chan struct{})
	releasePreflight := make(chan struct{})
	m.preflightDevice = func(string) error {
		close(preflightStarted)
		<-releasePreflight
		return syscall.EPERM
	}
	startDone := make(chan error, 1)
	go func() { startDone <- m.StartAll(context.Background()) }()
	select {
	case <-preflightStarted:
	case <-time.After(time.Second):
		t.Fatal("StartAll did not reach device preflight")
	}
	closeDone := make(chan struct{})
	go func() {
		m.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
		t.Fatal("Close raced past an in-progress StartAll transition")
	case <-time.After(50 * time.Millisecond):
	}
	close(releasePreflight)
	if err := <-startDone; err != nil {
		t.Fatalf("StartAll returned error: %v", err)
	}
	select {
	case <-closeDone:
	case <-time.After(time.Second):
		t.Fatal("Close did not finish after StartAll released the transition")
	}
	if snapshots := m.Snapshots(); len(snapshots) != 0 {
		t.Fatalf("Close left instances created by StartAll: %+v", snapshots)
	}
}

func TestDisabledConfigSkipsDevicePreflight(t *testing.T) {
	m, st, cfg, echo := newReloadTestManager(t)
	defer st.Close()
	defer echo.Close()
	defer m.Close()
	cfg.Enabled = false
	cfg.Interface = "missing-disabled-interface"
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	m.preflightDevice = func(string) error {
		t.Fatal("disabled configuration ran device preflight")
		return syscall.ENODEV
	}
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	if snapshots := m.Snapshots(); len(snapshots) != 0 {
		t.Fatalf("disabled configuration created runtime state: %+v", snapshots)
	}
}

func TestDefaultDevicePreflightRejectsMissingInterface(t *testing.T) {
	if err := defaultDevicePreflight("wwan-proxy-interface-that-does-not-exist"); err == nil || !strings.Contains(err.Error(), "interface lookup") {
		t.Fatalf("missing interface preflight error=%v", err)
	}
}

func TestReloadFailureRestoresListenerAndPreservesOldSession(t *testing.T) {
	m, st, cfg, echo := newReloadTestManager(t)
	defer st.Close()
	defer echo.Close()
	defer m.Close()
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForSOCKSListener(t, cfg.Listen)
	oldConn := dialSOCKSEcho(t, cfg.Listen, echo.Addr().(*net.TCPAddr))
	defer oldConn.Close()

	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	cfg.HTTPProxy = config.HTTPProxy{Enabled: true, Listen: occupied.Addr().String()}
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(context.Background(), cfg.ID); err == nil {
		t.Fatal("reload unexpectedly succeeded with an occupied HTTP listener")
	}
	restored, err := st.GetServer(context.Background(), cfg.ID)
	if err != nil || restored.HTTPProxy.Enabled {
		t.Fatalf("failed reload was not rolled back in SQLite: HTTP=%+v err=%v", restored.HTTPProxy, err)
	}
	assertEcho(t, oldConn, "old-session-after-rollback")

	newConn := dialSOCKSEcho(t, cfg.Listen, echo.Addr().(*net.TCPAddr))
	assertEcho(t, newConn, "restored-listener")
	_ = newConn.Close()
	_ = oldConn.Close()
}

func TestReloadSharesUDPAssociationHardLimitWithDrainingGeneration(t *testing.T) {
	m, st, cfg, echo := newReloadTestManager(t)
	defer st.Close()
	defer echo.Close()
	defer m.Close()
	cfg.UDP = config.UDP{
		Enabled: true, MaxAssociations: 1, BindIP: "127.0.0.1", Advertise: "auto",
		IdleTimeout: config.Duration(5 * time.Second), PortMin: 20000, PortMax: 30000,
	}
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.StartAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitForSOCKSListener(t, cfg.Listen)
	first, reply := openUDPAssociation(t, cfg.Listen)
	if reply != 0 {
		t.Fatalf("first UDP association reply=%d", reply)
	}
	defer first.Close()

	cfg.MaxConnections = 32
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	if err := m.Reload(context.Background(), cfg.ID); err != nil {
		t.Fatal(err)
	}
	second, reply := openUDPAssociation(t, cfg.Listen)
	_ = second.Close()
	if reply != 2 {
		t.Fatalf("replacement admitted UDP association while old generation held hard limit: reply=%d", reply)
	}

	_ = first.Close()
	deadline := time.Now().Add(time.Second)
	for {
		third, rep := openUDPAssociation(t, cfg.Listen)
		if rep == 0 {
			_ = third.Close()
			break
		}
		_ = third.Close()
		if time.Now().After(deadline) {
			t.Fatalf("UDP capacity was not released after old association closed; last reply=%d", rep)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func newReloadTestManager(t *testing.T) (*Manager, *store.Store, config.Server, net.Listener) {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/reload.db")
	if err != nil {
		t.Fatal(err)
	}
	heartbeat := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(heartbeat.Close)
	echo, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, acceptErr := echo.Accept()
			if acceptErr != nil {
				return
			}
			go func() {
				_, _ = io.Copy(conn, conn)
				_ = conn.Close()
			}()
		}
	}()
	cfg := config.Server{
		Enabled: true, Name: "reload-test", Listen: unusedTCPAddress(t), Interface: "lo",
		Auth: config.Auth{Method: "none"},
		Heartbeat: config.Heartbeat{
			URL: heartbeat.URL, Interval: config.Duration(5 * time.Second), Timeout: config.Duration(time.Second),
		},
	}
	if err := st.SaveServer(context.Background(), &cfg); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return New(context.Background(), st, logger), st, cfg, echo
}

func unusedTCPAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := ln.Addr().String()
	_ = ln.Close()
	return address
}

func waitForSOCKSListener(t *testing.T, address string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp4", address, 20*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("SOCKS listener %s did not start", address)
}

func dialSOCKSEcho(t *testing.T, proxyAddress string, target *net.TCPAddr) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", proxyAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		t.Fatal(err)
	}
	var method [2]byte
	if _, err := io.ReadFull(conn, method[:]); err != nil || method != [2]byte{5, 0} {
		t.Fatalf("SOCKS greeting=%v err=%v", method, err)
	}
	request := []byte{5, 1, 0, 1}
	request = append(request, target.IP.To4()...)
	request = binary.BigEndian.AppendUint16(request, uint16(target.Port))
	if _, err := conn.Write(request); err != nil {
		t.Fatal(err)
	}
	var reply [10]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		t.Fatal(err)
	}
	if reply[0] != 5 || reply[1] != 0 || reply[3] != 1 {
		t.Fatalf("SOCKS CONNECT reply=%v", reply)
	}
	return conn
}

func openUDPAssociation(t *testing.T, proxyAddress string) (net.Conn, byte) {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", proxyAddress, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte{5, 1, 0}); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	var method [2]byte
	if _, err := io.ReadFull(conn, method[:]); err != nil || method != [2]byte{5, 0} {
		_ = conn.Close()
		t.Fatalf("SOCKS greeting=%v err=%v", method, err)
	}
	if _, err := conn.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	var reply [10]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		_ = conn.Close()
		t.Fatal(err)
	}
	if reply[0] != 5 || reply[2] != 0 || reply[3] != 1 {
		_ = conn.Close()
		t.Fatalf("SOCKS UDP reply=%v", reply)
	}
	return conn, reply[1]
}

func assertEcho(t *testing.T, conn net.Conn, value string) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(time.Second))
	if _, err := conn.Write([]byte(value)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(value))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != value {
		t.Fatalf("echo=%q, want %q", got, value)
	}
	_ = conn.SetDeadline(time.Time{})
}
