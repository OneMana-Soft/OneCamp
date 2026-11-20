package business

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"math"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/helpers/safefetch"
	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// Box is how to reach a mailbox: the server, TLS ("tls" on 993, "starttls"
// on 143: never a login in clear), the account and the folder read.
// PrivateOK is the mailbox saved by a workspace admin, who may point it at
// this server's own network (a self-hosted mail server beside it).
type Box struct {
	Host      string
	Port      int
	TLS       string
	User      string
	Password  string
	Folder    string
	PrivateOK bool
	// ReadBudget is all the session may read from the server, every byte
	// before TLS: PollReadBudget for a poll; zero is TestReadBudget.
	ReadBudget int64
}

// The read budgets. A server says how large a message is (RFC822.SIZE) and
// then sends what it likes: go-imap reads a literal whole, whatever its
// declared size, so the cap is on the dialled conn (safefetch.LimitReads),
// and past it the session fails, backs off and is tried again (failed).
// PollReadBudget is a poll's: the PollBytes it fetches, one MaxMessage more
// (a cut is by declared size), and 1 MB for the greeting, PerPoll envelopes,
// the SEARCH answer and the protocol. TestReadBudget is a Test's: LOGIN and
// EXAMINE only.
const (
	PollReadBudget = PollBytes + MaxMessage + 1<<20
	TestReadBudget = 1 << 20
)

// Head is a message's envelope: what a message too large to fetch is
// taken by.
type Head struct {
	MessageID string
	From      string
	Subject   string
	Date      time.Time
}

// Fetched is one message read: its UID, size and bytes (nil when over
// MaxMessage, then Head is what's known).
type Fetched struct {
	UID  uint32
	Size int64
	Raw  []byte
	Head Head
}

// Session is a mailbox open for reading (EXAMINE: the door never changes it;
// BODY.PEEK: nothing is marked read).
type Session interface {
	// Validity is the folder's UIDVALIDITY; Next its UIDNEXT.
	Validity() uint32
	Next() uint32
	// After is up to max messages with a UID above uid, oldest first, and
	// whether more wait (past max or past PollBytes) for another poll.
	After(ctx context.Context, uid uint32, max int) ([]Fetched, bool, error)
	// Since is up to max messages that arrived on or after day, the same way.
	Since(ctx context.Context, day time.Time, max int) ([]Fetched, bool, error)
	Close()
}

// Opener opens a mailbox.
type Opener func(ctx context.Context, b Box) (Session, error)

// The errors a poll pauses a mailbox for (the admin sees which), rather than
// trying again.
var (
	ErrAuth    = errors.New("triage mail: the mail server refused the login")
	ErrTLS     = errors.New("triage mail: the mail server's TLS failed")
	ErrRefused = errors.New("triage mail: that address may not be reached")
	ErrBox     = errors.New("triage mail: a mailbox needs a host, and TLS on 993 or STARTTLS on 143")
)

// Timeouts: each command, and the dial and handshake.
const (
	dialTimeout    = 10 * time.Second
	commandTimeout = 30 * time.Second
)

// checkBox refuses a mailbox the door won't read: no host, or a port and TLS
// that aren't 993 with TLS or 143 with STARTTLS. Pure.
func checkBox(b Box) error {
	h := strings.TrimSpace(b.Host)
	if h == "" || len(h) > 253 || strings.ContainsAny(h, "/@ \t") {
		return ErrBox
	}
	if (b.TLS == "tls" && b.Port == 993) || (b.TLS == "starttls" && b.Port == 143) {
		return nil
	}
	return ErrBox
}

// dialPolicy is the rule for the address a mailbox is dialled at: the one
// rule for an endpoint the server sends a credential to
// (safefetch.EndpointPolicy): never metadata or link-local, and this server's
// own network only when a workspace admin saved the mailbox.
func dialPolicy(privateOK bool) safefetch.Policy {
	return safefetch.EndpointPolicy(privateOK)
}

// dialBox reaches a mailbox's server under its policy, and tlsRoots verifies
// the server's certificate (nil: the system's): seams for the tests' scripted
// servers.
var (
	dialBox = func(ctx context.Context, b Box) (net.Conn, error) {
		return dialPolicy(b.PrivateOK).Dial(ctx, "tcp", net.JoinHostPort(b.Host, strconv.Itoa(b.Port)))
	}
	tlsRoots *x509.CertPool
)

// openerFor is how mailboxes are opened here: the stub on the demo, which
// never dials; IMAP everywhere else.
func openerFor() Opener {
	if helpers.DemoMode() {
		return openStub
	}
	return openIMAP
}

