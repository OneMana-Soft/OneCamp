//go:build integration
// +build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	activityBusiness "github.com/akashc777/OneCamp/business/Activity"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/router"
	"github.com/akashc777/OneCamp/tests/integration"
)

// A4 H5: Priority is triage the person can clear. They take an item out of
// their own Priority (POST /activity/dismiss) and can put it back; an ask
// lets go of Priority a week after it was asked. Only Priority changes:
// the item stays in the feed (All), in its channel and in search, and
// nobody else's Priority moves. Each call goes through the API's own router
// as the person's signed-in request, so the caller is the session's.

// priorityItem is a feed item as the app reads it from GET /activity/unified.
type priorityItem struct {
	ActivityType string `json:"activity_type"`
	Priority     string `json:"priority"`
	// Key is the server's name for the item, the one the app posts to
	// take it out of Priority; Dismissed is that the person did.
	Key       string `json:"key"`
	Dismissed bool   `json:"dismissed"`
	Mention   *struct {
		Uid  string `json:"uid"`
		Post *struct {
			Uuid string `json:"post_uuid"`
		} `json:"mention_post"`
		Comment *struct {
			Uuid string `json:"comment_uuid"`
			Text string `json:"comment_text"`
		} `json:"mention_comment"`
	} `json:"mention"`
}

// priorityAPI is seedWorkWorld (ana, ben, cy, dee...) behind router.Routes().
type priorityAPI struct {
	w      *workWorld
	routes http.Handler
}

func newPriorityAPI(t *testing.T) *priorityAPI {
	t.Helper()
	t.Setenv("JWT_SECRET", "inbox-priority-dismiss-integration-jwt-secret")
	t.Setenv("CSRF_DISABLED", "true")
	w := seedWorkWorld(t)
	integration.SetupRedis(t)
	return &priorityAPI{w: w, routes: router.Routes()}
}

// call is who's request through the router ("" is nobody signed in), with
// their session as the web sends it: the Authorization and DeviceId cookies,
// the only places VerifyAuth reads a session from (authcookie).
func (p *priorityAPI) call(t *testing.T, who, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if who != "" {
		token, err := helpers.SignSessionToken(p.w.people[who].String(), helpers.TokenTypeAccess, time.Now().Add(10*time.Minute).Unix())
		if err != nil {
			t.Fatal(err)
		}
		req.AddCookie(&http.Cookie{Name: "Authorization", Value: token})
		req.AddCookie(&http.Cookie{Name: "DeviceId", Value: "device-1"})
	}
	rec := httptest.NewRecorder()
	p.routes.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// asks is who's feed (GET /activity/unified?notices=1), its mentions by the
// uuid of the message that asked: the post, or the reply on it.
func (p *priorityAPI) asks(t *testing.T, who string) map[string]priorityItem {
	t.Helper()
	code, body := p.call(t, who, http.MethodGet, "/activity/unified?notices=1&limit=50", "")
	var feed struct {
		Data struct {
			Activities []priorityItem `json:"activities"`
		} `json:"data"`
	}
	if code != http.StatusOK || json.Unmarshal([]byte(body), &feed) != nil {
		t.Fatalf("%s's feed: %d %s", who, code, body)
	}
	out := map[string]priorityItem{}
	for _, it := range feed.Data.Activities {
		switch {
		case it.Mention != nil && it.Mention.Post != nil:
			out[it.Mention.Post.Uuid] = it
		case it.Mention != nil && it.Mention.Comment != nil:
			out[it.Mention.Comment.Uuid] = it
		}
	}
	return out
}

// dismiss is who taking keys out of their Priority, or putting them back.
func (p *priorityAPI) dismiss(t *testing.T, who string, dismissed bool, keys ...string) int {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"keys": keys, "dismissed": dismissed})
	code, body := p.call(t, who, http.MethodPost, "/activity/dismiss", string(b))
	if code != http.StatusNoContent {
		t.Logf("%s's POST /activity/dismiss: %d %s", who, code, body)
	}
	return code
}

