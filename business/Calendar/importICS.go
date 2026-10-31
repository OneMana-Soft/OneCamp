package Calendar

// Importing a calendar file (calendar A4, SY-17): an .ics, or a zip of them
// (Google Calendar's export), into the person's own calendar, as a Content IO
// job with progress (business/ContentIO's Targeted importer), read through
// the one codec (business/ICS).
//
//   - A series comes in with its rule, the instances skipped in it and added
//     to it, and the instances changed on their own, as one write; a single
//     event as itself. All-day dates, zones, places, notes and alarms (as the
//     person's reminders) are kept.
//   - Guests come in as guests by email with the answers they gave, and are
//     never invited or told: the import is a copy of what happened elsewhere
//     (InviteImported). The person's own address is never a guest of theirs,
//     and an organiser who isn't them is a guest who said yes. Nobody in the
//     workspace gets it on their calendar by being named in a file.
//   - What can't come in is left out one event at a time, with the reason in
//     words: a bad event never loses the rest.
//   - An event already in the person's calendar (made here, or brought by an
//     earlier import of the same file) is left as it is and counted
//     (Unchanged), so importing a file twice makes nothing twice. One
//     someone else in the workspace has, with the same UID, comes in as the
//     person's own copy.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	contentIOBusiness "github.com/akashc777/OneCamp/business/ContentIO"
	ics "github.com/akashc777/OneCamp/business/ICS"
	workGraph "github.com/akashc777/OneCamp/domain/WorkGraph"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

func init() {
	contentIOBusiness.Pipeline.RegisterImporter(icsImporter{})
}

// The most one import takes: events (a series with its changed instances is
// one), calendar files in a zip, and one file's bytes once unzipped.
const (
	// maxCalendarNameRunes is the most a calendar's name holds.
	maxCalendarNameRunes = 120
	maxImportEvents      = 20000
	// maxNamedImports is how many of the events an import made its job names
	// (forJob); the rest are counted (contentIO Made.Count).
	maxNamedImports   = 100
	maxImportFiles    = 500
	maxImportICSBytes = 50 << 20
)

// icsImporter reads calendar files into a calendar.
type icsImporter struct{}

func (icsImporter) Format() string { return "ics" }

func (icsImporter) Into() string { return contentIOBusiness.IntoCalendar }

func (icsImporter) Accepts(filename, mediaType string) bool {
	switch strings.ToLower(path.Ext(filename)) {
	case ".ics", ".ical", ".icalendar", ".zip":
		return true
	}
	t, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(mediaType)), ";")
	return strings.TrimSpace(t) == "text/calendar"
}

// Import is never called: a calendar file is made into events by Make.
func (icsImporter) Import(context.Context, contentIOBusiness.Input, contentIOBusiness.Output) error {
	return errors.New("A calendar file is imported into a calendar.")
}

func (icsImporter) Make(ctx context.Context, who accessBusiness.Subject, in contentIOBusiness.Input, _ json.RawMessage, report func(done, total int)) (contentIOBusiness.Made, error) {
	cals, skipped, err := readCalendarFiles(in)
	if err != nil {
		return contentIOBusiness.Made{Skipped: skipped}, err
	}
	id, err := uuid.Parse(who.UserUUID)
	if err != nil {
		return contentIOBusiness.Made{}, ErrEventNotFound
	}
	user := &model.UserInfo{UserPostgresInfo: model.User{Id: id}, UserDgraphInfo: dgraphStruct.DgraphUser{Uid: who.UserUID, Uuid: who.UserUUID}}
	made, err := stored.importCalendars(ctx, user, cals, report)
	made.Skipped = append(skipped, made.Skipped...)
	return forJob(made), err
}

// forJob is an import as its job keeps it: the first maxNamedImports events
// named, the rest counted (Count), so a job of thousands stays small. The
// move from Google keeps every event it made, for its sync refs. Pure.
func forJob(made contentIOBusiness.Made) contentIOBusiness.Made {
	if len(made.Created) > maxNamedImports {
		made.Created = made.Created[:maxNamedImports:maxNamedImports]
	}
	return made
}

