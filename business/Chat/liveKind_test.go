package business

import (
	"testing"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// A call's message in a DM or a group reaches open apps as itself (calls
// CALLS-3b, CHAT-LEAD-2's review): its kind and facts ride the live payload,
// as a channel's call's post's do. An ordinary message carries neither.
func TestACallsMessageArrivesLiveAsACall(t *testing.T) {
	for _, kind := range []string{mqttStruct.CONVERSATION_KIND_DM, mqttStruct.CONVERSATION_KIND_GROUP} {
		p := asCreated(kind, &dgraphStruct.DgraphChat{Uuid: "c1", Body: "<p>Maya Chen started a call</p>", Kind: helpers.KindHuddle, System: `{"session":"s1"}`}).livePayload()
		if p.ChatKind != helpers.KindHuddle || p.ChatSystem != `{"session":"s1"}` || p.ChatHtmlText != "<p>Maya Chen started a call</p>" {
			t.Errorf("%s: %+v", kind, p)
		}
	}
	if p := asCreated(mqttStruct.CONVERSATION_KIND_DM, &dgraphStruct.DgraphChat{Uuid: "c2", Body: "<p>hi</p>"}).livePayload(); p.ChatKind != "" || p.ChatSystem != "" {
		t.Errorf("an ordinary message: %+v", p)
	}
}

// A DM's or group's reply by someone outside the conversation records no
// replier in the thread's index (calls CALLS-3b, CHAT-LEAD-2's condition 2).
func TestAnOutsidersChatReplyRecordsNoReplier(t *testing.T) {
	chat := &dgraphStruct.DgraphChat{Uuid: "11111111-1111-4111-8111-111111111111"}
	if r := chatReplyRecord("grp-1", chat, "u-jonas", []string{"u-ben"}, time.Time{}, true); r.By != "" || len(r.Mentioned) != 1 {
		t.Errorf("an outsider's reply: %+v", r)
	}
	if r := chatReplyRecord("grp-1", chat, "u-maya", nil, time.Time{}, false); r.By != "u-maya" {
		t.Errorf("a member's reply: %+v", r)
	}
}
