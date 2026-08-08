package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"wwan-proxy/internal/config"
	"wwan-proxy/internal/selfupdate"
)

type fakeUpdateController struct {
	info         selfupdate.Info
	statusErr    error
	triggerErr   error
	checkRemote  bool
	statusRoute  string
	triggerRoute string
	triggerCalls int
}

func (f *fakeUpdateController) Status(_ context.Context, checkRemote bool, downloadInterface string) (selfupdate.Info, error) {
	f.checkRemote = checkRemote
	f.statusRoute = downloadInterface
	return f.info, f.statusErr
}

func (f *fakeUpdateController) Trigger(_ context.Context, downloadInterface string) (selfupdate.Info, error) {
	f.triggerCalls++
	f.triggerRoute = downloadInterface
	return f.info, f.triggerErr
}

func TestConfiguredUpdateInterface(t *testing.T) {
	if err := validateConfiguredUpdateInterface("lo", []config.Server{{Interface: "lo"}}); err != nil {
		t.Fatalf("configured loopback interface rejected: %v", err)
	}
	if err := validateConfiguredUpdateInterface("lo", []config.Server{{Interface: "wwan0"}}); err == nil {
		t.Fatal("unconfigured interface was accepted")
	}
	if err := validateConfiguredUpdateInterface("bad interface", []config.Server{{Interface: "bad interface"}}); err == nil {
		t.Fatal("unsafe interface was accepted")
	}
}

func newUpdateHandlerServer(controller updateController) *Server {
	return &Server{updates: controller, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

func TestGetUpdateRefreshesOnlyWhenRequested(t *testing.T) {
	controller := &fakeUpdateController{info: selfupdate.Info{CurrentVersion: "aaaaaaaaaaaa", Checked: true, UpdateAvailable: true, InstallSupported: true}}
	server := newUpdateHandlerServer(controller)
	recorder := httptest.NewRecorder()
	server.getUpdate(recorder, httptest.NewRequest(http.MethodGet, "/api/update?refresh=1", nil))
	if recorder.Code != http.StatusOK || !controller.checkRemote {
		t.Fatalf("status=%d check_remote=%v body=%s", recorder.Code, controller.checkRemote, recorder.Body.String())
	}
	var info selfupdate.Info
	if err := json.Unmarshal(recorder.Body.Bytes(), &info); err != nil || info.CurrentVersion != "aaaaaaaaaaaa" || !info.UpdateAvailable {
		t.Fatalf("info=%+v err=%v", info, err)
	}
}

func TestStartUpdateSchedulesAgent(t *testing.T) {
	controller := &fakeUpdateController{info: selfupdate.Info{
		CurrentVersion: "aaaaaaaaaaaa",
		Latest:         &selfupdate.Release{Tag: "build-bbbbbbbbbbbb", Version: "bbbbbbbbbbbb"},
	}}
	server := newUpdateHandlerServer(controller)
	recorder := httptest.NewRecorder()
	server.startUpdate(recorder, httptest.NewRequest(http.MethodPost, "/api/update", nil))
	if recorder.Code != http.StatusAccepted || controller.triggerCalls != 1 {
		t.Fatalf("status=%d trigger_calls=%d body=%s", recorder.Code, controller.triggerCalls, recorder.Body.String())
	}
	if controller.triggerRoute != "" {
		t.Fatalf("default update unexpectedly selected route %q", controller.triggerRoute)
	}
}

func TestStartUpdateRejectsMalformedBody(t *testing.T) {
	controller := &fakeUpdateController{}
	server := newUpdateHandlerServer(controller)
	recorder := httptest.NewRecorder()
	server.startUpdate(recorder, httptest.NewRequest(http.MethodPost, "/api/update", bytes.NewBufferString(`{"unknown":true}`)))
	if recorder.Code != http.StatusBadRequest || controller.triggerCalls != 0 {
		t.Fatalf("status=%d trigger_calls=%d body=%s", recorder.Code, controller.triggerCalls, recorder.Body.String())
	}
}

func TestStartUpdateMapsExpectedErrors(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		status int
	}{
		{name: "already current", err: selfupdate.ErrNoUpdate, status: http.StatusConflict},
		{name: "busy", err: selfupdate.ErrUpdateInProgress, status: http.StatusConflict},
		{name: "unsupported", err: errors.Join(selfupdate.ErrUpdateUnsupported, errors.New("agent missing")), status: http.StatusNotImplemented},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newUpdateHandlerServer(&fakeUpdateController{triggerErr: test.err})
			recorder := httptest.NewRecorder()
			server.startUpdate(recorder, httptest.NewRequest(http.MethodPost, "/api/update", nil))
			if recorder.Code != test.status {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, test.status, recorder.Body.String())
			}
		})
	}
}
