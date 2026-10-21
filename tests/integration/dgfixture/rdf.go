package dgfixture

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sink takes one batch of N-Quads and gives back the uid each of its blank
// nodes got (by name, without "_:"), as a Dgraph mutation's answer does.
type Sink interface {
	Mutate(ctx context.Context, nquads []byte) (map[string]string, error)
}

// batchQuads is about how many N-Quads one mutation carries: large enough
// that a batch's round trip is a small part of its cost, small enough that
// one mutation stays well under Dgraph's limits.
const batchQuads = 20000

// zeroTime is a live node's *_deleted_at, as production writes it.
const zeroTime = "0001-01-01T00:00:00Z"

// kept are the kinds of node a later batch can point at, so their uids are
// kept after their batch; the rest (posts, chats, comments, reactions,
// mentions) are pointed at only from their own batch.
var kept = []string{"u", "team", "proj", "ch", "dm", "doc", "task", "board"}

// keptKind says whether a blank node's uid is kept: its name is a kept kind
// followed by digits.
func keptKind(key string) bool {
	i := strings.IndexFunc(key, func(r rune) bool { return r >= '0' && r <= '9' })
	if i <= 0 {
		return false
	}
	for _, k := range kept {
		if key[:i] == k {
			return true
		}
	}
	return false
}

// writer builds batches of N-Quads and sends each full one to the sink.
type writer struct {
	ctx      context.Context
	sink     Sink
	buf      bytes.Buffer
	quads    int
	declared map[string]bool   // blank nodes of the batch being built
	uids     map[string]string // kept nodes of batches sent, by name
	sent     int               // N-Quads sent
	batches  int
	err      error
}

func newWriter(ctx context.Context, sink Sink) *writer {
	return &writer{ctx: ctx, sink: sink, declared: map[string]bool{}, uids: map[string]string{}}
}

// node starts a node of type typ named key, in this batch.
func (w *writer) node(key, typ string) string {
	if _, done := w.uids[key]; done || w.declared[key] {
		panic(fmt.Sprintf("dgfixture: node %s written twice", key))
	}
	w.declared[key] = true
	s := "_:" + key
	w.lit(s, "dgraph.type", typ)
	return s
}

// ref is how this batch names the node key: by uid once its batch was sent,
// by blank node while it is in this one. A node in neither would be a new,
// empty node, so it panics.
func (w *writer) ref(key string) string {
	if u, ok := w.uids[key]; ok {
		return "<" + u + ">"
	}
	if w.declared[key] {
		return "_:" + key
	}
	panic(fmt.Sprintf("dgfixture: %s is pointed at before it is written, or from a later batch without being kept", key))
}

func (w *writer) quad(s, p, o string) {
	w.buf.WriteString(s)
	w.buf.WriteString(" <")
	w.buf.WriteString(p)
	w.buf.WriteString("> ")
	w.buf.WriteString(o)
	w.buf.WriteString(" .\n")
	w.quads++
}

// lit writes a string value; the generator writes ASCII only, which Go's
// quoting escapes as N-Quads do.
func (w *writer) lit(s, p, v string) { w.quad(s, p, strconv.Quote(v)) }

func (w *writer) num(s, p string, v int) { w.quad(s, p, strconv.Quote(strconv.Itoa(v))) }

func (w *writer) float(s, p string, v float64) {
	w.quad(s, p, strconv.Quote(strconv.FormatFloat(v, 'f', -1, 64)))
}

func (w *writer) boolean(s, p string, v bool) { w.quad(s, p, strconv.Quote(strconv.FormatBool(v))) }

func (w *writer) at(s, p string, t time.Time) { w.lit(s, p, t.UTC().Format(time.RFC3339Nano)) }

// deletedAt writes a *_deleted_at: the zero time for a live node.
func (w *writer) deletedAt(s, p string, deleted bool, when time.Time) {
	if deleted {
		w.at(s, p, when)
		return
	}
	w.lit(s, p, zeroTime)
}

func (w *writer) edge(s, p, o string) { w.quad(s, p, o) }

// edgeFacets writes an edge with facets, such as (kind="ss", lag=2).
func (w *writer) edgeFacets(s, p, o, facets string) {
	w.buf.WriteString(s)
	w.buf.WriteString(" <")
	w.buf.WriteString(p)
	w.buf.WriteString("> ")
	w.buf.WriteString(o)
	w.buf.WriteString(" (")
	w.buf.WriteString(facets)
	w.buf.WriteString(") .\n")
	w.quads++
}

// boundary is between two nodes: a full batch goes now.
func (w *writer) boundary() {
	if w.quads >= batchQuads {
		w.flush()
	}
}

// flush sends the batch being built.
func (w *writer) flush() {
	if w.err != nil || w.quads == 0 {
		w.reset()
		return
	}
	got, err := w.sink.Mutate(w.ctx, w.buf.Bytes())
	if err != nil {
		w.err = fmt.Errorf("dgfixture: batch %d (%d N-Quads): %w", w.batches+1, w.quads, err)
		w.reset()
		return
	}
	for key := range w.declared {
		if !keptKind(key) {
			continue
		}
		u, ok := got[key]
		if !ok {
			w.err = fmt.Errorf("dgfixture: batch %d gave %s no uid", w.batches+1, key)
			break
		}
		w.uids[key] = u
	}
	w.sent += w.quads
	w.batches++
	w.reset()
}

func (w *writer) reset() {
	w.buf.Reset()
	w.quads = 0
	clear(w.declared)
}
