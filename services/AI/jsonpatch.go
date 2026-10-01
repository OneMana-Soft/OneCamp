package ai

// RFC 6902 JSON Patch, for the state a remote agent keeps.
//
// AG-UI carries an agent's own scratchpad two ways: a whole snapshot, or a
// patch against the last one. The frameworks people build with (LangGraph and
// CopilotKit's shared state especially) lean on the patch form, so a client
// that reads only snapshots quietly freezes that state at the first one and
// then hands the agent back something it never wrote.
//
// Hand-written rather than a dependency, because it is a small, closed spec
// and every op is covered by a test below. It is deliberately strict: the
// document is remote-controlled and the result is sent back to that same
// remote, so an op this cannot apply exactly is an error, never a guess. The
// caller's answer to an error is to drop the state, not to keep a stale one.

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// maxPatchOps bounds one patch. A peer that wants more than this is not
// describing an agent's scratchpad.
const maxPatchOps = 512

type patchOp struct {
	Op    string          `json:"op"`
	Path  string          `json:"path"`
	From  string          `json:"from"`
	Value json.RawMessage `json:"value"`
}

// ApplyJSONPatch applies an RFC 6902 patch to a JSON document and returns the
// result. Both arguments and the result are raw JSON. An empty document is
// treated as an empty object, which is what a first patch is written against.
func ApplyJSONPatch(doc, patch json.RawMessage) (json.RawMessage, error) {
	if len(strings.TrimSpace(string(doc))) == 0 {
		doc = json.RawMessage("{}")
	}
	var root interface{}
	if err := json.Unmarshal(doc, &root); err != nil {
		return nil, fmt.Errorf("the document is not JSON: %w", err)
	}
	var ops []patchOp
	if err := json.Unmarshal(patch, &ops); err != nil {
		return nil, fmt.Errorf("the patch is not a JSON Patch array: %w", err)
	}
	if len(ops) > maxPatchOps {
		return nil, fmt.Errorf("the patch has %d operations, more than the %d allowed", len(ops), maxPatchOps)
	}
	for i, op := range ops {
		next, err := applyOne(root, op)
		if err != nil {
			return nil, fmt.Errorf("operation %d (%s %s): %w", i, op.Op, op.Path, err)
		}
		root = next
	}
	out, err := json.Marshal(root)
	if err != nil {
		return nil, fmt.Errorf("the patched document cannot be encoded: %w", err)
	}
	return out, nil
}

// applyOne applies a single operation and returns the new root, which differs
// from the old one only when the operation replaced the document itself.
func applyOne(root interface{}, op patchOp) (interface{}, error) {
	switch op.Op {
	case "add", "replace", "remove", "move", "copy", "test":
	default:
		return nil, fmt.Errorf("unknown operation")
	}

	path, err := parsePointer(op.Path)
	if err != nil {
		return nil, err
	}

	// The operations that read somewhere else first. Resolved before anything
	// is changed, so a failure leaves the document untouched.
	var moved interface{}
	if op.Op == "move" || op.Op == "copy" {
		from, ferr := parsePointer(op.From)
		if ferr != nil {
			return nil, fmt.Errorf("from: %w", ferr)
		}
		if isPrefix(from, path) {
			return nil, fmt.Errorf("cannot move a value into itself")
		}
		v, ok := valueAt(root, from)
		if !ok {
			return nil, fmt.Errorf("from %q does not exist", op.From)
		}
		moved = v
		if op.Op == "move" {
			r, rerr := remove(root, from)
			if rerr != nil {
				return nil, rerr
			}
			root = r
		}
	}

	switch op.Op {
	case "test":
		have, ok := valueAt(root, path)
		if !ok {
			return nil, fmt.Errorf("path does not exist")
		}
		var want interface{}
		if err := json.Unmarshal(nonNullValue(op.Value), &want); err != nil {
			return nil, fmt.Errorf("value is not JSON: %w", err)
		}
		if !sameJSON(have, want) {
			return nil, fmt.Errorf("value does not match")
		}
		return root, nil
	case "remove":
		return remove(root, path)
	case "move", "copy":
		return insert(root, path, moved, true)
	default: // add, replace
		var v interface{}
		if err := json.Unmarshal(nonNullValue(op.Value), &v); err != nil {
			return nil, fmt.Errorf("value is not JSON: %w", err)
		}
		return insert(root, path, v, op.Op == "add")
	}
}

