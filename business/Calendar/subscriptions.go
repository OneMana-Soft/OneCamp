package Calendar

// Calendars read from an address (calendar B8, SY-14): public holidays, a
// team's calendar in another tool, a secret link someone was given. Each is
// one of the person's calendars (kind 'subscribed') and one row of calendar
// sync's ledger under their "ics" link (business/CalendarSync/icsurl reads
// the file), so a subscription is kept in step as Google's calendars are.
//
//   - It is added only once its address has been read: an address that
//     isn't a calendar says why, and nothing is made.
//   - Its events are the address's: the person sees them whole, changes
//     none (roleReader), and is never busy because of them; nobody else
//     sees them at all, not even as busy. They come in through the one
//     import path, as copies under a UID of the calendar's own, with no
//     guests and no alarms.
//   - It is read again every 15 minutes (Google takes hours), or at once
//     with Refresh now; a file unchanged since the last read changes
//     nothing. An event that changed is changed in place (a reminder the
//     person set on it stays); one that left the file is deleted.
//   - The address is kept sealed, never written to a log, and never given
//     back: a private calendar's link is a secret. The person sees its host.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	calendarSyncBusiness "github.com/akashc777/OneCamp/business/CalendarSync"
	"github.com/akashc777/OneCamp/business/CalendarSync/icsurl"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	sweepBusiness "github.com/akashc777/OneCamp/business/Sweep"
	"github.com/akashc777/OneCamp/helpers"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

const (
	kindSubscribed = "subscribed"
	providerICS    = "ics"
	// subscriptionEvery is how long a subscription goes between reads;
	// Refresh now reads one at most once a minute.
	subscriptionEvery   = 15 * time.Minute
	subscriptionRefresh = time.Minute
	// A sweep reads at most this many subscriptions, for at most this long;
	// the rest wait for the next minute's.
	subscriptionsPerSweep = 50
	subscriptionSweepFor  = 50 * time.Second
	// subscriptionLimit bounds bringing one file's events in.
	subscriptionLimit = 10 * time.Minute
	// maxSubscriptions is the most calendars one person reads from
	// addresses, and maxSubscribedEvents the most events (a series with its
	// changed instances is one) one of them keeps.
	maxSubscriptions    = 50
	maxSubscribedEvents = 5000
)

// ErrSubscribedEvent is a change asked of an event on a subscribed calendar.
var ErrSubscribedEvent = inputError("This event is on a calendar you subscribe to: it changes only where that calendar is published.")

// errFeedOfSubscribed is a secret link asked of a subscribed calendar.
var errFeedOfSubscribed = inputError("A calendar you subscribe to is shown in another app by its own address.")

// subscribedOnly is a decision that a calendar, or an event, is its
// person's to see because they subscribe to it. Pure.
func subscribedOnly(d accessBusiness.Decision) bool {
	return len(d.Reasons) == 1 && d.Reasons[0] == reasonSubscribed
}

// SubscriptionView is a calendar read from an address, as its person sees
// it: never the address, only its host.
type SubscriptionView struct {
	CalendarID string `json:"calendar_id"`
	Name       string `json:"name"`
	Host       string `json:"host"`
	// ReadAt is when it was last read, or tried; Error why it couldn't be.
	ReadAt *time.Time `json:"read_at,omitempty"`
	Error  string     `json:"error,omitempty"`
	// Reading is its first read still bringing events in.
	Reading bool `json:"reading,omitempty"`
	// Events is how many events its file holds, when it has just been added.
	Events int `json:"events,omitempty"`
}

// readAddress reads an address's file whole; tests swap it.
var readAddress = icsurl.Read

func init() {
	sweepBusiness.Jobs.Register(sweepBusiness.Job{Name: "calendar.sync.ics", Every: time.Minute, Run: func(ctx context.Context) error {
		if !helpers.CalendarInPostgres() {
			return nil
		}
		return stored.readSubscriptions(ctx)
	}})
}

// addressDigest is how the ledger knows an address without keeping it in
// the clear: its SHA-256. Pure.
func addressDigest(address string) string {
	sum := sha256.Sum256([]byte(address))
	return hex.EncodeToString(sum[:])
}

