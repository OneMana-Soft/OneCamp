package domain

// A node's colour (docs F1, D-31): a camp hue's name, a folder's tile and a
// dot after a page's, a file's or a link's title. Ink is the default look, not
// a colour: no colour is the predicate removed. Many nodes are coloured in one
// write (a selection in the tree, coloured in bulk).

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/dgexec"
)

// ColorCap is the most docs one colouring takes.
const ColorCap = 200

// ErrTooManyToColor is a colouring of more than ColorCap docs.
var ErrTooManyToColor = errors.New("doc: at most 200 docs are coloured at once")

// colorRequest is WriteDocColors' upsert: the live docs among docUUIDs
// (each once; any that isn't a uuid left out), the colour set on them, or
// removed for none (""), in one mutation; each one's uuid written again, as
// every doc update writes it, so a delete committed while this is in flight
// aborts it, and its retry finds the doc gone; and the docs it changes. Pure.
func colorRequest(docUUIDs []string, color string) (string, []dgexec.Mutation, error) {
	if color != "" {
		if err := CheckColor(color); err != nil {
			return "", nil, err
		}
	}
	quoted := quoteUUIDs(docUUIDs)
	seen := make(map[string]bool, len(quoted))
	unique := quoted[:0]
	for _, q := range quoted {
		if !seen[q] {
			seen[q] = true
			unique = append(unique, q)
		}
	}
	switch {
	case len(unique) == 0:
		return "", nil, ErrNoDocToUpdate
	case len(unique) > ColorCap:
		return "", nil, ErrTooManyToColor
	}
	q := `{
		docs as var(func: eq(doc_uuid, [` + strings.Join(unique, ", ") + `])) @filter(` + liveDocFilter + `) { u as doc_uuid }
		colored(func: uid(docs)) { doc_uuid }
	}`
	m := dgexec.Mutation{SetNQuads: "uid(docs) <doc_uuid> val(u) .\n", Cond: "@if(gt(len(docs), 0))"}
	if color == "" {
		m.DelNQuads = "uid(docs) <doc_color> * .\n"
	} else {
		value, _ := json.Marshal(color)
		m.SetNQuads += "uid(docs) <doc_color> " + string(value) + " .\n"
	}
	return q, []dgexec.Mutation{m}, nil
}

// WriteDocColors colours the live docs among docUUIDs (color, or none for
// ""), in one write, and answers the ones it changed: a doc that isn't there
// or is in the trash is left as it is.
func WriteDocColors(ctx context.Context, docUUIDs []string, color string) ([]string, error) {
	q, muts, err := colorRequest(docUUIDs, color)
	if err != nil {
		return nil, err
	}
	resp, err := dgexec.Mutate(ctx, q, nil, muts...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/WriteDocColors err: %+v", err)
		return nil, err
	}
	var got struct {
		Colored []struct {
			UUID string `json:"doc_uuid"`
		} `json:"colored"`
	}
	if err := json.Unmarshal(resp.GetJson(), &got); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(got.Colored))
	for _, d := range got.Colored {
		out = append(out, d.UUID)
	}
	return out, nil
}
