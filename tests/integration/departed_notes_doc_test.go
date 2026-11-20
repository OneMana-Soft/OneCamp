//go:build integration
// +build integration

package integration_test

// V-1209 A and CR-405 N2 against Postgres 12 and Dgraph. A departed owner's
// call notes from a private channel go to someone still in it, never to the
// admin outside it; with nobody left in it they stay, and the admin learns
// only their title; a public channel's notes and a personal doc go to whom the
// admin named, as before; an event call's notes go to its organiser. The
// people still here and each event's people are one Postgres read (uuid[]
// arguments). A workspace admin joining a channel leaves one audit row; a
// member joining leaves none.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestAPrivateConversationsCallNotes|TestHandoverPeople|TestAWorkspaceAdminsJoin' -v

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	departedBusiness "github.com/akashc777/OneCamp/business/Departed"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	departedDomain "github.com/akashc777/OneCamp/domain/Departed"
	"github.com/akashc777/OneCamp/helpers"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

// notesWorld is c62Setup's people (owner the admin, dana departed, maya) plus
// kim (here), gone (departed), bot (a bot) and guest (an outside person), and
// an event maya organises with bot, guest, gone and kim invited, in that
// order. A bot and an outside person are never still here (CR-429 F2).
type notesWorld struct {
	people map[string]uuid.UUID
	event  uuid.UUID
	env    *integration.Env
}

