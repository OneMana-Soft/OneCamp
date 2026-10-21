package entityLinkBusiness

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	domain "github.com/akashc777/OneCamp/domain/EntityLink"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

const (
	openChan   = "c0000000-0000-4000-8000-000000000001"
	closedChan = "c0000000-0000-4000-8000-000000000002"
	oldChan    = "c0000000-0000-4000-8000-000000000003"
	postOpen   = "d0000000-0000-4000-8000-000000000001"
	postClosed = "d0000000-0000-4000-8000-000000000002"
	postGone   = "d0000000-0000-4000-8000-000000000003"
	teamIn     = "e0000000-0000-4000-8000-000000000001"
	teamOut    = "e0000000-0000-4000-8000-000000000002"
	boardOpen  = "f0000000-0000-4000-8000-000000000001"
	boardShut  = "f0000000-0000-4000-8000-000000000002"
	cmtOnOpen  = "ab000000-0000-4000-8000-000000000001"
	cmtOnShut  = "ab000000-0000-4000-8000-000000000002"
	memMine    = "ac000000-0000-4000-8000-000000000001"
	memTheirs  = "ac000000-0000-4000-8000-000000000002"
)

// chanRules decides channels by a table, docs as withRules does.
type chanRules struct {
	rules
	channels map[string]accessBusiness.Level
}

func (r chanRules) Resolve(ctx context.Context, who accessBusiness.Subject, res accessBusiness.Resource) (accessBusiness.Decision, error) {
	if res.Kind != "channel" {
		return r.rules.Resolve(ctx, who, res)
	}
	l, ok := r.channels[res.UUID]
	if !ok {
		return accessBusiness.Decision{}, accessBusiness.ErrNotFound
	}
	return accessBusiness.Decision{Level: l, Was: l}, nil
}

func withGraphKinds(t *testing.T) {
	withRules(t)
	base := accessBusiness.Rules.(rules)
	accessBusiness.Rules = chanRules{rules: base, channels: map[string]accessBusiness.Level{
		openChan: accessBusiness.LevelView, closedChan: accessBusiness.LevelNone, oldChan: accessBusiness.LevelView,
	}}
	past := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	zero := time.Time{}
	savedC, savedM, savedT, savedB, savedK, savedMem := readChannelChips, readMessageChips, readTeamChips, readBoardTitles, readCommentChips, readMemoryChips
	t.Cleanup(func() {
		readChannelChips, readMessageChips, readTeamChips, readBoardTitles, readCommentChips, readMemoryChips = savedC, savedM, savedT, savedB, savedK, savedMem
	})
	readChannelChips = func(_ context.Context, ids []string) ([]domain.ChannelChip, error) {
		all := map[string]domain.ChannelChip{
			openChan:   {UUID: openChan, Name: "launch", DeletedAt: &zero},
			closedChan: {UUID: closedChan, Name: "salaries", Private: true, DeletedAt: &zero},
			oldChan:    {UUID: oldChan, Name: "old", DeletedAt: &past},
		}
		var out []domain.ChannelChip
		for _, id := range ids {
			if c, ok := all[strings.ToLower(id)]; ok {
				out = append(out, c)
			}
		}
		return out, nil
	}
	msg := func(id, ch, text string, del time.Time) domain.MessageChip {
		m := domain.MessageChip{UUID: id, Text: text, DeletedAt: &del, By: &domain.PersonRef{UUID: maya.Uuid, Name: "maya", FullName: "Maya Lin"}}
		m.Channel = &struct {
			UUID      string     `json:"ch_uuid"`
			Name      string     `json:"ch_name"`
			DeletedAt *time.Time `json:"ch_deleted_at"`
		}{UUID: ch, Name: "launch", DeletedAt: &zero}
		return m
	}
	readMessageChips = func(_ context.Context, ids []string) ([]domain.MessageChip, error) {
		all := map[string]domain.MessageChip{
			postOpen:   msg(postOpen, openChan, "<p>See the <b>launch plan</b></p>", zero),
			postClosed: msg(postClosed, closedChan, "<p>Pay bands</p>", zero),
			postGone:   msg(postGone, openChan, "<p>old</p>", past),
		}
		var out []domain.MessageChip
		for _, id := range ids {
			if m, ok := all[strings.ToLower(id)]; ok {
				out = append(out, m)
			}
		}
		return out, nil
	}
	readTeamChips = func(_ context.Context, ids []string, uid string) ([]domain.TeamChip, error) {
		if uid != reader.UserUID {
			return nil, fmt.Errorf("read as %q", uid)
		}
		return []domain.TeamChip{{UUID: teamIn, Name: "Design", Member: 1, DeletedAt: &zero}, {UUID: teamOut, Name: "Board", DeletedAt: &zero}}, nil
	}
	readBoardTitles = func(_ context.Context, ids []string, userUUID string) (map[string]string, error) {
		if userUUID != reader.UserUUID {
			return nil, fmt.Errorf("read as %q", userUUID)
		}
		return map[string]string{boardOpen: "Flows"}, nil
	}
	readCommentChips = func(_ context.Context, ids []string) ([]domain.CommentChip, error) {
		on := func(id, doc string) domain.CommentChip {
			c := domain.CommentChip{UUID: id, Text: "<p>Looks good</p>", DeletedAt: &zero}
			c.Doc = &struct {
				UUID string `json:"doc_uuid"`
			}{UUID: doc}
			return c
		}
		return []domain.CommentChip{on(cmtOnOpen, openDoc), on(cmtOnShut, closedDoc)}, nil
	}
	readMemoryChips = func(_ context.Context, ids []string) ([]domain.MemoryChip, error) {
		m := func(id, owner string) domain.MemoryChip {
			c := domain.MemoryChip{UUID: id, Kind: "decision", Content: "Ship on Friday", Status: "open"}
			c.Owner = &struct {
				UUID string `json:"user_uuid"`
			}{UUID: owner}
			return c
		}
		return []domain.MemoryChip{m(memMine, reader.UserUUID), m(memTheirs, maya.Uuid)}, nil
	}
}

