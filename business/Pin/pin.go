// Package business (Pin) is pinned messages (chat B25, PROGRAM.md D-M13): a
// message pinned to its conversation, a channel's or a DM's or group's, so
// everyone in it can find it again. Anyone who may post in a conversation may
// pin and unpin there; in an announcement channel that is its channel
// managers. A conversation holds up to 100 pins. Pinning leaves a quiet line
// in the conversation ("Maya pinned a message"); unpinning doesn't.
//
// The pin is a row in Postgres (models/postgres/MessagePin); the message stays
// in the graph and is read as it is now whenever the pins are read, so an
// edit shows and a deleted message's pin goes.
package business

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	mqttBusiness "github.com/akashc777/OneCamp/business/Mqtt"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	domain "github.com/akashc777/OneCamp/domain/Message"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	models "github.com/akashc777/OneCamp/models/postgres/MessagePin"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// accessDecision is the Access Resolver's answer on a conversation.
type accessDecision = accessBusiness.Decision

// Max is the most pins a conversation holds.
const Max = 100

// Person is someone as a pin names them.
type Person struct {
	UUID       string `json:"user_uuid"`
	Name       string `json:"user_name,omitempty"`
	FullName   string `json:"user_full_name,omitempty"`
	ProfileKey string `json:"user_profile_object_key,omitempty"`
	IsBot      bool   `json:"is_bot,omitempty"`
}

// Message is a pinned message as it is now.
type Message struct {
	ID        string     `json:"id"`
	HTML      string     `json:"html"`
	CreatedAt *time.Time `json:"created_at,omitempty"`
	By        *Person    `json:"by,omitempty"`
	Files     int        `json:"files,omitempty"`
}

// Pin is one pinned message: who pinned it, when, and the message.
type Pin struct {
	MessageID string    `json:"message_id"`
	PinnedBy  Person    `json:"pinned_by"`
	PinnedAt  time.Time `json:"pinned_at"`
	Message   Message   `json:"message"`
}

// Pins is a conversation's pins, newest first, and whether the asker may pin
// and unpin there.
type Pins struct {
	Pins   []Pin `json:"pins"`
	CanPin bool  `json:"can_pin"`
	Max    int   `json:"max"`
}

// Seams for tests.
var (
	resolve    = conversationBusiness.Resolve
	readMsgs   = domain.Messages
	readPeople = userDomain.GetPeopleByUUIDs
	addPin     = models.Add
	addPins    = models.AddAll
	removePin  = models.Remove
	listPins   = models.List
	forgetPins = models.Forget
	writeLine  = conversationBusiness.WriteLine
	publish    = mqttBusiness.PublishToTopics
)

func refuse(status int, msg string) error {
	return &sendBusiness.Rejection{Status: status, Msg: msg}
}

// kinds are the conversations a message can be pinned in.
func pinnable(ref conversationBusiness.Ref) bool {
	switch ref.Kind {
	case conversationBusiness.KindChannel, conversationBusiness.KindDM, conversationBusiness.KindGroup:
		return ref.ID != ""
	}
	return false
}

// mayPin is whether a decision on a conversation lets its holder pin there:
// they post in it, and it isn't archived. Pure.
func mayPin(d accessDecision) bool {
	return d.Level >= conversationBusiness.LevelPost && !d.Archived
}

// List is ref's pins as who reads them, with whether who may pin there.
// conversationBusiness.ErrNotFound when ref isn't there or who can't read it.
// A pin of a message that is gone, or no longer in ref, is left out and
// dropped.
func List(ctx context.Context, who *userModels.UserInfo, ref conversationBusiness.Ref) (Pins, error) {
	if !pinnable(ref) {
		return Pins{}, conversationBusiness.ErrNotFound
	}
	d, err := resolve(ctx, who, ref)
	if err != nil {
		return Pins{}, err
	}
	out := Pins{Pins: []Pin{}, CanPin: mayPin(d), Max: Max}
	rows, err := listPins(ctx, ref.Kind, ref.ID, Max)
	if err != nil || len(rows) == 0 {
		return out, err
	}
	ids := make([]string, len(rows))
	byIDs := make([]string, 0, len(rows))
	for i, r := range rows {
		ids[i] = r.MessageID.String()
		byIDs = append(byIDs, r.PinnedBy.String())
	}
	msgs, err := readMsgs(ctx, ref.Kind == conversationBusiness.KindChannel, ids)
	if err != nil {
		return Pins{}, err
	}
	found := make(map[string]domain.Message, len(msgs))
	for _, m := range msgs {
		found[strings.ToLower(m.ID)] = m
	}
	names := peopleByUUID(ctx, byIDs)
	var gone []uuid.UUID
	for _, r := range rows {
		m, ok := found[r.MessageID.String()]
		if !ok || !liveIn(m, ref) {
			gone = append(gone, r.MessageID)
			continue
		}
		out.Pins = append(out.Pins, Pin{
			MessageID: r.MessageID.String(),
			PinnedBy:  names(r.PinnedBy.String()),
			PinnedAt:  r.PinnedAt,
			Message:   Message{ID: r.MessageID.String(), HTML: m.HTML, CreatedAt: m.CreatedAt, By: personOf(m.By), Files: m.Files},
		})
	}
	if len(gone) > 0 {
		// Best effort: the read answers without them either way.
		if err := forgetPins(context.WithoutCancel(ctx), ref.Kind, ref.ID, gone); err != nil {
			helpers.LogErrorWithContext(ctx, "business/Pin List: forgetting %d gone pins in %s %s err: %+v", len(gone), ref.Kind, ref.ID, err)
		}
	}
	return out, nil
}

