package business

// Reactions like Slack's (chat B25, PROGRAM.md D-M7): a person may react to a
// message or comment with several emoji, up to 23 of them, and a message
// holds up to 50 different emoji. A reaction is one person's one emoji there:
// adding an emoji they already have changes nothing and answers the one they
// have, and taking it back takes back every copy of it (an add from before
// this made a new node each time, so some people hold one twice).
//
// Every surface goes through here: a channel's messages (OnPost), a DM's and
// a group's (OnChat), and replies, doc comments and task comments
// (OnComment). Each surface keeps its own side effects (its live event, its
// activity item, a DM's push) and runs them only for what really changed.

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"time"

	domain "github.com/akashc777/OneCamp/domain/Reaction"
	"github.com/akashc777/OneCamp/helpers"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
)

// The caps (D-M7).
const (
	// MaxPerPerson is how many emoji one person may add to one message.
	MaxPerPerson = 23
	// MaxDistinct is how many different emoji one message may hold.
	MaxDistinct = 50
)

// Error makes a refusal an error, so a write that refuses says why through
// the error it returns.
func (r *Refusal) Error() string { return r.Msg }

// Fault is whether err is a fault to log: an error that isn't a refusal. A
// refusal is the person's to read, not the log's.
func Fault(err error) bool {
	_, refused := AsRefusal(err)
	return err != nil && !refused
}

// Answer is the status and words a reaction write that didn't go through
// answers with: a refusal's own, and for a fault, failed (what to try again).
func Answer(err error, failed string) (int, string) {
	if r, ok := AsRefusal(err); ok {
		return r.Status, r.Msg
	}
	return http.StatusServiceUnavailable, failed
}

// AsRefusal is err's refusal, when it is one.
func AsRefusal(err error) (*Refusal, bool) {
	var r *Refusal
	if errors.As(err, &r) {
		return r, true
	}
	return nil, false
}

// emojiID is what an emoji's id may be: emoji-mart's ids ("+1", "tada",
// "white_check_mark") and room for a skin tone or a workspace's own emoji
// (":" separated), never markup or a long string.
var emojiID = regexp.MustCompile(`^[a-z0-9_+:\-]{1,100}$`)

// ValidEmoji is whether id can name an emoji. Pure.
func ValidEmoji(id string) bool { return emojiID.MatchString(id) }

var (
	errNotAnEmoji = &Refusal{Status: http.StatusBadRequest, Msg: "That isn't an emoji we know."}
	errTooMany    = &Refusal{Status: http.StatusConflict, Msg: "You can add up to 23 reactions to one message."}
	errFull       = &Refusal{Status: http.StatusConflict, Msg: "This message already has 50 different reactions. Add one of those instead."}
	errNotThere   = &Refusal{Status: http.StatusNotFound, Msg: "That message isn't there any more."}
	errNotYours   = &Refusal{Status: http.StatusForbidden, Msg: "You can only change or remove your own reaction."}
	errNoReaction = &Refusal{Status: http.StatusNotFound, Msg: "There's no such reaction."}
)

// plan decides an add of emoji by by from what the parent holds: the
// reaction they already have with it (nothing to write), or a refusal when a
// cap is reached. first is whether it would be their first reaction there.
// Pure.
func plan(held []domain.Held, by, emoji string) (existing string, first bool, refusal *Refusal) {
	mine := 0
	distinct := map[string]bool{}
	for _, h := range held {
		distinct[h.Emoji] = true
		if h.By != by {
			continue
		}
		mine++
		if h.Emoji == emoji && existing == "" {
			existing = h.UID
		}
	}
	if existing != "" {
		return existing, false, nil
	}
	if mine >= MaxPerPerson {
		return "", false, errTooMany
	}
	if !distinct[emoji] && len(distinct) >= MaxDistinct {
		return "", false, errFull
	}
	return "", mine == 0, nil
}

