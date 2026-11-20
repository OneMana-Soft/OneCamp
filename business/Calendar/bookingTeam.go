package Calendar

// gcal-7 H4a: team booking pages. A collective page is booked with its owner
// and every member who accepted to host it: its free time is when all of them
// are free, its meetings land on the owner's calendar with the hosts on them,
// and each host is told as the owner is.
//
// Permissions: the owner alone edits the page and its host list (SavePage's
// id and user_id). A member sees a page among the ones they host only through
// their own host row, and accepts or leaves only that row; they never edit
// the page. A host's busy time counts from accepting until leaving. The public
// page names accepted hosts, nothing more.

import (
	"context"
	"fmt"
	"strings"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	bookingModel "github.com/akashc777/OneCamp/models/postgres/Booking"
	"github.com/google/uuid"
)

const (
	teamSolo       = "solo"
	teamCollective = "collective"
	teamRoundRobin = "round_robin"
	maxHosts       = 9 // besides the owner
	// teamBusyReads is how many of a team page's people have their calendars
	// read at once: each is their own provider call (their own Google token).
	teamBusyReads = 4
)

// teamBusyTimeout bounds a team page's whole busy read, every person's
// together (swapped in tests).
var teamBusyTimeout = busyReadTimeout

// checkTeam is a page's team mode and its hosts, each once. Pure.
func checkTeam(in PageInput) (string, []uuid.UUID, error) {
	mode := strings.TrimSpace(in.TeamMode)
	switch mode {
	case "", teamSolo:
		if len(in.Hosts) > 0 {
			return "", nil, &AvailabilityError{Msg: "Only a team page has hosts: make it a team page, or take its hosts off."}
		}
		return teamSolo, nil, nil
	case teamCollective:
	case teamRoundRobin:
		return "", nil, &AvailabilityError{Msg: "Round robin pages aren't open yet. Make it a team page where every host meets the guest."}
	default:
		return "", nil, &AvailabilityError{Msg: "A page is yours alone or a team's."}
	}
	seen := map[uuid.UUID]bool{}
	hosts := []uuid.UUID{}
	for _, h := range in.Hosts {
		id, err := uuid.Parse(strings.TrimSpace(h))
		if err != nil {
			return "", nil, &AvailabilityError{Msg: "A host must be a member of this workspace."}
		}
		if !seen[id] {
			seen[id] = true
			hosts = append(hosts, id)
		}
	}
	if len(hosts) > maxHosts {
		return "", nil, &AvailabilityError{Msg: fmt.Sprintf("A team page has up to %d hosts besides you.", maxHosts)}
	}
	return teamCollective, hosts, nil
}

// pageKeptFrom is the owner's stored page when an update leaves out its team
// mode or its hosts, to keep them from; nil on a create, when both are sent,
// or when the id is no uuid (CheckPage refuses it).
func pageKeptFrom(owner uuid.UUID, in PageInput) (*bookingModel.Page, error) {
	if in.Id == "" || (strings.TrimSpace(in.TeamMode) != "" && in.Hosts != nil) {
		return nil, nil
	}
	id, err := uuid.Parse(in.Id)
	if err != nil {
		return nil, nil
	}
	p, err := bookingModel.PageById(id)
	if err != nil {
		return nil, err
	}
	if p == nil || p.UserId != owner || p.DeletedAt != nil {
		return nil, &AvailabilityError{Msg: "That isn't one of your pages."}
	}
	return p, nil
}

// keepsHosts is whether an update leaves stored's host rows as they are: its
// host list was left out (nil, not []), and the save doesn't make a team
// page solo, which has nobody. Pure.
func keepsHosts(stored *bookingModel.Page, hosts []string, mode string) bool {
	return stored != nil && hosts == nil && !(mode == teamSolo && stored.TeamMode != teamSolo)
}

// checkHosts refuses the owner as a host of their own page, then anyone who
// isn't an active human member, in one read.
func checkHosts(ctx context.Context, owner uuid.UUID, hosts []uuid.UUID) error {
	for _, h := range hosts {
		if h == owner {
			return &AvailabilityError{Msg: "You own this page: add other members as its hosts."}
		}
	}
	if len(hosts) == 0 {
		return nil
	}
	ok, err := bookingModel.EligibleHosts(ctx, hosts)
	if err != nil {
		return err
	}
	for _, h := range hosts {
		if !ok[h] {
			return &AvailabilityError{Msg: "Only active members of this workspace can host a page: not bots, guests or people who have left."}
		}
	}
	return nil
}

