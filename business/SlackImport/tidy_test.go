package business

import (
	"testing"

	"github.com/google/uuid"
)

func TestAPrivateChannelIsHandedToWhoMadeItInSlack(t *testing.T) {
	ada, bo, cy, bot := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	resolved := map[string]uuid.UUID{"UADA": ada, "UBO": bo, "UCY": cy, "UBOT": bot}
	notBots := func(string) bool { return false }
	bots := func(id string) bool { return id == "UBOT" }

	for _, tc := range []struct {
		name    string
		channel SlackChannel
		isBot   func(string) bool
		want    uuid.UUID
		ok      bool
	}{
		{"its creator, who is in it", SlackChannel{Creator: "UBO", Members: []string{"UADA", "UBO"}}, notBots, bo, true},
		{"its first member, when the creator left it", SlackChannel{Creator: "UCY", Members: []string{"UADA", "UBO"}}, notBots, ada, true},
		{"its first member, when the creator didn't come across", SlackChannel{Creator: "UGONE", Members: []string{"UGHOST", "UBO"}}, notBots, bo, true},
		{"never a bot", SlackChannel{Creator: "UBOT", Members: []string{"UBOT", "UCY"}}, bots, cy, true},
		{"nobody, when no member came across", SlackChannel{Creator: "UGONE", Members: []string{"UGHOST"}}, notBots, uuid.Nil, false},
		{"nobody, when every member is a bot", SlackChannel{Members: []string{"UBOT"}}, bots, uuid.Nil, false},
	} {
		got, ok := heirOf(&tc.channel, resolved, tc.isBot)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: %v %v", tc.name, got, ok)
		}
	}
}

func TestTheImportingAdminStaysOnlyInThePrivateChannelsTheyWereIn(t *testing.T) {
	admin, ada := uuid.New(), uuid.New()
	resolved := map[string]uuid.UUID{"UADMIN": admin, "UADA": ada}
	if !wasMember(&SlackChannel{Members: []string{"UADA", "UADMIN"}}, resolved, admin) {
		t.Error("the admin was in it")
	}
	if wasMember(&SlackChannel{Members: []string{"UADA"}}, resolved, admin) {
		t.Error("the admin wasn't in it")
	}
	if wasMember(&SlackChannel{Members: []string{"UNOBODY"}}, map[string]uuid.UUID{}, admin) {
		t.Error("nobody came across")
	}
}
