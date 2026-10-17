package business

import (
	"context"
	"errors"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	docBusiness "github.com/akashc777/OneCamp/business/Doc"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	followModels "github.com/akashc777/OneCamp/models/postgres/Follow"
	"github.com/google/uuid"
)

const (
	docID   = "d0c00000-0000-4000-8000-000000000001"
	ownerID = "00000000-0000-4000-8000-0000000000a1"
	editID  = "00000000-0000-4000-8000-0000000000a2"
	talkID  = "00000000-0000-4000-8000-0000000000a3" // has commented
	readID  = "00000000-0000-4000-8000-0000000000a4"
	awayID  = "00000000-0000-4000-8000-0000000000a5" // can't open it
)

func user(id string) *dgraphStruct.DgraphUser {
	return &dgraphStruct.DgraphUser{Uuid: id, UserName: id[len(id)-2:]}
}

// privateDoc: an owner, an editor, a reader who commented, and a reader.
func privateDoc() *dgraphStruct.DgraphDoc {
	yes := true
	return &dgraphStruct.DgraphDoc{Uuid: docID, Title: "Launch notes", IsPrivate: &yes, CreatedBy: user(ownerID),
		EditingUser: []*dgraphStruct.DgraphUser{user(ownerID), user(editID)},
		ReadingUser: []*dgraphStruct.DgraphUser{user(talkID), user(readID)},
		Comments:    []*dgraphStruct.DgraphComment{{CommentBy: user(talkID)}}}
}

type followStore map[string]string

func withFollowSeams(t *testing.T, store followStore) {
	t.Helper()
	oldR, oldP, oldG, oldS, oldC := resolve, docPeople, getFollow, setFollow, clearFollow
	t.Cleanup(func() { resolve, docPeople, getFollow, setFollow, clearFollow = oldR, oldP, oldG, oldS, oldC })
	resolve = func(_ context.Context, who accessBusiness.Subject, _ accessBusiness.Resource) (accessBusiness.Decision, error) {
		switch who.UserUUID {
		case ownerID:
			return accessBusiness.Decision{Level: accessBusiness.LevelFull, Owner: true}, nil
		case editID:
			return accessBusiness.Decision{Level: accessBusiness.LevelEdit}, nil
		case talkID, readID:
			return accessBusiness.Decision{Level: accessBusiness.LevelView}, nil
		}
		return accessBusiness.Decision{}, nil
	}
	docPeople = func(context.Context, string) (*dgraphStruct.DgraphDoc, error) { return privateDoc(), nil }
	getFollow = func(_ context.Context, u uuid.UUID, _, _ string) (*followModels.Follow, error) {
		if m, ok := store[u.String()]; ok {
			return &followModels.Follow{Mode: m}, nil
		}
		return nil, followModels.ErrNotFound
	}
	setFollow = func(_ context.Context, u uuid.UUID, _, _, mode string) error { store[u.String()] = mode; return nil }
	clearFollow = func(_ context.Context, u uuid.UUID, _, _ string) error { delete(store, u.String()); return nil }
}

func who(id string) accessBusiness.Subject {
	return accessBusiness.Subject{UserUUID: id, UserUID: "0x" + id[len(id)-2:]}
}

// With no choice made, whoever takes part follows "replies and mentions" and
// everyone else "none"; someone who can't open the doc can't follow it.
func TestEveryoneHasAFollowModeByDefault(t *testing.T) {
	withFollowSeams(t, followStore{})
	for id, want := range map[string]string{ownerID: ModeMentions, editID: ModeMentions, talkID: ModeMentions, readID: ModeNone} {
		st, err := Get(context.Background(), who(id), "doc", docID)
		if err != nil || st.Mode != want || st.Chosen || st.Default != want {
			t.Errorf("%s: %+v, %v; want %s by default", id, st, err, want)
		}
	}
	if _, err := Get(context.Background(), who(awayID), "doc", docID); !errors.Is(err, ErrNotFound) {
		t.Errorf("someone who can't open the doc: %v, want ErrNotFound", err)
	}
	if _, err := Get(context.Background(), who(readID), "channel", docID); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("a kind with no follows: %v", err)
	}
}

// A choice is kept until it's cleared; a mode outside the three is refused.
func TestAFollowIsChosenAndCleared(t *testing.T) {
	store := followStore{}
	withFollowSeams(t, store)
	ctx := context.Background()
	if _, err := Set(ctx, who(readID), "doc", docID, "loud"); !errors.Is(err, ErrBadMode) {
		t.Errorf("an unknown mode: %v", err)
	}
	st, err := Set(ctx, who(readID), "doc", docID, ModeAll)
	if err != nil || st.Mode != ModeAll || !st.Chosen || st.Default != ModeNone {
		t.Fatalf("set: %+v, %v", st, err)
	}
	if st, _ := Get(ctx, who(readID), "doc", docID); st.Mode != ModeAll || !st.Chosen {
		t.Errorf("read back: %+v", st)
	}
	st, err = Clear(ctx, who(readID), "doc", docID)
	if err != nil || st.Mode != ModeNone || st.Chosen {
		t.Errorf("cleared: %+v, %v", st, err)
	}
	if _, err := Set(ctx, who(awayID), "doc", docID, ModeAll); !errors.Is(err, ErrNotFound) {
		t.Errorf("following a doc you can't open: %v", err)
	}
}

// A doc that isn't there answers like one you can't open.
func TestFollowingAMissingDocIsNotFound(t *testing.T) {
	withFollowSeams(t, followStore{})
	resolve = func(context.Context, accessBusiness.Subject, accessBusiness.Resource) (accessBusiness.Decision, error) {
		return accessBusiness.Decision{}, docBusiness.ErrNotFound
	}
	if _, err := Get(context.Background(), who(readID), "doc", docID); !errors.Is(err, ErrNotFound) {
		t.Errorf("a missing doc: %v", err)
	}
}