// channel is a public channel of members; it answers its graph uid.
func (p *priorityAPI) channel(t *testing.T, name string, members ...string) string {
	t.Helper()
	id := uuid.NewString()
	p.w.exec(`INSERT INTO channels (id, ch_name, ch_private) VALUES ($1, $2, false)`, id, name+"-"+id[:8])
	uids := p.w.dg.Mutate(t, []map[string]any{{"uid": "_:ch", "dgraph.type": "Channel", "ch_uuid": id, "ch_name": name,
		"ch_private": false, "ch_deleted_at": "0001-01-01T00:00:00Z", "ch_members": p.people(members...)}})
	return uids["ch"]
}

// people is each named person as a graph reference.
func (p *priorityAPI) people(names ...string) []map[string]any {
	var out []map[string]any
	for _, n := range names {
		out = append(out, map[string]any{"uid": p.w.account(n).UserDgraphInfo.Uid})
	}
	return out
}

// ask is by's message in the channel naming the people asked, as CreatePost
// stores it, written ago; it answers the message's uuid.
func (p *priorityAPI) ask(t *testing.T, by, channel, text string, ago time.Duration, asked ...string) string {
	t.Helper()
	post, _ := p.asked(t, by, channel, text, ago, asked...)
	return post
}

// asked is ask that also answers the message's graph uid, for a reply on it.
func (p *priorityAPI) asked(t *testing.T, by, channel, text string, ago time.Duration, asked ...string) (post, uid string) {
	t.Helper()
	post = uuid.NewString()
	at := time.Now().UTC().Add(-ago).Format(time.RFC3339Nano)
	uids := p.w.dg.Mutate(t, []map[string]any{
		{"uid": "_:p", "dgraph.type": "Post", "post_uuid": post, "post_text": text,
			"post_by": p.people(by)[0], "post_channel": map[string]any{"uid": channel},
			"post_created_at": at, "post_deleted_at": "0001-01-01T00:00:00Z"},
		{"uid": "_:m", "dgraph.type": "Mention", "mention_users": p.people(asked...), "mention_post": map[string]any{"uid": "_:p"}, "mention_created_at": at},
	})
	if uids["p"] == "" {
		t.Fatalf("no graph uid for the message: %v", uids)
	}
	return post, uids["p"]
}

// reply is by's reply on the message (its graph uid) naming the people asked,
// as CreateComment stores it, written ago; it answers the reply's uuid.
func (p *priorityAPI) reply(t *testing.T, by, post, text string, ago time.Duration, asked ...string) string {
	t.Helper()
	reply := uuid.NewString()
	at := time.Now().UTC().Add(-ago).Format(time.RFC3339Nano)
	p.w.dg.Mutate(t, []map[string]any{
		{"uid": "_:c", "dgraph.type": "Comment", "comment_uuid": reply, "comment_text": text, "comment_by": p.people(by)[0],
			"comment_post": map[string]any{"uid": post}, "comment_created_at": at, "comment_deleted_at": "0001-01-01T00:00:00Z"},
		{"uid": post, "post_comments": []any{map[string]any{"uid": "_:c"}}},
		{"uid": "_:m", "dgraph.type": "Mention", "mention_users": p.people(asked...), "mention_comment": map[string]any{"uid": "_:c"}, "mention_created_at": at},
	})
	return reply
}

