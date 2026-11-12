package business

import (
	"context"
	"errors"
	"slices"

	notificationBusiness "github.com/akashc777/OneCamp/business/Notification"
	"github.com/akashc777/OneCamp/initializers/firebaseInit"
)

// rank is the causes, strongest first (threads-notifications.md, Plan step
// 3): each person hears of an event once, by the strongest.
var rank = []Cause{CauseDM, CauseMention, CauseReplyToYou, CauseThreadReply, CauseTeam, CauseKeyword, CauseBroadcast, CauseAllPosts, CauseCall, CauseReaction}

// strongest is the strongest of causes by rank. Pure.
func strongest(causes []Cause) Cause {
	best, at := Cause(""), len(rank)
	for _, c := range causes {
		if i := slices.Index(rank, c); i >= 0 && i < at {
			best, at = c, i
		}
	}
	return best
}

// deliveriesOf is how a cause tells someone before their hold: a reaction
// moves the inbox alone (never a push or the badge), a message where they
// hear everything rings without an inbox item, and what names them (a DM, a
// mention, an answer to their message) is emailed too. Pure.
func deliveriesOf(c Cause) []Delivery {
	switch c {
	case CauseReaction:
		return []Delivery{ToInbox}
	case CauseAllPosts, CauseCall:
		return []Delivery{ToPush, ToDesktop, ToSound}
	case CauseDM, CauseMention, CauseReplyToYou:
		return []Delivery{ToInbox, ToBadge, ToPush, ToDesktop, ToSound, ToEmail}
	}
	return []Delivery{ToInbox, ToBadge, ToPush, ToDesktop, ToSound}
}

// holdable is what a hold keeps back: email until it ends, the rest for good.
func holdable(d Delivery) bool {
	return d == ToPush || d == ToDesktop || d == ToSound || d == ToEmail
}

// levelLets is whether a setting for the place lets a cause through:
// "Everything" all of them, "Mentions and threads" all but a DM's or a
// channel's plain message and a call, "Nothing" (muted) none. Pure. It is
// the one copy of the rule: the database's eligibility query that restated
// it went with A4 H2, when every chat site came to ask Plan.
func levelLets(level string, c Cause) bool {
	switch level {
	case "all":
		return true
	case "mention":
		return c != CauseAllPosts && c != CauseDM && c != CauseCall
	}
	return false
}

var errNoAsker = errors.New("business/Notify Explain: nobody asked")

func (r router) Plan(ctx context.Context, ev Event) ([]Decision, error) {
	ds, _, err := r.plan(ctx, ev, "")
	return ds, err
}

// Explain is the plan for asker alone: only asker is followed past the
// audience, so only asker's settings are read, and the trace names nobody
// else.
func (r router) Explain(ctx context.Context, asker string, ev Event) (Trace, error) {
	if key(asker) == "" {
		return Trace{}, errNoAsker
	}
	ds, tr, err := r.plan(ctx, ev, asker)
	if err != nil {
		return Trace{}, err
	}
	tr.UserID, tr.EventID = asker, ev.ID
	for _, d := range ds {
		if key(d.UserID) == key(asker) {
			tr.Decision = &d
		}
	}
	return tr, nil
}

// candidate is one person in an event's audience: every cause they're in it
// by, in the order found, and what the plan decided step by step.
type candidate struct {
	id     string
	causes []Cause
	level  string
	cause  Cause
}

