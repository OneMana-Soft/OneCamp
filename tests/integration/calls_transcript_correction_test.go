//go:build integration
// +build integration

package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	transcriptExport "github.com/akashc777/OneCamp/business/TranscriptExport"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/opensearch-project/opensearch-go/v4"
	"github.com/opensearch-project/opensearch-go/v4/opensearchapi"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	v1api "github.com/akashc777/OneCamp/business/PublicAPI"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	recordingController "github.com/akashc777/OneCamp/controllers/Recording"
	_ "github.com/akashc777/OneCamp/controllers/V1"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/opensearchInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	customMiddleware "github.com/akashc777/OneCamp/middleware"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// zoom-05 through the real routes: who may export a call's transcript and
// correct a line of it, in the app and through a token (M1, M2, M3, M5, M9,
// M10, M11), and what the stored line keeps (M4).
func TestACallsTranscriptIsExportedAndCorrectedByTheRule(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	integration.SetupLiveKit(t)
	dg := integration.SetupDgraph(t)
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"took":1,"errors":false,"items":[]}`)
	}))
	t.Cleanup(search.Close)
	client, err := opensearchapi.NewClient(opensearchapi.Config{Client: opensearch.Config{Addresses: []string{search.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	opensearchInit.OpenSearchClient = client

	// person makes a member who signs in, and answers them as a request carries them.
	person := func(email, name string) userModels.UserInfo {
		t.Helper()
		id, _, err := userBusiness.CreateUserWithMethod(ctx, email, name, nil, userModels.AuthMethodEmail, false)
		if err != nil {
			t.Fatalf("make %s: %v", name, err)
		}
		people, err := userDomain.GetPeopleByUUIDs(ctx, []string{id.String()})
		if err != nil || len(people) != 1 {
			t.Fatalf("%s's graph node: %v", name, err)
		}
		var u userModels.UserInfo
		u.UserPostgresInfo.Id = id
		u.UserPostgresInfo.EmailID = email
		u.UserDgraphInfo = *people[0]
		return u
	}
	sam, maya, mod, out := person("sam@zoom05.test", "Sam Rivera"), person("maya@zoom05.test", "Maya Chen"),
		person("mod@zoom05.test", "Mo Dee"), person("out@zoom05.test", "Out Sider")

	launch := uuid.NewString()
	started := time.Now().Add(-2 * time.Hour).UTC()
	rec := func(blank, egress string, deleted bool) map[string]any {
		r := map[string]any{"uid": "_:" + blank, "dgraph.type": "Recording", "recording_egress_id": egress,
			"recording_stared_at": started.Format(time.RFC3339), "recording_ended_at": started.Add(30 * time.Minute).Format(time.RFC3339),
			"recording_obj_key": "recordings/" + egress + ".mp4", "recording_channel": map[string]any{"uid": "_:ch"}}
		if deleted {
			r["recording_deleted_at"] = time.Now().UTC().Format(time.RFC3339)
		}
		return r
	}
	seeded := dg.Mutate(t, []map[string]any{{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": launch, "ch_name": "launch",
		"ch_members":    []map[string]any{{"uid": sam.UserDgraphInfo.Uid}, {"uid": maya.UserDgraphInfo.Uid}, {"uid": mod.UserDgraphInfo.Uid}},
		"ch_moderators": []map[string]any{{"uid": mod.UserDgraphInfo.Uid}},
		"ch_recording":  []map[string]any{rec("r1", "EG_zoom05", false), rec("rd", "EG_zoom05del", true)}}})
	say := func(egress string, by userModels.UserInfo, text string, offset int64) string {
		t.Helper()
		ts := started.Add(time.Duration(offset) * time.Millisecond).UnixMilli()
		uid, err := recordingDomain.AttachTranscriptLine(ctx, egress, &dgraphStruct.DgraphTranscript{Text: text, TimeStamp: &ts, OffsetMs: &offset,
			From: &dgraphStruct.DgraphUser{Uid: by.UserDgraphInfo.Uid}})
		if err != nil || uid == "" {
			t.Fatalf("file a line under %s: %q %v", egress, uid, err)
		}
		return uid
	}
	line := say("EG_zoom05", sam, "the lunch budget is nineteen thousand", 4200)
	say("EG_zoom05", maya, "and we ship on friday", 9100)
	gone := say("EG_zoom05del", sam, "a deleted call's words", 700)

	// The read by key reads no more lines than asked, the first said first,
	// and still counts them all.
	if home, err := recordingDomain.ReaderTranscript(ctx, "EG_zoom05", sam.UserDgraphInfo.Uid, 1); err != nil || home == nil ||
		len(home.Transcript) != 1 || home.Transcript[0].OffsetMs == nil || *home.Transcript[0].OffsetMs != 4200 || home.TranscriptCount != 2 {
		t.Fatalf("the read by key, bounded to one line: %v %+v", err, home)
	}

	// app is the member routes, the asker put in the request as sign-in does.
	// counted, once set, measures every request app makes (zoom-05 C1).
	var counted *integration.StoreCalls
	app := func(who userModels.UserInfo, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		r := chi.NewRouter()
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				ctx := context.WithValue(req.Context(), helpers.UserInfoContextKey, who)
				if counted != nil {
					ctx = counted.Context(ctx)
				}
				next.ServeHTTP(w, req.WithContext(ctx))
			})
		})
		r.Get("/channel/getRecordingTranscript/{channel_uuid}/{egress_id}", channelController.GetChannelRecordingTranscript)
		r.Get("/calls/recordings/{key}/transcript", recordingController.HandleExportTranscript)
		r.Patch("/calls/recordings/{key}/transcript/lines/{line_uid}", recordingController.HandleCorrectTranscriptLine)
		var payload bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&payload).Encode(body)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(method, path, &payload))
		return w
	}
	correct := func(who userModels.UserInfo, egress, uid, text, was string) *httptest.ResponseRecorder {
		return app(who, http.MethodPatch, "/calls/recordings/"+egress+"/transcript/lines/"+uid, map[string]string{"text": text, "was": was})
	}
	stored := func() *dgraphStruct.DgraphTranscript {
		t.Helper()
		home, err := recordingDomain.ReaderTranscript(ctx, "EG_zoom05", sam.UserDgraphInfo.Uid, 10)
		if err != nil || home == nil {
			t.Fatalf("read the line back: %v", err)
		}
		for _, l := range home.Transcript {
			if l.Uid == line {
				return l
			}
		}
		t.Fatalf("line %s is gone", line)
		return nil
	}

	// M1: someone outside the channel gets the missing line's answer, for the
	// correction and the export alike, and never the words.
	for name, w := range map[string]*httptest.ResponseRecorder{
		"correct": correct(out, "EG_zoom05", line, "mine now", "the lunch budget is nineteen thousand"),
		"export":  app(out, http.MethodGet, "/calls/recordings/EG_zoom05/transcript?format=txt", nil),
	} {
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "lunch") {
			t.Fatalf("M1 %s by an outsider: %d %s", name, w.Code, w.Body)
		}
	}
	if w := app(out, http.MethodGet, "/channel/getRecordingTranscript/"+launch+"/EG_zoom05?pageIndex=0&pageSize=20&format=vtt", nil); w.Code == http.StatusOK || strings.Contains(w.Body.String(), "lunch") {
		t.Fatalf("M1 the channel's export to an outsider: %d %s", w.Code, w.Body)
	}

	// M2: a member who neither said it nor moderates is refused; nothing changes.
	if w := correct(maya, "EG_zoom05", line, "maya's words", "the lunch budget is nineteen thousand"); w.Code != http.StatusForbidden {
		t.Fatalf("M2 a member: %d %s", w.Code, w.Body)
	}
	if s := stored(); s.Text != "the lunch budget is nineteen thousand" || s.OriginalText != "" {
		t.Fatalf("M2 the refused correction wrote %+v", s)
	}
	// The speaker corrects it; the original is kept.
	if w := correct(sam, "EG_zoom05", line, "the launch budget is ninety thousand", "the lunch budget is nineteen thousand"); w.Code != http.StatusOK {
		t.Fatalf("M2 the speaker: %d %s", w.Code, w.Body)
	}
	if s := stored(); s.Text != "the launch budget is ninety thousand" || s.OriginalText != "the lunch budget is nineteen thousand" ||
		s.CorrectedAt == nil || s.CorrectedBy == nil || s.CorrectedBy.UserFullName != "Sam Rivera" || s.From == nil || s.From.Uuid != sam.UserPostgresInfo.Id.String() {
		t.Fatalf("M4 stored %+v", s)
	}
	// M3 + M14: a stale edit is a 409 saying who changed it and its words now.
	w := correct(mod, "EG_zoom05", line, "the moderator's words", "the lunch budget is nineteen thousand")
	if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "Sam Rivera") || !strings.Contains(w.Body.String(), "ninety thousand") {
		t.Fatalf("M3 a stale edit: %d %s", w.Code, w.Body)
	}
	// A moderator corrects it; the original stays the filed words (M4).
	if w := correct(mod, "EG_zoom05", line, "the launch budget is ninety-one thousand", "the launch budget is ninety thousand"); w.Code != http.StatusOK {
		t.Fatalf("M2 a moderator: %d %s", w.Code, w.Body)
	}
	if s := stored(); s.OriginalText != "the lunch budget is nineteen thousand" || s.CorrectedBy == nil || s.CorrectedBy.UserFullName != "Mo Dee" {
		t.Fatalf("M4 after a second correction %+v", s)
	}
	// A correction changes the words only: Sam still said it, at the time he said it.
	if s := stored(); s.From == nil || s.From.Uuid != sam.UserPostgresInfo.Id.String() || s.OffsetMs == nil || *s.OffsetMs != 4200 {
		t.Fatalf("M4 a moderator's correction changed who said it or when: %+v %+v", s, s.From)
	}

	// M7: the channel's own read exports every line, corrected, as a file.
	w = app(maya, http.MethodGet, "/channel/getRecordingTranscript/"+launch+"/EG_zoom05?pageIndex=0&pageSize=1&format=srt", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ninety-one thousand") || !strings.Contains(w.Body.String(), "friday") ||
		strings.Contains(w.Body.String(), "nineteen") || !strings.HasPrefix(w.Header().Get("Content-Disposition"), `attachment; filename="launch call `) {
		t.Fatalf("M7 the channel's export: %d %q %s", w.Code, w.Header().Get("Content-Disposition"), w.Body)
	}

	// zoom-05 C1: a read in another language is the same read plus at most one
	// Dgraph mutation; a kept line takes no model call, a corrected one is
	// translated again; the export names the model; an outsider's answer is
	// the one without ?lang=.
	var asked [][]string
	was := transcriptExport.RegisterTranslator(func(_ context.Context, _ *userModels.UserInfo, texts []string, _ string) ([]string, string, error) {
		asked = append(asked, texts)
		out := make([]string, len(texts))
		for i, s := range texts {
			out[i] = strings.ToUpper(s)
		}
		return out, "stub 1B", nil
	})
	t.Cleanup(func() { transcriptExport.RegisterTranslator(was) })
	calls := integration.CountStoreCalls(t, env, dg.Endpoint)
	counted = calls
	read := func(who userModels.UserInfo, path string) (*httptest.ResponseRecorder, int) {
		calls.Reset()
		w := app(who, http.MethodGet, path, nil)
		return w, calls.Trips() + calls.Graph()
	}
	page := "/channel/getRecordingTranscript/" + launch + "/EG_zoom05?pageIndex=0&pageSize=20"
	_, plain := read(maya, page)
	if plain == 0 {
		t.Fatal("C1 the plain read counted no store calls: the counter isn't on the request")
	}
	w, first := read(maya, page+"&lang=es")
	var flat bytes.Buffer // WriteJSON indents; the translation's object is matched whole
	_ = json.Compact(&flat, w.Body.Bytes())
	if w.Code != http.StatusOK || first != plain+1 || len(asked) != 1 || len(asked[0]) != 2 ||
		!strings.Contains(flat.String(), `"translation":{"language":"es","model":"stub 1B","pending":0}`) ||
		!strings.Contains(w.Body.String(), "THE LAUNCH BUDGET IS NINETY-ONE THOUSAND") || strings.Contains(w.Body.String(), "transcript_translations") {
		t.Fatalf("C1 a first read in es: %d, %d store calls against %d plain, model asked %v\n%s", w.Code, first, plain, asked, w.Body)
	}
	if w, again := read(maya, page+"&lang=es"); w.Code != http.StatusOK || again != plain || len(asked) != 2 || len(asked[1]) != 0 {
		t.Fatalf("C1 a kept read in es: %d, %d store calls against %d plain, model asked %v", w.Code, again, plain, asked)
	}
	// A correction's answers never carry the line's kept translations: not
	// the 409's line as it reads now, not the 200's corrected line.
	if w := correct(mod, "EG_zoom05", line, "a stale edit", "the lunch budget is nineteen thousand"); w.Code != http.StatusConflict ||
		strings.Contains(w.Body.String(), "transcript_translations") || strings.Contains(w.Body.String(), "NINETY-ONE") {
		t.Fatalf("C1 a stale correction after the es read: %d %s", w.Code, w.Body)
	}
	if w := correct(mod, "EG_zoom05", line, "the launch budget is ninety-two thousand", "the launch budget is ninety-one thousand"); w.Code != http.StatusOK ||
		strings.Contains(w.Body.String(), "transcript_translations") || strings.Contains(w.Body.String(), "NINETY-ONE") {
		t.Fatalf("C1 a correction: %d %s", w.Code, w.Body)
	}
	if w, _ := read(maya, page+"&lang=es"); len(asked) != 3 || strings.Join(asked[2], "|") != "the launch budget is ninety-two thousand" ||
		!strings.Contains(w.Body.String(), "NINETY-TWO") {
		t.Fatalf("C1 a corrected line goes again alone: asked %v\n%s", asked, w.Body)
	}
	if w := app(maya, http.MethodGet, "/calls/recordings/EG_zoom05/transcript?format=txt&lang=es", nil); w.Code != http.StatusOK ||
		!strings.Contains(w.Body.String(), "Translated by stub 1B into es.") || !strings.Contains(w.Body.String(), "AND WE SHIP ON FRIDAY") {
		t.Fatalf("C1 the export in es: %d\n%s", w.Code, w.Body)
	}
	for _, p := range []string{page, "/calls/recordings/EG_zoom05/transcript?format=txt"} {
		plainW, langW := app(out, http.MethodGet, p, nil), app(out, http.MethodGet, p+"&lang=es", nil)
		if plainW.Code != langW.Code || plainW.Body.String() != langW.Body.String() || langW.Code == http.StatusOK {
			t.Fatalf("C1 an outsider in es: %d %s, without: %d %s", langW.Code, langW.Body, plainW.Code, plainW.Body)
		}
	}
	// The line goes back to its words before C1, which the checks after it read.
	if w := correct(mod, "EG_zoom05", line, "the launch budget is ninety-one thousand", "the launch budget is ninety-two thousand"); w.Code != http.StatusOK {
		t.Fatalf("C1 the line back: %d %s", w.Code, w.Body)
	}
	// KeepTranslations writes only while every line it names is still a line:
	// one purged since the read is never written back as a node holding only
	// its translation (nor is the batch it came in); a live one is written.
	rp := dg.Mutate(t, []map[string]any{{"uid": "_:rp", "dgraph.type": "Recording", "recording_egress_id": "EG_zoom05purge",
		"recording_stared_at": started.Format(time.RFC3339)}})["rp"]
	purged, live := say("EG_zoom05purge", sam, "words purged after the read", 100), say("EG_zoom05purge", maya, "words still kept", 200)
	if err := recordingDomain.DeleteRecordingNodes(ctx, recordingDomain.ArchivedRecording{Uid: rp, EgressId: "EG_zoom05purge", TranscriptUids: []string{purged}}); err != nil {
		t.Fatalf("C1 purge: %v", err)
	}
	blob := `{"es":{"t":"PALABRAS","m":"stub 1B","h":"h"}}`
	translationsOf := func() string {
		return string(dg.Query(t, `{ q(func: uid(`+purged+`, `+live+`)) { uid transcript_translations } }`))
	}
	_ = recordingDomain.KeepTranslations(ctx, []recordingDomain.KeptTranslation{{LineUid: purged, Translations: blob}, {LineUid: live, Translations: blob}})
	if got := translationsOf(); strings.Contains(got, "PALABRAS") {
		t.Fatalf("C1 a batch with a purged line was written: %s", got)
	}
	if err := recordingDomain.KeepTranslations(ctx, []recordingDomain.KeptTranslation{{LineUid: live, Translations: blob}}); err != nil ||
		strings.Count(translationsOf(), "PALABRAS") != 1 {
		t.Fatalf("C1 a live line's translation: %v %s", err, translationsOf())
	}

	// M5: a deleted recording's lines are neither corrected nor exported.
	if w := correct(sam, "EG_zoom05del", gone, "fixed", "a deleted call's words"); w.Code != http.StatusNotFound {
		t.Fatalf("M5 correct: %d %s", w.Code, w.Body)
	}
	if w := app(sam, http.MethodGet, "/calls/recordings/EG_zoom05del/transcript?format=vtt", nil); w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "deleted call") {
		t.Fatalf("M5 export: %d %s", w.Code, w.Body)
	}

	// M9: a uid of another kind (the recording, the channel, a line of another
	// recording) is the missing line's answer.
	for _, uid := range []string{seeded["r1"], seeded["ch"], gone, "not-a-uid"} {
		if w := correct(mod, "EG_zoom05", uid, "x", "y"); w.Code != http.StatusNotFound {
			t.Fatalf("M9 %s: %d %s", uid, w.Code, w.Body)
		}
	}

	// M11: each correction made is a row in the audit log, before and after.
	var rows int
	if err := env.PG.QueryRow(`SELECT count(*) FROM admin_audit_log WHERE action = 'transcript_line_corrected'
		AND metadata->>'line' = $1 AND metadata->>'after' = 'the launch budget is ninety thousand'
		AND metadata->>'before' = 'the lunch budget is nineteen thousand'`, line).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("M11 audit rows %d: %v", rows, err)
	}

	// M10: through the public API's own chain. A read-only token exports and
	// can't correct; a write token corrects; an agent switched off does neither.
	agent := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by) VALUES ($1, 'Scribe', $2)`, agent, sam.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	token := func(name string, agentID *uuid.UUID, scopes ...string) string {
		t.Helper()
		tk, err := apiTokenBusiness.CreateToken(ctx, name, scopes, 30, sam.UserPostgresInfo.Id, agentID, false)
		if err != nil {
			t.Fatalf("make %s: %v", name, err)
		}
		return tk.Plaintext
	}
	reader := token("reader", nil, apiTokenBusiness.ScopeMessagesRead)
	writer := token("writer", nil, apiTokenBusiness.ScopeMessagesRead, apiTokenBusiness.ScopeMessagesWrite)
	bound := token("scribe", &agent, apiTokenBusiness.ScopeMessagesRead, apiTokenBusiness.ScopeMessagesWrite)
	api := chi.NewRouter()
	api.Use(customMiddleware.VerifyApiToken)
	v1api.Mount(api, v1api.Middleware{RequireScope: customMiddleware.RequireScope})
	v1 := func(tk, method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var payload bytes.Buffer
		if body != nil {
			_ = json.NewEncoder(&payload).Encode(body)
		}
		req := httptest.NewRequest(method, path, &payload)
		req.Header.Set("Authorization", "Bearer "+tk)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		api.ServeHTTP(w, req)
		return w
	}
	exportPath, linePath := "/calls/recordings/EG_zoom05/transcript?format=vtt", "/calls/recordings/EG_zoom05/transcript/lines/"+line
	if w := v1(reader, http.MethodGet, exportPath, nil); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ninety-one") {
		t.Fatalf("M10 a read-only token's export: %d %s", w.Code, w.Body)
	}
	if w := v1(reader, http.MethodPatch, linePath, map[string]string{"text": "by token", "was": "the launch budget is ninety-one thousand"}); w.Code != http.StatusForbidden {
		t.Fatalf("M10 a read-only token's correction: %d %s", w.Code, w.Body)
	}
	if w := v1(writer, http.MethodPatch, linePath, map[string]string{"text": "by token", "was": "the launch budget is ninety-one thousand"}); w.Code != http.StatusOK {
		t.Fatalf("M10 a write token's correction (Sam said it): %d %s", w.Code, w.Body)
	}
	if _, err := env.PG.Exec(`UPDATE ai_agents SET is_active = false WHERE id = $1`, agent); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ method, path string }{{http.MethodGet, exportPath}, {http.MethodPatch, linePath}} {
		if w := v1(bound, c.method, c.path, map[string]string{"text": "agent", "was": "by token"}); w.Code != http.StatusUnauthorized || strings.Contains(w.Body.String(), "by token") {
			t.Fatalf("M10 a switched-off agent's %s: %d %s", c.method, w.Code, w.Body)
		}
	}
	if s := stored(); s.Text != "by token" {
		t.Fatalf("M10 the switched-off agent changed the line: %+v", s)
	}

	// A call's transcript reads follow the one live-call rule: while
	// a call goes on in the channel, begun before this recording, its lines
	// are for the people connected to it. Maya, Sam's token and the moderator
	// are connected to none: the missing line's answer, never its words.
	if _, err := env.PG.Exec(`INSERT INTO call_sessions (id, room, kind, resource_kind, resource_id, started_at) VALUES ($1, $2, 'huddle', 'channel', $2, $3)`,
		uuid.New(), launch, started.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"app export": app(maya, http.MethodGet, "/calls/recordings/EG_zoom05/transcript?format=txt", nil),
		"v1 export":  v1(reader, http.MethodGet, exportPath, nil),
		"correction": correct(mod, "EG_zoom05", line, "the moderator's words", "the launch budget is ninety-one thousand"),
	} {
		if w.Code != http.StatusNotFound || strings.Contains(w.Body.String(), "by token") {
			t.Errorf("M15 %s while the call goes on, not connected: %d", name, w.Code)
		}
	}
}
