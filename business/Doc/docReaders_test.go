package businness

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/akashc777/OneCamp/business/Access/level"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	accessModels "github.com/akashc777/OneCamp/models/postgres/Access"
	"github.com/google/uuid"
)

// The people of readersWorld, by uuid.
const (
	rOlu    = "00000000-0000-4000-8000-0000000000a1" // owns it
	rAnn    = "00000000-0000-4000-8000-0000000000a2" // full access
	rDi     = "00000000-0000-4000-8000-0000000000a5" // view
	rEve    = "00000000-0000-4000-8000-0000000000a6" // in the Design team
	rFay    = "00000000-0000-4000-8000-0000000000a7" // in the Design team
	rGus    = "00000000-0000-4000-8000-0000000000a8" // in project P, the doc's space
	rZed    = "00000000-0000-4000-8000-0000000000a9" // nothing
	rDesign = "00000000-0000-4000-8000-0000000000d1"
)

// readersWorld is a private doc in project P's space as ReadSharing reads it:
// Olu owns it, Ann has full access, Di may view, the Design team (Eve, Fay)
// may comment, and P's members (Gus) may edit, its general level.
func readersWorld() *Sharing {
	return &Sharing{
		DocUUID: "d0c00000-0000-4000-8000-000000000001", Owner: &dgraphStruct.DgraphUser{Uuid: rOlu},
		People: map[string]level.Level{rAnn: level.Full, rDi: level.View},
		Names:  map[string]*dgraphStruct.DgraphUser{},
		Teams: map[string]*TeamShare{rDesign: {UUID: rDesign, Name: "Design", Level: level.Comment,
			Members: []string{rEve, rFay}}},
		General: GeneralAccess{Private: true, Level: level.Edit},
		Space:   Space{Kind: "project", UUID: "p", Members: map[string]bool{rGus: true}},
	}
}

func ends(kind, id string, at time.Time) *accessModels.Expiry {
	return &accessModels.Expiry{ResourceKind: "doc", PrincipalKind: kind, PrincipalUUID: uuid.MustParse(id), Level: "view", ExpiresAt: at}
}

func withReaders(t *testing.T, s func() (*Sharing, error), e []*accessModels.Expiry, endsErr error) *int {
	t.Helper()
	oldS, oldE, oldN := readersSharing, readersEnding, readersNow
	t.Cleanup(func() { readersSharing, readersEnding, readersNow = oldS, oldE, oldN })
	reads := 0
	readersSharing = func(context.Context, string) (*Sharing, error) {
		reads++
		return s()
	}
	readersEnding = func(context.Context, string) ([]*accessModels.Expiry, error) { return e, endsErr }
	readersNow = func() time.Time { return noonR }
	return &reads
}

var noonR = time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

// Who may read a doc, of many people at once, is the access rule from the
// doc's side: its owner and people by name, a team's members, its space's
// members, nobody else; one read of its sharing for all of them, and each
// keyed as asked.
func TestReadersAreTheAccessRuleFromTheDocsSide(t *testing.T) {
	reads := withReaders(t, func() (*Sharing, error) { return readersWorld(), nil }, nil, nil)
	all := []string{rOlu, rAnn, rDi, rEve, rFay, rGus, rZed, "", strings.ToUpper(rEve)}
	got, err := DecidePeople(context.Background(), "doc", all)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]level.Level{rOlu: level.Full, rAnn: level.Full, rDi: level.View, rEve: level.Comment, rFay: level.Comment,
		rGus: level.Edit, rZed: level.None, strings.ToUpper(rEve): level.Comment}
	if len(got) != len(want) {
		t.Errorf("decided %d people, want %d (an empty id is left out): %v", len(got), len(want), got)
	}
	for id, l := range want {
		if got[id].Level != l {
			t.Errorf("%s: %s, want %s", id, got[id].Level, l)
		}
	}
	if *reads != 1 {
		t.Errorf("the sharing was read %d times for %d people, want once", *reads, len(all))
	}
	can, err := Readers(context.Background(), "doc", all)
	if err != nil || !can[rEve] || !can[rGus] || can[rZed] {
		t.Errorf("readers %v, %v: the team's member and the space's member read it, nobody else", can, err)
	}
	*reads = 0
	if got, err := DecidePeople(context.Background(), "doc", nil); err != nil || len(got) != 0 || *reads != 0 {
		t.Errorf("asking about nobody read the doc (%d) or answered %v %v", *reads, got, err)
	}
}