// hostsOf is pages' host rows with their names: one read of the rows, one of
// the names.
func hostsOf(ctx context.Context, pages []uuid.UUID) ([]bookingModel.Host, error) {
	hosts, err := bookingModel.HostsOfPages(ctx, pages)
	if err != nil || len(hosts) == 0 {
		return hosts, err
	}
	ids := make([]string, 0, len(hosts))
	for _, h := range hosts {
		ids = append(ids, h.UserId.String())
	}
	names, err := namesOf(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range hosts {
		hosts[i].Name = names[hosts[i].UserId.String()]
	}
	return hosts, nil
}

// namesOf is each person's display name by uuid, in one read.
func namesOf(ctx context.Context, ids []string) (map[string]string, error) {
	people, err := userDomain.GetPeopleByUUIDs(ctx, ids)
	out := make(map[string]string, len(people))
	for _, p := range people {
		if p != nil {
			out[p.Uuid] = bookingOwnerName(p)
		}
	}
	return out, err
}

// accepted is the hosts whose time counts: they accepted and haven't left.
func accepted(hosts []bookingModel.Host) []bookingModel.Host {
	out := []bookingModel.Host{}
	for _, h := range hosts {
		if h.State == bookingModel.HostAccepted {
			out = append(out, h)
		}
	}
	return out
}

// people is whose calendars the page's free time is cut from: its owner,
// then every host who accepted.
func (l *livePage) people() []string {
	out := []string{l.page.UserId.String()}
	for _, h := range accepted(l.hosts) {
		out = append(out, h.UserId.String())
	}
	return out
}

// hostIDs is the page's accepted hosts, the people its meetings carry.
func (l *livePage) hostIDs() []string { return l.people()[1:] }

// hostNames is what the public page shows of the hosts: accepted ones' names.
func (l *livePage) hostNames() []string {
	var out []string
	for _, h := range accepted(l.hosts) {
		out = append(out, h.Name)
	}
	return out
}

// teamBusy is when any of people is busy, read is each one's. Each person is
// one provider call (their own calendars, their own Google token: no read
// spans two people's), so the calls run side by side, at most teamBusyReads
// at once, under one timeout from ctx (CR-452 F4). A calendar that can't be
// read makes the whole answer read in part; a read that fails, or never
// answers, fails it.
func teamBusy(ctx context.Context, people []string, read func(ctx context.Context, who string) (Busy, error)) (Busy, error) {
	ctx, cancel := context.WithTimeout(ctx, teamBusyTimeout)
	defer cancel()
	each := make([]Busy, len(people))
	errs := make([]error, len(people))
	at := make([]int, len(people))
	for i := range people {
		at[i], errs[i] = i, ErrCalendarUnread // until its read answers
	}
	helpers.EachBounded("calendar.team-busy", at, teamBusyReads, func(i int) {
		each[i], errs[i] = read(ctx, people[i])
	})
	var all Busy
	for i, b := range each {
		if errs[i] != nil {
			return Busy{}, errs[i]
		}
		all.Intervals = append(all.Intervals, b.Intervals...)
		all.Spans = append(all.Spans, b.Spans...)
		all.GoogleUnread = all.GoogleUnread || b.GoogleUnread
	}
	// Each person's are sorted, the whole isn't: FreeSlots walks them in
	// order, so a host's 11:00 after the owner's 13:00 went unseen.
	all.Intervals = MergeBusy(all.Intervals)
	return all, nil
}

// MyBookingPages is the owner's pages, each team page with its hosts by
// name and state.
func MyBookingPages(ctx context.Context, owner uuid.UUID) ([]bookingModel.Page, error) {
	pages, err := bookingModel.ListPages(owner)
	if err != nil {
		return nil, err
	}
	ids := []uuid.UUID{}
	for _, p := range pages {
		if p.TeamMode != teamSolo {
			ids = append(ids, p.Id)
		}
	}
	if len(ids) == 0 {
		return pages, nil
	}
	hosts, err := hostsOf(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range pages {
		for _, h := range hosts {
			if h.PageId == pages[i].Id {
				pages[i].Hosts = append(pages[i].Hosts, h)
			}
		}
	}
	return pages, nil
}

// HostingBookingPages is the pages member hosts or is invited to, with their
// owners' names.
func HostingBookingPages(ctx context.Context, member uuid.UUID) ([]bookingModel.Hosted, error) {
	list, err := bookingModel.Hosting(ctx, member)
	if err != nil || len(list) == 0 {
		return list, err
	}
	ids := make([]string, 0, len(list))
	for _, h := range list {
		ids = append(ids, h.OwnerId.String())
	}
	names, err := namesOf(ctx, ids)
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].OwnerName = names[list[i].OwnerId.String()]
	}
	return list, nil
}

// AnswerBookingHost is member accepting (or leaving) page through their own
// host row; ErrNoPage when they have none on a live page. The page's answers
// are cut afresh: the member's time counts from now, or stops.
func AnswerBookingHost(ctx context.Context, page, member uuid.UUID, accept bool) error {
	owner, ok, err := bookingModel.AnswerHost(ctx, page, member, accept)
	if err != nil {
		return err
	}
	if !ok {
		return ErrNoPage
	}
	pageCache.forgetOwner(owner.String())
	return nil
}

// tellInvited tells each member just invited to host page p, owner's. The
// owner's name is the session's (owner); only a caller without one, or a
// session with no name, reads it.
func tellInvited(ctx context.Context, p *bookingModel.Page, owner *dgraphStruct.DgraphUser, invited []uuid.UUID) {
	if len(invited) == 0 {
		return
	}
	name := ""
	if owner != nil {
		name = bookingOwnerName(owner)
	}
	if name == "" {
		names, err := namesOf(ctx, []string{p.UserId.String()})
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar/tellInvited owner name err: %+v", err)
		}
		name = names[p.UserId.String()]
	}
	hosts := make([]string, 0, len(invited))
	for _, h := range invited {
		hosts = append(hosts, h.String())
	}
	notificationBusiness.TellBookingHostInvited(ctx, notificationBusiness.HostInvite{
		HostUUIDs: hosts, OwnerName: name, PageID: p.Id.String(),
		PageTitle: p.Title, InvitedAt: p.UpdatedAt,
	})
}
