package helpers

import (
	"regexp"
	"strings"
	"testing"
)

// Giving someone a channel, project or team must drop their cached profile,
// as taking one away does (TestRevokingMembershipInvalidatesTheMembersProfile).
//
// The person's graph profile (user:dgraph) is cached for an hour and lists
// their channels, projects and teams; global search, the AI's reach and
// channel-scoped slash commands are scoped by those lists. No grant path
// dropped it, so someone who joined a channel, or made one, couldn't find its
// history in search for up to an hour. The revoke paths had been fixed, which
// left joining as the slow direction. tests/integration
// TestGrantingMembershipBringsItIntoTheirSearch runs the paths; this walk is
// for the next one, which is the one that will forget.
//
// A grant is a business function writing a members edge: a channel's,
// project's or team's Members list built for a graph write.
func TestGrantingMembershipInvalidatesTheMembersProfile(t *testing.T) {
	grants := regexp.MustCompile(`Members:\s*\[\]\*dgraphStruct\.DgraphUser\{`)
	purges := "InvalidateUserMemberships"
	exempt := map[string]string{
		"AddChannelBotMemberEdge": "an agent's bot principal holds no session and searches " +
			"nothing; its cached profile authorizes nothing (business/Principal refuses a bot as " +
			"the authority for anything)",
	}
	sites := revokeSites(t, grants)
	if len(sites) < 6 {
		t.Fatalf("found %d grant paths; the channel, project and team ones alone are more, so "+
			"the pattern no longer fits how they are written", len(sites))
	}
	for _, s := range sites {
		if exempt[s.name] != "" {
			continue
		}
		if !strings.Contains(s.body, purges) {
			t.Errorf("%s: %s gives someone a channel, project or team without calling %s.\n"+
				"Until their cached profile is dropped, search, the AI's reach and slash "+
				"commands leave it out for up to the hour it's kept.", s.path, s.name, purges)
		}
	}
}