// hostOf is an address's host, which is all of it the person is shown. Pure.
func hostOf(address string) string {
	u, err := url.Parse(address)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

// Subscribe adds a calendar read from an address to who's calendars. The
// address is read once now, so one that isn't a calendar says why and
// nothing is made; then its events come in, in the background, and it is
// read again every 15 minutes. The calendar is named name, else what the
// file calls itself, else the address's host, and coloured hue (a camp hue,
// or none).
func (s *service) Subscribe(ctx context.Context, who *model.UserInfo, address, name, hue string) (*SubscriptionView, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	me := personOf(who)
	if me == uuid.Nil {
		return nil, ErrEventNotFound
	}
	addr, err := icsurl.Address(address)
	if err != nil {
		return nil, inputError(err.Error())
	}
	if hue != "" && !helpers.IsCampHue(hue) {
		return nil, inputError("Pick one of the calendar's colours.")
	}
	if _, ok := syncProvider(providerICS); !ok {
		return nil, ErrUnavailable
	}
	have, err := s.ledger.subscriptionsOf(ctx, me)
	if err != nil {
		return nil, err
	}
	remote := addressDigest(addr)
	if slices.ContainsFunc(have, func(h models.Subscription) bool { return h.RemoteID == remote }) {
		return nil, inputError("You subscribe to that calendar already.")
	}
	if len(have) >= maxSubscriptions {
		return nil, inputError(fmt.Sprintf("You can subscribe to up to %d calendars.", maxSubscriptions))
	}
	file, cursor, err := readAddress(ctx, addr)
	if err != nil {
		return nil, inputError(err.Error())
	}
	sealed, err := helpers.EncryptSecret(addr)
	if err != nil {
		return nil, err
	}
	link, err := s.ledger.ensureLink(ctx, me, providerICS)
	if err != nil {
		return nil, err
	}
	calName := clip(strings.TrimSpace(name), maxCalendarNameRunes-1)
	if calName == "" {
		calName = clip(strings.TrimSpace(file.Name), maxCalendarNameRunes-1)
	}
	if calName == "" {
		calName = hostOf(addr)
	}
	cal := s.newID()
	if err := s.store.subscribedCalendar(ctx, cal, me, calName, hue, s.zoneOf(ctx, me)); err != nil {
		return nil, err
	}
	now := s.now()
	sub := models.Subscription{SyncCalendar: models.SyncCalendar{LinkID: link.ID, RemoteID: remote, Name: calName, CalendarID: &cal},
		PersonID: me, SourceSealed: sealed, ReadAt: &now}
	added, err := s.ledger.addSubscription(ctx, sub)
	if err != nil || !added {
		if _, derr := s.store.deleteSubscribedCalendar(ctx, cal, me); derr != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: a calendar made for a subscription that wasn't added couldn't be deleted: %v", derr)
		}
		if err != nil {
			return nil, err
		}
		return nil, inputError("You subscribe to that calendar already.")
	}
	changes := icsurl.ChangesOf(file)
	events, _ := groupTags(changes)
	bring := func() {
		ctx, cancel := context.WithTimeout(context.Background(), subscriptionLimit)
		defer cancel()
		s.takeSubscribed(ctx, sub, changes, cursor)
	}
	if s.moveNow {
		bring()
	} else {
		helpers.GoSafeNamed("business/Calendar/subscribe", bring)
	}
	return &SubscriptionView{CalendarID: cal.String(), Name: calName, Host: hostOf(addr), ReadAt: &now, Reading: !s.moveNow, Events: len(events)}, nil
}

// Subscriptions are who's calendars read from addresses, and how each was
// last read.
func (s *service) Subscriptions(ctx context.Context, who *model.UserInfo) ([]SubscriptionView, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	me := personOf(who)
	if me == uuid.Nil {
		return nil, ErrEventNotFound
	}
	subs, err := s.ledger.subscriptionsOf(ctx, me)
	if err != nil {
		return nil, err
	}
	out := make([]SubscriptionView, 0, len(subs))
	for _, sub := range subs {
		out = append(out, subscriptionView(sub))
	}
	return out, nil
}

// subscriptionView is a subscription as its person sees it. Pure but for
// opening the sealed address, for its host.
func subscriptionView(sub models.Subscription) SubscriptionView {
	v := SubscriptionView{Name: sub.Name, ReadAt: sub.ReadAt, Error: sub.Error, Reading: sub.Cursor == "" && sub.Error == ""}
	if sub.CalendarID != nil {
		v.CalendarID = sub.CalendarID.String()
	}
	if addr, err := helpers.DecryptSecret(sub.SourceSealed); err == nil {
		v.Host = hostOf(addr)
	}
	return v
}