// readCalendarFiles reads an .ics, or every .ics in a zip, saying what it
// left out. It fails only when there is nothing it could read at all.
func readCalendarFiles(in contentIOBusiness.Input) ([]ics.Calendar, []contentIOBusiness.Skipped, error) {
	head := make([]byte, 2)
	n, _ := in.File.ReadAt(head, 0)
	isZip := n == 2 && head[0] == 'P' && head[1] == 'K'
	if !isZip {
		cal, err := ics.Read(io.LimitReader(in.Reader(), maxImportICSBytes))
		if err != nil {
			return nil, nil, err
		}
		return []ics.Calendar{cal}, skipsOf(cal, ""), nil
	}
	zr, err := zip.NewReader(in.File, in.Size)
	if err != nil {
		return nil, nil, errors.New("The zip couldn't be opened: it may be damaged.")
	}
	var cals []ics.Calendar
	var skipped []contentIOBusiness.Skipped
	for _, f := range zr.File {
		name := f.Name
		if f.FileInfo().IsDir() || strings.HasPrefix(path.Base(name), ".") || !strings.EqualFold(path.Ext(name), ".ics") {
			continue
		}
		if len(cals) >= maxImportFiles {
			skipped = append(skipped, contentIOBusiness.Skipped{What: name, Why: fmt.Sprintf("A zip can hold at most %d calendar files.", maxImportFiles)})
			continue
		}
		rc, err := f.Open()
		if err != nil {
			skipped = append(skipped, contentIOBusiness.Skipped{What: name, Why: "It couldn't be unzipped."})
			continue
		}
		data, err := io.ReadAll(io.LimitReader(rc, maxImportICSBytes+1))
		rc.Close()
		if err != nil || len(data) > maxImportICSBytes {
			skipped = append(skipped, contentIOBusiness.Skipped{What: name, Why: "It is larger than 50 MB unzipped."})
			continue
		}
		cal, err := ics.Read(bytes.NewReader(data))
		if err != nil {
			skipped = append(skipped, contentIOBusiness.Skipped{What: name, Why: err.Error()})
			continue
		}
		cals = append(cals, cal)
		skipped = append(skipped, skipsOf(cal, path.Base(name))...)
	}
	if len(cals) == 0 {
		return nil, skipped, errors.New("The zip holds no calendar (.ics) files.")
	}
	return cals, skipped, nil
}

func skipsOf(cal ics.Calendar, file string) []contentIOBusiness.Skipped {
	out := make([]contentIOBusiness.Skipped, 0, len(cal.Skipped))
	for _, s := range cal.Skipped {
		what := s.What
		if file != "" {
			what = file + ": " + what
		}
		out = append(out, contentIOBusiness.Skipped{What: what, Why: s.Why})
	}
	return out
}

// icsGroup is one event a file holds: a single event, or a series and the
// instances changed in it, which share its UID.
type icsGroup struct {
	uid       string
	main      *ics.Event
	overrides []ics.Event
	// file is the calendar file it first appears in.
	file int
}

// groupByUID gathers each file's events by UID, in the order they first
// appear. An instance whose series isn't in the file comes in on its own.
// Pure.
func groupByUID(cals []ics.Calendar) ([]*icsGroup, []contentIOBusiness.Skipped) {
	var order []*icsGroup
	byUID := map[string]*icsGroup{}
	var skipped []contentIOBusiness.Skipped
	for f, cal := range cals {
		for i := range cal.Events {
			ev := cal.Events[i]
			g := byUID[ev.UID]
			if g == nil {
				g = &icsGroup{uid: ev.UID, file: f}
				byUID[ev.UID] = g
				order = append(order, g)
			}
			switch {
			case ev.RecurrenceID != nil:
				g.overrides = append(g.overrides, ev)
			case g.main == nil:
				g.main = &ev
			default:
				skipped = append(skipped, contentIOBusiness.Skipped{What: titleOrUntitled(ev.Summary), Why: "It appears twice in the file: the first was kept."})
			}
		}
	}
	for _, g := range order {
		if g.main == nil && len(g.overrides) > 0 {
			first := g.overrides[0]
			first.RecurrenceID = nil
			g.main, g.overrides = &first, g.overrides[1:]
			for _, o := range g.overrides {
				skipped = append(skipped, contentIOBusiness.Skipped{What: titleOrUntitled(o.Summary), Why: "It changes one instance of a repeating event the file doesn't hold."})
			}
			g.overrides = nil
		}
	}
	return order, skipped
}

