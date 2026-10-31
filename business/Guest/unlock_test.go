package business

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	guestModel "github.com/akashc777/OneCamp/models/postgres/Guest"
	"github.com/akashc777/OneCamp/models/redis/registry"
	"github.com/google/uuid"
)

func withSecret(t *testing.T, secret string) {
	t.Helper()
	prev := unlockSecret
	t.Cleanup(func() { unlockSecret = prev })
	unlockSecret = func() string { return secret }
}

// A password's unlock opens its grant, at its version, until it runs out,
// and nothing else (D-25, condition 2: carried in the address, never a
// cookie). Moved here from business/Publish unlock_test.go with the unlock.
func TestAnUnlockOpensItsGrantAndNothingElse(t *testing.T) {
	withSecret(t, "test-secret-for-unlocks")
	at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	slug := "launch-notes"
	g := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceDoc, Capability: guestModel.CapabilityView, UnlockVersion: 3, Slug: &slug}
	token, lasts := MintUnlock(g, at)
	if token == "" || !strings.HasPrefix(token, "3.") || lasts != KindPage.UnlockFor || lasts != 12*time.Hour {
		t.Fatalf("token %q for %v", token, lasts)
	}
	if !Unlocks(token, g, at.Add(lasts-time.Second)) {
		t.Error("the unlock doesn't open its grant")
	}
	other := *g
	other.Id = uuid.New()
	moved := *g
	moved.UnlockVersion = 4
	for name, ok := range map[string]bool{
		"after it runs out":   Unlocks(token, g, at.Add(lasts)),
		"another link":        Unlocks(token, &other, at),
		"a new password":      Unlocks(token, &moved, at),
		"a changed signature": Unlocks(token[:len(token)-2]+"xx", g, at),
		"a later expiry":      Unlocks(strings.Replace(token, ".", ".9", 1), g, at),
		"nothing":             Unlocks("", g, at),
		"rubbish":             Unlocks("a.b.c", g, at),
		"too long":            Unlocks(strings.Repeat("1", 300), g, at),
		"no grant":            Unlocks(token, nil, at),
	} {
		if ok {
			t.Errorf("the unlock opens %s", name)
		}
	}

	withSecret(t, "another-secret")
	if Unlocks(token, g, at) {
		t.Error("an unlock signed with another secret opens the grant")
	}
	withSecret(t, "")
	if tok, _ := MintUnlock(g, at); tok != "" || Unlocks(token, g, at) {
		t.Error("a server with no secret gives or takes unlocks")
	}
}

// An unlock a doc's page gave before the move (signed under
// "onecamp/publish/unlock") still opens it, by its address or its token; a
// room's is signed under a key of its own, so the same grant id, version and
// expiry never make one kind's token open another's.
func TestUnlocksStayWithTheirKind(t *testing.T) {
	withSecret(t, "test-secret-for-unlocks")
	at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	id := uuid.New()
	slug := "launch-notes"
	page := &guestModel.GuestGrant{Id: id, ResourceType: guestModel.ResourceDoc, Capability: guestModel.CapabilityView, UnlockVersion: 2, Slug: &slug}
	link := &guestModel.GuestGrant{Id: id, ResourceType: guestModel.ResourceDoc, Capability: guestModel.CapabilityView, UnlockVersion: 2}

	// As business/Publish unlock.go signed it.
	exp := at.Add(12 * time.Hour).Unix()
	key := sha256.Sum256([]byte("onecamp/publish/unlock\x00test-secret-for-unlocks"))
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte(id.String() + "\x002\x00" + strconv.FormatInt(exp, 10)))
	before := "2." + strconv.FormatInt(exp, 10) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	if !Unlocks(before, page, at) || !Unlocks(before, link, at) {
		t.Fatal("an unlock given before the move stops opening its page")
	}
	if tok, _ := MintUnlock(page, at); tok != before {
		t.Fatalf("a page's unlock is signed differently now: %q, was %q", tok, before)
	}

	room := KindRoom
	room.UnlockFor = 12 * time.Hour
	if string(room.unlockKey()) == string(KindPage.unlockKey()) || string(KindLink.unlockKey()) != string(KindPage.unlockKey()) {
		t.Fatal("a room shares a page's key, or a page by its token doesn't")
	}
	if (Kind{Name: "room"}).unlockKey() == nil || string((Kind{Name: "room"}).unlockKey()) == string((Kind{Name: "other"}).unlockKey()) {
		t.Fatal("kinds without a label of their own share a key")
	}
}