// keyRows is who holds a row for key in inbox_marks, and whether each is
// dismissed, by person.
func (p *priorityAPI) keyRows(t *testing.T, key string) map[uuid.UUID]bool {
	t.Helper()
	rows, err := p.w.env.PG.Query(`SELECT user_id, dismissed_at IS NOT NULL FROM inbox_marks WHERE item_key = $1`, key)
	if err != nil {
		t.Fatalf("read inbox_marks: %v", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var who uuid.UUID
		var dismissed bool
		if err := rows.Scan(&who, &dismissed); err != nil {
			t.Fatal(err)
		}
		out[who] = dismissed
	}
	return out
}

// Ben asks ana and cy in one message. Ana takes it out of her Priority: it
// reads normal for her, marked dismissed, still in her feed; cy's Priority
// still holds it.
func TestADismissalLeavesOnlyTheirOwnPriority(t *testing.T) {
	p := newPriorityAPI(t)
	high, normal := activityBusiness.PriorityHigh, activityBusiness.PriorityNormal
	ch := p.channel(t, "launch", "ana", "ben", "cy")
	post := p.ask(t, "ben", ch, "@ana @cy can we get the rollback steps into the Launch notes before Thursday?", 30*time.Minute, "ana", "cy")

	ana, cy := p.asks(t, "ana")[post], p.asks(t, "cy")[post]
	if ana.Priority != high || cy.Priority != high {
		t.Fatalf("ben's ask reads %q for ana and %q for cy; want both %q", ana.Priority, cy.Priority, high)
	}
	if ana.Key == "" || ana.Dismissed {
		t.Fatalf("ana's feed sends ben's ask as key %q, dismissed %v; want a key, not dismissed", ana.Key, ana.Dismissed)
	}
	if code := p.dismiss(t, "ana", true, ana.Key); code != http.StatusNoContent {
		t.Fatalf("ana takes ben's ask out of her Priority: %d; want 204", code)
	}

	got, listed := p.asks(t, "ana")[post]
	if !listed {
		t.Fatalf("ben's ask left ana's feed; a dismissal only takes it out of Priority")
	}
	if got.Priority != normal || !got.Dismissed {
		t.Errorf("after ana's dismissal it reads %q, dismissed %v, for her; want %q and dismissed (never low: low reads as read)", got.Priority, got.Dismissed, normal)
	}
	if c := p.asks(t, "cy")[post]; c.Priority != high || c.Dismissed {
		t.Errorf("ana's dismissal moved cy's Priority: %q, dismissed %v; want %q, not dismissed", c.Priority, c.Dismissed, high)
	}
}

// Ana puts it back: it is Priority again, and her row is kept with no
// dismissal (nothing is ever deleted).
func TestADismissalIsUndoneAndKept(t *testing.T) {
	p := newPriorityAPI(t)
	ch := p.channel(t, "launch", "ana", "ben")
	post := p.ask(t, "ben", ch, "@ana could you check the rollback steps before Thursday?", time.Hour, "ana")
	key := p.asks(t, "ana")[post].Key
	if key == "" {
		t.Fatalf("ana's feed sends ben's ask with no key")
	}
	if code := p.dismiss(t, "ana", true, key); code != http.StatusNoContent {
		t.Fatalf("ana dismisses it: %d", code)
	}
	if code := p.dismiss(t, "ana", false, key); code != http.StatusNoContent {
		t.Fatalf("ana puts it back: %d", code)
	}
	if got := p.asks(t, "ana")[post]; got.Priority != activityBusiness.PriorityHigh || got.Dismissed {
		t.Errorf("put back, it reads %q, dismissed %v; want %q, not dismissed", got.Priority, got.Dismissed, activityBusiness.PriorityHigh)
	}
	if rows := p.keyRows(t, key); len(rows) != 1 || rows[p.w.people["ana"]] {
		t.Errorf("inbox_marks holds %v for the key; want ana's one row kept, not dismissed", rows)
	}
}

// An ask lets go of Priority a week after it was asked: eight days on it
// reads normal, six days on it is still Priority, and both are in the feed.
func TestPriorityLetsGoAfterSevenDays(t *testing.T) {
	p := newPriorityAPI(t)
	ch := p.channel(t, "launch", "ana", "ben")
	old := p.ask(t, "ben", ch, "@ana can you send the vendor list before Monday?", 8*24*time.Hour, "ana")
	recent := p.ask(t, "ben", ch, "@ana can you review the launch copy?", 6*24*time.Hour, "ana")

	feed := p.asks(t, "ana")
	o, oldListed := feed[old]
	r, recentListed := feed[recent]
	if !oldListed || !recentListed {
		t.Fatalf("ana's feed lists the 8-day ask %v and the 6-day ask %v; want both: age only lets go of Priority", oldListed, recentListed)
	}
	if o.Priority != activityBusiness.PriorityNormal || o.Dismissed {
		t.Errorf("the 8-day ask reads %q, dismissed %v; want %q, not dismissed", o.Priority, o.Dismissed, activityBusiness.PriorityNormal)
	}
	if r.Priority != activityBusiness.PriorityHigh {
		t.Errorf("the 6-day ask reads %q; want %q", r.Priority, activityBusiness.PriorityHigh)
	}
}

// Dee, who isn't in the channel, posts ana's key and names ana as the user:
// the same 204 as any dismissal (no answer says what exists), ana's Priority
// holds the ask, and the only row written is dee's own. Nobody signed in is
// refused, and a key that isn't a Priority item's is refused.
func TestNobodyChangesAnothersPriority(t *testing.T) {
	p := newPriorityAPI(t)
	ch := p.channel(t, "design", "ana", "ben")
	post := p.ask(t, "ben", ch, "@ana can you approve the hero image?", time.Hour, "ana")
	key := p.asks(t, "ana")[post].Key
	if key == "" {
		t.Fatalf("ana's feed sends ben's ask with no key")
	}
	ana := p.w.people["ana"].String()
	b, _ := json.Marshal(map[string]any{"keys": []string{key}, "dismissed": true, "user_id": ana, "user_uuid": ana})
	if code, body := p.call(t, "dee", http.MethodPost, "/activity/dismiss", string(b)); code != http.StatusNoContent {
		t.Errorf("dee's dismissal of a key she can't see: %d %s; want the same 204", code, body)
	}
	if got := p.asks(t, "ana")[post]; got.Priority != activityBusiness.PriorityHigh || got.Dismissed {
		t.Errorf("after dee's call ana's ask reads %q, dismissed %v; want %q, not dismissed", got.Priority, got.Dismissed, activityBusiness.PriorityHigh)
	}
	if _, listed := p.asks(t, "dee")[post]; listed {
		t.Errorf("dee's feed lists ben's message in a channel she isn't in")
	}
	if rows := p.keyRows(t, key); len(rows) != 1 || !rows[p.w.people["dee"]] {
		t.Errorf("inbox_marks holds %v for the key; want dee's own row alone", rows)
	}
	if code, _ := p.call(t, "", http.MethodPost, "/activity/dismiss", string(b)); code != http.StatusUnauthorized {
		t.Errorf("nobody signed in dismisses: %d; want 401", code)
	}
	if code := p.dismiss(t, "ana", true, "REACTION|id|0x1"); code != http.StatusBadRequest {
		t.Errorf("a reaction's key: %d; want 400", code)
	}
}

// atOnce is eight of ana's calls at once on eight fresh items, twelve
// rounds: each round takes the items out of her Priority, then puts them
// back. mixed: half the calls name the items in the other order; direct: the
// calls go to activityBusiness.Dismiss, without the route. Every call
// succeeds and each item keeps one row of ana's, dismissed and then cleared.
// Eight items, not two: a write that takes the rows in the order it meets
// the keys (a hashed DISTINCT keeps the caller's order only for keys whose
// hashes collide) deadlocks on a few pairs, so one pair is luck and eight
// meet such a pair nearly every round.
func (p *priorityAPI) atOnce(t *testing.T, label string, mixed, direct bool) {
	t.Helper()
	ana := p.w.people["ana"]
	var bad []string
	var mu sync.Mutex
	for round := 0; round < 12; round++ {
		var items []string
		for i := 0; i < 4; i++ {
			items = append(items, fmt.Sprintf("MENTION|id|0x%s-%d-%d", label, round, i), fmt.Sprintf("COMMENT|id|%s-%d-%d", label, round, i))
		}
		for _, dismissed := range []bool{true, false} {
			var wg sync.WaitGroup
			start := make(chan struct{})
			for i := 0; i < 8; i++ {
				keys := slices.Clone(items)
				if mixed && i%2 == 1 {
					slices.Reverse(keys)
				}
				body, _ := json.Marshal(map[string]any{"keys": keys, "dismissed": dismissed})
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					var res string
					if direct {
						if err := activityBusiness.Dismiss(context.Background(), ana, keys, dismissed); err != nil {
							res = err.Error()
						}
					} else if code, out := p.call(t, "ana", http.MethodPost, "/activity/dismiss", string(body)); code != http.StatusNoContent {
						res = fmt.Sprintf("%d %s", code, out)
					}
					if res != "" {
						mu.Lock()
						bad = append(bad, fmt.Sprintf("round %d dismissed=%v first key %s: %s", round, dismissed, keys[0], res))
						mu.Unlock()
					}
				}()
			}
			close(start)
			wg.Wait()
			for _, k := range items {
				if rows := p.keyRows(t, k); len(rows) != 1 || rows[ana] != dismissed {
					t.Errorf("%s round %d dismissed=%v: inbox_marks holds %v for %s; want ana's one row, dismissed %v", label, round, dismissed, rows, k, dismissed)
				}
			}
		}
	}
	if len(bad) > 0 {
		t.Errorf("%s: %d of 192 calls at once failed; first: %v", label, len(bad), bad[:min(len(bad), 3)])
	}
}

