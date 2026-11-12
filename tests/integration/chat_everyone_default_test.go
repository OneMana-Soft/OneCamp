//go:build integration
// +build integration

package integration_test

// Where the "@" list offers @everyone (V-731, slack-04), against real
// Postgres 12, Redis and Dgraph: in a channel new members join, which is the
// workspace's own #general until an admin chooses other channels
// (business/Channel.DefaultChannels, the rule joining uses). The list read
// only an admin's choice, so a workspace whose admin never chose offered
// @everyone nowhere.
//
// Run: go test -tags=integration ./tests/integration/ -run 'TestEveryoneIsOfferedWhereNewMembersJoin' -v

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/go-chi/chi/v5"

	channelBusiness "github.com/akashc777/OneCamp/business/Channel"
	reachBusiness "github.com/akashc777/OneCamp/business/Reach"
	settingsBusiness "github.com/akashc777/OneCamp/business/Settings"
	conversationController "github.com/akashc777/OneCamp/controllers/Conversation"
)

func TestEveryoneIsOfferedWhereNewMembersJoin(t *testing.T) {
	w := newReachWorld(t)
	// The settings are cached process-wide: start and end with none, so no
	// test reads another's defaults.
	settingsBusiness.ForgetSettingsForTest()
	t.Cleanup(settingsBusiness.ForgetSettingsForTest)
	maya := w.as(t, "maya", false)
	// offers is what Maya's "@" list in #launch offers to notify at once
	// (GET /conversations/channel/{id}/mentionables).
	offers := func(t *testing.T) []string {
		t.Helper()
		rc := chi.NewRouteContext()
		rc.URLParams.Add("kind", "channel")
		rc.URLParams.Add("id", w.channel.String())
		code, data, msg := w.run(context.WithValue(maya, chi.RouteCtxKey, rc), conversationController.GetMentionables, http.MethodGet, "", nil)
		var m reachBusiness.Mentionables
		if code != http.StatusOK || json.Unmarshal(data, &m) != nil {
			t.Fatalf("Maya's @ list in #launch: %d %s %s", code, data, msg)
		}
		var names []string
		for _, b := range m.Broadcasts {
			names = append(names, string(b.Name))
		}
		return names
	}

	// No admin has chosen, and #launch isn't the workspace's #general.
	if got := offers(t); slices.Contains(got, "everyone") || !slices.Contains(got, "channel") {
		t.Errorf("#launch, not #general, offers %q; want @channel and no @everyone", got)
	}

	// #launch is the #general the workspace was seeded with (pinned by id, so
	// a rename keeps it), and no admin has chosen: new members join it.
	if err := settingsBusiness.PinGeneralChannel(w.channel.String()); err != nil {
		t.Fatal(err)
	}
	if got := offers(t); !slices.Contains(got, "everyone") {
		t.Errorf("the workspace's #general, with no admin's choice, offers %q; want everyone among them", got)
	}

	// An admin chooses no channel for new members: @everyone goes.
	if _, err := channelBusiness.SetDefaultChannels(context.Background(), []string{}); err != nil {
		t.Fatal(err)
	}
	if got := offers(t); slices.Contains(got, "everyone") {
		t.Errorf("an admin chose no channel, yet #launch offers %q", got)
	}

	// An admin chooses #launch: it comes back.
	if _, err := channelBusiness.SetDefaultChannels(context.Background(), []string{w.channel.String()}); err != nil {
		t.Fatal(err)
	}
	if got := offers(t); !slices.Contains(got, "everyone") {
		t.Errorf("an admin chose #launch, yet it offers %q", got)
	}
}
