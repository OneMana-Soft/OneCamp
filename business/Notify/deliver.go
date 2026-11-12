package business

// Deliver, Retract and Tell (A4 H2, "One door for chat"): every chat notice
// goes through Plan and then Deliver, which records one row per person and
// delivery (notification_deliveries, migration 585) and tells only what the
// record says is new, so a retry or a double fire tells nobody twice. A held
// delivery is a row in state held until the hold ends, for the Sweeper (A4
// H4). Deleting a message retracts its rows.
//
// Permission model: the rows are their person's alone (read by Explain for
// that person only, a later head); Retract runs only on a message's delete
// path, which has already checked who may delete it; nothing here is served
// over HTTP. An edit's mention (TellAddedMentions) is heard by those it names
// who may hear of a mention where it was written, never wider than its write.

import (
	"context"
	"slices"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/helpers"
	deliveryModels "github.com/akashc777/OneCamp/models/postgres/NotificationDelivery"
	"github.com/google/uuid"
)

// Seams for tests: the delivery rows, and the push for a planned event.
var (
	recordRows     = deliveryModels.Record
	retractRows    = deliveryModels.Retract
	retractNotices = notificationBusiness.RetractMentionNotices
	deliverThrough = notificationBusiness.DeliverThroughPause
)

func (r router) Deliver(ctx context.Context, ev Event, ds []Decision) []Decision {
	if len(ds) == 0 {
		return nil
	}
	told := ds
	if ev.ID != "" {
		told = r.record(ctx, ev, ds)
	}
	var push []string
	for _, d := range told {
		if slices.Contains(d.To, ToPush) || slices.Contains(d.To, ToDesktop) {
			push = append(push, d.UserID)
		}
	}
	if len(push) == 0 {
		return told
	}
	// A chat event's hold was decided in Plan (a VIP's words and Notify
	// anyway pass it); a site's event is held by the push gate, as today.
	if ev.Place.Kind == "" {
		deliver(ctx, push, ev.Push, ev.ID)
	} else if err := deliverThrough(ctx, push, ev.Push, ev.ID); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Notify Deliver %s: %+v", ev.ID, err)
	}
	return told
}

// record writes ds's rows, each (event, person, delivery) once, and answers
// ds with only the deliveries newly written; nobody whose every delivery was
// written before. When the rows can't be written, it is logged and everyone
// is told as planned: a record never fails the change that caused it.
func (r router) record(ctx context.Context, ev Event, ds []Decision) []Decision {
	now := r.now().UTC()
	var rows []deliveryModels.Row
	for _, d := range ds {
		if _, err := uuid.Parse(d.UserID); err != nil {
			continue
		}
		row := deliveryModels.Row{EventID: ev.ID, UserID: d.UserID, Cause: string(d.Cause), MessageID: ev.MessageID}
		for _, to := range d.To {
			row.Delivery, row.State, row.SentAt, row.HoldUntil = string(to), "sent", &now, nil
			rows = append(rows, row)
		}
		for _, to := range d.Held {
			row.Delivery, row.State, row.SentAt, row.HoldUntil = string(to), "held", nil, d.HoldTill
			rows = append(rows, row)
		}
	}
	if _, err := uuid.Parse(ev.MessageID); err != nil {
		for i := range rows {
			rows[i].MessageID = ""
		}
	}
	fresh, err := recordRows(ctx, rows)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Notify Deliver %s: could not record the deliveries, telling everyone as planned: %+v", ev.ID, err)
		return ds
	}
	isNew := make(map[string]bool, len(fresh))
	for _, f := range fresh {
		isNew[key(f.UserID)+"|"+f.Delivery] = true
	}
	var told []Decision
	for _, d := range ds {
		if _, err := uuid.Parse(d.UserID); err != nil {
			told = append(told, d) // not recorded: told as planned
			continue
		}
		keep := func(in []Delivery) []Delivery {
			var out []Delivery
			for _, to := range in {
				if isNew[key(d.UserID)+"|"+string(to)] {
					out = append(out, to)
				}
			}
			return out
		}
		n := d
		n.To, n.Held = keep(d.To), keep(d.Held)
		if len(n.To) > 0 || len(n.Held) > 0 {
			told = append(told, n)
		}
	}
	return told
}

