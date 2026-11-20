package business

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	taskWriter "github.com/akashc777/OneCamp/business/TaskWriter"
	triageBusiness "github.com/akashc777/OneCamp/business/Triage"
	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	userDomain "github.com/akashc777/OneCamp/domain/User"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/safefetch"
	mailModel "github.com/akashc777/OneCamp/models/postgres/TriageMail"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/google/uuid"
)

// A task the door makes from mail is filed as written outside through the
// mail door (helpers.SubmissionDoor "mail"), as a public
// form's is: an agent or rule it wakes runs without the connecting admin's
// reach on a stranger's words (business/AIAgent writtenOutside).
func TestMailTasksAreFiledAsWrittenOutside(t *testing.T) {
	w := newPoll(t)
	w.sess.msgs = two()
	var made, held []bool
	makeTask = func(ctx context.Context, _ *userModels.UserInfo, _ uuid.UUID, in taskWriter.NewTask) (uuid.UUID, error) {
		made = append(made, helpers.SubmissionDoor(ctx) == "mail")
		w.made = append(w.made, in)
		return uuid.New(), nil
	}
	hold := holdAll
	holdAll = func(ctx context.Context, p uuid.UUID, src triageBusiness.Source, in []triageBusiness.Arrival) (int, error) {
		held = append(held, helpers.IsPublicSubmission(ctx))
		return hold(ctx, p, src, in)
	}
	if err := pollOne(context.Background(), box(7, 10)); err != nil {
		t.Fatal(err)
	}
	if len(made) != 2 || !made[0] || !made[1] || len(held) != 1 || !held[0] {
		t.Fatalf("filed as written outside: tasks %v, hold %v", made, held)
	}
}

// The kept password goes only to the server it was kept for: an empty
// password with any other host, TLS mode and port, or user is a new mailbox
// (a port alone cannot change: checkBox takes 993 TLS or 143 STARTTLS).
func TestKeptPasswordOnlyForItsOwnServer(t *testing.T) {
	t.Setenv("APP_SECRET_KEK", "door-fix-test-only")
	sealed, err := helpers.EncryptSecret("the-mailbox-password")
	if err != nil {
		t.Fatal(err)
	}
	have := &mailModel.Mailbox{ProjectUUID: uuid.New(), Host: "imap.example.com", Port: 993, TLSMode: "tls", Username: "help@example.com", SecretSealed: sealed, Folder: "INBOX"}
	same := Settings{Host: "imap.example.com", Port: 993, TLS: "tls", Username: "help@example.com", Folder: "Support"}
	cases := []struct {
		name string
		in   func(Settings) Settings
		keep bool
	}{
		{"the same server, another folder", func(s Settings) Settings { return s }, true},
		{"another host", func(s Settings) Settings { s.Host = "imap.example.org"; return s }, false},
		{"another TLS mode", func(s Settings) Settings { s.TLS = "starttls"; s.Port = 143; return s }, false},
		{"another user", func(s Settings) Settings { s.Username = "other@example.com"; return s }, false},
	}
	admin := workAccess.Standing{Role: workAccess.RoleAdmin}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			saved := setupWorld(t, have)
			var dialled []Box
			testBox = func(_ context.Context, b Box) error { dialled = append(dialled, b); return nil }
			in := c.in(same)
			terr := Test(context.Background(), &userModels.UserInfo{}, admin, have.ProjectUUID, in)
			_, serr := Save(context.Background(), &userModels.UserInfo{}, admin, have.ProjectUUID, in)
			if c.keep {
				if terr != nil || serr != nil || len(dialled) != 1 || dialled[0].Password != "the-mailbox-password" || len(*saved) != 1 {
					t.Fatalf("test %v, save %v, dialled %d, saved %d", terr, serr, len(dialled), len(*saved))
				}
				if dialled[0].ReadBudget != 0 {
					t.Fatalf("a Test opened with ReadBudget %d: want 0 (TestReadBudget, %d bytes)", dialled[0].ReadBudget, TestReadBudget)
				}
				return
			}
			if !errors.Is(terr, ErrNoPassword) || !errors.Is(serr, ErrNoPassword) || len(dialled) != 0 || len(*saved) != 0 {
				t.Fatalf("test %v, save %v: dialled %d, saved %d", terr, serr, len(dialled), len(*saved))
			}
		})
	}
}

