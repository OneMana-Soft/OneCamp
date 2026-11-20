package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	auditBusiness "github.com/akashc777/OneCamp/business/AdminAudit"
	recordingBusiness "github.com/akashc777/OneCamp/business/Recording"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/go-chi/chi/v5"
)

func keepTranscriptSeams(t *testing.T) {
	saved, savedRead := correctLine, readableTranscript
	t.Cleanup(func() { correctLine, readableTranscript = saved, savedRead })
}

// askCorrection patches line 0x9 of EG_1 as a member (admin: a workspace
// admin; token: through an API token) with body, and answers the response.
func askCorrection(admin, token bool, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPatch, "/calls/recordings/EG_1/transcript/lines/0x9", strings.NewReader(body))
	rc := chi.NewRouteContext()
	rc.URLParams.Add("key", "EG_1")
	rc.URLParams.Add("line_uid", "0x9")
	info := model.UserInfo{}
	info.UserDgraphInfo.Uid = "0x20"
	info.UserDgraphInfo.Uuid = "22222222-2222-2222-2222-222222222222"
	info.UserPostgresInfo.IsAdmin = admin
	ctx := context.WithValue(context.WithValue(r.Context(), chi.RouteCtxKey, rc), helpers.UserInfoContextKey, info)
	if token {
		ctx = context.WithValue(ctx, helpers.ApiTokenIDContextKey, "token-1")
	}
	w := httptest.NewRecorder()
	HandleCorrectTranscriptLine(w, r.WithContext(ctx))
	return w
}

const correctionBody = `{"text":"the launch budget","was":"the lunch budget"}`

// The business's answers, as the route says them: 404 for the missing line
// (and every non-reader), 403, 409 with the line now and its words, 400.
func TestACorrectionsAnswersAreTheRoutesStatuses(t *testing.T) {
	keepTranscriptSeams(t)
	now := time.Now()
	stale := &recordingBusiness.StaleLine{Line: &dgraphStruct.DgraphTranscript{Uid: "0x9", Text: "the words now", CorrectedAt: &now,
		CorrectedBy: &dgraphStruct.DgraphUser{UserFullName: "Maya Chen"}}}
	for err, want := range map[error]int{
		recordingBusiness.ErrLineMissing:   http.StatusNotFound,
		recordingBusiness.ErrMayNotCorrect: http.StatusForbidden,
		stale:                              http.StatusConflict,
		recordingBusiness.ErrBadCorrection: http.StatusBadRequest,
		auditBusiness.ErrNotRecorded:       http.StatusServiceUnavailable,
		errors.New("dgraph: unavailable"):  http.StatusServiceUnavailable,
	} {
		correctLine = func(context.Context, recordingBusiness.Corrector, recordingBusiness.Correction) (*dgraphStruct.DgraphTranscript, error) {
			return nil, err
		}
		w := askCorrection(false, false, correctionBody)
		if w.Code != want {
			t.Fatalf("%v answered %d, want %d", err, w.Code, want)
		}
		if want == http.StatusConflict && (!strings.Contains(w.Body.String(), "Maya Chen") || !strings.Contains(strings.Join(strings.Fields(w.Body.String()), ""), `"transcript_text":"thewordsnow"`)) {
			t.Fatalf("the 409 says %s", w.Body)
		}
	}
	if w := askCorrection(false, false, `{"text":`); w.Code != http.StatusBadRequest {
		t.Fatalf("a broken body answered %d", w.Code)
	}
}

