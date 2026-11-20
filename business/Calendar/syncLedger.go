package Calendar

// An admin's sync ledger (gcal-8 H1): every Google and Outlook link in the
// workspace, for a workspace admin (the /admin gate is the check), as link
// metadata and counts: whose, which address, its state (statusOf's), when
// last in step, stale since, why it isn't in step as a class, how many
// changes the provider refused and how many sends died. Never a token, an
// event or the provider's own words. An admin breaks a link the way its
// person disconnects it, audited, and the person is told in place; nothing
// here connects a link or acts as its person.

import (
	"context"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	"github.com/akashc777/OneCamp/helpers"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
)

// syncAdminBrokeWhy is what a person reads of their link an admin broke.
const syncAdminBrokeWhy = "An admin disconnected this. Connect it again to keep your calendars in step."

// ErrNoSuchSyncLink is an admin's break of an id no Google or Outlook link
// has: a bad id, a link gone, or one the ledger doesn't hold (ics). The
// controller answers it 404 "That sync link isn't there any more."
var ErrNoSuchSyncLink = errors.New("calendar: no such sync link")

// connectorConnected is whether a person's connector still holds tokens
// (connectorBusiness.IsConnected); swapped in tests.
var connectorConnected = connectorBusiness.IsConnected

// SyncLedgerRow is one Google or Outlook link as a workspace admin sees it.
type SyncLedgerRow struct {
	ID           string     `json:"id"`
	Person       Person     `json:"person"`
	Provider     string     `json:"provider"`
	Account      string     `json:"account,omitempty"`
	State        string     `json:"state"`
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
	StaleSince   *time.Time `json:"stale_since"`
	ErrorClass   string     `json:"error_class,omitempty"`
	Refusals     int        `json:"refusals"`
	DeadSends    int        `json:"dead_sends"`
}

// SyncLedger is a page of the workspace's links, and the next page's cursor.
type SyncLedger struct {
	Links []SyncLedgerRow `json:"links"`
	Next  string          `json:"next,omitempty"`
}

// KindSyncLink is a sync link as a Resource Event's resource.
const KindSyncLink = "calendar_sync_link"

// A ledger page holds ledgerPageSize links unless asked for fewer, and never
// more than ledgerPageMax.
const (
	ledgerPageSize = 50
	ledgerPageMax  = 100
)

// ErrNoSuchLedgerFilter is a ledger asked for by a state links don't have,
// or a provider calendars don't sync with both ways.
var ErrNoSuchLedgerFilter = inputError("Links are moving, synced, failed or broken, with Google or Outlook.")

// ledgerStates are the states a link has (models.Sync*).
var ledgerStates = map[string]bool{models.SyncMoving: true, models.SyncSynced: true, models.SyncFailed: true, models.SyncBroken: true}

// ledgerFilterOf is the page a ledger's query asks for: ?state= and
// ?provider= checked, ?limit= by the list helpers' rule (helpers.SizeParam),
// and ?cursor= the offset the last page handed out. Pure.
func ledgerFilterOf(q url.Values) (models.SyncLedgerFilter, error) {
	state, provider := q.Get("state"), q.Get("provider")
	if (state != "" && !ledgerStates[state]) || (provider != "" && syncWithOf(provider).connector == "") {
		return models.SyncLedgerFilter{}, ErrNoSuchLedgerFilter
	}
	return models.SyncLedgerFilter{State: state, Provider: provider, Limit: helpers.SizeParam(q, "limit", ledgerPageSize, ledgerPageMax),
		Offset: helpers.CursorOffset(q.Get("cursor"))}, nil
}

// readErrorClasses are the classes a pass keeps on a synced link
// (syncReadError).
var readErrorClasses = map[string]bool{readUnreachable: true, readRateLimited: true, readProviderError: true}

