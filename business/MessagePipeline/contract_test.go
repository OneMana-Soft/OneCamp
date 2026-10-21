package business

import (
	"context"
	"errors"
	"testing"

	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
)

// A send reaches business/Send exactly as the routes' bodies do, and is once
// in the place the routes name, so a retry through either is one message.
func TestASendIsTodaysSend(t *testing.T) {
	files := []*dgraphStruct.DgraphAttachment{{}}
	d := Draft{HTML: "<p>hi</p>", Files: files, ReplyTo: "r", ClientMsgID: "c1"}
	const me, them = "aaaa", "bbbb"

	ch := Write{Op: OpSend, To: conversationBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: "chan"}, Draft: d}
	if in := channelInput(ch); in.HTMLText != d.HTML || in.ChannelUuid != "chan" || in.ReplyToUUID != "r" || in.ClientMsgID != "c1" || len(in.MediaObj) != 1 {
		t.Errorf("channel: %+v", in)
	}
	if k := onceKey(ch, me); k != "ch:chan" {
		t.Errorf("channel once key %q", k)
	}

	dm := Write{Op: OpSend, To: conversationBusiness.Ref{Kind: conversationBusiness.KindDM, ID: them + " " + me}, Draft: d}
	in, ok := dmInput(dm, me)
	if !ok || in.ToUuid != them || in.TextHtml != d.HTML || in.ReplyToUuid != "r" || in.ClientMsgID != "c1" || len(in.MediaObjects) != 1 {
		t.Errorf("dm: %+v %v", in, ok)
	}
	if k := onceKey(dm, me); k != "dm:"+them {
		t.Errorf("dm once key %q", k)
	}
	if _, ok := dmInput(dm, "cccc"); ok {
		t.Error("a DM someone isn't in has no recipient")
	}
	self := Write{To: conversationBusiness.Ref{Kind: conversationBusiness.KindDM, ID: me + " " + me}}
	if in, ok := dmInput(self, me); !ok || in.ToUuid != me {
		t.Errorf("a DM with oneself: %+v %v", in, ok)
	}

	grp := Write{Op: OpSend, To: conversationBusiness.Ref{Kind: conversationBusiness.KindGroup, ID: "g"}, Draft: d}
	if in := groupInput(grp); in.GrpUuid != "g" || in.TextHtml != d.HTML || in.ClientMsgID != "c1" {
		t.Errorf("group: %+v", in)
	}
	// A new group is named by its people, its id made from them.
	fresh := Write{Op: OpSend, To: conversationBusiness.Ref{Kind: conversationBusiness.KindGroup}, People: []string{me, them, "cccc"}, Draft: d}
	if in := groupInput(fresh); in.GrpUuid != "" || len(in.Participants) != 3 || in.Participants[2] != "cccc" {
		t.Errorf("a new group: %+v", in)
	}
	if k := onceKey(grp, me); k != "grp:g" {
		t.Errorf("group once key %q", k)
	}
}

// What the routes still decide (edits, deletions, reactions, replies) the
// default leaves to them; a write for nobody, or to a DM someone isn't in,
// reaches nothing.
func TestWhatTheDefaultLeavesToTheRoutes(t *testing.T) {
	who := &userModels.UserInfo{}
	who.UserDgraphInfo.Uuid = "aaaa"
	for _, op := range []Op{OpEdit, OpDelete, OpReact} {
		if _, err := Messages.Write(context.Background(), who, Write{Op: op}); !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: %v", op, err)
		}
	}
	reply := Write{Op: OpSend, To: conversationBusiness.Ref{Kind: conversationBusiness.KindThread, ID: "root"}}
	if _, err := Messages.Write(context.Background(), who, reply); !errors.Is(err, ErrUnavailable) {
		t.Errorf("a reply: %v", err)
	}
	if _, err := Messages.Write(context.Background(), nil, Write{Op: OpSend}); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Errorf("nobody: %v", err)
	}
	notMine := Write{Op: OpSend, To: conversationBusiness.Ref{Kind: conversationBusiness.KindDM, ID: "bbbb cccc"}}
	if _, err := Messages.Write(context.Background(), who, notMine); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Errorf("a DM I'm not in: %v", err)
	}
}