// Ana takes the same eight items out of her Priority from several tabs at
// once, half naming them in the other order (her select-many and its Undo),
// then puts them back the same way: every call succeeds, none deadlocks.
func TestDismissingAtOnceInAnyOrder(t *testing.T) {
	p := newPriorityAPI(t)
	p.dismiss(t, "ana", true, "MENTION|id|0xwarm")
	p.atOnce(t, "any", true, true)
}

// The same items dismissed at once, in one order, through the route: one
// mark each, every call 204.
func TestDismissingAtOnceInOneOrder(t *testing.T) {
	p := newPriorityAPI(t)
	p.dismiss(t, "ana", true, "MENTION|id|0xwarm")
	p.atOnce(t, "one", false, false)
}

// Dee and ana each dismissed the same key; ana puts it back. Only ana's
// mark clears: dee's stays dismissed.
func TestPuttingBackLeavesAnothersMark(t *testing.T) {
	p := newPriorityAPI(t)
	ch := p.channel(t, "design", "ana", "ben")
	post := p.ask(t, "ben", ch, "@ana can you approve the hero image?", time.Hour, "ana")
	key := p.asks(t, "ana")[post].Key
	if key == "" {
		t.Fatalf("ben's ask has no key")
	}
	if p.dismiss(t, "dee", true, key) != http.StatusNoContent || p.dismiss(t, "ana", true, key) != http.StatusNoContent {
		t.Fatalf("dismissals refused")
	}
	if p.dismiss(t, "ana", false, key) != http.StatusNoContent {
		t.Fatalf("ana's put back refused")
	}
	if rows := p.keyRows(t, key); len(rows) != 2 || rows[p.w.people["ana"]] || !rows[p.w.people["dee"]] {
		t.Errorf("after ana puts it back inbox_marks holds %v; want ana's row cleared and dee's still dismissed", rows)
	}
}