// A correction made answers the line as it reads now, and hands the business
// the route's key, line and words.
func TestACorrectionMadeAnswersTheLine(t *testing.T) {
	keepTranscriptSeams(t)
	var got recordingBusiness.Correction
	correctLine = func(_ context.Context, who recordingBusiness.Corrector, c recordingBusiness.Correction) (*dgraphStruct.DgraphTranscript, error) {
		got = c
		return &dgraphStruct.DgraphTranscript{Uid: "0x9", Text: c.Text, OriginalText: c.Was}, nil
	}
	w := askCorrection(false, false, correctionBody)
	if w.Code != http.StatusOK || got != (recordingBusiness.Correction{Key: "EG_1", LineUID: "0x9", Text: "the launch budget", Was: "the lunch budget"}) {
		t.Fatalf("%d %s, asked %+v", w.Code, w.Body, got)
	}
	var out struct {
		Line dgraphStruct.DgraphTranscript `json:"line"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.Line.OriginalText != "the lunch budget" {
		t.Fatalf("body %s: %v", w.Body, err)
	}
}

// M2 + M10: the deleters are the recording's delete rule, reused: a
// channel's moderators and a workspace admin, anyone in a chat; through an
// API token a workspace admin is only a member.
func TestTheDeletersAreTheDeleteRulesAndATokenIsOnlyAMember(t *testing.T) {
	keepTranscriptSeams(t)
	var deletes func(*dgraphStruct.DgraphRecording) bool
	correctLine = func(_ context.Context, who recordingBusiness.Corrector, _ recordingBusiness.Correction) (*dgraphStruct.DgraphTranscript, error) {
		deletes = who.Deletes
		return &dgraphStruct.DgraphTranscript{}, nil
	}
	channel := func(moderator uint8) *dgraphStruct.DgraphRecording {
		return &dgraphStruct.DgraphRecording{Channel: &dgraphStruct.DgraphChannel{Uuid: "c1", IsMember: 1, IsAdmin: moderator}}
	}
	chat := &dgraphStruct.DgraphRecording{Dm: &dgraphStruct.DgraphDm{GroupingId: "g", ParticipantIsMember: 1}}
	for _, c := range []struct {
		name         string
		admin, token bool
		home         *dgraphStruct.DgraphRecording
		want         bool
	}{
		{"a member", false, false, channel(0), false},
		{"a moderator", false, false, channel(1), true},
		{"a workspace admin", true, false, channel(0), true},
		{"a workspace admin's token", true, true, channel(0), false},
		{"a moderator's token", false, true, channel(1), true},
		{"in the chat", false, false, chat, true},
		{"an event's call", true, false, &dgraphStruct.DgraphRecording{Event: "e1"}, false},
	} {
		askCorrection(c.admin, c.token, correctionBody)
		if deletes == nil || deletes(c.home) != c.want {
			t.Fatalf("%s: may delete %v, want %v", c.name, deletes != nil && deletes(c.home), c.want)
		}
	}
}

// The export by key: a reader gets the file; anyone else the missing answer;
// an unknown format is a 400; the read is the one capped read.
func TestTheExportByKeyAnswersAFileToItsReaders(t *testing.T) {
	keepTranscriptSeams(t)
	off := int64(0)
	reads := 0
	readableTranscript = func(_ context.Context, user, key string, max int) (*dgraphStruct.DgraphRecording, error) {
		reads++
		if key != "EG_1" || max != 5000 {
			t.Fatalf("read %s, %d lines", key, max)
		}
		return &dgraphStruct.DgraphRecording{EgressId: key, Channel: &dgraphStruct.DgraphChannel{Uuid: "c1", Name: "launch"},
			Transcript: []*dgraphStruct.DgraphTranscript{{Uid: "0x9", Text: "hello", OffsetMs: &off}}}, nil
	}
	ask := func(format string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, "/calls/recordings/EG_1/transcript?format="+format, nil)
		rc := chi.NewRouteContext()
		rc.URLParams.Add("key", "EG_1")
		info := model.UserInfo{}
		info.UserDgraphInfo.Uid = "0x20"
		ctx := context.WithValue(context.WithValue(r.Context(), chi.RouteCtxKey, rc), helpers.UserInfoContextKey, info)
		w := httptest.NewRecorder()
		HandleExportTranscript(w, r.WithContext(ctx))
		return w
	}
	w := ask("srt")
	if w.Code != http.StatusOK || reads != 1 || !strings.HasPrefix(w.Header().Get("Content-Disposition"), `attachment; filename="launch call`) ||
		!strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("%d, %d reads, %q", w.Code, reads, w.Header().Get("Content-Disposition"))
	}
	if w := ask("pdf"); w.Code != http.StatusBadRequest {
		t.Fatalf("pdf answered %d", w.Code)
	}
	readableTranscript = func(context.Context, string, string, int) (*dgraphStruct.DgraphRecording, error) {
		return nil, recordingBusiness.ErrLineMissing
	}
	if w := ask("vtt"); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "hello") {
		t.Fatalf("a non-reader got %d %s", w.Code, w.Body)
	}
}
