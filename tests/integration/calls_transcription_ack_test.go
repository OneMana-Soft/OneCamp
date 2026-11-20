//go:build integration

package integration_test

// zoom-09 C2: no call audio leaves the server by default, over the real routes.
// An install whose env chose the browser keeps it, and its admin is asked once
// to acknowledge where the audio goes; a fresh install transcribes on its own
// server and says why captions are off while nothing there runs; an admin sends
// audio anywhere else only with an acknowledgement naming where, which is one
// row in the audit log (who, when, where); and nobody but an admin reads the
// choice or gives it.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	transcriptionBusiness "github.com/akashc777/OneCamp/business/Transcription"
	settingsController "github.com/akashc777/OneCamp/controllers/Settings"
	transcriptionController "github.com/akashc777/OneCamp/controllers/Transcription"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestCallAudioLeavesTheServerOnlyWithAnAdminsAcknowledgement(t *testing.T) {
	ctx := context.Background()
	pg := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, pg.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	// A save of nothing drops the settings cached from another test's database,
	// and again at the end, so the next test never reads this one's (30 s cache).
	if err := transcriptionBusiness.Save(transcriptionBusiness.SaveInput{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := transcriptionBusiness.Save(transcriptionBusiness.SaveInput{}); err != nil {
			t.Error(err)
		}
	})

	person := func(admin bool) context.Context {
		t.Helper()
		id := uuid.New()
		email := "c2-" + id.String()[:8] + "@example.com"
		if _, err := pg.PG.Exec(`INSERT INTO users (id, email_id) VALUES ($1, $2)`, id, email); err != nil {
			t.Fatalf("seed a person: %v", err)
		}
		return context.WithValue(ctx, helpers.UserInfoContextKey,
			userModels.UserInfo{UserPostgresInfo: userModels.User{Id: id, EmailID: email, IsAdmin: admin}})
	}
	admin, member := person(true), person(false)
	adminID, _ := admin.Value(helpers.UserInfoContextKey).(userModels.UserInfo)

	// In the app: the session's person is in the context, as VerifyAuth leaves it.
	inApp := chi.NewRouter()
	inApp.With(customMiddleware.VerifyAdminAuthOnlyPostgres).Get("/admin/transcription/config", transcriptionController.GetConfig)
	inApp.With(customMiddleware.VerifyAdminAuthOnlyPostgres).Post("/admin/transcription/config", transcriptionController.UpdateConfig)
	inApp.Get("/config/client", settingsController.GetClientConfig)
	// From outside a session (an API token, a call guest's token): the real chain.
	outside := chi.NewRouter()
	outside.With(customMiddleware.VerifyAuth, customMiddleware.VerifyAdminAuthOnlyPostgres).Get("/admin/transcription/config", transcriptionController.GetConfig)
	outside.With(customMiddleware.VerifyAuth, customMiddleware.VerifyAdminAuthOnlyPostgres).Post("/admin/transcription/config", transcriptionController.UpdateConfig)

	type status struct {
		Mode                 string `json:"mode"`
		STTProvider          string `json:"stt_provider"`
		Where                string `json:"where"`
		AudioLeavesServer    bool   `json:"audio_leaves_server"`
		AcknowledgedWhere    string `json:"acknowledged_where"`
		NeedsAcknowledgement bool   `json:"needs_acknowledgement"`
		CaptionsOffReason    string `json:"captions_off_reason"`
	}
	call := func(routes http.Handler, as context.Context, method, path, body, auth string) (int, []byte) {
		t.Helper()
		req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body))).WithContext(as)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		return rec.Code, rec.Body.Bytes()
	}
	read := func() status {
		t.Helper()
		code, raw := call(inApp, admin, http.MethodGet, "/admin/transcription/config", "", "")
		var got struct {
			Data status `json:"data"`
		}
		if err := json.Unmarshal(raw, &got); code != http.StatusOK || err != nil {
			t.Fatalf("the admin's read: %d %v", code, err)
		}
		return got.Data
	}
	save := func(body string) (int, string, string) {
		t.Helper()
		code, raw := call(inApp, admin, http.MethodPost, "/admin/transcription/config", body, "")
		var got struct {
			Code  string `json:"code"`
			Where string `json:"where"`
		}
		_ = json.Unmarshal(raw, &got)
		return code, got.Code, got.Where
	}
	membersView := func() (string, string) {
		t.Helper()
		code, raw := call(inApp, member, http.MethodGet, "/config/client", "", "")
		var got struct {
			Data struct {
				Where  string `json:"transcription_where"`
				Reason string `json:"transcription_off_reason"`
			} `json:"data"`
		}
		if err := json.Unmarshal(raw, &got); code != http.StatusOK || err != nil {
			t.Fatalf("a member's client config: %d %v", code, err)
		}
		return got.Data.Where, got.Data.Reason
	}
	// Each acknowledgement's row: who, where, the speech provider (server mode
	// only), the host of an address the admin typed, and the whole metadata.
	type row struct {
		actor          uuid.UUID
		where          string
		provider, host string
		meta           string
	}
	rows := func() []row {
		t.Helper()
		res, err := pg.PG.Query(`SELECT actor_id, metadata->>'where',
			COALESCE(metadata->>'stt_provider', ''), COALESCE(metadata->>'host', ''), metadata::text
			FROM admin_audit_log
			WHERE action = 'call.privacy.audio_leaves_server' AND category = 'calls' ORDER BY seq`)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Close()
		var out []row
		for res.Next() {
			var r row
			if err := res.Scan(&r.actor, &r.where, &r.provider, &r.host, &r.meta); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}

	t.Run("an install whose env chose the browser keeps it and is asked once", func(t *testing.T) {
		t.Setenv("TRANSCRIPTION_MODE", "frontend")
		t.Setenv("STT_PROVIDER", "deepgram")
		s := read()
		if s.Mode != transcriptionBusiness.ModeFrontend || s.Where != transcriptionBusiness.WhereBrowser || !s.AudioLeavesServer || !s.NeedsAcknowledgement {
			t.Errorf("before the acknowledgement: %+v", s)
		}
		if where, reason := membersView(); where != transcriptionBusiness.WhereBrowser || reason != "" {
			t.Errorf("its captions stop before anyone acknowledged: %q %q", where, reason)
		}
		if code, why, _ := save(`{"acknowledge_where":"deepgram"}`); code != http.StatusConflict || why != "acknowledgement_required" {
			t.Errorf("an acknowledgement of somewhere else: %d %s", code, why)
		}
		if code, why, _ := save(`{"acknowledge_where":"browser"}`); code != http.StatusOK {
			t.Fatalf("the acknowledgement: %d %s", code, why)
		}
		if code, why, _ := save(`{"acknowledge_where":"browser"}`); code != http.StatusOK {
			t.Errorf("the same acknowledgement again: %d %s", code, why)
		}
		s = read()
		if s.Mode != transcriptionBusiness.ModeFrontend || s.NeedsAcknowledgement || s.AcknowledgedWhere != transcriptionBusiness.WhereBrowser {
			t.Errorf("after it: %+v", s)
		}
		if got := rows(); len(got) != 1 || got[0].actor != adminID.UserPostgresInfo.Id || got[0].where != transcriptionBusiness.WhereBrowser {
			t.Errorf("the rows: %+v, want one naming the admin and the browser", got)
		}
	})

	t.Run("nobody but an admin reads the choice or gives it", func(t *testing.T) {
		for _, c := range []struct {
			name   string
			routes http.Handler
			as     context.Context
			auth   string
			want   int
		}{
			{"a member", inApp, member, "", http.StatusForbidden},
			{"an API token", outside, ctx, "Bearer oc_" + uuid.NewString(), http.StatusUnauthorized},
			{"a call's guest", outside, ctx, "Bearer guest." + uuid.NewString(), http.StatusUnauthorized},
		} {
			if code, _ := call(c.routes, c.as, http.MethodGet, "/admin/transcription/config", "", c.auth); code != c.want {
				t.Errorf("%s reading: %d, want %d", c.name, code, c.want)
			}
			if code, _ := call(c.routes, c.as, http.MethodPost, "/admin/transcription/config", `{"mode":"frontend","acknowledge_where":"browser"}`, c.auth); code != c.want {
				t.Errorf("%s acknowledging: %d, want %d", c.name, code, c.want)
			}
		}
		if got := rows(); len(got) != 1 {
			t.Errorf("rows after the refusals: %d, want 1", len(got))
		}
	})

	t.Run("a fresh install keeps the audio on its server and says why captions are off", func(t *testing.T) {
		t.Setenv("TRANSCRIPTION_MODE", "")
		t.Setenv("STT_PROVIDER", "")
		s := read()
		if s.Mode != transcriptionBusiness.ModeBackend || s.STTProvider != transcriptionBusiness.STTLocal || s.Where != transcriptionBusiness.WhereServer || s.AudioLeavesServer || s.NeedsAcknowledgement {
			t.Errorf("a fresh install: %+v", s)
		}
		if s.CaptionsOffReason != transcriptionBusiness.ReasonTranscriberNotRunning {
			t.Errorf("no transcriber running: the admin's reason %q", s.CaptionsOffReason)
		}
		if where, reason := membersView(); where != transcriptionBusiness.WhereServer || reason != transcriptionBusiness.ReasonTranscriberNotRunning {
			t.Errorf("no transcriber running: a member sees %q %q", where, reason)
		}
		if err := transcriptionBusiness.RecordAgentHeartbeat(ctx, true); err != nil {
			t.Fatal(err)
		}
		if _, reason := membersView(); reason != "" {
			t.Errorf("with the transcriber running: %q", reason)
		}

		if code, why, where := save(`{"stt_provider":"deepgram"}`); code != http.StatusConflict || why != "acknowledgement_required" || where != transcriptionBusiness.STTDeepgram {
			t.Errorf("Deepgram without the acknowledgement: %d %s %s", code, why, where)
		}
		if s := read(); s.Where != transcriptionBusiness.WhereServer {
			t.Errorf("a refused save changed where the audio goes: %+v", s)
		}
		if code, why, _ := save(`{"stt_provider":"deepgram","acknowledge_where":"deepgram"}`); code != http.StatusOK {
			t.Fatalf("Deepgram with it: %d %s", code, why)
		}
		if code, why, _ := save(`{"stt_language":"en"}`); code != http.StatusOK {
			t.Errorf("a later save to the same place: %d %s", code, why)
		}
		if s := read(); s.Where != transcriptionBusiness.STTDeepgram || s.NeedsAcknowledgement || s.AcknowledgedWhere != transcriptionBusiness.STTDeepgram {
			t.Errorf("after Deepgram: %+v", s)
		}
		got := rows()
		if len(got) != 2 || got[1].where != transcriptionBusiness.STTDeepgram || got[1].actor != adminID.UserPostgresInfo.Id {
			t.Fatalf("the rows: %+v, want a second naming the admin and Deepgram", got)
		}
		if got[0].provider != "" || got[1].provider != transcriptionBusiness.STTDeepgram || got[1].host != "" {
			t.Errorf("the rows' providers: %+v, want none for the browser and Deepgram's for the server", got)
		}

		// An address typed for a custom endpoint (here in the env: the card's
		// SSRF guard resolves a typed URL, and a .test name never resolves):
		// the acknowledgement's row names its host, never the whole URL.
		t.Setenv("STT_BASE_URL", "https://stt.example.test/v1")
		if code, why, where := save(`{"stt_provider":"openai"}`); code != http.StatusConflict || why != "acknowledgement_required" || where != transcriptionBusiness.WhereCustom {
			t.Errorf("a typed address without the acknowledgement: %d %s %s", code, why, where)
		}
		if code, why, _ := save(`{"stt_provider":"openai","acknowledge_where":"custom"}`); code != http.StatusOK {
			t.Fatalf("a typed address with it: %d %s", code, why)
		}
		got = rows()
		if len(got) != 3 {
			t.Fatalf("the rows: %+v, want a third for the typed address", got)
		}
		if n := got[2]; n.where != transcriptionBusiness.WhereCustom || n.actor != adminID.UserPostgresInfo.Id ||
			n.provider != transcriptionBusiness.STTOpenAI || n.host != "stt.example.test" ||
			strings.Contains(n.meta, "https://") || strings.Contains(n.meta, "/v1") {
			t.Errorf("the typed address's row: %+v, want the admin, custom, openai and the host alone", n)
		}
	})
}
