package Calendar

// Moving from Google Calendar in one go (calendar B6): every calendar of the
// person's Google link, read through calendar sync's Google adapter
// (business/Integration googleSync.go) and written through the calendar's
// one import path (importICS.go), as a file is: their own calendar into
// their OneCamp calendar, each other one into a calendar of its name; series
// with their changes, guests with their answers (nobody invited or told),
// reminders, zones, call links and files. It runs in the background, its
// progress kept in sync's ledger (the calendar_sync migration), and asked
// for again it makes nothing twice. Where each event is at Google is kept,
// so two-way sync carries on from here.

import (
	"context"
	"encoding/json"
	"slices"
	"time"

	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	// Outlook's adapter registers itself (calendar B7), as icsurl's does.
	_ "github.com/akashc777/OneCamp/business/CalendarSync/microsoft"
	connectorBusiness "github.com/akashc777/OneCamp/business/Connector"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	integrationBusiness "github.com/akashc777/OneCamp/business/Integration"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// How long a move may take, and when one that stopped reporting is taken
// to have died (a restart) and may start again.
const (
	moveFromGoogleLimit = 2 * time.Hour
	moveStuckAfter      = 15 * time.Minute
)

// ErrGoogleNotLinked is a move asked of someone who hasn't connected Google;
// ErrOutlookNotLinked of someone who hasn't connected Outlook.
var (
	ErrGoogleNotLinked  = inputError("Connect Google Calendar first, then move your calendars.")
	ErrOutlookNotLinked = inputError("Connect Outlook first, then move your calendars.")
	// ErrNoSyncProvider is a move or a status asked of a provider calendars
	// don't sync with both ways.
	ErrNoSyncProvider = inputError("Calendars move from Google or Outlook.")
)

// syncWith is what differs between the providers calendars move from and
// sync with both ways: their name, how a person's link is read, what a person
// reads when it isn't there or is broken, and whether the admin has set the
// provider up.
type syncWith struct {
	name      string
	accountOf func(ctx context.Context, who *model.UserInfo) (string, bool, error)
	notLinked error
	brokenWhy string
	connector string
}

// syncWithOf is a provider's syncWith; one calendars don't move from has
// no accountOf.
func syncWithOf(provider string) syncWith {
	switch provider {
	case "google":
		return syncWith{name: "Google", accountOf: googleAccountOf, notLinked: ErrGoogleNotLinked, brokenWhy: syncBrokenWhy, connector: connectorBusiness.ProviderCalendar}
	case "microsoft":
		return syncWith{name: "Outlook", accountOf: microsoftAccountOf, notLinked: ErrOutlookNotLinked, brokenWhy: syncBrokenWhyMicrosoft, connector: connectorBusiness.ProviderOutlookCalendar}
	}
	return syncWith{name: provider, brokenWhy: "This account stopped answering. Connect it again."}
}

// twoWayProvider is whether calendars sync with provider both ways.
func twoWayProvider(provider string) bool {
	return syncWithOf(provider).accountOf != nil && slices.Contains(calendarSyncBusiness.Adapters.TwoWay(), provider)
}

// SyncStatus is where a person's move from a provider, and its sync, stand.
type SyncStatus struct {
	Provider     string                      `json:"provider"`
	Account      string                      `json:"account,omitempty"`
	State        string                      `json:"state"` // "none", "moving", "synced" or "failed"
	Progress     int                         `json:"progress"`
	Moved        int                         `json:"moved"`
	Unchanged    int                         `json:"unchanged"`
	Skipped      []contentIOBusiness.Skipped `json:"skipped,omitempty"`
	Error        string                      `json:"error,omitempty"`
	LastSyncedAt *time.Time                  `json:"last_synced_at,omitempty"`
	// Available is whether the admin has set the provider up, so people
	// can connect it.
	Available bool `json:"available"`
}

// syncLedger keeps calendar sync's ledger; the service's is Postgres.
type syncLedger interface {
	linkOf(ctx context.Context, person uuid.UUID, provider string) (*models.SyncLink, error)
	startMove(ctx context.Context, person uuid.UUID, provider, account string) (*models.SyncLink, bool, error)
	setProgress(ctx context.Context, id uuid.UUID, progress int) error
	finish(ctx context.Context, id uuid.UUID, state string, moved, unchanged int, skipped []contentIOBusiness.Skipped, why string) error
	keepCalendar(ctx context.Context, c models.SyncCalendar) error
	keepRefs(ctx context.Context, refs []models.SyncRef) error
	// What two-way sync reads and writes (syncGoogle.go).
	syncedLinks(ctx context.Context, provider string, limit int) ([]*models.SyncLink, error)
	markSynced(ctx context.Context, id uuid.UUID) error
	breakLink(ctx context.Context, id uuid.UUID, why string) error
	calendarsOf(ctx context.Context, link uuid.UUID) ([]models.SyncCalendar, error)
	// calendarsInto are the links' calendars that come into any of
	// calendars, with their links' providers (origin.go).
	calendarsInto(ctx context.Context, calendars []uuid.UUID) ([]models.SyncCalendar, error)
	markReadOnly(ctx context.Context, link uuid.UUID, remote string) error
	refsOf(ctx context.Context, link uuid.UUID) (map[uuid.UUID]models.SyncRef, error)
	dropRefs(ctx context.Context, link uuid.UUID, events []uuid.UUID) error
	changedSince(ctx context.Context, link, person uuid.UUID, calendars []uuid.UUID, now time.Time, limit int) ([]*models.Event, error)
	refusalsOf(ctx context.Context, link uuid.UUID, events []uuid.UUID) (map[uuid.UUID]models.SyncRefusal, error)
	keepRefusal(ctx context.Context, r models.SyncRefusal, now time.Time) error
	// Calendars read from an address (subscriptions.go).
	ensureLink(ctx context.Context, person uuid.UUID, provider string) (*models.SyncLink, error)
	addSubscription(ctx context.Context, s models.Subscription) (bool, error)
	subscriptionsOf(ctx context.Context, person uuid.UUID) ([]models.Subscription, error)
	dueSubscriptions(ctx context.Context, before time.Time, limit int) ([]models.Subscription, error)
	claimSubscription(ctx context.Context, link uuid.UUID, remote string, now, before time.Time) (bool, error)
	finishSubscriptionRead(ctx context.Context, link uuid.UUID, remote string, cursor *string, why string) error
	dropSubscription(ctx context.Context, link uuid.UUID, remote string) error
}

type postgresLedger struct{}

func (postgresLedger) linkOf(ctx context.Context, person uuid.UUID, provider string) (*models.SyncLink, error) {
	return models.SyncLinkOf(ctx, person, provider)
}

func (postgresLedger) startMove(ctx context.Context, person uuid.UUID, provider, account string) (*models.SyncLink, bool, error) {
	return models.StartMove(ctx, uuid.New(), person, provider, account, moveStuckAfter)
}

func (postgresLedger) setProgress(ctx context.Context, id uuid.UUID, progress int) error {
	return models.SetSyncProgress(ctx, id, progress)
}

func (postgresLedger) finish(ctx context.Context, id uuid.UUID, state string, moved, unchanged int, skipped []contentIOBusiness.Skipped, why string) error {
	raw, _ := json.Marshal(skipped)
	return models.FinishMove(ctx, id, state, moved, unchanged, raw, why)
}

func (postgresLedger) keepCalendar(ctx context.Context, c models.SyncCalendar) error {
	return models.KeepSyncCalendar(ctx, c)
}

func (postgresLedger) keepRefs(ctx context.Context, refs []models.SyncRef) error {
	return models.KeepSyncRefs(ctx, refs)
}

func (postgresLedger) syncedLinks(ctx context.Context, provider string, limit int) ([]*models.SyncLink, error) {
	return models.SyncedLinks(ctx, provider, limit)
}

func (postgresLedger) markSynced(ctx context.Context, id uuid.UUID) error {
	return models.MarkSynced(ctx, id)
}

func (postgresLedger) breakLink(ctx context.Context, id uuid.UUID, why string) error {
	return models.BreakSyncLink(ctx, id, why)
}

func (postgresLedger) markReadOnly(ctx context.Context, link uuid.UUID, remote string) error {
	return models.MarkSyncCalendarReadOnly(ctx, link, remote)
}

func (postgresLedger) calendarsOf(ctx context.Context, link uuid.UUID) ([]models.SyncCalendar, error) {
	return models.SyncCalendarsOf(ctx, link)
}

func (postgresLedger) calendarsInto(ctx context.Context, calendars []uuid.UUID) ([]models.SyncCalendar, error) {
	return models.SyncCalendarsInto(ctx, calendars)
}

func (postgresLedger) refsOf(ctx context.Context, link uuid.UUID) (map[uuid.UUID]models.SyncRef, error) {
	return models.SyncRefsOf(ctx, link)
}

func (postgresLedger) dropRefs(ctx context.Context, link uuid.UUID, events []uuid.UUID) error {
	return models.DropSyncRefs(ctx, link, events)
}

func (postgresLedger) changedSince(ctx context.Context, link, person uuid.UUID, calendars []uuid.UUID, now time.Time, limit int) ([]*models.Event, error) {
	return models.ChangedSince(ctx, models.Pool(), link, person, calendars, now, limit)
}

func (postgresLedger) refusalsOf(ctx context.Context, link uuid.UUID, events []uuid.UUID) (map[uuid.UUID]models.SyncRefusal, error) {
	return models.SyncRefusalsOf(ctx, link, events)
}

func (postgresLedger) keepRefusal(ctx context.Context, r models.SyncRefusal, now time.Time) error {
	return models.KeepSyncRefusal(ctx, r, now)
}

func (postgresLedger) ensureLink(ctx context.Context, person uuid.UUID, provider string) (*models.SyncLink, error) {
	return models.EnsureSyncLink(ctx, uuid.New(), person, provider)
}

func (postgresLedger) addSubscription(ctx context.Context, s models.Subscription) (bool, error) {
	return models.AddSubscription(ctx, s)
}

func (postgresLedger) subscriptionsOf(ctx context.Context, person uuid.UUID) ([]models.Subscription, error) {
	return models.SubscriptionsOf(ctx, person)
}

func (postgresLedger) dueSubscriptions(ctx context.Context, before time.Time, limit int) ([]models.Subscription, error) {
	return models.DueSubscriptions(ctx, before, limit)
}

func (postgresLedger) claimSubscription(ctx context.Context, link uuid.UUID, remote string, now, before time.Time) (bool, error) {
	return models.ClaimSubscription(ctx, link, remote, now, before)
}

func (postgresLedger) finishSubscriptionRead(ctx context.Context, link uuid.UUID, remote string, cursor *string, why string) error {
	return models.FinishSubscriptionRead(ctx, link, remote, cursor, why)
}

func (postgresLedger) dropSubscription(ctx context.Context, link uuid.UUID, remote string) error {
	return models.DropSubscription(ctx, link, remote)
}

// syncProvider is calendar sync's adapter for a provider; swapped in tests.
var syncProvider = calendarSyncBusiness.Adapters.For

// googleAccountOf is the address of who's Google link, and whether they have
// one; swapped in tests.
var googleAccountOf = func(ctx context.Context, who *model.UserInfo) (string, bool, error) {
	integration, err := integrationBusiness.GetGoogleCalendarIntegration(ctx, who)
	if err != nil {
		return "", false, err
	}
	linked, err := integrationBusiness.IsGoogleCalendarConnected(ctx, who)
	if err != nil || !linked {
		return "", false, err
	}
	return integrationBusiness.GoogleCalendarAccount(integration), true, nil
}

// microsoftAccountOf is the address of who's Outlook link, and whether they
// have one; swapped in tests.
var microsoftAccountOf = func(ctx context.Context, who *model.UserInfo) (string, bool, error) {
	return connectorBusiness.LinkedAccount(ctx, personOf(who), connectorBusiness.ProviderOutlookCalendar)
}

// statusOf is a ledger row as the person sees it. Pure.
func statusOf(provider string, l *models.SyncLink) *SyncStatus {
	if l == nil {
		return &SyncStatus{Provider: provider, State: "none"}
	}
	st := &SyncStatus{Provider: l.Provider, Account: l.Account, State: l.State, Progress: l.Progress, Moved: l.Moved,
		Unchanged: l.Unchanged, Error: l.Error, LastSyncedAt: l.LastSyncedAt}
	_ = json.Unmarshal(l.Skipped, &st.Skipped)
	return st
}

// SyncStatus is where who's own move from a provider stands, and whether the
// admin has set the provider up.
func (s *service) SyncStatus(ctx context.Context, who *model.UserInfo, provider string) (*SyncStatus, error) {
	if !twoWayProvider(provider) {
		return nil, ErrNoSyncProvider
	}
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	l, err := s.ledger.linkOf(ctx, personOf(who), provider)
	if err != nil {
		return nil, err
	}
	return s.statusFor(provider, l), nil
}

// statusFor is statusOf with whether the provider is set up.
func (s *service) statusFor(provider string, l *models.SyncLink) *SyncStatus {
	st := statusOf(provider, l)
	st.Available = connectorConfigured(syncWithOf(provider).connector)
	return st
}

// connectorConfigured is whether the admin has set a connector's provider
// up; swapped in tests.
var connectorConfigured = connectorBusiness.Configured

// MoveFrom starts moving every calendar of who's own link at a provider
// (Google, Outlook) into OneCamp, in the background, and says where it
// stands. Asked again while a move runs, it starts nothing.
func (s *service) MoveFrom(ctx context.Context, who *model.UserInfo, provider string) (*SyncStatus, error) {
	if !twoWayProvider(provider) {
		return nil, ErrNoSyncProvider
	}
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	me := personOf(who)
	if me == uuid.Nil {
		return nil, ErrEventNotFound
	}
	adapter, ok := syncProvider(provider)
	if !ok {
		return nil, ErrUnavailable
	}
	with := syncWithOf(provider)
	account, linked, err := with.accountOf(ctx, who)
	if err != nil {
		return nil, err
	}
	if !linked {
		return nil, with.notLinked
	}
	link, started, err := s.ledger.startMove(ctx, me, provider, account)
	if err != nil {
		return nil, err
	}
	if started {
		run := func() {
			ctx, cancel := context.WithTimeout(context.Background(), moveFromGoogleLimit)
			defer cancel()
			s.runMove(ctx, who, link, adapter)
		}
		if s.moveNow {
			run()
		} else {
			helpers.GoSafeNamed("business/Calendar/moveFromGoogle", run)
		}
		if l, err := s.ledger.linkOf(ctx, me, provider); err == nil && l != nil {
			link = l
		}
	}
	return s.statusFor(provider, link), nil
}

// runMove moves each of the link's calendars in turn, keeping where it has
// got, where each calendar went and where each event is at the provider.
func (s *service) runMove(ctx context.Context, who *model.UserInfo, link *models.SyncLink, provider calendarSyncBusiness.Adapter) {
	l := calendarSyncBusiness.Link{ID: link.ID, PersonUUID: link.PersonID.String(), Provider: link.Provider, Account: link.Account}
	name := syncWithOf(link.Provider).name
	remotes, err := provider.Calendars(ctx, l)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: a move from %s couldn't list its calendars: %v", name, err)
		_ = s.ledger.finish(ctx, link.ID, models.SyncFailed, 0, 0, nil, "Your "+name+" calendars couldn't be read. Connect "+name+" again, then try once more.")
		return
	}
	var moved, unchanged int
	var skipped []contentIOBusiness.Skipped
	for i, r := range remotes {
		changes, cursor, err := provider.Pull(ctx, r, "")
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: a move from %s couldn't read %q: %v", name, r.ID, err)
			skipped = append(skipped, contentIOBusiness.Skipped{What: r.Name, Why: "It couldn't be read from " + name + "."})
			continue
		}
		cal := ICS.Calendar{Name: r.Name, Zone: r.Zone}
		if r.Primary {
			cal.Name = "" // the person's own: into their own calendar
		}
		external := map[string]calendarSyncBusiness.Change{}
		for _, ch := range changes {
			if ch.Deleted {
				continue
			}
			cal.Events = append(cal.Events, ch.Event)
			if ch.Event.RecurrenceID == nil {
				external[ch.Event.UID] = ch
			}
		}
		share := func(done, total int) {
			if total == 0 {
				total = 1
			}
			_ = s.ledger.setProgress(ctx, link.ID, min(99, (i*100+done*100/total)/max(1, len(remotes))))
		}
		made, err := s.importCalendars(ctx, who, []ICS.Calendar{cal}, share)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: a move from %s couldn't write %q: %v", name, r.ID, err)
			skipped = append(skipped, contentIOBusiness.Skipped{What: r.Name, Why: "It couldn't be saved."})
			continue
		}
		moved, unchanged = moved+len(made.Created), unchanged+made.Unchanged
		for _, sk := range made.Skipped {
			sk.What = r.Name + ": " + sk.What
			skipped = append(skipped, sk)
		}
		local := s.keepRefsOf(ctx, link.ID, r.ID, made.Created, external)
		_ = s.ledger.keepCalendar(ctx, models.SyncCalendar{LinkID: link.ID, RemoteID: r.ID, Name: r.Name, Primary: r.Primary, CalendarID: local, Cursor: cursor, ReadOnly: r.ReadOnly})
	}
	_ = s.ledger.finish(ctx, link.ID, models.SyncSynced, moved, unchanged, skipped, "")
}

// keepRefsOf records where each event a move made is at the provider,
// found by its UID (a copy's is the event it copies), and answers the
// OneCamp calendar they came into.
func (s *service) keepRefsOf(ctx context.Context, linkID uuid.UUID, remote string, created []contentIOBusiness.Created, external map[string]calendarSyncBusiness.Change) *uuid.UUID {
	ids := make([]uuid.UUID, 0, len(created))
	for _, c := range created {
		if id, err := uuid.Parse(c.UUID); err == nil {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := s.store.getMany(ctx, ids)
	if err != nil {
		return nil
	}
	var calendar *uuid.UUID
	var refs []models.SyncRef
	for _, e := range rows {
		ch, ok := external[exportedUID(e.ICalUID)]
		if !ok {
			continue
		}
		refs = append(refs, models.SyncRef{LinkID: linkID, RemoteID: remote, ExternalID: ch.ExternalID, EventID: e.ID, ETag: ch.ETag})
		if calendar == nil {
			c := e.CalendarID
			calendar = &c
		}
	}
	if err := s.ledger.keepRefs(ctx, refs); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: where moved events are at their provider couldn't be kept: %v", err)
	}
	return calendar
}
