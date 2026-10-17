//go:build integration
// +build integration

package integration_test

// The channel browser's Discover tab finds every public channel the person
// could join, by name, a page at a time, against a real Dgraph.
//
// It read one page of 20 channels from /ch/allActiveChannels, in graph order
// with the person's own channels among them, and searched only that page:
// in a workspace of 30 public channels, ten never showed, and typing their
// names found nothing (SN-BUG-4). /ch/discover pages and searches on the
// server, without the channels the person is in, private ones or archived.
//
// Run: go test -tags=integration ./tests/integration/ -run TestDiscoverListsEveryChannelToJoin -v

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/google/uuid"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	channelController "github.com/akashc777/OneCamp/controllers/Channel"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/akashc777/OneCamp/tests/integration"
)

func TestDiscoverListsEveryChannelToJoin(t *testing.T) {
	ctx := context.Background()
	integration.StubMqttClient(t)
	dg := integration.SetupDgraph(t)

	const live = "0001-01-01T00:00:00Z"
	me := uuid.NewString()
	nodes := []map[string]any{{"uid": "_:me", "dgraph.type": "User", "user_uuid": me, "user_name": "me",
		"user_channels": []map[string]any{{"uid": "_:mine0"}, {"uid": "_:mine1"}, {"uid": "_:mine2"}}}}
	channel := func(key, name string, private bool, deletedAt string, members ...string) {
		n := map[string]any{"uid": "_:" + key, "dgraph.type": "Channel", "ch_uuid": uuid.NewString(), "ch_name": name,
			"ch_private": private, "ch_deleted_at": deletedAt}
		var m []map[string]any
		for _, who := range members {
			m = append(m, map[string]any{"uid": "_:" + who})
		}
		if len(m) > 0 {
			n["ch_members"] = m
		}
		nodes = append(nodes, n)
	}
	// Three channels the person is in (named to sort first), 25 they could
	// join, written in reverse order, one private and one archived.
	for i := 0; i < 3; i++ {
		channel(fmt.Sprintf("mine%d", i), fmt.Sprintf("aaa-mine-%d", i), false, live, "me")
	}
	for i := 25; i >= 1; i-- {
		channel(fmt.Sprintf("open%d", i), fmt.Sprintf("alpha-%02d", i), false, live)
	}
	channel("secret", "alpha-secret", true, live)
	channel("old", "alpha-old", false, "2026-10-01T00:00:00Z")
	uids := dg.Mutate(t, nodes)

	names := func(chs []*dgraphStruct.DgraphChannel) []string {
		var out []string
		for _, c := range chs {
			out = append(out, c.Name)
		}
		return out
	}
	page, total, err := channelBusiness.GetDiscoverableChannels(ctx, uids["me"], "", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if total != 25 || len(page) != 20 {
		t.Fatalf("the first page holds %d of %d; want 20 of 25 (not theirs, not private, not archived)", len(page), total)
	}
	got := names(page)
	if !sort.StringsAreSorted(got) || got[0] != "alpha-01" || got[19] != "alpha-20" {
		t.Errorf("the first page is %v; want alpha-01 to alpha-20, by name", got)
	}
	rest, _, err := channelBusiness.GetDiscoverableChannels(ctx, uids["me"], "", 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	if r := names(rest); len(r) != 5 || r[0] != "alpha-21" || r[4] != "alpha-25" {
		t.Errorf("the second page is %v; want alpha-21 to alpha-25", r)
	}
	found, total, err := channelBusiness.GetDiscoverableChannels(ctx, uids["me"], "alpha-2", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if total != 6 || len(found) != 6 || found[0].Name != "alpha-20" {
		t.Errorf("searching alpha-2 found %v (%d); want alpha-20 to alpha-25, past the first page", names(found), total)
	}

	// Through the route: the page, how many pages, and whether there is more.
	var info userModels.UserInfo
	info.UserDgraphInfo = dgraphStruct.DgraphUser{Uid: uids["me"], Uuid: me}
	rec := httptest.NewRecorder()
	channelController.GetDiscoverChannelList(rec, httptest.NewRequest(http.MethodGet, "/?pageIndex=0&pageSize=20&search=alpha", nil).
		WithContext(context.WithValue(ctx, helpers.UserInfoContextKey, info)))
	var body struct {
		Channels  []dgraphStruct.DgraphChannel `json:"channels_list"`
		PageCount int                          `json:"pageCount"`
		HasMore   bool                         `json:"has_more"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("/ch/discover answered %d %s", rec.Code, rec.Body.String())
	}
	if len(body.Channels) != 20 || body.PageCount != 2 || !body.HasMore {
		t.Errorf("/ch/discover?search=alpha: %d channels, %d pages, more %v; want 20, 2, true", len(body.Channels), body.PageCount, body.HasMore)
	}
}