// A message whose task isn't made (a store hiccup) is read again: the mark
// stops below it, so nothing above it is passed over.
func TestMailMarkStopsBelowAnUnmadeMessage(t *testing.T) {
	w := newPoll(t)
	w.sess.msgs = append(two(), Fetched{UID: 13, Raw: raw("<m3@x>", "Out <o3@side.example>", "third", "text/plain", "c")})
	n := 0
	makeTask = func(_ context.Context, _ *userModels.UserInfo, _ uuid.UUID, in taskWriter.NewTask) (uuid.UUID, error) {
		if n++; n == 2 {
			return uuid.Nil, errors.New("a store hiccup")
		}
		return uuid.New(), nil
	}
	if err := pollOne(context.Background(), box(7, 10)); err != nil {
		t.Fatal(err)
	}
	if w.done == nil || w.done.LastUID != 11 || len(w.done.Tasks) != 1 || len(w.done.Release) != 2 {
		t.Fatalf("mark %d, tasks %d, given back %d: want the mark at 11, one task, two keys given back", w.done.LastUID, len(w.done.Tasks), len(w.done.Release))
	}
}

// A renumbered folder with nothing in the last day keeps a mark at its top,
// so the next poll doesn't read the folder from UID 1.
func TestMailRenumberingWithNothingRecentKeepsAMark(t *testing.T) {
	w := newPoll(t)
	w.sess.validity, w.sess.next = 8, 500
	if err := pollOne(context.Background(), box(7, 400)); err != nil {
		t.Fatal(err)
	}
	if w.done == nil || w.done.LastUID != 499 {
		t.Fatalf("mark %d: want 499", w.done.LastUID)
	}
}

// The files stored for a task that couldn't be made are taken back, in one
// request: the next poll stores them again with the task.
func TestMailFailedTaskTakesItsFilesBack(t *testing.T) {
	// UID 11 (made) carries one file, UID 12 (not made) two: only UID 12's
	// are taken back, in one request; UID 11's stay with its task.
	w := newPoll(t)
	w.sess.msgs = filedPair()
	n := 0
	makeTask = func(_ context.Context, _ *userModels.UserInfo, _ uuid.UUID, in taskWriter.NewTask) (uuid.UUID, error) {
		if n++; n == 2 {
			return uuid.Nil, errors.New("a store hiccup")
		}
		return uuid.New(), nil
	}
	if err := pollOne(context.Background(), box(7, 10)); err != nil {
		t.Fatal(err)
	}
	if len(w.stored) != 3 || w.calls["remove"] != 1 || !slices.Equal(w.removed, w.stored[1:]) {
		t.Fatalf("stored %v, removed %v in %d requests: want UID 12's two files taken back in one, UID 11's %v kept", w.stored, w.removed, w.calls["remove"], w.stored[:1])
	}
	if w.done == nil || w.done.LastUID != 11 || len(w.done.Tasks) != 1 {
		t.Fatalf("recorded %v: want the first task and the mark at 11", w.done)
	}
}

// A poll whose tasks are all made takes no file back.
func TestMailMadeTasksKeepTheirFiles(t *testing.T) {
	w := newPoll(t)
	w.sess.msgs = filedPair()
	if err := pollOne(context.Background(), box(7, 10)); err != nil {
		t.Fatal(err)
	}
	if len(w.stored) != 3 || w.calls["remove"] != 0 || w.done == nil || len(w.done.Tasks) != 2 {
		t.Fatalf("stored %v, removed %v in %d requests, done %v: want 3 files kept, nothing removed, 2 tasks", w.stored, w.removed, w.calls["remove"], w.done)
	}
}

// A task that fails because the poll's own ctx ended still gives its files
// back: dropFiles runs on its own deadline (removeFiles fails on an ended ctx).
func TestMailFilesComeBackWhenThePollRunsOutOfTime(t *testing.T) {
	w := newPoll(t)
	w.sess.msgs = filedPair()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	n := 0
	makeTask = func(ctx context.Context, _ *userModels.UserInfo, _ uuid.UUID, in taskWriter.NewTask) (uuid.UUID, error) {
		if n++; n == 2 {
			cancel()
			return uuid.Nil, ctx.Err()
		}
		return uuid.New(), nil
	}
	_ = pollOne(ctx, box(7, 10))
	if w.calls["remove"] != 1 || len(w.stored) != 3 || !slices.Equal(w.removed, w.stored[1:]) {
		t.Fatalf("stored %v, removed %v in %d requests: want UID 12's two files taken back after the poll's ctx ended", w.stored, w.removed, w.calls["remove"])
	}
}

// filedPair is two messages with files: UID 11 one PDF, UID 12 two.
func filedPair() []Fetched {
	pdf := "--B\r\nContent-Type: application/pdf\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nJVBERi0=\r\n"
	one := "--B\r\nContent-Type: text/plain\r\n\r\nsee file\r\n" + pdf + "--B--\r\n"
	both := "--B\r\nContent-Type: text/plain\r\n\r\nsee files\r\n" + pdf + strings.Replace(pdf, "a.pdf", "b.pdf", 1) + "--B--\r\n"
	mixed := `multipart/mixed; boundary="B"`
	return []Fetched{
		{UID: 11, Raw: raw("<e@x>", "Out <o@side.example>", "a file", mixed, one)},
		{UID: 12, Raw: raw("<f@x>", "Out <o@side.example>", "files", mixed, both)},
	}
}

