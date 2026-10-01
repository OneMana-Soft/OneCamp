package business

import (
	"testing"

	dgraphStruct "github.com/akashc777/OneCamp/models/dgraph"
)

func user(uuid string) *dgraphStruct.DgraphUser { return &dgraphStruct.DgraphUser{Uuid: uuid} }

// The case that was wrong: an author who is not in their own doc's reading list.
//
// Nothing adds a creator to the grant lists automatically, so a rule that checks
// only those lists refuses the author of a private document access to it. The in-app
// executor has always counted the creator; this asserts the MCP surface agrees,
// because a second rule for one question is how the two drift.
func TestDocCreatorMaySeeTheirOwnDoc(t *testing.T) {
	const author = "11111111-1111-1111-1111-111111111111"

	doc := &dgraphStruct.DgraphDoc{
		CreatedBy: user(author),
		// Deliberately empty: the author appears in none of the grant lists.
		ReadingUser:    nil,
		EditingUser:    nil,
		CommentingUser: nil,
	}
	if !docGrantsAccess(doc, author) {
		t.Fatal("the creator of a doc was refused access to it. Nothing guarantees an " +
			"author is also in their own doc's reading list, so omitting the creator locks " +
			"people out of documents they wrote.")
	}
}

func TestDocGrantListsEachGrantSight(t *testing.T) {
	const me = "22222222-2222-2222-2222-222222222222"

	for _, c := range []struct {
		name string
		doc  *dgraphStruct.DgraphDoc
	}{
		{"reader", &dgraphStruct.DgraphDoc{ReadingUser: []*dgraphStruct.DgraphUser{user(me)}}},
		{"editor", &dgraphStruct.DgraphDoc{EditingUser: []*dgraphStruct.DgraphUser{user(me)}}},
		{"commenter", &dgraphStruct.DgraphDoc{CommentingUser: []*dgraphStruct.DgraphUser{user(me)}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if !docGrantsAccess(c.doc, me) {
				t.Fatalf("a %s was refused sight of the doc; all three grants imply "+
					"'may see it', which is the question a read tool asks", c.name)
			}
		})
	}
}

// The controls. Without these, the allows above could be passing because the
// function returns true for everyone.
func TestDocGrantsAccessRefusesEveryoneElse(t *testing.T) {
	const me = "33333333-3333-3333-3333-333333333333"
	const someoneElse = "44444444-4444-4444-4444-444444444444"

	cases := []struct {
		name string
		doc  *dgraphStruct.DgraphDoc
	}{
		{"nil doc", nil},
		{"no grants at all", &dgraphStruct.DgraphDoc{}},
		{"created by someone else", &dgraphStruct.DgraphDoc{CreatedBy: user(someoneElse)}},
		{"someone else is the reader", &dgraphStruct.DgraphDoc{
			ReadingUser: []*dgraphStruct.DgraphUser{user(someoneElse)}}},
		// A nil entry in a grant list must not be read as a match, or a partially
		// populated query result would grant access to anyone.
		{"nil entry in the reader list", &dgraphStruct.DgraphDoc{
			ReadingUser: []*dgraphStruct.DgraphUser{nil}}},
		{"nil creator", &dgraphStruct.DgraphDoc{CreatedBy: nil}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if docGrantsAccess(c.doc, me) {
				t.Fatalf("access was granted for %q; only the creator and the three grant "+
					"lists may grant sight of a private doc", c.name)
			}
		})
	}
}

// An empty principal must never match, including against a doc whose creator or
// grant entry has an empty uuid. Otherwise an unresolved caller would inherit
// access to every partially populated doc.
func TestDocGrantsAccessRefusesAnEmptyPrincipal(t *testing.T) {
	for _, c := range []struct {
		name string
		doc  *dgraphStruct.DgraphDoc
	}{
		{"empty creator uuid", &dgraphStruct.DgraphDoc{CreatedBy: user("")}},
		{"empty reader uuid", &dgraphStruct.DgraphDoc{
			ReadingUser: []*dgraphStruct.DgraphUser{user("")}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if docGrantsAccess(c.doc, "") {
				t.Fatal("an empty principal matched an empty uuid on the doc; two unknowns " +
					"must not compare equal in an authorization check")
			}
			if docGrantsAccess(c.doc, "   ") {
				t.Fatal("a whitespace principal matched an empty uuid on the doc")
			}
		})
	}
}