// openIMAP dials b under its policy, starts TLS, logs in and EXAMINEs the
// folder. A refused login is ErrAuth and a TLS failure ErrTLS, which pause the
// mailbox; anything else is a network error, tried again later.
func openIMAP(ctx context.Context, b Box) (Session, error) {
	if err := checkBox(b); err != nil {
		return nil, err
	}
	dctx, cancel := context.WithTimeout(ctx, dialTimeout)
	defer cancel()
	raw, err := dialBox(dctx, b)
	if err != nil {
		if safefetch.Refused(err) {
			return nil, ErrRefused
		}
		return nil, err
	}
	conn := safefetch.LimitReads(raw, cmp.Or(b.ReadBudget, TestReadBudget))
	_ = conn.SetDeadline(time.Now().Add(dialTimeout))
	opts := &imapclient.Options{TLSConfig: &tls.Config{ServerName: b.Host, MinVersion: tls.VersionTLS12, RootCAs: tlsRoots}}
	var c *imapclient.Client
	if b.TLS == "tls" {
		tc := tls.Client(conn, opts.TLSConfig)
		if err := tc.HandshakeContext(dctx); err != nil {
			_ = conn.Close()
			return nil, errors.Join(ErrTLS, err)
		}
		c = imapclient.New(tc, opts)
	} else if c, err = imapclient.NewStartTLS(conn, opts); err != nil {
		_ = conn.Close()
		return nil, errors.Join(ErrTLS, err)
	}
	s := &imapSession{c: c, conn: conn}
	s.closeOn(ctx)
	s.deadline()
	if err := c.Login(b.User, b.Password).Wait(); err != nil {
		s.Close()
		var ie *imap.Error
		if errors.As(err, &ie) {
			return nil, ErrAuth
		}
		return nil, err
	}
	s.deadline()
	data, err := c.Select(folderOf(b), &imap.SelectOptions{ReadOnly: true}).Wait()
	if err != nil {
		s.Close()
		return nil, err
	}
	s.validity, s.next = data.UIDValidity, uint32(data.UIDNext)
	return s, nil
}

func folderOf(b Box) string {
	if f := strings.TrimSpace(b.Folder); f != "" {
		return f
	}
	return "INBOX"
}

type imapSession struct {
	c              *imapclient.Client
	conn           net.Conn
	validity, next uint32
	stop           func() bool
}

// closeOn closes the connection when ctx ends (a poll's pollTimeout, a test's
// limit), so a slow server can't hold a command past it.
func (s *imapSession) closeOn(ctx context.Context) {
	conn := s.conn
	s.stop = context.AfterFunc(ctx, func() { _ = conn.Close() })
}

func (s *imapSession) deadline()        { _ = s.conn.SetDeadline(time.Now().Add(commandTimeout)) }
func (s *imapSession) Validity() uint32 { return s.validity }
func (s *imapSession) Next() uint32     { return s.next }

func (s *imapSession) Close() {
	if s.stop != nil {
		s.stop()
	}
	s.deadline()
	_ = s.c.Logout().Wait()
	_ = s.c.Close()
}

// After is one UID SEARCH for UIDs above uid, then the envelopes and bodies
// of only the lowest max: a backlog of thousands is never read whole.
func (s *imapSession) After(ctx context.Context, uid uint32, max int) ([]Fetched, bool, error) {
	if uid == math.MaxUint32 {
		return nil, false, nil
	}
	var set imap.UIDSet
	set.AddRange(imap.UID(uid+1), 0)
	return s.searchFetch(ctx, &imap.SearchCriteria{UID: []imap.UIDSet{set}}, uid, max)
}

func (s *imapSession) Since(ctx context.Context, day time.Time, max int) ([]Fetched, bool, error) {
	return s.searchFetch(ctx, &imap.SearchCriteria{Since: day}, 0, max)
}

// searchFetch runs one UID SEARCH and fetches only its lowest max UIDs above
// floor, oldest first; more is a hit left for another poll, past max or past
// PollBytes (After and Since share it).
func (s *imapSession) searchFetch(ctx context.Context, criteria *imap.SearchCriteria, floor uint32, max int) ([]Fetched, bool, error) {
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	s.deadline()
	data, err := s.c.UIDSearch(criteria, nil).Wait()
	if err != nil {
		return nil, false, err
	}
	uids, more, err := lowestAbove(data.All, imap.UID(floor), max)
	if err != nil {
		return nil, false, err
	}
	if len(uids) == 0 {
		return nil, more, nil
	}
	out, cut, err := s.fetch(ctx, uids)
	return out, more || cut, err
}

// searchRanges is how many ranges of one SEARCH answer lowestAbove reads: a
// server sends its answer low to high, so the lowest UIDs are in its first
// ranges, and any past the cap wait for another poll.
const searchRanges = 10_000

// errSearchAnswer is a SEARCH answer that can't be walked: a range with a
// `*` (no end), or sequence numbers where UIDs were asked for.
var errSearchAnswer = errors.New("triage mail: the SEARCH answer can't be walked")

