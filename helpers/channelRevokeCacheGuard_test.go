package helpers

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// revokeSite is a top-level function in business or controllers that calls
// one of the domain writes taking someone's access away.
type revokeSite struct {
	path, name, body string
}

// revokeSites walks business and controllers for top-level functions calling
// a write that revokes matches.
func revokeSites(t *testing.T, revokes *regexp.Regexp) []revokeSite {
	t.Helper()
	funcStart := regexp.MustCompile(`(?m)^func\b`)
	var sites []revokeSite
	for _, root := range []string{"../business", "../controllers"} {
		if _, err := os.Stat(root); err != nil {
			t.Fatalf("scan root %s missing — this ratchet would silently pass: %v", root, err)
		}
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			// Split on top-level func declarations so "some other function in
			// this file purges" cannot vouch for one that does not.
			body := string(src)
			starts := funcStart.FindAllStringIndex(body, -1)
			for i, at := range starts {
				end := len(body)
				if i+1 < len(starts) {
					end = starts[i+1][0]
				}
				fn := body[at[0]:end]
				if !revokes.MatchString(fn) {
					continue
				}
				name := strings.SplitN(strings.TrimPrefix(fn, "func "), "(", 2)[0]
				sites = append(sites, revokeSite{path: path, name: strings.TrimSpace(name), body: fn})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if len(sites) == 0 {
		t.Fatal("found no revoke call sites at all; this guard is no longer watching anything")
	}
	return sites
}

// Revoking channel access must purge the cache that authorization reads.
//
// WHY THIS IS ENFORCED. ChannelBasicInfo holds ch_is_member and ch_is_admin for
// thirty minutes and sits behind roughly two dozen gates: create a post, open a
// channel, an agent's send_message, the admins_only check. Adding a member went
// through CreateOrUpdateDgraphChannel, which purges. Removing one went straight
// to the Dgraph mutation and purged nothing, so granting access was instant while
// REVOKING IT TOOK UP TO HALF AN HOUR — an admin could remove somebody from a
// private channel, see the roster update, and that person could still read and
// still post. The asymmetry is the bug: a stale grant is a breach, a stale denial
// is an inconvenience.
//
// Written as a walk rather than a list of the three functions that exist today,
// because the next revoke path is the one that will forget, and a list cannot
// fail for code nobody remembered to add to it.
func TestRevokingChannelAccessInvalidatesTheCache(t *testing.T) {
	// The domain calls that take an edge away. Their own bodies cannot purge:
	// the cache is keyed on the channel UUID and they are handed only Dgraph
	// uids, so the duty lands on the caller that knows the UUID.
	revokes := regexp.MustCompile(`domain\.Delete(?:Channel)?(?:Member|Moderator)Edge\(`)
	purges := "InvalidateChannelBasicInfo"
	for _, s := range revokeSites(t, revokes) {
		if !strings.Contains(s.body, purges) {
			t.Errorf("%s: %s takes a channel membership or moderator edge away without "+
				"calling %s.\nUntil that purge runs, every permission gate still reads a "+
				"cached yes and the removed person keeps their access for up to the "+
				"30 minute TTL.", s.path, s.name, purges)
		}
	}
}

// Taking a channel, project or team from someone must drop their cached
// profile too.
//
// The person's graph profile (user:dgraph) is cached for an hour and lists
// their channels, projects and teams. Every request reads it through the auth
// middleware, and global search, the AI's reach and command delivery are
// scoped by those lists. No revoke path dropped it, so someone removed from a
// private channel kept searching it for up to an hour. Same walk as above, for
// the same reason: the next revoke path is the one that will forget.
func TestRevokingMembershipInvalidatesTheMembersProfile(t *testing.T) {
	// The domain writes that take a channel, project or team from someone.
	// They're handed graph uids and the cache is keyed on the person's uuid,
	// so again the duty lands on the caller.
	revokes := regexp.MustCompile(`domain\.(?:DeleteChannelMemberEdge|RemoveMemberFromProject|DeleteTeamMemberEdge)\(`)
	purges := "InvalidateUserMemberships"
	exempt := map[string]string{
		"RemoveChannelBotMemberEdge": "an agent's bot principal holds no session and searches " +
			"nothing; its cached profile authorizes nothing (business/Principal refuses a bot as " +
			"the authority for anything)",
	}
	for _, s := range revokeSites(t, revokes) {
		if exempt[s.name] != "" {
			continue
		}
		if !strings.Contains(s.body, purges) {
			t.Errorf("%s: %s takes a channel, project or team from someone without calling %s.\n"+
				"Until their cached profile is dropped, search and every other reader of it "+
				"still covers what they were removed from, for up to the hour it's kept.",
				s.path, s.name, purges)
		}
	}
}