// Ana dismisses ben's ask; ben replies on it and asks her again. The first
// ask stays dismissed; the new one in the same thread is its own item, back
// in her Priority.
func TestANewMentionInTheThreadShowsAgain(t *testing.T) {
	p := newPriorityAPI(t)
	ch := p.channel(t, "launch", "ana", "ben")
	post, uid := p.asked(t, "ben", ch, "@ana can you check the rollback steps before Thursday?", 2*time.Hour, "ana")
	first := p.asks(t, "ana")[post]
	if first.Priority != activityBusiness.PriorityHigh || first.Key == "" {
		t.Fatalf("the first ask reads %q key %q; want high with a key", first.Priority, first.Key)
	}
	if code := p.dismiss(t, "ana", true, first.Key); code != http.StatusNoContent {
		t.Fatalf("ana dismisses the first ask: %d", code)
	}
	reply := p.reply(t, "ben", uid, "<p>@ana one more: can you also send the vendor list?</p>", time.Minute, "ana")
	feed := p.asks(t, "ana")
	old, oldListed := feed[post]
	nw, newListed := feed[reply]
	if !oldListed || !newListed {
		t.Fatalf("ana's feed lists the first ask %v and the reply %v; want both", oldListed, newListed)
	}
	if old.Priority != activityBusiness.PriorityNormal || !old.Dismissed {
		t.Errorf("the dismissed first ask reads %q, dismissed %v; want normal and dismissed", old.Priority, old.Dismissed)
	}
	if nw.Key == "" || nw.Key == first.Key || nw.Priority != activityBusiness.PriorityHigh || nw.Dismissed {
		t.Errorf("the new mention in the thread is key %q (first %q), reads %q, dismissed %v; want its own key, high, not dismissed",
			nw.Key, first.Key, nw.Priority, nw.Dismissed)
	}
}

