package business

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	domain "github.com/akashc777/OneCamp/domain/Message"
	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
	mqttStruct "github.com/akashc777/OneCamp/models/mqtt"
	models "github.com/akashc777/OneCamp/models/postgres/MessagePin"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

var (
	launch = conversationBusiness.Ref{Kind: conversationBusiness.KindChannel, ID: "c0a80101-0000-4000-8000-000000000001"}
	mayaID = "a0000000-0000-4000-8000-00000000000a"
	msgOne = "b0000000-0000-4000-8000-000000000001"
	msgTwo = "b0000000-0000-4000-8000-000000000002"
)

// world is a fake of what Pin reads and writes: one decision, the messages
// in the graph, the pin rows, and what was told and written.
type world struct {
	decision accessBusiness.Decision
	denied   error
	messages map[string]domain.Message
	rows     []models.Pin
	full     bool
	told     []string // "+id" pinned, "-id" unpinned
	lines    []string
	forgot   []uuid.UUID
	batches  int // AddAll writes
}

func useWorld(t *testing.T, w *world) {
	t.Helper()
	was := struct {
		resolve    func(context.Context, *userModels.UserInfo, conversationBusiness.Ref) (accessBusiness.Decision, error)
		readMsgs   func(context.Context, bool, []string) ([]domain.Message, error)
		readPeople func(context.Context, []string) ([]*dgraphStruct.DgraphUser, error)
		addPin     func(context.Context, string, string, uuid.UUID, uuid.UUID, int) (bool, error)
		addPins    func(context.Context, []models.Row, int) (map[uuid.UUID]bool, error)
		removePin  func(context.Context, string, string, uuid.UUID) (bool, error)
		listPins   func(context.Context, string, string, int) ([]models.Pin, error)
		forgetPins func(context.Context, string, string, []uuid.UUID) error
		writeLine  func(context.Context, conversationBusiness.Ref, *dgraphStruct.DgraphUser, conversationBusiness.Line, string) (string, error)
		publish    func([]string, int8, interface{})
	}{resolve, readMsgs, readPeople, addPin, addPins, removePin, listPins, forgetPins, writeLine, publish}
	t.Cleanup(func() {
		resolve, readMsgs, readPeople, addPin, addPins, removePin, listPins, forgetPins, writeLine, publish =
			was.resolve, was.readMsgs, was.readPeople, was.addPin, was.addPins, was.removePin, was.listPins, was.forgetPins, was.writeLine, was.publish
	})
	resolve = func(context.Context, *userModels.UserInfo, conversationBusiness.Ref) (accessBusiness.Decision, error) {
		return w.decision, w.denied
	}
	readMsgs = func(_ context.Context, _ bool, ids []string) ([]domain.Message, error) {
		var out []domain.Message
		for _, id := range ids {
			if m, ok := w.messages[id]; ok {
				out = append(out, m)
			}
		}
		return out, nil
	}
	readPeople = func(_ context.Context, ids []string) ([]*dgraphStruct.DgraphUser, error) {
		var out []*dgraphStruct.DgraphUser
		for _, id := range ids {
			if id == mayaID {
				out = append(out, &dgraphStruct.DgraphUser{Uuid: mayaID, UserName: "maya", UserFullName: "Maya Chen"})
			}
		}
		return out, nil
	}
	addPin = func(_ context.Context, _, _ string, msg, by uuid.UUID, max int) (bool, error) {
		for _, r := range w.rows {
			if r.MessageID == msg {
				return false, nil
			}
		}
		if w.full || len(w.rows) >= max {
			return false, models.ErrFull
		}
		w.rows = append([]models.Pin{{MessageID: msg, PinnedBy: by, PinnedAt: time.Now()}}, w.rows...)
		return true, nil
	}
	addPins = func(_ context.Context, rows []models.Row, max int) (map[uuid.UUID]bool, error) {
		w.batches++
		pinned := map[uuid.UUID]bool{}
		for _, r := range rows {
			if slices.ContainsFunc(w.rows, func(p models.Pin) bool { return p.MessageID == r.Message }) {
				pinned[r.Message] = true
				continue
			}
			if w.full || len(w.rows) >= max {
				continue
			}
			w.rows = append(w.rows, models.Pin{MessageID: r.Message, PinnedBy: r.By, PinnedAt: r.At})
			pinned[r.Message] = true
		}
		return pinned, nil
	}
	removePin = func(_ context.Context, _, _ string, msg uuid.UUID) (bool, error) {
		n := len(w.rows)
		w.rows = slices.DeleteFunc(w.rows, func(r models.Pin) bool { return r.MessageID == msg })
		return len(w.rows) < n, nil
	}
	listPins = func(context.Context, string, string, int) ([]models.Pin, error) { return slices.Clone(w.rows), nil }
	forgetPins = func(_ context.Context, _, _ string, ids []uuid.UUID) error {
		w.forgot = append(w.forgot, ids...)
		return nil
	}
	writeLine = func(_ context.Context, _ conversationBusiness.Ref, _ *dgraphStruct.DgraphUser, l conversationBusiness.Line, text string) (string, error) {
		w.lines = append(w.lines, l.Kind+" "+l.To+": "+text)
		return "line", nil
	}
	publish = func(topics []string, kind int8, data interface{}) {
		if len(topics) != 1 || topics[0] == "" || kind != mqttStruct.MESSAGE_PIN {
			t.Errorf("told %v (%d), want the conversation's one message topic, as a pin", topics, kind)
		}
		p, ok := data.(*mqttStruct.MqttPin)
		if !ok || p.Kind != launch.Kind && p.Kind != conversationBusiness.KindDM || p.ByUuid != mayaID {
			t.Errorf("told %#v", data)
			return
		}
		if p.Pinned {
			w.told = append(w.told, "+"+p.MessageId)
		} else {
			w.told = append(w.told, "-"+p.MessageId)
		}
	}
}

