package business

// CR-409 F1: the IMAP server is chosen by whoever saves the mailbox, and a
// server can say a message is 100 bytes (RFC822.SIZE) and then send an 80 MB
// literal for its BODY[]; go-imap reads a literal whole. Every byte from the
// server passes one budget, on the dialled conn before TLS (openIMAP,
// safefetch.LimitReads): a poll's session reads at most PollReadBudget and
// fails past it; a Test's reads at most TestReadBudget.

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type readCounter struct {
	net.Conn
	n atomic.Int64
}

func (c *readCounter) Read(p []byte) (int, error) {
	k, err := c.Conn.Read(p)
	c.n.Add(int64(k))
	return k, err
}

// liar is a scripted IMAP server over TLS (httptest's certificate, for
// example.com) that openIMAP dials in place of the box's host: its LOGIN
// answer carries pad bytes of text, and its one message declares 100 bytes
// and sends an 80 MB BODY[]. It counts what the session read from it.
func liar(t *testing.T, pad int) *readCounter {
	t.Helper()
	return liarSearching(t, pad, func(string) string { return "* SEARCH 5" })
}

// liarSearching is liar with its SEARCH answered by search(tag).
func liarSearching(t *testing.T, pad int, search func(tag string) string) *readCounter {
	t.Helper()
	const literal = 80 << 20
	hs := httptest.NewTLSServer(http.NotFoundHandler())
	cert, roots := hs.TLS.Certificates[0], hs.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	hs.Close()
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		w := bufio.NewWriter(conn)
		r := bufio.NewReader(conn)
		fmt.Fprint(w, "* OK [CAPABILITY IMAP4rev1] ready\r\n")
		w.Flush()
		for {
			line, err := r.ReadString('\n')
			if err != nil {
				return
			}
			tag := strings.Fields(line)[0]
			up := strings.ToUpper(line)
			switch {
			case strings.Contains(up, "CAPABILITY"):
				fmt.Fprintf(w, "* CAPABILITY IMAP4rev1\r\n%s OK done\r\n", tag)
			case strings.Contains(up, "LOGIN"):
				fmt.Fprintf(w, "* OK %s\r\n%s OK done\r\n", strings.Repeat("a", pad), tag)
			case strings.Contains(up, "SEARCH"):
				fmt.Fprintf(w, "%s\r\n%s OK done\r\n", search(tag), tag)
			case strings.Contains(up, "ENVELOPE"):
				fmt.Fprintf(w, "* 1 FETCH (UID 5 RFC822.SIZE 100 ENVELOPE (NIL \"s\" NIL NIL NIL NIL NIL NIL NIL \"<p@x>\"))\r\n%s OK done\r\n", tag)
			case strings.Contains(up, "BODY"):
				fmt.Fprintf(w, "* 1 FETCH (UID 5 BODY[] {%d}\r\n", literal)
				head := "Message-ID: <p@x>\r\nSubject: s\r\n\r\n"
				w.WriteString(head)
				chunk := strings.Repeat("A", 1<<20)
				for left := literal - len(head); left > 0; left -= len(chunk) {
					if _, err := w.WriteString(chunk[:min(left, len(chunk))]); err != nil {
						return
					}
				}
				fmt.Fprintf(w, ")\r\n%s OK done\r\n", tag)
			case strings.Contains(up, "LOGOUT"):
				fmt.Fprintf(w, "* BYE\r\n%s OK done\r\n", tag)
				w.Flush()
				return
			default:
				fmt.Fprintf(w, "%s OK done\r\n", tag)
			}
			if w.Flush() != nil {
				return
			}
		}
	}()
	counted := &readCounter{}
	d0, r0 := dialBox, tlsRoots
	t.Cleanup(func() { dialBox, tlsRoots = d0, r0 })
	tlsRoots = roots
	dialBox = func(ctx context.Context, _ Box) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", ln.Addr().String())
		counted.Conn = conn
		return counted, err
	}
	return counted
}

func liarBox(budget int64) Box {
	return Box{Host: "example.com", Port: 993, TLS: "tls", User: "help@example.com", Password: "pw-example", Folder: "INBOX", ReadBudget: budget}
}

func TestMailServerThatLiesAboutItsSizeIsCutAtTheBudget(t *testing.T) {
	counted := liar(t, 0)
	s, err := openIMAP(context.Background(), liarBox(PollReadBudget))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	got, _, err := s.After(context.Background(), 0, PerPoll)
	if read := counted.n.Load(); read > PollReadBudget || err == nil {
		t.Fatalf("the session read %d bytes from a server that declared a 100-byte message and sent 80 MB, and got %d messages (err %v): want at most PollReadBudget (%d), then an error",
			read, len(got), err, PollReadBudget)
	}
}

// A Test reads at most TestReadBudget: a server whose LOGIN answer runs past
// it fails the Test, and opens under a poll's budget.
func TestMailTestReadsAtMostItsBudget(t *testing.T) {
	counted := liar(t, 2<<20)
	if s, err := openIMAP(context.Background(), liarBox(0)); err == nil || counted.n.Load() > TestReadBudget {
		if s != nil {
			s.Close()
		}
		t.Fatalf("a Test read %d bytes of a 2 MB LOGIN answer (err %v): want at most TestReadBudget (%d), then an error", counted.n.Load(), err, TestReadBudget)
	}
	liar(t, 2<<20)
	s, err := openIMAP(context.Background(), liarBox(PollReadBudget))
	if err != nil {
		t.Fatalf("a poll's session refused a 2 MB LOGIN answer: %v", err)
	}
	s.Close()
}
