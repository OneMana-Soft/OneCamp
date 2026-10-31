//go:build integration
// +build integration

package integration_test

// The DM list in pages (DG4), against real Postgres 12 and Dgraph: a client
// that asks for no page gets the whole list, before the backfill and after;
// the backfill orders every DM written before the order existed, and run
// again finds none; a page is one graph round trip; pages run newest first
// with nothing repeated or skipped, DMs with equal last times included; a
// first page brings the DMs further down with unread messages; a new message
// moves its DM to the top, a backdated one never moves it back, and a new
// DM's first message gives it its time; a bad edge is refused.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	chatController "github.com/akashc777/OneCamp/controllers/Chat"
	chatDomain "github.com/akashc777/OneCamp/domain/Chat"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/dgraphInit"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	configModel "github.com/akashc777/OneCamp/models/postgres/Config"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestTheDMListInPages(t *testing.T) {
	ctx := context.Background()
	env := integration.SetupEnv(t)
	if err := postgresInit.ConnectPostgres(ctx, env.DSN); err != nil {
		t.Fatal(err)
	}
	integration.SetupRedis(t)
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)
	t.Cleanup(func() { helpers.SettleBackground(10 * time.Second) })

	const live = "0001-01-01T00:00:00Z"
	const bigDMs = 120
	base := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	small, big := uuid.New(), uuid.New()
	others := make([]uuid.UUID, bigDMs+1)
	for i := range others {
		others[i] = uuid.New()
	}
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := env.PG.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	for i, id := range append([]uuid.UUID{small, big}, others...) {
		exec(`INSERT INTO users (id, email_id, username, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4, NOW(), NOW())`,
			id, fmt.Sprintf("p%d@acme.test", i), fmt.Sprintf("p%d", i), fmt.Sprintf("p%d", i))
	}

	// Each DM has two messages, the i-th's newest i minutes before base,
	// except DMs 48 to 52, whose newest are all at the same instant, across
	// the first page's end. DM 70's messages are all deleted. None has its
	// order yet.
	newestAt := func(i int) time.Time {
		if i >= 48 && i <= 52 {
			return base.Add(-48 * time.Minute)
		}
		return base.Add(-time.Duration(i) * time.Minute)
	}
	nodes := []map[string]any{
		{"uid": "_:small", "dgraph.type": "User", "user_uuid": small.String(), "user_name": "small", "user_deleted_at": live},
		{"uid": "_:big", "dgraph.type": "User", "user_uuid": big.String(), "user_name": "big", "user_deleted_at": live},
	}
	dmsOf := map[string][]map[string]any{}
	for i, o := range others {
		nodes = append(nodes, map[string]any{"uid": fmt.Sprintf("_:o%d", i), "dgraph.type": "User", "user_uuid": o.String(), "user_name": fmt.Sprintf("o%d", i), "user_deleted_at": live})
		for _, me := range []struct {
			name string
			id   uuid.UUID
			n    int
		}{{"small", small, 10}, {"big", big, bigDMs}} {
			if i >= me.n {
				continue
			}
			deleted := live
			if me.name == "big" && i == 70 {
				deleted = base.Format(time.RFC3339)
			}
			at := newestAt(i)
			key := fmt.Sprintf("_:%s%d", me.name, i)
			nodes = append(nodes, map[string]any{
				"uid": key, "dgraph.type": "Dm",
				"dm_grouping_id":  helpers.GetGroupingId(me.id.String(), o.String()),
				"dm_participants": []map[string]any{{"uid": "_:" + me.name}, {"uid": fmt.Sprintf("_:o%d", i)}},
				"dm_chats": []map[string]any{
					{"dgraph.type": "Chat", "chat_uuid": uuid.NewString(), "chat_body_text": "<p>earlier</p>", "chat_created_at": at.Add(-time.Hour).Format(time.RFC3339Nano), "chat_deleted_at": deleted, "chat_from": map[string]any{"uid": "_:" + me.name}},
					{"dgraph.type": "Chat", "chat_uuid": uuid.NewString(), "chat_body_text": fmt.Sprintf("<p>newest %d</p>", i), "chat_created_at": at.Format(time.RFC3339Nano), "chat_deleted_at": deleted, "chat_from": map[string]any{"uid": fmt.Sprintf("_:o%d", i)}},
				},
			})
			dmsOf[me.name] = append(dmsOf[me.name], map[string]any{"uid": key})
		}
	}
	nodes[0]["user_dms"], nodes[1]["user_dms"] = dmsOf["small"], dmsOf["big"]
	dg.Mutate(t, nodes)

	whole := func(me uuid.UUID) int {
		t.Helper()
		u, err := chatBusiness.GetUserChatListWithLatestChat(ctx, me)
		if err != nil {
			t.Fatal(err)
		}
		return len(u.DMs)
	}
	trips := countingClient(t, dg.Endpoint)
	page := func(me uuid.UUID, edge *chatDomain.DMEdge) (chatBusiness.DMListPage, int) {
		t.Helper()
		before := trips.n.Load()
		p, err := chatBusiness.GetUserChatListPage(ctx, me, edge)
		if err != nil {
			t.Fatal(err)
		}
		return p, int(trips.n.Load() - before)
	}

	// Before the backfill: a client asking for no page, and one asking for a
	// page, both get the whole list (the deleted DM left out, as ever).
	if n := whole(big); n != bigDMs-1 {
		t.Fatalf("the whole list before the backfill: %d DMs, want %d", n, bigDMs-1)
	}
	if p, _ := page(big, nil); len(p.DMs) != bigDMs-1 || p.HasMore {
		t.Fatalf("a page before the backfill: %d DMs, has_more=%v; want the whole list", len(p.DMs), p.HasMore)
	}

	// The backfill: batches until none is left; run again, it finds none.
	stamped := 0
	for {
		n, err := chatDomain.StampUnstampedDMs(ctx, 40)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			break
		}
		stamped += n
	}
	if stamped != 10+bigDMs {
		t.Fatalf("the backfill ordered %d DMs, want %d", stamped, 10+bigDMs)
	}
	if n, err := chatDomain.StampUnstampedDMs(ctx, 40); err != nil || n != 0 {
		t.Fatalf("the backfill run again stamped %d (%v), want 0", n, err)
	}
	if err := configModel.UpsertConfig("chat.dm_order_backfill", "done: test"); err != nil {
		t.Fatal(err)
	}
	if n := whole(big); n != bigDMs-1 {
		t.Fatalf("the whole list after the backfill: %d DMs, want %d", n, bigDMs-1)
	}

	// Big has read everything but DM 110, far down the list.
	far := helpers.GetGroupingId(big.String(), others[110].String())
	exec(`INSERT INTO last_seen_chat (user_id, grp_id, user_last_seen) VALUES ($1, $2, $3)`, big, far, base.Add(-48*time.Hour))
	exec(`INSERT INTO chats (grp_id, created_by, created_at) VALUES ($1, $2, $3)`, far, others[110], base.Add(-110*time.Minute))

	p10, trips10 := page(small, nil)
	if trips10 != 1 || len(p10.DMs) != 10 || p10.HasMore || p10.Next != nil {
		t.Fatalf("small's page: %d DMs, has_more=%v, %d round trips; want 10, false, 1", len(p10.DMs), p10.HasMore, trips10)
	}
	first, tripsFirst := page(big, nil)
	if tripsFirst != 1 || len(first.DMs) != chatBusiness.DMPageSize+1 || !first.HasMore || first.Next == nil {
		t.Fatalf("big's first page: %d DMs (50 and the unread one), has_more=%v, %d round trips", len(first.DMs), first.HasMore, tripsFirst)
	}
	if got := first.DMs[chatBusiness.DMPageSize]; got.GroupingId != far || got.UnreadMessageCount != 1 {
		t.Fatalf("the unread DM from further down: %s, %d unread", got.GroupingId, got.UnreadMessageCount)
	}

	// Walk every page: each DM once, newest first (then by grouping id), a
	// round trip each, the deleted one never shown.
	type key struct {
		at  time.Time
		grp string
	}
	seen := map[string]bool{}
	var last *key
	walk := func(dms []*dgraphStruct.DgraphDm, n int) {
		t.Helper()
		for _, d := range dms {
			if seen[d.GroupingId] {
				t.Fatalf("page %d repeats %s", n, d.GroupingId)
			}
			k := key{*d.LastChatAt, d.GroupingId}
			if last != nil && (k.at.After(last.at) || (k.at.Equal(last.at) && k.grp > last.grp)) {
				t.Fatalf("page %d goes back up the list at %s", n, d.GroupingId)
			}
			last, seen[d.GroupingId] = &k, true
		}
	}
	walk(first.DMs[:chatBusiness.DMPageSize], 1)
	pages, next := 1, first.Next
	for next != nil {
		p, n := page(big, &chatDomain.DMEdge{At: next.At, GroupingID: next.GroupingID})
		if n != 1 {
			t.Fatalf("page %d made %d round trips", pages+1, n)
		}
		pages++
		walk(p.DMs, pages)
		next = p.Next
	}
	if len(seen) != bigDMs-1 || pages != 3 {
		t.Fatalf("%d DMs over %d pages, want %d over 3", len(seen), pages, bigDMs-1)
	}

	bigNode, err := userDomain.GetDgraphUserInfoByUUID(ctx, big.String())
	if err != nil || bigNode == nil {
		t.Fatalf("load big's node: %v", err)
	}
	// write adds a message from big, as a send does.
	write := func(grp string, at time.Time, participants ...uuid.UUID) {
		t.Helper()
		id := uuid.NewString()
		zero := time.Unix(0, 0).UTC()
		dm := &dgraphStruct.DgraphDm{Uid: "uid(dm)", GroupingId: grp, DType: []string{"Dm"},
			Chats: []*dgraphStruct.DgraphChat{{Uid: "uid(ch)", Uuid: id, DType: []string{"Chat"}, Body: "<p>" + at.Format(time.RFC3339) + "</p>",
				CreatedAt: &at, DeletedAt: &zero, From: &dgraphStruct.DgraphUser{Uid: bigNode.Uid}}}}
		for _, p := range participants {
			u, err := userDomain.GetDgraphUserInfoByUUID(ctx, p.String())
			if err != nil || u == nil {
				t.Fatalf("load %s: %v", p, err)
			}
			dm.Participants = append(dm.Participants, &dgraphStruct.DgraphUser{Uid: u.Uid, DMs: []*dgraphStruct.DgraphDm{{Uid: "uid(dm)"}}})
		}
		if _, _, err := chatDomain.CreateDgraphChat(ctx, dm, id); err != nil {
			t.Fatal(err)
		}
	}
	top := func() *dgraphStruct.DgraphDm {
		t.Helper()
		p, _ := page(big, nil)
		return p.DMs[0]
	}

	// A new message moves its DM to the top, with its time.
	moved := helpers.GetGroupingId(big.String(), others[100].String())
	now := time.Now().UTC().Truncate(time.Millisecond)
	write(moved, now)
	if d := top(); d.GroupingId != moved || !d.LastChatAt.Equal(now) {
		t.Fatalf("after a new message the top is %s at %v, want %s at %v", d.GroupingId, d.LastChatAt, moved, now)
	}

	// A backdated message (an import's) leaves a newer time as it is.
	write(moved, base.Add(-500*time.Hour))
	if d := top(); d.GroupingId != moved || !d.LastChatAt.Equal(now) {
		t.Fatalf("after a backdated message the top is %s at %v, want %s still at %v", d.GroupingId, d.LastChatAt, moved, now)
	}

	// A new DM's first message, backdated as an import's, gives the DM that
	// time, on the DM itself and on no other node.
	fresh := helpers.GetGroupingId(big.String(), others[bigDMs].String())
	older := base.Add(-30 * time.Second)
	write(fresh, older, big, others[bigDMs])
	dms, freshAt, orphans, err := newDMTime(ctx, fresh)
	if err != nil {
		t.Fatal(err)
	}
	if dms != 1 || !freshAt.Equal(older) || orphans != 0 {
		t.Fatalf("a new DM: %d DMs by its id at %v, %d nodes with a last time and no DM; want 1 at %v, 0", dms, freshAt, orphans, older)
	}

	// Through the route: no page is the whole list; ?paged=1 a first page; a
	// bad edge is refused.
	pg, err := userDomain.GetActiveUserWithAdminFlagByUserUUID(ctx, big)
	if err != nil || pg == nil {
		t.Fatalf("load big: %v", err)
	}
	asBig := context.WithValue(ctx, helpers.UserInfoContextKey, userModels.UserInfo{UserPostgresInfo: *pg, UserDgraphInfo: *bigNode})
	call := func(query url.Values) (int, chatBusiness.DMListPage) {
		t.Helper()
		rec := httptest.NewRecorder()
		chatController.GetUserChatListWithLatestChat(rec, httptest.NewRequest(http.MethodGet, "/getLatestChatList?"+query.Encode(), nil).WithContext(asBig))
		var body struct {
			Data chatBusiness.DMListPage `json:"data"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body.Data
	}
	if code, data := call(url.Values{}); code != http.StatusOK || len(data.DMs) != bigDMs || data.HasMore {
		t.Fatalf("no page asked: %d, %d DMs, has_more=%v; want the whole list of %d", code, len(data.DMs), data.HasMore, bigDMs)
	}
	if code, data := call(url.Values{"paged": {"1"}}); code != http.StatusOK || !data.HasMore || data.Next == nil {
		t.Fatalf("?paged=1: %d, has_more=%v", code, data.HasMore)
	}
	for _, bad := range []url.Values{
		{"at": {"yesterday"}, "grp_id": {moved}},
		{"at": {base.Format(time.RFC3339)}, "grp_id": {"0x1"}},
		{"at": {base.Format(time.RFC3339)}},
		{"at": {"1960-01-01T00:00:00Z"}, "grp_id": {moved}},
	} {
		if code, _ := call(bad); code != http.StatusBadRequest {
			t.Fatalf("edge %v answered %d, want 400", bad, code)
		}
	}
}

// newDMTime is how many DMs have grp as their id, the time of the first, and
// how many nodes have a last time and no grouping id (none should).
func newDMTime(ctx context.Context, grp string) (dms int, at time.Time, orphans int, err error) {
	txn := dgraphInit.DgraphClient.NewReadOnlyTxn()
	defer func() { _ = txn.Discard(ctx) }()
	res, err := txn.QueryWithVars(ctx, `query q($g: string) {
		dm(func: eq(dm_grouping_id, $g)) { dm_last_chat_at }
		orphans(func: has(dm_last_chat_at)) @filter(not has(dm_grouping_id)) { count(uid) }
	}`, map[string]string{"$g": grp})
	if err != nil {
		return 0, at, 0, err
	}
	var got struct {
		DM []struct {
			At time.Time `json:"dm_last_chat_at"`
		} `json:"dm"`
		Orphans []struct {
			Count int `json:"count"`
		} `json:"orphans"`
	}
	if err := json.Unmarshal(res.GetJson(), &got); err != nil {
		return 0, at, 0, err
	}
	if len(got.DM) > 0 {
		at = got.DM[0].At
	}
	if len(got.Orphans) > 0 {
		orphans = got.Orphans[0].Count
	}
	return len(got.DM), at, orphans, nil
}