// TestTheGraphKindsShowOnlyWhatTheirReaderMaySee: a channel as the Access
// Resolver decides it; a post as its channel is; a team for its people; a
// board by its own rule; a comment as what it's on; a memory item for its
// owner alone. Whatever the reader can't see says "none" and nothing else.
func TestTheGraphKindsShowOnlyWhatTheirReaderMaySee(t *testing.T) {
	withGraphKinds(t)
	refs := []Ref{
		{"channel", openChan}, {"channel", closedChan}, {"channel", oldChan},
		{"message", postOpen}, {"message", postClosed}, {"message", postGone},
		{"team", teamIn}, {"team", teamOut},
		{"board", boardOpen}, {"board", boardShut},
		{"comment", cmtOnOpen}, {"comment", cmtOnShut},
		{"memory", memMine}, {"memory", memTheirs},
	}
	got, err := ReadEntities(context.Background(), reader, refs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"channel ok launch", "channel none", "channel gone",
		"message ok See the launch plan", "message none", "message gone",
		"team ok Design", "team none",
		"board ok Flows", "board none",
		"comment ok Looks good", "comment none",
		"memory ok Ship on Friday", "memory none",
	}
	for i, e := range got {
		line := strings.TrimSpace(e.Kind + " " + e.Access + " " + e.Title)
		if line != want[i] {
			t.Errorf("%s %s: %q, want %q", refs[i].Kind, refs[i].ID, line, want[i])
		}
		if e.Access != AccessOK && (e.Data != nil || e.Title != "") {
			t.Errorf("%s %s says more than %q: %+v", refs[i].Kind, refs[i].ID, e.Access, e)
		}
	}
	if d := got[3].Data; d["channel_id"] != openChan || d["channel_name"] != "launch" || d["by_name"] != "Maya Lin" {
		t.Errorf("a post's data: %+v", d)
	}
	if d := got[10].Data; d["on_kind"] != "doc" || d["on_id"] != openDoc {
		t.Errorf("a comment's data: %+v", d)
	}
}

// TestReadManyReadsPastTheChipCap: the chips' endpoint takes MaxRefs a read;
// the work graph reads more through ReadMany, by the same readers.
func TestReadManyReadsPastTheChipCap(t *testing.T) {
	withRules(t)
	refs := make([]Ref, MaxRefs+1)
	for i := range refs {
		refs[i] = Ref{Kind: "doc", ID: openDoc}
	}
	if _, err := ReadEntities(context.Background(), reader, refs); err != ErrTooMany {
		t.Fatalf("ReadEntities took %d refs: %v", len(refs), err)
	}
	got, err := ReadMany(context.Background(), reader, refs)
	if err != nil || len(got) != len(refs) || got[MaxRefs].Access != AccessOK {
		t.Fatalf("ReadMany: %d entries, %v", len(got), err)
	}
}

var _ = dgraphStruct.DgraphUser{}
