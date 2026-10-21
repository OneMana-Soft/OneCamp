package business

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	conversationBusiness "github.com/akashc777/OneCamp/business/Conversation"
	sendBusiness "github.com/akashc777/OneCamp/business/Send"
	models "github.com/akashc777/OneCamp/models/postgres/MessageDraft"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// Drafts kept on the server: what a key names, what a draft may hold, who may
// keep one where, and the words a refusal says. The stores are fakes here;
// tests/integration/drafts_test.go runs them for real.

const (
	ch   = "0c4a0000-0000-4000-8000-000000000001"
	root = "0c4a0000-0000-4000-8000-0000000000aa"
	ana  = "0c4a0000-0000-4000-8000-00000000000a"
	ben  = "0c4a0000-0000-4000-8000-00000000000b"
)

type fakeStore struct {
	rows     map[string]models.Draft
	readable map[conversationBusiness.Ref]bool
	writes   int
	limit    int
	full     bool
}

func withFakes(t *testing.T) *fakeStore {
	t.Helper()
	f := &fakeStore{rows: map[string]models.Draft{}, readable: map[conversationBusiness.Ref]bool{}, limit: 1000}
	was := []any{resolve, putDraft, delDraft, listDrafts, allowWrite, now}
	resolve = func(_ context.Context, _ *userModels.UserInfo, ref conversationBusiness.Ref) (accessBusiness.Decision, error) {
		if f.readable[ref] {
			return accessBusiness.Decision{Level: conversationBusiness.LevelPost}, nil
		}
		return accessBusiness.Decision{}, conversationBusiness.ErrNotFound
	}
	at := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	putDraft = func(_ context.Context, _ uuid.UUID, d models.Draft, max int) (time.Time, error) {
		if _, had := f.rows[d.Key]; !had && (f.full || len(f.rows) >= max) {
			return time.Time{}, models.ErrFull
		}
		at = at.Add(time.Second)
		d.UpdatedAt = at
		f.rows[d.Key] = d
		return at, nil
	}
	delDraft = func(_ context.Context, _ uuid.UUID, key string) (bool, error) {
		_, had := f.rows[key]
		delete(f.rows, key)
		return had, nil
	}
	listDrafts = func(_ context.Context, _ uuid.UUID, _ int) ([]models.Draft, error) {
		var out []models.Draft
		for _, d := range f.rows {
			out = append(out, d)
		}
		return out, nil
	}
	allowWrite = func(context.Context, uuid.UUID) bool {
		f.writes++
		return f.writes <= f.limit
	}
	now = func() time.Time { return time.Date(2026, 10, 17, 9, 30, 0, 0, time.UTC) }
	t.Cleanup(func() {
		resolve = was[0].(func(context.Context, *userModels.UserInfo, conversationBusiness.Ref) (accessBusiness.Decision, error))
		putDraft = was[1].(func(context.Context, uuid.UUID, models.Draft, int) (time.Time, error))
		delDraft = was[2].(func(context.Context, uuid.UUID, string) (bool, error))
		listDrafts = was[3].(func(context.Context, uuid.UUID, int) ([]models.Draft, error))
		allowWrite = was[4].(func(context.Context, uuid.UUID) bool)
		now = was[5].(func() time.Time)
	})
	return f
}

func me() *userModels.UserInfo {
	var u userModels.UserInfo
	u.UserPostgresInfo.Id = uuid.MustParse(ana)
	return &u
}

func refusal(t *testing.T, err error, status int, words string) {
	t.Helper()
	r, ok := sendBusiness.AsRejection(err)
	if !ok || r.Status != status || (words != "" && r.Msg != words) {
		t.Fatalf("got %v, want %d %q", err, status, words)
	}
}

func TestADraftKeyNamesOnePlace(t *testing.T) {
	ok := map[string]conversationBusiness.Ref{
		"channel:" + ch:                  {Kind: "channel", ID: ch},
		"channel:" + strings.ToUpper(ch): {Kind: "channel", ID: ch},
		"dm:" + ana + " " + ben:          {Kind: "dm", ID: ana + " " + ben},
		"group:9f86d081884c7d65":         {Kind: "group", ID: "9f86d081884c7d65"},
		"post-thread:" + root:            {Kind: "thread", ID: root},
		"chat-thread:" + root:            {Kind: "thread", ID: root},
	}
	for key, want := range ok {
		got, err := ParseKey(key)
		if err != nil || got != want {
			t.Errorf("%q: %+v %v, want %+v", key, got, err, want)
		}
	}
	for _, key := range []string{
		"", "channel:", "channel:not-a-uuid", "dm:" + ben + " " + ana, "dm:" + ana, "dm:" + ana + "  " + ben,
		"group:has space", "group:a/b", "thread:" + root, "post-thread:x", "doc:" + ch, "channel" + ch,
		"group:" + strings.Repeat("a", 200),
	} {
		if _, err := ParseKey(key); err == nil {
			t.Errorf("%q named a place", key)
		}
	}
}

