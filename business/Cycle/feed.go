package business

// A project's cycles as a calendar (linear-12, part 4): a secret link of a
// person's own that Google Calendar, Apple Calendar or Outlook subscribes to,
// with each of the project's cycles as an all-day event from its first day to
// its last, named with the project, and how much of it is done.
//
// The link is a bearer secret, kept as a calendar's feed keeps its own
// (business/Calendar/feeds.go): only its hash is stored, it is shown once
// when made, making another stops the old one at once, and nothing here
// writes it or its hash to a log. Anyone who may view the project may make
// one; each read is decided again for its person, so a person who left the
// workspace or the project, an archived project, or a workspace that turned
// calendar links off (calendar.feeds.allowed) answers nothing, the same
// ErrFeedNotFound for each. The feed is written through the one ICS codec.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	ICS "github.com/akashc777/OneCamp/business/ICS"
	policyBusiness "github.com/akashc777/OneCamp/business/Policy"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	projectDomain "github.com/akashc777/OneCamp/domain/Project"
	"github.com/akashc777/OneCamp/helpers"
	cycleModel "github.com/akashc777/OneCamp/models/postgres/Cycle"
	authService "github.com/akashc777/OneCamp/services/Auth"
	"github.com/google/uuid"
)

// ErrFeedNotFound is every read of a link that isn't a project's cycles now.
var ErrFeedNotFound = errors.New("cycle feed not found")

// FeedView is a person's link to a project's cycles: the address only when
// it has just been made.
type FeedView struct {
	Live      bool       `json:"live"`
	URL       string     `json:"url,omitempty"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
}

// feedRefresh is how often a calendar app is asked to read the link again.
const feedRefresh = time.Hour

// maxFeedCycles is the most cycles a feed carries: the newest.
const maxFeedCycles = 200

// Seams for tests.
var (
	feedsAllowed = func(ctx context.Context) bool {
		return policyBusiness.Policies.Bool(ctx, policyBusiness.CalendarFeedsAllowed)
	}
	liveFeed     = cycleModel.LiveFeed
	feedByHash   = cycleModel.FeedByHash
	makeFeed     = cycleModel.MakeFeed
	revokeFeed   = cycleModel.RevokeFeed
	uidOf        = accessBusiness.UIDOf
	feedProjects = projectDomain.GetProjectsReadAccessFor
	feedStanding = workAccess.OfMany
	feedCycles   = List
	newFeedID    = uuid.New
)

// FeedURL is the public address of a link's token: the API's own, which a
// calendar app reads with no session.
func FeedURL(raw string) string {
	return authService.BackendBaseURL() + "/public/cycles/" + raw + ".ics"
}

// FeedTokenOf is the token in a link's last path element ("….ics"). Pure.
func FeedTokenOf(file string) string { return strings.TrimSuffix(strings.TrimSpace(file), ".ics") }

// Feed is person's link to the project's cycles: whether they have one, and since when.
func Feed(ctx context.Context, project, person uuid.UUID) (*FeedView, error) {
	f, err := liveFeed(ctx, project, person)
	if err != nil || f == nil {
		return &FeedView{}, err
	}
	at := f.CreatedAt
	return &FeedView{Live: true, CreatedAt: &at}, nil
}

// MakeFeed gives person a new link to the project's cycles, shown this once;
// a link they had stops at once. The caller has checked they may view it.
func MakeFeed(ctx context.Context, project, person uuid.UUID) (*FeedView, error) {
	if !feedsAllowed(ctx) {
		return nil, &CycleError{"Your workspace has turned off links to calendars."}
	}
	raw, hash, err := helpers.NewLinkSecret()
	if err != nil {
		return nil, err
	}
	f := cycleModel.Feed{ID: newFeedID(), ProjectUUID: project, PersonID: person, TokenHash: hash}
	if err := makeFeed(ctx, f); err != nil {
		return nil, err
	}
	now := time.Now()
	return &FeedView{Live: true, URL: FeedURL(raw), CreatedAt: &now}, nil
}

// StopFeed stops person's link to the project's cycles: it answers nothing at once.
func StopFeed(ctx context.Context, project, person uuid.UUID) error {
	return revokeFeed(ctx, project, person)
}

// FeedICS is the project's cycles a link's token names, if its person may
// view the project now.
func FeedICS(ctx context.Context, token string, now time.Time) ([]byte, error) {
	if token == "" || !feedsAllowed(ctx) {
		return nil, ErrFeedNotFound
	}
	f, err := feedByHash(ctx, helpers.LinkSecretHash(token))
	if err != nil {
		return nil, err
	}
	if f == nil {
		return nil, ErrFeedNotFound
	}
	// Someone who isn't an active person in the workspace has no graph uid.
	uid, err := uidOf(ctx, accessBusiness.Subject{UserUUID: f.PersonID.String()})
	if errors.Is(err, helpers.ErrNotFound) {
		return nil, ErrFeedNotFound
	}
	if err != nil {
		return nil, err
	}
	if uid == "" {
		return nil, ErrFeedNotFound
	}
	projects, err := feedProjects(ctx, []string{f.ProjectUUID.String()}, uid)
	if err != nil {
		return nil, err
	}
	standings, err := feedStanding(ctx, f.PersonID.String(), projects)
	if err != nil {
		return nil, err
	}
	s, ok := standings[strings.ToLower(f.ProjectUUID.String())]
	if !ok || s.Archived || !s.Can(workAccess.NeedView) || len(projects) == 0 || projects[0] == nil {
		return nil, ErrFeedNotFound
	}
	cycles, err := feedCycles(ctx, f.ProjectUUID, now)
	if err != nil {
		return nil, err
	}
	return CyclesICS(projects[0].Name, cycles, now), nil
}

// CyclesICS is a project's cycles as a calendar: one all-day event each, the
// newest maxFeedCycles of them. Pure.
func CyclesICS(project string, cycles []View, now time.Time) []byte {
	if len(cycles) > maxFeedCycles {
		cycles = cycles[len(cycles)-maxFeedCycles:]
	}
	evs := make([]ICS.Event, 0, len(cycles))
	for _, c := range cycles {
		evs = append(evs, cycleEvent(project, c, now))
	}
	name := "Cycles"
	if project != "" {
		name = project + " cycles"
	}
	return ICS.WriteMessage(ICS.Message{Method: "PUBLISH", Name: name, Refresh: feedRefresh, Events: evs})
}

// cycleEvent is one cycle as an all-day event, free time (a cycle books
// nobody's day). Pure.
func cycleEvent(project string, c View, now time.Time) ICS.Event {
	label := c.Label()
	summary := label
	if project != "" {
		summary = project + ": " + label
	}
	desc := "No tasks yet"
	if c.Progress.Total > 0 {
		desc = fmt.Sprintf("%d of %d tasks done", c.Progress.Done, c.Progress.Total)
	}
	if c.CompletedAt != nil {
		desc = "Completed. " + desc
	}
	return ICS.Event{
		UID:         "cycle-" + c.Id.String() + "@onecamp",
		Stamp:       now,
		StartDate:   dayOf(c.StartsAt),
		EndDate:     dayOf(c.EndsAt),
		Summary:     summary,
		Description: desc,
		Transparent: true,
	}
}

// dayOf is the calendar day a cycle's edge falls on. A cycle starts and ends
// at midnight where the person who made it was (the app sends that moment), so
// the day is the UTC day nearest to it: right for every zone from UTC-12 to
// UTC+12. Pure.
func dayOf(t time.Time) string {
	return t.UTC().Add(12 * time.Hour).Format("2006-01-02")
}
