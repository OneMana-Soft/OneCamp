package business

import (
	"fmt"
	"slices"
	"testing"
)

// A group of up to nine people is a DM to each of them, emailed to every
// one; in a bigger group, as in a channel, only those its message names or
// answers are emailed, while each still hears it in the app. The message is
// told as business/Chat notifyNewChat tells it: a DM place, no audience, its
// people every member of the conversation, the author too.
func TestABigGroupEmailsOnlyThoseItNamesOrAnswers(t *testing.T) {
	people := func(n int) []string {
		ids := make([]string, n)
		for i := range ids {
			ids[i] = fmt.Sprintf("p%d", i)
		}
		return ids
	}
	grp := Place{Kind: PlaceDM, ID: "g"}
	emailed := func(d Decision) bool { return slices.Contains(d.To, ToEmail) }

	eight := people(8) // nine with its author
	got := planned(t, &fakeInputs{levels: all(eight...), members: append(eight, "me")}, Event{Cause: CauseMessage, Actor: "me", Place: grp})
	for _, id := range eight {
		if !emailed(got[id]) {
			t.Errorf("in a group of nine, %s isn't emailed: %+v", id, got[id])
		}
	}

	nine := people(9) // ten with its author
	got = planned(t, &fakeInputs{levels: all(nine...), members: append(nine, "me")},
		Event{Cause: CauseMessage, Actor: "me", Place: grp, Mentions: []string{"p1"}, Author: "p2"})
	for _, id := range []string{"p1", "p2"} {
		if !emailed(got[id]) {
			t.Errorf("in a group of ten, %s (named or answered) isn't emailed: %+v", id, got[id])
		}
	}
	for _, id := range []string{"p0", "p3", "p8"} {
		if emailed(got[id]) {
			t.Errorf("in a group of ten, %s (neither named nor answered) is emailed: %+v", id, got[id])
		}
		if !slices.Contains(got[id].To, ToInbox) || !slices.Contains(got[id].To, ToPush) {
			t.Errorf("in a group of ten, %s no longer hears it in the app: %+v", id, got[id])
		}
	}

	dm := Place{Kind: PlaceDM, ID: "d"}
	got = planned(t, &fakeInputs{levels: all("ann"), members: []string{"ann"}}, Event{Cause: CauseMessage, Actor: "me", Place: dm, Audience: []string{"me", "ann"}})
	if !emailed(got["ann"]) {
		t.Errorf("a DM isn't emailed: %+v", got["ann"])
	}
}
