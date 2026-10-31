package business

import "testing"

// Who draws on the board on a call's stage (calls package B, main's ruling of
// 26 Oct): everyone in the call from the workspace by default; only the
// presenter once they say so; the call's guests only under "Guests can draw",
// which is off until the presenter turns it on, and never while only the
// presenter draws.
func TestWhoDrawsOnTheBoardOnTheStage(t *testing.T) {
	everyone := Board{UUID: "b", By: "0x1", Draw: DrawEveryone}
	presenter := Board{UUID: "b", By: "0x1", Draw: DrawPresenter}
	guestsToo := Board{UUID: "b", By: "0x1", Draw: DrawEveryone, Guests: true}
	guestsButPresenter := Board{UUID: "b", By: "0x1", Draw: DrawPresenter, Guests: true}
	for _, c := range []struct {
		name     string
		b        Board
		identity string
		guest    bool
		want     bool
	}{
		{"a member draws by default", everyone, "0x2", false, true},
		{"a guest doesn't by default", everyone, "guest-a", true, false},
		{"a guest draws once guests may", guestsToo, "guest-a", true, true},
		{"only the presenter draws", presenter, "0x2", false, false},
		{"the presenter always draws", presenter, "0x1", false, true},
		{"guests don't while only the presenter draws", guestsButPresenter, "guest-a", true, false},
	} {
		if got := c.b.Draws(c.identity, c.guest); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}

// What a call gives on a board, in the one board authoriser (the member's
// BoardCollabAuthorize): reading it while it is on the stage of a call they
// are in now, and drawing as the board's own rights or the call say. Nothing
// through an API token: a token acts at most as a member, and a call needs
// them in it.
func TestACallOnlyAddsToABoardsOwnRights(t *testing.T) {
	grant := &Grant{Board: Board{UUID: "b", By: "0x1", Draw: DrawEveryone}, Identity: "0x2"}
	presenterOnly := &Grant{Board: Board{UUID: "b", By: "0x1", Draw: DrawPresenter}, Identity: "0x2"}
	for _, c := range []struct {
		name               string
		canRead, canEdit   bool
		grant              *Grant
		throughToken       bool
		wantRead, wantEdit bool
	}{
		{"outside the call, no access", false, false, nil, false, false, false},
		{"in the call, a board they can't read", false, false, grant, false, true, true},
		{"in the call, while only the presenter draws", false, false, presenterOnly, false, true, false},
		{"an editor still edits while only the presenter draws", true, true, presenterOnly, false, true, true},
		{"a reader outside the call reads", true, false, nil, false, true, false},
		{"through an API token the call gives nothing", false, false, grant, true, false, false},
		{"through an API token their own rights stand", true, true, grant, true, true, true},
	} {
		read, edit := BoardAccess(c.canRead, c.canEdit, c.grant, c.throughToken)
		if read != c.wantRead || edit != c.wantEdit {
			t.Errorf("%s: read %v edit %v", c.name, read, edit)
		}
	}
}
