package business

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// lockedBoardLink is a board link locked with password.
func lockedBoardLink(t *testing.T, password string) *guestModel.GuestGrant {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	h := string(hash)
	return &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceBoard, Capability: guestModel.CapabilityEdit,
		PasswordHash: &h, UnlockVersion: 1}
}

// to_win 1: a board link lets its guest view, comment or edit; nothing else,
// and no other kind of link gets edit.
func TestABoardLinkTakesViewCommentOrEdit(t *testing.T) {
	for _, role := range []string{"view", "comment", "edit"} {
		if got, err := BoardRole(role); err != nil || got != role {
			t.Errorf("BoardRole(%q) = %q, %v; want it as asked", role, got, err)
		}
		if got := linkCapability(guestModel.ResourceBoard, role); got != role {
			t.Errorf("a board link asked for %q is %q", role, got)
		}
	}
	for _, role := range []string{"", "full", "admin", "post", "join"} {
		if _, err := BoardRole(role); !errors.Is(err, ErrNoRole) {
			t.Errorf("BoardRole(%q) = %v, want ErrNoRole", role, err)
		}
	}
	for _, kind := range []string{guestModel.ResourceDoc, guestModel.ResourceTable, guestModel.ResourceChannel, guestModel.ResourceProject} {
		if got := linkCapability(kind, guestModel.CapabilityEdit); got != guestModel.CapabilityView {
			t.Errorf("a %s link asked for edit is %q, want view", kind, got)
		}
	}
}

// to_win 2 + M-3: a board link's password is the docs' (C6): the wrong one is
// refused, the right one gives an unlock that opens the link afterwards.
func TestM3_ABoardLinkPasswordOpensOnlyWithTheRightOne(t *testing.T) {
	withSecret(t, "board-links-secret")
	countTries(t)
	ctx := context.Background()
	g := lockedBoardLink(t, "orchid-42")
	if _, err := OpenLocked(ctx, g, "", "", "203.0.113.7"); !errors.Is(err, ErrPasswordNeeded) {
		t.Fatalf("no password: %v, want ErrPasswordNeeded", err)
	}
	if _, err := OpenLocked(ctx, g, "tulip-42", "", "203.0.113.7"); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("a wrong password: %v, want ErrWrongPassword", err)
	}
	unlock, err := OpenLocked(ctx, g, "orchid-42", "", "203.0.113.7")
	if err != nil || unlock == "" {
		t.Fatalf("the right password: %q, %v; want an unlock", unlock, err)
	}
	if !Unlocked(g, unlock) || Unlocked(g, "") {
		t.Fatal("the unlock should open the link, and nothing else should")
	}
	if again, err := OpenLocked(ctx, g, "", unlock, "198.51.100.9"); err != nil || again != unlock {
		t.Fatalf("an unlock opens the link without the password again: %q, %v", again, err)
	}
	g.UnlockVersion++ // a new password
	if Unlocked(g, unlock) {
		t.Fatal("an unlock given under the old password still opens the link")
	}
	open := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceBoard, Capability: guestModel.CapabilityView}
	if got, err := OpenLocked(ctx, open, "", "", "203.0.113.7"); err != nil || got != "" || !Unlocked(open, "") {
		t.Fatalf("a link without a password opens as it is: %q, %v", got, err)
	}
}

// M-3: password tries are limited per address (the 11th from one address is
// refused, even with the right password) and per link (past its ceiling each
// new address gets one try).
func TestM3_PasswordTriesAreLimitedByLinkAndByIP(t *testing.T) {
	withSecret(t, "board-links-secret")
	countTries(t)
	ctx := context.Background()
	g := lockedBoardLink(t, "orchid-42")
	for i := 1; i <= unlockTriesPerWindow; i++ {
		if _, err := OpenLocked(ctx, g, "wrong-guess", "", "203.0.113.7"); !errors.Is(err, ErrWrongPassword) {
			t.Fatalf("try %d: %v, want ErrWrongPassword", i, err)
		}
	}
	if _, err := OpenLocked(ctx, g, "orchid-42", "", "203.0.113.7"); !errors.Is(err, ErrTooManyTries) {
		t.Fatalf("try %d from one address: %v, want ErrTooManyTries", unlockTriesPerWindow+1, err)
	}
	if _, err := OpenLocked(ctx, g, "orchid-42", "", "198.51.100.9"); err != nil {
		t.Fatalf("another address still opens the link: %v", err)
	}
	h := lockedBoardLink(t, "orchid-42")
	for i := 0; i < unlockTriesPerGrant; i++ {
		_, _ = OpenLocked(ctx, h, "wrong-guess", "", fmt.Sprintf("10.0.%d.%d", i/250, i%250))
	}
	fresh := "192.0.2.200"
	if _, err := OpenLocked(ctx, h, "wrong-guess", "", fresh); !errors.Is(err, ErrWrongPassword) {
		t.Fatalf("past the link's ceiling a new address gets one try: %v", err)
	}
	if _, err := OpenLocked(ctx, h, "orchid-42", "", fresh); !errors.Is(err, ErrTooManyTries) {
		t.Fatalf("past the link's ceiling a new address's second try: %v, want ErrTooManyTries", err)
	}
}