// mineWith is every reaction of by's with emoji. Pure.
func mineWith(held []domain.Held, by, emoji string) []string {
	var uids []string
	for _, h := range held {
		if h.By == by && h.Emoji == emoji {
			uids = append(uids, h.UID)
		}
	}
	return uids
}

// Parent is what a reaction is on.
type Parent struct {
	On On
	// UUID is the message's or comment's uuid.
	UUID string
	// ContentBy is the graph uid of who wrote it, "" when unknown.
	ContentBy string
}

// Added is what an add did.
type Added struct {
	// UID is the reaction's node: the one added, or the one they already had.
	UID string
	// Created is whether anything was written.
	Created bool
	// Retagged is whether UID was the person's other reaction, given this
	// emoji in place (an app from before several reactions changing its one).
	Retagged bool
	// First is whether it is the person's first reaction on the parent: a
	// reaction tells its author once, never once per emoji.
	First bool
	// ParentUID is the parent's graph node.
	ParentUID string
}

// Seams for tests.
var (
	reactionsOn = domain.ReactionsOn
	addOnce     = domain.AddOnce
	retag       = domain.Retag
	removeFrom  = domain.RemoveFrom
	now         = time.Now
)

// Add adds emoji to p as by (a graph uid), once: an emoji they already have
// there answers that reaction and writes nothing. The error is a *Refusal
// for a bad emoji, a cap or a parent that isn't there, and anything else is a
// fault.
func Add(ctx context.Context, p Parent, by, emoji string) (Added, error) {
	if !ValidEmoji(emoji) {
		return Added{}, errNotAnEmoji
	}
	parentUID, held, err := reactionsOn(ctx, string(p.On), p.UUID)
	if errors.Is(err, domain.ErrNoParent) {
		return Added{}, errNotThere
	}
	if err != nil {
		return Added{}, err
	}
	existing, first, refusal := plan(held, by, emoji)
	if refusal != nil {
		return Added{}, refusal
	}
	if existing != "" {
		return Added{UID: existing, ParentUID: parentUID}, nil
	}
	uid, created, err := addOnce(ctx, string(p.On), p.UUID, parentUID, domain.NewReaction{By: by, ContentBy: p.ContentBy, Emoji: emoji, At: now()})
	if errors.Is(err, domain.ErrNoParent) {
		return Added{}, errNotThere
	}
	if err != nil {
		return Added{}, err
	}
	// Not created: a twin of this add landed first (the key's @upsert), and
	// that one answers.
	return Added{UID: uid, Created: created, First: created && first, ParentUID: parentUID}, nil
}

// Told is how open apps are told of an add: a new reaction (TYPE_CREATE), or
// one given another emoji in place (TYPE_UPDATE). Only for an add that
// Created or Retagged.
func (a Added) Told() int8 {
	if a.Retagged {
		return mqttStruct.TYPE_UPDATE
	}
	return mqttStruct.TYPE_CREATE
}

// Removed is one reaction taken back.
type Removed struct {
	UID   string
	Emoji string
}

// RemoveMine takes back by's reaction reactionUID on p, and every other copy
// of the same emoji by them there. The reaction must be theirs: someone
// else's is refused, as is a uid p doesn't hold.
func RemoveMine(ctx context.Context, p Parent, by, reactionUID string) ([]Removed, error) {
	parentUID, held, err := reactionsOn(ctx, string(p.On), p.UUID)
	if errors.Is(err, domain.ErrNoParent) {
		return nil, errNotThere
	}
	if err != nil {
		return nil, err
	}
	var named *domain.Held
	for i := range held {
		if held[i].UID == reactionUID {
			named = &held[i]
			break
		}
	}
	switch {
	case named == nil:
		return nil, errNoReaction
	case named.By != by:
		return nil, errNotYours
	}
	return removeEmoji(ctx, p, parentUID, mineWith(held, by, named.Emoji), named.Emoji)
}