// nonNullValue lets an operation that omits "value" mean the JSON null it is,
// rather than failing to parse as empty input.
func nonNullValue(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

// parsePointer splits an RFC 6901 pointer into its decoded tokens. The empty
// pointer addresses the whole document and yields no tokens.
func parsePointer(p string) ([]string, error) {
	if p == "" {
		return nil, nil
	}
	if !strings.HasPrefix(p, "/") {
		return nil, fmt.Errorf("a path must be empty or start with /")
	}
	parts := strings.Split(p[1:], "/")
	out := make([]string, len(parts))
	for i, s := range parts {
		// ~1 before ~0, or an escaped tilde followed by a 1 would decode twice.
		out[i] = strings.ReplaceAll(strings.ReplaceAll(s, "~1", "/"), "~0", "~")
	}
	return out, nil
}

// isPrefix reports whether a addresses b or one of its ancestors.
func isPrefix(a, b []string) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// valueAt resolves a pointer, reporting whether it addresses anything.
func valueAt(node interface{}, tokens []string) (interface{}, bool) {
	for _, t := range tokens {
		switch n := node.(type) {
		case map[string]interface{}:
			v, ok := n[t]
			if !ok {
				return nil, false
			}
			node = v
		case []interface{}:
			i, err := index(t, len(n), false)
			if err != nil {
				return nil, false
			}
			node = n[i]
		default:
			return nil, false
		}
	}
	return node, true
}

// insert writes value at the pointer. adding distinguishes "add" (which makes
// a new member, or splices into an array) from "replace" (which requires the
// location to exist already).
func insert(root interface{}, tokens []string, value interface{}, adding bool) (interface{}, error) {
	if len(tokens) == 0 {
		return value, nil // the whole document
	}
	parent, ok := valueAt(root, tokens[:len(tokens)-1])
	if !ok {
		return nil, fmt.Errorf("the parent does not exist")
	}
	last := tokens[len(tokens)-1]
	switch p := parent.(type) {
	case map[string]interface{}:
		if !adding {
			if _, exists := p[last]; !exists {
				return nil, fmt.Errorf("cannot replace a member that does not exist")
			}
		}
		p[last] = value
		return root, nil
	case []interface{}:
		i, err := index(last, len(p), adding)
		if err != nil {
			return nil, err
		}
		if !adding {
			p[i] = value
			return root, nil
		}
		// An add splices, so the slice is rebuilt and the parent that holds it
		// has to be pointed at the new one.
		grown := make([]interface{}, 0, len(p)+1)
		grown = append(grown, p[:i]...)
		grown = append(grown, value)
		grown = append(grown, p[i:]...)
		return reseat(root, tokens[:len(tokens)-1], grown)
	default:
		return nil, fmt.Errorf("the parent is not an object or an array")
	}
}

// remove deletes what the pointer addresses.
func remove(root interface{}, tokens []string) (interface{}, error) {
	if len(tokens) == 0 {
		return nil, fmt.Errorf("the whole document cannot be removed")
	}
	parent, ok := valueAt(root, tokens[:len(tokens)-1])
	if !ok {
		return nil, fmt.Errorf("the parent does not exist")
	}
	last := tokens[len(tokens)-1]
	switch p := parent.(type) {
	case map[string]interface{}:
		if _, exists := p[last]; !exists {
			return nil, fmt.Errorf("cannot remove a member that does not exist")
		}
		delete(p, last)
		return root, nil
	case []interface{}:
		i, err := index(last, len(p), false)
		if err != nil {
			return nil, err
		}
		shrunk := make([]interface{}, 0, len(p)-1)
		shrunk = append(shrunk, p[:i]...)
		shrunk = append(shrunk, p[i+1:]...)
		return reseat(root, tokens[:len(tokens)-1], shrunk)
	default:
		return nil, fmt.Errorf("the parent is not an object or an array")
	}
}

// reseat puts a rebuilt slice back where it came from. A Go slice is a value,
// so splicing one produces a new slice its holder still has to be told about;
// forgetting this is the classic way a patch appears to work and changes
// nothing.
func reseat(root interface{}, tokens []string, value interface{}) (interface{}, error) {
	if len(tokens) == 0 {
		return value, nil
	}
	holder, ok := valueAt(root, tokens[:len(tokens)-1])
	if !ok {
		return nil, fmt.Errorf("the parent does not exist")
	}
	last := tokens[len(tokens)-1]
	switch h := holder.(type) {
	case map[string]interface{}:
		h[last] = value
		return root, nil
	case []interface{}:
		i, err := index(last, len(h), false)
		if err != nil {
			return nil, err
		}
		h[i] = value
		return root, nil
	default:
		return nil, fmt.Errorf("the parent is not an object or an array")
	}
}

// index resolves an array token. "-" is the position after the last element
// and is meaningful only where a value is being added.
func index(token string, length int, allowEnd bool) (int, error) {
	if token == "-" {
		if !allowEnd {
			return 0, fmt.Errorf("- addresses the end of an array and can only be added to")
		}
		return length, nil
	}
	if token == "" || (len(token) > 1 && token[0] == '0') {
		return 0, fmt.Errorf("%q is not an array index", token)
	}
	i, err := strconv.Atoi(token)
	if err != nil || i < 0 {
		return 0, fmt.Errorf("%q is not an array index", token)
	}
	if i > length || (i == length && !allowEnd) {
		return 0, fmt.Errorf("index %d is past the end of a %d element array", i, length)
	}
	return i, nil
}

// sameJSON compares two decoded documents by value, which is what "test"
// means: order within an object does not matter, order within an array does.
func sameJSON(a, b interface{}) bool {
	ja, ea := json.Marshal(canonical(a))
	jb, eb := json.Marshal(canonical(b))
	return ea == nil && eb == nil && string(ja) == string(jb)
}

// canonical rebuilds a decoded document so encoding it is stable. Go already
// writes object keys in sorted order, so this only has to walk.
func canonical(v interface{}) interface{} {
	switch n := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(n))
		for k, val := range n {
			out[k] = canonical(val)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(n))
		for i, val := range n {
			out[i] = canonical(val)
		}
		return out
	default:
		return v
	}
}
