package business

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"testing"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Reaction"
)

const (
	maya  = "0x10"
	jonas = "0x11"
	post  = "0x20"
)

// graph is a fake of the reactions the domain reads and writes, for one
// parent: what Add, RemoveMine and Replace decide on, and what they wrote.
type graph struct {
	held     []domain.Held
	added    []domain.NewReaction
	removed  []string
	retagged []string
	next     int
	// twin is a reaction of the same person and emoji that lands between
	// the read and the write (the key's @upsert answers with it).
	twin string
	gone bool // no such parent
}

func useGraph(t *testing.T, g *graph) {
	t.Helper()
	wasOn, wasAdd, wasRetag, wasRemove := reactionsOn, addOnce, retag, removeFrom
	reactionsOn = func(_ context.Context, edge, parentUUID string) (string, []domain.Held, error) {
		if g.gone {
			return "", nil, domain.ErrNoParent
		}
		return post, slices.Clone(g.held), nil
	}
	addOnce = func(_ context.Context, edge, parentUUID, parentUID string, r domain.NewReaction) (string, bool, error) {
		if g.twin != "" {
			return g.twin, false, nil
		}
		g.next++
		uid := fmt.Sprintf("0x9%d", g.next)
		g.added = append(g.added, r)
		g.held = append(g.held, domain.Held{UID: uid, Emoji: r.Emoji, By: r.By})
		return uid, true, nil
	}
	retag = func(_ context.Context, edge, parentUID, uid, by, emoji string, _ time.Time) (bool, error) {
		if g.twin != "" {
			return false, nil
		}
		for i := range g.held {
			if g.held[i].UID == uid {
				g.held[i].Emoji = emoji
				g.retagged = append(g.retagged, uid+":"+emoji)
				return true, nil
			}
		}
		return false, nil
	}
	removeFrom = func(_ context.Context, edge, parentUID string, uids []string) error {
		g.removed = append(g.removed, uids...)
		g.held = slices.DeleteFunc(g.held, func(h domain.Held) bool { return slices.Contains(uids, h.UID) })
		return nil
	}
	t.Cleanup(func() { reactionsOn, addOnce, retag, removeFrom = wasOn, wasAdd, wasRetag, wasRemove })
}

var onPost = Parent{On: OnPost, UUID: "p-1", ContentBy: jonas}

// Several reactions per person, each emoji once (D-M7): a second emoji is a
// second reaction, never a replacement, and adding an emoji you already have
// writes nothing and answers the one you have.
func TestAPersonAddsSeveralEmojiEachOnce(t *testing.T) {
	g := &graph{}
	useGraph(t, g)
	ctx := context.Background()

	thumbs, err := Add(ctx, onPost, maya, "+1")
	if err != nil || !thumbs.Created || !thumbs.First {
		t.Fatalf("first reaction: %+v %v", thumbs, err)
	}
	tada, err := Add(ctx, onPost, maya, "tada")
	if err != nil || !tada.Created || tada.First || tada.UID == thumbs.UID {
		t.Fatalf("second emoji: %+v %v (it must be a reaction of its own, and not her first)", tada, err)
	}
	again, err := Add(ctx, onPost, maya, "+1")
	if err != nil || again.Created || again.UID != thumbs.UID {
		t.Fatalf("the same emoji again: %+v %v, want her %s and nothing written", again, err, thumbs.UID)
	}
	if len(g.added) != 2 || len(g.held) != 2 {
		t.Fatalf("wrote %d, holds %d; want 2 and 2", len(g.added), len(g.held))
	}
	if g.added[0].ContentBy != jonas || g.added[0].By != maya {
		t.Fatalf("the reaction names who added it and whose message it is: %+v", g.added[0])
	}

	// Jonas's first is his, whatever Maya has.
	his, err := Add(ctx, onPost, jonas, "+1")
	if err != nil || !his.Created || !his.First {
		t.Fatalf("someone else's first: %+v %v", his, err)
	}
}

// Of two adds of one reaction at once, the one that lands second answers the
// first one's reaction, and says it wrote nothing (so nothing is told twice).
func TestATwinAddAnswersTheReactionThatLanded(t *testing.T) {
	g := &graph{twin: "0x77"}
	useGraph(t, g)
	got, err := Add(context.Background(), onPost, maya, "eyes")
	if err != nil || got.Created || got.First || got.UID != "0x77" {
		t.Fatalf("got %+v %v, want the twin 0x77 and nothing created", got, err)
	}
}