// subscriptionOf is who's subscription that reads into a calendar.
func (s *service) subscriptionOf(ctx context.Context, who *model.UserInfo, calendarID string) (*models.Subscription, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	cal, err := uuid.Parse(strings.TrimSpace(calendarID))
	if err != nil {
		return nil, ErrEventNotFound
	}
	subs, err := s.ledger.subscriptionsOf(ctx, personOf(who))
	if err != nil {
		return nil, err
	}
	for _, sub := range subs {
		if sub.CalendarID != nil && *sub.CalendarID == cal {
			return &sub, nil
		}
	}
	return nil, ErrEventNotFound
}

// ownSubscribed is a subscribed calendar of who's, which they may rename
// and recolour though its events are the address's.
func (s *service) ownSubscribed(ctx context.Context, who *model.UserInfo, id string) (uuid.UUID, error) {
	cal, err := uuid.Parse(strings.TrimSpace(id))
	if err != nil {
		return uuid.Nil, ErrEventNotFound
	}
	decided, err := s.decideCalendars(ctx, subjectOf(who), []string{cal.String()})
	if err != nil {
		return uuid.Nil, err
	}
	if d := decided[cal.String()]; d.Owner && subscribedOnly(d) {
		return cal, nil
	}
	return uuid.Nil, ErrEventNotFound
}

// RefreshSubscription reads one of who's subscribed calendars now (at most
// once a minute) and says how it stands.
func (s *service) RefreshSubscription(ctx context.Context, who *model.UserInfo, calendarID string) (*SubscriptionView, error) {
	sub, err := s.subscriptionOf(ctx, who, calendarID)
	if err != nil {
		return nil, err
	}
	if _, err := s.readSubscription(ctx, *sub, s.now().Add(-subscriptionRefresh)); err != nil {
		return nil, err
	}
	if sub, err = s.subscriptionOf(ctx, who, calendarID); err != nil {
		return nil, err
	}
	v := subscriptionView(*sub)
	return &v, nil
}

// Unsubscribe removes one of who's subscribed calendars and everything on
// it. The calendar goes first, so nothing is left that its subscription
// isn't reading; a ledger row left behind is never read again (its
// calendar is gone) and is taken again by the same address.
func (s *service) Unsubscribe(ctx context.Context, who *model.UserInfo, calendarID string) error {
	sub, err := s.subscriptionOf(ctx, who, calendarID)
	if err != nil {
		return err
	}
	if _, err := s.store.deleteSubscribedCalendar(ctx, *sub.CalendarID, sub.PersonID); err != nil {
		return err
	}
	return s.ledger.dropSubscription(ctx, sub.LinkID, sub.RemoteID)
}

// readSubscriptions reads each subscription due (unread for 15 minutes),
// the longest unread first, at most subscriptionsPerSweep a minute.
func (s *service) readSubscriptions(ctx context.Context) error {
	if err := s.ready(ctx); err != nil {
		return nil // the store isn't in use yet
	}
	start := s.now()
	due, err := s.ledger.dueSubscriptions(ctx, start.Add(-subscriptionEvery), subscriptionsPerSweep)
	if err != nil {
		return err
	}
	for _, sub := range due {
		if s.now().Sub(start) > subscriptionSweepFor || ctx.Err() != nil {
			break
		}
		if _, err := s.readSubscription(ctx, sub, start.Add(-subscriptionEvery)); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: a subscription of calendar %v couldn't be read: %v", sub.CalendarID, err)
		}
	}
	return nil
}

// readSubscription reads one subscription now, when nobody has read it
// since before (two instances, or Refresh now and the sweep, never read one
// at once), and brings in what changed. Why its address couldn't be read is
// kept, in words, for its person; it says whether it read.
func (s *service) readSubscription(ctx context.Context, sub models.Subscription, before time.Time) (bool, error) {
	claimed, err := s.ledger.claimSubscription(ctx, sub.LinkID, sub.RemoteID, s.now(), before)
	if err != nil || !claimed {
		return false, err
	}
	provider, ok := syncProvider(providerICS)
	if !ok {
		return false, ErrUnavailable
	}
	addr, err := helpers.DecryptSecret(sub.SourceSealed)
	if err != nil || addr == "" {
		return true, s.ledger.finishSubscriptionRead(ctx, sub.LinkID, sub.RemoteID, nil, "Its address couldn't be opened here: remove the calendar and add it again.")
	}
	remote := calendarSyncBusiness.Remote{Link: calendarSyncBusiness.Link{ID: sub.LinkID, PersonUUID: sub.PersonID.String(), Provider: providerICS},
		ID: sub.RemoteID, Name: sub.Name, Source: addr}
	changes, cursor, err := provider.Pull(ctx, remote, sub.Cursor)
	if err != nil {
		return true, s.ledger.finishSubscriptionRead(ctx, sub.LinkID, sub.RemoteID, nil, err.Error())
	}
	if cursor == sub.Cursor {
		return true, s.ledger.finishSubscriptionRead(ctx, sub.LinkID, sub.RemoteID, nil, "")
	}
	s.takeSubscribed(ctx, sub, changes, cursor)
	return true, nil
}