// plan is steps 1-5 for everyone, or for only one person when only is set
// (Explain), with that person's trace.
func (r router) plan(ctx context.Context, ev Event, only string) ([]Decision, Trace, error) {
	var tr Trace
	note := func(step, verdict string, causes ...Cause) {
		if only != "" {
			tr.Steps = append(tr.Steps, Step{Step: step, Verdict: verdict, Causes: causes})
		}
	}
	if only != "" && key(only) == key(ev.Actor) {
		note("audience", "actor")
		return nil, tr, nil
	}
	if ev.Place.Kind == "" {
		ds := r.sitePlan(ev, only, note)
		if ev.Cause != CauseTest {
			return ds, tr, nil
		}
		// A test notice (Test) is the one site's event the router holds
		// itself: it shows what the person's hold does to a notice.
		ds, err := r.hold(ctx, ev, ds, note)
		return ds, tr, err
	}

	// 1. The audience, by cause.
	byID := map[string]*candidate{}
	var order []*candidate
	add := func(ids []string, c Cause) {
		for _, id := range ids {
			k := key(id)
			if k == "" || k == key(ev.Actor) || (only != "" && k != key(only)) {
				continue
			}
			p := byID[k]
			if p == nil {
				p = &candidate{id: id}
				byID[k] = p
				order = append(order, p)
			}
			if !slices.Contains(p.causes, c) {
				p.causes = append(p.causes, c)
			}
		}
	}
	p := ev.Place
	inThread := p.ThreadRootID != ""
	var follows notificationBusiness.Follows
	switch {
	case ev.Cause == CauseReaction:
		add([]string{ev.Author}, CauseReaction)
	case ev.Cause == CauseMention:
		// A mention an edit added is heard by those it names alone
		// (TellAddedMentions).
		add(ev.Mentions, CauseMention)
	case ev.Cause == CauseCall:
		// A call rings those who hear everything where it started.
		ids, err := r.in.Members(ctx, p)
		if err != nil {
			return nil, tr, err
		}
		add(ids, CauseCall)
	default:
		// A DM's or a group's message is to its people: as the site has
		// them, or those who hear everything there (one on "Mentions and
		// threads" hears only what names them). Its thread's replies are
		// the thread's people's.
		if (p.Kind == PlaceDM || p.Kind == PlaceGroup) && !inThread {
			ids := ev.Audience
			if len(ids) == 0 {
				var err error
				if ids, err = r.in.Members(ctx, p); err != nil {
					return nil, tr, err
				}
			}
			add(ids, CauseDM)
		}
		add(ev.Mentions, CauseMention)
		add([]string{ev.Author}, CauseReplyToYou)
		if inThread {
			var err error
			if follows, err = r.in.Follows(ctx, p); err != nil {
				return nil, tr, err
			}
			add(ev.Repliers, CauseThreadReply)
			add(follows.On, CauseThreadReply)
		}
		// A team's mention and a broadcast reach no more than the write
		// recorded (Within), however many they reach now.
		if len(ev.Teams) > 0 {
			ids, err := r.in.Reached(ctx, ev, ev.Teams, "")
			if err != nil {
				return nil, tr, err
			}
			add(withinOf(ids, ev.Within), CauseTeam)
		}
		if ev.Broadcast != "" {
			ids, err := r.in.Reached(ctx, ev, nil, ev.Broadcast)
			if err != nil {
				return nil, tr, err
			}
			add(withinOf(ids, ev.Within), CauseBroadcast)
		}
		if text := ev.Push[firebaseInit.FIREBASE_PUSH_DATA_BODY]; text != "" && p.Kind == PlaceChannel {
			ids, err := r.in.Keywords(ctx, p, text, ev.Actor)
			if err != nil {
				return nil, tr, err
			}
			add(ids, CauseKeyword)
		}
		if p.Kind == PlaceChannel && !inThread {
			ids, err := r.in.Members(ctx, p)
			if err != nil {
				return nil, tr, err
			}
			add(ids, CauseAllPosts)
		}
	}
	if len(order) == 0 {
		note("audience", "not_in_audience")
		return nil, tr, nil
	}
	note("audience", "in", order[0].causes...)

	// 2. Access: one read of everyone's setting for the place, which only
	// those in it now have.
	ids := make([]string, len(order))
	for i, c := range order {
		ids[i] = c.id
	}
	levels, err := r.in.Levels(ctx, p, ids)
	if err != nil {
		return nil, tr, err
	}
	var ds []Decision
	var kept []*candidate
	for _, c := range order {
		level, in := levels[key(c.id)]
		if !in {
			note("access", NotInPlace)
			if slices.Contains(c.causes, CauseMention) {
				ds = append(ds, Decision{UserID: c.id, Cause: CauseMention, Note: NotInPlace})
			}
			continue
		}
		note("access", "in_place")
		c.level = level
		// 3. One cause a person: the strongest.
		c.cause = strongest(c.causes)
		note("cause", "chosen", c.cause)
		kept = append(kept, c)
	}

	// 4. Preference: the place's level and mute; in a thread, the thread's
	// people as its own rule hears them (business/Notification
	// ThreadAudience: the follow choice, and "Nothing" hearing only what is
	// addressed to them); never a broadcast from a thread.
	var told map[string]bool
	if inThread {
		aud := notificationBusiness.ThreadAudience(levels, ev.Actor, ev.Author, ev.Repliers, ev.Mentions, follows)
		told = make(map[string]bool, len(aud.Told))
		for _, id := range aud.Told {
			told[key(id)] = true
		}
	}
	var heard []Decision
	for _, c := range kept {
		verdict := "kept"
		switch {
		case inThread && c.cause == CauseBroadcast:
			verdict = "broadcast_in_thread"
		case inThread && (c.cause == CauseMention || c.cause == CauseReplyToYou || c.cause == CauseThreadReply):
			if !told[key(c.id)] {
				verdict = "thread_setting"
			}
		case !levelLets(c.level, c.cause):
			verdict = "level_" + c.level
		}
		note("preference", verdict)
		if verdict == "kept" {
			heard = append(heard, Decision{UserID: c.id, Cause: c.cause, To: deliveriesOf(c.cause)})
		}
	}

	// 5. The hold.
	heard, err = r.hold(ctx, ev, heard, note)
	if err != nil {
		return nil, tr, err
	}
	return append(heard, ds...), tr, nil
}

