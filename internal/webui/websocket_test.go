package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/manager"
	"wwan-proxy/internal/store"
)

func TestOverviewWebSocketAuthenticationPushRefreshAndLogout(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "websocket.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	managerContext, managerCancel := context.WithCancel(context.Background())
	defer managerCancel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := manager.New(managerContext, st, logger)
	defer mgr.Close()
	ui := New("127.0.0.1:0", st, mgr, logger)
	ui.websocketInterval = 20 * time.Millisecond
	t.Cleanup(ui.websocketCancel)
	testServer := httptest.NewServer(ui.http.Handler)
	defer testServer.Close()

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar}
	dialContext, dialCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer dialCancel()
	unauthorized, response, err := websocket.Dial(dialContext, testServer.URL+"/api/ws", &websocket.DialOptions{HTTPClient: client})
	if unauthorized != nil {
		_ = unauthorized.Close(websocket.StatusNormalClosure, "")
		t.Fatal("unauthenticated WebSocket connection was accepted")
	}
	if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated handshake status=%v err=%v", statusCode(response), err)
	}

	authBody := []byte(`{"username":"administrator","password":"StrongPassword!42"}`)
	response, err = client.Post(testServer.URL+"/api/auth/initialize", "application/json", bytes.NewReader(authBody))
	if err != nil || response.StatusCode != http.StatusCreated {
		t.Fatalf("initialize status=%v err=%v", statusCode(response), err)
	}
	_ = response.Body.Close()

	serverConfig := config.Server{Enabled: false, Name: "websocket-test", Listen: "127.0.0.1:11881", Interface: "lo", Auth: config.Auth{Method: "none"}}
	configBody, _ := json.Marshal(serverConfig)
	response, err = client.Post(testServer.URL+"/api/servers", "application/json", bytes.NewReader(configBody))
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("save configuration status=%v err=%v", statusCode(response), err)
	}
	_ = response.Body.Close()

	connection, response, err := websocket.Dial(dialContext, testServer.URL+"/api/ws", &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		t.Fatalf("authenticated WebSocket handshake status=%v err=%v", statusCode(response), err)
	}
	defer connection.Close(websocket.StatusNormalClosure, "")
	type overviewMessage struct {
		Servers []config.Server `json:"servers"`
		Process struct {
			HeapBytes        uint64 `json:"heap_bytes"`
			WebSocketClients int64  `json:"websocket_clients"`
		} `json:"process"`
	}
	readContext, readCancel := context.WithTimeout(context.Background(), time.Second)
	var first overviewMessage
	if err := wsjson.Read(readContext, connection, &first); err != nil {
		readCancel()
		t.Fatal(err)
	}
	readCancel()
	if len(first.Servers) != 1 || first.Servers[0].Name != "websocket-test" || first.Process.HeapBytes == 0 || first.Process.WebSocketClients != 1 {
		t.Fatalf("unexpected initial WebSocket overview: %+v", first)
	}

	writeContext, writeCancel := context.WithTimeout(context.Background(), time.Second)
	if err := connection.Write(writeContext, websocket.MessageText, []byte("refresh")); err != nil {
		writeCancel()
		t.Fatal(err)
	}
	writeCancel()
	readContext, readCancel = context.WithTimeout(context.Background(), 3*time.Second)
	var refreshed overviewMessage
	if err := wsjson.Read(readContext, connection, &refreshed); err != nil {
		readCancel()
		t.Fatalf("manual WebSocket refresh failed: %v", err)
	}
	readCancel()
	if len(refreshed.Servers) != 1 {
		t.Fatalf("manual refresh returned unexpected overview: %+v", refreshed)
	}

	crossOriginHeader := make(http.Header)
	crossOriginHeader.Set("Origin", "https://attacker.example")
	evilConnection, evilResponse, evilErr := websocket.Dial(dialContext, testServer.URL+"/api/ws", &websocket.DialOptions{HTTPClient: client, HTTPHeader: crossOriginHeader})
	if evilConnection != nil {
		_ = evilConnection.Close(websocket.StatusNormalClosure, "")
		t.Fatal("cross-origin WebSocket connection was accepted")
	}
	if evilErr == nil || evilResponse == nil || evilResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin handshake status=%v err=%v", statusCode(evilResponse), evilErr)
	}

	response, err = client.Post(testServer.URL+"/api/auth/logout", "application/json", nil)
	if err != nil || response.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status=%v err=%v", statusCode(response), err)
	}
	_ = response.Body.Close()
	readContext, readCancel = context.WithTimeout(context.Background(), time.Second)
	for err == nil {
		var afterLogout overviewMessage
		err = wsjson.Read(readContext, connection, &afterLogout)
	}
	readCancel()
	if websocket.CloseStatus(err) != websocket.StatusPolicyViolation {
		t.Fatalf("logout should close WebSocket with policy violation, status=%v err=%v", websocket.CloseStatus(err), err)
	}
}

func statusCode(response *http.Response) any {
	if response == nil {
		return nil
	}
	return response.StatusCode
}