// Dismiss, dismiss again, put back, dismiss again: every call 204, and the
// last word holds: dismissed, one row.
func TestDismissingAgainAfterPuttingBack(t *testing.T) {
	p := newPriorityAPI(t)
	ch := p.channel(t, "launch", "ana", "ben")
	post := p.ask(t, "ben", ch, "@ana could you check the rollback steps?", time.Hour, "ana")
	key := p.asks(t, "ana")[post].Key
	for i, d := range []bool{true, true, false, true} {
		if code := p.dismiss(t, "ana", d, key); code != http.StatusNoContent {
			t.Fatalf("call %d (dismissed=%v): %d", i, d, code)
		}
	}
	if got := p.asks(t, "ana")[post]; got.Priority != activityBusiness.PriorityNormal || !got.Dismissed {
		t.Errorf("dismissed again after a put back, it reads %q, dismissed %v; want normal, dismissed", got.Priority, got.Dismissed)
	}
	if rows := p.keyRows(t, key); len(rows) != 1 || !rows[p.w.people["ana"]] {
		t.Errorf("inbox_marks holds %v; want ana's one row, dismissed", rows)
	}
}

// One call that names the same item twice (a select-many over a list that
// shows it twice) dismisses it once, and one put-back naming it twice puts
// it back once: each call 204, ana's one row, dismissed then cleared.
func TestNamingAnItemTwiceInOneCallMarksItOnce(t *testing.T) {
	p := newPriorityAPI(t)
	ch := p.channel(t, "launch", "ana", "ben")
	post := p.ask(t, "ben", ch, "@ana could you check the rollback steps?", time.Hour, "ana")
	key := p.asks(t, "ana")[post].Key
	if key == "" {
		t.Fatalf("ben's ask has no key")
	}
	if code := p.dismiss(t, "ana", true, key, key); code != http.StatusNoContent {
		t.Fatalf("one dismissal naming the item twice: %d; want 204", code)
	}
	if rows := p.keyRows(t, key); len(rows) != 1 || !rows[p.w.people["ana"]] {
		t.Errorf("after one dismissal naming it twice inbox_marks holds %v; want ana's one row, dismissed", rows)
	}
	if code := p.dismiss(t, "ana", false, key, key); code != http.StatusNoContent {
		t.Fatalf("one put-back naming the item twice: %d; want 204", code)
	}
	if rows := p.keyRows(t, key); len(rows) != 1 || rows[p.w.people["ana"]] {
		t.Errorf("after one put-back naming it twice inbox_marks holds %v; want ana's one row, cleared", rows)
	}
}

// Migration 599 down, up, up again, down, up; then a 200-byte key is stored,
// a 201-byte key is refused and the same (person, key) is refused twice.
func TestInboxMarksMigrationUpDownUp(t *testing.T) {
	p := newPriorityAPI(t)
	pg := p.w.env.PG
	exists := func() bool {
		var ok bool
		if err := pg.QueryRow(`SELECT to_regclass('inbox_marks') IS NOT NULL`).Scan(&ok); err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !exists() {
		t.Fatalf("inbox_marks missing after every migration on a blank DB")
	}
	for i, dir := range []string{"down", "up", "up", "down", "up"} {
		if _, err := pg.Exec(migrationSQL(t, "inbox_marks", dir)); err != nil {
			t.Fatalf("step %d %s: %v", i, dir, err)
		}
		if exists() != (dir == "up") {
			t.Fatalf("step %d %s: inbox_marks exists %v", i, dir, !(dir == "up"))
		}
	}
	ana := p.w.people["ana"]
	insert := `INSERT INTO inbox_marks (user_id, item_key) VALUES ($1, $2)`
	if _, err := pg.Exec(insert, ana, strings.Repeat("k", 200)); err != nil {
		t.Errorf("a 200-byte key: %v; want stored", err)
	}
	if _, err := pg.Exec(insert, ana, strings.Repeat("k", 201)); err == nil {
		t.Errorf("a 201-byte key was stored; want the check to refuse it")
	}
	if _, err := pg.Exec(insert, ana, strings.Repeat("k", 200)); err == nil {
		t.Errorf("the same (person, key) stored twice; want the primary key to refuse it")
	}
}
