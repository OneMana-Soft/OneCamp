//go:build integration
// +build integration

package integration_test

// The phone waits while you're in OneCamp (A4 H4): a chat notice to someone
// in OneCamp (a device of theirs connected, not set offline) sends the desktop
// notice now and keeps the push back their notify.push_delay (a minute until
// they choose), as a 'waiting' row and its payload in Redis; the Sweeper's
// pass sends it once when due, unless they read the message meanwhile, it was
// deleted, its payload is gone, or a hold began. A held push is never sent.
// Every test reads the rows the router wrote and the pushes a fake FCM
// sender saw, through the routes people use.
//
// Run: go test -tags=integration ./tests/integration/ -run 'Push.*Wait|Waiting|PauseHeldPush' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	channelAdapter "github.com/akashc777/OneCamp/adapter/Channel"
	postAdapter "github.com/akashc777/OneCamp/adapter/Post"
	userAdapter "github.com/akashc777/OneCamp/adapter/User"
	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	postController "github.com/akashc777/OneCamp/controllers/Post"
	preferencesController "github.com/akashc777/OneCamp/controllers/Preferences"
	userController "github.com/akashc777/OneCamp/controllers/User"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	redisInit "github.com/akashc777/OneCamp/initializers/redis"
	"github.com/go-chi/chi/v5"
)

// with is ctx with these route params (name, value, …).
func with(ctx context.Context, params ...string) context.Context {
	rc := chi.NewRouteContext()
	for i := 0; i+1 < len(params); i += 2 {
		rc.URLParams.Add(params[i], params[i+1])
	}
	return context.WithValue(ctx, chi.RouteCtxKey, rc)
}

// delay sets name's notify.push_delay through PUT /preferences/{key},
// answering the status.
func (w *reachWorld) delay(t *testing.T, name string, value any) int {
	t.Helper()
	code, _, _ := w.run(with(w.as(t, name, false), "key", "notify.push_delay"), preferencesController.SetMine, http.MethodPut, "", map[string]any{"value": value})
	return code
}