// A grant whose end has passed counts as gone, before the sweep takes it
// away: Di's own, and the Design team's for both its members. A grant that
// ends later, and the owner, are untouched.
func TestReadersLeaveOutGrantsThatHaveEnded(t *testing.T) {
	past, later := noonR.Add(-time.Second), noonR.Add(time.Hour)
	withReaders(t, func() (*Sharing, error) { return readersWorld(), nil },
		[]*accessModels.Expiry{ends("user", rDi, past), ends("team", rDesign, noonR), ends("user", rAnn, later), ends("user", rOlu, past)}, nil)
	can, err := Readers(context.Background(), "doc", []string{rOlu, rAnn, rDi, rEve, rFay, rGus})
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]bool{rOlu: true, rAnn: true, rDi: false, rEve: false, rFay: false, rGus: true} {
		if can[id] != want {
			t.Errorf("%s may read: %v, want %v", id, can[id], want)
		}
	}

	// withoutEnded leaves the sharing it was given alone.
	s := readersWorld()
	after := withoutEnded(s, []*accessModels.Expiry{ends("user", rDi, past), ends("team", rDesign, past)}, noonR)
	if s.People[rDi] != level.View || s.Teams[rDesign] == nil || after.People[rDi] != level.None || after.Teams[rDesign] != nil {
		t.Errorf("before %v %v, after %v %v", s.People, s.Teams, after.People, after.Teams)
	}
	if withoutEnded(s, []*accessModels.Expiry{nil, ends("user", rDi, later)}, noonR) != s {
		t.Error("with no grant ended, the sharing should be the one given")
	}
}

// Someone taken out of the doc's space, or off a doc in the trash, reads
// nothing; and when the sharing or the grants that end can't be read, nobody
// is decided for.
func TestReadersOfALeftSpaceATrashedDocAndAFailedRead(t *testing.T) {
	left := func() (*Sharing, error) {
		s := readersWorld()
		delete(s.Space.Members, rGus)
		return s, nil
	}
	withReaders(t, left, nil, nil)
	if can, _ := Readers(context.Background(), "doc", []string{rGus}); can[rGus] {
		t.Error("someone taken out of the doc's space still reads it")
	}

	trashed := func() (*Sharing, error) {
		s := readersWorld()
		s.Deleted = true
		return s, nil
	}
	withReaders(t, trashed, nil, nil)
	if can, _ := Readers(context.Background(), "doc", []string{rOlu, rAnn}); can[rOlu] || can[rAnn] {
		t.Errorf("a doc in the trash is read by %v", can)
	}

	gone := errors.New("graph down")
	withReaders(t, func() (*Sharing, error) { return nil, gone }, nil, nil)
	if can, err := Readers(context.Background(), "doc", []string{rOlu}); !errors.Is(err, gone) || can != nil {
		t.Errorf("an unreadable sharing answered %v, %v", can, err)
	}
	withReaders(t, func() (*Sharing, error) { return readersWorld(), nil }, nil, errNoStore)
	if can, err := Readers(context.Background(), "doc", []string{rOlu}); !errors.Is(err, errNoStore) || can != nil {
		t.Errorf("unreadable ends answered %v, %v", can, err)
	}
}

// The person's own decisions agree with the doc's side: a doc holding a grant
// whose end has passed is decided again from its side, so Di (whose view
// ended) can't open it in the minute before the sweep; with no uuid to decide
// by, nobody can; a doc with no ended grant is left as decided, unread.
func TestOpeningADocLeavesOutGrantsThatHaveEnded(t *testing.T) {
	withReaders(t, func() (*Sharing, error) { return readersWorld(), nil },
		[]*accessModels.Expiry{ends("user", rDi, noonR.Add(-time.Second))}, nil)
	old := endedOn
	t.Cleanup(func() { endedOn = old })
	endedOn = func(context.Context, []string) (map[string]bool, error) { return map[string]bool{"ended": true}, nil }
	given := map[string]DocDecision{"ended": {Level: level.View, Unlocked: level.View}, "other": {Level: level.Edit}}
	got, err := withEndedGrants(context.Background(), Viewer{UUID: rDi}, given)
	if err != nil || got["ended"].Level != level.None || got["other"].Level != level.Edit {
		t.Errorf("di: %+v %v", got, err)
	}
	got, _ = withEndedGrants(context.Background(), Viewer{UID: "0x9"}, map[string]DocDecision{"ended": {Level: level.Full, Unlocked: level.Full}})
	if got["ended"].Level != level.None {
		t.Errorf("with no uuid: %+v", got["ended"])
	}
	endedOn = func(context.Context, []string) (map[string]bool, error) { return nil, errNoStore }
	if _, err := withEndedGrants(context.Background(), Viewer{UUID: rDi}, map[string]DocDecision{"x": {}}); err == nil {
		t.Error("an unreadable expiry table said something")
	}
}