// A kind that gives no unlocks (no UnlockFor yet) gives none: a room's row
// before calls fills it in.
func TestAKindWithoutALifetimeGivesNoUnlock(t *testing.T) {
	withSecret(t, "test-secret-for-unlocks")
	slug := "dana"
	room := &guestModel.GuestGrant{Id: uuid.New(), ResourceType: guestModel.ResourceMeeting, Slug: &slug}
	if KindRoom.UnlockFor == 0 {
		if tok, lasts := MintUnlock(room, time.Now()); tok != "" || lasts != 0 {
			t.Fatalf("a room without a lifetime gave %q for %v", tok, lasts)
		}
	}
}

// countTries stands in for the limits' counter: tries per key, each limit
// counted as Redis's fixed windows count it. It returns the counts.
func countTries(t *testing.T) map[string]int {
	t.Helper()
	prev := triesAllowed
	t.Cleanup(func() { triesAllowed = prev })
	counts := map[string]int{}
	triesAllowed = func(_ context.Context, spec registry.Spec, args []string, limit int) bool {
		key := spec.Build(args...)
		counts[key]++
		return counts[key] <= limit
	}
	return counts
}

// Tries are counted per address and grant, whichever kind's address they
// were made at.
func TestTriesAreCountedPerAddressAndGrant(t *testing.T) {
	countTries(t)
	g := &guestModel.GuestGrant{Id: uuid.New()}
	for i := 0; i < unlockTriesPerWindow; i++ {
		if !UnlockTryAllowed(context.Background(), "203.0.113.9", g) {
			t.Fatalf("try %d of the address's %d was refused", i+1, unlockTriesPerWindow)
		}
	}
	if UnlockTryAllowed(context.Background(), "203.0.113.9", g) {
		t.Fatal("a try past the address's limit was let through")
	}
	if !UnlockTryAllowed(context.Background(), "203.0.113.9", &guestModel.GuestGrant{Id: uuid.New()}) || !UnlockTryAllowed(context.Background(), "198.51.100.7", g) {
		t.Fatal("another grant, or another address, was refused")
	}
	if UnlockTryAllowed(context.Background(), "203.0.113.9", nil) {
		t.Fatal("no grant was tried")
	}
}

// Past the grant's ceiling, guesses from many addresses slow down instead of
// shutting the link: each new address gets one try, and no more.
func TestALinkPastItsCeilingSlowsGuessesInsteadOfShutting(t *testing.T) {
	countTries(t)
	g := &guestModel.GuestGrant{Id: uuid.New()}
	for i := 0; i < unlockTriesPerGrant; i++ {
		if !UnlockTryAllowed(context.Background(), fmt.Sprintf("10.0.%d.%d", i/250, i%250), g) {
			t.Fatalf("try %d, under the ceiling, was refused", i+1)
		}
	}
	if !UnlockTryAllowed(context.Background(), "203.0.113.9", g) {
		t.Fatal("past the ceiling, a new address's first try was refused: the link is shut")
	}
	if UnlockTryAllowed(context.Background(), "203.0.113.9", g) {
		t.Fatal("past the ceiling, an address got a second try")
	}
}

// An address past its own tries never counts against its link's ceiling, so
// one stranger can't bring the ceiling down on everyone.
func TestAnAddressPastItsTriesDoesNotSpendTheLinksCeiling(t *testing.T) {
	counts := countTries(t)
	g := &guestModel.GuestGrant{Id: uuid.New()}
	for i := 0; i < 3*unlockTriesPerWindow; i++ {
		UnlockTryAllowed(context.Background(), "198.51.100.7", g)
	}
	if got := counts[registry.PublishUnlockGrantRate.Build(g.Id.String())]; got != unlockTriesPerWindow {
		t.Fatalf("the link's ceiling counted %d tries, want the address's %d", got, unlockTriesPerWindow)
	}
}