// pushRow is name's push row of the message, once written (or 10 s): its
// state, hold_until and event id.
func (w *reachWorld) pushRow(t *testing.T, messageID, name string) (state string, holdUntil *time.Time, eventID string) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; {
		err := w.env.PG.QueryRow(`SELECT state, hold_until, event_id FROM notification_deliveries
			WHERE message_id = $1 AND user_id = $2 AND delivery = 'push'`, messageID, w.people[name]).Scan(&state, &holdUntil, &eventID)
		if err == nil || time.Now().After(deadline) {
			return state, holdUntil, eventID
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// rowState is name's row of delivery for the message, now.
func (w *reachWorld) rowState(t *testing.T, messageID, name, delivery string) string {
	t.Helper()
	var state string
	_ = w.env.PG.QueryRow(`SELECT state FROM notification_deliveries WHERE message_id = $1 AND user_id = $2 AND delivery = $3`,
		messageID, w.people[name], delivery).Scan(&state)
	return state
}

// due makes the message's waiting and held push rows due now.
func (w *reachWorld) due(t *testing.T, messageID string) {
	t.Helper()
	if _, err := w.env.PG.Exec(`UPDATE notification_deliveries SET hold_until = NOW() - interval '1 second'
		WHERE message_id = $1 AND delivery = 'push' AND state IN ('waiting', 'held')`, messageID); err != nil {
		t.Fatal(err)
	}
}

// pushesTo is how many pushes of the message reached name's devices.
func (w *reachWorld) pushesTo(messageID, name string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	n := 0
	for _, p := range w.pushes {
		if p.data[firebaseInit.FIREBASE_PUSH_DATA_THREAD_ID] != messageID {
			continue
		}
		for _, tok := range p.tokens {
			if tok == "tok-"+name {
				n++
			}
		}
	}
	return n
}

// payloadKept is whether the waiting push's payload is in Redis.
func payloadKept(t *testing.T, eventID, userID string) bool {
	t.Helper()
	n, err := redisInit.RedisClient.Exists(context.Background(), "notify:wait:"+eventID+":"+userID).Result()
	if err != nil {
		t.Fatal(err)
	}
	return n == 1
}

// pass is one pass of the Sweeper's notify.push.wait job.
func pass(t *testing.T) {
	t.Helper()
	if err := notifyBusiness.SendWaitingPushesForTest(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
}

// waitingFor posts a mention of jonas (in OneCamp, a minute) in #launch and
// answers the post and its push row's event, once the row is 'waiting'.
func (w *reachWorld) waitingFor(t *testing.T) (post, eventID string) {
	t.Helper()
	if code := w.delay(t, "jonas", 1); code != http.StatusOK {
		t.Fatalf("Jonas setting a minute: %d", code)
	}
	post = w.waitingPost(t, "<p>"+w.mention("jonas")+" the deck?</p>", "jonas")
	_, _, eventID = w.pushRow(t, post, "jonas")
	return post, eventID
}

// waitingPost posts html as maya in #launch and answers the post, once each
// of names' push rows of it is 'waiting'.
func (w *reachWorld) waitingPost(t *testing.T, html string, names ...string) string {
	t.Helper()
	code, post, msg := w.post(t, w.as(t, "maya", false), html)
	if code != http.StatusOK || post == "" {
		t.Fatalf("post: %d %s", code, msg)
	}
	for _, name := range names {
		if state, _, _ := w.pushRow(t, post, name); state != "waiting" {
			t.Fatalf("%s's push row %q, want waiting", name, state)
		}
	}
	return post
}

// online sets name online through POST /user/updateStatus: with the device
// the world gave them connected, they are in OneCamp.
func (w *reachWorld) online(t *testing.T, name string) {
	t.Helper()
	if code, _, msg := w.run(w.as(t, name, false), userController.UpdateUserStatus, http.MethodPost, "", userAdapter.InputUserStatus{Status: "online"}); code != http.StatusOK {
		t.Fatalf("%s going online: %d %s", name, code, msg)
	}
}

// private makes #launch a private channel Maya made: one its members alone
// may read (taking someone out reads who made it).
func (w *reachWorld) private(t *testing.T) {
	t.Helper()
	if _, err := w.env.PG.Exec(`UPDATE channels SET ch_private = true WHERE id = $1`, w.channel); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Q []struct {
			UID string `json:"uid"`
		} `json:"q"`
	}
	if err := json.Unmarshal(w.dg.Query(t, `{ q(func: eq(ch_uuid, "`+w.channel.String()+`")) { uid } }`), &got); err != nil || len(got.Q) != 1 {
		t.Fatalf("#launch's node: %v %+v", err, got)
	}
	w.dg.Mutate(t, map[string]any{"uid": got.Q[0].UID, "ch_private": true, "ch_created_by": map[string]any{"uid": w.uids["maya"]}})
}

func TestAPushWaitsWhileTheyreInOneCampThenGoesOnce(t *testing.T) {
	w := newReachWorld(t)
	if code := w.delay(t, "jonas", 3); code != http.StatusBadRequest {
		t.Errorf("a delay of 3 minutes answered %d, want 400", code)
	}
	if code := w.delay(t, "jonas", 1); code != http.StatusOK {
		t.Fatalf("Jonas setting a minute: %d", code)
	}
	// Sam's device is connected but he set himself offline; Ada has none:
	// neither is in OneCamp, so theirs go now on the default minute.
	before := time.Now()
	code, post, msg := w.post(t, w.as(t, "maya", false), "<p>"+w.mention("jonas")+" "+w.mention("sam")+" "+w.mention("ada")+" the deck?</p>")
	if code != http.StatusOK {
		t.Fatalf("post: %d %s", code, msg)
	}
	state, till, eventID := w.pushRow(t, post, "jonas")
	if state != "waiting" || till == nil || till.Before(before.Add(50*time.Second)) || till.After(time.Now().Add(70*time.Second)) {
		t.Fatalf("Jonas's push row %q until %v, want waiting about a minute", state, till)
	}
	if got := w.rowState(t, post, "jonas", "desktop"); got != "sent" {
		t.Errorf("Jonas's desktop row %q, want sent now", got)
	}
	for _, name := range []string{"sam", "ada"} {
		if s, _, _ := w.pushRow(t, post, name); s != "sent" {
			t.Errorf("%s's push row %q, want sent now (not in OneCamp)", name, s)
		}
	}
	if !payloadKept(t, eventID, w.people["jonas"].String()) {
		t.Fatal("the waiting push's payload isn't kept")
	}
	time.Sleep(300 * time.Millisecond)
	if n := w.pushesTo(post, "jonas"); n != 0 {
		t.Fatalf("Jonas was pushed %d times before his minute", n)
	}

	pass(t) // not due yet
	if got := w.rowState(t, post, "jonas", "push"); got != "waiting" || w.pushesTo(post, "jonas") != 0 {
		t.Fatalf("before due: row %q, %d pushes; want still waiting", got, w.pushesTo(post, "jonas"))
	}
	w.due(t, post)
	pass(t)
	pass(t) // a second pass sends nothing more
	if got := w.rowState(t, post, "jonas", "push"); got != "sent" {
		t.Errorf("Jonas's push row %q after the pass, want sent", got)
	}
	if n := w.pushesTo(post, "jonas"); n != 1 {
		t.Errorf("Jonas was pushed %d times, want once", n)
	}
	if payloadKept(t, eventID, w.people["jonas"].String()) {
		t.Error("the payload stays after the push was sent")
	}
}

func TestReadingFirstCancelsTheWaitingPush(t *testing.T) {
	w := newReachWorld(t)
	post, eventID := w.waitingFor(t)
	jonas := w.as(t, "jonas", false)
	if code, _, msg := w.run(with(jonas, "channel_uuid", w.channel.String()), channelController.MarkChannelSeen, http.MethodPost, "", nil); code != http.StatusOK {
		t.Fatalf("markSeen: %d %s", code, msg)
	}
	w.due(t, post)
	pass(t)
	if got := w.rowState(t, post, "jonas", "push"); got != "dropped" {
		t.Errorf("Jonas's push row %q, want dropped (he read it first)", got)
	}
	if n := w.pushesTo(post, "jonas"); n != 0 {
		t.Errorf("Jonas was pushed %d times after reading it", n)
	}
	if payloadKept(t, eventID, w.people["jonas"].String()) {
		t.Error("the payload stays after the read cancelled the push")
	}
}

func TestReadingADMFirstCancelsTheWaitingPush(t *testing.T) {
	w := newReachWorld(t)
	if code := w.delay(t, "jonas", 5); code != http.StatusOK {
		t.Fatalf("Jonas setting five minutes: %d", code)
	}
	msg := w.dmAs(t, w.as(t, "maya", false), "jonas", "<p>lunch?</p>")
	state, till, eventID := w.pushRow(t, msg, "jonas")
	if state != "waiting" || till == nil || till.Before(time.Now().Add(4*time.Minute)) {
		t.Fatalf("Jonas's DM push row %q until %v, want waiting five minutes", state, till)
	}
	jonas := w.as(t, "jonas", false)
	if code, _, m := w.run(with(jonas, "user_uuid", w.people["maya"].String()), chatController.MarkChatSeen, http.MethodPost, "", nil); code != http.StatusOK {
		t.Fatalf("dm seen: %d %s", code, m)
	}
	w.due(t, msg)
	pass(t)
	if got := w.rowState(t, msg, "jonas", "push"); got != "dropped" || w.pushesTo(msg, "jonas") != 0 {
		t.Errorf("Jonas's DM push row %q, %d pushes; want dropped, none", got, w.pushesTo(msg, "jonas"))
	}
	if payloadKept(t, eventID, w.people["jonas"].String()) {
		t.Error("the payload stays after the read cancelled the push")
	}
}

func TestTwoPassesAtOnceSendOneWaitingPush(t *testing.T) {
	w := newReachWorld(t)
	post, _ := w.waitingFor(t)
	w.due(t, post)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := 0; i < 4; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			_ = notifyBusiness.SendWaitingPushesForTest(context.Background())
		}()
	}
	start.Done()
	done.Wait()
	if n := w.pushesTo(post, "jonas"); n != 1 {
		t.Errorf("Jonas was pushed %d times by four passes at once, want once", n)
	}
	if got := w.rowState(t, post, "jonas", "push"); got != "sent" {
		t.Errorf("Jonas's push row %q, want sent", got)
	}
}