func titleOrUntitled(s string) string {
	if strings.TrimSpace(s) == "" {
		return "An event with no title"
	}
	return s
}

// importedUID is the UID a copy of an event someone else in the workspace
// has is kept under: UIDs are unique, and the copy is this calendar's.
func importedUID(uid string, calendar uuid.UUID) string { return uid + "~" + calendar.String() }

// importCalendars makes the files' events in who's own calendar.
func (s *service) importCalendars(ctx context.Context, who *model.UserInfo, cals []ics.Calendar, report func(done, total int)) (contentIOBusiness.Made, error) {
	return s.importFiles(ctx, who, cals, report, uuid.Nil)
}

// importFiles makes the files' events: each file into a calendar of its
// own as an import does, or, for a calendar read from an address
// (subscribed, calendar B8), every event into that one calendar as a copy
// that is the address's: read only, never busy, with no guests and no
// alarms, its UID kept apart from any the workspace has.
func (s *service) importFiles(ctx context.Context, who *model.UserInfo, cals []ics.Calendar, report func(done, total int), subscribed uuid.UUID) (contentIOBusiness.Made, error) {
	var made contentIOBusiness.Made
	if err := s.ready(ctx); err != nil {
		return made, err
	}
	me := personOf(who)
	if me == uuid.Nil {
		return made, ErrEventNotFound
	}
	zone := s.zoneOf(ctx, me)
	personal, err := s.store.personalCalendar(ctx, me, zone)
	if err != nil {
		return made, err
	}
	groups, skipped := groupByUID(cals)
	made.Skipped = append(made.Skipped, skipped...)
	if len(groups) > maxImportEvents {
		for _, g := range groups[maxImportEvents:] {
			made.Skipped = append(made.Skipped, contentIOBusiness.Skipped{What: titleOrUntitled(g.main.Summary), Why: fmt.Sprintf("One import takes at most %d events.", maxImportEvents)})
		}
		groups = groups[:maxImportEvents]
	}

	// What's here already, and whose addresses are whose (a subscription
	// keeps nobody's).
	var emails []string
	for _, cal := range cals {
		emails = append(emails, strings.ToLower(strings.TrimSpace(cal.Name)))
	}
	for _, g := range groups {
		emails = append(emails, strings.ToLower(g.main.Organizer.Email))
		for _, a := range g.main.Attendees {
			emails = append(emails, strings.ToLower(a.Email))
		}
	}
	accounts := map[string]uuid.UUID{}
	if subscribed == uuid.Nil {
		if accounts, err = s.store.peopleByEmail(ctx, emails); err != nil {
			return made, err
		}
	}
	mine := func(email string) bool { return accounts[strings.ToLower(strings.TrimSpace(email))] == me }

	// Each file is a calendar: Google names your own by your address, and
	// it comes into your calendar; each other one into a calendar of its
	// name, yours, found again by a later import.
	target := make([]uuid.UUID, len(cals))
	ours := map[uuid.UUID]bool{personal: true}
	for f, cal := range cals {
		name := clip(strings.TrimSpace(cal.Name), maxCalendarNameRunes-1)
		if subscribed != uuid.Nil {
			target[f], ours[subscribed] = subscribed, true
			continue
		}
		if name == "" || mine(name) {
			target[f] = personal
			continue
		}
		c, err := s.store.importedCalendar(ctx, me, name, zone)
		if err != nil {
			return made, err
		}
		target[f], ours[c] = c, true
	}

	uids := make([]string, 0, 2*len(groups))
	for _, g := range groups {
		uids = append(uids, g.uid, importedUID(g.uid, target[g.file]))
	}
	have, err := s.store.byICalUIDs(ctx, uids)
	if err != nil {
		return made, err
	}
	byUID := map[string]*models.Event{}
	ids := make([]uuid.UUID, 0, len(have))
	for _, e := range have {
		byUID[e.ICalUID] = e
		ids = append(ids, e.ID)
	}
	onIt, err := s.store.attendees(ctx, ids)
	if err != nil {
		return made, err
	}

	// Where they went, and what to show first: the next to come, else the latest.
	counts := map[uuid.UUID]int{}
	now := s.now()
	var next, latest *contentIOBusiness.Created
	var nextAt, latestAt time.Time
	var written []string
	for i, g := range groups {
		report(i, len(groups))
		what := titleOrUntitled(g.main.Summary)
		uid, calendar := g.uid, target[g.file]
		if _, ok := byUID[importedUID(uid, calendar)]; ok {
			made.Unchanged++ // from an earlier import
			continue
		}
		if subscribed != uuid.Nil {
			uid = importedUID(uid, calendar) // the address's copy, always
		} else if e := byUID[uid]; e != nil {
			if ours[e.CalendarID] || e.CreatedBy == me || personIn(onIt[e.ID], me) {
				made.Unchanged++ // made here, or the person is on it
				continue
			}
			uid = importedUID(uid, calendar)
		}
		created, why := s.importGroup(ctx, me, calendar, zone, uid, g, mine, subscribed != uuid.Nil)
		if why != "" {
			made.Skipped = append(made.Skipped, contentIOBusiness.Skipped{What: what, Why: why})
			continue
		}
		c := contentIOBusiness.Created{Kind: "event", UUID: created.ID.String(), Title: created.Title, At: importedAt(created)}
		written = append(written, c.UUID)
		made.Count++
		counts[calendar]++
		made.Created = append(made.Created, c)
		start, _ := spanOf(created)
		if at, ok := nextFrom(created, start, now); ok && (next == nil || at.Before(nextAt)) {
			shown := c
			if !at.Equal(start) {
				shown.At = at.UTC().Format(time.RFC3339) // a series that goes on from now
			}
			next, nextAt = &shown, at
		}
		if latest == nil || start.After(latestAt) {
			latest, latestAt = &c, start
		}
	}
	report(len(groups), len(groups))
	// An import announces nothing, so the work graph is told of its events
	// here (graphNodes.go), an address's having no node, and search of them
	// all (searchEntries.go).
	if subscribed == uuid.Nil {
		nodesChanged(ctx, workGraph.KindEvent, written)
	}
	SearchEntriesChanged(ctx, written)
	made.Show = next
	if made.Show == nil {
		made.Show = latest
	}
	// Each calendar that got events, in the file's order; yours as Personal.
	seen := map[uuid.UUID]bool{}
	for f, cal := range cals {
		id := target[f]
		if seen[id] || counts[id] == 0 {
			continue
		}
		seen[id] = true
		into := contentIOBusiness.Into{UUID: id.String(), Kind: "external", Title: clip(strings.TrimSpace(cal.Name), maxCalendarNameRunes-1), Count: counts[id]}
		switch id {
		case personal:
			into.Kind, into.Title = "personal", "Personal"
		case subscribed:
			into.Kind = kindSubscribed
		}
		made.Into = append(made.Into, into)
	}
	return made, nil
}