func TestADraftIsKeptWhereItsWriterCanRead(t *testing.T) {
	f := withFakes(t)
	ctx := context.Background()
	f.readable[conversationBusiness.Ref{Kind: "channel", ID: ch}] = true

	kept, err := Put(ctx, me(), "channel:"+ch, Draft{HTML: "<p>Ship it Friday</p>", AlsoSend: true,
		ReplyTo: &ReplyTo{UUID: root, AuthorName: "Maya", Text: strings.Repeat("é", 400)}})
	if err != nil {
		t.Fatal(err)
	}
	if kept.Key != "channel:"+ch || kept.HTML != "<p>Ship it Friday</p>" || kept.UpdatedAt.IsZero() {
		t.Fatalf("kept %+v", kept)
	}
	// "Also send" is a channel thread's only, and the quoted reply is cut to size.
	if kept.AlsoSend || len([]rune(kept.ReplyTo.Text)) != maxReplyText {
		t.Fatalf("also send %v, reply text %d characters", kept.AlsoSend, len([]rune(kept.ReplyTo.Text)))
	}
	var stored ReplyTo
	if err := json.Unmarshal(f.rows["channel:"+ch].ReplyTo, &stored); err != nil || stored.UUID != root {
		t.Fatalf("stored reply %s: %v", f.rows["channel:"+ch].ReplyTo, err)
	}

	// A place its writer can't read keeps nothing, and says so.
	_, err = Put(ctx, me(), "dm:"+ana+" "+ben, Draft{HTML: "<p>hi</p>"})
	refusal(t, err, http.StatusNotFound, "That conversation isn't there any more, so its draft can't be kept.")
	_, err = Put(ctx, me(), "post-thread:"+root, Draft{HTML: "<p>hi</p>"})
	refusal(t, err, http.StatusNotFound, "That thread isn't there any more, so its draft can't be kept.")
	if len(f.rows) != 1 {
		t.Fatalf("kept %d drafts, want 1", len(f.rows))
	}

	// A channel thread's "Also send" is kept.
	f.readable[conversationBusiness.Ref{Kind: "thread", ID: root}] = true
	kept, err = Put(ctx, me(), "post-thread:"+root, Draft{HTML: "<p>yes</p>", AlsoSend: true})
	if err != nil || !kept.AlsoSend {
		t.Fatalf("thread draft %+v %v", kept, err)
	}
}

func TestABlankDraftEmptiesItsPlace(t *testing.T) {
	f := withFakes(t)
	ctx := context.Background()
	f.readable[conversationBusiness.Ref{Kind: "channel", ID: ch}] = true
	if _, err := Put(ctx, me(), "channel:"+ch, Draft{HTML: "<p>Ship it</p>"}); err != nil {
		t.Fatal(err)
	}
	for _, html := range []string{"", "<p></p>", "<p> &nbsp; </p><p><br></p>"} {
		f.rows["channel:"+ch] = models.Draft{Key: "channel:" + ch, HTML: "<p>Ship it</p>"}
		kept, err := Put(ctx, me(), "channel:"+ch, Draft{HTML: html})
		if err != nil || kept.HTML != "" || kept.UpdatedAt.IsZero() {
			t.Fatalf("%q: %+v %v", html, kept, err)
		}
		if _, still := f.rows["channel:"+ch]; still {
			t.Fatalf("%q left the draft kept", html)
		}
	}
	// A picture, a file or a reply alone is something.
	for _, d := range []Draft{
		{HTML: `<p><img src="x.png"></p>`},
		{Files: []File{{UUID: root, FileName: "plan.pdf", Type: "document"}}},
		{ReplyTo: &ReplyTo{UUID: root}},
	} {
		if _, err := Put(ctx, me(), "channel:"+ch, d); err != nil {
			t.Fatal(err)
		}
		if _, kept := f.rows["channel:"+ch]; !kept {
			t.Fatalf("%+v emptied the place", d)
		}
	}
}

