package business

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/helpers"
	departedModel "github.com/akashc777/OneCamp/models/postgres/Departed"
	userModel "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

var (
	dana  = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	cast  = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	maya  = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	quiet = uuid.MustParse("44444444-4444-4444-8444-444444444444")
	admin = accessBusiness.Subject{UserUUID: "55555555-5555-4555-8555-555555555555", Admin: true}
)

// world is a workspace where Dana left with docs and links, a member of the
// demo's cast left too, and someone left with nothing; Maya is still here.
type world struct {
	docs      map[string][]string // owner -> docs
	links     map[uuid.UUID]int
	broken    map[string]bool // docs whose recovery fails
	recovered []string
	told      []notificationBusiness.Notice
	toldWho   [][]notificationBusiness.NoticeRecipient
	counted   [][]uuid.UUID // who each count read was asked about
	rooms     map[uuid.UUID]int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	w := &world{
		docs:   map[string][]string{dana.String(): {"d1", "d2", "d3"}, cast.String(): {"c1"}},
		links:  map[uuid.UUID]int{dana: 2, cast: 1},
		rooms:  map[uuid.UUID]int{dana: 1},
		broken: map[string]bool{},
	}
	prev := []any{departedPeople, departedOne, activeMember, docsOwned, linkCounts, names, recoverDoc, moveLinks, revokeLinks, adminUsers, tell, activeAdmin, docCounts}
	t.Cleanup(func() {
		departedPeople = prev[0].(func(context.Context, int) ([]departedModel.Person, error))
		departedOne = prev[1].(func(context.Context, uuid.UUID) (*departedModel.Person, error))
		activeMember = prev[2].(func(context.Context, uuid.UUID) (bool, string, error))
		docsOwned = prev[3].(func(context.Context, string, int) ([]string, int, error))
		linkCounts = prev[4].(func(context.Context, []uuid.UUID) (map[uuid.UUID]int, error))
		names = prev[5].(func(context.Context, []string) (map[string]string, error))
		recoverDoc = prev[6].(func(context.Context, accessBusiness.Subject, string, string) error)
		moveLinks = prev[7].(func(context.Context, uuid.UUID, uuid.UUID) (int64, int64, error))
		revokeLinks = prev[8].(func(context.Context, uuid.UUID) (int64, error))
		adminUsers = prev[9].(func(context.Context) ([]*userModel.User, error))
		tell = prev[10].(func(context.Context, notificationBusiness.Notice, []notificationBusiness.NoticeRecipient) []string)
		activeAdmin = prev[11].(func(context.Context, string) bool)
		docCounts = prev[12].(func(context.Context, []uuid.UUID) (map[string]int, error))
	})
	left := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	people := []departedModel.Person{
		{ID: dana, Email: "dana@acme.test", LeftAt: left},
		{ID: cast, Email: "ravi" + helpers.DemoCastEmailDomain, LeftAt: left},
		{ID: quiet, Email: "quiet@acme.test", LeftAt: left},
	}
	departedPeople = func(context.Context, int) ([]departedModel.Person, error) { return people, nil }
	departedOne = func(_ context.Context, id uuid.UUID) (*departedModel.Person, error) {
		for _, p := range people {
			if p.ID == id {
				p := p
				return &p, nil
			}
		}
		return nil, nil
	}
	activeMember = func(_ context.Context, id uuid.UUID) (bool, string, error) { return id == maya, "maya@acme.test", nil }
	docsOwned = func(_ context.Context, owner string, first int) ([]string, int, error) {
		all := w.docs[owner]
		if first > len(all) {
			first = len(all)
		}
		return append([]string(nil), all[:first]...), len(all), nil
	}
	docCounts = func(_ context.Context, who []uuid.UUID) (map[string]int, error) {
		w.counted = append(w.counted, who)
		out := map[string]int{}
		for _, id := range who {
			if n := len(w.docs[id.String()]); n > 0 {
				out[id.String()] = n
			}
		}
		return out, nil
	}
	linkCounts = func(_ context.Context, who []uuid.UUID) (map[uuid.UUID]int, error) {
		w.counted = append(w.counted, who)
		out := map[uuid.UUID]int{}
		for _, id := range who {
			if n := w.links[id]; n > 0 {
				out[id] = n
			}
		}
		return out, nil
	}
	names = func(_ context.Context, ids []string) (map[string]string, error) {
		return map[string]string{dana.String(): "Dana Ross"}, nil
	}
	recoverDoc = func(_ context.Context, _ accessBusiness.Subject, doc, to string) error {
		if w.broken[doc] {
			return errors.New("the graph refused")
		}
		for owner, docs := range w.docs {
			for i, d := range docs {
				if d == doc {
					w.docs[owner] = append(docs[:i:i], docs[i+1:]...)
					w.docs[to] = append(w.docs[to], doc)
				}
			}
		}
		w.recovered = append(w.recovered, doc)
		return nil
	}
	moveLinks = func(_ context.Context, from, to uuid.UUID) (int64, int64, error) {
		n := w.links[from]
		w.links[to] += n
		delete(w.links, from)
		return int64(n), int64(w.rooms[from]), nil
	}
	revokeLinks = func(_ context.Context, id uuid.UUID) (int64, error) {
		n := w.links[id]
		delete(w.links, id)
		return int64(n), nil
	}
	adminUsers = func(context.Context) ([]*userModel.User, error) {
		return []*userModel.User{{Id: uuid.MustParse(admin.UserUUID), EmailID: "akash@acme.test"}, {Id: cast, EmailID: "lead" + helpers.DemoCastEmailDomain}}, nil
	}
	tell = func(_ context.Context, n notificationBusiness.Notice, r []notificationBusiness.NoticeRecipient) []string {
		w.told = append(w.told, n)
		w.toldWho = append(w.toldWho, r)
		return nil
	}
	activeAdmin = func(_ context.Context, id string) bool { return id == admin.UserUUID }
	return w
}