func maya() *userModels.UserInfo {
	return &userModels.UserInfo{UserDgraphInfo: dgraphStruct.DgraphUser{Uid: "0x10", Uuid: mayaID, UserName: "maya", UserFullName: "Maya Chen"}}
}

func live(in string) domain.Message {
	return domain.Message{ID: msgOne, HTML: "<p>launch plan</p>", In: in, By: &domain.Person{UUID: "j", Name: "jonas"}}
}

// A member pins a message once: it is in the list, named with who pinned
// it, the conversation is told, and a quiet line says so. Pinning it again,
// or unpinning one that isn't pinned, changes nothing and tells nobody.
func TestAMemberPinsAMessageOnce(t *testing.T) {
	w := &world{decision: accessBusiness.Decision{Level: conversationBusiness.LevelPost}, messages: map[string]domain.Message{msgOne: live(launch.ID)}}
	useWorld(t, w)
	ctx := context.Background()

	if err := Set(ctx, maya(), launch, msgOne, true); err != nil {
		t.Fatal(err)
	}
	if err := Set(ctx, maya(), launch, msgOne, true); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.told, []string{"+" + msgOne}) || len(w.lines) != 1 || !strings.Contains(w.lines[0], "message.pinned "+msgOne+": maya pinned a message.") {
		t.Fatalf("told %v, lines %v; want one pin told and one line", w.told, w.lines)
	}
	pins, err := List(ctx, maya(), launch)
	if err != nil || !pins.CanPin || pins.Max != 100 || len(pins.Pins) != 1 {
		t.Fatalf("list: %+v %v", pins, err)
	}
	if p := pins.Pins[0]; p.MessageID != msgOne || p.PinnedBy.FullName != "Maya Chen" || p.Message.HTML != "<p>launch plan</p>" || p.Message.By == nil || p.Message.By.Name != "jonas" {
		t.Fatalf("pin %+v", p)
	}

	if err := Set(ctx, maya(), launch, msgTwo, false); err != nil {
		t.Fatal(err)
	}
	if err := Set(ctx, maya(), launch, msgOne, false); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(w.told, []string{"+" + msgOne, "-" + msgOne}) || len(w.rows) != 0 {
		t.Fatalf("told %v, rows %v; want the unpin told once and no rows", w.told, w.rows)
	}
}