func setupNotesWorld(t *testing.T) *notesWorld {
	t.Helper()
	env, people, _ := c62Setup(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"kim", "gone"} {
		id := uuid.New()
		people[name] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			id, name+"-"+id.String()[:8]+"@example.test", name, name)
	}
	exec(`UPDATE users SET deleted_at = NOW() WHERE id = $1`, people["gone"])
	for name, column := range map[string]string{"bot": "is_bot", "guest": "is_external"} {
		id := uuid.New()
		people[name] = id
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at, `+column+`) VALUES ($1, $2, $3, $4, NOW(), NOW(), true)`,
			id, name+"-"+id.String()[:8]+"@example.test", name, name)
	}
	calendar, event := uuid.New(), uuid.New()
	exec(`INSERT INTO calendars (id, owner_id) VALUES ($1, $2)`, calendar, people["maya"])
	exec(`INSERT INTO events (id, calendar_id, created_by, ical_uid, start_at, end_at) VALUES ($1, $2, $3, $4, NOW(), NOW() + interval '1 hour')`,
		event, calendar, people["maya"], event.String()+"@onecamp")
	exec(`INSERT INTO event_attendees (event_id, user_id, added_at) VALUES ($1, $4, NOW() - interval '4 minutes'), ($1, $5, NOW() - interval '3 minutes'),
		($1, $2, NOW() - interval '2 minutes'), ($1, $3, NOW() - interval '1 minute')`,
		event, people["gone"], people["kim"], people["bot"], people["guest"])
	return &notesWorld{people: people, event: event, env: env}
}

func TestAPrivateConversationsCallNotesGoToSomeoneStillInIt(t *testing.T) {
	w := setupNotesWorld(t)
	ctx := context.Background()
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	t.Cleanup(dg.Close)
	p := w.people
	user := func(name string) map[string]any { return map[string]any{"uid": "_:" + name} }
	docs := map[string]string{}
	nodes := []map[string]any{}
	for _, name := range []string{"owner", "dana", "maya", "kim", "gone", "bot", "guest"} {
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "User", "user_uuid": p[name].String()})
	}
	for _, d := range []string{"private", "lonely", "public", "event", "mine"} {
		docs[d] = uuid.NewString()
		nodes = append(nodes, map[string]any{"dgraph.type": "Doc", "doc_uuid": docs[d], "doc_title": d + " notes", "doc_private": true,
			"doc_owner": user("dana"), "doc_created_by": user("dana"), "doc_deleted_at": "0001-01-01T00:00:00Z"})
	}
	channel := func(name string, private bool, members ...string) {
		var ms []map[string]any
		for _, m := range members {
			ms = append(ms, user(m))
		}
		nodes = append(nodes, map[string]any{"uid": "_:" + name, "dgraph.type": "Channel", "ch_uuid": uuid.NewString(), "ch_name": name,
			"ch_private": private, "ch_members": ms, "ch_created_by": user("dana"), "ch_moderators": ms[1:]})
	}
	channel("secret", true, "dana", "kim")
	// Nobody here is left in empty: gone departed, and a bot and an outside
	// person never take a private conversation's notes.
	channel("empty", true, "dana", "gone", "bot", "guest")
	channel("open", false, "dana", "maya")
	call := func(doc, home string) map[string]any {
		m := map[string]any{"dgraph.type": "Recording", "recording_egress_id": "s-" + doc, "recording_ended_at": "2026-11-10T10:00:00Z",
			"recording_notes_doc": docs[doc]}
		if home == "" {
			m["recording_event"] = w.event.String()
		} else {
			m["recording_channel"] = map[string]any{"uid": "_:" + home}
		}
		return m
	}
	nodes = append(nodes, call("private", "secret"), call("lonely", "empty"), call("public", "open"), call("event", ""))
	dg.Mutate(t, nodes)
	ownerOf := func(doc string) string {
		t.Helper()
		var out struct {
			N []struct {
				Owner struct {
					UUID string `json:"user_uuid"`
				} `json:"doc_owner"`
			} `json:"n"`
		}
		if err := json.Unmarshal(dg.Query(t, `{ n(func: eq(doc_uuid, "`+docs[doc]+`")) { doc_owner { user_uuid } } }`), &out); err != nil || len(out.N) != 1 {
			t.Fatalf("read %s: %v", doc, err)
		}
		return out.N[0].Owner.UUID
	}

	// The admin hands Dana's work to themselves, from outside every channel.
	admin := accessBusiness.Subject{UserUUID: p["owner"].String(), Admin: true}
	m, err := departedBusiness.HandOver(ctx, admin, p["dana"], p["owner"])
	if err != nil || m.Docs != 4 || m.Failed != 0 {
		t.Fatalf("handover %+v %v, want four docs moved", m, err)
	}
	for doc, want := range map[string]string{"private": "kim", "lonely": "dana", "public": "owner", "event": "maya", "mine": "owner"} {
		if got := ownerOf(doc); got != p[want].String() {
			t.Errorf("%s notes went to %s, want %s", doc, got, want)
		}
	}
	// What stayed is named in the answer by its title alone.
	raw, _ := json.Marshal(m)
	var answer struct {
		Kept []string `json:"kept"`
	}
	if err := json.Unmarshal(raw, &answer); err != nil || strings.Join(answer.Kept, ",") != "lonely notes" {
		t.Fatalf("kept %v %v", answer.Kept, err)
	}
	if strings.Contains(string(raw), docs["lonely"]) {
		t.Fatalf("the answer names the kept doc's id: %s", raw)
	}
}

func TestHandoverPeopleReadsActivePeopleAndEventPeopleInOneRead(t *testing.T) {
	w := setupNotesWorld(t)
	ctx := context.Background()
	p := w.people
	trashed := uuid.New()
	if _, err := w.env.PG.Exec(`INSERT INTO events (id, calendar_id, created_by, ical_uid, start_at, end_at, deleted_at)
		SELECT $1, calendar_id, created_by, $2, start_at, end_at, NOW() FROM events WHERE id = $3`, trashed, trashed.String()+"@onecamp", w.event); err != nil {
		t.Fatal(err)
	}
	active, events, err := departedDomain.HandoverPeople(ctx,
		[]uuid.UUID{p["bot"], p["guest"], p["maya"], p["dana"], p["kim"], p["gone"], uuid.New()}, []uuid.UUID{w.event, trashed, uuid.New()})
	if err != nil {
		t.Fatal(err)
	}
	var here []string
	for id, ok := range active {
		if ok {
			for name, pid := range p {
				if pid == id {
					here = append(here, name)
				}
			}
		}
	}
	sort.Strings(here)
	if strings.Join(here, ",") != "kim,maya" {
		t.Fatalf("still here: %v", here)
	}
	if len(events) != 1 || len(events[w.event]) != 2 || events[w.event][0] != p["maya"] || events[w.event][1] != p["kim"] {
		t.Fatalf("event people %v, want the organiser then kim (gone left, bot and guest are never here, the trashed event has none)", events)
	}
	if a, e, err := departedDomain.HandoverPeople(ctx, nil, nil); err != nil || len(a) != 0 || len(e) != 0 {
		t.Fatalf("no one asked: %v %v %v", a, e, err)
	}
}

func TestAWorkspaceAdminsJoinLeavesOneAuditRow(t *testing.T) {
	t.Setenv("RESEND_API_KEY", "")
	t.Setenv("APP_SECRET_KEK", "admin-join-integration-kek")
	lr := setupLandReady(t)
	member := func(email, name string) userModels.UserInfo {
		id, _, err := userBusiness.CreateUserWithMethod(lr.ctx, email, name, nil, userModels.AuthMethodEmail, false)
		if err != nil {
			t.Fatal(err)
		}
		return lr.info(id)
	}
	dana, maya := member("dana@example.test", "Dana"), member("maya@example.test", "Maya")
	err, ch := channelBusiness.CreateChannel(lr.ctx, &channelAdapter.InputCreateChannel{ChannelName: "street"}, &dana)
	if err != nil {
		t.Fatal(err)
	}
	join := func(who userModels.UserInfo) {
		t.Helper()
		raw, _ := json.Marshal(channelAdapter.InputJoinChannel{ChannelUuid: ch.String()})
		rec := httptest.NewRecorder()
		channelController.JoinChannel(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw)).
			WithContext(context.WithValue(lr.ctx, helpers.UserInfoContextKey, who)))
		if rec.Code >= 300 {
			t.Fatalf("%d %s", rec.Code, rec.Body.String())
		}
	}
	join(maya)
	join(lr.owner)
	audited(t, lr.env.PG, "channel.admin.join", 1)
	var name, channel string
	if err := lr.env.PG.QueryRow(`SELECT COALESCE(metadata->>'channel_name', ''), COALESCE(metadata->>'channel_uuid', '') FROM admin_audit_log
		WHERE action = 'channel.admin.join'`).Scan(&name, &channel); err != nil || name != "street" || channel != ch.String() {
		t.Fatalf("the row names %q %q, %v", name, channel, err)
	}
	if !lr.memberOf(lr.owner.UserPostgresInfo.Id, ch) || !lr.memberOf(lr.userID("maya@example.test"), ch) {
		t.Fatal("a join did not join")
	}
}
