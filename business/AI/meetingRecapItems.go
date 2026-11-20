package business

// A call's recap, kept on the call (gcal-new-event-notes). The recap the
// agent already writes (one model call) is split once it is written: its
// words (summary and decisions) and its action items, each with who and by
// when when it names them, as the prompt asks it to write them ("@Name: the
// task (due YYYY-MM-DD)", the date looked up in the days the transcript is
// sent with). Both are kept on the call's transcript node whatever the notes
// document setting (domain/Recording CallRecap), so an event shows them on
// the event and every instance of its series, to whoever may read the call's
// transcript there. An event's call then tells the event's people
// (business/Calendar TellRecap): its members through the router, and its
// guests by email only when its organiser asked for it.

import (
	"context"
	"regexp"
	"strings"
	"time"

	calendarBusiness "github.com/akashc777/OneCamp/business/Calendar"
	liveKitBusiness "github.com/akashc777/OneCamp/business/LiveKit"
	recordingDomain "github.com/akashc777/OneCamp/domain/Recording"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/google/uuid"
)

// maxActionItemLen is the most of an action item's words kept.
const maxActionItemLen = 400

var (
	// actionItemsHeading is a recap's line that heads its action items:
	// "**📋 Action Items**", "### Action items:", "Action Items:".
	actionItemsHeading = regexp.MustCompile(`(?i)^\W*action\s+items?\W*$`)
	// recapBullet is a list line's marker: "- ", "* ", "• ", "1. ", "2) ",
	// and a checkbox after it.
	recapBullet = regexp.MustCompile(`^(?:[-*•+]|\d{1,2}[.)])\s+(?:\[[ xX]?\]\s*)?`)
	// itemOwner is an item that starts with whom it is for, as the prompt
	// asks: "@Maya Rao: …", or in bold, "**Maya**: …" or "**Maya:** …". A
	// plain "Word: …" is not read as a name: it is as often "Rollback: …".
	itemOwner = regexp.MustCompile(`^(?:@([^:\n]{1,60}):|\*\*([^*:\n]{1,60})(?::\*\*|\*\*:))\s*(.+)$`)
	// itemDue is an item's deadline as the prompt asks for it, "(due
	// 2026-11-14)", or "by 2026-11-14" in its words.
	itemDue      = regexp.MustCompile(`(?i)\s*\(\s*due:?\s*(\d{4}-\d{2}-\d{2})\s*\)`)
	itemDueByDay = regexp.MustCompile(`(?i)\b(?:by|before|on)\s+(\d{4}-\d{2}-\d{2})\b`)
)

// splitRecap is a recap as its call keeps it: its words without its action
// items section (Text), and that section's items, each with who and by when
// when it names them, at most recordingDomain.MaxActionItems. A recap with
// no such section keeps all its words and no items; "None" is no item. Pure.
func splitRecap(recap string) recordingDomain.CallRecap {
	lines := strings.Split(strings.ReplaceAll(recap, "\r\n", "\n"), "\n")
	var kept []string
	var items []recordingDomain.ActionItem
	in := false
	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case actionItemsHeading.MatchString(t):
			in = true
			continue
		case in && recordingDomain.IsRecapHeading(t):
			in = false
		}
		if !in {
			kept = append(kept, l)
			continue
		}
		if t = strings.TrimSpace(recapBullet.ReplaceAllString(t, "")); t == "" || recordingDomain.IsRecapNone(t) {
			continue
		}
		if len(items) < recordingDomain.MaxActionItems {
			items = append(items, parseActionItem(t))
		}
	}
	return recordingDomain.CallRecap{Text: strings.TrimSpace(strings.Join(kept, "\n")), Items: items}
}