// Only a connecting admin who is gone (userDomain.ErrNoActor) pauses the
// mailbox as owner_left; any other error reading them is a hiccup, backed off
// and tried again with no pause.
func TestMailOwnerGonePausesAndAHiccupRetries(t *testing.T) {
	for _, c := range []struct {
		err      error
		pause    string
		failures int
	}{{userDomain.ErrNoActor, PausedOwnerLeft, 0}, {errors.New("a store hiccup"), "", 1}} {
		w := newPoll(t)
		w.sess.msgs = two()
		ownerOf = func(context.Context, uuid.UUID) (*userModels.UserInfo, error) { return nil, c.err }
		_ = pollOne(context.Background(), box(7, 10))
		if w.state == nil || w.state.PausedReason != c.pause || w.state.Failures != c.failures || len(w.made) != 0 || w.done != nil {
			t.Errorf("owner err %v: state %+v, made %d, done %v: want paused %q with %d failures and nothing made", c.err, w.state, len(w.made), w.done, c.pause, c.failures)
		}
	}
}

// A session that reads past its budget (a server sending more than it
// declared) ends the poll with no pause: backed off and tried again.
func TestMailOverBudgetPollIsTriedAgainNotPaused(t *testing.T) {
	w := newPoll(t)
	w.openErr = fmt.Errorf("read: %w", safefetch.ErrReadBudget)
	if err := pollOne(context.Background(), box(7, 10)); !errors.Is(err, safefetch.ErrReadBudget) {
		t.Fatalf("poll err %v: want the budget's", err)
	}
	if w.state == nil || w.state.PausedReason != "" || w.state.Failures != 1 || !w.state.NextPoll.After(time.Now()) {
		t.Fatalf("state %+v: want no pause, one failure, due later", w.state)
	}
}

// A server that declares a small message and sends a body past MaxMessage
// (CR-409's belt): taken as too large, its body never parsed.
func TestMailBodyPastMaxMessageIsTooLargeWhateverItsDeclaredSize(t *testing.T) {
	big := raw("<lie@x>", "Out <o@side.example>", "lie", "text/plain", strings.Repeat("a", MaxMessage))
	m := Parse(Fetched{UID: 9, Size: 100, Raw: big, Head: Head{MessageID: "<lie@x>", From: "o@side.example", Subject: "lie"}})
	if !m.TooLarge || len(m.Files) != 0 || m.Subject != "lie" {
		t.Fatalf("a %d-byte body declared as 100: too large %v, files %d, subject %q: want it taken as too large", len(big), m.TooLarge, len(m.Files), m.Subject)
	}
}