// subscribedGroup is one event of an address's file: a series or a single
// event, and the instances changed on their own, which share its UID.
type subscribedGroup struct {
	main      *ICS.Event
	instances []ICS.Event
	tag       string
}

// groupTags gathers a file's changes by UID, in the order they first
// appear, each with one version for the whole (its series' and its
// instances'), so a change to any of them is a change to it. A cancelled
// series is as good as gone. Pure.
func groupTags(changes []calendarSyncBusiness.Change) ([]string, map[string]*subscribedGroup) {
	var order []string
	groups := map[string]*subscribedGroup{}
	versions := map[string][]string{}
	for i := range changes {
		ch := changes[i]
		uid := ch.Event.UID
		g := groups[uid]
		if g == nil {
			g = &subscribedGroup{}
			groups[uid] = g
			order = append(order, uid)
		}
		if ch.SeriesID == "" {
			if g.main == nil {
				ev := ch.Event
				g.main = &ev
				versions[uid] = append([]string{"=" + ch.ETag}, versions[uid]...)
			}
			continue
		}
		g.instances = append(g.instances, ch.Event)
		versions[uid] = append(versions[uid], ch.ExternalID+"="+ch.ETag)
	}
	kept := order[:0]
	for _, uid := range order {
		g := groups[uid]
		if g.main == nil || cancelled(*g.main) {
			delete(groups, uid)
			continue
		}
		v := versions[uid]
		slices.Sort(v[1:])
		sum := sha256.Sum256([]byte(strings.Join(v, "\n")))
		g.tag = hex.EncodeToString(sum[:12])
		kept = append(kept, uid)
	}
	return kept, groups
}

// takeSubscribed brings one read of an address into its calendar: a new
// event through the import path, a changed one onto its row, and one that
// left the file deleted. The cursor is kept only once all of it is in, so a
// read cut short is read whole again.
func (s *service) takeSubscribed(ctx context.Context, sub models.Subscription, changes []calendarSyncBusiness.Change, cursor string) {
	fail := func(what string, err error) {
		helpers.LogErrorWithContext(ctx, "business/Calendar: a subscription of calendar %v couldn't %s: %v", sub.CalendarID, what, err)
		_ = s.ledger.finishSubscriptionRead(ctx, sub.LinkID, sub.RemoteID, nil, "Its events couldn't all be brought in: it will be read again in 15 minutes.")
	}
	if sub.CalendarID == nil {
		return
	}
	calendar := *sub.CalendarID
	who := &model.UserInfo{UserPostgresInfo: model.User{Id: sub.PersonID}}
	who.UserDgraphInfo.Uuid = sub.PersonID.String()
	order, groups := groupTags(changes)
	if len(order) > maxSubscribedEvents {
		for _, uid := range order[maxSubscribedEvents:] {
			delete(groups, uid)
		}
		order = order[:maxSubscribedEvents]
	}
	refs, err := s.ledger.refsOf(ctx, sub.LinkID)
	if err != nil {
		fail("read where its events are", err)
		return
	}
	byUID := map[string]models.SyncRef{}
	var gone []uuid.UUID
	for _, r := range refs {
		if r.RemoteID != sub.RemoteID {
			continue
		}
		if _, ok := groups[r.ExternalID]; ok {
			byUID[r.ExternalID] = r
		} else {
			gone = append(gone, r.EventID)
		}
	}
	// What left the file goes (its refs with it).
	if err := s.store.deleteSubscribedEvents(ctx, calendar, gone); err != nil {
		fail("delete what left its file", err)
		return
	}
	// What changed, in place; what's new, through the import path. Each
	// group is its own event, so the changed ones are read in one go, and
	// where they are is kept once, after their writes.
	var ids []uuid.UUID
	for _, uid := range order {
		if ref, has := byUID[uid]; has && ref.ETag != groups[uid].tag {
			ids = append(ids, ref.EventID)
		}
	}
	here := map[uuid.UUID]*models.Event{}
	if len(ids) > 0 {
		rows, err := s.store.getMany(ctx, ids)
		if err != nil {
			fail("read its events", err)
			return
		}
		for _, e := range rows {
			here[e.ID] = e
		}
	}
	var fresh []ICS.Event
	var kept []models.SyncRef
	external := map[string]calendarSyncBusiness.Change{}
	for _, uid := range order {
		g := groups[uid]
		ref, has := byUID[uid]
		if has && ref.ETag == g.tag {
			continue
		}
		if e := here[ref.EventID]; has && e != nil && e.DeletedAt == nil {
			s.retakeSubscribed(ctx, e, *g.main, g.instances)
			ref.ETag = g.tag
			kept = append(kept, ref)
			continue
		}
		fresh = append(fresh, *g.main)
		fresh = append(fresh, g.instances...)
		external[uid] = calendarSyncBusiness.Change{ExternalID: uid, ETag: g.tag}
	}
	if len(kept) > 0 {
		if err := s.ledger.keepRefs(ctx, kept); err != nil {
			fail("keep where its events are", err)
			return
		}
	}
	if len(fresh) > 0 {
		made, err := s.importFiles(ctx, who, []ICS.Calendar{{Name: sub.Name, Events: fresh}}, func(int, int) {}, calendar)
		if err != nil {
			fail("bring new events in", err)
			return
		}
		s.keepRefsOf(ctx, sub.LinkID, sub.RemoteID, made.Created, external)
		// An event brought in by a read cut short before its ref was kept is
		// the address's still: brought up to date, its ref kept now.
		if made.Unchanged > 0 {
			s.adoptSubscribed(ctx, sub, calendar, groups, external)
		}
	}
	// What changed in place, or left the file, is announced nowhere: search
	// is told here (searchEntries.go). What's new came through the import.
	changed := make([]string, 0, len(gone)+len(ids))
	for _, id := range append(slices.Clip(gone), ids...) {
		changed = append(changed, id.String())
	}
	SearchEntriesChanged(ctx, changed)
	_ = s.ledger.finishSubscriptionRead(ctx, sub.LinkID, sub.RemoteID, &cursor, "")
}