// Who may pin: whoever posts in the conversation. A member of an
// announcement channel, a public channel's reader and anyone in an archived
// channel are told why in words; someone who can't read it learns nothing.
func TestOnlyThoseWhoPostMayPin(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		d    accessBusiness.Decision
		ref  conversationBusiness.Ref
		want string
	}{
		{"an announcement channel's member", accessBusiness.Decision{Level: conversationBusiness.LevelReply}, launch, "Only channel managers can pin messages in this channel."},
		{"a public channel's reader", accessBusiness.Decision{Level: conversationBusiness.LevelRead}, launch, "Join the channel to pin messages in it."},
		{"an archived channel", accessBusiness.Decision{Level: conversationBusiness.LevelRead, Archived: true}, launch, "This channel is archived, so its pins can't change."},
		{"a DM with someone who left", accessBusiness.Decision{Level: conversationBusiness.LevelRead}, conversationBusiness.Ref{Kind: conversationBusiness.KindDM, ID: "a b"}, "You can't pin messages in this conversation."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := &world{decision: c.d, messages: map[string]domain.Message{msgOne: live(c.ref.ID)}}
			useWorld(t, w)
			err := Set(ctx, maya(), c.ref, msgOne, true)
			if rj, ok := sendBusiness.AsRejection(err); !ok || rj.Status != http.StatusForbidden || rj.Msg != c.want {
				t.Fatalf("got %v, want 403 %q", err, c.want)
			}
			pins, err := List(ctx, maya(), c.ref)
			if err != nil || pins.CanPin {
				t.Fatalf("list says can pin: %+v %v", pins, err)
			}
			if len(w.rows) != 0 || len(w.told) != 0 {
				t.Fatalf("wrote %v, told %v", w.rows, w.told)
			}
		})
	}

	w := &world{denied: conversationBusiness.ErrNotFound}
	useWorld(t, w)
	if err := Set(ctx, maya(), launch, msgOne, true); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Fatalf("someone who can't read it: %v", err)
	}
	if _, err := List(ctx, maya(), launch); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Fatalf("someone who can't read its pins: %v", err)
	}
	if err := Set(ctx, maya(), conversationBusiness.Ref{Kind: conversationBusiness.KindThread, ID: msgOne}, msgOne, true); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Fatalf("a thread isn't a place pins live: %v", err)
	}
}

// Only a person's live message in this conversation is pinned; at 100 pins a
// conversation is full, said in words.
func TestOnlyALiveMessageHereIsPinned(t *testing.T) {
	ctx := context.Background()
	deleted := time.Now()
	cases := []struct {
		name   string
		m      *domain.Message
		id     string
		status int
		want   string
	}{
		{"not a message id", nil, "x", http.StatusBadRequest, "That isn't a message."},
		{"no such message", nil, msgOne, http.StatusNotFound, "That message isn't in this conversation."},
		{"another channel's", &domain.Message{ID: msgOne, In: "other"}, msgOne, http.StatusNotFound, "That message isn't in this conversation."},
		{"a deleted message", &domain.Message{ID: msgOne, In: launch.ID, DeletedAt: &deleted}, msgOne, http.StatusNotFound, "That message was deleted."},
		{"a system line", &domain.Message{ID: msgOne, In: launch.ID, Kind: "system"}, msgOne, http.StatusBadRequest, "A line about the conversation can't be pinned."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := &world{decision: accessBusiness.Decision{Level: conversationBusiness.LevelPost}, messages: map[string]domain.Message{}}
			if c.m != nil {
				w.messages[msgOne] = *c.m
			}
			useWorld(t, w)
			err := Set(ctx, maya(), launch, c.id, true)
			if rj, ok := sendBusiness.AsRejection(err); !ok || rj.Status != c.status || rj.Msg != c.want {
				t.Fatalf("got %v, want %d %q", err, c.status, c.want)
			}
			if len(w.rows) != 0 || len(w.told) != 0 || len(w.lines) != 0 {
				t.Fatalf("wrote %v, told %v, lines %v", w.rows, w.told, w.lines)
			}
		})
	}

	w := &world{decision: accessBusiness.Decision{Level: conversationBusiness.LevelPost}, messages: map[string]domain.Message{msgOne: live(launch.ID)}, full: true}
	useWorld(t, w)
	err := Set(ctx, maya(), launch, msgOne, true)
	if rj, ok := sendBusiness.AsRejection(err); !ok || rj.Status != http.StatusConflict || !strings.Contains(rj.Msg, "100 pinned messages") {
		t.Fatalf("a full conversation: %v", err)
	}
}