// liveIn is whether m is a person's message, still there, in ref. Pure.
func liveIn(m domain.Message, ref conversationBusiness.Ref) bool {
	return m.Kind != conversationBusiness.KindSystem && !helpers.IsSoftDeleted(m.DeletedAt) &&
		strings.EqualFold(strings.TrimSpace(m.In), strings.TrimSpace(ref.ID))
}

func personOf(p *domain.Person) *Person {
	if p == nil {
		return nil
	}
	return &Person{UUID: p.UUID, Name: p.Name, FullName: p.FullName, ProfileKey: p.ProfileKey, IsBot: p.IsBot}
}

// peopleByUUID reads people's names in one query and answers a lookup; a
// person it couldn't read is named by their uuid only.
func peopleByUUID(ctx context.Context, ids []string) func(string) Person {
	people, err := readPeople(ctx, ids)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "business/Pin peopleByUUID err: %+v", err)
	}
	byID := make(map[string]Person, len(people))
	for _, p := range people {
		if p == nil {
			continue
		}
		person := Person{UUID: p.Uuid, Name: p.UserName, FullName: p.UserFullName, IsBot: p.IsBot}
		if p.ProfileKey != nil {
			person.ProfileKey = *p.ProfileKey
		}
		byID[strings.ToLower(p.Uuid)] = person
	}
	return func(id string) Person {
		if p, ok := byID[strings.ToLower(id)]; ok {
			return p
		}
		return Person{UUID: id}
	}
}

// Set pins (pinned) or unpins messageID in ref as who. Pinning a pinned
// message, or unpinning one that isn't, changes nothing. A refusal is a
// *sendBusiness.Rejection in words; conversationBusiness.ErrNotFound when ref
// isn't there or who can't read it.
func Set(ctx context.Context, who *userModels.UserInfo, ref conversationBusiness.Ref, messageID string, pinned bool) error {
	if !pinnable(ref) {
		return conversationBusiness.ErrNotFound
	}
	msg, err := uuid.Parse(messageID)
	if err != nil {
		return refuse(http.StatusBadRequest, "That isn't a message.")
	}
	d, err := resolve(ctx, who, ref)
	if err != nil {
		return err
	}
	if !mayPin(d) {
		return cantPin(ref, d)
	}
	me, err := uuid.Parse(who.UserDgraphInfo.Uuid)
	if err != nil {
		return refuse(http.StatusForbidden, "You can't pin messages in this conversation.")
	}

	if !pinned {
		removed, err := removePin(ctx, ref.Kind, ref.ID, msg)
		if err != nil || !removed {
			return err
		}
		tell(ref, msg.String(), false, &who.UserDgraphInfo)
		return nil
	}

	if err := pinnableMessage(ctx, ref, msg); err != nil {
		return err
	}

	added, err := addPin(ctx, ref.Kind, ref.ID, msg, me, Max)
	if errors.Is(err, models.ErrFull) {
		return refuse(http.StatusConflict, "This conversation has 100 pinned messages, the most it holds. Unpin one first.")
	}
	if err != nil || !added {
		return err
	}
	tell(ref, msg.String(), true, &who.UserDgraphInfo)
	line := conversationBusiness.Line{Kind: conversationBusiness.LineMessagePinned, Actor: who.UserDgraphInfo.Uuid, To: msg.String()}
	if _, err := writeLine(context.WithoutCancel(ctx), ref, &who.UserDgraphInfo, line, who.UserDgraphInfo.DisplayName()+" pinned a message."); err != nil {
		// The pin stands; the line is a courtesy.
		helpers.LogErrorWithContext(ctx, "business/Pin Set: the pinned line in %s %s err: %+v", ref.Kind, ref.ID, err)
	}
	return nil
}

// pinnableMessage is nil when msg is a person's live message in ref, the only
// kind of message a pin holds, and the refusal in words otherwise.
func pinnableMessage(ctx context.Context, ref conversationBusiness.Ref, msg uuid.UUID) error {
	msgs, err := readMsgs(ctx, ref.Kind == conversationBusiness.KindChannel, []string{msg.String()})
	if err != nil {
		return err
	}
	var m *domain.Message
	if len(msgs) > 0 {
		m = &msgs[0]
	}
	return pinnableAs(ref, m)
}

