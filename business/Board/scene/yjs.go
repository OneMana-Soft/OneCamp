// Package scene writes a board's drawing on the server: Excalidraw elements as
// the board's canvas keeps them, and the Yjs update the collaboration service
// opens a board from. It is the one copy for every board the server draws: the
// demo's launch retro (business/DemoSeed) and boards brought in from another
// tool (business/Import, W-2).
package scene

// A Yjs document update, written by hand, for the one shape a server-written
// board needs: root maps of plain values.
//
// A board's canvas is a Yjs document (other-services/collaboration-service),
// whose root map "elements" holds each Excalidraw element under its id, "files"
// each picture's metadata and "boardMeta" the board's own settings
// (components/board/boardCanvas.tsx in the web app). The collaboration service
// opens a board by applying its saved state, base64, as one update
// (Y.applyUpdate), so a board the server writes is that update. There is no Go
// Yjs library in the build, and this needs very little of the format: one
// client, one item per key, each holding one value (ContentAny), no deletions.
// The layout follows yjs's encoding v1 (Item.write, writeClientsStructs) and
// lib0's writeAny.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// Entry is one key of a root map and its value: nil, bool, string, int, int64,
// float64, []any or map[string]any (nested as the same).
type Entry struct {
	Key   string
	Value any
}

// Root is one root map of a document and the entries to set in it.
type Root struct {
	Name    string
	Entries []Entry
}

// The bits of an item's info byte, and the content kind it holds.
const (
	hasParentSub = 0x20
	contentAny   = 8
)

// MapsUpdate encodes one Yjs update that sets the entries of several root maps
// (a board's "elements", "files" and "boardMeta"), as client, from clock 0, in
// the order given.
func MapsUpdate(client uint32, roots ...Root) ([]byte, error) {
	n := 0
	for _, r := range roots {
		n += len(r.Entries)
	}
	var b []byte
	b = appendVarUint(b, 1) // one client
	b = appendVarUint(b, uint64(n))
	b = appendVarUint(b, uint64(client))
	b = appendVarUint(b, 0) // its first clock
	for _, r := range roots {
		for _, e := range r.Entries {
			// No left or right neighbour: each key is set once, so the parent is
			// named outright, as a root type, with the key as the item's parentSub.
			b = append(b, contentAny|hasParentSub)
			b = appendVarUint(b, 1) // the parent is a root type, named next
			b = appendVarString(b, r.Name)
			b = appendVarString(b, e.Key)
			b = appendVarUint(b, 1) // one value
			var err error
			if b, err = appendAny(b, e.Value); err != nil {
				return nil, fmt.Errorf("%s.%s: %w", r.Name, e.Key, err)
			}
		}
	}
	return appendVarUint(b, 0), nil // the delete set: nothing deleted
}

// appendVarUint is lib0's writeVarUint: seven bits a byte, low first.
func appendVarUint(b []byte, n uint64) []byte {
	for n > 0x7f {
		b = append(b, byte(0x80|(n&0x7f)))
		n >>= 7
	}
	return append(b, byte(n))
}

// appendVarInt is lib0's writeVarInt: the first byte carries a sign bit and
// six bits, the rest seven.
func appendVarInt(b []byte, n int64) []byte {
	neg := n < 0
	if neg {
		n = -n
	}
	first := byte(n & 0x3f)
	if neg {
		first |= 0x40
	}
	if n > 0x3f {
		first |= 0x80
	}
	b = append(b, first)
	n >>= 6
	for n > 0 {
		next := byte(n & 0x7f)
		if n > 0x7f {
			next |= 0x80
		}
		b = append(b, next)
		n >>= 7
	}
	return b
}

func appendVarString(b []byte, s string) []byte {
	b = appendVarUint(b, uint64(len(s)))
	return append(b, s...)
}

// appendAny is lib0's writeAny for the values a board element holds. An
// object's keys are written sorted, so the same element encodes the same way.
// A number that isn't finite has no place on a canvas, and is refused.
func appendAny(b []byte, v any) ([]byte, error) {
	switch x := v.(type) {
	case nil:
		return append(b, 126), nil
	case bool:
		if x {
			return append(b, 120), nil
		}
		return append(b, 121), nil
	case string:
		return appendVarString(append(b, 119), x), nil
	case int:
		return appendAny(b, int64(x))
	case int64:
		if x >= -math.MaxInt32 && x <= math.MaxInt32 {
			return appendVarInt(append(b, 125), x), nil
		}
		return appendAny(b, float64(x))
	case float64:
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, fmt.Errorf("the number %v can't be drawn", x)
		}
		if x == math.Trunc(x) && math.Abs(x) <= math.MaxInt32 {
			return appendVarInt(append(b, 125), int64(x)), nil
		}
		b = append(b, 123)
		return binary.BigEndian.AppendUint64(b, math.Float64bits(x)), nil
	case []any:
		b = appendVarUint(append(b, 117), uint64(len(x)))
		for _, item := range x {
			var err error
			if b, err = appendAny(b, item); err != nil {
				return nil, err
			}
		}
		return b, nil
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b = appendVarUint(append(b, 118), uint64(len(keys)))
		for _, k := range keys {
			b = appendVarString(b, k)
			var err error
			if b, err = appendAny(b, x[k]); err != nil {
				return nil, err
			}
		}
		return b, nil
	}
	return nil, fmt.Errorf("a %T cannot go in a Yjs value", v)
}