// importedAt is an event's start as an import names it: an instant, or a date. Pure.
func importedAt(e *models.Event) string {
	if e.StartAt != nil {
		return e.StartAt.UTC().Format(time.RFC3339)
	}
	return e.StartDate
}

// nextFrom is when an event next happens from now: its start, if that is to
// come, or now for a series that goes on past it; false when it is over. Pure.
func nextFrom(e *models.Event, start, now time.Time) (time.Time, bool) {
	if !start.Before(now) {
		return start, true
	}
	if e.RRule != "" && (e.UntilAt == nil || !e.UntilAt.Before(now)) {
		return now, true
	}
	return time.Time{}, false
}

func personIn(guests []models.Attendee, person uuid.UUID) bool {
	for _, g := range guests {
		if g.UserID != nil && *g.UserID == person {
			return true
		}
	}
	return false
}

// importGroup writes one event of a file, with its changed instances, or
// says why it couldn't. A subscribed one is the address's: read only (its
// source says so), never busy, with no guests and no alarms.
func (s *service) importGroup(ctx context.Context, me, calendar uuid.UUID, zone, uid string, g *icsGroup, mine func(string) bool, subscribed bool) (*models.Event, string) {
	ev := g.main
	if strings.EqualFold(ev.Status, "CANCELLED") {
		return nil, "It was cancelled."
	}
	now := s.now()
	series, why := importedRow(*ev, zone)
	if why != "" {
		return nil, why
	}
	if ev.RRule != "" {
		if err := setRule(series, ev.RRule); err != nil {
			return nil, "Its repeat couldn't be read: " + err.Error()
		}
		series.Exdates = append(series.Exdates, ev.ExDates...)
		series.Rdates = append(series.Rdates, ev.RDates...)
	}
	series.ID, series.CalendarID, series.CreatedBy = s.newID(), calendar, me
	series.ICalUID, series.Source, series.Sequence = uid, sourceICS, 0
	if subscribed {
		series.Source, series.Transparency = sourceSubscribed, transparencyTransparent
	}
	series.CreatedAt, series.UpdatedAt = now, now
	if err := check(series); err != nil {
		return nil, err.Error()
	}

	b := batch{inserts: []*models.Event{series}, now: now, by: &me}
	if !subscribed {
		guests, err := importedGuests(series.ID, *ev, mine, now)
		if err != nil {
			return nil, "Its guests couldn't be kept."
		}
		b.guests = guests
	}
	if series.RRule != "" {
		for _, o := range g.overrides {
			rid := o.RecurrenceID.UTC()
			if series.AllDay() {
				rid = time.Date(rid.Year(), rid.Month(), rid.Day(), 0, 0, 0, 0, time.UTC)
			}
			if !isInstance(series, rid) {
				continue // a change to an instance the rule doesn't make
			}
			if strings.EqualFold(o.Status, "CANCELLED") {
				series.Exdates = append(series.Exdates, rid)
				continue
			}
			row, why := importedRow(o, zone)
			if why != "" || row.AllDay() != series.AllDay() {
				continue
			}
			inst := madeInstance(series, rid)
			row.Kind, row.Visibility, row.Hue = series.Kind, series.Visibility, series.Hue
			row.GuestsCanModify, row.GuestsCanInvite, row.GuestsCanSeeGuests = series.GuestsCanModify, series.GuestsCanInvite, series.GuestsCanSeeGuests
			s.newOverride(row, inst, now)
			row.Source, row.Sequence, row.UpdatedAt = series.Source, 0, now
			if subscribed {
				row.Transparency = transparencyTransparent
			}
			row.Changed = changesOf(inst.Event, row)
			if len(row.Changed) == 0 {
				continue
			}
			b.inserts = append(b.inserts, row)
		}
	}
	if err := s.store.apply(ctx, b); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: an imported event couldn't be written: %v", err)
		return nil, "It couldn't be saved."
	}
	if !subscribed {
		s.importAlarms(ctx, me, series, ev.Alarms, now)
	}
	return series, ""
}

