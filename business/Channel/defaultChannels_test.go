package business

import (
	"reflect"
	"testing"

	channelModels "github.com/akashc777/OneCamp/models/postgres/Channel"
	"github.com/google/uuid"
)

// A new member opens on #general when they were put in it, it being where the
// workspace says hello; otherwise on the first channel they joined; with none,
// on Home (uuid.Nil).
func TestANewMemberLandsInGeneralFirst(t *testing.T) {
	general, news, eng := uuid.New(), uuid.New(), uuid.New()
	cases := []struct {
		joined []Channel
		want   uuid.UUID
	}{
		{[]Channel{{news, "news"}, {general, GeneralChannelName}}, general},
		{[]Channel{{eng, "engineering"}, {news, "news"}}, eng},
		{nil, uuid.Nil},
	}
	for _, c := range cases {
		if got := landingOf(c.joined, ""); got != c.want {
			t.Errorf("landingOf(%v) = %v, want %v", c.joined, got, c.want)
		}
	}
	// The workspace's own #general, pinned by id when it was seeded, wins
	// whatever it has been renamed to.
	everyone := uuid.New()
	if got := landingOf([]Channel{{news, "news"}, {general, GeneralChannelName}, {everyone, "everyone"}}, everyone.String()); got != everyone {
		t.Errorf("the pinned #general renamed #everyone: landing %v, want it", got)
	}
}

// What an admin saves is ids; anything else is refused before it reaches the
// database, and the order they chose is kept.
func TestDefaultChannelIDsAreCheckedAndKeptInOrder(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	got := validIDs([]string{" " + b.String() + " ", "general", a.String(), b.String(), ""})
	if want := []string{b.String(), a.String()}; !reflect.DeepEqual(got, want) {
		t.Errorf("validIDs = %v, want %v", got, want)
	}
	upper := []string{a.String(), " " + a.String()}
	if n := len(dedupe(upper)); n != 1 {
		t.Errorf("the same channel twice counted as %d", n)
	}
	found := []channelModels.Channel{{Id: a, Name: "alpha"}, {Id: b, Name: "beta"}}
	ordered := inOrder([]string{b.String(), uuid.NewString(), a.String()}, found)
	if len(ordered) != 2 || ordered[0].Name != "beta" || ordered[1].Name != "alpha" {
		t.Errorf("inOrder = %v, want beta then alpha, the missing one left out", ordered)
	}
}