// syncErrorClass is why a link isn't in step, as a class, from its status
// (statusOf): a synced link's read error, which anything not a class reads
// as the provider's; a broken link, disconnected by an admin or by its
// provider; a move that failed. Never the words kept on the link. Pure.
func syncErrorClass(st *SyncStatus) string {
	switch {
	case st.ReadError != "" && readErrorClasses[st.ReadError]:
		return st.ReadError
	case st.ReadError != "":
		return readProviderError
	case st.State == models.SyncBroken && st.Error == syncAdminBrokeWhy:
		return "admin_disconnected"
	case st.State == models.SyncBroken:
		return "disconnected"
	case st.State == models.SyncFailed:
		return "move_failed"
	}
	return ""
}

// ledgerRowOf is r as an admin's ledger shows it at now: its state, when it
// was last in step and stale since are statusOf's, its person named. Pure.
func ledgerRowOf(r models.SyncLedgerRow, names map[string]*dgraphStruct.DgraphUser, now time.Time) SyncLedgerRow {
	st := statusOf(r.Link.Provider, &r.Link, now)
	return SyncLedgerRow{ID: r.Link.ID.String(), Person: personNamed(r.Link.PersonID, names), Provider: st.Provider, Account: st.Account,
		State: st.State, LastSyncedAt: st.LastSyncedAt, StaleSince: st.StaleSince, ErrorClass: syncErrorClass(st),
		Refusals: r.Refusals, DeadSends: r.DeadSends}
}

// SyncLedger is a page of the workspace's Google and Outlook links, for a
// workspace admin (the /admin gate is the check), with the next page's
// cursor while there may be more. q is the list's query (ledgerFilterOf).
func (s *service) SyncLedger(ctx context.Context, _ *model.UserInfo, q url.Values) (*SyncLedger, error) {
	f, err := ledgerFilterOf(q)
	if err != nil {
		return nil, err
	}
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := s.ledger.ledgerPage(ctx, f)
	if err != nil {
		return nil, err
	}
	people := make([]string, 0, len(rows))
	for _, r := range rows {
		if id := r.Link.PersonID.String(); !slices.Contains(people, id) {
			people = append(people, id)
		}
	}
	names, now := s.namesByID(ctx, people), s.now()
	out := &SyncLedger{Links: make([]SyncLedgerRow, 0, len(rows))}
	for _, r := range rows {
		out.Links = append(out.Links, ledgerRowOf(r, names, now))
	}
	if len(rows) == f.Limit {
		out.Next = helpers.OffsetCursor(f.Offset + f.Limit)
	}
	return out, nil
}

// disconnectConnector clears a person's connector tokens as their own
// disconnect does (connectorBusiness.Disconnect); swapped in tests.
var disconnectConnector = connectorBusiness.Disconnect

// BreakSyncLink is a workspace admin's break of one Google or Outlook link by
// its id: the link is broken whatever its state, with syncAdminBrokeWhy for
// its person to read on their own status; its tokens are cleared the way the
// person's own disconnect clears them; the audit log records who broke
// whose. Sent again, when the link is broken so already and its tokens are
// gone, it does nothing more and is not audited again; when the tokens
// outlived a clear that failed, it clears them and is audited then. It never
// connects anything. ErrNoSuchSyncLink for an id no such link has.
func (s *service) BreakSyncLink(ctx context.Context, admin *model.UserInfo, id string) error {
	link, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return ErrNoSuchSyncLink
	}
	if err := s.ready(ctx); err != nil {
		return err
	}
	l, again, err := s.ledger.breakByAdmin(ctx, link, syncAdminBrokeWhy)
	if err != nil {
		return err
	}
	if l == nil {
		return ErrNoSuchSyncLink
	}
	connector := syncWithOf(l.Provider).connector
	if again && !connectorConnected(ctx, l.PersonID, connector) {
		return nil
	}
	if err := disconnectConnector(ctx, l.PersonID, connector); err != nil {
		return err
	}
	s.emit(ctx, resourceEventBusiness.Event{Kind: resourceEventBusiness.CalendarSyncLinkBroken, ResourceKind: KindSyncLink, ResourceUUID: l.ID.String(),
		Actor: resourceEventBusiness.Actor{UserUUID: personOf(admin).String()}, At: s.now(), Subjects: []string{l.PersonID.String()},
		Data: map[string]string{"by": "admin", "provider": l.Provider, "person_uuid": l.PersonID.String()}})
	return nil
}