func removeEmoji(ctx context.Context, p Parent, parentUID string, uids []string, emoji string) ([]Removed, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	if err := removeFrom(ctx, string(p.On), parentUID, uids); err != nil {
		helpers.LogErrorWithContext(ctx, "business/Reaction removeEmoji %s on %s err: %+v", emoji, p.UUID, err)
		return nil, err
	}
	out := make([]Removed, 0, len(uids))
	for _, uid := range uids {
		out = append(out, Removed{UID: uid, Emoji: emoji})
	}
	return out, nil
}

// Replace is an app from before several reactions changing its one reaction
// (old) to emoji. That app keeps old's node and later takes it back by it, so
// old takes emoji in place, and the person's other copies of old's emoji go.
// When they already have emoji there, old and its copies go and that one
// answers; when old is gone already (another tab took it back), emoji is
// added. Apps loaded before this still send it, so it keeps working for them.
func Replace(ctx context.Context, p Parent, by, old, emoji string) (Added, []Removed, error) {
	if !ValidEmoji(emoji) {
		return Added{}, nil, errNotAnEmoji
	}
	parentUID, held, err := reactionsOn(ctx, string(p.On), p.UUID)
	if errors.Is(err, domain.ErrNoParent) {
		return Added{}, nil, errNotThere
	}
	if err != nil {
		return Added{}, nil, err
	}
	var named *domain.Held
	for i := range held {
		if held[i].UID == old {
			named = &held[i]
			break
		}
	}
	switch {
	case named == nil:
		added, err := Add(ctx, p, by, emoji)
		return added, nil, err
	case named.By != by:
		return Added{}, nil, errNotYours
	case named.Emoji == emoji:
		return Added{UID: old, ParentUID: parentUID}, nil, nil
	}
	if have := mineWith(held, by, emoji); len(have) > 0 {
		removed, err := removeEmoji(ctx, p, parentUID, mineWith(held, by, named.Emoji), named.Emoji)
		return Added{UID: have[0], ParentUID: parentUID}, removed, err
	}
	if refusal := retagFits(held, named.Emoji, emoji); refusal != nil {
		return Added{}, nil, refusal
	}
	changed, err := retag(ctx, string(p.On), parentUID, old, by, emoji, now())
	if err != nil {
		return Added{}, nil, err
	}
	if !changed {
		// A twin of this emoji landed first: old goes, and the twin answers.
		removed, err := removeEmoji(ctx, p, parentUID, mineWith(held, by, named.Emoji), named.Emoji)
		if err != nil {
			return Added{}, nil, err
		}
		added, err := Add(ctx, p, by, emoji)
		return added, removed, err
	}
	var copies []string
	for _, uid := range mineWith(held, by, named.Emoji) {
		if uid != old {
			copies = append(copies, uid)
		}
	}
	removed, err := removeEmoji(ctx, p, parentUID, copies, named.Emoji)
	return Added{UID: old, Retagged: true, ParentUID: parentUID}, removed, err
}

// retagFits is whether a reaction may change from one emoji to another under
// the cap of different emoji: only a change that brings in a new emoji while
// leaving the old one held can grow the count. Pure.
func retagFits(held []domain.Held, from, to string) *Refusal {
	distinct := map[string]int{}
	for _, h := range held {
		distinct[h.Emoji]++
	}
	if distinct[to] > 0 {
		return nil
	}
	n := len(distinct) + 1
	if distinct[from] == 1 {
		n--
	}
	if n > MaxDistinct {
		return errFull
	}
	return nil
}

// Set is every surface's add: emoji added to p as by, or, when an app from
// before several reactions names its one reaction (old), that one replaced
// with it.
func Set(ctx context.Context, p Parent, by, old, emoji string) (Added, []Removed, error) {
	if old != "" {
		return Replace(ctx, p, by, old, emoji)
	}
	added, err := Add(ctx, p, by, emoji)
	return added, nil, err
}