// A pin of a message that was deleted, or moved out of the conversation, is
// left out of the list and its row dropped.
func TestTheListLeavesOutPinsOfMessagesThatAreGone(t *testing.T) {
	deleted := time.Now()
	gone := uuid.MustParse(msgTwo)
	w := &world{
		decision: accessBusiness.Decision{Level: conversationBusiness.LevelPost},
		messages: map[string]domain.Message{
			msgOne: live(launch.ID),
			msgTwo: {ID: msgTwo, In: launch.ID, DeletedAt: &deleted},
		},
		rows: []models.Pin{{MessageID: uuid.MustParse(msgOne), PinnedBy: uuid.MustParse(mayaID)}, {MessageID: gone, PinnedBy: uuid.MustParse(mayaID)}},
	}
	useWorld(t, w)
	pins, err := List(context.Background(), maya(), launch)
	if err != nil || len(pins.Pins) != 1 || pins.Pins[0].MessageID != msgOne {
		t.Fatalf("list %+v %v, want only the live one", pins, err)
	}
	if !slices.Equal(w.forgot, []uuid.UUID{gone}) {
		t.Fatalf("forgot %v, want the deleted one's pin", w.forgot)
	}
}

// An import's pins come across in one write, each as its pinner at its time,
// by Set's rules for the message: a message that isn't a person's live one in
// its conversation is refused in Set's words, and nobody is told or sees a
// line.
func TestAnImportsPinsAreOneWriteBySetsRules(t *testing.T) {
	deleted := time.Now()
	msgThree := "b0000000-0000-4000-8000-000000000003"
	w := &world{messages: map[string]domain.Message{
		msgOne:   live(launch.ID),
		msgTwo:   {ID: msgTwo, In: launch.ID, DeletedAt: &deleted},
		msgThree: {ID: msgThree, In: launch.ID, Kind: "system"},
	}}
	useWorld(t, w)
	bo := uuid.MustParse("b0b00000-0000-4000-8000-0000000000b0")
	at := time.Date(2025, 10, 1, 9, 1, 0, 0, time.UTC)
	refused, err := Imported(context.Background(), []ImportedPin{
		{Ref: launch, Message: msgOne, By: bo, At: at},
		{Ref: launch, Message: msgTwo, By: bo, At: at},
		{Ref: launch, Message: msgThree, By: bo, At: at},
		{Ref: launch, Message: "x", By: bo, At: at},
		{Ref: conversationBusiness.Ref{Kind: "thread", ID: msgOne}, Message: msgOne, By: bo, At: at},
	})
	if err != nil {
		t.Fatal(err)
	}
	if w.batches != 1 || len(w.rows) != 1 || w.rows[0].MessageID.String() != msgOne || w.rows[0].PinnedBy != bo || !w.rows[0].PinnedAt.Equal(at) {
		t.Fatalf("%d writes, rows %+v: want one write pinning the first, as Bo at his time", w.batches, w.rows)
	}
	for i, want := range map[int]string{1: "That message was deleted.", 2: "A line about the conversation can't be pinned.", 3: "That isn't a message."} {
		if rj, ok := sendBusiness.AsRejection(refused[i]); !ok || rj.Msg != want {
			t.Errorf("pin %d: %v, want %q", i, refused[i], want)
		}
	}
	if !errors.Is(refused[4], conversationBusiness.ErrNotFound) || refused[0] != nil || len(refused) != 4 {
		t.Errorf("refused %v", refused)
	}
	if len(w.told) != 0 || len(w.lines) != 0 {
		t.Errorf("told %v, lines %v: an import tells nobody", w.told, w.lines)
	}

	// A full conversation refuses the rest in Set's words; a pinned message
	// stays pinned.
	w.full = true
	refused, err = Imported(context.Background(), []ImportedPin{{Ref: launch, Message: msgOne, By: bo, At: at}, {Ref: launch, Message: msgThree, By: bo, At: at}})
	if err != nil || refused[0] != nil {
		t.Fatalf("the pinned one again: %v %v", refused, err)
	}
	w.messages[msgThree] = domain.Message{ID: msgThree, In: launch.ID}
	refused, _ = Imported(context.Background(), []ImportedPin{{Ref: launch, Message: msgThree, By: bo, At: at}})
	if rj, ok := sendBusiness.AsRejection(refused[0]); !ok || rj.Status != http.StatusConflict {
		t.Errorf("a full conversation: %v", refused[0])
	}
}
