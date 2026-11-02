package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	"github.com/akashc777/OneCamp/controllers/httpx"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// A meeting's call is recorded only by whoever runs it by rule (its owner, an
// event's organiser: HostByRule, as its lobby is changed), never through an
// API token and never by an admin's flag, through the ONE start; a meeting
// that keeps nothing says so, and every start and stop is in the audit log.
func TestOnlyWhoRunsAMeetingRecordsIt(t *testing.T) {
	maya := uuid.MustParse("0b6e5d4c-3b2a-4190-8f7e-6d5c4b3a2910")
	was := struct {
		host  func(context.Context, string, *dgraphStruct.DgraphUser, bool) (bool, error)
		start func(context.Context, string, *dgraphStruct.DgraphUser) error
		stop  func(context.Context, string) error
		audit func(*http.Request, string, string, string, map[string]interface{})
	}{meetingHost, startCallRecording, stopCallRecording, recordAudit}
	t.Cleanup(func() {
		meetingHost, startCallRecording, stopCallRecording, recordAudit = was.host, was.start, was.stop, was.audit
	})
	host, startErr := true, error(nil)
	var asked, started, stopped, audited []string
	meetingHost = func(_ context.Context, room string, u *dgraphStruct.DgraphUser, admin bool) (bool, error) {
		asked = append(asked, room+" "+u.Uid)
		if admin {
			t.Error("an admin's flag counted towards running a meeting")
		}
		return host, nil
	}
	startCallRecording = func(_ context.Context, room string, u *dgraphStruct.DgraphUser) error {
		started = append(started, room+" "+u.Uid)
		return startErr
	}
	stopCallRecording = func(_ context.Context, room string) error {
		stopped = append(stopped, room)
		return nil
	}
	recordAudit = func(_ *http.Request, action, _, _ string, meta map[string]interface{}) {
		audited = append(audited, fmt.Sprint(action, " ", meta["meeting"], " ", meta["on"]))
	}
	ask := func(body string, token bool) int {
		r := httptest.NewRequest(http.MethodPost, "/calls/meetings/m1/recording", strings.NewReader(body))
		rc := chi.NewRouteContext()
		rc.URLParams.Add("meeting_id", "m1")
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
		info := &models.UserInfo{}
		info.UserDgraphInfo.Uid, info.UserDgraphInfo.Uuid, info.UserPostgresInfo.Id = "0x2a", maya.String(), maya
		code, _ := askAs(SetMeetingRecording, r, info, token)
		return code
	}

	if code := ask(`{"on":true}`, true); code != http.StatusForbidden {
		t.Errorf("through an API token: %d, want 403", code)
	}
	if code := ask(`{}`, false); code != http.StatusBadRequest {
		t.Errorf("without on: %d, want 400", code)
	}
	host = false
	if code := ask(`{"on":true}`, false); code != http.StatusNotFound {
		t.Errorf("someone who doesn't run the meeting: %d, want 404", code)
	}
	if len(started)+len(stopped)+len(audited) != 0 {
		t.Fatalf("a refused ask started %v, stopped %v, audited %v", started, stopped, audited)
	}
	host = true
	if code := ask(`{"on":true}`, false); code != http.StatusOK {
		t.Errorf("its host starts it: %d, want 200", code)
	}
	if code := ask(`{"on":false}`, false); code != http.StatusOK {
		t.Errorf("its host stops it: %d, want 200", code)
	}
	if !slices.Equal(started, []string{"meet-m1 0x2a"}) || !slices.Equal(stopped, []string{"meet-m1"}) {
		t.Errorf("started %v and stopped %v, want meet-m1 once each, as its host", started, stopped)
	}
	if !slices.Equal(audited, []string{"call.recording m1 true", "call.recording m1 false"}) {
		t.Errorf("audited %v, want the start and the stop", audited)
	}
	if !slices.Contains(asked, "meet-m1 0x2a") {
		t.Errorf("asked who runs %v, want the meeting's own room", asked)
	}
	startErr = liveKitBusiness.ErrTranscriptNotKept
	if code := ask(`{"on":true}`, false); code != http.StatusConflict {
		t.Errorf("a meeting that keeps nothing: %d, want 409", code)
	}
	startErr = errors.New("the call server didn't answer")
	if code := ask(`{"on":true}`, false); code != http.StatusServiceUnavailable {
		t.Errorf("a start the server couldn't make: %d, want 503", code)
	}
	if len(audited) != 2 {
		t.Errorf("a start that didn't happen was audited: %v", audited)
	}
}

// A stop when nothing records the meeting's call (its host stopped it in
// another tab, or it ended with its room) is the asker's view being behind,
// answered as on every route that stops a recording: 400 with the plain
// message, never the 503 that tells clients and monitoring the server is down.
// Nothing is audited, since nothing changed. A stop the call server couldn't
// make is still a 503.
func TestAStopWithNothingRecordingIsTheAsksStateNotTheServers(t *testing.T) {
	was := struct {
		host  func(context.Context, string, *dgraphStruct.DgraphUser, bool) (bool, error)
		stop  func(context.Context, string) error
		audit func(*http.Request, string, string, string, map[string]interface{})
	}{meetingHost, stopCallRecording, recordAudit}
	t.Cleanup(func() { meetingHost, stopCallRecording, recordAudit = was.host, was.stop, was.audit })
	var stopErr error
	audited := 0
	meetingHost = func(context.Context, string, *dgraphStruct.DgraphUser, bool) (bool, error) { return true, nil }
	stopCallRecording = func(context.Context, string) error { return stopErr }
	recordAudit = func(*http.Request, string, string, string, map[string]interface{}) { audited++ }
	stop := func() (int, string) {
		r := httptest.NewRequest(http.MethodPost, "/calls/meetings/m1/recording", strings.NewReader(`{"on":false}`))
		rc := chi.NewRouteContext()
		rc.URLParams.Add("meeting_id", "m1")
		r = r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rc))
		maya := uuid.MustParse("0b6e5d4c-3b2a-4190-8f7e-6d5c4b3a2910")
		info := &models.UserInfo{}
		info.UserDgraphInfo.Uid, info.UserDgraphInfo.Uuid, info.UserPostgresInfo.Id = "0x2a", maya.String(), maya
		code, body := askAs(SetMeetingRecording, r, info, false)
		var msg string
		_ = json.Unmarshal(body["msg"], &msg)
		return code, msg
	}

	stopErr = fmt.Errorf("stopping meet-m1: %w", liveKitBusiness.ErrNothingRecording)
	if code, msg := stop(); code != http.StatusBadRequest || msg != httpx.NothingRecording {
		t.Errorf("a stop with nothing recording: %d %q, want 400 %q", code, msg, httpx.NothingRecording)
	}
	if audited != 0 {
		t.Errorf("a stop that changed nothing was audited %d times", audited)
	}
	stopErr = errors.New("the call server didn't answer")
	if code, _ := stop(); code != http.StatusServiceUnavailable {
		t.Errorf("a stop the call server couldn't make: %d, want 503", code)
	}
}