// errShort is an update that ends before what it says it holds.
var errShort = errors.New("the update ends early")

// Decode reads back an update MapsUpdate wrote, with Maps's parser: the root
// maps and their values, numbers as float64. It reads only what MapsUpdate
// writes (one client, items of one value each under a root's key, nothing
// deleted) and refuses anything else, so a state the server is about to save
// is read back first (State) and a board the collaboration service couldn't
// open is never stored.
func Decode(b []byte) (map[string]map[string]any, error) {
	st, err := parseState(b)
	if err != nil {
		return nil, err
	}
	if len(st.clients) != 1 || len(st.deletes) != 0 {
		return nil, errors.New("not a board state this package writes: wanted one client and nothing deleted")
	}
	roots := map[string]map[string]any{}
	for _, structs := range st.clients {
		for _, s := range structs {
			if s.gc || !s.hasRoot || s.sub == nil || s.origin != nil || s.right != nil || s.kind != contentAny || len(s.values) != 1 {
				return nil, fmt.Errorf("not a board state this package writes: wanted an item of one value under a root's key at clock %d", s.id.clock)
			}
			if roots[s.root] == nil {
				roots[s.root] = map[string]any{}
			}
			roots[s.root][*s.sub] = s.values[0]
		}
	}
	return roots, nil
}

type decoder struct {
	b   []byte
	pos int
	err error
}

func (d *decoder) fail(want string) error {
	if d.err == nil {
		d.err = fmt.Errorf("not a board state: wanted %s at byte %d", want, d.pos)
	}
	return d.err
}

func (d *decoder) byte() byte {
	if d.err != nil || d.pos >= len(d.b) {
		if d.err == nil {
			d.err = errShort
		}
		return 0
	}
	d.pos++
	return d.b[d.pos-1]
}

func (d *decoder) uint() uint64 {
	var n uint64
	for shift := uint(0); shift < 64; shift += 7 {
		c := d.byte()
		n |= uint64(c&0x7f) << shift
		if c < 0x80 || d.err != nil {
			return n
		}
	}
	d.fail("a shorter number")
	return 0
}

func (d *decoder) int() int64 {
	c := d.byte()
	n := int64(c & 0x3f)
	neg := c&0x40 != 0
	shift := uint(6)
	for c&0x80 != 0 && d.err == nil && shift < 64 {
		c = d.byte()
		n |= int64(c&0x7f) << shift
		shift += 7
	}
	if neg {
		n = -n
	}
	return n
}

func (d *decoder) str() string {
	n := d.uint()
	if d.err != nil || uint64(len(d.b)-d.pos) < n {
		if d.err == nil {
			d.err = errShort
		}
		return ""
	}
	s := string(d.b[d.pos : d.pos+int(n)])
	d.pos += int(n)
	return s
}

func (d *decoder) any(depth int) any {
	if depth > 64 {
		d.fail("values nested less deeply")
		return nil
	}
	switch t := d.byte(); t {
	case 127, 126: // undefined, null
		return nil
	case 124: // a float32
		if len(d.b)-d.pos < 4 {
			d.err = errShort
			return nil
		}
		v := math.Float32frombits(binary.BigEndian.Uint32(d.b[d.pos:]))
		d.pos += 4
		return float64(v)
	case 122: // a bigint
		if len(d.b)-d.pos < 8 {
			d.err = errShort
			return nil
		}
		v := int64(binary.BigEndian.Uint64(d.b[d.pos:]))
		d.pos += 8
		return float64(v)
	case 116: // bytes
		return d.bytes()
	case 120:
		return true
	case 121:
		return false
	case 119:
		return d.str()
	case 125:
		return float64(d.int())
	case 123:
		if len(d.b)-d.pos < 8 {
			d.err = errShort
			return nil
		}
		v := math.Float64frombits(binary.BigEndian.Uint64(d.b[d.pos:]))
		d.pos += 8
		return v
	case 117:
		n := d.uint()
		out := make([]any, 0, min(n, 1024))
		for i := uint64(0); i < n && d.err == nil; i++ {
			out = append(out, d.any(depth+1))
		}
		return out
	case 118:
		n := d.uint()
		out := make(map[string]any, min(n, 1024))
		for i := uint64(0); i < n && d.err == nil; i++ {
			k := d.str()
			out[k] = d.any(depth + 1)
		}
		return out
	default:
		d.fail(fmt.Sprintf("a kind of value lib0 writes (kind %d)", t))
		return nil
	}
}
