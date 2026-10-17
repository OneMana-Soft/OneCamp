package contentcodec

import (
	"bytes"
	"encoding/json"
)

// CanonicalJSON is a doc's JSON as the fixtures hold it and as the
// TypeScript codec writes it: keys sorted at every level, two-space indent,
// no HTML escaping, a final newline. Two docs are the same doc when their
// canonical JSON is the same.
func CanonicalJSON(n *Node) ([]byte, error) {
	raw, err := json.Marshal(n)
	if err != nil {
		return nil, err
	}
	var generic any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&generic); err != nil {
		return nil, err
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(generic); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}