// adoptSubscribed brings up to date events already on a subscribed calendar
// whose refs its ledger lost (a read cut short), found by their copied
// UIDs, and keeps their refs.
func (s *service) adoptSubscribed(ctx context.Context, sub models.Subscription, calendar uuid.UUID, groups map[string]*subscribedGroup, external map[string]calendarSyncBusiness.Change) {
	uids := make([]string, 0, len(external))
	for uid := range external {
		uids = append(uids, importedUID(uid, calendar))
	}
	have, err := s.store.byICalUIDs(ctx, uids)
	if err != nil {
		return
	}
	var refs []models.SyncRef
	for _, e := range have {
		if e.CalendarID != calendar || e.Source != sourceSubscribed {
			continue
		}
		uid := exportedUID(e.ICalUID)
		ch, ok := external[uid]
		if !ok || groups[uid] == nil {
			continue
		}
		s.retakeSubscribed(ctx, e, *groups[uid].main, groups[uid].instances)
		refs = append(refs, models.SyncRef{LinkID: sub.LinkID, RemoteID: sub.RemoteID, ExternalID: ch.ExternalID, EventID: e.ID, ETag: ch.ETag})
	}
	if err := s.ledger.keepRefs(ctx, refs); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: a subscription's events couldn't be found again: %v", err)
	}
}

// retakeSubscribed writes an address's changed event onto its row (its id,
// and so a reminder the person set on it, stays) and its changed instances
// as the file has them now: the ones it had go, and the file's come.
func (s *service) retakeSubscribed(ctx context.Context, e *models.Event, main ICS.Event, instances []ICS.Event) {
	s.takeRemote(ctx, e, main)
	now := s.now()
	if had, err := s.store.overridesOfSeries(ctx, e.ID); err == nil && len(had) > 0 {
		if err := s.store.apply(ctx, batch{folded: idsOf(had), now: now, by: &e.CreatedBy}); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: a subscribed event's instances couldn't be cleared: %v", err)
			return
		}
	}
	for _, inst := range instances {
		if inst.RecurrenceID == nil {
			continue
		}
		// Read again each time: skipping an instance changes the series.
		series, err := s.store.get(ctx, e.ID)
		if err != nil || series == nil || series.RRule == "" {
			return
		}
		s.takeRemoteInstance(ctx, series, inst, now)
	}
}