// A poll whose budget runs out after its first task still records it (held,
// key finished, mark below the rest): otherwise a later poll makes it again.
func TestMailPollOutOfTimeRecordsWhatItMade(t *testing.T) {
	w := newPoll(t)
	w.sess.msgs = two()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	makeTask = func(ctx context.Context, _ *userModels.UserInfo, _ uuid.UUID, _ taskWriter.NewTask) (uuid.UUID, error) {
		if err := ctx.Err(); err != nil {
			return uuid.Nil, err
		}
		cancel() // the poll's budget ends after this task
		return uuid.New(), nil
	}
	err := pollOne(ctx, box(7, 10))
	if w.done == nil || len(w.done.Tasks) != 1 || len(w.done.Keys) != 1 || len(w.held) != 1 || w.done.LastUID != 11 {
		t.Fatalf("recorded %v, held %d (poll err %v): want the one task held, its key finished and the mark at 11", w.done, len(w.held), err)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// A key another poll's live claim holds is neither fresh nor made: the mark
// stops below it, the fresh ones around it are made, and the next poll takes
// it once it is fresh.
func TestMailMarkStopsBelowAKeyAnotherPollHolds(t *testing.T) {
	w := newPoll(t)
	w.sess.msgs = append(two(), Fetched{UID: 13, Raw: raw("<m3@x>", "Out <o3@side.example>", "third", "text/plain", "c")})
	third := string(Parse(w.sess.msgs[2]).Key)
	w.live = map[string]bool{string(Parse(w.sess.msgs[1]).Key): true}
	// The first poll claims all but the held key; the next, all but the
	// third (the first made it).
	w.fresh = func(keys [][]byte) [][]byte {
		var out [][]byte
		for _, k := range keys {
			if !w.live[string(k)] && (w.live != nil || string(k) != third) {
				out = append(out, k)
			}
		}
		return out
	}
	if err := pollOne(context.Background(), box(7, 10)); err != nil {
		t.Fatal(err)
	}
	if w.done == nil || w.done.LastUID != 11 || len(w.done.Tasks) != 2 {
		t.Fatalf("mark %d, tasks %d: want the mark at 11 below the held key and the two fresh made", w.done.LastUID, len(w.done.Tasks))
	}
	// The other poll let go: the next reads 12 and 13 again, makes 12, and
	// passes 13 (made by this poll).
	w.live = nil
	if err := pollOne(context.Background(), box(7, w.done.LastUID)); err != nil {
		t.Fatal(err)
	}
	if w.done.LastUID != 13 || len(w.done.Tasks) != 1 || len(w.made) != 3 || w.made[2].Name != "from outside" {
		t.Fatalf("the next poll: mark %d, tasks %d, made %d: want 12 made once and the mark at 13", w.done.LastUID, len(w.done.Tasks), len(w.made))
	}
}

// A renumbering whose newest hits pass the poll's byte budget marks the last
// message read, never the folder's top: the cut ones are read by the next poll.
func TestMailRenumberingCutByBytesMarksTheLastRead(t *testing.T) {
	w := newPoll(t)
	w.sess.validity, w.sess.next = 8, 600
	for i := range 25 {
		id := "<r" + strconv.Itoa(i) + "@x>"
		w.sess.msgs = append(w.sess.msgs, Fetched{UID: uint32(101 + i), Size: 3 << 20, Raw: raw(id, "Out <o@side.example>", "renumbered", "text/plain", "b")})
	}
	if err := pollOne(context.Background(), box(7, 400)); err != nil {
		t.Fatal(err)
	}
	if w.done == nil {
		t.Fatal("the poll saved nothing")
	}
	if len(w.done.Tasks) != 16 || w.done.LastUID != 116 {
		t.Fatalf("tasks %d, mark %d: want the 16 the 50 MB budget reads and the mark at 116, never 599", len(w.done.Tasks), w.done.LastUID)
	}
	// The next poll reads on from the mark: the nine cut are filed.
	if err := pollOne(context.Background(), box(8, w.done.LastUID)); err != nil {
		t.Fatal(err)
	}
	if len(w.done.Tasks) != 9 || w.done.LastUID != 125 {
		t.Fatalf("the next poll made %d tasks, mark %d: want the 9 cut and the mark at 125", len(w.done.Tasks), w.done.LastUID)
	}
}

// A renumbering whose hits all vanish before they are read moves nothing:
// the next poll searches again rather than read the folder from UID 1.
func TestMailRenumberingWithHitsGoneSearchesAgain(t *testing.T) {
	w := newPoll(t)
	w.sess.validity, w.sess.next, w.sess.gone = 8, 600, true
	w.sess.msgs = two()
	if err := pollOne(context.Background(), box(7, 400)); err != nil {
		t.Fatal(err)
	}
	if w.done == nil || w.done.UIDValidity != 7 || w.done.LastUID != 400 {
		t.Fatalf("saved %+v: want validity 7 and the mark 400 kept", w.done)
	}
}

// One poll downloads at most PollBytes: what would pass it waits for the next
// poll; headers-only messages cost nothing; the first is always taken.
func TestMailPollDownloadsAtMostItsBytes(t *testing.T) {
	sizes := func(fs []Fetched) []int64 {
		var out []int64
		for _, f := range fs {
			out = append(out, f.Size)
		}
		return out
	}
	of := func(sz ...int64) []Fetched {
		var out []Fetched
		for i, s := range sz {
			out = append(out, Fetched{UID: uint32(11 + i), Size: s})
		}
		return out
	}
	const mb = 1 << 20
	for _, c := range []struct {
		in   []Fetched
		want int
	}{
		{of(9*mb, 9*mb, 9*mb, 9*mb, 9*mb, 9*mb, 9*mb), 5},
		{of(MaxMessage+1, MaxMessage+1, 9*mb, 9*mb), 4},
		{of(10*mb, 10*mb, 10*mb, 10*mb, 10*mb, 1), 4},
		{of(1), 1},
		{nil, 0},
	} {
		got, cut := withinBytes(c.in, 45*mb)
		if len(got) != c.want || cut != (c.want < len(c.in)) {
			t.Errorf("sizes %v under 45 MB took %v (cut %v), want the first %d", sizes(c.in), sizes(got), cut, c.want)
		}
	}
	if got, cut := withinBytes(of(9*mb, 1), 5*mb); len(got) != 1 || !cut {
		t.Errorf("a first message over the budget alone was not taken: %d (cut %v)", len(got), cut)
	}
}

// A mailbox's lease outlasts the slowest pass that could hold it: batch
// mailboxes, together at once, pollTimeout and two recordTimeouts each.
func TestMailLeaseOutlastsThePass(t *testing.T) {
	pass := time.Duration((batch+together-1)/together) * (pollTimeout + 2*recordTimeout)
	if lease <= pass {
		t.Fatalf("lease %v, a pass can take %v", lease, pass)
	}
}
