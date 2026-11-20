package Calendar

// A call's recap reaches its event's people (gcal-new-event-notes). When the
// recap of an event's call is written (business/AI's meeting recap, as the
// server), TellRecap tells them:
//
//   - The event's members (its organiser and the members it invites: the
//     people who may read the call's transcript on the event, CallPeople)
//     hear of it through the Notification Router (business/Notify
//     calendar.recap): in their inbox, and by push and email as their own
//     rules let them (email_calendar_recaps, on unless they turn it off). The
//     notice names the event and says one line of the recap; the event shows
//     the rest to them. A member of the DM or group the recap was posted to
//     (Recap.InChat) has it there, and that message is all they get.
//   - Its guests by email get the recap by email, through the calendar's own
//     email path, only when its organiser turned on "Send the recap to
//     guests" for that event (recap_to_guests, off for every event until
//     then): the recap's words and its action items, never the transcript,
//     the recording, the notes document or anything else of the event's.
//
// WHO SETS IT. The switch is the organiser's alone (SetRecapToGuests), as
// "Email me when guests answer" is: a guest the organiser lets change the
// event is refused, as for any change of how the organiser's event tells
// people. A repeating event's switch is its series'.

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	notifyBusiness "github.com/akashc777/OneCamp/business/Notify"
	resourceEventBusiness "github.com/akashc777/OneCamp/business/ResourceEvent"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
	models "github.com/akashc777/OneCamp/models/postgres/CalendarV2"
	model "github.com/akashc777/OneCamp/models/postgres/User"
	emailService "github.com/akashc777/OneCamp/services/Email"
	"github.com/google/uuid"
)

// noticeRecap is the notice of a call's recap, by business/Notify's cause.
var noticeRecap = string(notifyBusiness.CauseCalendarRecap)

// Recap is a finished call's recap, as its event's people hear of it: the
// call it is of (Key, its node's), its words (Text: its summary and
// decisions) and its action items. It holds nothing else of the call.
type Recap struct {
	Key   string
	Text  string
	Items []RecapItem
	// InChat is who already had the recap in the event's conversation (the
	// members its DM or group is of, when the recap was posted there): the
	// chat message is all they get, never a second notice.
	InChat []uuid.UUID
}

// RecapItem is one of a recap's action items: what, who when it names them,
// and by when (YYYY-MM-DD) when it names a date.
type RecapItem struct {
	Text  string
	Owner string
	Due   string
}

// maxRecapLine is the most of a recap a notice says.
const maxRecapLine = 160

// TellRecap tells the people of the event whose call this recap is of, as the
// server (the recap has no reader): its members through the router, and its
// guests by email when its organiser turned on "Send the recap to guests".
// ErrEventNotFound for an event in the trash or not there.
func TellRecap(ctx context.Context, event uuid.UUID, r Recap) error {
	return stored.tellRecap(ctx, event, r)
}

func (s *service) tellRecap(ctx context.Context, event uuid.UUID, r Recap) error {
	if strings.TrimSpace(r.Text) == "" && len(r.Items) == 0 {
		return nil
	}
	e, guests, err := s.load(ctx, event)
	if err != nil {
		return err
	}
	owner := e
	if e.SeriesID != nil {
		// An instance's switch is its series', and so are its guests (an
		// instance changed on its own keeps none of its own).
		if series, seriesGuests, err := s.load(ctx, *e.SeriesID); err == nil {
			owner, guests = series, seriesGuests
		}
	}
	s.recapNotice(ctx, e, guests, r)
	if owner.RecapToGuests {
		s.mailRecap(ctx, e, guests, r)
	}
	return nil
}

// recapNotice tells the event's members (CallPeople: who may read its call's
// transcript) of its call's recap, each once, through the router's plan; not
// those who had it in the event's conversation (r.InChat).
func (s *service) recapNotice(ctx context.Context, e *models.Event, guests []models.Attendee, r Recap) {
	members := slices.DeleteFunc(callPeople(e, guests).Members, func(id uuid.UUID) bool { return slices.Contains(r.InChat, id) })
	if len(members) == 0 {
		return
	}
	people, inboxOnly, err := planned(ctx, noticeRecap, uuid.Nil, members, false)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Calendar: a recap's notice couldn't be planned: %v", err)
		return
	}
	if len(people) == 0 {
		return
	}
	recipients := make([]notificationBusiness.NoticeRecipient, 0, len(people))
	for _, id := range people {
		reason := "guest"
		if id == e.CreatedBy {
			reason = "organiser"
		}
		recipients = append(recipients, notificationBusiness.NoticeRecipient{UserUUID: id.String(), Reason: reason, InboxOnly: inboxOnly[id]})
	}
	ref := e.ID.String()
	tellNotice(ctx, notificationBusiness.Notice{
		Kind: noticeRecap, ResourceKind: KindEvent, ResourceUUID: ref, ResourceTitle: e.Title,
		Data:     map[string]string{"call": r.Key, "action_items": strconv.Itoa(len(r.Items))},
		DedupKey: noticeRecap + "|" + ref + "|" + r.Key,
		Title:    "Recap of " + e.Title, Body: recapLine(r), Path: eventPath(ref),
		Email: notificationBusiness.EventCalendarRecap,
		Push:  &notificationBusiness.Push{Type: firebaseInit.FIREBASE_PUSH_DATA_TYPE_CALENDAR, ID: ref, Tag: "calendar-recap:" + ref},
	}, recipients)
}

