package helpers

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

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
	funcStart := regexp.MustCompile(`(?m)^func\b`)

	roots := []string{"../business", "../controllers"}
	checked := 0
	for _, root := range roots {
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
				checked++
				if !strings.Contains(fn, purges) {
					name := strings.SplitN(strings.TrimPrefix(fn, "func "), "(", 2)[0]
					t.Errorf("%s: %s takes a channel membership or moderator edge away without "+
						"calling %s.\nUntil that purge runs, every permission gate still reads a "+
						"cached yes and the removed person keeps their access for up to the "+
						"30 minute TTL.", path, strings.TrimSpace(name), purges)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", root, err)
		}
	}
	if checked == 0 {
		t.Fatal("found no channel revoke call sites at all; this guard is no longer watching anything")
	}
}
