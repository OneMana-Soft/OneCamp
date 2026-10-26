package linear

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"testing"
)

// Linear shows a private team's issues to its members alone, and a
// restricted team's (one inside a private team) to some people too, so their
// issues come in restricted, with the team's members as who else may see
// them; a public team's come in open. A visibility Linear adds later is kept
// too, never opened.
func TestAnIssueOfATeamLinearDoesntShowEveryoneComesInRestricted(t *testing.T) {
	withFakeLinear(t, func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"teams":{"nodes":[
			{"id":"t-hr","key":"HR","name":"People","visibility":"private","members":{"nodes":[{"id":"u-rae"},{"id":"u-al"}]}},
			{"id":"t-pay","key":"PAY","name":"Payroll","visibility":"restricted","members":{"nodes":[{"id":"u-al"}]}},
			{"id":"t-eng","key":"ENG","name":"Engineering","visibility":"public","members":{"nodes":[{"id":"u-ben"}]}},
			{"id":"t-x","key":"X","name":"Board","visibility":"secret","members":{"nodes":[{"id":"u-cy"}]}}],
			"pageInfo":{"hasNextPage":false}}}}`)
	})
	teams, err := New().fetchTeams(context.Background(), "tok")
	if err != nil {
		t.Fatal(err)
	}
	snap := &workspaceSnapshot{keptTeams: keptTeams(teams)}
	for _, c := range []struct {
		team, why string
		audience  []string
	}{
		{"t-hr", "a private team", []string{"u-rae", "u-al"}},
		{"t-pay", "a team inside a private team", []string{"u-al"}},
		{"t-eng", "", nil},
		{"t-x", "a team that isn't public", []string{"u-cy"}},
	} {
		st := snap.task(New(), linearIssue{ID: "iss-" + c.team, Identifier: "I-1", Title: "Pay bands", TeamID: c.team}, "p")
		if st.Restricted != c.why || !slices.Equal(st.Audience, c.audience) {
			t.Errorf("an issue of %s: restricted %q to %v; want %q to %v", c.team, st.Restricted, st.Audience, c.why, c.audience)
		}
	}
}