func TestDeletingBeforeDueDropsTheWaitingPush(t *testing.T) {
	w := newReachWorld(t)
	post, eventID := w.waitingFor(t)
	maya := w.as(t, "maya", false)
	if code, _, msg := w.run(maya, postController.DeletePost, http.MethodDelete, "", postAdapter.InputCreateOrUpdatePostInfo{Uuid: post, ChannelUuid: w.channel.String()}); code != http.StatusOK {
		t.Fatalf("delete: %d %s", code, msg)
	}
	deadline := time.Now().Add(10 * time.Second)
	for w.rowState(t, post, "jonas", "push") != "dropped" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := w.rowState(t, post, "jonas", "push"); got != "dropped" {
		t.Fatalf("Jonas's push row %q after the delete, want dropped", got)
	}
	if payloadKept(t, eventID, w.people["jonas"].String()) {
		t.Error("the payload stays after the delete")
	}
	if _, err := w.env.PG.Exec(`UPDATE notification_deliveries SET hold_until = NOW() - interval '1 second' WHERE message_id = $1`, post); err != nil {
		t.Fatal(err)
	}
	pass(t)
	if got, n := w.rowState(t, post, "jonas", "push"), w.pushesTo(post, "jonas"); got != "dropped" || n != 0 {
		t.Errorf("after a pass: row %q, %d pushes; want dropped, none", got, n)
	}
}