func TestADraftsSizeIsRefusedInWords(t *testing.T) {
	f := withFakes(t)
	ctx := context.Background()
	f.readable[conversationBusiness.Ref{Kind: "channel", ID: ch}] = true
	_, err := Put(ctx, me(), "channel:"+ch, Draft{HTML: "<p>" + strings.Repeat("a", MaxHTML) + "</p>"})
	refusal(t, err, http.StatusRequestEntityTooLarge, "That draft is too long to keep. Shorten it, or send it in parts.")
	files := make([]File, MaxFiles+1)
	for i := range files {
		files[i] = File{UUID: root, FileName: "a.png", Type: "image"}
	}
	_, err = Put(ctx, me(), "channel:"+ch, Draft{HTML: "<p>a</p>", Files: files})
	refusal(t, err, http.StatusBadRequest, "A draft can hold up to 20 files.")
	_, err = Put(ctx, me(), "channel:"+ch, Draft{HTML: "<p>a</p>", Files: []File{{UUID: root}}})
	refusal(t, err, http.StatusBadRequest, "One of that draft's files can't be kept. Attach it again.")
	_, err = Put(ctx, me(), "nowhere", Draft{HTML: "<p>a</p>"})
	refusal(t, err, http.StatusBadRequest, "That isn't a place a draft can be kept.")
	if len(f.rows) != 0 || f.writes != 0 {
		t.Fatalf("a refused draft wrote: %d rows, %d writes", len(f.rows), f.writes)
	}
}

func TestDraftsAreCappedAndRateLimited(t *testing.T) {
	f := withFakes(t)
	ctx := context.Background()
	f.readable[conversationBusiness.Ref{Kind: "channel", ID: ch}] = true
	if _, err := Put(ctx, me(), "channel:"+ch, Draft{HTML: "<p>a</p>"}); err != nil {
		t.Fatal(err)
	}
	f.full = true
	// One already kept may still change; a new one is refused.
	if _, err := Put(ctx, me(), "channel:"+ch, Draft{HTML: "<p>b</p>"}); err != nil {
		t.Fatalf("changing a kept draft at the cap: %v", err)
	}
	f.readable[conversationBusiness.Ref{Kind: "group", ID: "abc"}] = true
	_, err := Put(ctx, me(), "group:abc", Draft{HTML: "<p>c</p>"})
	refusal(t, err, http.StatusConflict, "You have 500 drafts, the most that can be kept. Send or empty some first.")

	f.limit = f.writes
	_, err = Put(ctx, me(), "channel:"+ch, Draft{HTML: "<p>d</p>"})
	refusal(t, err, http.StatusTooManyRequests, "")
	refusal(t, Delete(ctx, me(), "channel:"+ch), http.StatusTooManyRequests, "")
	if f.rows["channel:"+ch].HTML != "<p>b</p>" {
		t.Fatalf("a limited write changed the draft: %+v", f.rows["channel:"+ch])
	}
}

func TestDraftsAreTheirWritersAlone(t *testing.T) {
	f := withFakes(t)
	ctx := context.Background()
	// Nobody signed in keeps or reads anything.
	if _, err := Put(ctx, &userModels.UserInfo{}, "channel:"+ch, Draft{HTML: "<p>a</p>"}); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Fatalf("no one's draft: %v", err)
	}
	if _, err := List(ctx, nil); !errors.Is(err, conversationBusiness.ErrNotFound) {
		t.Fatalf("no one's list: %v", err)
	}
	// Emptying needs no access: a conversation you left keeps your draft until you empty it.
	f.rows["dm:"+ana+" "+ben] = models.Draft{Key: "dm:" + ana + " " + ben, HTML: "<p>hi</p>"}
	if err := Delete(ctx, me(), "dm:"+ana+" "+ben); err != nil || len(f.rows) != 0 {
		t.Fatalf("emptying: %v, %d left", err, len(f.rows))
	}
	refusal(t, Delete(ctx, me(), "dm:nobody"), http.StatusBadRequest, "That isn't a place a draft can be kept.")
}

func TestTheListReadsBackWhatWasKept(t *testing.T) {
	f := withFakes(t)
	ctx := context.Background()
	f.rows["chat-thread:"+root] = models.Draft{Key: "chat-thread:" + root, HTML: "<p>later</p>",
		ReplyTo:   json.RawMessage(`{"uuid":"` + root + `","author_name":"Maya","text":"Lunch?"}`),
		Files:     json.RawMessage(`[{"attachment_uuid":"` + root + `","attachment_file_name":"plan.pdf","attachment_type":"document","attachment_size":10}]`),
		UpdatedAt: time.Date(2026, 10, 17, 9, 0, 0, 0, time.FixedZone("IST", 19800))}
	f.rows["channel:"+ch] = models.Draft{Key: "channel:" + ch, HTML: "<p>a</p>", ReplyTo: json.RawMessage(`{"nope":1}`)}
	got, err := List(ctx, me())
	if err != nil || got.Max != Max || len(got.Drafts) != 2 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, d := range got.Drafts {
		switch d.Key {
		case "chat-thread:" + root:
			if d.ReplyTo == nil || d.ReplyTo.Text != "Lunch?" || len(d.Files) != 1 || d.Files[0].FileName != "plan.pdf" || d.UpdatedAt.Location() != time.UTC {
				t.Fatalf("read back %+v", d)
			}
		case "channel:" + ch:
			if d.ReplyTo != nil {
				t.Fatalf("a reply with no message read back as %+v", d.ReplyTo)
			}
		}
	}
}
