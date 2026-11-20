package domain

import (
	"context"
	"regexp"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	dgraphModels "github.com/akashc777/OneCamp/models/dgraph/Channel"
)

// channelInfoRoot is the list query's root: the channel by its id, and the
// filter (if any) it is taken under, before its first brace.
var channelInfoRoot = regexp.MustCompile(`channelInfo\(func: eq\(ch_uuid, \$id\)\)([^{]*)\{`)

// The channel recording list's one read finds the channel only for someone in
// it: the gate is the query root's filter on the reader, so for anyone else
// the read finds no channel and takes none of its recordings (V-1163). A gate
// moved into Go after the read would load a page of recordings first.
func TestTheChannelRecordingListReadsOnlyAChannelTheReaderIsIn(t *testing.T) {
	was := channelRecordingListRead
	t.Cleanup(func() { channelRecordingListRead = was })
	for _, c := range []struct{ name, start, end string }{{"every recording", "", ""}, {"a date range", "2026-11-01", "2026-11-17"}} {
		var asked string
		var vars map[string]string
		reads := 0
		channelRecordingListRead = func(_ context.Context, query string, variables map[string]string) (*dgraphStruct.DgraphChannel, error) {
			reads++
			asked, vars = query, variables
			return nil, dgraphModels.ErrNotFound
		}
		_, err := GetChannelAllRecordingList(context.Background(), "c1", "0x2a", c.start, c.end, 0, 20)
		if err != dgraphModels.ErrNotFound || reads != 1 {
			t.Errorf("%s: %d reads, err %v; want the one read and its not-found", c.name, reads, err)
		}
		root := channelInfoRoot.FindStringSubmatch(asked)
		if root == nil || root[1] != " @filter(uid_in(ch_members, $userId)) " {
			t.Errorf("%s: the root is %q, want it taken under @filter(uid_in(ch_members, $userId))\n%s", c.name, root, asked)
		}
		if vars["$userId"] != "0x2a" || vars["$id"] != "c1" {
			t.Errorf("%s: $userId %q and $id %q, want the reader 0x2a and the channel c1", c.name, vars["$userId"], vars["$id"])
		}
	}
}
