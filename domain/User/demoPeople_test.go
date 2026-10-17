package domain

import (
	"testing"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func TestDemoVisibleUsersKeepsTheCastAndBots(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	t.Setenv("DEMO_USER_EMAIL", "visitor@demo.example")
	users := []*dgraphStruct.DgraphUser{
		{UserName: "owner", EmailID: "owner@example.com"},
		{UserName: "maya", EmailID: "maya.chen" + helpers.DemoCastEmailDomain},
		{UserName: "captain", EmailID: AgentBotPrefix + "x" + BotEmailDomain, IsBot: true},
		nil,
	}
	got := DemoVisibleUsers("visitor@demo.example", users)
	if len(got) != 2 || got[0].UserName != "maya" || got[1].UserName != "captain" {
		t.Errorf("got %d people: %+v", len(got), got)
	}
	if len(DemoVisibleUsers("owner@example.com", users)) != len(users) {
		t.Error("a real member of the demo lost people from a list")
	}
}
