package business

import (
	"context"
	"encoding/json"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	attachmentBusiness "github.com/akashc777/OneCamp/business/Attachment"
	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	"github.com/akashc777/OneCamp/helpers"
	mailModel "github.com/akashc777/OneCamp/models/postgres/TriageMail"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// raw is one RFC 5322 message: headers, then body as given.
func raw(id, from, subject, contentType, body string) []byte {
	h := "From: " + from + "\r\nSubject: " + subject + "\r\nDate: Tue, 17 Nov 2026 10:00:00 +0000\r\n"
	if id != "" {
		h += "Message-ID: " + id + "\r\n"
	}
	return []byte(h + "MIME-Version: 1.0\r\nContent-Type: " + contentType + "\r\n\r\n" + body)
}

func TestMailParseCapsSubjectBodyHeaders(t *testing.T) {
	m := Parse(Fetched{UID: 1, Raw: raw("<a@x>", "Ann <ann@x.example>", strings.Repeat("s", 300), "text/plain; charset=utf-8", strings.Repeat("b", MaxText+500))})
	if len([]rune(m.Subject)) != MaxSubject || len(m.Text) > MaxText || !m.Cut {
		t.Fatalf("subject %d runes, text %d bytes, cut %v", len([]rune(m.Subject)), len(m.Text), m.Cut)
	}
	if m.From != "ann@x.example" || m.FromName != "Ann" {
		t.Errorf("from %q %q", m.From, m.FromName)
	}
	huge := append([]byte("X-Pad: "+strings.Repeat("p", MaxHeader)+"\r\n"), raw("<b@x>", "b@x.example", "hi", "text/plain", "body")...)
	if m := Parse(Fetched{UID: 2, Raw: huge}); m.Text != "" || m.Subject == "hi" {
		t.Errorf("headers over the cap were read: %+v", m.Subject)
	}
}

func TestMailHTMLBodyBecomesText(t *testing.T) {
	body := `<html><body><script>alert(1)</script><p>Please <b>fix</b> the login</p><img src="http://tracker.example/p.gif"></body></html>`
	m := Parse(Fetched{UID: 1, Raw: raw("<h@x>", "h@x.example", "html", "text/html; charset=utf-8", body)})
	if strings.Contains(m.Text, "<") || strings.Contains(m.Text, "alert") || strings.Contains(m.Text, "tracker.example") {
		t.Fatalf("html survived: %q", m.Text)
	}
	if !strings.Contains(m.Text, "fix") {
		t.Errorf("the words were lost: %q", m.Text)
	}
}

func TestMailAttachmentCaps(t *testing.T) {
	var b strings.Builder
	for i := 0; i < MaxFiles+2; i++ {
		fmt.Fprintf(&b, "--B\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"f%d.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0=\r\n", i)
	}
	body := "--B\r\nContent-Type: text/plain\r\n\r\nsee files\r\n" + b.String() + "--B--\r\n"
	m := Parse(Fetched{UID: 1, Raw: raw("<f@x>", "f@x.example", "files", `multipart/mixed; boundary="B"`, body)})
	if len(m.Files) != MaxFiles || m.Dropped != 2 || m.Text != "see files" {
		t.Fatalf("files %d, dropped %d, text %q", len(m.Files), m.Dropped, m.Text)
	}
	if m.Files[0].Name != "f0.pdf" || string(m.Files[0].Data) != "%PDF-" {
		t.Errorf("file %q %q", m.Files[0].Name, m.Files[0].Data)
	}
}

func TestMailOversizeTakesHeadersOnly(t *testing.T) {
	m := Parse(Fetched{UID: 9, Size: MaxMessage + 1, Head: Head{MessageID: "<big@x>", From: "big@x.example", Subject: "huge"}})
	if !m.TooLarge || m.Subject != "huge" || len(m.Files) != 0 || m.From != "big@x.example" || len(m.Key) != 32 {
		t.Fatalf("%+v", m)
	}
}

func TestMailMessageKeyBothKeys(t *testing.T) {
	at := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	a, b := MessageKey("<ID@x>", "", at, "", nil), MessageKey(" <id@x> ", "other", at, "other", []byte("x"))
	if len(a) != 32 || string(a) != string(b) {
		t.Fatalf("one Message-ID, two keys: %x %x", a, b)
	}
	if long := MessageKey("<"+strings.Repeat("l", 4000)+"@x>", "", at, "", nil); len(long) != 32 {
		t.Errorf("a long Message-ID is %d bytes, want its 32-byte hash", len(long))
	}
	f1, f2 := MessageKey("", "a@x", at, "s", []byte("one")), MessageKey("", "a@x", at, "s", []byte("two"))
	if len(f1) != 32 || string(f1) == string(f2) || string(f1) == string(a) {
		t.Errorf("fallback keys: %x %x", f1, f2)
	}
	if string(f1) != string(MessageKey("", "A@X", at, "s", []byte("one"))) {
		t.Errorf("the fallback key depends on the address's case")
	}
}

// fakeSession is a mailbox: its validity, next UID and messages. Like the
// real session it reads at most PollBytes a poll (firstWithin).
type fakeSession struct {
	validity, next uint32
	msgs           []Fetched
	afterMax       int
	sinceCalls     int
	afterCalls     int
	gone           bool // the hits vanish between SEARCH and FETCH
}

func (f *fakeSession) Validity() uint32 { return f.validity }
func (f *fakeSession) Next() uint32     { return f.next }
func (f *fakeSession) After(_ context.Context, uid uint32, max int) ([]Fetched, bool, error) {
	f.afterCalls++
	f.afterMax = max
	var hits []Fetched
	for _, m := range f.msgs {
		if m.UID > uid {
			hits = append(hits, m)
		}
	}
	out, more := firstWithin(hits, max)
	return out, more, nil
}
func (f *fakeSession) Since(_ context.Context, _ time.Time, max int) ([]Fetched, bool, error) {
	f.sinceCalls++
	if f.gone {
		return nil, len(f.msgs) > 0, nil
	}
	out, more := firstWithin(f.msgs, max)
	return out, more, nil
}
func (f *fakeSession) Close() {}

// pollWorld fakes every store a poll calls and counts the calls.
type pollWorld struct {
	sess    *fakeSession
	openErr error
	room    int
	on      bool
	members map[string]uuid.UUID
	fresh   func(keys [][]byte) [][]byte
	live    map[string]bool // keys another poll's live claim holds: neither fresh nor made
	calls   map[string]int
	made    []taskWriter.NewTask
	held    []triageBusiness.Arrival
	done    *mailModel.Done
	state   *mailModel.State
	stored  []string // the keys storeFile put
	removed []string // the keys removeFiles took back
	box     *Box     // the Box the poll opened: its target, budget and policy
}

func newPoll(t *testing.T) *pollWorld {
	t.Helper()
	w := &pollWorld{sess: &fakeSession{validity: 7, next: 10}, room: 20, on: true, members: map[string]uuid.UUID{}, calls: map[string]int{}}
	w.fresh = func(keys [][]byte) [][]byte { return keys }
	o0, r0, c0, m0, w0, t0, s0, h0, f0, st0, d0 := openBox, room, claimKeys, membersByEmail, ownerOf, makeTask, storeFile, holdAll, finish, setState, decrypt
	t.Cleanup(func() {
		openBox, room, claimKeys, membersByEmail, ownerOf, makeTask, storeFile, holdAll, finish, setState, decrypt = o0, r0, c0, m0, w0, t0, s0, h0, f0, st0, d0
	})
	openBox = func(_ context.Context, b Box) (Session, error) {
		w.calls["open"]++
		w.box = &b
		if w.openErr != nil {
			return nil, w.openErr
		}
		return w.sess, nil
	}
	room = func(context.Context, uuid.UUID) (int, bool, error) { w.calls["room"]++; return w.room, w.on, nil }
	claimKeys = func(_ context.Context, _, _ uuid.UUID, keys [][]byte) ([][]byte, [][]byte, error) {
		w.calls["claim"]++
		once := map[string]bool{}
		for _, k := range keys {
			if once[string(k)] {
				t.Errorf("one claim names a key twice (Postgres refuses it)")
			}
			once[string(k)] = true
		}
		// A key this claim didn't take was made from by an earlier poll, or is
		// another poll's live claim (w.live).
		fresh := w.fresh(keys)
		took := map[string]bool{}
		for _, k := range fresh {
			took[string(k)] = true
		}
		var made [][]byte
		for _, k := range keys {
			if !took[string(k)] && !w.live[string(k)] {
				made = append(made, k)
			}
		}
		return fresh, made, nil
	}
	membersByEmail = func(_ context.Context, emails []string) (map[string]uuid.UUID, error) {
		w.calls["members"]++
		return w.members, nil
	}
	ownerOf = func(context.Context, uuid.UUID) (*userModels.UserInfo, error) {
		w.calls["owner"]++
		return &userModels.UserInfo{}, nil
	}
	makeTask = func(_ context.Context, _ *userModels.UserInfo, _ uuid.UUID, in taskWriter.NewTask) (uuid.UUID, error) {
		w.calls["task"]++
		w.made = append(w.made, in)
		return uuid.New(), nil
	}
	storeFile = func(context.Context, File, uuid.UUID) (attachmentBusiness.Stored, error) {
		w.calls["file"]++
		id := uuid.New()
		w.stored = append(w.stored, "k/"+id.String())
		return attachmentBusiness.Stored{ID: id, ObjKey: "k/" + id.String(), SafeName: "f.pdf"}, nil
	}
	rf0 := removeFiles
	t.Cleanup(func() { removeFiles = rf0 })
	removeFiles = func(ctx context.Context, keys []string) error {
		w.calls["remove"]++
		if _, ok := ctx.Deadline(); !ok {
			t.Errorf("files taken back on a ctx with no deadline of its own: a slow store would hold the poll's slot")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		w.removed = append(w.removed, keys...)
		return nil
	}
	// The hold and the finish fail on an ended ctx, as the store's do.
	holdAll = func(ctx context.Context, _ uuid.UUID, src triageBusiness.Source, in []triageBusiness.Arrival) (int, error) {
		w.calls["hold"]++
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if src != triageBusiness.FromMail {
			t.Errorf("held as %q", src)
		}
		w.held = append(w.held, in...)
		return len(in), nil
	}
	finish = func(ctx context.Context, d mailModel.Done) error {
		w.calls["finish"]++
		if d.Polled.IsZero() {
			t.Errorf("a finish without the row's token (Done.Polled): it would never apply")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		w.done = &d
		return nil
	}
	setState = func(_ context.Context, _ uuid.UUID, s mailModel.State) error {
		w.calls["state"]++
		if s.Polled.IsZero() {
			t.Errorf("a state without the row's token (State.Polled): it would never apply")
		}
		w.state = &s
		return nil
	}
	decrypt = func(string) (string, error) { return "pw", nil }
	return w
}

func box(validity, last int64) mailModel.Mailbox {
	return mailModel.Mailbox{ID: uuid.New(), ProjectUUID: uuid.New(), Host: "imap.example.com", Port: 993, TLSMode: "tls",
		Username: "help@example.com", SecretSealed: "sealed", Folder: "INBOX", UIDValidity: validity, LastUID: last, ConnectedBy: uuid.New(),
		UpdatedAt: time.Now()}
}

func two() []Fetched {
	return []Fetched{
		{UID: 11, Raw: raw("<m1@x>", "Member <Mem@Team.example>", "from a member", "text/plain", "hello")},
		{UID: 12, Raw: raw("<m2@x>", "Out <out@side.example>", "from outside", "text/plain", "@everyone hi <b>x</b>")},
	}
}

func TestMailSenderMemberElseOutsider(t *testing.T) {
	w := newPoll(t)
	w.sess.msgs = two()
	member := uuid.New()
	w.members["mem@team.example"] = member
	if err := pollOne(context.Background(), box(7, 10)); err != nil {
		t.Fatal(err)
	}
	if len(w.held) != 2 || w.held[0].From.UserUUID == nil || *w.held[0].From.UserUUID != member {
		t.Fatalf("held %+v", w.held)
	}
	if out := w.held[1].From; out.UserUUID != nil || out.Email != "out@side.example" || out.Name != "Out" {
		t.Errorf("outsider %+v", out)
	}
	if d := w.made[1].Description; strings.Contains(d, "<b>") || !strings.Contains(d, "&lt;b&gt;") {
		t.Errorf("the body was not escaped: %q", d)
	}
	if w.done == nil || w.done.LastUID != 12 || len(w.done.Tasks) != 2 {
		t.Errorf("done %+v", w.done)
	}
}

func TestMailPollStoreCallsBounded(t *testing.T) {
	w := newPoll(t)
	w.sess.msgs = two()
	if err := pollOne(context.Background(), box(7, 10)); err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"room": 1, "open": 1, "claim": 1, "members": 1, "owner": 1, "task": 2, "hold": 1, "finish": 1}
	for k, n := range want {
		if w.calls[k] != n {
			t.Errorf("%s called %d times, want %d (calls %v)", k, w.calls[k], n, w.calls)
		}
	}
}

func TestMailRepeatMessageIDDropped(t *testing.T) {
	w := newPoll(t)
	w.sess.msgs = append(two(), Fetched{UID: 13, Raw: raw("<m1@x>", "Member <Mem@Team.example>", "again", "text/plain", "hello")})
	w.fresh = func(keys [][]byte) [][]byte { return keys[1:] }
	if err := pollOne(context.Background(), box(7, 10)); err != nil {
		t.Fatal(err)
	}
	if w.calls["task"] != 1 || len(w.held) != 1 || w.done.LastUID != 13 {
		t.Fatalf("tasks %d, held %d, done %+v", w.calls["task"], len(w.held), w.done)
	}
}

func TestMailPollTakesAtMostRoom(t *testing.T) {
	w := newPoll(t)
	w.sess.msgs = two()
	w.room = 1
	if err := pollOne(context.Background(), box(7, 10)); err != nil {
		t.Fatal(err)
	}
	if w.sess.afterMax != 1 || w.calls["task"] != 1 || w.done.LastUID != 11 {
		t.Fatalf("max %d, tasks %d, last %d", w.sess.afterMax, w.calls["task"], w.done.LastUID)
	}
	w2 := newPoll(t)
	w2.room = 0
	if err := pollOne(context.Background(), box(7, 10)); err != nil || w2.calls["open"] != 0 {
		t.Errorf("a full triage still dialled: %v %v", err, w2.calls)
	}
}

func TestMailTriageOffPauses(t *testing.T) {
	w := newPoll(t)
	w.on = false
	_ = pollOne(context.Background(), box(7, 10))
	if w.calls["open"] != 0 || w.state == nil || w.state.PausedReason != PausedTriageOff {
		t.Fatalf("calls %v state %+v", w.calls, w.state)
	}
}

func TestMailAuthFailurePausesNoRetry(t *testing.T) {
	w := newPoll(t)
	w.openErr = fmt.Errorf("login: %w", ErrAuth)
	_ = pollOne(context.Background(), box(7, 10))
	if w.state == nil || w.state.PausedReason != PausedAuth || w.calls["task"] != 0 {
		t.Fatalf("state %+v calls %v", w.state, w.calls)
	}
	w2 := newPoll(t)
	w2.openErr = fmt.Errorf("dial: connection reset")
	b := box(7, 10)
	b.Failures = 2
	_ = pollOne(context.Background(), b)
	if w2.state == nil || w2.state.PausedReason != "" || w2.state.Failures != 3 || time.Until(w2.state.NextPoll) < 7*time.Minute {
		t.Fatalf("a network error: state %+v", w2.state)
	}
}

func TestMailFirstPollStartsAtNext(t *testing.T) {
	w := newPoll(t)
	w.sess.msgs = two()
	_ = pollOne(context.Background(), box(0, 0))
	if w.calls["task"] != 0 || w.sess.afterCalls != 0 || w.done == nil || w.done.UIDValidity != 7 || w.done.LastUID != 9 {
		t.Fatalf("first poll: calls %v done %+v", w.calls, w.done)
	}
}

func TestMailUIDValidityChangeSearchesSince(t *testing.T) {
	w := newPoll(t)
	w.sess.msgs = two()
	b := box(5, 400)
	at := time.Now().Add(-time.Hour)
	b.LastPolledAt = &at
	if err := pollOne(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	if w.sess.sinceCalls != 1 || w.sess.afterCalls != 0 || w.done.UIDValidity != 7 || w.done.LastUID != 12 {
		t.Fatalf("since %d after %d done %+v", w.sess.sinceCalls, w.sess.afterCalls, w.done)
	}
}

func TestMailDemoNeverDials(t *testing.T) {
	t.Setenv("DEMO_MODE", "true")
	s, err := openerFor()(context.Background(), Box{Host: "imap.example.com", Port: 993, TLS: "tls"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.(*stubSession); !ok {
		t.Fatalf("the demo opened %T", s)
	}
	msgs, _, _ := s.After(context.Background(), 0, 20)
	if len(msgs) < 3 {
		t.Fatalf("the stub delivered %d", len(msgs))
	}
	if string(Parse(msgs[0]).Key) != string(Parse(msgs[2]).Key) {
		t.Errorf("the stub's repeat has another Message-ID")
	}
}

func TestMailDialRefusesMetadataLinkLocal(t *testing.T) {
	for _, a := range []string{"169.254.169.254", "fe80::1", "224.0.0.1", "0.0.0.0", "100.100.100.200"} {
		if dialPolicy(true).Allows(netip.MustParseAddr(a)) {
			t.Errorf("%s allowed", a)
		}
	}
	if !dialPolicy(false).Allows(netip.MustParseAddr("93.184.216.34")) {
		t.Errorf("a public mail server refused")
	}
}

func TestMailPrivateNetworkOnlyWorkspaceAdmin(t *testing.T) {
	for _, a := range []string{"10.0.0.5", "127.0.0.1", "192.168.1.10", "100.64.0.9"} {
		if dialPolicy(false).Allows(netip.MustParseAddr(a)) {
			t.Errorf("%s allowed for a project admin", a)
		}
		if !dialPolicy(true).Allows(netip.MustParseAddr(a)) {
			t.Errorf("%s refused for a workspace admin", a)
		}
	}
}

func TestMailPlaintextAuthRefused(t *testing.T) {
	bad := []Box{{Host: "h", Port: 143, TLS: "none"}, {Host: "h", Port: 143, TLS: ""}, {Host: "h", Port: 25, TLS: "tls"}, {Host: "", Port: 993, TLS: "tls"}, {Host: "h", Port: 993, TLS: "starttls"}}
	for _, b := range bad {
		if checkBox(b) == nil {
			t.Errorf("%+v accepted", b)
		}
	}
	for _, b := range []Box{{Host: "h", Port: 993, TLS: "tls"}, {Host: "h", Port: 143, TLS: "starttls"}} {
		if err := checkBox(b); err != nil {
			t.Errorf("%+v refused: %v", b, err)
		}
	}
}

// setupWorld fakes the mailbox store for the routes' business.
func setupWorld(t *testing.T, have *mailModel.Mailbox) *[]mailModel.Mailbox {
	t.Helper()
	var saved []mailModel.Mailbox
	b0, s0, r0, se0, te0 := byProject, saveBox, removeBox, seal, testBox
	t.Cleanup(func() { byProject, saveBox, removeBox, seal, testBox = b0, s0, r0, se0, te0 })
	byProject = func(context.Context, uuid.UUID) (*mailModel.Mailbox, error) { return have, nil }
	saveBox = func(_ context.Context, m mailModel.Mailbox) error { saved = append(saved, m); return nil }
	removeBox = func(context.Context, uuid.UUID, uuid.UUID) (bool, error) { return true, nil }
	seal = func(s string) (string, error) { return "sealed:" + s, nil }
	testBox = func(context.Context, Box) error { return nil }
	return &saved
}

func TestMailSecretNeverInView(t *testing.T) {
	have := box(7, 10)
	have.SecretSealed = "sealed-secret-xyz"
	setupWorld(t, &have)
	v, err := Read(context.Background(), workAccess.Standing{Role: workAccess.RoleAdmin}, have.ProjectUUID)
	if err != nil || !v.Connected || v.ConnectedAs != "help@example.com" {
		t.Fatalf("%+v %v", v, err)
	}
	raw, _ := json.Marshal(v)
	if strings.Contains(string(raw), "sealed-secret-xyz") || strings.Contains(strings.ToLower(string(raw)), "password") {
		t.Errorf("the read carries the secret: %s", raw)
	}
}

func TestMailMemberAndTokenRefused(t *testing.T) {
	saved := setupWorld(t, nil)
	in := Settings{Host: "imap.example.com", Port: 993, TLS: "tls", Username: "u", Password: "p"}
	who := &userModels.UserInfo{}
	if _, err := Save(context.Background(), who, workAccess.Standing{Role: workAccess.RoleMember}, uuid.New(), in); err != ErrNotAdmin {
		t.Errorf("a member saved: %v", err)
	}
	if _, err := Read(context.Background(), workAccess.Standing{Role: workAccess.RoleMember}, uuid.New()); err != ErrNotAdmin {
		t.Errorf("a member read: %v", err)
	}
	tok := helpers.WithActsThroughToken(context.Background())
	if _, err := Save(tok, who, workAccess.Standing{Role: workAccess.RoleAdmin}, uuid.New(), in); err != ErrThroughToken {
		t.Errorf("a token saved: %v", err)
	}
	if err := Remove(tok, who, workAccess.Standing{Role: workAccess.RoleAdmin}, uuid.New()); err != ErrThroughToken {
		t.Errorf("a token removed: %v", err)
	}
	if len(*saved) != 0 {
		t.Errorf("saved %d", len(*saved))
	}
}

func TestMailSaveSealsAndPrivateOnlyForWorkspaceAdmin(t *testing.T) {
	saved := setupWorld(t, nil)
	in := Settings{Host: "imap.example.com", Port: 993, TLS: "tls", Username: "u", Password: "p"}
	pa := &userModels.UserInfo{}
	if _, err := Save(context.Background(), pa, workAccess.Standing{Role: workAccess.RoleAdmin}, uuid.New(), in); err != nil {
		t.Fatal(err)
	}
	wa := &userModels.UserInfo{}
	wa.UserPostgresInfo.IsAdmin = true
	if _, err := Save(context.Background(), wa, workAccess.Standing{Role: workAccess.RoleAdmin}, uuid.New(), in); err != nil {
		t.Fatal(err)
	}
	if len(*saved) != 2 || (*saved)[0].PrivateOK || !(*saved)[1].PrivateOK || (*saved)[0].SecretSealed != "sealed:p" {
		t.Fatalf("saved %+v", *saved)
	}
}
