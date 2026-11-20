package business

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"slices"
	"testing"
	"time"

	workAccess "github.com/akashc777/OneCamp/business/WorkAccess"
	mailModel "github.com/akashc777/OneCamp/models/postgres/TriageMail"
	userModels "github.com/akashc777/OneCamp/models/postgres/User"
	"github.com/emersion/go-imap/v2"
	"github.com/google/uuid"
)

// A server's SEARCH answer is a set of ranges, and one range is a few bytes
// however many UIDs it names: `* ESEARCH (TAG "x") UID ALL 1:4294967295` is
// about 40. The session walks the ranges it was sent and never expands one,
// so what After allocates stays small whatever the range says.
func TestMailSearchRangeIsNeverExpanded(t *testing.T) {
	const top = 4_000_000
	counted := liarSearching(t, 0, func(tag string) string { return fmt.Sprintf("* ESEARCH (TAG %q) UID ALL 1:%d", tag, top) })
	s, err := openIMAP(context.Background(), liarBox(TestReadBudget))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, _, err = s.After(context.Background(), 0, PerPoll)
	runtime.ReadMemStats(&after)
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 16<<20 {
		t.Fatalf("After allocated %d bytes for a %d-byte session whose SEARCH answered ALL 1:%d (err %v): want at most 16 MiB; expanding 1:4294967295 is over 16 GiB",
			alloc, counted.n.Load(), top, err)
	}
}

// lowestAbove keeps the lowest max UIDs above floor from the ranges a server
// sent, in any order and overlapping, and whether it left any.
func TestMailLowestAboveWalksRanges(t *testing.T) {
	r := func(a, b imap.UID) imap.UIDRange { return imap.UIDRange{Start: a, Stop: b} }
	cases := []struct {
		name  string
		set   imap.NumSet
		floor imap.UID
		max   int
		want  []imap.UID
		more  bool
	}{
		{"no answer", nil, 0, 3, nil, false},
		{"single UIDs, as a plain SEARCH sends them", imap.UIDSet{r(4, 4), r(9, 9), r(2, 2)}, 0, 3, []imap.UID{2, 4, 9}, false},
		{"one huge range", imap.UIDSet{r(1, 4_000_000)}, 10, 3, []imap.UID{11, 12, 13}, true},
		{"the top UID", imap.UIDSet{r(4294967290, 4294967295)}, 4294967293, 5, []imap.UID{4294967294, 4294967295}, false},
		{"out of order and overlapping", imap.UIDSet{r(50, 60), r(5, 7), r(6, 8), r(3, 3)}, 4, 4, []imap.UID{5, 6, 7, 8}, true},
		{"overlaps that fit", imap.UIDSet{r(5, 7), r(6, 8), r(7, 7)}, 0, 4, []imap.UID{5, 6, 7, 8}, false},
		{"the newest message below the mark", imap.UIDSet{r(3, 3)}, 10, 3, nil, false},
		{"a range given high to low", imap.UIDSet{r(9, 7)}, 0, 5, []imap.UID{7, 8, 9}, false},
		{"none wanted, some there", imap.UIDSet{r(11, 11)}, 10, 0, nil, true},
		{"none wanted, none there", imap.UIDSet{r(1, 10)}, 10, 0, nil, false},
	}
	for _, c := range cases {
		got, more, err := lowestAbove(c.set, c.floor, c.max)
		if err != nil || !slices.Equal(got, c.want) || more != c.more {
			t.Errorf("%s: got %v, more %v, err %v: want %v, more %v", c.name, got, more, err, c.want, c.more)
		}
	}
}

// A `*` in a SEARCH answer has no end to walk to: it is an error, never
// expanded (go-imap refuses it in ESEARCH today; this is the belt).
func TestMailLowestAboveRefusesAStar(t *testing.T) {
	for _, set := range []imap.UIDSet{{{Start: 5, Stop: 0}}, {{Start: 1, Stop: 2}, {Start: 0, Stop: 9}}} {
		if got, _, err := lowestAbove(set, 0, PerPoll); err == nil {
			t.Errorf("%v answered %v with no error: want a `*` refused", set, got)
		}
	}
}

// lowestAbove reads at most searchRanges ranges: a server sends its answer
// low to high, so the lowest UIDs are in the first ranges, and what is past
// the cap waits for another poll (more). A UID past the cap is never seen.
func TestMailLowestAboveReadsAtMostSearchRanges(t *testing.T) {
	var set imap.UIDSet
	for i := range searchRanges {
		u := imap.UID(1000 + 2*i)
		set = append(set, imap.UIDRange{Start: u, Stop: u})
	}
	set = append(set, imap.UIDRange{Start: 1, Stop: 1})
	got, more, err := lowestAbove(set, 0, 2)
	if err != nil || !slices.Equal(got, []imap.UID{1000, 1002}) || !more {
		t.Fatalf("a %d-range answer whose UID 1 is past the cap gave %v, more %v, err %v: want [1000 1002], more", len(set), got, more, err)
	}
}