func (router) Retract(ctx context.Context, messageID string) {
	if _, err := uuid.Parse(messageID); err != nil {
		return
	}
	people, err := retractRows(ctx, messageID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Notify Retract %s: %+v", messageID, err)
		return
	}
	// Every notice the message made goes with it: the inbox notice of a
	// mention an edit added (NoteMentionsAdded) reached only those told here.
	if len(people) > 0 {
		if err := retractNotices(ctx, messageID, people); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Notify Retract %s: its notices: %+v", messageID, err)
		}
	}
}

// Tell is Plan then Deliver, the one door every chat notice goes through: who
// hears of ev, told, and what was newly told answered (the site's live
// Activity and email go to those people). A plan that can't be made tells
// nobody and is logged; nothing fails the change that caused it.
func Tell(ctx context.Context, ev Event) []Decision {
	ds, err := Notices.Plan(ctx, ev)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Notify Tell %s: could not plan, telling nobody: %+v", ev.ID, err)
		return nil
	}
	return Notices.Deliver(ctx, ev, ds)
}

// TellAddedMentions tells whom a save of a message's words where (s: who its
// mention node named before and names now, s.ID the message) newly names, n's
// actor aside, through the one door, as any other cause: Plan chooses who of
// them hears of a mention there by their setting, of those in it now (never
// more than it names: mentionAdded), Deliver writes one row per person and
// delivery, cause chat.mention, about the message, so Why answers them and
// deleting it retracts them, and pushes as n reads. Its id is the message's,
// so a retry, a second save, or a remove and a re-add tells nobody twice. The
// door writes no inbox notice and sends no email: those it told are noted in
// the inbox, and those it emails whom the words name are emailed, as n reads
// (business/Notification NoteMentionsAdded). Answers who was told.
func TellAddedMentions(ctx context.Context, where Place, n notificationBusiness.Notice, s notificationBusiness.MentionSave) []Decision {
	if n.Actor == nil {
		return nil
	}
	added := notificationBusiness.MentionsAdded(s.Before, s.After, n.Actor.Uuid)
	if len(added) == 0 {
		return nil
	}
	ev := mentionAdded(where, n.Actor.Uuid, added)
	ev.ID, ev.MessageID, ev.Push = "mention:"+s.Kind+":"+s.ID, s.ID, notificationBusiness.PushData(n)
	told := Tell(ctx, ev)
	notificationBusiness.NoteMentionsAdded(ctx, n, s, By(told, ToInbox), By(told, ToEmail))
	return told
}

// mentionAdded is the event of the mentions an edit of actor's added where:
// heard by those it names alone, nobody else (Plan's CauseMention). Pure.
func mentionAdded(where Place, actor string, mentions []string) Event {
	return Event{Cause: CauseMention, Actor: actor, Mentions: mentions, Place: where}
}

// Told is who of ds hears of the event at all, now or after their hold, in
// order. Pure.
func Told(ds []Decision) []string {
	var out []string
	for _, d := range ds {
		if len(d.To) > 0 || len(d.Held) > 0 {
			out = append(out, d.UserID)
		}
	}
	return out
}

// By is who of ds is told by delivery, now or after their hold, in order.
// Pure.
func By(ds []Decision, delivery Delivery) []string {
	var out []string
	for _, d := range ds {
		if slices.Contains(d.To, delivery) || slices.Contains(d.Held, delivery) {
			out = append(out, d.UserID)
		}
	}
	return out
}

// Of is who of ds hears of the event by one of causes, in order. Pure.
func Of(ds []Decision, causes ...Cause) []string {
	var out []string
	for _, d := range ds {
		if slices.Contains(causes, d.Cause) && (len(d.To) > 0 || len(d.Held) > 0) {
			out = append(out, d.UserID)
		}
	}
	return out
}
