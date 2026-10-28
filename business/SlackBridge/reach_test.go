package business

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	model "github.com/akashc777/OneCamp/models/postgres/SlackBridge"
	"github.com/google/uuid"
)

// withReader answers who may read a channel from reads (by person, then
// channel), or with err, for the test's length, and counts the reads.
func withReader(t *testing.T, reads map[string]map[string]bool, err error) *int {
	t.Helper()
	n := 0
	prev := readsChannel
	readsChannel = func(_ context.Context, who, channel string) (bool, error) {
		n++
		if err != nil {
			return false, err
		}
		return reads[who][channel], nil
	}
	t.Cleanup(func() { readsChannel = prev })
	return &n
}

func TestOnlyAChannelItsLinkerCanReadIsLinked(t *testing.T) {
	ada, max := uuid.New(), uuid.New()
	exec := uuid.New()
	reads := withReader(t, map[string]map[string]bool{ada.String(): {exec.String(): true}}, nil)
	ctx := context.Background()

	if err := mayLink(ctx, &ada, exec); err != nil {
		t.Errorf("ada, who can read #exec, may not link it: %v", err)
	}
	if err := mayLink(ctx, &max, exec); !errors.Is(err, ErrChannelUnreadable) {
		t.Errorf("max, who can't read #exec, linking it: %v, want ErrChannelUnreadable", err)
	}
	before := *reads
	if err := mayLink(ctx, nil, exec); !errors.Is(err, ErrChannelUnreadable) || *reads != before {
		t.Errorf("nobody linking: %v after %d reads, want ErrChannelUnreadable without a read", err, *reads-before)
	}
	// Link asks first: nothing about the bridge or Slack is read for a refusal.
	if _, err := Link(ctx, "C2", exec, &max); !errors.Is(err, ErrChannelUnreadable) {
		t.Errorf("Link for max: %v, want ErrChannelUnreadable", err)
	}
}

func TestALinkWhoseChannelCantBeReadIsARefusalInWords(t *testing.T) {
	withReader(t, nil, errors.New("graph down"))
	by, channel := uuid.New(), uuid.New()
	err := mayLink(context.Background(), &by, channel)
	if !errors.Is(err, ErrChannelUnreadable) {
		t.Fatalf("a failed read: %v, want ErrChannelUnreadable", err)
	}
	if err.Error() != "You can only link a channel you can read. Join it first, or ask someone in it to link it." {
		t.Errorf("the refusal says %q", err.Error())
	}
}

func TestTheAdminPageNamesAPrivateChannelOnlyToWhoCanReadIt(t *testing.T) {
	yes, no := true, false
	cases := []struct {
		what     string
		ch       *dgraphStruct.DgraphChannel
		linkedBy string
		want     string
		hidden   bool
	}{
		{"a public channel, to anyone", &dgraphStruct.DgraphChannel{Uuid: "c1", Name: "general", IsPrivate: &no}, "Ada", "general", false},
		{"a private channel, to a member", &dgraphStruct.DgraphChannel{Uuid: "c2", Name: "board", IsPrivate: &yes, IsMember: 1}, "Ada", "board", false},
		{"a private channel, to a moderator", &dgraphStruct.DgraphChannel{Uuid: "c2", Name: "board", IsPrivate: &yes, IsAdmin: 1}, "Ada", "board", false},
		{"a private channel, to someone outside it", &dgraphStruct.DgraphChannel{Uuid: "c2", Name: "board", IsPrivate: &yes}, "Ada", "private channel (linked by Ada)", true},
		{"a private channel nobody is known to have linked", &dgraphStruct.DgraphChannel{Uuid: "c2", Name: "board", IsPrivate: &yes}, "", "private channel", true},
		{"a channel whose privacy didn't load, to someone outside it", &dgraphStruct.DgraphChannel{Uuid: "c3", Name: "ops"}, "Ada", "private channel (linked by Ada)", true},
		{"a channel that couldn't be read", nil, "Ada", "private channel (linked by Ada)", true},
	}
	for _, c := range cases {
		got, hidden := shownName(c.ch, c.linkedBy)
		if got != c.want || hidden != c.hidden {
			t.Errorf("%s: shown as %q (hidden %v), want %q (hidden %v)", c.what, got, hidden, c.want, c.hidden)
		}
	}
}

func TestTheAuditLogNeverNamesAPrivateChannel(t *testing.T) {
	private := &LinkView{SlackChannelName: "exec-mirror", ChannelName: "board", private: true}
	if got := private.AuditLine(); strings.Contains(got, "board") || got != "Linked Slack #exec-mirror with a private channel" {
		t.Errorf("a private channel's link is logged as %q", got)
	}
	public := &LinkView{SlackChannelName: "general", ChannelName: "general"}
	if got := public.AuditLine(); got != "Linked Slack #general with #general" {
		t.Errorf("a public channel's link is logged as %q", got)
	}
}

func TestTheLinkedChannelsAreReadOnceAsTheViewer(t *testing.T) {
	board, ops := uuid.New(), uuid.New()
	links := []model.Link{{ChannelUUID: board}, {ChannelUUID: ops}}
	var calls [][]string
	var as []string
	prev := channelStandings
	t.Cleanup(func() { channelStandings = prev })
	channelStandings = func(_ context.Context, ids []string, uid string) ([]*dgraphStruct.DgraphChannel, error) {
		calls, as = append(calls, ids), append(as, uid)
		return []*dgraphStruct.DgraphChannel{{Uuid: board.String(), Name: "board"}}, nil
	}
	got, err := linkedChannels(context.Background(), nil, links)
	if err != nil || len(calls) != 1 || !slices.Equal(calls[0], []string{board.String(), ops.String()}) || as[0] != "" {
		t.Fatalf("read %v as %q (%v), want both channels in one read, as nobody", calls, as, err)
	}
	if got[board.String()] == nil || got[ops.String()] != nil {
		t.Errorf("read %v, want #board, and #ops left out as gone", got)
	}

	channelStandings = func(context.Context, []string, string) ([]*dgraphStruct.DgraphChannel, error) {
		return nil, errors.New("graph down")
	}
	if _, err := linkedChannels(context.Background(), nil, links); err == nil {
		t.Error("a failed read of the linked channels is no error")
	}
	if _, err := linkedChannels(context.Background(), nil, nil); err != nil {
		t.Errorf("no links read the graph: %v", err)
	}
}

func TestOutboundCarriesOnlyWhatTheLinksCreatorCanRead(t *testing.T) {
	ada, max := uuid.New(), uuid.New()
	board := uuid.New()
	reads := withReader(t, map[string]map[string]bool{ada.String(): {board.String(): true}}, nil)
	ctx := context.Background()

	if !creatorReads(ctx, model.Link{ChannelUUID: board, CreatedBy: &ada}) {
		t.Error("a link ada made, who can read #board, carries nothing")
	}
	if creatorReads(ctx, model.Link{ChannelUUID: board, CreatedBy: &max}) {
		t.Error("a link max made, who can't read #board, carries its posts")
	}
	before := *reads
	if creatorReads(ctx, model.Link{ChannelUUID: board}) || *reads != before {
		t.Error("a link with no creator carries its posts, or asked who may read it")
	}

	withReader(t, nil, errors.New("graph down"))
	if creatorReads(ctx, model.Link{ChannelUUID: board, CreatedBy: &ada}) {
		t.Error("a link whose creator's access can't be read carries its posts")
	}
}
