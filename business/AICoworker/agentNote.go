package aicoworker

// The daily note: the member's AI teammate comes to them.
//
// Everything a member needs to act on was already assembled (GetWhatNeedsMe:
// approvals waiting, overdue tasks, commitments, today's calendar), but it
// waited on a home-screen card and a bell for the member to go looking. The
// agents people now expect (Muse, Grok Bot) come to you instead. So the first
// time a member opens OneCamp on a given day, OneCamp AI leaves one short note
// in their DM with what needs them, as links, and an offer to help. They answer
// in that same DM, where the coworker already works.
//
// Properties:
//   - Once a day per member, whatever the number of tabs or instances: the day
//     is claimed in one statement (models/AgentNote.ClaimDay) before anything
//     is posted.
//   - The member's own day, from their browser, so it arrives in their morning
//     without a stored time zone. A date far from the server's is ignored.
//   - Nothing to say, nothing sent: an empty queue posts no note.
//   - No model call. The note is the queue, in words, so it costs nothing and
//     cannot be wrong about what it lists.
//   - Off when AI or the coworker is off, and each member can turn it off.

import (
	"context"
	"fmt"
	"html"
	"os"
	"strings"
	"time"

	aiAdapter "github.com/akashc777/OneCamp/adapter/AI"
	chatAdapter "github.com/akashc777/OneCamp/adapter/Chat"
	aiBusiness "github.com/akashc777/OneCamp/business/AI"
	chatBusiness "github.com/akashc777/OneCamp/business/Chat"
	userBusiness "github.com/akashc777/OneCamp/business/User"
	"github.com/akashc777/OneCamp/helpers"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	noteModel "github.com/akashc777/OneCamp/models/postgres/AgentNote"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	ai "github.com/akashc777/OneCamp/services/AI"
	"github.com/google/uuid"
)

// noteMaxItems keeps the note readable at a glance; the rest are one click away.
const noteMaxItems = 5

// NoteResult tells the client whether a note was left, and where.
type NoteResult struct {
	Posted bool `json:"posted"`
	// BotUUID is the DM to open: /app/chat/{bot_uuid}.
	BotUUID string `json:"bot_uuid,omitempty"`
	Items   int    `json:"items"`
}

// Seams.
var (
	noteClaimDay = noteModel.ClaimDay
	noteGates    = func(ctx context.Context) bool {
		if svc := ai.GetService(); svc == nil || !svc.IsEnabled() {
			return false
		}
		s, err := aiModels.GetSettings(ctx)
		return err == nil && s != nil && s.Enabled && s.CoworkerEnabled
	}
	noteQueue  = aiBusiness.GetWhatNeedsMe
	noteBot    = userBusiness.GetAutomationBot
	notePost   = postNoteAsBot
	noteNow    = time.Now
	noteLeftOn = noteModel.LeftOn
	// noteShared is the demo's shared visitor account: one person to the
	// server, a new person every few minutes in reality. Each of them should
	// find the note that was left today, not only whoever opened first.
	noteShared = func(u *userModels.UserInfo) bool {
		email := strings.TrimSpace(os.Getenv("DEMO_USER_EMAIL"))
		return os.Getenv("ONECAMP_DEMO_HOST") != "" && email != "" && strings.EqualFold(email, u.UserPostgresInfo.EmailID)
	}
)

// noteDay is the member's calendar day: theirs when it is plausible (within a
// day of the server's, which covers every time zone), the server's otherwise.
func noteDay(clientDay string, now time.Time) time.Time {
	today := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC)
	d, err := time.Parse("2006-01-02", strings.TrimSpace(clientDay))
	if err != nil || d.Before(today.AddDate(0, 0, -1)) || d.After(today.AddDate(0, 0, 1)) {
		return today
	}
	return d
}

// LeaveDailyNote leaves today's note for the member if there is one to leave.
func LeaveDailyNote(ctx context.Context, userInfo *userModels.UserInfo, clientDay, clientZone string) (*NoteResult, error) {
	if userInfo == nil || !noteGates(ctx) {
		return &NoteResult{}, nil
	}
	bot := noteBot(ctx)
	if bot == nil || bot.UUID == "" {
		return &NoteResult{}, nil
	}
	day := noteDay(clientDay, noteNow())
	claimed, err := noteClaimDay(ctx, userInfo.UserPostgresInfo.Id, day)
	if err != nil {
		return &NoteResult{BotUUID: bot.UUID}, err
	}
	if !claimed {
		if noteShared(userInfo) {
			if left, _ := noteLeftOn(ctx, userInfo.UserPostgresInfo.Id, day); left {
				// Point this visitor at the note already there; post nothing new.
				if q, qerr := noteQueue(ctx, userInfo); qerr == nil && q != nil && len(q.Items) > 0 {
					return &NoteResult{Posted: true, BotUUID: bot.UUID, Items: len(q.Items)}, nil
				}
			}
		}
		return &NoteResult{BotUUID: bot.UUID}, nil
	}
	queue, err := noteQueue(ctx, userInfo)
	if err != nil || queue == nil || !queue.Enabled || len(queue.Items) == 0 {
		return &NoteResult{BotUUID: bot.UUID}, err
	}
	if err := notePost(ctx, bot, userInfo, composeNote(firstName(userInfo), queue.Items, helpers.Location(clientZone), noteNow())); err != nil {
		return &NoteResult{BotUUID: bot.UUID}, err
	}
	return &NoteResult{Posted: true, BotUUID: bot.UUID, Items: len(queue.Items)}, nil
}