// hold is step 5: pause, schedule and focus time hold push, desktop and sound
// (for good) and email (until the hold ends); the inbox and badge still move.
// A VIP's words and Notify anyway pass. One read of who is held, and one of
// whose VIP the actor is, for those held alone.
func (r router) hold(ctx context.Context, ev Event, ds []Decision, note func(string, string, ...Cause)) ([]Decision, error) {
	var ask []string
	for _, d := range ds {
		if slices.ContainsFunc(d.To, holdable) {
			ask = append(ask, d.UserID)
		}
	}
	if len(ask) == 0 {
		for range ds {
			note("hold", "nothing_to_hold")
		}
		return ds, nil
	}
	held, err := r.in.Held(ctx, ask, r.now())
	if err != nil {
		return nil, err
	}
	var heldIDs []string
	for _, id := range ask {
		if held[key(id)].Reason != "" {
			heldIDs = append(heldIDs, id)
		}
	}
	vip := map[string]bool{}
	if len(heldIDs) > 0 && ev.Actor != "" {
		got, err := r.in.VIPs(ctx, ev.Actor, heldIDs)
		if err != nil {
			return nil, err
		}
		for id, is := range got {
			vip[key(id)] = is
		}
	}
	anyway := map[string]bool{}
	for _, id := range ev.Anyway {
		anyway[key(id)] = true
	}
	for i := range ds {
		d := &ds[i]
		st := held[key(d.UserID)]
		switch {
		case !slices.ContainsFunc(d.To, holdable):
			note("hold", "nothing_to_hold")
		case st.Reason == "":
			note("hold", "not_held")
		case vip[key(d.UserID)]:
			note("hold", "vip_passes")
		case anyway[key(d.UserID)]:
			note("hold", "anyway_passes")
		default:
			note("hold", "held:"+st.Reason)
			var to []Delivery
			for _, x := range d.To {
				if holdable(x) {
					d.Held = append(d.Held, x)
				} else {
					to = append(to, x)
				}
			}
			d.To, d.HoldTill = to, st.Until
		}
	}
	return ds, nil
}

// sitePlan is an event whose site decided who hears (the calendar's): those
// people, each once, never the actor, by inbox, push and desktop notice, or
// the inbox alone when the site says not to send. Their hold is the push
// gate's at delivery, as today. A test notice is a push and desktop notice
// alone (it has no inbox item), held in plan. Nothing is read.
func (r router) sitePlan(ev Event, only string, note func(string, string, ...Cause)) []Decision {
	to := []Delivery{ToInbox, ToPush, ToDesktop}
	switch {
	case ev.Cause == CauseTest:
		to = []Delivery{ToPush, ToDesktop}
	case ev.Quiet:
		to = []Delivery{ToInbox}
	}
	seen := map[string]bool{"": true, key(ev.Actor): true}
	var ds []Decision
	for _, id := range ev.Audience {
		if seen[key(id)] || (only != "" && key(id) != key(only)) {
			continue
		}
		seen[key(id)] = true
		ds = append(ds, Decision{UserID: id, Cause: ev.Cause, To: slices.Clone(to)})
	}
	if len(ds) == 0 {
		note("audience", "not_in_audience")
	} else {
		note("audience", "site", ev.Cause)
	}
	return ds
}