func TestAWaitingPushWhosePayloadIsGoneIsDropped(t *testing.T) {
	w := newReachWorld(t)
	post, eventID := w.waitingFor(t)
	if err := redisInit.RedisClient.Del(context.Background(), "notify:wait:"+eventID+":"+w.people["jonas"].String()).Err(); err != nil {
		t.Fatal(err)
	}
	w.due(t, post)
	pass(t)
	if got, n := w.rowState(t, post, "jonas", "push"), w.pushesTo(post, "jonas"); got != "dropped" || n != 0 {
		t.Errorf("row %q, %d pushes; want dropped, none", got, n)
	}
}

func TestAPayloadThatCantBeKeptPushesNow(t *testing.T) {
	w := newReachWorld(t)
	t.Cleanup(notifyBusiness.FailPayloadsForTest())
	if code := w.delay(t, "jonas", 1); code != http.StatusOK {
		t.Fatalf("Jonas setting a minute: %d", code)
	}
	code, post, msg := w.post(t, w.as(t, "maya", false), "<p>"+w.mention("jonas")+" the deck?</p>")
	if code != http.StatusOK {
		t.Fatalf("post: %d %s", code, msg)
	}
	deadline := time.Now().Add(10 * time.Second)
	for w.rowState(t, post, "jonas", "push") != "sent" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if got := w.rowState(t, post, "jonas", "push"); got != "sent" {
		t.Errorf("Jonas's push row %q, want sent now", got)
	}
	for deadline := time.Now().Add(10 * time.Second); w.pushesTo(post, "jonas") == 0 && time.Now().Before(deadline); {
		time.Sleep(50 * time.Millisecond)
	}
	if n := w.pushesTo(post, "jonas"); n != 1 {
		t.Errorf("Jonas was pushed %d times, want once, now", n)
	}
}

// REGRESSION GUARD: a pause holds the push for good ('held'), even once its
// hold_until has passed; the Sweeper's pass never sends it. It holds today
// and must hold after.
func TestAPauseHeldPushStaysHeldThroughAPass(t *testing.T) {
	w := newReachWorld(t)
	w.pause(t, "jonas")
	code, post, msg := w.post(t, w.as(t, "maya", false), "<p>"+w.mention("jonas")+" the deck?</p>")
	if code != http.StatusOK {
		t.Fatalf("post: %d %s", code, msg)
	}
	if state, _, _ := w.pushRow(t, post, "jonas"); state != "held" {
		t.Fatalf("Jonas's push row %q, want held (paused)", state)
	}
	w.due(t, post)
	pass(t)
	if got, n := w.rowState(t, post, "jonas", "push"), w.pushesTo(post, "jonas"); got != "held" || n != 0 {
		t.Errorf("after a pass: row %q, %d pushes; want held, never pushed", got, n)
	}
}