func firstName(u *userModels.UserInfo) string {
	name := strings.TrimSpace(u.UserDgraphInfo.UserName)
	if name == "" {
		return ""
	}
	return strings.Fields(name)[0]
}

// duePhrase says when something is due, in the member's zone: "due today at
// 5:00 PM", "due tomorrow at 9:00 AM", "overdue since Thu, Sep 25". Empty when
// the item has no exact time, so the caller falls back to its label.
func duePhrase(dueTime string, loc *time.Location, now time.Time) string {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(dueTime))
	if err != nil {
		return ""
	}
	t, now = t.In(loc), now.In(loc)
	day := func(x time.Time) time.Time { return time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, loc) }
	switch days := int(day(t).Sub(day(now)).Hours() / 24); {
	case t.Before(now) && days < 0:
		return "overdue since " + t.Format("Mon, Jan 2")
	case t.Before(now):
		return "overdue since " + t.Format("3:04 PM")
	case days == 0:
		return "due today at " + t.Format("3:04 PM")
	case days == 1:
		return "due tomorrow at " + t.Format("3:04 PM")
	default:
		return "due " + t.Format("Mon, Jan 2")
	}
}

// composeNote writes the note as chat HTML. Every value from the workspace is
// escaped: a task title is text, never markup.
func composeNote(name string, items []aiAdapter.AttentionItem, loc *time.Location, now time.Time) string {
	var b strings.Builder
	greeting := "Hi"
	if name != "" {
		greeting += " " + html.EscapeString(name)
	}
	count := "One thing needs you today:"
	if len(items) > 1 {
		count = fmt.Sprintf("%d things need you today:", len(items))
	}
	fmt.Fprintf(&b, "<p>%s. %s</p><ul>", greeting, count)
	for i, it := range items {
		if i == noteMaxItems {
			break
		}
		title := html.EscapeString(strings.TrimSpace(it.Title))
		if strings.HasPrefix(it.URL, "/") {
			// A path, not a full URL: the client routes /app links in place.
			title = fmt.Sprintf(`<a href="%s">%s</a>`, html.EscapeString(it.URL), title)
		}
		parts := []string{}
		if due := duePhrase(it.DueTime, loc, now); due != "" {
			parts = append(parts, due)
		} else {
			for _, p := range []string{it.Kind, it.Subtitle} {
				if p = strings.TrimSpace(p); p != "" {
					parts = append(parts, p)
				}
			}
		}
		if c := strings.TrimSpace(it.Context); c != "" {
			parts = append(parts, c)
		}
		if len(parts) > 0 {
			title += " · " + html.EscapeString(strings.Join(parts, " · "))
		}
		fmt.Fprintf(&b, "<li>%s</li>", title)
	}
	b.WriteString("</ul>")
	if len(items) > noteMaxItems {
		fmt.Fprintf(&b, "<p>And %d more on your home screen.</p>", len(items)-noteMaxItems)
	}
	b.WriteString("<p>Reply here and I&#39;ll help, for example: <em>draft an update on my overdue tasks</em>.</p>")
	b.WriteString("<p>I leave this once a day when you first open OneCamp. Turn it off in Settings, Notifications.</p>")
	return b.String()
}

// postNoteAsBot authors the note in the member's DM with the coworker, the
// same way the coworker's own replies are written there.
func postNoteAsBot(ctx context.Context, bot *userBusiness.BotIdentity, to *userModels.UserInfo, htmlText string) error {
	botInfo, err := aiBusiness.BuildUserInfoByUserUUID(ctx, bot.UUID)
	if err != nil || botInfo == nil {
		return fmt.Errorf("cannot build bot identity: %w", err)
	}
	toID := to.UserPostgresInfo.Id
	if toID == uuid.Nil {
		return fmt.Errorf("no recipient")
	}
	recipient, err := userBusiness.GetDgraphUserInfoByUUID(ctx, toID.String())
	if err != nil || recipient == nil {
		return fmt.Errorf("cannot resolve recipient: %w", err)
	}
	_, err = chatBusiness.CreateChat(ctx, &chatAdapter.ChatInfo{ToUuid: toID.String(), TextHtml: htmlText}, botInfo, recipient, toID, nil)
	return err
}

// NoteEnabled and SetNoteEnabled are the member's own switch.
func NoteEnabled(ctx context.Context, userID uuid.UUID) (bool, error) {
	return noteModel.Enabled(ctx, userID)
}

func SetNoteEnabled(ctx context.Context, userID uuid.UUID, enabled bool) error {
	return noteModel.SetEnabled(ctx, userID, enabled)
}
