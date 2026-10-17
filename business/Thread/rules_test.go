package business

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	accessBusiness "github.com/akashc777/OneCamp/business/Access"
)

// Who may do what with a thread, by level, authorship and deletion: reading
// takes view; starting and replying take comment; resolving, reopening and
// reattaching take comment and being the author, or edit or full. Nobody can
// see a thread on something they can't view, and nothing is done in the trash.
func TestThreadPermissions(t *testing.T) {
	L := func(l accessBusiness.Level) accessBusiness.Decision { return accessBusiness.Decision{Level: l} }
	cases := []struct {
		name   string
		d      accessBusiness.Decision
		a      act
		author bool
		want   error // nil, ErrNotFound, ErrForbidden or ErrGone
	}{
		{"no access reads nothing", L(accessBusiness.LevelNone), actRead, false, ErrNotFound},
		{"a viewer reads", L(accessBusiness.LevelView), actRead, false, nil},
		{"no access posts nothing", L(accessBusiness.LevelNone), actPost, false, ErrNotFound},
		{"a viewer can't comment", L(accessBusiness.LevelView), actPost, false, ErrForbidden},
		{"a commenter comments", L(accessBusiness.LevelComment), actPost, false, nil},
		{"an editor comments", L(accessBusiness.LevelEdit), actPost, false, nil},
		{"a commenter can't resolve someone else's thread", L(accessBusiness.LevelComment), actSettle, false, ErrForbidden},
		{"a commenter resolves their own thread", L(accessBusiness.LevelComment), actSettle, true, nil},
		{"a viewer can't resolve even their own thread", L(accessBusiness.LevelView), actSettle, true, ErrForbidden},
		{"an editor resolves anyone's thread", L(accessBusiness.LevelEdit), actSettle, false, nil},
		{"full access resolves anyone's thread", L(accessBusiness.LevelFull), actSettle, false, nil},
		{"no access can't resolve, and isn't told the thread exists", L(accessBusiness.LevelNone), actSettle, true, ErrNotFound},
		{"the owner of a deleted page reads nothing", accessBusiness.Decision{Level: accessBusiness.LevelFull, Owner: true, Deleted: true}, actRead, true, ErrGone},
		{"nobody comments in the trash", accessBusiness.Decision{Level: accessBusiness.LevelFull, Deleted: true}, actPost, true, ErrGone},
		{"nobody resolves in the trash", accessBusiness.Decision{Level: accessBusiness.LevelEdit, Deleted: true}, actSettle, false, ErrGone},
	}
	for _, c := range cases {
		got := allowed(c.d, c.a, c.author)
		if (c.want == nil) != (got == nil) || (c.want != nil && !errors.Is(got, c.want)) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
	// A refusal says what it would take.
	var r *Refusal
	if err := allowed(L(accessBusiness.LevelComment), actSettle, false); !errors.As(err, &r) || !strings.Contains(r.Msg, "started this thread") {
		t.Errorf("the refusal to resolve says %v", err)
	}
}

func TestAnAnchorKeepsOnlyWhatItsKindMeans(t *testing.T) {
	doc := []AnchorKind{AnchorYRange, AnchorBlock, AnchorNone}
	pos := []byte{1, 2, 3}

	a, err := validAnchor(Anchor{Kind: AnchorYRange, Start: pos, End: pos, BlockID: "b1", Lineage: "L", Quote: "Ship on Thursday"}, doc)
	if err != nil || string(a.Start) != string(pos) || a.Quote != "Ship on Thursday" || a.Lineage != "L" || a.BlockID != "b1" {
		t.Fatalf("a range anchor: %+v, %v", a, err)
	}
	if a, err = validAnchor(Anchor{Start: pos, Quote: "x", Lineage: "L"}, doc); err != nil || a.Kind != AnchorNone || a.Start != nil || a.Quote != "" || a.Lineage != "" {
		t.Errorf("no kind is a page-level thread with nothing else: %+v, %v", a, err)
	}
	if a, err = validAnchor(Anchor{Kind: AnchorBlock, BlockID: "b1", Start: pos, End: pos}, doc); err != nil || a.Start != nil || a.End != nil {
		t.Errorf("a block anchor drops positions: %+v, %v", a, err)
	}

	bad := []struct {
		name string
		a    Anchor
		kind []AnchorKind
	}{
		{"a range with one end", Anchor{Kind: AnchorYRange, Start: pos}, doc},
		{"a block with no block", Anchor{Kind: AnchorBlock}, doc},
		{"a kind the resource doesn't take", Anchor{Kind: AnchorTime, Meta: json.RawMessage(`{"ms":1}`)}, doc},
		{"a time with no time", Anchor{Kind: AnchorTime}, []AnchorKind{AnchorTime}},
		{"meta that isn't an object", Anchor{Kind: AnchorTime, Meta: json.RawMessage(`[1]`)}, []AnchorKind{AnchorTime}},
		{"meta that is two objects", Anchor{Kind: AnchorTime, Meta: json.RawMessage(`{"a":1}{"b":2}`)}, []AnchorKind{AnchorTime}},
		{"a huge position", Anchor{Kind: AnchorYRange, Start: make([]byte, maxPositionBytes+1), End: pos}, doc},
		{"a huge quote", Anchor{Kind: AnchorYRange, Start: pos, End: pos, Quote: strings.Repeat("é", maxQuoteRunes+1)}, doc},
		{"a huge lineage", Anchor{Kind: AnchorYRange, Start: pos, End: pos, Lineage: strings.Repeat("a", maxIDLen+1)}, doc},
	}
	for _, b := range bad {
		var inv *Invalid
		if _, err := validAnchor(b.a, b.kind); !errors.As(err, &inv) {
			t.Errorf("%s: got %v, want an Invalid", b.name, err)
		}
	}
	// The kinds other resources will take keep their meta and lose the rest.
	if a, err := validAnchor(Anchor{Kind: AnchorMessage, Meta: json.RawMessage(`{"message_uuid":"m"}`), Start: pos, Lineage: "L"}, []AnchorKind{AnchorMessage}); err != nil || a.Start != nil || a.Lineage != "" || string(a.Meta) != `{"message_uuid":"m"}` {
		t.Errorf("a message anchor: %+v, %v", a, err)
	}
}

func TestACommentSaysSomething(t *testing.T) {
	for _, b := range []Body{{HTML: ""}, {HTML: "<p>  </p>"}, {HTML: "<p></p>", Attachments: json.RawMessage(`[]`)}, {HTML: "<p></p>", Attachments: json.RawMessage(`null`)}} {
		var inv *Invalid
		if _, err := validBody(b); !errors.As(err, &inv) {
			t.Errorf("%+v: got %v, want an Invalid", b, err)
		}
	}
	if _, err := validBody(Body{HTML: "<p></p>", Attachments: json.RawMessage(`[{"attachment_uuid":"a"}]`)}); err != nil {
		t.Errorf("a comment that is only a file: %v", err)
	}
	if _, err := validBody(Body{HTML: "<p>" + strings.Repeat("a", maxBodyBytes) + "</p>"}); err == nil {
		t.Error("a comment past the limit was taken")
	}
	if b, err := validBody(Body{HTML: "<p>ok</p>", Attachments: json.RawMessage(`[]`)}); err != nil || b.Attachments != nil {
		t.Errorf("no files is no attachments: %+v, %v", b, err)
	}
}