// to_win 2: a new board password follows the docs' rule (SecretProblem), and
// the stored hash is bcrypt's, never the password.
func TestABoardPasswordIsHashedUnderTheDocsRule(t *testing.T) {
	if _, err := hashSecret("password", "abc"); err == nil {
		t.Fatal("a three-letter password was taken")
	}
	h, err := hashSecret("password", "orchid-42")
	if err != nil || h == nil || *h == "orchid-42" || bcrypt.CompareHashAndPassword([]byte(*h), []byte("orchid-42")) != nil {
		t.Fatalf("hashSecret = %v, %v; want a bcrypt hash", h, err)
	}
}

// M-2 + how a change reaches open sessions: changing a live board link ends
// its guests' live sessions, so their tabs are authorised afresh.
func TestChangingABoardLinkEndsItsLiveSessions(t *testing.T) {
	ctx := context.Background()
	id := uuid.New()
	g := &guestModel.GuestGrant{Id: id, ResourceType: guestModel.ResourceBoard, Capability: guestModel.CapabilityView, UnlockVersion: 1}
	prevOn, prevGet, prevChange, prevEnd := guestOn, grantByID, changeLink, endLiveSessions
	t.Cleanup(func() { guestOn, grantByID, changeLink, endLiveSessions = prevOn, prevGet, prevChange, prevEnd })
	guestOn = func() bool { return true }
	grantByID = func(context.Context, uuid.UUID) (*guestModel.GuestGrant, error) { return g, nil }
	var written guestModel.LinkChange
	changeLink = func(_ context.Context, _ uuid.UUID, _ *guestModel.GuestGrant, c guestModel.LinkChange) (*guestModel.GuestGrant, error) {
		written = c
		out := *g
		out.Capability, out.PasswordHash = c.Capability, c.PasswordHash
		return &out, nil
	}
	var ended []uuid.UUID
	endLiveSessions = func(ids ...uuid.UUID) { ended = append(ended, ids...) }
	role, pw := "edit", "orchid-42"
	saved, err := ChangeBoardLink(ctx, id, BoardLinkChange{Capability: &role, Password: &pw})
	if err != nil || saved.Capability != "edit" || saved.PasswordHash == nil {
		t.Fatalf("ChangeBoardLink = %+v, %v", saved, err)
	}
	if !written.NewUnlock || written.PasswordHash == nil || *written.PasswordHash == pw {
		t.Errorf("a new password should be hashed and move the unlock version: %+v", written)
	}
	if len(ended) != 1 || ended[0] != id {
		t.Errorf("the link's live sessions ended: %v, want [%s]", ended, id)
	}
	bad := "owner"
	if _, err := ChangeBoardLink(ctx, id, BoardLinkChange{Capability: &bad}); !errors.Is(err, ErrNoRole) {
		t.Errorf("a role a board link doesn't take: %v", err)
	}
	// The write is a compare-and-set: not written while the link is still
	// live means someone changed it since it was read (409), never a 404.
	changeLink = func(context.Context, uuid.UUID, *guestModel.GuestGrant, guestModel.LinkChange) (*guestModel.GuestGrant, error) {
		return nil, nil
	}
	if _, err := ChangeBoardLink(ctx, id, BoardLinkChange{Capability: &role}); !errors.Is(err, ErrLinkChangedMeanwhile) {
		t.Errorf("a change that lost the race to another: %v, want ErrLinkChangedMeanwhile", err)
	}
	past := time.Now().Add(-time.Hour)
	g.ExpiresAt = &past
	if _, err := ChangeBoardLink(ctx, id, BoardLinkChange{Capability: &role}); !errors.Is(err, ErrNotABoardLink) {
		t.Errorf("changing an ended link: %v, want ErrNotABoardLink", err)
	}
}