// An admin sees who left with work under their name: never the demo's own
// people, nor someone who left with nothing. Their docs and links are
// counted once for everyone, never a read per person, and the demo's own
// people aren't asked about.
func TestAdminsSeeWhoLeftWithWork(t *testing.T) {
	w := newWorld(t)
	people, err := List(context.Background())
	if err != nil || len(people) != 1 {
		t.Fatalf("%v, %+v", err, people)
	}
	if p := people[0]; p.UserUUID != dana.String() || p.Name != "Dana Ross" || p.Docs != 3 || p.Links != 2 {
		t.Fatalf("%+v", p)
	}
	if len(w.counted) != 2 {
		t.Fatalf("%d count reads, want one for docs and one for links", len(w.counted))
	}
	for _, asked := range w.counted {
		if len(asked) != 2 || asked[0] != dana || asked[1] != quiet {
			t.Fatalf("asked about %v, want Dana and the one who left with nothing", asked)
		}
	}
}

// A handover moves every doc and every link to someone in the workspace,
// and only an admin acting themselves may do it, only for someone who left.
func TestAHandoverMovesTheirDocsAndLinks(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	if _, err := HandOver(ctx, accessBusiness.Subject{UserUUID: admin.UserUUID}, dana, maya); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("a member: %v", err)
	}
	if _, err := HandOver(ctx, accessBusiness.Subject{UserUUID: admin.UserUUID, Admin: true, AgentUUID: "agent"}, dana, maya); !errors.Is(err, ErrNotAdmin) {
		t.Fatalf("an admin's agent: %v", err)
	}
	if _, err := HandOver(ctx, admin, maya, dana); !errors.Is(err, ErrNotDeparted) {
		t.Fatalf("someone still here: %v", err)
	}
	if _, err := HandOver(ctx, admin, dana, quiet); !errors.Is(err, ErrNotAMember) {
		t.Fatalf("to someone who left: %v", err)
	}
	m, err := HandOver(ctx, admin, dana, maya)
	// Her personal room closes rather than moving: it is hers, at her name.
	if err != nil || m.Docs != 3 || m.Links != 2 || m.Failed != 0 || m.RoomsClosed != 1 {
		t.Fatalf("%+v %v", m, err)
	}
	if len(w.docs[dana.String()]) != 0 || len(w.docs[maya.String()]) != 3 || w.links[maya] != 2 {
		t.Fatalf("docs %v, links %v", w.docs, w.links)
	}
}

// A doc that can't move is counted and left for the next run, never tried
// for ever: the handover ends.
func TestAHandoverLeavesADocItCantMove(t *testing.T) {
	w := newWorld(t)
	w.broken["d2"] = true
	m, err := HandOver(context.Background(), admin, dana, maya)
	if err != nil || m.Docs != 2 || m.Failed != 1 {
		t.Fatalf("%+v %v", m, err)
	}
	if strings.Join(w.docs[dana.String()], ",") != "d2" {
		t.Fatalf("left with Dana: %v", w.docs[dana.String()])
	}
}

// Turning their links off revokes them all; for an admin, for someone who left.
func TestTurningTheirLinksOff(t *testing.T) {
	w := newWorld(t)
	if _, err := TurnOffLinks(context.Background(), accessBusiness.Subject{Admin: false}, dana); !errors.Is(err, ErrNotAdmin) {
		t.Fatal(err)
	}
	if _, err := TurnOffLinks(context.Background(), admin, maya); !errors.Is(err, ErrNotDeparted) {
		t.Fatal(err)
	}
	if n, err := TurnOffLinks(context.Background(), admin, dana); err != nil || n != 2 || w.links[dana] != 0 {
		t.Fatalf("%d %v", n, err)
	}
}

// The sweep tells each real admin once per person, in the Inbox only; the
// demo's own people are never the subject or the reader.
func TestTheSweepTellsRealAdminsOnce(t *testing.T) {
	w := newWorld(t)
	if err := TellAdmins(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(w.told) != 1 {
		t.Fatalf("told %d times", len(w.told))
	}
	n, who := w.told[0], w.toldWho[0]
	if n.ResourceUUID != dana.String() || n.DedupKey != "departed:"+dana.String() || n.Title != "Dana Ross left with 3 docs and 2 links under their name" || n.Data["work"] != "3 docs and 2 links" {
		t.Fatalf("%+v", n)
	}
	if len(who) != 1 || who[0].UserUUID != admin.UserUUID || !who[0].InboxOnly {
		t.Fatalf("told %+v", who)
	}
}

// Only an admin's Inbox can open the notice, and it names the person.
func TestTheNoticeOpensForAdminsOnly(t *testing.T) {
	newWorld(t)
	open, titles, err := noticeNames(context.Background(), admin.UserUUID, []string{dana.String()})
	if err != nil || !open[dana.String()] || titles[dana.String()] != "Dana Ross" {
		t.Fatalf("%v %v %v", open, titles, err)
	}
	open, _, _ = noticeNames(context.Background(), maya.String(), []string{dana.String()})
	if open[dana.String()] {
		t.Fatal("a member opens it")
	}
}

func TestSummary(t *testing.T) {
	for _, c := range []struct {
		docs, links int
		want        string
	}{{3, 2, "3 docs and 2 links"}, {1, 0, "1 doc"}, {0, 1, "1 link"}, {12, 1, "12 docs and 1 link"}} {
		if got := Summary(c.docs, c.links); got != c.want {
			t.Errorf("%d, %d: %q", c.docs, c.links, got)
		}
	}
}