func TestRightAwayPushesNowWhileInOneCamp(t *testing.T) {
	w := newReachWorld(t)
	if code := w.delay(t, "jonas", 0); code != http.StatusOK {
		t.Fatalf("Jonas setting right away: %d", code)
	}
	code, post, msg := w.post(t, w.as(t, "maya", false), "<p>"+w.mention("jonas")+" the deck?</p>")
	if code != http.StatusOK {
		t.Fatalf("post: %d %s", code, msg)
	}
	if state, till, _ := w.pushRow(t, post, "jonas"); state != "sent" || till != nil {
		t.Errorf("Jonas's push row %q until %v, want sent now", state, till)
	}
	if got := w.pushedFor(t, post, []string{"jonas"}); len(got) != 1 {
		t.Errorf("pushed %v, want Jonas now", got)
	}
}

// Jonas and Sam both wait on two mentions, and Jonas reads #launch between
// them: one pass sends each device its own pushes alone: Jonas the second
// alone (he read the first), Sam both.
func TestEachWaitingPushGoesToItsOwnPersonAlone(t *testing.T) {
	w := newReachWorld(t)
	w.online(t, "sam")
	for _, name := range []string{"jonas", "sam"} {
		if code := w.delay(t, name, 1); code != http.StatusOK {
			t.Fatalf("%s setting a minute: %d", name, code)
		}
	}
	both := w.mention("jonas") + " " + w.mention("sam")
	first := w.waitingPost(t, "<p>"+both+" the deck?</p>", "jonas", "sam")
	if code, _, msg := w.run(with(w.as(t, "jonas", false), "channel_uuid", w.channel.String()), channelController.MarkChannelSeen, http.MethodPost, "", nil); code != http.StatusOK {
		t.Fatalf("markSeen: %d %s", code, msg)
	}
	second := w.waitingPost(t, "<p>"+both+" and the budget?</p>", "jonas", "sam")
	w.due(t, first)
	w.due(t, second)
	pass(t)
	for _, c := range []struct {
		post, name, state string
		pushes            int
	}{{first, "jonas", "dropped", 0}, {first, "sam", "sent", 1}, {second, "jonas", "sent", 1}, {second, "sam", "sent", 1}} {
		if got, n := w.rowState(t, c.post, c.name, "push"), w.pushesTo(c.post, c.name); got != c.state || n != c.pushes {
			t.Errorf("%s's push of %s: row %q, %d pushes; want %q, %d", c.name, c.post, got, n, c.state, c.pushes)
		}
	}
}

// Sam, taken out of a private channel while his push waits, may no longer
// read it: the pass drops it and he is never pushed.
func TestTakenOutBeforeDueDropsTheWaitingPush(t *testing.T) {
	w := newReachWorld(t)
	w.private(t)
	w.online(t, "sam")
	if code := w.delay(t, "sam", 1); code != http.StatusOK {
		t.Fatalf("Sam setting a minute: %d", code)
	}
	post := w.waitingPost(t, "<p>"+w.mention("sam")+" the deck?</p>", "sam")
	_, _, eventID := w.pushRow(t, post, "sam")
	if code, _, msg := w.run(w.as(t, "jonas", false), channelController.RemoveChannelMember, http.MethodPost, "",
		channelAdapter.InputChannelMemberInfo{ChannelUuid: w.channel.String(), UserUuid: w.people["sam"].String()}); code != http.StatusOK {
		t.Fatalf("Jonas taking Sam out: %d %s", code, msg)
	}
	w.due(t, post)
	pass(t)
	if got, n := w.rowState(t, post, "sam", "push"), w.pushesTo(post, "sam"); got != "dropped" || n != 0 {
		t.Errorf("Sam's push row %q, %d pushes; want dropped, none (he can't read it now)", got, n)
	}
	if payloadKept(t, eventID, w.people["sam"].String()) {
		t.Error("the payload stays after the push was dropped")
	}
}

// A pause Jonas begins while his push waits holds it: the pass drops it and
// he is never pushed.
func TestAPauseBegunDuringTheWaitDropsTheWaitingPush(t *testing.T) {
	w := newReachWorld(t)
	post, eventID := w.waitingFor(t)
	w.pause(t, "jonas")
	w.due(t, post)
	pass(t)
	if got, n := w.rowState(t, post, "jonas", "push"), w.pushesTo(post, "jonas"); got != "dropped" || n != 0 {
		t.Errorf("Jonas's push row %q, %d pushes; want dropped, none (he paused)", got, n)
	}
	if payloadKept(t, eventID, w.people["jonas"].String()) {
		t.Error("the payload stays after the push was dropped")
	}
}