// recapLine is the one line of a recap its notice says: its first point,
// else how many action items it has. Pure.
func recapLine(r Recap) string {
	for _, l := range strings.Split(r.Text, "\n") {
		if l = strings.TrimSpace(l); recordingDomain.IsRecapHeading(l) {
			continue
		}
		l = strings.TrimSpace(strings.ReplaceAll(strings.TrimLeft(l, "-*•+ "), "**", ""))
		if l == "" || recordingDomain.IsRecapNone(l) {
			continue
		}
		if utf8.RuneCountInString(l) > maxRecapLine {
			l = string([]rune(l)[:maxRecapLine-1]) + "…"
		}
		return l
	}
	switch len(r.Items) {
	case 0:
		return "The call's recap is on the event."
	case 1:
		return "1 action item"
	}
	return strconv.Itoa(len(r.Items)) + " action items"
}

// mailRecap emails the event's guests by email its call's recap: its words
// and action items alone (recapMail), under the workspace's email rules,
// never from the public demo (outsidersMailed).
func (s *service) mailRecap(ctx context.Context, e *models.Event, guests []models.Attendee, r Recap) {
	to := emailGuests(guests)
	if len(to) == 0 || !outsidersMailed() {
		return
	}
	organiser := s.organiserAddress(ctx, e)
	replyTo := ""
	if organiser.Email != emailService.SenderAddress() {
		replyTo = organiser.Email
	}
	m := recapMail(r)
	for _, g := range to {
		m.TemplateData = emailService.TemplateData{
			RecipientName: g.Name, ActorName: organiser.Name, Title: "Recap of " + e.Title, BrandName: "OneCamp",
			Why: organiser.Name + " sends the recap of this meeting's call to its guests from OneCamp.",
		}
		htmlBody, text, err := emailService.RenderRecap(m)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: a recap's email couldn't be written: %v", err)
			return
		}
		if err := sendGuestMail(ctx, emailService.SendOptions{
			From: emailService.SenderAddress(), To: g.Email, Subject: "Recap: " + e.Title, HTML: htmlBody, Text: text, ReplyTo: replyTo,
			Keep: emailService.InvitationReserve, Tags: map[string]string{"kind": "calendar_recap"},
		}); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Calendar: a recap wasn't sent: %v", err)
		}
	}
}

// recapMail is a recap as its email says it: its words, a line each (a
// heading as a heading, without its markers), then its action items with who
// and by when. Nothing else is in it, and it is plain strings alone: the
// email's own template escapes every one (emailService.RenderRecap). Pure.
func recapMail(r Recap) emailService.RecapMail {
	var m emailService.RecapMail
	for _, l := range strings.Split(strings.TrimSpace(r.Text), "\n") {
		switch l = strings.TrimSpace(l); {
		case recordingDomain.IsRecapHeading(l):
			m.Lines = append(m.Lines, emailService.RecapLine{Text: strings.Trim(l, "*#: "), Heading: true})
		case l != "":
			m.Lines = append(m.Lines, emailService.RecapLine{Text: strings.ReplaceAll(l, "**", "")})
		}
	}
	for _, it := range r.Items {
		m.Items = append(m.Items, emailService.RecapMailItem{Text: it.Text, Owner: it.Owner, Due: it.Due})
	}
	return m
}

// SetRecapToGuests turns the organiser's "Send the recap to guests" on or off
// for an event (a series, from any of its instances), and answers the
// occurrence asked for as they read it now. The organiser's alone: a guest
// who may change the event is refused (ErrOnlyCreatorChanges), anyone else
// told there is no such event. It moves no sequence and tells no guest.
func (s *service) SetRecapToGuests(ctx context.Context, who *model.UserInfo, id string, on bool) (*Event, error) {
	if err := s.ready(ctx); err != nil {
		return nil, err
	}
	inst, guests, err := s.resolve(ctx, who, id)
	if err != nil {
		return nil, err
	}
	e, me := inst.owner(), personOf(who)
	if err := mayChange(roleOf(e, guests, me), e); err != nil {
		return nil, err
	}
	if me != e.CreatedBy {
		return nil, ErrOnlyCreatorChanges
	}
	changed, err := s.store.setRecapToGuests(ctx, e.ID, on)
	if err != nil {
		return nil, err
	}
	if changed {
		e.RecapToGuests = on
		s.announce(ctx, who, resourceEventBusiness.CalendarEventUpdated, e, guestIDs(guests), map[string]string{"changed": "recap_to_guests"})
	}
	return s.view(ctx, who, inst, guests), nil
}