// The caps: 23 emoji a person, 50 different emoji a message. An emoji the
// message already has can always be added under 50, and a cap refuses in
// words, as a 409.
func TestTheCapsRefuseInWords(t *testing.T) {
	ctx := context.Background()

	full := &graph{}
	for i := 0; i < MaxPerPerson; i++ {
		full.held = append(full.held, domain.Held{UID: fmt.Sprintf("0x1%02d", i), Emoji: fmt.Sprintf("e%d", i), By: maya})
	}
	useGraph(t, full)
	if _, err := Add(ctx, onPost, maya, "rocket"); !refusedWith(err, http.StatusConflict, "You can add up to 23 reactions to one message.") {
		t.Fatalf("a 24th emoji: %v", err)
	}
	if got, err := Add(ctx, onPost, maya, "e3"); err != nil || got.Created {
		t.Fatalf("one she has, at the cap: %+v %v (it answers hers)", got, err)
	}

	many := &graph{}
	for i := 0; i < MaxDistinct; i++ {
		many.held = append(many.held, domain.Held{UID: fmt.Sprintf("0x2%02d", i), Emoji: fmt.Sprintf("e%d", i), By: fmt.Sprintf("0x3%02d", i)})
	}
	useGraph(t, many)
	if _, err := Add(ctx, onPost, maya, "rocket"); !refusedWith(err, http.StatusConflict, "This message already has 50 different reactions. Add one of those instead.") {
		t.Fatalf("a 51st emoji: %v", err)
	}
	if got, err := Add(ctx, onPost, maya, "e7"); err != nil || !got.Created {
		t.Fatalf("one of the fifty: %+v %v", got, err)
	}
	if len(many.added) != 1 {
		t.Fatalf("wrote %d, want only the one of the fifty", len(many.added))
	}
}

