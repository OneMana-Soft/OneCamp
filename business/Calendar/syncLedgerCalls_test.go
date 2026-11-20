package Calendar

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	"github.com/google/uuid"
)

// connectorStub stands in for a person's connector tokens: who was
// disconnected, from what, and whether each still holds its tokens.
type connectorStub struct {
	calls     []connectorCall
	connected map[connectorCall]bool
	// failNext makes the next disconnect fail, its tokens kept.
	failNext bool
}

type connectorCall struct {
	person    uuid.UUID
	connector string
}

func stubConnector(t *testing.T) *connectorStub {
	t.Helper()
	c := &connectorStub{connected: map[connectorCall]bool{}}
	prevDisconnect, prevConnected := disconnectConnector, connectorConnected
	t.Cleanup(func() { disconnectConnector, connectorConnected = prevDisconnect, prevConnected })
	disconnectConnector = func(_ context.Context, person uuid.UUID, connector string) error {
		k := connectorCall{person, connector}
		c.calls = append(c.calls, k)
		if c.failNext {
			c.failNext = false
			return errors.New("the revoke timed out")
		}
		c.connected[k] = false
		return nil
	}
	connectorConnected = func(_ context.Context, person uuid.UUID, connector string) bool {
		return c.connected[connectorCall{person, connector}]
	}
	return c
}

func (h *harness) brokenEvents() int {
	n := 0
	for _, e := range h.emitted {
		if e.Kind == resourceEventBusiness.CalendarSyncLinkBroken {
			n++
		}
	}
	return n
}

// A page of three links from two people is one ledger read and one ask for
// the two people; a break clears its person's tokens once and is audited
// once; sent again it does neither, unless the tokens outlived a failed
// clear; an id no Google or Outlook link has isn't there.
func TestSyncLedgerReadsOnceAndBreaksOnce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, maya, ada := h.member("Sam"), h.member("Maya"), h.member("Ada")
	ledger := newMemLedger(h.store, func() time.Time { return h.clock })
	h.s.ledger = ledger
	link := func(who uuid.UUID, provider string) *models.SyncLink {
		t.Helper()
		l, err := ledger.ensureLink(ctx, who, provider)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	samGoogle, _, mayaGoogle := link(personOf(sam), "google"), link(personOf(sam), "microsoft"), link(personOf(maya), "google")
	mayaICS := link(personOf(maya), "ics")
	people := h.s.people
	var asked [][]string
	h.s.people = func(ctx context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		asked = append(asked, ids)
		return people(ctx, ids)
	}
	page, err := h.s.SyncLedger(ctx, ada, url.Values{})
	if err != nil || len(page.Links) != 3 {
		t.Fatalf("the ledger: %+v %v", page, err)
	}
	if ledger.pageCalls != 1 || len(asked) != 1 || len(asked[0]) != 2 {
		t.Errorf("3 links of 2 people: %d ledger reads, people asked %v; want 1 read, 1 ask for 2", ledger.pageCalls, asked)
	}

	c := stubConnector(t)
	samKey, mayaKey := connectorCall{personOf(sam), connectorBusiness.ProviderCalendar}, connectorCall{personOf(maya), connectorBusiness.ProviderCalendar}
	c.connected[samKey], c.connected[mayaKey] = true, true
	if err := h.s.BreakSyncLink(ctx, ada, samGoogle.ID.String()); err != nil {
		t.Fatal(err)
	}
	if len(c.calls) != 1 || c.calls[0] != samKey || h.brokenEvents() != 1 {
		t.Fatalf("Ada's break of Sam's link: disconnects %+v, %d broken events", c.calls, h.brokenEvents())
	}
	if e := h.emitted[len(h.emitted)-1]; e.ResourceUUID != samGoogle.ID.String() || e.Actor.UserUUID != personOf(ada).String() ||
		len(e.Subjects) != 1 || e.Subjects[0] != personOf(sam).String() {
		t.Errorf("the break's event: %+v", e)
	}
	if err := h.s.BreakSyncLink(ctx, ada, samGoogle.ID.String()); err != nil || len(c.calls) != 1 || h.brokenEvents() != 1 {
		t.Errorf("the break sent again: %v, disconnects %+v, %d broken events; want nothing done", err, c.calls, h.brokenEvents())
	}

	// A clear that fails keeps the tokens: the break sent again clears them
	// and is audited then, once.
	c.failNext = true
	if err := h.s.BreakSyncLink(ctx, ada, mayaGoogle.ID.String()); err == nil || h.brokenEvents() != 1 {
		t.Fatalf("a break whose clear failed: %v, %d broken events", err, h.brokenEvents())
	}
	if err := h.s.BreakSyncLink(ctx, ada, mayaGoogle.ID.String()); err != nil || len(c.calls) != 3 || h.brokenEvents() != 2 {
		t.Errorf("the retry: %v, disconnects %+v, %d broken events", err, c.calls, h.brokenEvents())
	}
	if err := h.s.BreakSyncLink(ctx, ada, mayaGoogle.ID.String()); err != nil || len(c.calls) != 3 || h.brokenEvents() != 2 {
		t.Errorf("the retry sent again: %v, disconnects %+v, %d broken events", err, c.calls, h.brokenEvents())
	}

	for _, id := range []string{uuid.New().String(), "not-an-id", mayaICS.ID.String()} {
		if err := h.s.BreakSyncLink(ctx, ada, id); !errors.Is(err, ErrNoSuchSyncLink) {
			t.Errorf("a break of %s: %v, want ErrNoSuchSyncLink", id, err)
		}
	}
}

// An admin's break while the first move is still running stays: the move's
// end doesn't make the link synced again.
func TestAnAdminBreakOutlivesARunningFirstMove(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	sam, ada := h.member("Sam"), h.member("Ada")
	ledger := newMemLedger(h.store, func() time.Time { return h.clock })
	h.s.ledger = ledger
	stubConnector(t)
	l, _, err := ledger.startMove(ctx, personOf(sam), "google", "sam@example.com")
	if err != nil || l.State != models.SyncMoving {
		t.Fatalf("the move: %+v %v", l, err)
	}
	if err := h.s.BreakSyncLink(ctx, ada, l.ID.String()); err != nil {
		t.Fatal(err)
	}
	if err := ledger.finish(ctx, l.ID, models.SyncSynced, 4, 0, nil, ""); err != nil {
		t.Fatal(err)
	}
	if got := ledger.byID(l.ID); got.State != models.SyncBroken || got.Error != syncAdminBrokeWhy {
		t.Errorf("the move's end undid the break: %+v", got)
	}
}
