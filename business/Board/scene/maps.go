package scene

// Reading a board as the collaboration service keeps it: a whole Yjs document
// (Y.encodeStateAsUpdate, the first encoding) that any number of screens
// wrote to, overwrote and deleted from. Decode reads only what MapsUpdate
// writes; Maps reads what a Y.Doc shows.

import (
	"encoding/json"
	"fmt"
	"sort"
	"unicode/utf8"
)

// yID is a struct's place in a Yjs document: the client that wrote it, and
// that client's clock.
type yID struct{ client, clock uint64 }

// yStruct is one struct of an update: an item, or a run of collected garbage.
type yStruct struct {
	id     yID
	length uint64
	gc     bool
	// An item's neighbours when it was written (origin before it, right
	// after it) and, when it was written with neither, its parent: a root by
	// name, or a shared type's item.
	origin, right *yID
	root          string
	hasRoot       bool
	parent        *yID
	// sub is the map key whose value the item is.
	sub *string
	// gone: its content was deleted and collected (ContentDeleted).
	gone bool
	// kind is the content's kind (the info byte's low five bits), and values
	// what it holds: plain values, text, bytes, or nil for a shared type of
	// its own.
	kind   byte
	values []any
}

// yState is a whole Yjs state: each client's structs in clock order, and the
// ranges the delete set names, by client.
type yState struct {
	clients map[uint64][]*yStruct
	deletes map[uint64][][2]uint64
}

// parseState reads a whole state in Yjs's first encoding, as yjs's
// readClientsStructRefs and readDeleteSet would. An update that isn't a whole
// state (a client from a later clock, a gap) is refused, and so is one that
// ends early, has bytes after its delete set, or holds a kind of content Yjs
// doesn't write.
func parseState(b []byte) (*yState, error) {
	d := decoder{b: b}
	st := &yState{clients: map[uint64][]*yStruct{}, deletes: map[uint64][][2]uint64{}}
	clients := d.uint()
	for i := uint64(0); i < clients && d.err == nil; i++ {
		count, client, clock := d.uint(), d.uint(), d.uint()
		if _, twice := st.clients[client]; twice {
			return nil, d.fail("each client once")
		}
		if clock != 0 {
			return nil, d.fail("a whole state: each client from its first clock")
		}
		structs := make([]*yStruct, 0, min(count, 1<<16))
		for j := uint64(0); j < count && d.err == nil; j++ {
			s := d.yStruct(client, clock)
			structs = append(structs, s)
			clock += s.length
		}
		st.clients[client] = structs
	}
	deleting := d.uint()
	for i := uint64(0); i < deleting && d.err == nil; i++ {
		client, ranges := d.uint(), d.uint()
		for j := uint64(0); j < ranges && d.err == nil; j++ {
			clock, length := d.uint(), d.uint()
			st.deletes[client] = append(st.deletes[client], [2]uint64{clock, length})
		}
	}
	if d.err != nil {
		return nil, d.err
	}
	if d.pos != len(b) {
		return nil, d.fail("nothing after the delete set")
	}
	for _, ranges := range st.deletes {
		sort.Slice(ranges, func(i, j int) bool { return ranges[i][0] < ranges[j][0] })
	}
	return st, nil
}

// yStruct reads one struct, as yjs's readClientsStructRefs and its content
// readers do.
func (d *decoder) yStruct(client, clock uint64) *yStruct {
	info := d.byte()
	s := &yStruct{id: yID{client, clock}, kind: info & 0x1f}
	switch s.kind {
	case 0:
		s.gc, s.length = true, d.uint()
		if s.length == 0 {
			d.fail("garbage of some length")
		}
		return s
	case 10:
		d.fail("a whole state, without gaps")
		return s
	}
	if info&0x80 != 0 {
		s.origin = &yID{d.uint(), d.uint()}
	}
	if info&0x40 != 0 {
		s.right = &yID{d.uint(), d.uint()}
	}
	if info&0xc0 == 0 {
		if d.uint() == 1 {
			s.root, s.hasRoot = d.str(), true
		} else {
			s.parent = &yID{d.uint(), d.uint()}
		}
		if info&hasParentSub != 0 {
			k := d.str()
			s.sub = &k
		}
	}
	switch s.kind {
	case 1: // deleted, its content collected
		s.gone, s.length = true, d.uint()
	case 2: // JSON, as older Yjs wrote values
		n := d.uint()
		for i := uint64(0); i < n && d.err == nil; i++ {
			var v any
			if t := d.str(); t != "undefined" && d.err == nil {
				if err := json.Unmarshal([]byte(t), &v); err != nil {
					d.fail("a JSON value")
				}
			}
			s.values = append(s.values, v)
		}
		s.length = n
	case 3: // bytes
		s.values, s.length = []any{d.bytes()}, 1
	case 4: // text: one per UTF-16 unit, as JavaScript counts it
		t := d.str()
		s.values, s.length = []any{t}, utf16Len(t)
	case 5: // an embed in text
		d.str()
		s.length = 1
	case 6: // a mark in text: its key and value
		d.str()
		d.str()
		s.length = 1
	case 7: // a shared type of its own; an XML element or hook has a name
		if ref := d.uint(); ref == 3 || ref == 5 {
			d.str()
		}
		s.values, s.length = []any{nil}, 1
	case 8: // plain values
		n := d.uint()
		s.values = make([]any, 0, min(n, 1024))
		for i := uint64(0); i < n && d.err == nil; i++ {
			s.values = append(s.values, d.any(0))
		}
		s.length = n
	case 9: // a document of its own: its guid and options
		d.str()
		d.any(0)
		s.values, s.length = []any{nil}, 1
	default:
		d.fail(fmt.Sprintf("a kind of content Yjs writes (%d)", s.kind))
	}
	if s.length == 0 && d.err == nil {
		d.fail("content of some length")
	}
	return s
}