// importedRow is a file's event as a row (text longer than an event holds is
// cut, with an ellipsis, by clip): its times, title, notes, place and
// whether it shows as busy. Its rule, ids and guests are the caller's.
func importedRow(ev ics.Event, zone string) (*models.Event, string) {
	e := blank(zone)
	e.Title = clip(strings.TrimSpace(ev.Summary), maxTitleRunes-1)
	e.Notes = clip(importedNotes(ev), maxNotesRunes-1)
	e.Location = clip(strings.TrimSpace(ev.Location), maxPlaceRunes-1)
	if ev.Transparent {
		e.Transparency = transparencyTransparent
	}
	var start, end When
	if ev.StartDate != "" {
		start, end = When{Date: ev.StartDate}, When{Date: ev.EndDate}
	} else {
		z := ev.Zone
		if z == "" {
			z = "UTC"
		}
		st, en := ev.Start.UTC(), ev.End.UTC()
		start, end = When{At: &st, Zone: z}, When{At: &en, Zone: z}
	}
	if err := setWhen(e, start, end); err != nil {
		return nil, "Its times couldn't be kept: " + err.Error()
	}
	return e, ""
}

// importedNotes is an event's notes with where it's joined first (so Join
// offers it, as it offers any call link in the notes) and its files' links
// after. Pure.
func importedNotes(ev ics.Event) string {
	notes := strings.TrimSpace(ev.Description)
	if c := ev.Conference; c != "" && !strings.Contains(notes, c) {
		notes = strings.TrimSpace("Join: " + c + "\n\n" + notes)
	}
	var files []string
	for _, a := range ev.Attachments {
		if strings.Contains(notes, a.URL) {
			continue
		}
		name := strings.TrimSpace(a.Name)
		if name == "" {
			name = "A file"
		}
		files = append(files, "- "+name+": "+a.URL)
	}
	if len(files) > 0 {
		notes = strings.TrimSpace(notes + "\n\nAttachments:\n" + strings.Join(files, "\n"))
	}
	return notes
}

