package business

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"github.com/emersion/go-imap/v2/imapserver"
	"github.com/emersion/go-imap/v2/imapserver/imapmemserver"
)

// envelopeCounter counts the ENVELOPE items the server sends, across reads
// that split the word.
type envelopeCounter struct {
	net.Conn
	mu   sync.Mutex
	tail []byte
	n    int
}

var envelopeWord = []byte("ENVELOPE (")

func (c *envelopeCounter) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	buf := append(c.tail, p[:n]...)
	c.n += bytes.Count(buf, envelopeWord)
	// Keep only what could start a word split by the next read.
	keep := min(len(buf), len(envelopeWord)-1)
	c.tail = append([]byte(nil), buf[len(buf)-keep:]...)
	c.mu.Unlock()
	return n, err
}

func (c *envelopeCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// memMailbox serves an in-memory INBOX of n messages (UIDs 1..n) and returns a
// session on it whose connection counts the envelopes it reads.
func memMailbox(t *testing.T, n int) (*imapSession, *envelopeCounter) {
	t.Helper()
	return memMailboxOf(t, n, "hello")
}

// memMailboxOf is memMailbox with n messages of body.
func memMailboxOf(t *testing.T, n int, body string) (*imapSession, *envelopeCounter) {
	t.Helper()
	user := imapmemserver.NewUser("door@example.com", "pw")
	if err := user.Create("INBOX", nil); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		msg := raw(fmt.Sprintf("<m%d@example.com>", i), "someone@example.org", fmt.Sprintf("note %d", i), "text/plain", body)
		if _, err := user.Append("INBOX", bytes.NewReader(msg), &imap.AppendOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	srv := imapserver.New(&imapserver.Options{
		NewSession: func(*imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return imapmemserver.NewUserSession(user), nil, nil
		},
		Caps:         imap.CapSet{imap.CapIMAP4rev1: {}},
		InsecureAuth: true,
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	counted := &envelopeCounter{Conn: conn}
	c := imapclient.New(counted, nil)
	if err := c.Login("door@example.com", "pw").Wait(); err != nil {
		t.Fatal(err)
	}
	data, err := c.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		t.Fatal(err)
	}
	s := &imapSession{c: c, conn: counted, validity: data.UIDValidity, next: uint32(data.UIDNext)}
	t.Cleanup(s.Close)
	return s, counted
}

// A backlog of thousands above the mark costs one search and the envelopes
// of at most PerPoll, oldest first, and the next poll carries on from there.
func TestMailBacklogReadsAtMostPerPoll(t *testing.T) {
	s, counted := memMailbox(t, 5000)
	ctx := context.Background()
	got, more, err := s.After(ctx, 10, PerPoll)
	if err != nil || !more {
		t.Fatalf("err %v, more %v: want the backlog left for the next poll", err, more)
	}
	if e := counted.count(); e > PerPoll {
		t.Fatalf("read %d envelopes for a 5000-message backlog, want <= %d", e, PerPoll)
	}
	if len(got) != PerPoll || got[0].UID != 11 || got[len(got)-1].UID != 10+PerPoll {
		t.Fatalf("kept %d, want UIDs 11..%d oldest first", len(got), 10+PerPoll)
	}
	for i, f := range got {
		if f.UID != uint32(11+i) || len(f.Raw) == 0 || f.Head.Subject != fmt.Sprintf("note %d", 11+i) {
			t.Fatalf("message %d is UID %d %q (raw %d bytes)", i, f.UID, f.Head.Subject, len(f.Raw))
		}
	}
	next, _, err := s.After(ctx, got[len(got)-1].UID, PerPoll)
	if err != nil {
		t.Fatal(err)
	}
	if len(next) != PerPoll || next[0].UID != 11+PerPoll {
		t.Fatalf("the next poll starts at %v, want UID %d", next, 11+PerPoll)
	}
	if e := counted.count(); e > 2*PerPoll {
		t.Fatalf("two polls read %d envelopes, want <= %d", e, 2*PerPoll)
	}
}

// A renumbering whose window holds more than PollBytes, on a real session
// (CR-400's probe): Since reads five 9 MB messages and the mark stops at the
// last read, so After reads the sixth next poll.
func TestMailRenumberingOverPollBytesReadsTheRestNextPoll(t *testing.T) {
	s, _ := memMailboxOf(t, 6, strings.Repeat("x", 9<<20))
	w := newPoll(t)
	openBox = func(context.Context, Box) (Session, error) { return s, nil }
	if err := pollOne(context.Background(), box(int64(s.validity)+1, 3)); err != nil {
		t.Fatal(err)
	}
	if w.done == nil || w.done.LastUID != 5 || len(w.done.Tasks) != 5 {
		t.Fatalf("saved %v: want 5 tasks and the mark at 5, UID 6 left for the next poll", w.done)
	}
}

// "n:*" answers with the newest message even when it is below n: a mark at
// the top reads nothing.
func TestMailNothingAboveTheMark(t *testing.T) {
	s, counted := memMailbox(t, 3)
	got, more, err := s.After(context.Background(), 3, PerPoll)
	if err != nil || more {
		t.Fatalf("err %v, more %v", err, more)
	}
	if len(got) != 0 || counted.count() != 0 {
		t.Fatalf("a mark at the newest UID read %d messages, %d envelopes", len(got), counted.count())
	}
}

// A session's connection closes when its context ends: a slow server holds no
// command past a poll's or a test's limit.
func TestMailSessionClosesWhenItsContextEnds(t *testing.T) {
	s, _ := memMailbox(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	s.closeOn(ctx)
	if _, _, err := s.After(context.Background(), 0, 5); err != nil {
		t.Fatalf("before the end: %v", err)
	}
	cancel()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, _, err := s.After(context.Background(), 0, 5); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the session still answers after its context ended")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
