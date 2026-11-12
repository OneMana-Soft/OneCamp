//go:build integration

package integration_test

// CAL53-G15 head A: the calendar on the public API, its webhooks and its MCP
// tools, against real Postgres 12, Dgraph and Redis. A token acts as its
// member through the calendar's own rules: Ana's token with calendar:read
// lists her events; Ben's, who sees Ana's calendar only as busy, gets Busy
// blocks and never a title or notes; a private event of Ana's is 404 to him
// by id; calendar:read can't write (403 in words); a token with no calendar
// scope reads nothing; a revoked token and a killed agent's are refused, on
// /v1 and on MCP; create, change and delete land on the calendar, the delete
// in the trash and brought back; event.created reaches a webhook busy-shaped
// (no title, no notes), and only one whose owner may see the event.
//
// Run: go test -tags=integration ./tests/integration/ -run '^TestCAL53G15CalendarAPI$' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	apiTokenBusiness "github.com/akashc777/OneCamp/business/ApiToken"
	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	mcpBusiness "github.com/akashc777/OneCamp/business/MCPServer"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	webhookBusiness "github.com/akashc777/OneCamp/business/Webhook"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
	"github.com/google/uuid"
)

func TestCAL53G15CalendarAPI(t *testing.T) {
	t.Setenv("CALENDAR_STORE", "postgres")
	t.Setenv("JWT_SECRET", "cal53-g15-integration-secret")
	t.Setenv("ALLOWED_USERS", "")
	env, dg := calendarStores(t)
	integration.SetupRedis(t)
	ctx := context.Background()
	settingsBusiness.ForgetSettingsForTest()
	t.Cleanup(settingsBusiness.ForgetSettingsForTest)

	var mu sync.Mutex
	sent := map[uuid.UUID][]map[string]any{}
	t.Cleanup(webhookBusiness.ObserveDeliveriesForTest(func(id uuid.UUID, kind string, payload []byte) {
		var body struct {
			Data map[string]any `json:"data"`
		}
		_ = json.Unmarshal(payload, &body)
		body.Data["~kind"], body.Data["~raw"] = kind, string(payload)
		mu.Lock()
		sent[id] = append(sent[id], body.Data)
		mu.Unlock()
	}))

	ana := seedCalendarPerson(t, env.PG, dg, "Ana")
	ben := seedCalendarPerson(t, env.PG, dg, "Ben")
	hook := func(owner uuid.UUID, kinds string) uuid.UUID {
		t.Helper()
		id := uuid.New()
		if _, err := env.PG.Exec(`INSERT INTO webhooks (id, name, type, token_hash, target_url, events, created_by, scope_type) VALUES ($1, $2, 'outgoing', $3, $4, $5, $6, 'org')`,
			id, "hook-"+id.String()[:8], "hash-"+id.String(), "https://hooks.example.com/"+id.String(), kinds, owner); err != nil {
			t.Fatal(err)
		}
		return id
	}
	anasHook, bensHook := hook(ana.UserPostgresInfo.Id, `["event.created"]`), hook(ben.UserPostgresInfo.Id, `["event.created"]`)
	webhookBusiness.ForgetDispatchCacheForTest()

	events := calendarBusiness.Events
	start := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Hour)
	at := func(h int) calendarBusiness.When {
		x := start.Add(time.Duration(h) * time.Hour)
		return calendarBusiness.When{At: &x, Zone: "UTC"}
	}
	const notes = "salary review notes g15"
	open, err := events.Create(ctx, ana, calendarBusiness.EventInput{Title: "Roadmap sync g15", Notes: notes, Start: at(0), End: at(1)})
	if err != nil {
		t.Fatalf("Ana's event: %v", err)
	}
	private, err := events.Create(ctx, ana, calendarBusiness.EventInput{Title: "Doctor g15", Notes: notes, Start: at(3), End: at(4), Visibility: "private"})
	if err != nil {
		t.Fatalf("Ana's private event: %v", err)
	}
	anaCal, err := events.PersonCalendar(ctx, ana, ana.UserDgraphInfo.Uuid)
	if err != nil {
		t.Fatal(err)
	}

	token := func(who uuid.UUID, agent *uuid.UUID, scopes ...string) *apiTokenBusiness.CreatedToken {
		t.Helper()
		tok, err := apiTokenBusiness.CreateToken(ctx, "g15", scopes, 30, who, agent, false)
		if err != nil {
			t.Fatalf("a token with %v: %v", scopes, err)
		}
		return tok
	}
	read, write := apiTokenBusiness.ScopeCalendarRead, apiTokenBusiness.ScopeCalendarWrite
	anaTok := token(ana.UserPostgresInfo.Id, nil, read, write)
	anaReadOnly := token(ana.UserPostgresInfo.Id, nil, read)
	benTok := token(ben.UserPostgresInfo.Id, nil, read)
	benWrite := token(ben.UserPostgresInfo.Id, nil, read, write)
	noScope := token(ana.UserPostgresInfo.Id, nil, apiTokenBusiness.ScopeTasksRead)
	revoked := token(ana.UserPostgresInfo.Id, nil, read)
	if err := apiTokenBusiness.RevokeToken(ctx, revoked.Token.Id, ana.UserPostgresInfo.Id); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	agent := uuid.New()
	if _, err := env.PG.Exec(`INSERT INTO ai_agents (id, name, created_by, trigger_type, dm_able, ambient) VALUES ($1, 'Agent', $2, 'mention', true, true)`, agent, ana.UserPostgresInfo.Id); err != nil {
		t.Fatal(err)
	}
	agentTok := token(ana.UserPostgresInfo.Id, &agent, read, write)

	routes := router.Routes()
	send := func(tok *apiTokenBusiness.CreatedToken, method, path string, body any) (int, string) {
		t.Helper()
		var rd io.Reader
		if body != nil {
			raw, _ := json.Marshal(body)
			rd = bytes.NewReader(raw)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("Authorization", "Bearer "+tok.Plaintext)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		routes.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	window := "from=" + start.Add(-time.Hour).Format(time.RFC3339) + "&to=" + start.Add(6*time.Hour).Format(time.RFC3339)

	t.Run("a member's token lists her events, whole", func(t *testing.T) {
		code, out := send(anaTok, http.MethodGet, "/v1/events?"+window, nil)
		if code != 200 || !strings.Contains(out, open.ID) || !strings.Contains(out, "Roadmap sync g15") || !strings.Contains(out, private.ID) {
			t.Fatalf("Ana's list: %d %s", code, out)
		}
		if code, out := send(anaTok, http.MethodGet, "/v1/events/"+open.ID, nil); code != 200 || !strings.Contains(out, notes) {
			t.Errorf("Ana reads her own event: %d %s", code, out)
		}
		if code, out := send(anaTok, http.MethodGet, "/v1/calendars", nil); code != 200 || !strings.Contains(out, anaCal.String()) {
			t.Errorf("Ana's calendars: %d %s", code, out)
		}
	})
	t.Run("a busy-only reader sees Busy blocks, never a title or notes", func(t *testing.T) {
		code, out := send(benTok, http.MethodGet, "/v1/events?"+window+"&calendar_id="+anaCal.String(), nil)
		if code != 200 || !strings.Contains(out, "Busy") || !strings.Contains(out, open.ID) {
			t.Fatalf("Ben's view of Ana's calendar: %d %s", code, out)
		}
		for _, secret := range []string{"Roadmap sync g15", "Doctor g15", notes} {
			if strings.Contains(out, secret) {
				t.Errorf("Ben's busy-only view carries %q: %s", secret, out)
			}
		}
	})
	t.Run("a private event is 404 to a non-attendee", func(t *testing.T) {
		code, out := send(benTok, http.MethodGet, "/v1/events/"+private.ID, nil)
		if code != http.StatusNotFound || strings.Contains(out, "Doctor g15") || strings.Contains(out, notes) {
			t.Errorf("Ben reads Ana's private event: %d %s", code, out)
		}
	})
	t.Run("calendar:read can't write, in words", func(t *testing.T) {
		code, out := send(anaReadOnly, http.MethodPost, "/v1/events", map[string]any{"title": "Not allowed g15", "start": at(5), "end": at(6)})
		if code != http.StatusForbidden || !strings.Contains(out, "calendar:write") {
			t.Errorf("a calendar:read token wrote: %d %s", code, out)
		}
	})
	// find_time's schema says what the Availability Engine takes: a meeting
	// of 15 minutes to 8 hours, between two times (V-787).
	t.Run("find_time's schema says what the engine takes", func(t *testing.T) {
		from := start.Add(24 * time.Hour).Format(time.RFC3339)
		to := start.Add(72 * time.Hour).Format(time.RFC3339)
		for _, c := range []struct {
			body map[string]any
			says string
		}{
			{map[string]any{"duration_minutes": 5, "from": from, "to": to}, "duration_minutes is at least 15"},
			{map[string]any{"duration_minutes": 600, "from": from, "to": to}, "duration_minutes is at most 480"},
			{map[string]any{"duration_minutes": 30, "to": to}, "from is required"},
			{map[string]any{"duration_minutes": 30, "from": from}, "to is required"},
		} {
			if code, out := send(anaTok, http.MethodPost, "/v1/events/find-time", c.body); code != http.StatusBadRequest || !strings.Contains(out, c.says) {
				t.Errorf("find-time %v: %d %s, want 400 saying %q", c.body, code, out, c.says)
			}
		}
		if code, out := send(anaTok, http.MethodPost, "/v1/events/find-time", map[string]any{"duration_minutes": 30, "from": from, "to": to}); code != 200 || !strings.Contains(out, `"suggestions"`) {
			t.Errorf("find-time for 30 minutes: %d %s", code, out)
		}
	})
	t.Run("a token without calendar:read reads nothing", func(t *testing.T) {
		for _, path := range []string{"/v1/events?" + window, "/v1/events/" + open.ID, "/v1/calendars"} {
			if code, out := send(noScope, http.MethodGet, path, nil); code != http.StatusForbidden || !strings.Contains(out, "calendar:read") || strings.Contains(out, "Roadmap") {
				t.Errorf("%s with no calendar scope: %d %s", path, code, out)
			}
		}
	})
	t.Run("create, change and delete land on the calendar; the delete is soft", func(t *testing.T) {
		code, out := send(anaTok, http.MethodPost, "/v1/events", map[string]any{"title": "Made by API g15", "start": at(5), "end": at(6)})
		var made calendarBusiness.Event
		if err := json.Unmarshal([]byte(out), &made); code != http.StatusCreated || err != nil || made.ID == "" {
			t.Fatalf("create: %d %s", code, out)
		}
		if e, err := events.Get(ctx, ana, made.ID); err != nil || e.Title != "Made by API g15" {
			t.Fatalf("the made event on Ana's calendar: %+v %v", e, err)
		}
		if code, out := send(anaTok, http.MethodPatch, "/v1/events/"+made.ID, map[string]any{"title": "Renamed g15", "sequence": made.Sequence}); code != 200 || !strings.Contains(out, "Renamed g15") {
			t.Fatalf("patch: %d %s", code, out)
		}
		if code, out := send(benTok, http.MethodDelete, "/v1/events/"+made.ID, nil); code != http.StatusForbidden && code != http.StatusNotFound {
			t.Errorf("Ben's read token deleted Ana's event: %d %s", code, out)
		}
		if code, out := send(anaTok, http.MethodDelete, "/v1/events/"+made.ID, nil); code != 200 {
			t.Fatalf("delete: %d %s", code, out)
		}
		var deletedAt *time.Time
		if err := env.PG.QueryRow(`SELECT deleted_at FROM events WHERE id = $1`, made.ID).Scan(&deletedAt); err != nil || deletedAt == nil {
			t.Errorf("the deleted event's row: deleted_at %v (%v), want it kept and marked", deletedAt, err)
		}
		if code, out := send(anaTok, http.MethodPost, "/v1/events/"+made.ID+"/restore", nil); code != http.StatusCreated {
			t.Errorf("restore: %d %s", code, out)
		}
		if _, err := events.Get(ctx, ana, made.ID); err != nil {
			t.Errorf("the restored event: %v", err)
		}
	})
	t.Run("a revoked token and a killed agent are refused", func(t *testing.T) {
		if code, out := send(revoked, http.MethodGet, "/v1/events?"+window, nil); code != http.StatusUnauthorized {
			t.Errorf("a revoked token: %d %s", code, out)
		}
		if code, out := send(agentTok, http.MethodGet, "/v1/events?"+window, nil); code != 200 {
			t.Fatalf("the agent's token while it is live: %d %s", code, out)
		}
		if _, err := env.PG.Exec(`UPDATE ai_agents SET is_active = false WHERE id = $1`, agent); err != nil {
			t.Fatal(err)
		}
		if code, out := send(agentTok, http.MethodGet, "/v1/events?"+window, nil); code != http.StatusUnauthorized || strings.Contains(out, "Roadmap") {
			t.Errorf("a killed agent's token read: %d %s", code, out)
		}
		if code, out := send(agentTok, http.MethodPost, "/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": "list_events", "arguments": map[string]any{"from": start.Format(time.RFC3339), "to": start.Add(6 * time.Hour).Format(time.RFC3339)}}}); code != http.StatusUnauthorized || strings.Contains(out, "Roadmap") {
			t.Errorf("a killed agent's token over MCP: %d %s", code, out)
		}
	})
	t.Run("event.created reaches a webhook busy-shaped, only for an owner who may see it", func(t *testing.T) {
		made, err := events.Create(ctx, ana, calendarBusiness.EventInput{Title: "Hooked g15", Notes: notes, Start: at(7), End: at(8)})
		if err != nil {
			t.Fatal(err)
		}
		var got map[string]any
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && got == nil; time.Sleep(100 * time.Millisecond) {
			mu.Lock()
			for _, d := range sent[anasHook] {
				if d["event_id"] == made.ID {
					got = d
				}
			}
			mu.Unlock()
		}
		if got == nil || got["~kind"] != "event.created" {
			t.Fatalf("Ana's webhook was sent no event.created for %s: %v", made.ID, sent[anasHook])
		}
		for _, secret := range []string{"Hooked g15", notes} {
			if strings.Contains(got["~raw"].(string), secret) {
				t.Errorf("the webhook carries %q: %s", secret, got["~raw"])
			}
		}
		if got["start"] == nil || got["calendar_id"] != anaCal.String() {
			t.Errorf("the webhook names no busy time or calendar: %v", got)
		}
		// Ben sees Ana's calendar as busy: he is sent the busy shape too.
		mu.Lock()
		bens := len(sent[bensHook])
		mu.Unlock()
		if bens == 0 {
			t.Errorf("Ben, who sees Ana's calendar as busy, was sent nothing")
		}
		// Free time isn't shown to someone who sees only busy: Ana's webhook is
		// sent her free event, Ben's isn't (filtered per subscriber).
		free, err := events.Create(ctx, ana, calendarBusiness.EventInput{Title: "Free g15", Start: at(11), End: at(12), Transparency: "transparent"})
		if err != nil {
			t.Fatal(err)
		}
		toAna := false
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline) && !toAna; time.Sleep(100 * time.Millisecond) {
			mu.Lock()
			for _, d := range sent[anasHook] {
				toAna = toAna || d["event_id"] == free.ID
			}
			mu.Unlock()
		}
		mu.Lock()
		defer mu.Unlock()
		for _, d := range sent[bensHook] {
			if d["event_id"] == free.ID {
				t.Errorf("Ben, who sees Ana's calendar only as busy, was sent her free time: %v", d)
			}
		}
		if !toAna {
			t.Errorf("Ana's webhook was sent no event.created for her free event")
		}
	})
	// mcpOn turns MCP on with the calendar's tools, in the AI edition only.
	ai := helpers.FeatureRegistered(helpers.FeatureNameAI)
	mcpOn := func(t *testing.T) {
		t.Helper()
		if err := mcpBusiness.SetAdmission(ctx, true, "*"); err != nil {
			t.Fatalf("turn MCP on: %v", err)
		}
		for _, register := range []func() error{mcpBusiness.RegisterBridgedTools, mcpBusiness.PublishOps} {
			if err := register(); err != nil && !strings.Contains(err.Error(), "already registered") {
				t.Fatalf("register the MCP tools: %v", err)
			}
		}
	}
	tool := func(tok *apiTokenBusiness.CreatedToken, name string, args map[string]any) (int, string) {
		return send(tok, http.MethodPost, "/v1/mcp", map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
			"params": map[string]any{"name": name, "arguments": args}})
	}
	// toolOK is whether an MCP call was answered and not refused: a governed
	// refusal (scope, reach) is a JSON-RPC error, the op's own a result with
	// isError.
	toolOK := func(code int, out string) bool {
		var env struct {
			Error  json.RawMessage `json:"error"`
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		err := json.Unmarshal([]byte(out), &env)
		return code == 200 && err == nil && (len(env.Error) == 0 || string(env.Error) == "null") && !env.Result.IsError
	}
	t.Run("MCP: list_events and create_event act as Ana", func(t *testing.T) {
		if !ai {
			t.Skip("MCP is the AI edition's")
		}
		mcpOn(t)
		span := map[string]any{"from": start.Add(-time.Hour).Format(time.RFC3339), "to": start.Add(9 * time.Hour).Format(time.RFC3339)}
		code, out := tool(anaTok, "list_events", span)
		if !toolOK(code, out) || !strings.Contains(out, "Roadmap sync g15") {
			t.Fatalf("Ana's list_events: %d %s", code, out)
		}
		code, out = tool(anaTok, "create_event", map[string]any{"title": "Made by MCP g15", "start": at(9), "end": at(10)})
		if !toolOK(code, out) {
			t.Fatalf("Ana's create_event: %d %s", code, out)
		}
		list, _, err := events.Range(ctx, ana, start, start.Add(12*time.Hour))
		found := false
		for _, e := range list {
			found = found || e.Title == "Made by MCP g15"
		}
		if err != nil || !found {
			t.Errorf("the MCP-made event isn't on Ana's calendar (%v)", err)
		}
		if code, out := tool(anaReadOnly, "create_event", map[string]any{"title": "Read-only MCP g15", "start": at(9), "end": at(10)}); toolOK(code, out) {
			t.Errorf("a calendar:read token made an event over MCP: %d %s", code, out)
		}
		if code, out := tool(benTok, "get_event", map[string]any{"id": private.ID}); strings.Contains(out, "Doctor g15") || strings.Contains(out, notes) {
			t.Errorf("Ben read Ana's private event over MCP: %d %s", code, out)
		}
	})
	// A deleted event is in the trash, not on the calendar: no read answers it,
	// through /v1 or MCP, and its webhook says deleted, until restore_event.
	t.Run("a deleted event is refused by every read and its webhook says deleted, until restore_event", func(t *testing.T) {
		trashHook := hook(ana.UserPostgresInfo.Id, `["event.deleted","event.restored"]`)
		webhookBusiness.ForgetDispatchCacheForTest()
		if ai {
			mcpOn(t)
		}
		made, err := events.Create(ctx, ana, calendarBusiness.EventInput{Title: "Trashed g15", Notes: notes, Start: at(13), End: at(14)})
		if err != nil {
			t.Fatal(err)
		}
		span := "from=" + start.Format(time.RFC3339) + "&to=" + start.Add(15*time.Hour).Format(time.RFC3339)
		reads := func(when string, live bool) {
			t.Helper()
			code, out := send(anaTok, http.MethodGet, "/v1/events/"+made.ID, nil)
			if live && (code != 200 || !strings.Contains(out, "Trashed g15")) || !live && (code != http.StatusNotFound || strings.Contains(out, "Trashed g15")) {
				t.Errorf("%s: GET /v1/events/{id} answered %d %s", when, code, out)
			}
			if code, out := send(anaTok, http.MethodGet, "/v1/events?"+span, nil); code != 200 || strings.Contains(out, made.ID) != live {
				t.Errorf("%s: list_events answered %d, the event listed %v, want %v: %s", when, code, strings.Contains(out, made.ID), live, out)
			}
			if !ai {
				return
			}
			code, out = tool(anaTok, "get_event", map[string]any{"id": made.ID})
			if got := toolOK(code, out) && strings.Contains(out, "Trashed g15"); got != live || !live && strings.Contains(out, notes) {
				t.Errorf("%s: MCP get_event answered %d %s", when, code, out)
			}
		}
		delivered := func(kind string) map[string]any {
			t.Helper()
			for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
				mu.Lock()
				for _, d := range sent[trashHook] {
					if d["event_id"] == made.ID && d["~kind"] == kind {
						mu.Unlock()
						return d
					}
				}
				mu.Unlock()
			}
			t.Errorf("Ana's webhook was sent no %s for %s: %v", kind, made.ID, sent[trashHook])
			return nil
		}

		reads("live", true)
		if code, out := send(anaTok, http.MethodDelete, "/v1/events/"+made.ID, nil); code != 200 {
			t.Fatalf("delete: %d %s", code, out)
		}
		reads("in the trash", false)
		if d := delivered("event.deleted"); d != nil {
			for _, secret := range []string{"Trashed g15", notes} {
				if strings.Contains(d["~raw"].(string), secret) {
					t.Errorf("the event.deleted webhook carries %q: %s", secret, d["~raw"])
				}
			}
		}
		mu.Lock()
		for _, d := range sent[trashHook] {
			if d["event_id"] == made.ID && d["~kind"] != "event.deleted" {
				t.Errorf("while in the trash the event went out as %v, not deleted: %v", d["~kind"], d)
			}
		}
		mu.Unlock()

		if ai {
			if code, out := tool(anaTok, "restore_event", map[string]any{"id": made.ID}); !toolOK(code, out) {
				t.Fatalf("MCP restore_event: %d %s", code, out)
			}
		} else if code, out := send(anaTok, http.MethodPost, "/v1/events/"+made.ID+"/restore", nil); code != http.StatusCreated {
			t.Fatalf("restore: %d %s", code, out)
		}
		reads("restored", true)
		delivered("event.restored")
	})

	// MCP's scope check is its own: a token without calendar:read is refused
	// every calendar tool in words, before the op runs, and no event comes
	// back.
	t.Run("MCP: a token without calendar:read is refused, in words", func(t *testing.T) {
		if !ai {
			t.Skip("MCP is the AI edition's")
		}
		mcpOn(t)
		span := map[string]any{"from": start.Add(-time.Hour).Format(time.RFC3339), "to": start.Add(6 * time.Hour).Format(time.RFC3339)}
		for name, args := range map[string]map[string]any{"list_events": span, "get_event": {"id": open.ID}, "list_calendars": {}} {
			if code, out := tool(noScope, name, args); toolOK(code, out) || !strings.Contains(out, "scope") || strings.Contains(out, "Roadmap") || strings.Contains(out, notes) {
				t.Errorf("%s with no calendar scope: %d %s", name, code, out)
			}
		}
	})

	// One rule per action, the calendar's own, and the same answer on /v1 and
	// over MCP: an edit follows the event's guest permission (mayEdit), and a
	// cancel and a restore are the organiser's. Ben, with calendar:write, is a
	// guest on two of Ana's events: one whose guests may change it, one whose
	// may not. The scope never refuses him first; the calendar does, in its
	// words.
	guestPair := func(t *testing.T, name string, h int) (shut, mod *calendarBusiness.Event) {
		t.Helper()
		no, yes := false, true
		for _, may := range []*bool{&no, &yes} {
			e, err := events.Create(ctx, ana, calendarBusiness.EventInput{Title: name + " g15", Start: at(h), End: at(h + 1), Notify: "none",
				Attendees: []calendarBusiness.GuestInput{{UUID: ben.UserPostgresInfo.Id.String()}}, GuestsCanModify: may})
			if err != nil {
				t.Fatalf("Ana's event with Ben on it (guests may change it: %v): %v", *may, err)
			}
			if *may {
				mod = e
			} else {
				shut = e
			}
		}
		return shut, mod
	}
	title := func(id string) string {
		e, err := events.Get(ctx, ana, id)
		if err != nil {
			return "(" + err.Error() + ")"
		}
		return e.Title
	}
	live := func(id string) bool {
		_, err := events.Get(ctx, ana, id)
		return err == nil
	}
	const cantChange, cantDelete = "Only the person who made this event can change it.", "Only the person who made this event can delete it."
	t.Run("guest writes: PATCH /v1 follows the event's guest permission", func(t *testing.T) {
		shut, mod := guestPair(t, "Patch", 16)
		if code, out := send(benWrite, http.MethodPatch, "/v1/events/"+shut.ID, map[string]any{"title": "Ben's patch g15", "sequence": shut.Sequence}); code != http.StatusForbidden || !strings.Contains(out, cantChange) {
			t.Errorf("a guest without modify: PATCH answered %d %s", code, out)
		}
		if got := title(shut.ID); got != "Patch g15" {
			t.Errorf("a guest without modify changed the event to %q", got)
		}
		if code, out := send(benWrite, http.MethodPatch, "/v1/events/"+mod.ID, map[string]any{"title": "Ben's patch g15", "sequence": mod.Sequence}); code != 200 || !strings.Contains(out, "Ben's patch g15") {
			t.Errorf("a guest with modify: PATCH answered %d %s", code, out)
		}
		if got := title(mod.ID); got != "Ben's patch g15" {
			t.Errorf("a guest with modify: the event reads %q", got)
		}
	})
	t.Run("guest writes: MCP update_event follows the event's guest permission", func(t *testing.T) {
		if !ai {
			t.Skip("MCP is the AI edition's")
		}
		mcpOn(t)
		shut, mod := guestPair(t, "Update", 17)
		if code, out := tool(benWrite, "update_event", map[string]any{"id": shut.ID, "title": "Ben's update g15", "sequence": shut.Sequence}); toolOK(code, out) || !strings.Contains(out, cantChange) {
			t.Errorf("a guest without modify: update_event answered %d %s", code, out)
		}
		if got := title(shut.ID); got != "Update g15" {
			t.Errorf("a guest without modify changed the event to %q", got)
		}
		if code, out := tool(benWrite, "update_event", map[string]any{"id": mod.ID, "title": "Ben's update g15", "sequence": mod.Sequence}); !toolOK(code, out) {
			t.Errorf("a guest with modify: update_event answered %d %s", code, out)
		}
		if got := title(mod.ID); got != "Ben's update g15" {
			t.Errorf("a guest with modify: the event reads %q", got)
		}
	})
	t.Run("guest writes: DELETE /v1 is the organiser's", func(t *testing.T) {
		shut, mod := guestPair(t, "Delete", 18)
		for _, e := range []*calendarBusiness.Event{shut, mod} {
			if code, out := send(benWrite, http.MethodDelete, "/v1/events/"+e.ID, nil); code != http.StatusForbidden || !strings.Contains(out, cantDelete) {
				t.Errorf("a guest (modify %v): DELETE answered %d %s", e.GuestsCanModify, code, out)
			}
			if !live(e.ID) {
				t.Errorf("a guest (modify %v) put the event in the trash", e.GuestsCanModify)
			}
		}
		if code, out := send(anaTok, http.MethodDelete, "/v1/events/"+mod.ID, nil); code != 200 || live(mod.ID) {
			t.Errorf("the organiser: DELETE answered %d %s", code, out)
		}
	})
	t.Run("guest writes: MCP cancel_event is the organiser's", func(t *testing.T) {
		if !ai {
			t.Skip("MCP is the AI edition's")
		}
		mcpOn(t)
		shut, mod := guestPair(t, "Cancel", 19)
		for _, e := range []*calendarBusiness.Event{shut, mod} {
			if code, out := tool(benWrite, "cancel_event", map[string]any{"id": e.ID}); toolOK(code, out) || !strings.Contains(out, cantDelete) {
				t.Errorf("a guest (modify %v): cancel_event answered %d %s", e.GuestsCanModify, code, out)
			}
			if !live(e.ID) {
				t.Errorf("a guest (modify %v) put the event in the trash", e.GuestsCanModify)
			}
		}
		if code, out := tool(anaTok, "cancel_event", map[string]any{"id": mod.ID}); !toolOK(code, out) || live(mod.ID) {
			t.Errorf("the organiser: cancel_event answered %d %s", code, out)
		}
	})
	trashed := func(t *testing.T, name string, h int) (shut, mod *calendarBusiness.Event) {
		t.Helper()
		shut, mod = guestPair(t, name, h)
		for _, e := range []*calendarBusiness.Event{shut, mod} {
			if err := events.Delete(ctx, ana, e.ID, "", nil); err != nil {
				t.Fatalf("Ana deletes her event: %v", err)
			}
		}
		return shut, mod
	}
	t.Run("guest writes: restore over /v1 is the organiser's", func(t *testing.T) {
		shut, mod := trashed(t, "Restore", 20)
		for _, e := range []*calendarBusiness.Event{shut, mod} {
			if code, out := send(benWrite, http.MethodPost, "/v1/events/"+e.ID+"/restore", nil); code != http.StatusNotFound || live(e.ID) {
				t.Errorf("a guest (modify %v): restore answered %d %s", e.GuestsCanModify, code, out)
			}
		}
		if code, out := send(anaTok, http.MethodPost, "/v1/events/"+mod.ID+"/restore", nil); code != http.StatusCreated || !live(mod.ID) {
			t.Errorf("the organiser: restore answered %d %s", code, out)
		}
	})
	t.Run("guest writes: MCP restore_event is the organiser's", func(t *testing.T) {
		if !ai {
			t.Skip("MCP is the AI edition's")
		}
		mcpOn(t)
		shut, mod := trashed(t, "Bring back", 21)
		for _, e := range []*calendarBusiness.Event{shut, mod} {
			if code, out := tool(benWrite, "restore_event", map[string]any{"id": e.ID}); toolOK(code, out) || live(e.ID) {
				t.Errorf("a guest (modify %v): restore_event answered %d %s", e.GuestsCanModify, code, out)
			}
		}
		if code, out := tool(anaTok, "restore_event", map[string]any{"id": mod.ID}); !toolOK(code, out) || !live(mod.ID) {
			t.Errorf("the organiser: restore_event answered %d %s", code, out)
		}
	})
}