// lowestAbove keeps the lowest max UIDs above floor, ascending, and whether
// it left any, from the ranges a SEARCH answered: ESEARCH's ALL, or a plain
// SEARCH's UIDs, which come as one-UID ranges. It never expands a range (the
// server chooses them, and 1:4294967295 is 40 bytes): it reads at most
// searchRanges of them and takes at most max UIDs from each, so its work is
// O(ranges x max) and its memory O(max). A range "n:*" answers with the
// newest message even when it is below n, so floor drops it.
func lowestAbove(set imap.NumSet, floor imap.UID, max int) ([]imap.UID, bool, error) {
	if set == nil {
		return nil, false, nil
	}
	ranges, ok := set.(imap.UIDSet)
	if !ok {
		return nil, false, errSearchAnswer
	}
	if max < 0 {
		max = 0
	}
	var keep []imap.UID
	more := false
	for i, r := range ranges {
		if i == searchRanges {
			more = true
			break
		}
		lo, hi := r.Start, r.Stop
		if lo == 0 || hi == 0 {
			return nil, false, errSearchAnswer
		}
		if lo > hi {
			lo, hi = hi, lo
		}
		if hi <= floor {
			continue
		}
		if lo <= floor {
			lo = floor + 1
		}
		// Past max UIDs of one range, any other range's are past max too.
		n := uint64(hi-lo) + 1
		if n > uint64(max) {
			n, more = uint64(max), true
		}
		for u := uint64(lo); u < uint64(lo)+n; u++ {
			keep = append(keep, imap.UID(u))
		}
		if len(keep) >= 4*max {
			keep = lowest(keep, max, &more)
		}
	}
	return lowest(keep, max, &more), more, nil
}

// lowest is keep's lowest max distinct UIDs, ascending; more is set when it
// drops one.
func lowest(keep []imap.UID, max int, more *bool) []imap.UID {
	slices.Sort(keep)
	keep = slices.Compact(keep)
	if len(keep) > max {
		*more = true
		keep = keep[:max]
	}
	return keep
}

// firstWithin is the first max of hits (oldest first) within PollBytes, and
// whether any hit waits for another poll: the in-memory sessions' searchFetch.
func firstWithin(hits []Fetched, max int) ([]Fetched, bool) {
	out, cut := withinBytes(hits[:min(len(hits), max)], PollBytes)
	return out, cut || len(hits) > max
}

// PollBytes is how much mail one poll downloads: the messages after the
// first that would pass it wait for the next poll (the mark stops before them).
const PollBytes = 50 << 20

// withinBytes is fs (oldest first) cut where the bodies it downloads would
// pass budget, and whether it cut. A message over MaxMessage costs nothing
// (only its headers are kept), and the first message is always kept, so a
// mailbox never sticks.
func withinBytes(fs []Fetched, budget int64) ([]Fetched, bool) {
	var spent int64
	for i, f := range fs {
		if f.Size > MaxMessage {
			continue
		}
		if spent += f.Size; spent > budget && i > 0 {
			return fs[:i], true
		}
	}
	return fs, false
}

// fetch reads the sizes and envelopes of uids, then the bodies of those under
// MaxMessage: a large message is never downloaded. One poll downloads at most
// PollBytes (withinBytes); cut is a message left for the next poll.
func (s *imapSession) fetch(ctx context.Context, uids []imap.UID) ([]Fetched, bool, error) {
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	s.deadline()
	heads, err := s.c.Fetch(imap.UIDSetNum(uids...), &imap.FetchOptions{UID: true, RFC822Size: true, Envelope: true}).Collect()
	if err != nil {
		return nil, false, err
	}
	var out []Fetched
	for _, h := range heads {
		f := Fetched{UID: uint32(h.UID), Size: h.RFC822Size}
		if e := h.Envelope; e != nil {
			f.Head = Head{MessageID: e.MessageID, Subject: e.Subject, Date: e.Date}
			if len(e.From) > 0 {
				f.Head.From = e.From[0].Addr()
			}
		}
		out = append(out, f)
	}
	// Oldest first (a server may answer in any order): the last UID is the poll's mark.
	slices.SortFunc(out, func(a, b Fetched) int { return cmp.Compare(a.UID, b.UID) })
	out, cut := withinBytes(out, PollBytes)
	var small []imap.UID
	for _, f := range out {
		if f.Size <= MaxMessage {
			small = append(small, imap.UID(f.UID))
		}
	}
	if len(small) == 0 {
		return out, cut, nil
	}
	section := &imap.FetchItemBodySection{Peek: true}
	s.deadline()
	bodies, err := s.c.Fetch(imap.UIDSetNum(small...), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}).Collect()
	if err != nil {
		return nil, false, err
	}
	raw := make(map[uint32][]byte, len(bodies))
	for _, b := range bodies {
		raw[uint32(b.UID)] = b.FindBodySection(section)
	}
	for i := range out {
		out[i].Raw = raw[out[i].UID]
	}
	return out, cut, nil
}