// bytes reads a length and that many bytes (lib0's readVarUint8Array).
func (d *decoder) bytes() []byte {
	n := d.uint()
	if d.err != nil || uint64(len(d.b)-d.pos) < n {
		if d.err == nil {
			d.err = errShort
		}
		return nil
	}
	out := append([]byte(nil), d.b[d.pos:d.pos+int(n)]...)
	d.pos += int(n)
	return out
}

// utf16Len is how long JavaScript says a string is.
func utf16Len(s string) uint64 {
	var n uint64
	for _, r := range s {
		if r >= 0x10000 && r <= utf8.MaxRune {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// find is the struct that holds id, or nil.
func (st *yState) find(id yID) *yStruct {
	structs := st.clients[id.client]
	i := sort.Search(len(structs), func(i int) bool { return structs[i].id.clock+structs[i].length > id.clock })
	if i < len(structs) && structs[i].id.clock <= id.clock {
		return structs[i]
	}
	return nil
}

// deleted is whether a struct's last unit (a map value's) is deleted: its
// content collected, or named by the delete set.
func (st *yState) deleted(s *yStruct) bool {
	if s.gc || s.gone {
		return true
	}
	last := s.id.clock + s.length - 1
	ranges := st.deletes[s.id.client]
	i := sort.Search(len(ranges), func(i int) bool { return ranges[i][0]+ranges[i][1] > last })
	return i < len(ranges) && ranges[i][0] <= last
}

// yPlace is where an item is: under a root (by name) as a map key's value,
// and how many overwrites of that key came before it.
type yPlace struct {
	root  string
	sub   *string
	depth int
	// shown: under a root of the document. An item in a shared type of its
	// own, or whose neighbour was collected (Yjs collects it too), isn't.
	shown bool
}

// places finds every item's place as yjs's Item.getMissing does: from its
// parent when it was written with one, else from its origin's, else from its
// right neighbour's. A chain of overwrites is followed once (memo).
func (st *yState) places() (map[*yStruct]yPlace, error) {
	memo := map[*yStruct]yPlace{}
	total := 0
	for _, structs := range st.clients {
		total += len(structs)
	}
	for _, structs := range st.clients {
		for _, s := range structs {
			if s.gc {
				continue
			}
			var chain []*yStruct
			cur := s
			var base yPlace
			for {
				if p, ok := memo[cur]; ok {
					base = p
					break
				}
				if cur.hasRoot {
					base = yPlace{root: cur.root, sub: cur.sub, shown: true}
					memo[cur] = base
					break
				}
				if cur.parent != nil {
					memo[cur] = base
					break
				}
				var left, right *yStruct
				for _, n := range []struct {
					id  *yID
					got **yStruct
				}{{cur.origin, &left}, {cur.right, &right}} {
					if n.id == nil {
						continue
					}
					if *n.got = st.find(*n.id); *n.got == nil {
						return nil, fmt.Errorf("not a whole board state: an item's neighbour %d:%d isn't in it", n.id.client, n.id.clock)
					}
				}
				// Beside collected garbage, or with no neighbour at all, Yjs
				// collects the item too: it is shown nowhere.
				if (left == nil && right == nil) || (left != nil && left.gc) || (right != nil && right.gc) {
					memo[cur] = base
					break
				}
				next := left
				if next == nil {
					next = right
				}
				chain = append(chain, cur)
				if len(chain) > total {
					return nil, fmt.Errorf("not a whole board state: its items' neighbours go round in a ring")
				}
				cur = next
			}
			// Each item down the chain is one more overwrite of the same key.
			for i := len(chain) - 1; i >= 0; i-- {
				if base.shown {
					base.depth++
				}
				memo[chain[i]] = base
			}
		}
	}
	return memo, nil
}

// Maps reads the root maps of a whole board state as a Y.Doc that applied it
// shows them: each key's live value, after every screen's writes and
// deletions, numbers as float64. Where two screens set one key at once,
// Yjs keeps the later overwrite, and of two at the same point, the one of
// the larger client id. A value that is a shared type of its own (a map in
// a map) reads as nil; the items of lists and text are read past. An update
// that isn't a whole state is refused (Y.encodeStateAsUpdate writes one).
func Maps(b []byte) (map[string]map[string]any, error) {
	st, err := parseState(b)
	if err != nil {
		return nil, err
	}
	places, err := st.places()
	if err != nil {
		return nil, err
	}
	type pick struct {
		s     *yStruct
		depth int
	}
	best := map[string]map[string]pick{}
	for client, structs := range st.clients {
		for _, s := range structs {
			p := places[s]
			if s.gc || !p.shown || p.sub == nil || st.deleted(s) || len(s.values) == 0 {
				continue
			}
			keys := best[p.root]
			if keys == nil {
				keys = map[string]pick{}
				best[p.root] = keys
			}
			was, ok := keys[*p.sub]
			if !ok || p.depth > was.depth || (p.depth == was.depth && client > was.s.id.client) {
				keys[*p.sub] = pick{s, p.depth}
			}
		}
	}
	roots := make(map[string]map[string]any, len(best))
	for root, keys := range best {
		m := make(map[string]any, len(keys))
		for k, p := range keys {
			m[k] = p.s.values[len(p.s.values)-1]
		}
		roots[root] = m
	}
	return roots, nil
}
