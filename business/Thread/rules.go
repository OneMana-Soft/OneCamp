package business

// The rules threads keep, as pure functions over the Access Resolver's
// decision, so every one is tested without a store (rules_test.go).

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
	"github.com/akashc777/OneCamp/helpers"
)

// act is what someone asks to do with a thread.
type act int

const (
	actRead   act = iota // list, open, mark read
	actPost              // start a thread, reply
	actSettle            // resolve, reopen, reattach
)

// allowed decides whether someone with decision d may do a with a thread.
// author is whether they started the thread (for actSettle). A deleted
// resource takes nothing (I9); a refusal says what it would take.
func allowed(d accessBusiness.Decision, a act, author bool) error {
	if d.Deleted {
		return ErrGone
	}
	switch a {
	case actRead:
		if d.Level < accessBusiness.LevelView {
			return ErrNotFound // someone who can't see it isn't told it exists
		}
	case actPost:
		if d.Level < accessBusiness.LevelView {
			return ErrNotFound
		}
		if d.Level < accessBusiness.LevelComment {
			return &Refusal{Msg: "You can view this, but not comment on it."}
		}
	case actSettle:
		if d.Level < accessBusiness.LevelView {
			return ErrNotFound
		}
		if d.Level < accessBusiness.LevelComment {
			return &Refusal{Msg: "You can view this, but not comment on it."}
		}
		if !author && d.Level < accessBusiness.LevelEdit {
			return &Refusal{Msg: "Only the person who started this thread, or someone who can edit, can do that."}
		}
	}
	return nil
}

// Limits on what a thread takes.
const (
	maxBodyBytes     = 256 << 10 // a comment's HTML
	maxPositionBytes = 512       // one encoded Yjs relative position (a few dozen in practice)
	maxIDLen         = 128       // a block id, a lineage
	maxQuoteRunes    = 10000     // the quoted text
	maxMetaBytes     = 4 << 10   // an anchor's own detail
)

// validAnchor checks an anchor for a resource that takes the given kinds, and
// returns it with what its kind doesn't use cleared, so a stored anchor holds
// only what it means. An empty kind is 'none'.
func validAnchor(a Anchor, takes []AnchorKind) (Anchor, error) {
	if a.Kind == "" {
		a.Kind = AnchorNone
	}
	ok := false
	for _, k := range takes {
		ok = ok || k == a.Kind
	}
	if !ok {
		return Anchor{}, &Invalid{Msg: "A thread here can't be pinned to that."}
	}
	if len(a.BlockID) > maxIDLen || len(a.Lineage) > maxIDLen {
		return Anchor{}, &Invalid{Msg: "That anchor isn't one this app makes."}
	}
	if utf8.RuneCountInString(a.Quote) > maxQuoteRunes || !utf8.ValidString(a.Quote) {
		return Anchor{}, &Invalid{Msg: "That selection is too long to comment on. Select less text."}
	}
	if len(a.Meta) > 0 {
		if len(a.Meta) > maxMetaBytes || !jsonObject(a.Meta) {
			return Anchor{}, &Invalid{Msg: "That anchor isn't one this app makes."}
		}
	}
	switch a.Kind {
	case AnchorYRange:
		if len(a.Start) == 0 || len(a.End) == 0 || len(a.Start) > maxPositionBytes || len(a.End) > maxPositionBytes {
			return Anchor{}, &Invalid{Msg: "Select some text to comment on."}
		}
	case AnchorBlock:
		if a.BlockID == "" {
			return Anchor{}, &Invalid{Msg: "That block can't take a comment yet."}
		}
		a.Start, a.End = nil, nil
	case AnchorMessage, AnchorTime, AnchorCell:
		if len(a.Meta) == 0 {
			return Anchor{}, &Invalid{Msg: "That anchor isn't one this app makes."}
		}
		a.Start, a.End, a.BlockID, a.Lineage = nil, nil, "", ""
	case AnchorNone:
		return Anchor{Kind: AnchorNone}, nil
	}
	return a, nil
}

// jsonObject reports whether b is one JSON object.
func jsonObject(b []byte) bool {
	var v map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&v); err != nil || v == nil {
		return false
	}
	return !dec.More()
}

// validBody checks a comment before it is written.
func validBody(b Body) (Body, error) {
	if len(b.HTML) > maxBodyBytes {
		return Body{}, &Invalid{Msg: "That comment is too long."}
	}
	hasFiles := len(b.Attachments) > 0 && !bytes.Equal(bytes.TrimSpace(b.Attachments), []byte("null")) &&
		!bytes.Equal(bytes.TrimSpace(b.Attachments), []byte("[]"))
	if strings.TrimSpace(helpers.HTMLToPlainText(b.HTML)) == "" && !hasFiles {
		return Body{}, &Invalid{Msg: "Write something first."}
	}
	if !hasFiles {
		b.Attachments = nil
	}
	return b, nil
}