// parseActionItem is one item's words as an action item: who it is for when it
// starts with them, its due date when it names one as a date, its words
// without either and without markdown's bold, at most maxActionItemLen. Pure.
func parseActionItem(t string) recordingDomain.ActionItem {
	var it recordingDomain.ActionItem
	if m := itemOwner.FindStringSubmatch(t); m != nil {
		it.Owner = strings.TrimSpace(helpers.FirstNonBlank(m[1], m[2]))
		t = m[3]
	}
	if m := itemDue.FindStringSubmatch(t); m != nil && isDate(m[1]) {
		it.Due = m[1]
		t = itemDue.ReplaceAllString(t, "")
	} else if m := itemDueByDay.FindStringSubmatch(t); m != nil && isDate(m[1]) {
		it.Due = m[1]
	}
	t = strings.TrimSpace(strings.ReplaceAll(t, "**", ""))
	if r := []rune(t); len(r) > maxActionItemLen {
		t = string(r[:maxActionItemLen-1]) + "…"
	}
	it.Text = t
	return it
}

// isDate is a real day written YYYY-MM-DD. Pure.
func isDate(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}

// recapCalendar is the day a call ended and the week after it, sent with its
// transcript so an action item's deadline ("by Friday") is written as a date
// the model looks up rather than counts (UpcomingDays). Pure.
func recapCalendar(ended time.Time) string {
	if ended.IsZero() {
		return ""
	}
	ended = ended.UTC()
	return "The call ended on " + ended.Format("Monday 2006-01-02") + " (UTC). " + UpcomingDays(ended, 7) + "\n\nTranscript:\n"
}

// Where a call's recap is kept and who is told of it (seams for tests).
var tellEventRecap = calendarBusiness.TellRecap

// keepRecap keeps a call's recap on its transcript node with what else it
// made (its notes document, and for an event's call the conversation its
// recap went to), and tells an event's people of it. Best-effort: a failure
// is logged, and never stops the recap's delivery.
func keepRecap(ctx context.Context, call liveKitBusiness.FinishedCall, home recapHome, recap, notesDoc, recapChat string) {
	kept := splitRecap(recap)
	if err := recordCallArtefacts(ctx, call.SessionKey, notesDoc, recapChat, kept); err != nil {
		helpers.LogErrorWithContext(ctx, "AI meeting recap: could not note the call's artefacts for room %q: %+v", call.Room, err)
	}
	if home.event == nil || home.home.Event == uuid.Nil {
		return
	}
	told := calendarBusiness.Recap{Key: call.SessionKey, Text: kept.Text, InChat: home.inChat(recapChat)}
	for _, it := range kept.Items {
		told.Items = append(told.Items, calendarBusiness.RecapItem{Text: it.Text, Owner: it.Owner, Due: it.Due})
	}
	if err := tellEventRecap(ctx, home.home.Event, told); err != nil {
		helpers.LogErrorWithContext(ctx, "AI meeting recap: the event's people couldn't be told of the recap for room %q: %+v", call.Room, err)
	}
}

// recapForChat is a recap as its chat post and its notes write it: each
// action item's owner in bold ("**Maya:** send the deck"), never as the
// prompt asks the model to write it ("@Maya: send the deck"). A plain
// "@Name" in a channel's post summons an agent of that name (business/AIAgent
// mentionMatches), and a recap's item is no message to its owner; the bold
// form is read back as the same item (itemOwner). Only an item's leading
// owner changes. Pure.
func recapForChat(recap string) string {
	lines := strings.Split(recap, "\n")
	in := false
	for i, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case actionItemsHeading.MatchString(t):
			in = true
			continue
		case in && recordingDomain.IsRecapHeading(t):
			in = false
		}
		if !in {
			continue
		}
		bullet := recapBullet.FindString(t)
		if m := itemOwner.FindStringSubmatch(t[len(bullet):]); m != nil && m[1] != "" {
			lines[i] = l[:len(l)-len(strings.TrimLeft(l, " \t"))] + bullet + "**" + strings.TrimSpace(m[1]) + ":** " + m[3]
		}
	}
	return strings.Join(lines, "\n")
}