// The answers a file's PARTSTAT gives, as the calendar keeps them.
var importedAnswers = map[string]string{
	ics.Accepted: responseAccepted, ics.Declined: responseDeclined, ics.Tentative: responseTentative,
}

// importedGuests are a file's guests, and its organiser when that isn't the
// person, as guests by email with their answers; never the person.
func importedGuests(event uuid.UUID, ev ics.Event, mine func(string) bool, now time.Time) ([]models.Attendee, error) {
	var out []models.Attendee
	seen := map[string]bool{}
	add := func(a ics.Attendee) error {
		email := strings.ToLower(strings.TrimSpace(a.Email))
		if email == "" || seen[email] || mine(email) || len(out) >= maxGuests || !strings.Contains(email, "@") {
			return nil
		}
		seen[email] = true
		response := importedAnswers[strings.ToUpper(a.PartStat)]
		if response == "" {
			response = responseNeedsAction
		}
		role := roleRequired
		if a.Optional {
			role = roleOptional
		}
		g := models.Attendee{EventID: event, Email: email, Name: clip(strings.TrimSpace(a.Name), maxGuestNameRunes-1), Role: role,
			Response: response, AddedAt: now, InviteState: models.InviteImported}
		if response != responseNeedsAction {
			at := now
			g.RespondedAt = &at
		}
		g, err := withLink(g)
		if err != nil {
			return err
		}
		out = append(out, g)
		return nil
	}
	if o := ev.Organizer; o.Email != "" {
		if err := add(ics.Attendee{Address: o, PartStat: ics.Accepted}); err != nil {
			return nil, err
		}
	}
	for _, a := range ev.Attendees {
		if err := add(a); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// importAlarms keeps a file's alarms as the person's reminders for the
// event, when they fit what a reminder can be.
func (s *service) importAlarms(ctx context.Context, me uuid.UUID, e *models.Event, alarms []ics.Alarm, now time.Time) {
	if len(alarms) == 0 {
		return
	}
	minutes := make([]int, 0, len(alarms))
	for _, a := range alarms {
		minutes = append(minutes, int(a.Before/time.Minute))
	}
	ok, err := checkReminders(minutes, e.AllDay())
	if err != nil || len(ok) == 0 {
		return
	}
	if err := s.rem.setReminders(ctx, e.ID, me, ok, now); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: an imported event's reminders couldn't be kept: %v", err)
		return
	}
	if err := s.planReminders(ctx, e.ID, []uuid.UUID{me}); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: an imported event's reminders couldn't be planned: %v", err)
	}
}