// An emoji id is emoji-mart's, or a workspace emoji's: never markup or a
// long string, which a client could store on every message.
func TestOnlyAnEmojiIdIsAdded(t *testing.T) {
	g := &graph{}
	useGraph(t, g)
	for _, bad := range []string{"", "<b>x</b>", "Thumbs Up", "a/b", string(make([]byte, 101))} {
		if _, err := Add(context.Background(), onPost, maya, bad); !refusedWith(err, http.StatusBadRequest, "That isn't an emoji we know.") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	for _, good := range []string{"+1", "-1", "100", "white_check_mark", "thumbsup::skin-tone-2", "flag-gb"} {
		if !ValidEmoji(good) {
			t.Errorf("%q is an emoji id", good)
		}
	}
	if len(g.added) != 0 {
		t.Fatalf("wrote %d for bad ids", len(g.added))
	}
}

// Taking back a reaction takes back every copy of that emoji by the person
// (an add from before this made a node each time), and nobody else's; a
// reaction that isn't theirs, or isn't on the message, is refused.
func TestRemoveTakesBackEveryCopyOfYourEmojiAndOnlyYours(t *testing.T) {
	g := &graph{held: []domain.Held{
		{UID: "0xa1", Emoji: "+1", By: maya},
		{UID: "0xa2", Emoji: "+1", By: maya}, // a twin from before
		{UID: "0xa3", Emoji: "tada", By: maya},
		{UID: "0xa4", Emoji: "+1", By: jonas},
	}}
	useGraph(t, g)
	ctx := context.Background()

	if _, err := RemoveMine(ctx, onPost, maya, "0xa4"); !refusedWith(err, http.StatusForbidden, "You can only change or remove your own reaction.") {
		t.Fatalf("someone else's: %v", err)
	}
	if _, err := RemoveMine(ctx, onPost, maya, "0xff"); !refusedWith(err, http.StatusNotFound, "There's no such reaction.") {
		t.Fatalf("one the message doesn't hold: %v", err)
	}
	removed, err := RemoveMine(ctx, onPost, maya, "0xa1")
	if err != nil {
		t.Fatal(err)
	}
	var uids []string
	for _, r := range removed {
		uids = append(uids, r.UID)
		if r.Emoji != "+1" {
			t.Errorf("removed %+v names the wrong emoji", r)
		}
	}
	if !slices.Equal(uids, []string{"0xa1", "0xa2"}) || !slices.Equal(g.removed, []string{"0xa1", "0xa2"}) {
		t.Fatalf("removed %v (wrote %v), want both of Maya's 👍 and nothing else", uids, g.removed)
	}
	if len(g.held) != 2 {
		t.Fatalf("left %v, want her 🎉 and Jonas's 👍", g.held)
	}
}

// An app from before several reactions changes its one reaction, and keeps
// its node to take it back by later: so the node takes the new emoji in
// place, and the person's other copies of the old one go. When they already
// have the new emoji, the old one goes and theirs answers; when the old one
// is gone already (another tab took it back), the new one is added.
func TestReplaceKeepsTheNodeTheAppHolds(t *testing.T) {
	ctx := context.Background()

	g := &graph{held: []domain.Held{{UID: "0xb1", Emoji: "+1", By: maya}, {UID: "0xb2", Emoji: "+1", By: maya}, {UID: "0xb3", Emoji: "eyes", By: maya}}}
	useGraph(t, g)
	added, removed, err := Set(ctx, onPost, maya, "0xb1", "heart")
	if err != nil || added.UID != "0xb1" || !added.Retagged || added.Created || added.First {
		t.Fatalf("replace: %+v %v", added, err)
	}
	if len(removed) != 1 || removed[0].UID != "0xb2" || !slices.Equal(g.retagged, []string{"0xb1:heart"}) {
		t.Fatalf("removed %+v, retagged %v; want the old copy gone and 0xb1 a heart", removed, g.retagged)
	}
	if added.Told() != 1 { // mqtt TYPE_UPDATE
		t.Fatalf("a retag is told as an update, got %d", added.Told())
	}

	// She has 👀 already: the change to 👀 leaves her one 👀.
	added, removed, err = Set(ctx, onPost, maya, "0xb1", "eyes")
	if err != nil || added.UID != "0xb3" || added.Created || added.Retagged || len(removed) != 1 || removed[0].UID != "0xb1" {
		t.Fatalf("replacing with one she has: %+v %+v %v", added, removed, err)
	}

	// Gone already: the new one is added.
	added, removed, err = Set(ctx, onPost, maya, "0xb1", "tada")
	if err != nil || !added.Created || len(removed) != 0 {
		t.Fatalf("replacing one that's gone: %+v %+v %v", added, removed, err)
	}

	// Never someone else's.
	if _, _, err := Set(ctx, onPost, jonas, "0xb3", "rocket"); !refusedWith(err, http.StatusForbidden, "You can only change or remove your own reaction.") {
		t.Fatalf("replacing someone else's: %v", err)
	}
}

// A change from an emoji several people hold to a new one adds a different
// emoji, so it is held to the cap of 50; a change from one only she holds
// isn't.
func TestReplaceKeepsTheCapOfDifferentEmoji(t *testing.T) {
	full := &graph{}
	for i := 0; i < MaxDistinct; i++ {
		full.held = append(full.held, domain.Held{UID: fmt.Sprintf("0x4%02d", i), Emoji: fmt.Sprintf("e%d", i), By: jonas})
	}
	full.held = append(full.held, domain.Held{UID: "0xc1", Emoji: "e0", By: maya})
	useGraph(t, full)
	if _, _, err := Set(context.Background(), onPost, maya, "0xc1", "rocket"); !refusedWith(err, http.StatusConflict, "This message already has 50 different reactions. Add one of those instead.") {
		t.Fatalf("a 51st emoji by a change: %v", err)
	}
	alone := &graph{held: slices.Clone(full.held[1:MaxDistinct])} // e0 is now hers alone
	alone.held = append(alone.held, domain.Held{UID: "0xc2", Emoji: "e0", By: maya})
	useGraph(t, alone)
	if added, _, err := Set(context.Background(), onPost, maya, "0xc2", "rocket"); err != nil || !added.Retagged {
		t.Fatalf("a change that leaves her emoji's place free: %+v %v", added, err)
	}
}

// A message that isn't there answers 404 in words; a graph that doesn't
// answer is a fault, logged and answered as "try again".
func TestAMissingMessageIsRefusedAndAFaultIsTryAgain(t *testing.T) {
	useGraph(t, &graph{gone: true})
	_, err := Add(context.Background(), onPost, maya, "+1")
	if !refusedWith(err, http.StatusNotFound, "That message isn't there any more.") || Fault(err) {
		t.Fatalf("gone: %v", err)
	}

	was := reactionsOn
	reactionsOn = func(context.Context, string, string) (string, []domain.Held, error) {
		return "", nil, errors.New("deadline exceeded")
	}
	t.Cleanup(func() { reactionsOn = was })
	_, err = Add(context.Background(), onPost, maya, "+1")
	if !Fault(err) {
		t.Fatalf("a graph that doesn't answer is a fault: %v", err)
	}
	if status, msg := Answer(err, "Couldn't add the reaction. Try again."); status != http.StatusServiceUnavailable || msg != "Couldn't add the reaction. Try again." {
		t.Fatalf("answered %d %q", status, msg)
	}
}

// The reaction's time is the server's.
func TestTheReactionIsStampedByTheServer(t *testing.T) {
	g := &graph{}
	useGraph(t, g)
	at := time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC)
	was := now
	now = func() time.Time { return at }
	t.Cleanup(func() { now = was })
	if _, err := Add(context.Background(), onPost, maya, "+1"); err != nil {
		t.Fatal(err)
	}
	if !g.added[0].At.Equal(at) {
		t.Fatalf("stamped %v, want %v", g.added[0].At, at)
	}
}

func refusedWith(err error, status int, msg string) bool {
	r, ok := AsRefusal(err)
	return ok && r.Status == status && r.Msg == msg
}