// The poll opens its mailbox with the poll's read budget and the policy its
// saver earned (private addresses only for a workspace admin's save).
func TestMailPollOpensWithThePollBudgetAndTheSaversPolicy(t *testing.T) {
	for _, privateOK := range []bool{false, true} {
		w := newPoll(t)
		mb := box(7, 10)
		mb.PrivateOK = privateOK
		if err := pollOne(context.Background(), mb); err != nil {
			t.Fatal(err)
		}
		b := w.box
		if b == nil || b.Host != mb.Host || b.Port != mb.Port || b.TLS != mb.TLSMode || b.ReadBudget != PollReadBudget || b.PrivateOK != privateOK {
			if b == nil {
				b = &Box{}
			}
			t.Fatalf("saved with PrivateOK %v, the poll opened %s:%d (%s), ReadBudget %d, PrivateOK %v: want %s:%d (%s), PollReadBudget (%d), PrivateOK %v",
				privateOK, b.Host, b.Port, b.TLS, b.ReadBudget, b.PrivateOK, mb.Host, mb.Port, mb.TLSMode, PollReadBudget, privateOK)
		}
	}
}

// The real dial refuses this server's own network before any connect: a
// loopback or private address unless a workspace admin saved it, and
// link-local (a cloud's metadata address) for everyone. Loopback goes first
// and the first wrong answer stops the test, so a dial that lost its policy
// fails on 127.0.0.1 and never knocks on another address.
func TestMailRealDialRefusesInsideAddresses(t *testing.T) {
	for _, c := range []struct {
		host      string
		privateOK bool
	}{{"127.0.0.1", false}, {"10.0.0.5", false}, {"169.254.169.254", false}, {"169.254.169.254", true}} {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		s, err := openIMAP(ctx, Box{Host: c.host, Port: 993, TLS: "tls", User: "help@example.com", Password: "pw-example", Folder: "INBOX", PrivateOK: c.privateOK})
		cancel()
		if s != nil {
			s.Close()
		}
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("%s with PrivateOK %v: %v, want ErrRefused before any connect", c.host, c.privateOK, err)
		}
	}
}

// A project admin who is not a workspace admin can't have Test dial a
// private address: the real dial refuses it.
func TestMailTestByANonWorkspaceAdminRefusesAPrivateAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	in := Settings{Host: "10.0.0.5", Port: 993, TLS: "tls", Username: "help@example.com", Password: "pw-example", Folder: "INBOX"}
	err := Test(ctx, &userModels.UserInfo{}, workAccess.Standing{Role: workAccess.RoleAdmin}, uuid.New(), in)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("a project admin's Test of 10.0.0.5: %v, want ErrRefused", err)
	}
}

// A poll the pollTimeout ended still records its failure, on a write of its
// own (the poll's ctx is done): it backs off, and after its count pauses
// "unreachable", so a server that never finishes can't hold a slot forever.
func TestMailTimedOutPollRecordsItsFailure(t *testing.T) {
	for _, c := range []struct {
		failures int
		want     string
	}{{0, ""}, {unreachable - 1, PausedUnreachable}} {
		w := newPoll(t)
		openBox = func(ctx context.Context, _ Box) (Session, error) {
			<-ctx.Done() // a tarpit: the poll's own deadline ends it
			return nil, errors.New("use of closed network connection")
		}
		written, own := errors.New("never written"), false
		setState = func(ctx context.Context, _ uuid.UUID, s mailModel.State) error {
			written = ctx.Err()
			_, own = ctx.Deadline()
			if written != nil {
				return written
			}
			w.state = &s
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		mb := box(7, 10)
		mb.Failures = c.failures
		err := pollOne(ctx, mb)
		cancel()
		if written != nil || !own || w.state == nil || w.state.Failures != c.failures+1 || w.state.PausedReason != c.want {
			t.Fatalf("after %d failures a timed-out poll (%v) wrote on a ctx that was %v (deadline %v), state %+v: want failure %d recorded, paused %q",
				c.failures, err, written, own, w.state, c.failures+1, c.want)
		}
	}
}

// After a Save the poller dials the name. A name that points inside this
// server's network and one that doesn't resolve leave the same state, as a
// Test answers both the same, so the GET can't tell which names resolve inside.
func TestMailPollStateIsTheSameForAnInsideNameAndAMissingOne(t *testing.T) {
	n0 := now
	t.Cleanup(func() { now = n0 })
	at := time.Date(2026, 11, 18, 4, 30, 0, 0, time.UTC)
	now = func() time.Time { return at }
	var states []mailModel.State
	for _, cause := range []error{ErrRefused, &net.DNSError{Err: "no such host", Name: "imap.example.com", IsNotFound: true}} {
		w := newPoll(t)
		w.openErr = cause
		_ = pollOne(context.Background(), box(7, 10))
		if w.state == nil {
			t.Fatalf("%v: no state written", cause)
		}
		st := *w.state
		st.Polled = time.Time{}
		states = append(states, st)
	}
	if states[0] != states[1] {
		t.Fatalf("an inside name left %+v, a missing one %+v: want the same", states[0], states[1])
	}
}