// pinnableAs is pinnableMessage's rule for a message as read (nil for none
// read). Pure.
func pinnableAs(ref conversationBusiness.Ref, m *domain.Message) error {
	switch {
	case m == nil || !strings.EqualFold(m.In, ref.ID):
		return refuse(http.StatusNotFound, "That message isn't in this conversation.")
	case helpers.IsSoftDeleted(m.DeletedAt):
		return refuse(http.StatusNotFound, "That message was deleted.")
	case m.Kind == conversationBusiness.KindSystem:
		return refuse(http.StatusBadRequest, "A line about the conversation can't be pinned.")
	}
	return nil
}

// ImportedPin is a pin an import brings across: a message in its
// conversation, who pinned it and when.
type ImportedPin struct {
	Ref     conversationBusiness.Ref
	Message string
	By      uuid.UUID
	At      time.Time
}

// Imported pins each of pins as its pinner, at its time, for an import
// bringing conversations' pins across (business/SlackImport), in one read of
// the messages and one write. Each keeps Set's rules (a person's live message
// in its conversation, at most Max a conversation), but nothing is told live
// and no line is written: the import's own history says who pinned what, and
// nobody watches an import arrive. Pinning a pinned message changes nothing.
// refused says why each pin it didn't pin wasn't, by its index in pins.
func Imported(ctx context.Context, pins []ImportedPin) (refused map[int]error, err error) {
	refused = map[int]error{}
	var channels, chats []string
	for i, p := range pins {
		switch _, perr := uuid.Parse(p.Message); {
		case !pinnable(p.Ref):
			refused[i] = conversationBusiness.ErrNotFound
		case perr != nil:
			refused[i] = refuse(http.StatusBadRequest, "That isn't a message.")
		case p.Ref.Kind == conversationBusiness.KindChannel:
			channels = append(channels, p.Message)
		default:
			chats = append(chats, p.Message)
		}
	}
	read := map[string]domain.Message{}
	if err := readInto(ctx, true, channels, read); err != nil {
		return nil, err
	}
	if err := readInto(ctx, false, chats, read); err != nil {
		return nil, err
	}
	var rows []models.Row
	var from []int
	for i, p := range pins {
		if refused[i] != nil {
			continue
		}
		msg := uuid.MustParse(p.Message)
		var m *domain.Message
		if got, ok := read[strings.ToLower(msg.String())]; ok {
			m = &got
		}
		if why := pinnableAs(p.Ref, m); why != nil {
			refused[i] = why
			continue
		}
		rows = append(rows, models.Row{Kind: p.Ref.Kind, ID: p.Ref.ID, Message: msg, By: p.By, At: p.At})
		from = append(from, i)
	}
	pinned, err := addPins(ctx, rows, Max)
	if err != nil {
		return nil, err
	}
	for j, r := range rows {
		if !pinned[r.Message] {
			refused[from[j]] = refuse(http.StatusConflict, "This conversation has 100 pinned messages, the most it holds.")
		}
	}
	return refused, nil
}

// readInto reads the messages ids name, of one kind (a channel's posts when
// channel, else a DM's or group's messages), into read by lower-cased id: a
// conversation holds at most Max pins, so MaxRead at a time.
func readInto(ctx context.Context, channel bool, ids []string, read map[string]domain.Message) error {
	for len(ids) > 0 {
		n := min(len(ids), domain.MaxRead)
		msgs, err := readMsgs(ctx, channel, ids[:n])
		if err != nil {
			return err
		}
		for _, m := range msgs {
			read[strings.ToLower(m.ID)] = m
		}
		ids = ids[n:]
	}
	return nil
}

// cantPin says why d doesn't let its holder pin in ref. Pure.
func cantPin(ref conversationBusiness.Ref, d accessDecision) error {
	switch {
	case d.Archived:
		return refuse(http.StatusForbidden, "This channel is archived, so its pins can't change.")
	case ref.Kind == conversationBusiness.KindChannel && d.Level == conversationBusiness.LevelReply:
		// An announcement channel: its members reply in threads.
		return refuse(http.StatusForbidden, "Only channel managers can pin messages in this channel.")
	case ref.Kind == conversationBusiness.KindChannel:
		// A public channel read by someone not in it.
		return refuse(http.StatusForbidden, "Join the channel to pin messages in it.")
	}
	return refuse(http.StatusForbidden, "You can't pin messages in this conversation.")
}

// topicOf is the conversation's message topic, where its open apps listen.
func topicOf(ref conversationBusiness.Ref) string {
	if ref.Kind == conversationBusiness.KindChannel {
		return helpers.GetMqttTopicForChannelMessage(ref.ID)
	}
	return helpers.GetMqttTopicForDmMessage(ref.ID)
}

// tell tells the conversation's open apps of a pin or an unpin.
func tell(ref conversationBusiness.Ref, messageID string, pinned bool, by *dgraphStruct.DgraphUser) {
	publish([]string{topicOf(ref)}, mqttStruct.MESSAGE_PIN, &mqttStruct.MqttPin{
		Kind: ref.Kind, Id: ref.ID, MessageId: messageID, Pinned: pinned, ByUuid: by.Uuid, ByName: by.DisplayName(),
	})
}
