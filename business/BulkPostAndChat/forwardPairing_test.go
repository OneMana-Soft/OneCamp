package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	openSearchStruct "github.com/akashc777/OneCamp/models/openSearch"
)

// helpers to keep the cases readable

func dmNode(groupingID, chatUUID string) *dgraphStruct.DgraphDm {
	return &dgraphStruct.DgraphDm{
		GroupingId: groupingID,
		Chats:      []*dgraphStruct.DgraphChat{{Uuid: chatUUID}},
	}
}

func participant(uuid, name string) *openSearchStruct.OpenSearchChatParticipants {
	return &openSearchStruct.OpenSearchChatParticipants{Uuid: uuid, Name: name}
}

// THE DEFECT THIS PINS DOWN.
//
// Forwarding one message to several destinations walks fwdMessageInput.FwdTo in whatever
// order the client picked them. dgraphDMs grows for BOTH group-chat and 1:1 destinations,
// but the participants and grouping-id slices only grew for 1:1 ones. The consumers then
// paired them by slice index.
//
// So a forward to [group chat, DM] produced dgraphDMs[0] = the GROUP node, while
// groupingUUIDs[0] and chatParticipants[0] described the DM. Indexing the group's message
// under the DM's grouping id and participant list puts it in front of people who were never
// in that group, and the DM forward itself was then skipped by the `i >= len` guard and
// never indexed at all.
//
// Pairing is now by chat UUID, so destination order cannot matter.
func TestForwardedDMTargetsPairByIdentityNotPosition(t *testing.T) {
	const (
		groupGrpID = "group-grouping-id"
		groupChat  = "chat-in-the-group"
		dmGrpID    = "dm-grouping-id"
		dmChat     = "chat-in-the-dm"
	)

	dmParticipants := []*openSearchStruct.OpenSearchChatParticipants{
		participant("self", "Me"),
		participant("other", "Priya"),
	}

	// A group destination chosen FIRST, a DM second: the order that used to misalign.
	dgraphDMs := []*dgraphStruct.DgraphDm{
		dmNode(groupGrpID, groupChat),
		dmNode(dmGrpID, dmChat),
	}
	participantsByChatUUID := map[string][]*openSearchStruct.OpenSearchChatParticipants{
		dmChat: dmParticipants, // only the 1:1 forward has participants
	}

	targets := forwardedDMTargets(dgraphDMs, participantsByChatUUID)

	if len(targets) != 1 {
		t.Fatalf("expected exactly the 1:1 forward to be indexed, got %d targets", len(targets))
	}
	got := targets[0]

	if got.Chat.Uuid != dmChat {
		t.Errorf("indexed the wrong chat: got %q, want the DM's chat %q", got.Chat.Uuid, dmChat)
	}
	// The heart of it: the group's grouping id must never ride along with the DM's message,
	// and vice versa.
	if got.GroupingID != dmGrpID {
		t.Errorf("grouping id came from the wrong destination: got %q, want %q — this is the "+
			"cross-conversation misindex", got.GroupingID, dmGrpID)
	}
	if len(got.Participants) != len(dmParticipants) {
		t.Fatalf("participants did not follow their own chat: got %d, want %d",
			len(got.Participants), len(dmParticipants))
	}
	for i := range dmParticipants {
		if got.Participants[i].Uuid != dmParticipants[i].Uuid {
			t.Errorf("participant %d: got %q, want %q", i,
				got.Participants[i].Uuid, dmParticipants[i].Uuid)
		}
	}
}

// Order must be irrelevant: the same destinations in any arrangement produce the same pairing.
func TestForwardedDMTargetsAreOrderIndependent(t *testing.T) {
	partsA := []*openSearchStruct.OpenSearchChatParticipants{participant("a", "A")}
	partsB := []*openSearchStruct.OpenSearchChatParticipants{participant("b", "B")}

	participantsByChatUUID := map[string][]*openSearchStruct.OpenSearchChatParticipants{
		"chat-a": partsA,
		"chat-b": partsB,
	}

	arrangements := map[string][]*dgraphStruct.DgraphDm{
		"group first": {
			dmNode("grp-1", "chat-grp1"),
			dmNode("dm-a", "chat-a"),
			dmNode("grp-2", "chat-grp2"),
			dmNode("dm-b", "chat-b"),
		},
		"DMs first": {
			dmNode("dm-a", "chat-a"),
			dmNode("dm-b", "chat-b"),
			dmNode("grp-1", "chat-grp1"),
			dmNode("grp-2", "chat-grp2"),
		},
		"interleaved the other way": {
			dmNode("dm-b", "chat-b"),
			dmNode("grp-1", "chat-grp1"),
			dmNode("dm-a", "chat-a"),
		},
	}

	// Every arrangement must map each chat to its OWN grouping id.
	wantGrouping := map[string]string{"chat-a": "dm-a", "chat-b": "dm-b"}

	for name, dms := range arrangements {
		targets := forwardedDMTargets(dms, participantsByChatUUID)
		for _, target := range targets {
			want, ok := wantGrouping[target.Chat.Uuid]
			if !ok {
				t.Errorf("%s: a group-chat forward (%q) was indexed as a 1:1 DM",
					name, target.Chat.Uuid)
				continue
			}
			if target.GroupingID != want {
				t.Errorf("%s: chat %q paired with grouping id %q, want %q",
					name, target.Chat.Uuid, target.GroupingID, want)
			}
		}
	}
}

// Group-only forwards must yield nothing to index here rather than borrowing another
// destination's identity.
func TestGroupOnlyForwardIndexesNothingHere(t *testing.T) {
	dgraphDMs := []*dgraphStruct.DgraphDm{
		dmNode("grp-1", "chat-grp1"),
		dmNode("grp-2", "chat-grp2"),
	}

	if targets := forwardedDMTargets(dgraphDMs, nil); len(targets) != 0 {
		t.Errorf("group-only forward produced %d targets, want 0", len(targets))
	}
}

// Malformed nodes are skipped instead of panicking: the old code indexed Chats[0] unguarded.
func TestForwardedDMTargetsSkipMalformedNodes(t *testing.T) {
	participantsByChatUUID := map[string][]*openSearchStruct.OpenSearchChatParticipants{
		"good": {participant("a", "A")},
	}

	dgraphDMs := []*dgraphStruct.DgraphDm{
		nil,
		{GroupingId: "no-chats", Chats: nil},
		{GroupingId: "nil-chat", Chats: []*dgraphStruct.DgraphChat{nil}},
		dmNode("dm-good", "good"),
	}

	targets := forwardedDMTargets(dgraphDMs, participantsByChatUUID)
	if len(targets) != 1 {
		t.Fatalf("expected only the well-formed DM, got %d", len(targets))
	}
	if targets[0].GroupingID != "dm-good" {
		t.Errorf("got grouping id %q, want %q", targets[0].GroupingID, "dm-good")
	}
}
